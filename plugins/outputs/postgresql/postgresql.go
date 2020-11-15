package postgresql

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

type Postgresql struct {
	Connection                 string
	Schema                     string
	TagsAsForeignkeys          bool
	TagsAsJsonb                bool
	FieldsAsJsonb              bool
	CreateTemplates            []*Template
	AddColumnTemplates         []*Template
	TagTableCreateTemplates    []*Template
	TagTableAddColumnTemplates []*Template
	TagTableSuffix             string
	PoolSize                   int

	dbContext       context.Context
	dbContextCancel func()
	db              *pgxpool.Pool
	tableManager    *TableManager

	writeChan chan *RowSource
}

func init() {
	outputs.Add("postgresql", func() telegraf.Output { return newPostgresql() })
}

const createTableTemplate = "CREATE TABLE IF NOT EXISTS {TABLE}({COLUMNS})"

func newPostgresql() *Postgresql {
	return &Postgresql{
		Schema:                     "public",
		CreateTemplates:            []*Template{TableCreateTemplate},
		AddColumnTemplates:         []*Template{TableAddColumnTemplate},
		TagTableCreateTemplates:    []*Template{TableCreateTemplate},
		TagTableAddColumnTemplates: []*Template{TableAddColumnTemplate},
		TagTableSuffix:             "_tag",
	}
}

// Connect establishes a connection to the target database and prepares the cache
func (p *Postgresql) Connect() error {
	poolConfig, err := pgxpool.ParseConfig("pool_max_conns=1 " + p.Connection)
	if err != nil {
		return err
	}

	// Yes, we're not supposed to store the context. However since we don't receive a context, we have to.
	p.dbContext, p.dbContextCancel = context.WithCancel(context.Background())
	p.db, err = pgxpool.ConnectConfig(p.dbContext, poolConfig)
	if err != nil {
		log.Printf("E! Couldn't connect to server\n%v", err)
		return err
	}
	p.tableManager = NewTableManager(p)

	maxConns := int(p.db.Stat().MaxConns())
	if maxConns > 1 {
		p.writeChan = make(chan *RowSource)
		for i := 0; i < maxConns; i++ {
			go p.writeWorker(p.dbContext)
		}
	}

	return nil
}

// dbConnectHook checks to see whether we lost all connections, and if so resets any known state of the database (e.g. cached tables).
func (p *Postgresql) dbConnectedHook(ctx context.Context, conn *pgx.Conn) error {
	if p.db == nil || p.tableManager == nil {
		// This will happen on the initial connect since we haven't set it yet.
		// Also meaning there is no state to reset.
		return nil
	}

	stat := p.db.Stat()
	if stat.AcquiredConns()+stat.IdleConns() > 0 {
		return nil
	}

	p.tableManager.ClearTableCache()

	return nil
}

// Close closes the connection to the database
func (p *Postgresql) Close() error {
	p.tableManager = nil
	p.dbContextCancel()
	p.db.Close()
	return nil
}

