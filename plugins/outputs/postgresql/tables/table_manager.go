package tables

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/pkg/errors"
	"log"
	"strings"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

const (
	addColumnTemplate           = "ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s;"
	findExistingColumnsTemplate = "SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = $1 and table_name = $2"
)

type columnInDbDef struct {
	dataType utils.PgDataType
	exists   bool
}

type TableManager struct {
	Tables        map[string]map[string]utils.PgDataType
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
	tm.Tables = make(map[string]map[string]utils.PgDataType)
}

// Creates a table in the database with the column names and types specified in 'colDetails'
func (tm *TableManager) createTable(ctx context.Context, tableName string, colDetails *utils.TargetColumns) error {
	colDetails.Sort()
	sql := tm.generateCreateTableSQL(tableName, colDetails)
	if _, err := tm.db.Exec(ctx, sql); err != nil {
		return err
	}

	columns := make(map[string]utils.PgDataType, len(colDetails.Names))
	for i, colName := range colDetails.Names {
		columns[colName] = colDetails.DataTypes[i]
	}

	tm.Tables[tableName] = columns
	return nil
}

// addColumnsToTable adds the indicated columns to the table in the database.
// This is an idempotent operation, so attempting to add a column which already exists is a silent no-op.
func (tm *TableManager) addColumnsToTable(ctx context.Context, tableName string, columnIndices []int, colDetails *utils.TargetColumns) error {
	fullTableName := utils.FullTableName(tm.schema, tableName).Sanitize()
	for _, colIndex := range columnIndices {
		name := colDetails.Names[colIndex]
		dataType := colDetails.DataTypes[colIndex]
		addColumnQuery := fmt.Sprintf(addColumnTemplate, fullTableName, utils.QuoteIdent(name), dataType)
		_, err := tm.db.Exec(ctx, addColumnQuery)
		if err != nil {
			return fmt.Errorf("adding '%s': %w", name, err)
		}

		//FIXME if the column exists, but is a different type, we won't get an error, but we need to ensure the type is one
		// we can use, and not just assume it's correct.
		tm.Tables[tableName][name] = dataType
	}

	return nil
}

// Populate the 'tableTemplate' (supplied as config option to the plugin) with the details of
// the required columns for the measurement to create a 'CREATE TABLE' SQL statement.
// The order, column names and data types are given in 'colDetails'.
func (tm *TableManager) generateCreateTableSQL(tableName string, colDetails *utils.TargetColumns) string {
	colDefs := make([]string, len(colDetails.Names))
	var pk []string
	for colIndex, colName := range colDetails.Names {
		colDefs[colIndex] = utils.QuoteIdent(colName) + " " + string(colDetails.DataTypes[colIndex])
		if colDetails.Roles[colIndex] != utils.FieldColType {
			pk = append(pk, colName)
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
		log.Printf("E! Couldn't discover existing columns of table: %s\n%v", tableName, err)
		return errors.Wrap(err, "could not discover existing columns")
	}
	defer rows.Close()
	cols := make(map[string]utils.PgDataType)
	for rows.Next() {
		var colName, colTypeStr string
		err := rows.Scan(&colName, &colTypeStr)
		if err != nil {
			log.Printf("E! Couldn't discover columns of table: %s\n%v", tableName, err)
			return err
		}
		cols[colName] = utils.PgDataType(colTypeStr)
	}

	tm.Tables[tableName] = cols
	return nil
}

func (tm *TableManager) EnsureStructure(ctx context.Context, tableName string, columns *utils.TargetColumns) error {
	structure, ok := tm.Tables[tableName]
	if !ok {
		// We don't know about the table. First try to query it.
		if err := tm.refreshTableStructure(ctx, tableName); err != nil {
			return fmt.Errorf("querying table structure: %w", err)
		}
		structure, ok = tm.Tables[tableName]
		if !ok {
			// Ok, table doesn't exist, now we can create it.
			if err := tm.createTable(ctx, tableName, columns); err != nil {
				return fmt.Errorf("creating table: %w", err)
			}
			structure = tm.Tables[tableName]
		}
	}

	missingColumns, err := tm.checkColumns(structure, columns)
	if err != nil {
		return fmt.Errorf("column validation: %w", err)
	}
	if len(missingColumns) == 0 {
		return nil
	}

	if err := tm.addColumnsToTable(ctx, tableName, missingColumns, columns); err != nil {
		return fmt.Errorf("adding columns: %w", err)
	}

	return nil
}

func (tm *TableManager) checkColumns(structure map[string]utils.PgDataType, columns *utils.TargetColumns) ([]int, error) {
	var missingColumns []int
	for i, colName := range columns.Names {
		colType, ok := structure[colName]
		if !ok {
			missingColumns = append(missingColumns, i)
		}
		if !utils.PgTypeCanContain(colType, columns.DataTypes[i]) {
			return nil, fmt.Errorf("column type '%s' cannot store '%s'", colType, columns.DataTypes[i])
		}
	}
	return missingColumns, nil
}
