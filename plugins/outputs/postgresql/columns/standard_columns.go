package columns

import "github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"

// Column names and data types for standard fields (time, tag_id, tags, and fields)
const (
	TimeColumnName          = "time"
	TimeColumnDataType      = utils.PgTimestamptz
	TimeColumnDefinition    = TimeColumnName + " " + utils.PgTimestamptz
	TagIDColumnName         = "tag_id"
	TagIDColumnDataType     = utils.PgBigInt
	TagsJSONColumn          = "tags"
	FieldsJSONColumn        = "fields"
	JSONColumnDataType      = utils.PgJSONb
)
