package postgresql

import (
	"fmt"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

// TableSource satisfies pgx.CopyFromSource
type TableSource struct {
	postgresql   *Postgresql
	metrics      []telegraf.Metric
	cursor       int
	cursorValues []interface{}
	cursorError  error

	// tagPositions is the position of each tag within the tag set. Regardless of whether tags are foreign keys or not
	tagPositions map[string]int
	// tagColumns is the list of tags to emit. List is in order.
	tagColumns []utils.Column
	// tagSets is the list of tag IDs to tag values in use within the TableSource. The position of each value in the list
	// corresponds to the key name in the tagColumns list.
	// This data is used to build out the foreign tag table when enabled.
	tagSets map[int64][]*telegraf.Tag

	// fieldPositions is the position of each field within the tag set.
	fieldPositions map[string]int
	// fieldColumns is the list of fields to emit. List is in order.
	fieldColumns []utils.Column

	droppedTagColumns []string
}

func NewTableSource(postgresql *Postgresql) *TableSource {
	tsrc := &TableSource{
		postgresql: postgresql,
		cursor:     -1,
		tagSets:    make(map[int64][]*telegraf.Tag),
	}
	if !postgresql.FieldsAsJsonb {
		tsrc.tagPositions = map[string]int{}
		tsrc.fieldPositions = map[string]int{}
	}
	return tsrc
}

func (tsrc *TableSource) AddMetric(metric telegraf.Metric) {
	if tsrc.postgresql.TagsAsForeignkeys {
		tagID := utils.GetTagID(metric)
		if _, ok := tsrc.tagSets[tagID]; !ok {
			tsrc.tagSets[tagID] = metric.TagList()
		}
	}

	if !tsrc.postgresql.TagsAsJsonb {
		for _, t := range metric.TagList() {
			if _, ok := tsrc.tagPositions[t.Key]; !ok {
				tsrc.tagPositions[t.Key] = len(tsrc.tagPositions)
				tsrc.tagColumns = append(tsrc.tagColumns, ColumnFromTag(t.Key, t.Value))
			}
		}
	}

	if !tsrc.postgresql.FieldsAsJsonb {
		for _, f := range metric.FieldList() {
			if _, ok := tsrc.fieldPositions[f.Key]; !ok {
				tsrc.fieldPositions[f.Key] = len(tsrc.fieldPositions)
				tsrc.fieldColumns = append(tsrc.fieldColumns, ColumnFromField(f.Key, f.Value))
			}
		}
	}

	tsrc.metrics = append(tsrc.metrics, metric)
}

func (tsrc *TableSource) Name() string {
	if len(tsrc.metrics) == 0 {
		return ""
	}
	return tsrc.metrics[0].Name()
}

// Returns the superset of all tags of all metrics.
func (tsrc *TableSource) TagColumns() []utils.Column {
	var cols []utils.Column

	if tsrc.postgresql.TagsAsJsonb {
		cols = append(cols, TagsJSONColumn)
	} else {
		cols = append(cols, tsrc.tagColumns...)
	}

	return cols
}

// Returns the superset of all fields of all metrics.
func (tsrc *TableSource) FieldColumns() []utils.Column {
	return tsrc.fieldColumns
}

// Returns the full column list, including time, tag id or tags, and fields.
func (tsrc *TableSource) MetricTableColumns() []utils.Column {
	cols := []utils.Column{
		TimeColumn,
	}

	if tsrc.postgresql.TagsAsForeignkeys {
		cols = append(cols, TagIDColumn)
	} else {
		cols = append(cols, tsrc.TagColumns()...)
	}

	if tsrc.postgresql.FieldsAsJsonb {
		cols = append(cols, FieldsJSONColumn)
	} else {
		cols = append(cols, tsrc.FieldColumns()...)
	}

	return cols
}

func (tsrc *TableSource) TagTableColumns() []utils.Column {
	cols := []utils.Column{
		TagIDColumn,
	}

	cols = append(cols, tsrc.TagColumns()...)

	return cols
}

func (tsrc *TableSource) ColumnNames() []string {
	cols := tsrc.MetricTableColumns()
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
	}
	return names
}

func (tsrc *TableSource) DropColumn(col utils.Column) {
	switch col.Role {
	case utils.TagColType:
		tsrc.dropTagColumn(col)
	case utils.FieldColType:
		tsrc.dropFieldColumn(col)
	default:
		panic(fmt.Sprintf("Tried to perform an invalid column drop. This should not have happened. measurement=%s name=%s role=%v", tsrc.Name(), col.Name, col.Role))
	}
}

// Drops the tag column from conversion. Any metrics containing this tag will be skipped.
func (tsrc *TableSource) dropTagColumn(col utils.Column) {
	if col.Role != utils.TagColType || tsrc.postgresql.TagsAsJsonb {
		panic(fmt.Sprintf("Tried to perform an invalid tag drop. This should not have happened. measurement=%s tag=%s", tsrc.Name(), col.Name))
	}
	tsrc.droppedTagColumns = append(tsrc.droppedTagColumns, col.Name)

	pos, ok := tsrc.tagPositions[col.Name]
	if !ok {
		return
	}

	delete(tsrc.tagPositions, col.Name)
	for n, p := range tsrc.tagPositions {
		if p > pos {
			tsrc.tagPositions[n] -= 1
		}
	}

	tsrc.tagColumns = append(tsrc.tagColumns[:pos], tsrc.tagColumns[pos+1:]...)

	for setID, set := range tsrc.tagSets {
		for _, tag := range set {
			if tag.Key == col.Name {
				// The tag is defined, so drop the whole set
				delete(tsrc.tagSets, setID)
				break
			}
		}
	}
}

