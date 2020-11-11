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
	tableExistsTemplate         = "SELECT tablename FROM pg_tables WHERE tablename = $1 AND schemaname = $2;"
	findExistingColumnsTemplate = "SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = $1 and table_name = $2"
)

type columnInDbDef struct {
	dataType utils.PgDataType
	exists   bool
}

// Manager defines an abstraction that can check the state of tables in a PG
// database, create, and update them.
type Manager interface {
	// Exists checks if a table with the given name already is present in the DB.
	Exists(ctx context.Context, tableName string) bool
	// Creates a table in the database with the column names and types specified in 'colDetails'
	CreateTable(ctx context.Context, tableName string, colDetails *utils.TargetColumns) error
	// This function queries a table in the DB if the required columns in 'colDetails' are present and what is their
	// data type. For existing columns it checks if the data type in the DB can safely hold the data from the metrics.
	// It returns:
	//   - the indices of the missing columns (from colDetails)
	//   - or an error if
	//     = it couldn't discover the columns of the table in the db
	//     = the existing column types are incompatible with the required column types
	FindColumnMismatch(ctx context.Context, tableName string, colDetails *utils.TargetColumns) ([]int, error)
	// From the column details (colDetails) of a given measurement, 'columnIndices' specifies which are missing in the DB.
	// this function will add the new columns with the required data type.
	AddColumnsToTable(ctx context.Context, tableName string, columnIndices []int, colDetails *utils.TargetColumns) error
	ClearTableCache()
}

type defTableManager struct {
	Tables        map[string]bool
	db            *pgxpool.Pool
	schema        string
	tableTemplate string
}

// NewManager returns an instance of the tables.Manager interface
// that can handle checking and updating the state of tables in the PG database.
func NewManager(db *pgxpool.Pool, schema, tableTemplate string) Manager {
	return &defTableManager{
		Tables:        make(map[string]bool),
		db:            db,
		tableTemplate: tableTemplate,
		schema:        schema,
	}
}

// ClearTableCache clear the local cache of which table exists.
func (t *defTableManager) ClearTableCache() {
	t.Tables = make(map[string]bool)
}

// Exists checks if a table with the given name already is present in the DB.
func (t *defTableManager) Exists(ctx context.Context, tableName string) bool {
	if _, ok := t.Tables[tableName]; ok {
		return true
	}

	commandTag, err := t.db.Exec(ctx, tableExistsTemplate, tableName, t.schema)
	if err != nil {
		log.Printf("E! Error checking for existence of metric table: %s\nSQL: %s\n%v", tableName, tableExistsTemplate, err)
		return false
	}

	if commandTag.RowsAffected() == 1 {
		t.Tables[tableName] = true
		return true
	}

	return false
}

// Creates a table in the database with the column names and types specified in 'colDetails'
func (t *defTableManager) CreateTable(ctx context.Context, tableName string, colDetails *utils.TargetColumns) error {
	colDetails.Sort()
	sql := t.generateCreateTableSQL(tableName, colDetails)
	if _, err := t.db.Exec(ctx, sql); err != nil {
		log.Printf("E! Couldn't create table: %s\nSQL: %s\n%v", tableName, sql, err)
		return err
	}

	t.Tables[tableName] = true
	return nil
}

// This function queries a table in the DB if the required columns in 'colDetails' are present and what is their
// data type. For existing columns it checks if the data type in the DB can safely hold the data from the metrics.
// It returns:
//   - the indices of the missing columns (from colDetails)
//   - or an error if
//     = it couldn't discover the columns of the table in the db
//     = the existing column types are incompatible with the required column types
func (t *defTableManager) FindColumnMismatch(ctx context.Context, tableName string, colDetails *utils.TargetColumns) ([]int, error) {
	columnPresence, err := t.findColumnPresence(ctx, tableName, colDetails.Names)
	if err != nil {
		return nil, err
	}

	var missingCols []int
	for colIndex := range colDetails.Names {
		colStateInDb := columnPresence[colIndex]
		if !colStateInDb.exists {
			missingCols = append(missingCols, colIndex)
			continue
		}
		typeInDb := colStateInDb.dataType
		typeInMetric := colDetails.DataTypes[colIndex]
		if !utils.PgTypeCanContain(typeInDb, typeInMetric) {
			return nil, fmt.Errorf("E! A column exists in '%s' of type '%s' required type '%s'", tableName, typeInDb, typeInMetric)
		}
	}

	return missingCols, nil
}

