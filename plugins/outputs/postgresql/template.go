package postgresql

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"

	"github.com/Masterminds/sprig"
)

var TableCreateTemplate = newTemplate(`CREATE TABLE {{.table}} ({{.columns}})`)
var TableAddColumnTemplate = newTemplate(`ALTER TABLE {{.table}} ADD COLUMN IF NOT EXISTS {{.columns|join ", ADD COLUMN IF NOT EXISTS "}}`)

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
func QuoteLiteral(str fmt.Stringer) string {
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
	return QuoteIdentifier(tt.Schema) + "." + QuoteIdentifier(tt.Name)
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
		idents[i] = QuoteIdentifier(tc.Name)
	}
	return idents
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

func (tcs TemplateColumns) Tags() TemplateColumns {
	var cols []TemplateColumn
	for _, tc := range tcs {
		if tc.Role == utils.TagColType || tc.Role == utils.TagsIDColType {
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
	data := map[string]interface{}{
		"table":       table,
		"columns":     NewTemplateColumns(newColumns),
		"metricTable": metricTable,
		"tagTable":    tagTable,
	}

	buf := bytes.NewBuffer(nil)
	err := (*template.Template)(t).Execute(buf, data)
	return buf.Bytes(), err
}
