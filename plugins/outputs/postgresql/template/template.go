package template

import (
	"bytes"
	"encoding/base32"
	"fmt"
	"hash/fnv"
	"strings"
	"text/template"
	"unsafe"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"

	"github.com/Masterminds/sprig"
)

var TableCreateTemplate = newTemplate(`CREATE TABLE {{.table}} ({{.columns}})`)
var TableAddColumnTemplate = newTemplate(`ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}`)
var TagTableCreateTemplate = newTemplate(`CREATE TABLE {{.table}} ({{.columns}}, PRIMARY KEY (tag_id))`)

var templateFuncs = map[string]interface{}{
	"quoteIdentifier": QuoteIdentifier,
	"quoteLiteral":    QuoteLiteral,
}

func asString(obj interface{}) string {
	switch obj := obj.(type) {
	case string:
		return obj
	case []byte:
		return string(obj)
	case fmt.Stringer:
		return obj.String()
	default:
		return fmt.Sprintf("%v", obj)
	}
}
func QuoteIdentifier(name interface{}) string {
	return `"` + strings.ReplaceAll(asString(name), `"`, `""`) + `"`
}
func QuoteLiteral(str interface{}) string {
	return "'" + strings.ReplaceAll(asString(str), "'", "''") + "'"
}

type TemplateTable struct {
	Schema  string
	Name    string
	Columns TemplateColumns
}

func NewTemplateTable(schemaName, tableName string, columns []utils.Column) *TemplateTable {
	if tableName == "" {
		return nil
	}
	return &TemplateTable{
		Schema:  schemaName,
		Name:    tableName,
		Columns: NewTemplateColumns(columns),
	}
}

//func (tt *TemplateTable) SetName(name string) {
//	tt.Name = name
//}
//func (tt *TemplateTable) SetSchema(schema string) {
//	tt.Schema = schema
//}
func (tt *TemplateTable) String() string {
	return tt.Identifier()
}
func (tt *TemplateTable) Identifier() string {
	if tt.Schema == "" {
		return QuoteIdentifier(tt.Name)
	}
	return QuoteIdentifier(tt.Schema) + "." + QuoteIdentifier(tt.Name)
}
func (tt *TemplateTable) WithSchema(name string) *TemplateTable {
	ttNew := &TemplateTable{}
	*ttNew = *tt
	ttNew.Schema = name
	return ttNew
}
func (tt *TemplateTable) WithName(name string) *TemplateTable {
	ttNew := &TemplateTable{}
	*ttNew = *tt
	ttNew.Name = name
	return ttNew
}
func (tt *TemplateTable) WithSuffix(suffixes ...string) *TemplateTable {
	ttNew := &TemplateTable{}
	*ttNew = *tt
	ttNew.Name += strings.Join(suffixes, "")
	return ttNew
}

//func (tt *TemplateTable) Literal() string {
//	return QuoteLiteral(tt.Identifier())
//}

type TemplateColumn utils.Column

func (tc TemplateColumn) String() string {
	return tc.Definition()
}
func (tc TemplateColumn) Definition() string {
	return tc.Identifier() + " " + string(tc.Type)
}
func (tc TemplateColumn) Identifier() string {
	return QuoteIdentifier(tc.Name)
}
func (tc TemplateColumn) Selector() string {
	if tc.Type != "" {
		return tc.Identifier()
	}
	return "NULL as " + tc.Identifier()
}
func (tc TemplateColumn) IsTag() bool {
	return tc.Role == utils.TagColType
}
func (tc TemplateColumn) IsField() bool {
	return tc.Role == utils.FieldColType
}

//func (tc TemplateColumn) Literal() string {
//	return QuoteLiteral(tc.Name)
//}

type TemplateColumns []TemplateColumn

func NewTemplateColumns(cols []utils.Column) TemplateColumns {
	tcs := make(TemplateColumns, len(cols))
	for i, col := range cols {
		tcs[i] = TemplateColumn(col)
	}
	return tcs
}

func (tcs TemplateColumns) List() []TemplateColumn {
	return tcs
}

