package postgresql

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/columns"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/tables"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

type Postgresql struct {
	Connection        string
	Schema            string
	DoSchemaUpdates   bool
	TagsAsForeignkeys bool
	TagsAsJsonb       bool
	FieldsAsJsonb     bool
	TableTemplate     string
	TagTableSuffix    string
	PoolSize          int

	dbContext       context.Context
	dbContextCancel func()
	db              *pgxpool.Pool
	tables          tables.Manager
	tagTables       tables.Manager

	writeChan chan []telegraf.Metric

	rows    transformer
	columns columns.Mapper
}

func init() {
	outputs.Add("postgresql", func() telegraf.Output { return newPostgresql() })
}

const createTableTemplate = "CREATE TABLE IF NOT EXISTS {TABLE}({COLUMNS})"

func newPostgresql() *Postgresql {
	return &Postgresql{
		Schema:          "public",
		TableTemplate:   createTableTemplate,
		TagTableSuffix:  "_tag",
		DoSchemaUpdates: true,
	}
}

// Connect establishes a connection to the target database and prepares the cache
func (p *Postgresql) Connect() error {
	poolConfig, err := pgxpool.ParseConfig("pool_max_conns=1 " + p.Connection)
	if err != nil {
		return err
	}

	poolConfig.AfterConnect = p.dbConnectedHook

	// Yes, we're not supposed to store the context. However since we don't receive a context, we have to.
	p.dbContext, p.dbContextCancel = context.WithCancel(context.Background())
	p.db, err = pgxpool.ConnectConfig(p.dbContext, poolConfig)
	if err != nil {
		log.Printf("E! Couldn't connect to server\n%v", err)
		return err
	}
	p.tables = tables.NewManager(p.db, p.Schema, p.TableTemplate)
	if p.TagsAsForeignkeys {
		p.tagTables = tables.NewManager(p.db, p.Schema, createTableTemplate)
	}

	p.rows = newRowTransformer(p.TagsAsForeignkeys, p.TagsAsJsonb, p.FieldsAsJsonb)
	p.columns = columns.NewMapper(p.TagsAsForeignkeys, p.TagsAsJsonb, p.FieldsAsJsonb)

	maxConns := int(p.db.Stat().MaxConns())
	if maxConns > 1 {
		p.writeChan = make(chan []telegraf.Metric, maxConns)
		for i := 0; i < maxConns; i++ {
			go p.writeWorker(p.dbContext)
		}
	}

	return nil
}

// dbConnectHook checks to see whether we lost all connections, and if so resets any known state of the database (e.g. cached tables).
func (p *Postgresql) dbConnectedHook(ctx context.Context, conn *pgx.Conn) error {
	if p.db == nil || p.tables == nil {
		// This will happen on the initial connect since we haven't set it yet.
		// Also meaning there is no state to reset.
		return nil
	}

	stat := p.db.Stat()
	if stat.AcquiredConns()+stat.IdleConns() > 0 {
		return nil
	}

	p.tables.ClearTableCache()
	if p.tagTables != nil {
		p.tagTables.ClearTableCache()
	}

	return nil
}

// Close closes the connection to the database
func (p *Postgresql) Close() error {
	p.tables = nil
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
	metricsByMeasurement := utils.GroupMetricsByMeasurement(metrics)

	if p.db.Stat().MaxConns() > 1 {
		return p.writeConcurrent(metricsByMeasurement)
	} else {
		return p.writeSequential(metricsByMeasurement)
	}
}

func (p *Postgresql) writeSequential(metricsByMeasurement map[string][]telegraf.Metric) error {
	for _, metrics := range metricsByMeasurement {
		err := p.writeMetricsFromMeasure(p.dbContext, metrics)
		if err != nil {
			log.Printf("copy error: %v", err)
			return err
		}
	}
	return nil
}

func (p *Postgresql) writeConcurrent(metricsByMeasurement map[string][]telegraf.Metric) error {
	for _, metrics := range metricsByMeasurement {
		select {
		case p.writeChan <- metrics:
		case <-p.dbContext.Done():
			return nil
		}
	}
	return nil
}

var backoffInit = time.Millisecond * 250
var backoffMax = time.Second * 15

func (p *Postgresql) writeWorker(ctx context.Context) {
	for {
		select {
		case metrics := <-p.writeChan:
			backoff := time.Duration(0)
			for {
				err := p.writeMetricsFromMeasure(ctx, metrics)
				if err == nil {
					break
				}

				if !isTempError(err) {
					log.Printf("write error (permanent): %v", err)
					break
				}
				log.Printf("write error (retry in %s): %v", backoff, err)
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
		case <-p.dbContext.Done():
			return
		}
	}
}

func isTempError(err error) bool {
	return false
}

// Writes the metrics from a specified measure. All the provided metrics must belong to the same measurement.
func (p *Postgresql) writeMetricsFromMeasure(ctx context.Context, metrics []telegraf.Metric) error {
	targetColumns, targetTagColumns := p.columns.Target(metrics)
	measureName := metrics[0].Name()

	if p.DoSchemaUpdates {
		if err := p.prepareTable(ctx, p.tables, measureName, targetColumns); err != nil {
			return err
		}
		if p.TagsAsForeignkeys {
			tagTableName := measureName + p.TagTableSuffix
			if err := p.prepareTable(ctx, p.tagTables, tagTableName, targetTagColumns); err != nil {
				return err
			}
		}
	}
	numColumns := len(targetColumns.Names)
	values := make([][]interface{}, len(metrics))
	var rowTransformErr error
	for rowNum, metric := range metrics {
		values[rowNum], rowTransformErr = p.rows.createRowFromMetric(numColumns, metric, targetColumns, targetTagColumns)
		if rowTransformErr != nil {
			log.Printf("E! Could not transform metric to proper row\n%v", rowTransformErr)
			return rowTransformErr
		}
	}

	fullTableName := utils.FullTableName(p.Schema, measureName)
	_, err := p.db.CopyFrom(ctx, fullTableName, targetColumns.Names, pgx.CopyFromRows(values))
	return err
}

// Checks if a table exists in the db, and then validates if all the required columns
// are present or some are missing (if metrics changed their field or tag sets).
func (p *Postgresql) prepareTable(ctx context.Context, tableManager tables.Manager, tableName string, details *utils.TargetColumns) error {
	tableExists := tableManager.Exists(ctx, tableName)

	if !tableExists {
		return tableManager.CreateTable(ctx, tableName, details)
	}

	missingColumns, err := tableManager.FindColumnMismatch(ctx, tableName, details)
	if err != nil {
		return err
	}
	if len(missingColumns) == 0 {
		return nil
	}
	return tableManager.AddColumnsToTable(ctx, tableName, missingColumns, details)
}