// From the column details (colDetails) of a given measurement, 'columnIndices' specifies which are missing in the DB.
// this function will add the new columns with the required data type.
func (t *defTableManager) AddColumnsToTable(ctx context.Context, tableName string, columnIndices []int, colDetails *utils.TargetColumns) error {
	fullTableName := utils.FullTableName(t.schema, tableName).Sanitize()
	for _, colIndex := range columnIndices {
		name := colDetails.Names[colIndex]
		dataType := colDetails.DataTypes[colIndex]
		addColumnQuery := fmt.Sprintf(addColumnTemplate, fullTableName, utils.QuoteIdent(name), dataType)
		_, err := t.db.Exec(ctx, addColumnQuery)
		if err != nil {
			log.Printf("E! Couldn't add missing columns to the table: %s\nError executing: %s\n%v", tableName, addColumnQuery, err)
			return err
		}
	}

	return nil
}

// Populate the 'tableTemplate' (supplied as config option to the plugin) with the details of
// the required columns for the measurement to create a 'CREATE TABLE' SQL statement.
// The order, column names and data types are given in 'colDetails'.
func (t *defTableManager) generateCreateTableSQL(tableName string, colDetails *utils.TargetColumns) string {
	colDefs := make([]string, len(colDetails.Names))
	var pk []string
	for colIndex, colName := range colDetails.Names {
		colDefs[colIndex] = utils.QuoteIdent(colName) + " " + string(colDetails.DataTypes[colIndex])
		if colDetails.Roles[colIndex] != utils.FieldColType {
			pk = append(pk, colName)
		}
	}

	fullTableName := utils.FullTableName(t.schema, tableName).Sanitize()
	query := strings.Replace(t.tableTemplate, "{TABLE}", fullTableName, -1)
	query = strings.Replace(query, "{TABLELITERAL}", utils.QuoteLiteral(fullTableName), -1)
	query = strings.Replace(query, "{COLUMNS}", strings.Join(colDefs, ","), -1)
	query = strings.Replace(query, "{KEY_COLUMNS}", strings.Join(pk, ","), -1)

	return query
}

// For a given table and an array of column names it checks the database if those columns exist,
// and what's their data type.
func (t *defTableManager) findColumnPresence(ctx context.Context, tableName string, columns []string) ([]*columnInDbDef, error) {
	existingCols, err := t.findExistingColumns(ctx, tableName)
	if err != nil {
		return nil, err
	}
	if len(existingCols) == 0 {
		log.Printf("E! Table exists, but no columns discovered, user doesn't have enough permissions")
		return nil, errors.New("Table exists, but no columns discovered, user doesn't have enough permissions")
	}

	columnStatus := make([]*columnInDbDef, len(columns))
	for i := 0; i < len(columns); i++ {
		currentColumn := columns[i]
		colType, exists := existingCols[currentColumn]
		if !exists {
			colType = ""
		}
		columnStatus[i] = &columnInDbDef{
			exists:   exists,
			dataType: colType,
		}
	}

	return columnStatus, nil
}

func (t *defTableManager) findExistingColumns(ctx context.Context, table string) (map[string]utils.PgDataType, error) {
	rows, err := t.db.Query(ctx, findExistingColumnsTemplate, t.schema, table)
	if err != nil {
		log.Printf("E! Couldn't discover existing columns of table: %s\n%v", table, err)
		return nil, errors.Wrap(err, "could not discover existing columns")
	}
	defer rows.Close()
	cols := make(map[string]utils.PgDataType)
	for rows.Next() {
		var colName, colTypeStr string
		err := rows.Scan(&colName, &colTypeStr)
		if err != nil {
			log.Printf("E! Couldn't discover columns of table: %s\n%v", table, err)
			return nil, err
		}
		cols[colName] = utils.PgDataType(colTypeStr)
	}
	return cols, nil
}
