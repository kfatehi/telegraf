package tables

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v4/pgxpool"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

const (
	addColumnTemplate           = "ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s;"
	findExistingColumnsTemplate = "SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = $1 and table_name = $2"
)

type TableManager struct {
	Tables        map[string]map[string]utils.PgDataType
	tablesMutex   sync.RWMutex
	db            *pgxpool.Pool
	schema        string
	tableTemplate string
}

// NewManager returns an instance of the tables.Manager interface
// that can handle checking and updating the state of tables in the PG database.
func NewManager(db *pgxpool.Pool, schema, tableTemplate string) *TableManager {
	return &TableManager{
		Tables:        make(map[string]map[string]utils.PgDataType),
		db:            db,
		tableTemplate: tableTemplate,
		schema:        schema,
	}
}

// ClearTableCache clear the table structure cache.
func (tm *TableManager) ClearTableCache() {
	tm.tablesMutex.Lock()
	tm.Tables = make(map[string]map[string]utils.PgDataType)
	tm.tablesMutex.Unlock()
}

// Creates a table in the database with the column names and types specified in 'colDetails'
func (tm *TableManager) createTable(ctx context.Context, tableName string, colDetails []utils.Column) error {
	utils.ColumnList(colDetails).Sort()
	sql := tm.generateCreateTableSQL(tableName, colDetails)
	if _, err := tm.db.Exec(ctx, sql); err != nil {
		return err
	}

	structure := map[string]utils.PgDataType{}
	for _, col := range colDetails {
		structure[col.Name] = col.Type
	}

	tm.tablesMutex.Lock()
	tm.Tables[tableName] = structure
	tm.tablesMutex.Unlock()
	return nil
}

// addColumnsToTable adds the indicated columns to the table in the database.
// This is an idempotent operation, so attempting to add a column which already exists is a silent no-op.
func (tm *TableManager) addColumnsToTable(ctx context.Context, tableName string, colDetails []utils.Column) error {
	utils.ColumnList(colDetails).Sort()
	fullTableName := utils.FullTableName(tm.schema, tableName).Sanitize()
	for _, col := range colDetails {
		addColumnQuery := fmt.Sprintf(addColumnTemplate, fullTableName, utils.QuoteIdent(col.Name), col.Type)
		_, err := tm.db.Exec(ctx, addColumnQuery)
		if err != nil {
			return fmt.Errorf("adding '%s': %w", col.Name, err)
		}

		//FIXME if the column exists, but is a different type, we won't get an error, but we need to ensure the type is one
		// we can use, and not just assume it's correct.
		tm.tablesMutex.Lock()
		tm.Tables[tableName][col.Name] = col.Type
		tm.tablesMutex.Unlock()
	}

	return nil
}

// Populate the 'tableTemplate' (supplied as config option to the plugin) with the details of
// the required columns for the measurement to create a 'CREATE TABLE' SQL statement.
// The order, column names and data types are given in 'colDetails'.
func (tm *TableManager) generateCreateTableSQL(tableName string, colDetails []utils.Column) string {
	colDefs := make([]string, len(colDetails))
	var pk []string
	for i, col := range colDetails {
		colDefs[i] = utils.QuoteIdent(col.Name) + " " + string(col.Type)
		if col.Role != utils.FieldColType {
			pk = append(pk, col.Name)
		}
	}

	fullTableName := utils.FullTableName(tm.schema, tableName).Sanitize()
	query := strings.Replace(tm.tableTemplate, "{TABLE}", fullTableName, -1)
	query = strings.Replace(query, "{TABLELITERAL}", utils.QuoteLiteral(fullTableName), -1)
	query = strings.Replace(query, "{COLUMNS}", strings.Join(colDefs, ","), -1)
	query = strings.Replace(query, "{KEY_COLUMNS}", strings.Join(pk, ","), -1)

	return query
}

func (tm *TableManager) refreshTableStructure(ctx context.Context, tableName string) error {
	rows, err := tm.db.Query(ctx, findExistingColumnsTemplate, tm.schema, tableName)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := make(map[string]utils.PgDataType)
	for rows.Next() {
		var colName, colTypeStr string
		err := rows.Scan(&colName, &colTypeStr)
		if err != nil {
			return err
		}
		cols[colName] = utils.PgDataType(colTypeStr)
	}

	if len(cols) > 0 {
		tm.tablesMutex.Lock()
		tm.Tables[tableName] = cols
		tm.tablesMutex.Unlock()
	}
	return nil
}

func (tm *TableManager) EnsureStructure(ctx context.Context, tableName string, columns []utils.Column, doSchemaUpdate bool) ([]utils.Column, error) {
	tm.tablesMutex.RLock()
	structure, ok := tm.Tables[tableName]
	tm.tablesMutex.RUnlock()
	if !ok {
		// We don't know about the table. First try to query it.
		if err := tm.refreshTableStructure(ctx, tableName); err != nil {
			return nil, fmt.Errorf("querying table structure: %w", err)
		}
		tm.tablesMutex.RLock()
		structure, ok = tm.Tables[tableName]
		tm.tablesMutex.RUnlock()
		if !ok {
			// Ok, table doesn't exist, now we can create it.
			if err := tm.createTable(ctx, tableName, columns); err != nil {
				return nil, fmt.Errorf("creating table: %w", err)
			}
			tm.tablesMutex.RLock()
			structure = tm.Tables[tableName]
			tm.tablesMutex.RUnlock()
		}
	}

	missingColumns, err := tm.checkColumns(structure, columns)
	if err != nil {
		return nil, fmt.Errorf("column validation: %w", err)
	}
	if len(missingColumns) == 0 {
		return nil, nil
	}

	if doSchemaUpdate {
		if err := tm.addColumnsToTable(ctx, tableName, missingColumns); err != nil {
			return nil, fmt.Errorf("adding columns: %w", err)
		}
		return nil, nil
	}

	return missingColumns, nil
}

func (tm *TableManager) checkColumns(structure map[string]utils.PgDataType, columns []utils.Column) ([]utils.Column, error) {
	var missingColumns []utils.Column
	for _, col := range columns {
		dbColType, ok := structure[col.Name]
		if !ok {
			missingColumns = append(missingColumns, col)
			continue
		}
		if !utils.PgTypeCanContain(dbColType, col.Type) {
			return nil, fmt.Errorf("column type '%s' cannot store '%s'", dbColType, col.Type)
		}
	}
	return missingColumns, nil
}
