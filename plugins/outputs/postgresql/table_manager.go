package postgresql

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

const (
	refreshTableStructureStatement = "SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = $1 and table_name = $2"
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
		err := rows.Scan(&colName, &colTypeStr)
		if err != nil {
			return err
		}
		cols[colName] = utils.Column{
			Name: colName,
			Type: utils.PgDataType(colTypeStr),
			Role: utils.FieldColType, //FIXME this isn't necessarily correct. could be some other role. But while it's a lie, I don't think it affect anything.
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
	createTemplates []*Template,
	addColumnsTemplates []*Template,
	metricsTableName string,
	tagsTableName string,
) ([]utils.Column, error) {
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
	if len(missingColumns) > 0 {
		// Sort so that:
		//   * When we create/alter the table the columns are in a sane order (telegraf gives us the fields in random order)
		//   * When we display errors about missing columns, the order is also sane, and consistent
		utils.ColumnList(missingColumns).Sort()
	}
	return missingColumns, nil
}

func (tm *TableManager) executeTemplates(
	ctx context.Context,
	tmpls []*Template,
	tableName string,
	newColumns []utils.Column,
	metricsTableName string,
	tagsTableName string,
) error {
	tmplTable := NewTemplateTable(tm.Schema, tableName, colMapToSlice(tm.Tables[tableName]))
	metricsTmplTable := NewTemplateTable(tm.Schema, metricsTableName, colMapToSlice(tm.Tables[metricsTableName]))
	tagsTmplTable := NewTemplateTable(tm.Schema, tagsTableName, colMapToSlice(tm.Tables[tagsTableName]))

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
			return fmt.Errorf("executing `%.40s`: %w", sql, err)
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

func (tm *TableManager) MatchSource(ctx context.Context, rowSource *RowSource) error {
	metricsTableName := rowSource.Name()
	var tagsTableName string
	if tm.TagsAsForeignkeys {
		tagsTableName = metricsTableName + tm.TagTableSuffix

		missingCols, err := tm.EnsureStructure(
			ctx,
			tagsTableName,
			rowSource.TagColumns(),
			tm.TagTableCreateTemplates,
			tm.TagTableAddColumnTemplates,
			metricsTableName,
			tagsTableName,
		)
		if err != nil {
			return err
		}

		if len(missingCols) > 0 {
			colDefs := make([]string, len(missingCols))
			for i, col := range missingCols {
				colDefs[i] = col.Name + " " + string(col.Type)
			}
			//TODO just drop the individual rows instead. See RowSource.Next()
			return fmt.Errorf("missing tag columns: %s", strings.Join(colDefs, ", "))
		}
	}

	missingCols, err := tm.EnsureStructure(ctx,
		metricsTableName,
		rowSource.Columns(),
		tm.CreateTemplates,
		tm.AddColumnTemplates,
		metricsTableName,
		tagsTableName,
	)
	if err != nil {
		return err
	}

	if len(missingCols) > 0 {
		colDefs := make([]string, len(missingCols))
		for i, col := range missingCols {
			colDefs[i] = col.Name + " " + string(col.Type)
		}
		log.Printf("[outputs.postgresql] Error: table '%s' is missing columns (dropping fields): %s", metricsTableName, strings.Join(colDefs, ", "))

		for _, col := range missingCols {
			rowSource.DropFieldColumn(col)
		}
	}

	return nil
}