func (tcs TemplateColumns) Definitions() []string {
	defs := make([]string, len(tcs))
	for i, tc := range tcs {
		defs[i] = tc.Definition()
	}
	return defs
}

func (tcs TemplateColumns) Identifiers() []string {
	idents := make([]string, len(tcs))
	for i, tc := range tcs {
		idents[i] = tc.Identifier()
	}
	return idents
}

func (tcs TemplateColumns) Selectors() []string {
	selectors := make([]string, len(tcs))
	for i, tc := range tcs {
		selectors[i] = tc.Selector()
	}
	return selectors
}

func (tcs TemplateColumns) String() string {
	colStrs := make([]string, len(tcs))
	for i, tc := range tcs {
		colStrs[i] = tc.String()
	}
	return strings.Join(colStrs, ", ")
}

func (tcs TemplateColumns) Keys() TemplateColumns {
	var cols []TemplateColumn
	for _, tc := range tcs {
		if tc.Role != utils.FieldColType {
			cols = append(cols, tc)
		}
	}
	return cols
}

func (tcs TemplateColumns) Sorted() TemplateColumns {
	cols := append([]TemplateColumn{}, tcs...)
	(*utils.ColumnList)(unsafe.Pointer(&cols)).Sort()
	return cols
}

func (tcs TemplateColumns) Concat(tcsList ...TemplateColumns) TemplateColumns {
	tcsNew := append(TemplateColumns{}, tcs...)
	for _, tcs := range tcsList {
		tcsNew = append(tcsNew, tcs...)
	}
	return tcsNew
}

// Generates a list of SQL selectors against the given columns.
// For each column in tcs, if the column also exist in tcsFrom, it will be selected. If the column does not exist NULL will be selected.
func (tcs TemplateColumns) Union(tcsFrom TemplateColumns) TemplateColumns {
	tcsNew := append(TemplateColumns{}, tcs...)
TCS:
	for i, tc := range tcs {
		for _, tcFrom := range tcsFrom {
			if tc.Name == tcFrom.Name {
				continue TCS
			}
		}
		tcsNew[i].Type = ""
	}
	return tcsNew
}

func (tcs TemplateColumns) Tags() TemplateColumns {
	var cols []TemplateColumn
	for _, tc := range tcs {
		if tc.Role == utils.TagColType {
			cols = append(cols, tc)
		}
	}
	return cols
}

func (tcs TemplateColumns) Fields() TemplateColumns {
	var cols []TemplateColumn
	for _, tc := range tcs {
		if tc.Role == utils.FieldColType {
			cols = append(cols, tc)
		}
	}
	return cols
}

func (tcs TemplateColumns) Hash() string {
	hash := fnv.New32a()
	for _, tc := range tcs.Sorted() {
		hash.Write([]byte(tc.Name))
		hash.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash.Sum(nil)))
}

type Template template.Template

func newTemplate(templateString string) *Template {
	t := &Template{}
	if err := t.UnmarshalText([]byte(templateString)); err != nil {
		panic(err)
	}
	return t
}

func (t *Template) UnmarshalText(text []byte) error {
	tmpl := template.New("")
	tmpl.Option("missingkey=error")
	tmpl.Funcs(templateFuncs)
	tmpl.Funcs(sprig.TxtFuncMap())
	tt, err := tmpl.Parse(string(text))
	if err != nil {
		return err
	}
	*t = Template(*tt)
	return nil
}

func (t *Template) Render(table *TemplateTable, newColumns []utils.Column, metricTable *TemplateTable, tagTable *TemplateTable) ([]byte, error) {
	tcs := NewTemplateColumns(newColumns).Sorted()
	data := map[string]interface{}{
		"table":       table,
		"columns":     tcs,
		"allColumns":  tcs.Concat(table.Columns).Sorted(),
		"metricTable": metricTable,
		"tagTable":    tagTable,
	}

	buf := bytes.NewBuffer(nil)
	err := (*template.Template)(t).Execute(buf, data)
	return buf.Bytes(), err
}
