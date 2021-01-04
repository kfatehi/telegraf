package postgresql

import (
	"context"
	"fmt"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/template"
	"log"
	"strings"
	"sync"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

const (
	refreshTableStructureStatement = "SELECT column_name, data_type, col_description((table_schema||'.'||table_name)::regclass::oid, ordinal_position) FROM information_schema.columns WHERE table_schema = $1 and table_name = $2"
)

type TableManager struct {
	*Postgresql

	Tables      map[string]map[string]utils.Column
	tablesMutex sync.RWMutex
}

// NewTableManager returns an instance of the tables.Manager interface
// that can handle checking and updating the state of tables in the PG database.
func NewTableManager(postgresql *Postgresql) *TableManager {
	return &TableManager{
		Postgresql: postgresql,
		Tables:     make(map[string]map[string]utils.Column),
	}
}

// ClearTableCache clear the table structure cache.
func (tm *TableManager) ClearTableCache() {
	tm.tablesMutex.Lock()
	tm.Tables = make(map[string]map[string]utils.Column)
	tm.tablesMutex.Unlock()
}

func (tm *TableManager) refreshTableStructure(ctx context.Context, tableName string) error {
	rows, err := tm.db.Query(ctx, refreshTableStructureStatement, tm.Schema, tableName)
	if err != nil {
		return err
	}
	defer rows.Close()

	cols := make(map[string]utils.Column)
	for rows.Next() {
		var colName, colTypeStr string
		desc := new(string)
		err := rows.Scan(&colName, &colTypeStr, &desc)
		if err != nil {
			return err
		}

		role := utils.FieldColType
		switch colName {
		case TimeColumnName:
			role = utils.TimeColType
		case TagIDColumnName:
			role = utils.TagsIDColType
		case TagsJSONColumnName:
			role = utils.TagColType
		case FieldsJSONColumnName:
			role = utils.FieldColType
		default:
			// We don't want to monopolize the column comment (preventing user from storing other information there), so just look at the first word
			if desc != nil {
				descWords := strings.Split(*desc, " ")
				if descWords[0] == "tag" {
					role = utils.TagColType
				}
			}
		}

		cols[colName] = utils.Column{
			Name: colName,
			Type: utils.PgDataType(colTypeStr),
			Role: role,
		}
	}

	if len(cols) > 0 {
		tm.tablesMutex.Lock()
		tm.Tables[tableName] = cols
		tm.tablesMutex.Unlock()
	}

	return nil
}

func (tm *TableManager) EnsureStructure(
	ctx context.Context,
	tableName string,
	columns []utils.Column,
	createTemplates []*template.Template,
	addColumnsTemplates []*template.Template,
	metricsTableName string,
	tagsTableName string,
) ([]utils.Column, error) {
	// Sort so that:
	//   * When we create/alter the table the columns are in a sane order (telegraf gives us the fields in random order)
	//   * When we display errors about missing columns, the order is also sane, and consistent
	utils.ColumnList(columns).Sort()

	tm.tablesMutex.RLock()
	dbColumns, ok := tm.Tables[tableName]
	tm.tablesMutex.RUnlock()
	if !ok {
		// We don't know about the table. First try to query it.
		if err := tm.refreshTableStructure(ctx, tableName); err != nil {
			return nil, fmt.Errorf("querying table structure: %w", err)
		}
		tm.tablesMutex.RLock()
		dbColumns, ok = tm.Tables[tableName]
		tm.tablesMutex.RUnlock()
		if !ok {
			// Ok, table doesn't exist, now we can create it.
			if err := tm.executeTemplates(ctx, createTemplates, tableName, columns, metricsTableName, tagsTableName); err != nil {
				return nil, fmt.Errorf("creating table: %w", err)
			}
			tm.tablesMutex.RLock()
			dbColumns = tm.Tables[tableName]
			tm.tablesMutex.RUnlock()
		}
	}

	missingColumns, err := tm.checkColumns(dbColumns, columns)
	if err != nil {
		return nil, fmt.Errorf("column validation: %w", err)
	}
	if len(missingColumns) == 0 {
		return nil, nil
	}

	if len(addColumnsTemplates) == 0 {
		return missingColumns, nil
	}

	if err := tm.executeTemplates(ctx, addColumnsTemplates, tableName, missingColumns, metricsTableName, tagsTableName); err != nil {
		return nil, fmt.Errorf("adding columns: %w", err)
	}
	return tm.checkColumns(tm.Tables[tableName], columns)
}

func (tm *TableManager) checkColumns(dbColumns map[string]utils.Column, srcColumns []utils.Column) ([]utils.Column, error) {
	var missingColumns []utils.Column
	for _, srcCol := range srcColumns {
		dbCol, ok := dbColumns[srcCol.Name]
		if !ok {
			missingColumns = append(missingColumns, srcCol)
			continue
		}
		if !utils.PgTypeCanContain(dbCol.Type, srcCol.Type) {
			return nil, fmt.Errorf("column type '%s' cannot store '%s'", dbCol.Type, srcCol.Type)
		}
	}
	return missingColumns, nil
}

func (tm *TableManager) executeTemplates(
	ctx context.Context,
	tmpls []*template.Template,
	tableName string,
	newColumns []utils.Column,
	metricsTableName string,
	tagsTableName string,
) error {
	tmplTable := template.NewTemplateTable(tm.Schema, tableName, colMapToSlice(tm.Tables[tableName]))
	metricsTmplTable := template.NewTemplateTable(tm.Schema, metricsTableName, colMapToSlice(tm.Tables[metricsTableName]))
	tagsTmplTable := template.NewTemplateTable(tm.Schema, tagsTableName, colMapToSlice(tm.Tables[tagsTableName]))

	/* https://github.com/jackc/pgx/issues/872
	stmts := make([]string, len(tmpls))
	batch := &pgx.Batch{}
	for i, tmpl := range tmpls {
		sql, err := tmpl.Render(tmplTable, newColumns, metricsTmplTable, tagsTmplTable)
		if err != nil {
			return err
		}
		stmts[i] = string(sql)
		batch.Queue(stmts[i])
	}

	batch.Queue(refreshTableStructureStatement, tm.Schema, tableName)

	batchResult := tm.db.SendBatch(ctx, batch)
	defer batchResult.Close()

	for i := 0; i < len(tmpls); i++ {
		if x, err := batchResult.Exec(); err != nil {
			return fmt.Errorf("executing `%.40s...`: %v %w", stmts[i], x, err)
		}
	}

	rows, err := batchResult.Query()
	if err != nil {
		return fmt.Errorf("refreshing table: %w", err)
	}
	tm.refreshTableStructureResponse(tableName, rows)
	*/

	tx, err := tm.db.Begin(ctx)
	if err != nil {
		return err
	}

	for _, tmpl := range tmpls {
		sql, err := tmpl.Render(tmplTable, newColumns, metricsTmplTable, tagsTmplTable)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("executing `%s`: %w", sql, err)
		}
	}

	// We need to be able to determine the role of the column when reading the structure back (because of the templates).
	// For some columns we can determine this by the column name (time, tag_id, etc). However tags and fields can have any
	// name, and look the same. So we add a comment to tag columns, and through process of elimination what remains are
	// field columns.
	for _, col := range newColumns {
		if col.Role != utils.TagColType {
			continue
		}
		if _, err := tx.Exec(ctx, "COMMENT ON COLUMN "+tmplTable.String()+"."+template.QuoteIdentifier(col.Name)+" IS 'tag'"); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("setting column role comment: %s", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	return tm.refreshTableStructure(ctx, tableName)
}

func colMapToSlice(colMap map[string]utils.Column) []utils.Column {
	if colMap == nil {
		return nil
	}
	cols := make([]utils.Column, 0, len(colMap))
	for _, col := range colMap {
		cols = append(cols, col)
	}
	return cols
}

// MatchSource scans through the metrics, determining what columns are needed for inserting, and ensuring the DB schema matches.
// If the schema does not match, and schema updates are disabled:
//   If a field missing from the DB, the field is omitted.
//   If a tag is missing from the DB, the metric is dropped.
func (tm *TableManager) MatchSource(ctx context.Context, rowSource *TableSource) error {
	metricTableName := rowSource.Name()
	var tagTableName string
	if tm.TagsAsForeignkeys {
		tagTableName = metricTableName + tm.TagTableSuffix

		missingCols, err := tm.EnsureStructure(
			ctx,
			tagTableName,
			rowSource.TagTableColumns(),
			tm.TagTableCreateTemplates,
			tm.TagTableAddColumnTemplates,
			metricTableName,
			tagTableName,
		)
		if err != nil {
			return err
		}

		if len(missingCols) > 0 {
			colDefs := make([]string, len(missingCols))
			for i, col := range missingCols {
				rowSource.DropColumn(col)
				colDefs[i] = col.Name + " " + string(col.Type)
			}
			log.Printf("[outputs.postgresql] Error: table '%s' is missing tag columns (dropping metrics): %s", tagTableName, strings.Join(colDefs, ", "))
		}
	}

	missingCols, err := tm.EnsureStructure(ctx,
		metricTableName,
		rowSource.MetricTableColumns(),
		tm.CreateTemplates,
		tm.AddColumnTemplates,
		metricTableName,
		tagTableName,
	)
	if err != nil {
		return err
	}

	if len(missingCols) > 0 {
		colDefs := make([]string, len(missingCols))
		for i, col := range missingCols {
			rowSource.DropColumn(col)
			colDefs[i] = col.Name + " " + string(col.Type)
		}
		log.Printf("[outputs.postgresql] Error: table '%s' is missing columns (dropping fields): %s", metricTableName, strings.Join(colDefs, ", "))
	}

	return nil
}