// Drops the field column from conversion. Any metrics containing this field will have the field omitted.
func (tsrc *TableSource) dropFieldColumn(col utils.Column) {
	if col.Role != utils.FieldColType || tsrc.postgresql.FieldsAsJsonb {
		panic(fmt.Sprintf("Tried to perform an invalid field drop. This should not have happened. measurement=%s field=%s", tsrc.Name(), col.Name))
	}

	pos, ok := tsrc.fieldPositions[col.Name]
	if !ok {
		return
	}

	delete(tsrc.fieldPositions, col.Name)
	for n, p := range tsrc.fieldPositions {
		if p > pos {
			tsrc.fieldPositions[n] -= 1
		}
	}

	tsrc.fieldColumns = append(tsrc.fieldColumns[:pos], tsrc.fieldColumns[pos+1:]...)
}

func (tsrc *TableSource) Next() bool {
	for {
		if tsrc.cursor+1 >= len(tsrc.metrics) {
			tsrc.cursorValues = nil
			tsrc.cursorError = nil
			return false
		}
		tsrc.cursor += 1

		tsrc.cursorValues, tsrc.cursorError = tsrc.values()
		if tsrc.cursorValues != nil || tsrc.cursorError != nil {
			return true
		}
	}
}

func (tsrc *TableSource) Reset() {
	tsrc.cursor = -1
}

// values calculates the values for the metric at the cursor position.
// If the metric cannot be emitted, such as due to dropped tags, or all fields dropped, the return value is nil.
func (tsrc *TableSource) values() ([]interface{}, error) {
	metric := tsrc.metrics[tsrc.cursor]
	tags := metric.TagList()
	fields := metric.FieldList()

	values := []interface{}{}

	values = append(values, metric.Time())

	if !tsrc.postgresql.TagsAsForeignkeys {
		if !tsrc.postgresql.TagsAsJsonb {
			// tags_as_foreignkey=false, tags_as_json=false
			tagValues := make([]interface{}, len(tsrc.tagPositions))
			for _, tag := range tags {
				tagPos, ok := tsrc.tagPositions[tag.Key]
				if !ok {
					// tag has been dropped, we can't emit or we risk collision with another metric
					return nil, nil
				}
				tagValues[tagPos] = tag.Value
			}
			values = append(values, tagValues...)
		} else {
			// tags_as_foreign_key=false, tags_as_json=true
			values = append(values, utils.TagListToJSON(metric.TagList()))
		}
	} else {
		// tags_as_foreignkey=true
		tagID := utils.GetTagID(metric)
		if _, ok := tsrc.tagSets[tagID]; !ok {
			// tag has been dropped, we can't emit or we risk collision with another metric
			return nil, nil
		}
		values = append(values, tagID)
	}

	if !tsrc.postgresql.FieldsAsJsonb {
		// fields_as_json=false
		fieldValues := make([]interface{}, len(tsrc.fieldPositions))
		fieldsEmpty := true
		for _, field := range fields {
			// we might have dropped the field due to the table missing the column & schema updates being turned off
			if fPos, ok := tsrc.fieldPositions[field.Key]; ok {
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
		value, err := utils.FieldListToJSON(metric.FieldList())
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}

	return values, nil
}

func (tsrc *TableSource) Values() ([]interface{}, error) {
	return tsrc.cursorValues, tsrc.cursorError
}

func (tsrc *TableSource) Err() error {
	return nil
}

type TagTableSource struct {
	*TableSource
	tagIDs []int64

	cursor       int
	cursorValues []interface{}
	cursorError  error
}

func NewTagTableSource(tsrc *TableSource) *TagTableSource {
	ttsrc := &TagTableSource{
		TableSource: tsrc,
		cursor:      -1,
	}

	ttsrc.tagIDs = make([]int64, 0, len(tsrc.tagSets))
	for tagID := range tsrc.tagSets {
		ttsrc.tagIDs = append(ttsrc.tagIDs, tagID)
	}

	return ttsrc
}

func (ttsrc *TagTableSource) Name() string {
	return ttsrc.TableSource.Name() + ttsrc.postgresql.TagTableSuffix
}

func (ttsrc *TagTableSource) ColumnNames() []string {
	cols := ttsrc.TagTableColumns()
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
	}
	return names
}

func (ttsrc *TagTableSource) Next() bool {
	for {
		if ttsrc.cursor+1 >= len(ttsrc.tagIDs) {
			ttsrc.cursorValues = nil
			ttsrc.cursorError = nil
			return false
		}
		ttsrc.cursor += 1

		ttsrc.cursorValues, ttsrc.cursorError = ttsrc.values()
		if ttsrc.cursorValues != nil || ttsrc.cursorError != nil {
			return true
		}
	}
}

func (ttsrc *TagTableSource) Reset() {
	ttsrc.cursor = -1
}

func (ttsrc *TagTableSource) values() ([]interface{}, error) {
	tagID := ttsrc.tagIDs[ttsrc.cursor]
	tagSet := ttsrc.tagSets[tagID]

	var values []interface{}
	if !ttsrc.postgresql.TagsAsJsonb {
		values = make([]interface{}, len(tagSet)+1)
		for _, tag := range tagSet {
			values[ttsrc.TableSource.tagPositions[tag.Key]+1] = tag.Value // +1 to account for tag_id column
		}
	} else {
		values = make([]interface{}, 2)
		values[1] = utils.TagListToJSON(tagSet)
	}
	values[0] = tagID

	return values, nil
}

func (ttsrc *TagTableSource) Values() ([]interface{}, error) {
	return ttsrc.cursorValues, ttsrc.cursorError
}

func (ttsrc *TagTableSource) Err() error {
	return nil
}
