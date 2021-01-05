package postgresql

import (
	"context"
	"errors"
	"fmt"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/template"
	"github.com/jackc/pgconn"
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

	writeChan chan *TableSource
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
		p.writeChan = make(chan *TableSource)
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
	tableSources := map[string]*TableSource{}

	for _, m := range metrics {
		rs := tableSources[m.Name()]
		if rs == nil {
			rs = NewTableSource(p)
			tableSources[m.Name()] = rs
		}
		rs.AddMetric(m)
	}

	if p.db.Stat().MaxConns() > 1 {
		return p.writeConcurrent(tableSources)
	} else {
		return p.writeSequential(tableSources)
	}
}

func (p *Postgresql) writeSequential(tableSources map[string]*TableSource) error {
	for _, tableSource := range tableSources {
		err := p.writeMetricsFromMeasure(p.dbContext, tableSource)
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

func (p *Postgresql) writeConcurrent(tableSources map[string]*TableSource) error {
	for _, tableSource := range tableSources {
		select {
		case p.writeChan <- tableSource:
		case <-p.dbContext.Done():
			return nil
		}
	}
	return nil
}

func (p *Postgresql) writeWorker(ctx context.Context) {
	for {
		select {
		case tableSource := <-p.writeChan:
			if err := p.writeRetry(ctx, tableSource); err != nil {
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

func (p *Postgresql) writeRetry(ctx context.Context, tableSource *TableSource) error {
	backoff := time.Duration(0)
	for {
		err := p.writeMetricsFromMeasure(ctx, tableSource)
		if err == nil {
			return nil
		}

		if !isTempError(err) {
			return err
		}
		log.Printf("write error (retry in %s): %v", backoff, err)
		tableSource.Reset()
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
func (p *Postgresql) writeMetricsFromMeasure(ctx context.Context, tableSource *TableSource) error {
	err := p.tableManager.MatchSource(ctx, tableSource)
	if err != nil {
		return err
	}

	if p.TagsAsForeignkeys {
		if err := p.WriteTagTable(ctx, tableSource); err != nil {
			// log and continue. As the admin can correct the issue, and tags don't change over time, they can be added from
			// future metrics after issue is corrected.
			log.Printf("[outputs.postgresql] Error: Writing to tag table '%s': %s", tableSource.Name()+p.TagTableSuffix, err)
		}
	}

	fullTableName := utils.FullTableName(p.Schema, tableSource.Name())
	_, err = p.db.CopyFrom(ctx, fullTableName, tableSource.ColumnNames(), tableSource)
	return err
}

func (p *Postgresql) WriteTagTable(ctx context.Context, tableSource *TableSource) error {
	//TODO cache which tagSets we've already inserted and skip them.
	ttsrc := NewTagTableSource(tableSource)

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	ident := pgx.Identifier{ttsrc.postgresql.Schema, ttsrc.Name()}
	identTemp := pgx.Identifier{ttsrc.Name() + "_temp"}
	_, err = tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE %s (LIKE %s) ON COMMIT DROP", identTemp.Sanitize(), ident.Sanitize()))
	if err != nil {
		return fmt.Errorf("creating tags temp table: %w", err)
	}

	if _, err := tx.CopyFrom(ctx, identTemp, ttsrc.ColumnNames(), ttsrc); err != nil {
		return fmt.Errorf("copying into tags temp table: %w", err)
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s ON CONFLICT (tag_id) DO NOTHING", ident.Sanitize(), identTemp.Sanitize())); err != nil {
		return fmt.Errorf("inserting into tags table: %w", err)
	}

	return tx.Commit(ctx)
}
