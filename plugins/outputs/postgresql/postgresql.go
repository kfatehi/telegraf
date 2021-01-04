package postgresql

import (
	"context"
	"errors"
	"fmt"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/template"
	"github.com/jackc/pgconn"
	"log"
	"strings"
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
	CreateTemplates            []*template.Template
	AddColumnTemplates         []*template.Template
	TagTableCreateTemplates    []*template.Template
	TagTableAddColumnTemplates []*template.Template
	TagTableSuffix             string
	PoolSize                   int
	RetryMaxBackoff            internal.Duration

	dbContext       context.Context
	dbContextCancel func()
	db              *pgxpool.Pool
	tableManager    *TableManager

	writeChan chan *RowSource
}

func init() {
	outputs.Add("postgresql", func() telegraf.Output { return newPostgresql() })
}

func newPostgresql() *Postgresql {
	return &Postgresql{
		Schema:                     "public",
		CreateTemplates:            []*template.Template{template.TableCreateTemplate},
		AddColumnTemplates:         []*template.Template{template.TableAddColumnTemplate},
		TagTableCreateTemplates:    []*template.Template{template.TagTableCreateTemplate},
		TagTableAddColumnTemplates: []*template.Template{template.TableAddColumnTemplate},
		TagTableSuffix:             "_tag",
		RetryMaxBackoff:            internal.Duration{time.Second * 15},
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
			if isTempError(err) {
				//TODO use a transaction so that we don't end up with a partial write, and end up retrying metrics we've already written
				return err
			}
			log.Printf("write error (permanent, dropping sub-batch): %v", err)
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
				log.Printf("write error (permanent, dropping sub-batch): %v", err)
			}
		case <-p.dbContext.Done():
			return
		}
	}
}

func isTempError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr); pgErr != nil {
		return false
	}
	return true
}

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
			backoff = time.Millisecond * 250
		} else {
			backoff *= 2
			if backoff > p.RetryMaxBackoff.Duration {
				backoff = p.RetryMaxBackoff.Duration
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

	if p.TagsAsForeignkeys {
		if err := p.WriteTagTable(ctx, rowSource); err != nil {
			// log and continue. As the admin can correct the issue, and tags don't change over time, they can be added from
			// future metrics after issue is corrected.
			log.Printf("[outputs.postgresql] Error: Writing to tag table '%s': %s", rowSource.Name()+p.TagTableSuffix, err)
		}
	}

	fullTableName := utils.FullTableName(p.Schema, rowSource.Name())
	_, err = p.db.CopyFrom(ctx, fullTableName, rowSource.ColumnNames(), rowSource)
	return err
}

func (p *Postgresql) WriteTagTable(ctx context.Context, rowSource *RowSource) error {
	tagCols := rowSource.TagTableColumns()

	columnNames := make([]string, len(tagCols))
	placeholders := make([]string, len(tagCols))
	for i, col := range tagCols {
		columnNames[i] = template.QuoteIdentifier(col.Name)
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}

	// pgx batch code will automatically convert this into a prepared statement & cache it
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (tag_id) DO NOTHING",
		template.QuoteIdentifier(p.Schema)+"."+template.QuoteIdentifier(rowSource.Name()+p.TagTableSuffix),
		strings.Join(columnNames, ","),
		strings.Join(placeholders, ","),
	)

	batch := &pgx.Batch{}
	//TODO rowSource should emit another source for the tags. We shouldn't have to dive into its private methods.
	//TODO cache which tagSets we've already inserted and skip them.
	//TODO copy into a temp table, and then `insert ... on conflict` from that into the tag table.
	for tagID, tagSet := range rowSource.tagSets {
		values := make([]interface{}, len(columnNames))
		values[0] = tagID

		if !p.TagsAsJsonb {
			for _, tag := range tagSet {
				values[rowSource.tagPositions[tag.Key]+1] = tag.Value // +1 to account for tag_id column
			}
		} else {
			values[1] = utils.TagListToJSON(tagSet)
		}

		batch.Queue(sql, values...)
	}
	results := p.db.SendBatch(ctx, batch)
	defer results.Close()

	for i := 0; i < len(rowSource.tagSets); i++ {
		if _, err := results.Exec(); err != nil {
			return err
		}
	}

	return nil
}
