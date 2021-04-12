package postgresql

import (
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/template"
	"github.com/influxdata/telegraf/plugins/outputs/postgresql/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTableManager_EnsureStructure(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	cols := []utils.Column{
		ColumnFromTag("foo", ""),
		ColumnFromField("baz", 0),
	}
	missingCols, err := p.tableManager.EnsureStructure(
		ctx,
		p.db,
		t.Name(),
		cols,
		p.CreateTemplates,
		p.AddColumnTemplates,
		t.Name(),
		"",
		)
	require.NoError(t, err)
	require.Empty(t, missingCols)

	assert.EqualValues(t, cols[0], p.tableManager.Tables[t.Name()]["foo"])
	assert.EqualValues(t, cols[1], p.tableManager.Tables[t.Name()]["baz"])
}

func TestTableManager_refreshTableStructure(t *testing.T) {
	p := newPostgresqlTest(t)
	require.NoError(t, p.Connect())

	cols := []utils.Column{
		ColumnFromTag("foo", ""),
		ColumnFromField("baz", 0),
	}
	_, err := p.tableManager.EnsureStructure(
		ctx,
		p.db,
		t.Name(),
		cols,
		p.CreateTemplates,
		p.AddColumnTemplates,
		t.Name(),
		"",
	)
	require.NoError(t, err)

	p.tableManager.ClearTableCache()
	require.Empty(t, p.tableManager.Tables)

	require.NoError(t, p.tableManager.refreshTableStructure(ctx, p.db, t.Name()))

	assert.EqualValues(t, cols[0], p.tableManager.Tables[t.Name()]["foo"])
	assert.EqualValues(t, cols[1], p.tableManager.Tables[t.Name()]["baz"])
}

func TestTableManager_MatchSource(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(&p.Postgresql, metrics)[t.Name()]

	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.Contains(t, p.tableManager.Tables[t.Name() + p.TagTableSuffix], "tag")
	assert.Contains(t, p.tableManager.Tables[t.Name()], "a")
}

// verify that TableManager updates & caches the DB table structure unless the incoming metric can't fit.
func TestTableManager_cache(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(&p.Postgresql, metrics)[t.Name()]

	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
}

// Verify that when alter statements are disabled and a metric comes in with a new tag key, that the tag is omitted.
func TestTableSource_noAlterMissingTag(t *testing.T) {
	p := newPostgresqlTest(t)
	p.AddColumnTemplates = []*template.Template{}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(&p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo", "bar": "baz"}, MSI{"a": 3}),
	}
	tsrc = NewTableSources(&p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, tsrc.ColumnNames(), "bar")
}

// Verify that when alter statements are disabled with foreign tags and a metric comes in with a new tag key, that the
// field is omitted.
func TestTableSource_noAlterMissingTagTableTag(t *testing.T) {
	p := newPostgresqlTest(t)
	p.TagsAsForeignKeys = true
	p.TagTableAddColumnTemplates = []*template.Template{}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(&p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo", "bar": "baz"}, MSI{"a": 3}),
	}
	tsrc = NewTableSources(&p.Postgresql, metrics)[t.Name()]
	ttsrc := NewTagTableSource(tsrc)
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, ttsrc.ColumnNames(), "bar")
}

// verify that when alter statements are disabled and a metric comes in with a new field key, that the field is omitted.
func TestTableSource_noAlterMissingField(t *testing.T) {
	p := newPostgresqlTest(t)
	p.AddColumnTemplates = []*template.Template{}
	require.NoError(t, p.Connect())

	metrics := []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 1}),
	}
	tsrc := NewTableSources(&p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))

	metrics = []telegraf.Metric{
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 2}),
		newMetric(t, "", MSS{"tag": "foo"}, MSI{"a": 3, "b":3}),
	}
	tsrc = NewTableSources(&p.Postgresql, metrics)[t.Name()]
	require.NoError(t, p.tableManager.MatchSource(ctx, p.db, tsrc))
	assert.NotContains(t, tsrc.ColumnNames(), "b")
}
