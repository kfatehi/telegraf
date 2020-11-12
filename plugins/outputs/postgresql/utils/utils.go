package utils

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v4"

	"github.com/influxdata/telegraf"
)

const (
	insertIntoSQLTemplate = "INSERT INTO %s(%s) VALUES(%s)"
)

// GroupMetricsByMeasurement groups the list of metrics by the measurement name.
func GroupMetricsByMeasurement(m []telegraf.Metric) map[string][]telegraf.Metric {
	groups := make(map[string][]telegraf.Metric)
	for _, metric := range m {
		var group []telegraf.Metric
		var ok bool
		name := metric.Name()
		if group, ok = groups[name]; !ok {
			group = []telegraf.Metric{}
		}
		groups[name] = append(group, metric)
	}
	return groups
}

// BuildJsonb returns a byte array of the json representation
// of the passed object.
func BuildJsonb(data interface{}) ([]byte, error) {
	d, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// QuoteIdent returns a sanitized string safe to use in SQL as an identifier
func QuoteIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// QuoteLiteral returns a sanitized string safe to use in sql as a string literal
func QuoteLiteral(name string) string {
	return "'" + strings.Replace(name, "'", "''", -1) + "'"
}

// FullTableName returns a sanitized table name with it's schema (if supplied)
func FullTableName(schema, name string) pgx.Identifier {
	if schema != "" {
		return pgx.Identifier{schema, name}
	}

	return pgx.Identifier{name}
}

// Constants for naming PostgreSQL data types both in
// their short and long versions.
const (
	PgBool                     = "boolean"
	PgSmallInt                 = "smallint"
	PgInteger                  = "integer"
	PgBigInt                   = "bigint"
	PgReal                     = "real"
	PgDoublePrecision          = "double precision"
	PgText                     = "text"
	PgTimestampWithTimeZone    = "timestamp with time zone"
	PgTimestampWithoutTimeZone = "timestamp without time zone"
	PgSerial                   = "serial"
	PgJSONb                    = "jsonb"
)

// DerivePgDatatype returns the appropriate PostgreSQL data type
// that could hold the value.
func DerivePgDatatype(value interface{}) PgDataType {
	switch value.(type) {
	case bool:
		return PgBool
	case uint64, int64, int, uint:
		return PgBigInt
	case uint32, int32:
		return PgInteger
	case int16, int8:
		return PgSmallInt
	case float64:
		return PgDoublePrecision
	case float32:
		return PgReal
	case string:
		return PgText
	case time.Time:
		return PgTimestampWithTimeZone
	default:
		log.Printf("E! Unknown datatype %T(%v)", value, value)
		return PgText
	}
}

// PgTypeCanContain tells you if one PostgreSQL data type can contain
// the values of another without data loss.
func PgTypeCanContain(canThis PgDataType, containThis PgDataType) bool {
	switch canThis {
	case containThis:
		return true
	case PgBigInt:
		return containThis == PgInteger || containThis == PgSmallInt
	case PgInteger:
		return containThis == PgSmallInt
	case PgDoublePrecision:
		return containThis == PgReal || containThis == PgBigInt || containThis == PgInteger || containThis == PgSmallInt
	case PgReal:
		return containThis == PgBigInt || containThis == PgInteger || containThis == PgSmallInt
	case PgTimestampWithTimeZone:
		return containThis == PgTimestampWithoutTimeZone
	default:
		return false
	}
}

// GenerateInsert returns a SQL statement to insert values in a table
// with $X placeholders for the values
func GenerateInsert(fullSanitizedTableName string, columns []string) string {
	valuePlaceholders := make([]string, len(columns))
	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		valuePlaceholders[i] = fmt.Sprintf("$%d", i+1)
		quotedColumns[i] = QuoteIdent(column)
	}

	columnNames := strings.Join(quotedColumns, ",")
	values := strings.Join(valuePlaceholders, ",")
	return fmt.Sprintf(insertIntoSQLTemplate, fullSanitizedTableName, columnNames, values)
}

func GetTagID(metric telegraf.Metric) int64 {
	hash := fnv.New64a()
	for _, tag := range metric.TagList() {
		_, _ = hash.Write([]byte(tag.Key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(tag.Value))
		_, _ = hash.Write([]byte{0})
	}
	// Convert to int64 as postgres does not support uint64
	return int64(hash.Sum64())
}
