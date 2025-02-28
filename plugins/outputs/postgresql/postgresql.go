package postgresql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coocood/freecache"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgtype"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/models"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/sqltemplate"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

type dbh interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	Exec(ctx context.Context, sql string, arguments ...interface{}) (commandTag pgconn.CommandTag, err error)
	Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error)
}

var sampleConfig = `
	## Specify connection address via the standard libpq connection string:
  ##   host=... user=... password=... sslmode=... dbname=...
	## Or a URL:
  ##   postgres://[user[:password]]@localhost[/dbname]\
  ##       ?sslmode=[disable|verify-ca|verify-full]
  ##
  ## All connection parameters are optional. Environment vars are also supported.
  ## e.g. PGPASSWORD, PGHOST, PGUSER, PGDATABASE 
  ## All supported vars can be found here:
	##  https://www.postgresql.org/docs/current/libpq-envars.html
  ##
  ## Non-standard parameters:
  ##   pool_max_conns (default: 1) - Maximum size of connection pool for parallel (per-batch per-table) inserts.
  ##   pool_min_conns (default: 0) - Minimum size of connection pool.
  ##   pool_max_conn_lifetime (default: 0s) - Maximum age of a connection before closing.
  ##   pool_max_conn_idle_time (default: 0s) - Maximum idle time of a connection before closing.
  ##   pool_health_check_period (default: 0s) - Duration between health checks on idle connections.
	# connection = ""

  ## Postgres schema to use.
  # schema = "public"

  ## Store tags as foreign keys in the metrics table. Default is false.
  # tags_as_foreign_keys = false

  ## Suffix to append to table name (measurement name) for the foreign tag table.
  # tag_table_suffix = "_tag"

  ## Deny inserting metrics if the foreign tag can't be inserted.
  # foreign_tag_constraint = false

  ## Store all tags as a JSONB object in a single 'tags' column.
  # tags_as_jsonb = false

  ## Store all fields as a JSONB object in a single 'fields' column.
  # fields_as_jsonb = false

  ## Templated statements to execute when creating a new table.
  # create_templates = [
  #   '''CREATE TABLE {{.table}} ({{.columns}})''',
  # ]

  ## Templated statements to execute when adding columns to a table.
  ## Set to an empty list to disable. Points containing tags for which there is no column will be skipped. Points
  ## containing fields for which there is no column will have the field omitted.
  # add_column_templates = [
  #   '''ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}''',
  # ]

  ## Templated statements to execute when creating a new tag table.
  # tag_table_create_templates = [
  #   '''CREATE TABLE {{.table}} ({{.columns}}, PRIMARY KEY (tag_id))''',
  # ]

  ## Templated statements to execute when adding columns to a tag table.
  ## Set to an empty list to disable. Points containing tags for which there is no column will be skipped.
  # tag_table_add_column_templates = [
  #   '''ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}''',
  # ]

  ## Controls whether to use the uint8 data type provided by the pguint extension.
  # use_uint8 = false

  ## When using pool_max_conns>1, and a temporary error occurs, the query is retried with an incremental backoff. This
  ## controls the maximum backoff duration.
  # retry_max_backoff = "15s"

  ## Approximate number of tag IDs to store in in-memory cache (when using tags_as_foreign_keys).
  ## This is an optimization to skip inserting known tag IDs.
  ## Each entry consumes approximately 34 bytes of memory.
  # tag_cache_size = 100000

  ## Enable & set the log level for the Postgres driver.
  # log_level = "warn" # trace, debug, info, warn, error, none
`

type Postgresql struct {
	Connection                 string                  `toml:"connection"`
	Schema                     string                  `toml:"schema"`
	TagsAsForeignKeys          bool                    `toml:"tags_as_foreign_keys"`
	TagTableSuffix             string                  `toml:"tag_table_suffix"`
	ForeignTagConstraint       bool                    `toml:"foreign_tag_constraint"`
	TagsAsJsonb                bool                    `toml:"tags_as_jsonb"`
	FieldsAsJsonb              bool                    `toml:"fields_as_jsonb"`
	CreateTemplates            []*sqltemplate.Template `toml:"create_templates"`
	AddColumnTemplates         []*sqltemplate.Template `toml:"add_column_templates"`
	TagTableCreateTemplates    []*sqltemplate.Template `toml:"tag_table_create_templates"`
	TagTableAddColumnTemplates []*sqltemplate.Template `toml:"tag_table_add_column_templates"`
	UseUint8                   bool                    `toml:"use_uint8"`
	RetryMaxBackoff            config.Duration         `toml:"retry_max_backoff"`
	TagCacheSize               int                     `toml:"tag_cache_size"`
	LogLevel                   string                  `toml:"log_level"`

	dbContext       context.Context
	dbContextCancel func()
	dbConfig        *pgxpool.Config
	db              *pgxpool.Pool
	tableManager    *TableManager
	tagsCache       *freecache.Cache

	pguint8 *pgtype.DataType

	writeChan      chan *TableSource
	writeWaitGroup *utils.WaitGroup

	Logger telegraf.Logger `toml:"-"`
}

