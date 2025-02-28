package postgresql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/sqltemplate"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
)

func TestTableManager_EnsureStructure(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	cols := []utils.Column{
		p.columnFromTag("foo", ""),
		p.columnFromField("baz", 0),
	}
	missingCols, err := p.tableManager.EnsureStructure(
		ctx,
		p.db,
		p.tableManager.table(t.Name()),
		cols,
		p.CreateTemplates,
		p.AddColumnTemplates,
		p.tableManager.table(t.Name()),
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, missingCols)

	tblCols := p.tableManager.table(t.Name()).columns
	assert.EqualValues(t, cols[0], tblCols["foo"])
	assert.EqualValues(t, cols[1], tblCols["baz"])
}

func TestTableManager_EnsureStructure_alter(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	cols := []utils.Column{
		p.columnFromTag("foo", ""),
		p.columnFromField("bar", 0),
	}
	_, err := p.tableManager.EnsureStructure(
		ctx,
		p.db,
		p.tableManager.table(t.Name()),
		cols,
		p.CreateTemplates,
		p.AddColumnTemplates,
		p.tableManager.table(t.Name()),
		nil,
	)
	require.NoError(t, err)

	cols = append(cols, p.columnFromField("baz", 0))
	missingCols, err := p.tableManager.EnsureStructure(
		ctx,
		p.db,
		p.tableManager.table(t.Name()),
		cols,
		p.CreateTemplates,
		p.AddColumnTemplates,
		p.tableManager.table(t.Name()),
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, missingCols)

	tblCols := p.tableManager.table(t.Name()).columns
	assert.EqualValues(t, cols[0], tblCols["foo"])
	assert.EqualValues(t, cols[1], tblCols["bar"])
	assert.EqualValues(t, cols[2], tblCols["baz"])
}

func TestTableManager_getColumns(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	cols := []utils.Column{
		p.columnFromTag("foo", ""),
		p.columnFromField("baz", 0),
	}
	_, err := p.tableManager.EnsureStructure(
		ctx,
		p.db,
		p.tableManager.table(t.Name()),
		cols,
		p.CreateTemplates,
		p.AddColumnTemplates,
		p.tableManager.table(t.Name()),
		nil,
	)
	require.NoError(t, err)

	p.tableManager.ClearTableCache()
	require.Empty(t, p.tableManager.table(t.Name()).columns)

	curCols, err := p.tableManager.getColumns(ctx, p.db, t.Name())
	require.NoError(t, err)

	assert.EqualValues(t, cols[0], curCols["foo"])
	assert.EqualValues(t, cols[1], curCols["baz"])
}

func TestTableManager_MatchSource(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]

	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.Contains(t, p.tableManager.table(t.Name()+p.TagTableSuffix).columns, "tag")
	assert.Contains(t, p.tableManager.table(t.Name()).columns, "a")
}

func TestTableManager_MatchSource_UnsignedIntegers(t *testing.T) {
	p := newPostgresqlTest(t)
	p.UseUint8 = true
	_ = p.Init()
	require.NoError(t, p.Connect())

	row := p.db.QueryRow(ctx, "SELECT count(*) FROM pg_extension WHERE extname='uint'")
	var n int
	require.NoError(t, row.Scan(&n))
	if n == 0 {
		t.Skipf("pguint extension is not installed")
		t.SkipNow()
	}

	metrics := []telegraf.Metric{
		newMetric(t, "", nil, MSI{"a": uint64(1)}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]

	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.Equal(t, PgUint8, p.tableManager.table(t.Name()).columns["a"].Type)
}

func TestTableManager_noCreateTable(t *testing.T) {
	p := newPostgresqlTest(t)
	p.CreateTemplates = nil
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]

	require.Error(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
}

func TestTableManager_noCreateTagTable(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagTableCreateTemplates = nil
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]

	require.Error(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
}

// verify that TableManager updates & caches the DB table structure unless the incoming metric can't fit.
func TestTableManager_cache(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]

	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
}

// Verify that when alter statements are disabled and a metric comes in with a new tag key, that the tag is omitted.
func TestTableManager_noAlterMissingTag(t *testing.T) {
	p := newPostgresqlTest(t)
	p.AddColumnTemplates = []*sqltemplate.Template{}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo", "bar": "baz"}, MSI{"a": 3}),
	}
	tsrc = NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, tsrc.ColumnNames(), "bar")
}

// Verify that when using foreign tags and alter statements are disabled and a metric comes in with a new tag key, that
// the tag is omitted.
func TestTableManager_noAlterMissingTagTableTag(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	p.TagTableAddColumnTemplates = []*sqltemplate.Template{}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo", "bar": "baz"}, MSI{"a": 3}),
	}
	tsrc = NewTableSources(p.Postgresql, metrics)[t.Name()]
	ttsrc := NewTagTableSource(tsrc)
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, ttsrc.ColumnNames(), "bar")
}

// Verify that when using foreign tags and alter statements generate a permanent error and a metric comes in with a new
// tag key, that the tag is omitted.
func TestTableManager_badAlterTagTable(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	tmpl := &sqltemplate.Template{}
	_ = tmpl.UnmarshalText([]byte("bad"))
	p.TagTableAddColumnTemplates = []*sqltemplate.Template{tmpl}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo", "bar": "baz"}, MSI{"a": 3}),
	}
	tsrc = NewTableSources(p.Postgresql, metrics)[t.Name()]
	ttsrc := NewTagTableSource(tsrc)
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, ttsrc.ColumnNames(), "bar")
}

// verify that when alter statements are disabled and a metric comes in with a new field key, that the field is omitted.
func TestTableManager_noAlterMissingField(t *testing.T) {
	p := newPostgresqlTest(t)
	p.AddColumnTemplates = []*sqltemplate.Template{}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 3, "b": 3}),
	}
	tsrc = NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, tsrc.ColumnNames(), "b")
}

// verify that when alter statements generate a permanent error and a metric comes in with a new field key, that the field is omitted.
func TestTableManager_badAlterField(t *testing.T) {
	p := newPostgresqlTest(t)
	tmpl := &sqltemplate.Template{}
	_ = tmpl.UnmarshalText([]byte("bad"))
	p.AddColumnTemplates = []*sqltemplate.Template{tmpl}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 3, "b": 3}),
	}
	tsrc = NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, tsrc.ColumnNames(), "b")
}

func TestTableManager_addColumnTemplates(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"foo": "bar"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	p = newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	tmpl := &sqltemplate.Template{}
	require.NoError(t, tmpl.UnmarshalText([]byte(`-- addColumnTemplate: {{ . }}`)))
	p.AddColumnTemplates = append(p.AddColumnTemplates, tmpl)
	require.NoError(t, p.Connect())

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"pop": "tart"}, MSI{"a": 1, "b": 2}),
	}
	tsrc = NewTableSources(p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	p.Logger.Info("ok")
	var log string
	for _, l := range p.Logger.Logs() {
		if strings.Contains(l.String(), "-- addColumnTemplate") {
			log = l.String()
			break
		}
	}
	assert.Contains(t, log, `table:"public"."TestTableManager_addColumnTemplates"`)
	assert.Contains(t, log, `columns:"b" bigint`)
	assert.Contains(t, log, `allColumns:"time" timestamp without time zone, "tag_id" bigint, "a" bigint, "b" bigint`)
	assert.Contains(t, log, `metricTable:"public"."TestTableManager_addColumnTemplates"`)
	assert.Contains(t, log, `tagTable:"public"."TestTableManager_addColumnTemplates_tag"`)
}
