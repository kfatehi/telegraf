package utils

import (
	"sort"
	"strings"
)

// ColumnRole specifies the role of a column in a metric.
// It helps map the columns to the DB.
type ColumnRole int

const (
	TimeColType ColumnRole = iota + 1
	TagsIDColType
	TagColType
	FieldColType
)

// PgDataType defines a string that represents a PostgreSQL data type.
type PgDataType string

// TargetColumns contains all the information needed to map a collection of
// metrics who belong to the same Measurement.
type TargetColumns struct {
	// the names the columns will have in the database
	Names []string
	// column name -> order number. where to place each column in rows
	// batched to the db
	Target map[string]int
	// the data type of each column should have in the db. used when checking
	// if the schema matches or it needs updates
	DataTypes []PgDataType
	// the role each column has, helps properly map the metric to the db
	Roles []ColumnRole
}

func (tcs TargetColumns) Len() int {
	return len(tcs.Names)
}

func (tcs TargetColumns) Less(i, j int) bool {
	if tcs.Roles[i] != tcs.Roles[j] {
		return tcs.Roles[i] < tcs.Roles[j]
	}
	return strings.ToLower(tcs.Names[i]) < strings.ToLower(tcs.Names[j])
}

func (tcs TargetColumns) Swap(i, j int) {
	tcs.Names[i], tcs.Names[j] = tcs.Names[j], tcs.Names[i]
	tcs.Target[tcs.Names[i]], tcs.Target[tcs.Names[j]] = tcs.Target[tcs.Names[j]], tcs.Target[tcs.Names[i]]
	tcs.DataTypes[i], tcs.DataTypes[j] = tcs.DataTypes[j], tcs.DataTypes[i]
	tcs.Roles[i], tcs.Roles[j] = tcs.Roles[j], tcs.Roles[i]
}

func (tcs TargetColumns) Sort() {
	sort.Sort(tcs)
}