func init() {
	outputs.Add("postgresql", func() telegraf.Output { return newPostgresql() })
}

func newPostgresql() *Postgresql {
	return &Postgresql{}
}

func (p *Postgresql) Init() error {
	if p.Schema == "" {
		p.Schema = "public"
	}

	if p.TagTableSuffix == "" {
		p.TagTableSuffix = "_tag"
	}

	if p.CreateTemplates == nil {
		t := &sqltemplate.Template{}
		_ = t.UnmarshalText([]byte(`CREATE TABLE {{.table}} ({{.columns}})`))
		p.CreateTemplates = []*sqltemplate.Template{t}
	}

	if p.AddColumnTemplates == nil {
		t := &sqltemplate.Template{}
		_ = t.UnmarshalText([]byte(`ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}`))
		p.AddColumnTemplates = []*sqltemplate.Template{t}
	}

	if p.TagTableCreateTemplates == nil {
		t := &sqltemplate.Template{}
		_ = t.UnmarshalText([]byte(`CREATE TABLE {{.table}} ({{.columns}}, PRIMARY KEY (tag_id))`))
		p.TagTableCreateTemplates = []*sqltemplate.Template{t}
	}

	if p.TagTableAddColumnTemplates == nil {
		t := &sqltemplate.Template{}
		_ = t.UnmarshalText([]byte(`ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}`))
		p.TagTableAddColumnTemplates = []*sqltemplate.Template{t}
	}

	if p.RetryMaxBackoff == 0 {
		p.RetryMaxBackoff = config.Duration(time.Second * 15)
	}

	if p.TagCacheSize == 0 {
		p.TagCacheSize = 100000
	} else if p.TagCacheSize < 0 {
		return fmt.Errorf("invalid tag_cache_size")
	}

	if p.LogLevel == "" {
		p.LogLevel = "warn"
	}

	if p.TagTableAddColumnTemplates == nil {
		t := &sqltemplate.Template{}
		_ = t.UnmarshalText([]byte(`ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}`))
	}

	if p.Logger == nil {
		p.Logger = models.NewLogger("outputs", "postgresql", "")
	}

	var err error
	if p.dbConfig, err = pgxpool.ParseConfig(p.Connection); err != nil {
		return err
	}
	parsedConfig, _ := pgx.ParseConfig(p.Connection)
	if _, ok := parsedConfig.Config.RuntimeParams["pool_max_conns"]; !ok {
		// The pgx default for pool_max_conns is 4. However we want to default to 1.
		p.dbConfig.MaxConns = 1
	}

	if _, ok := p.dbConfig.ConnConfig.RuntimeParams["application_name"]; !ok {
		p.dbConfig.ConnConfig.RuntimeParams["application_name"] = "telegraf"
	}

	if p.LogLevel != "" {
		p.dbConfig.ConnConfig.Logger = utils.PGXLogger{Logger: p.Logger}
		p.dbConfig.ConnConfig.LogLevel, err = pgx.LogLevelFromString(p.LogLevel)
		if err != nil {
			return fmt.Errorf("invalid log level")
		}
	}

	if p.UseUint8 {
		p.dbConfig.AfterConnect = p.registerUint8
	}

	return nil
}

func (p *Postgresql) SampleConfig() string { return sampleConfig }
func (p *Postgresql) Description() string  { return "Send metrics to PostgreSQL" }

// Connect establishes a connection to the target database and prepares the cache
func (p *Postgresql) Connect() error {
	// Yes, we're not supposed to store the context. However since we don't receive a context, we have to.
	p.dbContext, p.dbContextCancel = context.WithCancel(context.Background())
	var err error
	p.db, err = pgxpool.ConnectConfig(p.dbContext, p.dbConfig)
	if err != nil {
		p.Logger.Errorf("Couldn't connect to server\n%v", err)
		return err
	}
	p.tableManager = NewTableManager(p)

	if p.TagsAsForeignKeys {
		p.tagsCache = freecache.NewCache(p.TagCacheSize * 34) // from testing, each entry consumes approx 34 bytes
	}

	maxConns := int(p.db.Stat().MaxConns())
	if maxConns > 1 {
		p.writeChan = make(chan *TableSource)
		p.writeWaitGroup = utils.NewWaitGroup()
		for i := 0; i < maxConns; i++ {
			p.writeWaitGroup.Add(1)
			go p.writeWorker(p.dbContext)
		}
	}

	return nil
}

