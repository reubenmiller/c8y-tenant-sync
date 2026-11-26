package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAlarmMapping registers the alarm mapping of a fake tenant
func fakeAlarmMapping(t *testing.T, current map[string]string) *fakeC8y {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/options/alarm.type.mapping", http.StatusOK, current)
	f.JSON("PUT /tenant/options/alarm.type.mapping", http.StatusOK, map[string]string{})
	return f
}

func TestAlarmMappingValue(t *testing.T) {
	assert.Equal(t, "MINOR|Some text", AlarmMappingEntry{Severity: "MINOR", Text: "Some text"}.Value())
	assert.Equal(t, "NONE|", AlarmMappingEntry{Severity: "NONE"}.Value())

	assert.Equal(t, AlarmMappingEntry{Severity: "MINOR", Text: "a|b"}, parseAlarmMappingValue("MINOR|a|b"), "only the first | separates")
	assert.Equal(t, AlarmMappingEntry{Severity: "MAJOR"}, parseAlarmMappingValue("MAJOR|"))
	assert.Equal(t, AlarmMappingEntry{Severity: "MAJOR"}, parseAlarmMappingValue("MAJOR"))
}

func TestLoadManifestAlarmMapping(t *testing.T) {
	path := writeManifest(t, `
alarmMapping:
  foo2:
    severity: NONE
    text: example 2
  foo1:
    severity: MINOR
    text: Some foo1 mapping
  bar:
    severity: MAJOR
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, AlarmMappingSpec{
		"bar":  {Severity: "MAJOR"},
		"foo1": {Severity: "MINOR", Text: "Some foo1 mapping"},
		"foo2": {Severity: "NONE", Text: "example 2"},
	}, manifest.AlarmMapping)
}

func TestLoadManifestAlarmMappingRejectsUnknownFields(t *testing.T) {
	path := writeManifest(t, "alarmMapping:\n  foo:\n    severity: MINOR\n    message: typo\n")
	_, err := LoadManifest(path)
	assert.ErrorContains(t, err, "message")
}

func TestValidateAlarmMapping(t *testing.T) {
	m := &Manifest{
		AlarmMapping: AlarmMappingSpec{
			"ok":      {Severity: "CRITICAL", Text: "text"},
			"bad":     {Severity: "minor", Text: "text"},
			"missing": {Text: "no severity"},
		},
		TenantOptions: []TenantOptionSpec{
			{Category: AlarmMappingCategory, Key: "ok", Value: "MAJOR|other"},
			{Category: AlarmMappingCategory, Key: "other", Value: "MAJOR|"},
		},
	}
	err := m.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, `alarmMapping.bad: unknown severity "minor"`)
	assert.ErrorContains(t, err, "alarmMapping.missing: severity is required")
	assert.ErrorContains(t, err, "tenantOptions[0]: alarm.type.mapping/ok is also set in alarmMapping")
	assert.NotContains(t, err.Error(), "tenantOptions[1]")
	assert.NotContains(t, err.Error(), "alarmMapping.ok")
}

func TestSyncAlarmMappingWritesOnlyChanges(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{
		"same":     "MINOR|unchanged",
		"noPipe":   "MAJOR",
		"severity": "MAJOR|keep",
		"text":     "MAJOR|old text",
		"other":    "NONE|not in the manifest",
	})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), AlarmMappingSpec{
		"same":     {Severity: "MINOR", Text: "unchanged"},
		"noPipe":   {Severity: "MAJOR"},
		"severity": {Severity: "CRITICAL", Text: "keep"},
		"text":     {Severity: "MAJOR", Text: "new text"},
		"new":      {Severity: "WARNING"},
	}))

	// One request with the changed alarm types only, encoded as
	// SEVERITY|text (other is kept, noPipe is the same mapping)
	puts := f.Requests(http.MethodPut, "/tenant/options/alarm.type.mapping")
	require.Len(t, puts, 1)
	assert.Equal(t, map[string]any{
		"severity": "CRITICAL|keep",
		"text":     "MAJOR|new text",
		"new":      "WARNING|",
	}, puts[0].Body)

	assert.Equal(t, []ChangeResult{
		{Section: SectionAlarmMapping, Item: "new", Action: ActionCreated, Detail: "severity WARNING (keep text)"},
		{Section: SectionAlarmMapping, Item: "noPipe", Action: ActionUnchanged, Detail: "severity MAJOR (keep text)"},
		{Section: SectionAlarmMapping, Item: "same", Action: ActionUnchanged, Detail: `severity MINOR, text "unchanged"`},
		{Section: SectionAlarmMapping, Item: "severity", Action: ActionUpdated, Detail: "severity MAJOR → CRITICAL"},
		{Section: SectionAlarmMapping, Item: "text", Action: ActionUpdated, Detail: `text "old text" → "new text"`},
	}, syncer.Results)
}

func TestSyncAlarmMappingUnchanged(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"foo": "MINOR|x"})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), AlarmMappingSpec{"foo": {Severity: "MINOR", Text: "x"}}))
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"), "nothing is written when nothing changed")
	require.Len(t, syncer.Results, 1)
	assert.Equal(t, ActionUnchanged, syncer.Results[0].Action)
}

func TestSyncAlarmMappingWriteFails(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"same": "MINOR|x"})
	f.JSON("PUT /tenant/options/alarm.type.mapping", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), AlarmMappingSpec{
		"same": {Severity: "MINOR", Text: "x"},
		"new":  {Severity: "MAJOR", Text: "y"},
	}))
	require.Len(t, syncer.Results, 2)
	assert.Equal(t, "new", syncer.Results[0].Item)
	assert.Equal(t, ActionFailed, syncer.Results[0].Action)
	assert.ErrorContains(t, syncer.Results[0].Err, "denied")
	assert.Equal(t, ActionUnchanged, syncer.Results[1].Action)
}

func TestSyncAlarmMappingReadFails(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/options/alarm.type.mapping", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), AlarmMappingSpec{"foo": {Severity: "MINOR", Text: "x"}}))
	require.Len(t, syncer.Results, 1)
	assert.Equal(t, ActionFailed, syncer.Results[0].Action)
	assert.Equal(t, AlarmMappingCategory, syncer.Results[0].Item)
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"))
}

func TestSyncAlarmMappingDryRun(t *testing.T) {
	t.Run("without a session", func(t *testing.T) {
		syncer := &Syncer{DryRun: true}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), AlarmMappingSpec{
			"b": {Severity: "NONE"},
			"a": {Severity: "MINOR", Text: "x"},
		}))
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "a", Action: ActionPlanned, Detail: `set severity MINOR, text "x"`},
			{Section: SectionAlarmMapping, Item: "b", Action: ActionPlanned, Detail: "set severity NONE (keep text)"},
		}, syncer.Results)
	})

	t.Run("compared with the tenant", func(t *testing.T) {
		f := fakeAlarmMapping(t, map[string]string{"a": "MINOR|x", "b": "MAJOR|"})
		syncer := &Syncer{DryRun: true, DryRunReader: f.Client()}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), AlarmMappingSpec{
			"a": {Severity: "MINOR", Text: "x"},
			"b": {Severity: "NONE"},
			"c": {Severity: "WARNING"},
		}))

		assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"), "a dry run never writes")
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "a", Action: ActionUnchanged, Detail: `severity MINOR, text "x"`},
			{Section: SectionAlarmMapping, Item: "b", Action: ActionPlanned, Detail: "severity MAJOR → NONE"},
			{Section: SectionAlarmMapping, Item: "c", Action: ActionPlanned, Detail: "severity WARNING (keep text)"},
		}, syncer.Results)
	})
}

func TestIncludeMergesAlarmMappingPerAlarmType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base.yaml", []byte(`alarmMapping:
  a: {severity: MINOR, text: base}
  b: {severity: MAJOR, text: base}
`))
	writeFile(t, dir, "tenant.yaml", []byte(`include:
  - path: base.yaml
alarmMapping:
  b: {severity: NONE}
  c: {severity: WARNING}
`))

	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	assert.Equal(t, AlarmMappingSpec{
		"a": {Severity: "MINOR", Text: "base"},
		"b": {Severity: "NONE"},
		"c": {Severity: "WARNING"},
	}, manifest.AlarmMapping)
}

func TestAlarmMappingSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  foo:\n    severity: MINOR\n    text: some text\n  bar:\n    severity: NONE\n")))
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  foo:\n    severity: minor\n")), "unknown severity")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  foo:\n    text: no severity\n")), "severity is required")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  foo: MINOR|text\n")), "the encoded string is not accepted")
}
