package postgresql

import (
	"fmt"

	"github.com/influxdata/telegraf"
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
	postgresql   *Postgresql
	metrics      []telegraf.Metric
	cursor       int
	cursorValues []interface{}
	cursorError  error

	// tagPositions is the position of each tag within the tag set. Regardless of whether tags are foreign keys or not
	tagPositions map[string]int
	// tagColumns is the list of tags to emit. List is in order.
	tagColumns []utils.Column
	// tagSets is the list of tag IDs to tag values in use within the RowSource. The position of each value in the list
	// corresponds to the key name in the tagColumns list.
	// This data is used to build out the foreign tag table when enabled.
	// Technically the tag values will only contain strings, since all tag values are strings. But this is a restriction of telegraf
	// metrics, and not postgres. It might be nice to support somehow converting tag values into native times.
	tagSets map[int64][]interface{}

	// fieldPositions is the position of each field within the tag set.
	fieldPositions map[string]int
	// fieldColumns is the list of fields to emit. List is in order.
	fieldColumns []utils.Column

	droppedTagColumns []string
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
				values[i] = ColumnFromTag(tag.Key, tag.Value)
			}
			rs.tagSets[tagID] = values
		}
	}

	if !rs.postgresql.TagsAsJsonb {
		for _, t := range metric.TagList() {
			if _, ok := rs.tagPositions[t.Key]; !ok {
				rs.tagPositions[t.Key] = len(rs.tagPositions)
				rs.tagColumns = append(rs.tagColumns, ColumnFromTag(t.Key, t.Value))
			}
		}
	}

	if !rs.postgresql.FieldsAsJsonb {
		for _, f := range metric.FieldList() {
			if _, ok := rs.fieldPositions[f.Key]; !ok {
				rs.fieldPositions[f.Key] = len(rs.fieldPositions)
				rs.fieldColumns = append(rs.fieldColumns, ColumnFromField(f.Key, f.Value))
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

// Returns the superset of all tags of all metrics.
func (rs *RowSource) TagColumns() []utils.Column {
	var cols []utils.Column

	if rs.postgresql.TagsAsForeignkeys {
		cols = append(cols, TagIDColumn)
	}

	if rs.postgresql.TagsAsJsonb {
		cols = append(cols, TagsJSONColumn)
	} else {
		cols = append(cols, rs.tagColumns...)
	}

	return cols
}

// Returns the superset of the union of all tags+fields of all metrics.
func (rs *RowSource) Columns() []utils.Column {
	cols := []utils.Column{
		TimeColumn,
	}

	if rs.postgresql.TagsAsForeignkeys {
		cols = append(cols, TagIDColumn)
	} else {
		cols = append(cols, rs.TagColumns()...)
	}

	if rs.postgresql.FieldsAsJsonb {
		cols = append(cols, FieldsJSONColumn)
	} else {
		cols = append(cols, rs.fieldColumns...)
	}

	return cols
}

func (rs *RowSource) DropColumn(col utils.Column) {
	switch col.Role {
	case utils.TagColType:
		rs.dropTagColumn(col)
	case utils.FieldColType:
		rs.dropFieldColumn(col)
	default:
		panic(fmt.Sprintf("Tried to perform an invalid column drop. This should not have happened. measurement=%s name=%s role=%v", rs.Name(), col.Name, col.Role))
	}
}

// Drops the tag column from conversion. Any metrics containing this tag will be skipped.
func (rs *RowSource) dropTagColumn(col utils.Column) {
	if col.Role != utils.TagColType || rs.postgresql.TagsAsJsonb {
		panic(fmt.Sprintf("Tried to perform an invalid tag drop. This should not have happened. measurement=%s tag=%s", rs.Name(), col.Name))
	}
	rs.droppedTagColumns = append(rs.droppedTagColumns, col.Name)

	pos, ok := rs.tagPositions[col.Name]
	if !ok {
		return
	}

	delete(rs.tagPositions, col.Name)
	for n, p := range rs.tagPositions {
		if p > pos {
			rs.tagPositions[n] -= 1
		}
	}

	rs.tagColumns = append(rs.tagColumns[:pos], rs.tagColumns[pos+1:]...)

	for setID, set := range rs.tagSets {
		if set[pos] != nil {
			// The tag is defined, so drop the whole set
			delete(rs.tagSets, setID)
		} else {
			// The tag is null, so keep the set, and just drop the column so we don't try to use it.
			rs.tagSets[setID] = append(set, set[:pos], set[pos+1:])
		}
	}
}

// Drops the field column from conversion. Any metrics containing this field will have the field omitted.
func (rs *RowSource) dropFieldColumn(col utils.Column) {
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

	rs.fieldColumns = append(rs.fieldColumns[:pos], rs.fieldColumns[pos+1:]...)
}

func (rs *RowSource) Next() bool {
	for {
		if rs.cursor+1 >= len(rs.metrics) {
			rs.cursorValues = nil
			rs.cursorError = nil
			return false
		}
		rs.cursor += 1

		rs.cursorValues, rs.cursorError = rs.values()
		if rs.cursorValues != nil || rs.cursorError != nil {
			return true
		}
	}
}

func (rs *RowSource) Reset() {
	rs.cursor = -1
}

// values calculates the values for the metric at the cursor position.
// If the metric cannot be emitted, such as due to dropped tags, or all fields dropped, the return value is nil.
func (rs *RowSource) values() ([]interface{}, error) {
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
				tagPos, ok := rs.tagPositions[tag.Key]
				if !ok {
					// tag has been dropped, we can't emit or we risk collision with another metric
					return nil, nil
				}
				tagValues[tagPos] = tag.Value
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
		tagID := utils.GetTagID(metric)
		if _, ok := rs.tagSets[tagID]; !ok {
			// tag has been dropped, we can't emit or we risk collision with another metric
			return nil, nil
		}
		values = append(values, tagID)
	}

	if !rs.postgresql.FieldsAsJsonb {
		// fields_as_json=false
		fieldValues := make([]interface{}, len(rs.fieldPositions))
		fieldsEmpty := true
		for _, field := range fields {
			// we might have dropped the field due to the table missing the column & schema updates being turned off
			if fPos, ok := rs.fieldPositions[field.Key]; ok {
				fieldValues[fPos] = field.Value
				fieldsEmpty = false
			}
		}
		if fieldsEmpty {
			// all fields have been dropped. Don't emit a metric with just tags and no fields.
			return nil, nil
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

func (rs *RowSource) Values() ([]interface{}, error) {
	return rs.cursorValues, rs.cursorError
}

func (rs *RowSource) Err() error {
	return nil
}
