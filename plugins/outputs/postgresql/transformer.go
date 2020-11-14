package postgresql

import (
	"fmt"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/columns"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

func (p *Postgresql) splitRowSources(metrics []telegraf.Metric) map[string]*RowSource {
	rowSources := map[string]*RowSource{}

	for _, m := range metrics {
		rs := rowSources[m.Name()]
		if rs == nil {
			rs = NewRowSource(p)
			rowSources[m.Name()] = rs
		}
		rs.AddMetric(m)
	}

	return rowSources
}

type RowSource struct {
	postgresql *Postgresql
	metrics    []telegraf.Metric
	cursor     int

	tagPositions map[string]int
	tagColumns   []utils.Column

	fieldPositions map[string]int
	fieldColumns   []utils.Column

	// Technically this will only contain strings, since all tag values are strings. But this is a restriction of telegraf
	// metrics, and not postgres. It might be nice to support somehow converting tag values into native times.
	tagSets map[int64][]interface{}
}

func NewRowSource(postgresql *Postgresql) *RowSource {
	rs := &RowSource{
		postgresql: postgresql,
		cursor:     -1,
		tagSets:    make(map[int64][]interface{}),
	}
	if !postgresql.FieldsAsJsonb {
		rs.tagPositions = map[string]int{}
		rs.fieldPositions = map[string]int{}
	}
	return rs
}

func (rs *RowSource) AddMetric(metric telegraf.Metric) {
	if rs.postgresql.TagsAsForeignkeys {
		tagID := utils.GetTagID(metric)
		if _, ok := rs.tagSets[tagID]; !ok {
			tags := metric.TagList()
			values := make([]interface{}, len(tags))
			for i, tag := range tags {
				values[i] = columns.ColumnFromTag(tag.Key, tag.Value)
			}
			rs.tagSets[tagID] = values
		}
	}

	if !rs.postgresql.TagsAsJsonb {
		for _, t := range metric.TagList() {
			if _, ok := rs.tagPositions[t.Key]; !ok {
				rs.tagPositions[t.Key] = len(rs.tagPositions)
				rs.tagColumns = append(rs.tagColumns, columns.ColumnFromTag(t.Key, t.Value))
			}
		}
	}

	if !rs.postgresql.FieldsAsJsonb {
		for _, f := range metric.FieldList() {
			if _, ok := rs.fieldPositions[f.Key]; !ok {
				rs.fieldPositions[f.Key] = len(rs.fieldPositions)
				rs.fieldColumns = append(rs.fieldColumns, columns.ColumnFromField(f.Key, f.Value))
			}
		}
	}

	rs.metrics = append(rs.metrics, metric)
}

func (rs *RowSource) Name() string {
	if len(rs.metrics) == 0 {
		return ""
	}
	return rs.metrics[0].Name()
}

func (rs *RowSource) TagColumns() []utils.Column {
	var cols []utils.Column

	if rs.postgresql.TagsAsForeignkeys {
		cols = append(cols, columns.TagIDColumn)
	}

	if rs.postgresql.TagsAsJsonb {
		cols = append(cols, columns.TagsJSONColumn)
	} else {
		cols = append(cols, rs.tagColumns...)
	}

	return cols
}

func (rs *RowSource) Columns() []utils.Column {
	cols := []utils.Column{
		columns.TimeColumn,
	}

	if rs.postgresql.TagsAsForeignkeys {
		cols = append(cols, columns.TagIDColumn)
	} else {
		cols = append(cols, rs.TagColumns()...)
	}

	if rs.postgresql.FieldsAsJsonb {
		cols = append(cols, columns.FieldsJSONColumn)
	} else {
		cols = append(cols, rs.fieldColumns...)
	}

	return cols
}

func (rs *RowSource) DropFieldColumn(col utils.Column) {
	if col.Role != utils.FieldColType || rs.postgresql.FieldsAsJsonb {
		panic(fmt.Sprintf("Tried to perform an invalid field drop. This should not have happened. measurement=%s field=%s", rs.Name(), col.Name))
	}

	pos, ok := rs.fieldPositions[col.Name]
	if !ok {
		return
	}
	delete(rs.fieldPositions, col.Name)
	for n, p := range rs.fieldPositions {
		if p > pos {
			rs.fieldPositions[n] -= 1
		}
	}

	for i, fc := range rs.fieldColumns {
		if fc.Name != col.Name {
			continue
		}
		rs.fieldColumns = append(rs.fieldColumns[:i], rs.fieldColumns[i+1:]...)
		break
	}
}

func (rs *RowSource) Next() bool {
	if rs.cursor+1 >= len(rs.metrics) {
		return false
	}
	rs.cursor += 1
	return true
}

func (rs *RowSource) Reset() {
	rs.cursor = -1
}

func (rs *RowSource) Values() ([]interface{}, error) {
	metric := rs.metrics[rs.cursor]
	tags := metric.TagList()
	fields := metric.FieldList()

	values := []interface{}{}

	values = append(values, metric.Time())

	if !rs.postgresql.TagsAsForeignkeys {
		if !rs.postgresql.TagsAsJsonb {
			// tags_as_foreignkey=false, tags_as_json=false
			tagValues := make([]interface{}, len(rs.tagPositions))
			for _, tag := range tags {
				tagValues[rs.tagPositions[tag.Key]] = tag.Value
			}
			values = append(values, tagValues...)
		} else {
			// tags_as_foreign_key=false, tags_as_json=true
			value, err := utils.BuildJsonb(metric.Tags())
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
	} else {
		// tags_as_foreignkey=true
		values = append(values, utils.GetTagID(metric))
	}

	if !rs.postgresql.FieldsAsJsonb {
		// fields_as_json=false
		fieldValues := make([]interface{}, len(rs.fieldPositions))
		for _, field := range fields {
			// we might have dropped the field due to the table missing the column & schema updates being turned off
			if fPos, ok := rs.fieldPositions[field.Key]; ok {
				fieldValues[fPos] = field.Value
			}
		}
		values = append(values, fieldValues...)
	} else {
		// fields_as_json=true
		value, err := utils.BuildJsonb(metric.Fields())
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}

	return values, nil
}

func (rs *RowSource) Err() error {
	return nil
}