func (p *Postgresql) registerUint8(ctx context.Context, conn *pgx.Conn) error {
	if p.pguint8 == nil {
		dt := pgtype.DataType{
			// Use 'numeric' type for encoding/decoding across the wire
			// It might be more efficient to create a native pgtype.Type, but would involve a lot of code. So this is
			// probably good enough.
			Value: &Uint8{},
			Name:  "uint8",
		}
		row := conn.QueryRow(p.dbContext, "SELECT oid FROM pg_type WHERE typname=$1", dt.Name)
		if err := row.Scan(&dt.OID); err != nil {
			return fmt.Errorf("retreiving OID for uint8 data type: %w", err)
		}
		p.pguint8 = &dt
	}

	conn.ConnInfo().RegisterDataType(*p.pguint8)
	return nil
}

// Close closes the connection(s) to the database.
func (p *Postgresql) Close() error {
	if p.writeChan != nil {
		// We're using async mode. Gracefully close with timeout.
		close(p.writeChan)
		select {
		case <-p.writeWaitGroup.C():
		case <-time.NewTimer(time.Second * 5).C:
			p.Logger.Warnf("Shutdown timeout expired while waiting for metrics to flush. Some metrics may not be written to database.")
		}
	}

	// Die!
	p.dbContextCancel()
	p.db.Close()
	p.tableManager = nil
	return nil
}

func (p *Postgresql) Write(metrics []telegraf.Metric) error {
	if p.tagsCache != nil {
		// gather at the start of write so there's less chance of any async operations ongoing
		p.Logger.Debugf("cache: size=%d hit=%d miss=%d full=%d\n",
			p.tagsCache.EntryCount(),
			p.tagsCache.HitCount(),
			p.tagsCache.MissCount(),
			p.tagsCache.EvacuateCount(),
		)
		p.tagsCache.ResetStatistics()
	}

	tableSources := NewTableSources(p, metrics)

	var err error
	if p.db.Stat().MaxConns() > 1 {
		err = p.writeConcurrent(tableSources)
	} else {
		err = p.writeSequential(tableSources)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			// PgError doesn't include .Detail in Error(), so we concat it onto .Message.
			if pgErr.Detail != "" {
				pgErr.Message += "; " + pgErr.Detail
			}
		}
	}

	return err
}

func (p *Postgresql) writeSequential(tableSources map[string]*TableSource) error {
	tx, err := p.db.Begin(p.dbContext)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback(p.dbContext) //nolint:errcheck

	for _, tableSource := range tableSources {
		sp := tx
		if len(tableSources) > 1 {
			// wrap each sub-batch in a savepoint so that if a permanent error is received, we can drop just that one sub-batch, and insert everything else.
			sp, err = tx.Begin(p.dbContext)
			if err != nil {
				return fmt.Errorf("starting savepoint: %w", err)
			}
		}

		err := p.writeMetricsFromMeasure(p.dbContext, sp, tableSource)
		if err != nil {
			if isTempError(err) {
				// return so that telegraf will retry the whole batch
				return err
			}
			p.Logger.Errorf("write error (permanent, dropping sub-batch): %v", err)
			if len(tableSources) > 1 {
				if err := sp.Rollback(p.dbContext); err != nil {
					return err
				}
			}
		}
		// savepoints do not need to be committed (released), so save the round trip and skip it
	}

	if err := tx.Commit(p.dbContext); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
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
	defer p.writeWaitGroup.Done()
	for {
		select {
		case tableSource, ok := <-p.writeChan:
			if !ok {
				return
			}
			if err := p.writeRetry(ctx, tableSource); err != nil {
				p.Logger.Errorf("write error (permanent, dropping sub-batch): %v", err)
			}
		case <-p.dbContext.Done():
			return
		}
	}
}