var sampleConfig = `
  ## specify address via a url matching:
  ##   postgres://[pqgotest[:password]]@localhost[/dbname]\
  ##       ?sslmode=[disable|verify-ca|verify-full]
  ## or a simple string:
  ##   host=localhost user=pqotest password=... sslmode=... dbname=app_production
  ##
  ## All connection parameters are optional. Also supported are PG environment vars
  ## e.g. PGPASSWORD, PGHOST, PGUSER, PGDATABASE 
  ## all supported vars here: https://www.postgresql.org/docs/current/libpq-envars.html
  ##
  ## Without the dbname parameter, the driver will default to a database
  ## with the same name as the user. This dbname is just for instantiating a
  ## connection with the server and doesn't restrict the databases we are trying
  ## to grab metrics for.
  ##
  connection = "host=localhost user=postgres sslmode=verify-full"

  ## Update existing tables to match the incoming metrics automatically. Default is true
  # do_schema_updates = true

  ## Store tags as foreign keys in the metrics table. Default is false.
  # tags_as_foreignkeys = false

  ## Template to use for generating tables
  ## Available Variables:
  ##   {TABLE} - tablename as identifier
  ##   {TABLELITERAL} - tablename as string literal
  ##   {COLUMNS} - column definitions
  ##   {KEY_COLUMNS} - comma-separated list of key columns (time + tags)

  ## Default template
  # table_template = "CREATE TABLE IF NOT EXISTS {TABLE}({COLUMNS})"
  ## Example for timescaledb
  # table_template = "CREATE TABLE IF NOT EXISTS {TABLE}({COLUMNS}); SELECT create_hypertable({TABLELITERAL},'time',chunk_time_interval := '1 week'::interval,if_not_exists := true);"

  ## Schema to create the tables into
  # schema = "public"

  ## Use jsonb datatype for tags
  # tags_as_jsonb = false

  ## Use jsonb datatype for fields
  # fields_as_jsonb = false

`

func (p *Postgresql) SampleConfig() string { return sampleConfig }
func (p *Postgresql) Description() string  { return "Send metrics to PostgreSQL" }

func (p *Postgresql) Write(metrics []telegraf.Metric) error {
	rowSources := p.splitRowSources(metrics)

	if p.db.Stat().MaxConns() > 1 {
		return p.writeConcurrent(rowSources)
	} else {
		return p.writeSequential(rowSources)
	}
}

func (p *Postgresql) writeSequential(rowSources map[string]*RowSource) error {
	for _, rowSource := range rowSources {
		err := p.writeMetricsFromMeasure(p.dbContext, rowSource)
		if err != nil {
			if !isTempError(err) {
				log.Printf("write error (permanent): %v", err)
			}
			//TODO use a transaction so that we don't end up with a partial write, and end up retrying metrics we've already written
			return err
		}
	}
	return nil
}

func (p *Postgresql) writeConcurrent(rowSources map[string]*RowSource) error {
	for _, rowSource := range rowSources {
		select {
		case p.writeChan <- rowSource:
		case <-p.dbContext.Done():
			return nil
		}
	}
	return nil
}

func (p *Postgresql) writeWorker(ctx context.Context) {
	for {
		select {
		case rowSource := <-p.writeChan:
			if err := p.writeRetry(ctx, rowSource); err != nil {
				log.Printf("write error (permanent): %v", err)
			}
		case <-p.dbContext.Done():
			return
		}
	}
}

func isTempError(err error) bool {
	return false
}

var backoffInit = time.Millisecond * 250
var backoffMax = time.Second * 15

func (p *Postgresql) writeRetry(ctx context.Context, rowSource *RowSource) error {
	backoff := time.Duration(0)
	for {
		err := p.writeMetricsFromMeasure(ctx, rowSource)
		if err == nil {
			return nil
		}

		if !isTempError(err) {
			return err
		}
		log.Printf("write error (retry in %s): %v", backoff, err)
		rowSource.Reset()
		time.Sleep(backoff)

		if backoff == 0 {
			backoff = backoffInit
		} else {
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
		}
	}
}

// Writes the metrics from a specified measure. All the provided metrics must belong to the same measurement.
func (p *Postgresql) writeMetricsFromMeasure(ctx context.Context, rowSource *RowSource) error {
	err := p.tableManager.MatchSource(ctx, rowSource)
	if err != nil {
		return err
	}

	targetColumns := rowSource.Columns()
	measureName := rowSource.Name()

	columnPositions := make(map[string]int, len(targetColumns))
	columnNames := make([]string, len(targetColumns))
	for i, col := range targetColumns {
		columnPositions[col.Name] = i
		columnNames[i] = col.Name
	}

	fullTableName := utils.FullTableName(p.Schema, measureName)
	_, err = p.db.CopyFrom(ctx, fullTableName, columnNames, rowSource)
	return err
}