// isTempError reports whether the error received during a metric write operation is temporary or permanent.
// A temporary error is one that if the write were retried at a later time, that it might succeed.
// Note however that this applies to the transaction as a whole, not the individual operation. Meaning for example a
// write might come in that needs a new table created, but another worker already created the table in between when we
// checked for it, and tried to create it. In this case, the operation error is permanent, as we can try `CREATE TABLE`
// again and it will still fail. But if we retry the transaction from scratch, when we perform the table check we'll see
// it exists, so we consider the error temporary.
func isTempError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr); pgErr != nil {
		// https://www.postgresql.org/docs/12/errcodes-appendix.html
		errClass := pgErr.Code[:2]
		switch errClass {
		case "23": // Integrity Constraint Violation
			switch pgErr.Code { //nolint:revive
			case "23505": // unique_violation
				if strings.Contains(err.Error(), "pg_type_typname_nsp_index") {
					// Happens when you try to create 2 tables simultaneously.
					return true
				}
			}
		case "25": // Invalid Transaction State
			// If we're here, this is a bug, but recoverable
			return true
		case "40": // Transaction Rollback
			switch pgErr.Code { //nolint:revive
			case "40P01": // deadlock_detected
				return true
			}
		case "42": // Syntax Error or Access Rule Violation
			switch pgErr.Code {
			case "42701": // duplicate_column
				return true
			case "42P07": // duplicate_table
				return true
			}
		case "53": // Insufficient Resources
			return true
		case "57": // Operator Intervention
			switch pgErr.Code { //nolint:revive
			case "57014": // query_cancelled
				// This one is a bit of a mess. This code comes back when PGX cancels the query. Such as when PGX can't
				// convert to the column's type. So even though the error was originally generated by PGX, we get the
				// error from Postgres.
				return false
			case "57P04": // database_dropped
				return false
			}
			return true
		}
		// Assume that any other error that comes from postgres is a permanent error
		return false
	}

	if err, ok := err.(interface{ Temporary() bool }); ok {
		return err.Temporary()
	}

	// Assume that any other error is permanent.
	// This may mean that we incorrectly discard data that could have been retried, but the alternative is that we get
	// stuck retrying data that will never succeed, causing good data to be dropped because the buffer fills up.
	return false
}

func (p *Postgresql) writeRetry(ctx context.Context, tableSource *TableSource) error {
	backoff := time.Duration(0)
	for {
		err := p.writeMetricsFromMeasure(ctx, p.db, tableSource)
		if err == nil {
			return nil
		}

		if !isTempError(err) {
			return err
		}
		p.Logger.Errorf("write error (retry in %s): %v", backoff, err)
		tableSource.Reset()
		time.Sleep(backoff)

		if backoff == 0 {
			backoff = time.Millisecond * 250
		} else {
			backoff *= 2
			if backoff > time.Duration(p.RetryMaxBackoff) {
				backoff = time.Duration(p.RetryMaxBackoff)
			}
		}
	}
}

// Writes the metrics from a specified measure. All the provided metrics must belong to the same measurement.
func (p *Postgresql) writeMetricsFromMeasure(ctx context.Context, db dbh, tableSource *TableSource) error {
	err := p.tableManager.MatchSource(ctx, db, tableSource)
	if err != nil {
		return err
	}

	if p.TagsAsForeignKeys {
		if err := p.writeTagTable(ctx, db, tableSource); err != nil {
			if p.ForeignTagConstraint {
				return fmt.Errorf("writing to tag table '%s': %s", tableSource.Name()+p.TagTableSuffix, err)
			}
			// log and continue. As the admin can correct the issue, and tags don't change over time, they can be
			// added from future metrics after issue is corrected.
			p.Logger.Errorf("writing to tag table '%s': %s", tableSource.Name()+p.TagTableSuffix, err)
		}
	}

	fullTableName := utils.FullTableName(p.Schema, tableSource.Name())
	if _, err := db.CopyFrom(ctx, fullTableName, tableSource.ColumnNames(), tableSource); err != nil {
		return err
	}

	return nil
}

func (p *Postgresql) writeTagTable(ctx context.Context, db dbh, tableSource *TableSource) error {
	ttsrc := NewTagTableSource(tableSource)

	// Check whether we have any tags to insert
	if !ttsrc.Next() {
		return nil
	}
	ttsrc.Reset()

	// need a transaction so that if it errors, we don't roll back the parent transaction, just the tags
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	ident := pgx.Identifier{ttsrc.postgresql.Schema, ttsrc.Name()}
	identTemp := pgx.Identifier{ttsrc.Name() + "_temp"}
	sql := fmt.Sprintf("CREATE TEMP TABLE %s (LIKE %s) ON COMMIT DROP", identTemp.Sanitize(), ident.Sanitize())
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("creating tags temp table: %w", err)
	}

	if _, err := tx.CopyFrom(ctx, identTemp, ttsrc.ColumnNames(), ttsrc); err != nil {
		return fmt.Errorf("copying into tags temp table: %w", err)
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s ORDER BY tag_id ON CONFLICT (tag_id) DO NOTHING", ident.Sanitize(), identTemp.Sanitize())); err != nil {
		return fmt.Errorf("inserting into tags table: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	ttsrc.UpdateCache()
	return nil
}
