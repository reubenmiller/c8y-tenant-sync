package main

import (
	"context"
	"net/http"
	"os"
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
	assert.Equal(t, "MINOR|Some text", AlarmMappingSpec{Type: "a", Severity: "MINOR", Description: "Some text"}.Value())
	assert.Equal(t, "NONE|", AlarmMappingSpec{Type: "a", Severity: "NONE"}.Value())

	assert.Equal(t, AlarmMappingSpec{Type: "a", Severity: "MINOR", Description: "x|y"}, parseAlarmMapping("a", "MINOR|x|y"), "only the first | separates")
	assert.Equal(t, AlarmMappingSpec{Type: "a", Severity: "MAJOR"}, parseAlarmMapping("a", "MAJOR|"))
	assert.Equal(t, AlarmMappingSpec{Type: "a", Severity: "MAJOR"}, parseAlarmMapping("a", "MAJOR"))
	assert.Equal(t, AlarmMappingSpec{Type: "a", Severity: "MINOR", Description: "x"}, parseAlarmMapping("a", "minor|x"), "the severity is upper case")
}

func TestLoadManifestAlarmMapping(t *testing.T) {
	path := writeManifest(t, `
alarmMapping:
  - type: foo2
    severity: NONE
    description: example 2
  - type: foo1
    severity: MINOR
    description: Some foo1 mapping
  - type: bar
    severity: MAJOR
  - type: lower
    severity: minor
  - type: mixed
    severity: " Critical "
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, []AlarmMappingSpec{
		{Type: "foo2", Severity: "NONE", Description: "example 2"},
		{Type: "foo1", Severity: "MINOR", Description: "Some foo1 mapping"},
		{Type: "bar", Severity: "MAJOR"},
		{Type: "lower", Severity: "MINOR"},
		{Type: "mixed", Severity: "CRITICAL"},
	}, manifest.AlarmMapping.Mappings, "severities are case-insensitive and stored in upper case")
}

func TestLoadManifestAlarmMappingRejectsUnknownSeverity(t *testing.T) {
	path := writeManifest(t, "alarmMapping:\n  - type: foo\n    severity: minr\n")
	_, err := LoadManifest(path)
	assert.ErrorContains(t, err, `alarmMapping[0]: unknown severity "MINR"`)
}

func TestLoadManifestAlarmMappingRejectsUnknownFields(t *testing.T) {
	path := writeManifest(t, "alarmMapping:\n  - type: foo\n    severity: MINOR\n    text: typo\n")
	_, err := LoadManifest(path)
	assert.ErrorContains(t, err, "text")
}

func TestValidateAlarmMapping(t *testing.T) {
	m := &Manifest{
		AlarmMapping: &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
			{Type: "ok", Severity: "CRITICAL", Description: "text"},
			{Type: "bad", Severity: "SEVERE"},
			{Type: "missing", Description: "no severity"},
			{Severity: "MAJOR"},
			{Type: "ok", Severity: "MAJOR"},
		}},
		TenantOptions: []TenantOptionSpec{
			{Category: AlarmMappingCategory, Key: "ok", Value: "MAJOR|other"},
			{Category: AlarmMappingCategory, Key: "other", Value: "MAJOR|"},
		},
	}
	err := m.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, `alarmMapping[1]: unknown severity "SEVERE"`)
	assert.ErrorContains(t, err, "alarmMapping[2]: severity is required")
	assert.ErrorContains(t, err, "alarmMapping[3]: type is required")
	assert.ErrorContains(t, err, "alarmMapping[4]: duplicate type ok")
	assert.ErrorContains(t, err, "tenantOptions[0]: alarm.type.mapping/ok is also set in alarmMapping")
	assert.NotContains(t, err.Error(), "tenantOptions[1]")
	assert.NotContains(t, err.Error(), "alarmMapping[0]")
}

func TestSyncAlarmMappingWritesOnlyChanges(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{
		"same":        "MINOR|unchanged",
		"noPipe":      "MAJOR",
		"severity":    "MAJOR|keep",
		"description": "MAJOR|old text",
		"other":       "NONE|not in the manifest",
	})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
		{Type: "same", Severity: "MINOR", Description: "unchanged"},
		{Type: "noPipe", Severity: "MAJOR"},
		{Type: "severity", Severity: "CRITICAL", Description: "keep"},
		{Type: "description", Severity: "MAJOR", Description: "new text"},
		{Type: "new", Severity: "WARNING"},
	}}))

	// One request with the changed alarm types only, encoded as
	// SEVERITY|description (other is kept, noPipe is the same mapping)
	puts := f.Requests(http.MethodPut, "/tenant/options/alarm.type.mapping")
	require.Len(t, puts, 1)
	assert.Equal(t, map[string]any{
		"severity":    "CRITICAL|keep",
		"description": "MAJOR|new text",
		"new":         "WARNING|",
	}, puts[0].Body)

	// Results follow the manifest order
	assert.Equal(t, []ChangeResult{
		{Section: SectionAlarmMapping, Item: "same", Action: ActionUnchanged, Detail: `severity MINOR, description "unchanged"`},
		{Section: SectionAlarmMapping, Item: "noPipe", Action: ActionUnchanged, Detail: "severity MAJOR (keep description)"},
		{Section: SectionAlarmMapping, Item: "severity", Action: ActionUpdated, Detail: "severity MAJOR → CRITICAL"},
		{Section: SectionAlarmMapping, Item: "description", Action: ActionUpdated, Detail: `description "old text" → "new text"`},
		{Section: SectionAlarmMapping, Item: "new", Action: ActionCreated, Detail: "severity WARNING (keep description)"},
	}, syncer.Results)
}

func TestSyncAlarmMappingUnchanged(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"foo": "MINOR|x"})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{{Type: "foo", Severity: "MINOR", Description: "x"}}}))
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"), "nothing is written when nothing changed")
	require.Len(t, syncer.Results, 1)
	assert.Equal(t, ActionUnchanged, syncer.Results[0].Action)
}

func TestSyncAlarmMappingWriteFails(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"same": "MINOR|x"})
	f.JSON("PUT /tenant/options/alarm.type.mapping", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
		{Type: "new", Severity: "MAJOR", Description: "y"},
		{Type: "same", Severity: "MINOR", Description: "x"},
	}}))
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

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{{Type: "foo", Severity: "MINOR", Description: "x"}}}))
	require.Len(t, syncer.Results, 1)
	assert.Equal(t, ActionFailed, syncer.Results[0].Action)
	assert.Equal(t, AlarmMappingCategory, syncer.Results[0].Item)
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"))
}

func TestSyncAlarmMappingDryRun(t *testing.T) {
	t.Run("without a session", func(t *testing.T) {
		syncer := &Syncer{DryRun: true}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
			{Type: "a", Severity: "MINOR", Description: "x"},
			{Type: "b", Severity: "NONE"},
		}}))
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "a", Action: ActionPlanned, Detail: `set severity MINOR, description "x"`},
			{Section: SectionAlarmMapping, Item: "b", Action: ActionPlanned, Detail: "set severity NONE (keep description)"},
		}, syncer.Results)
	})

	t.Run("compared with the tenant", func(t *testing.T) {
		f := fakeAlarmMapping(t, map[string]string{"a": "MINOR|x", "b": "MAJOR|"})
		syncer := &Syncer{DryRun: true, DryRunReader: f.Client()}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
			{Type: "a", Severity: "MINOR", Description: "x"},
			{Type: "b", Severity: "NONE"},
			{Type: "c", Severity: "WARNING"},
		}}))

		assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"), "a dry run never writes")
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "a", Action: ActionUnchanged, Detail: `severity MINOR, description "x"`},
			{Section: SectionAlarmMapping, Item: "b", Action: ActionPlanned, Detail: "severity MAJOR → NONE"},
			{Section: SectionAlarmMapping, Item: "c", Action: ActionPlanned, Detail: "severity WARNING (keep description)"},
		}, syncer.Results)
	})
}

func TestIncludeMergesAlarmMappingByType(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base.yaml", []byte(`alarmMapping:
  - {type: a, severity: MINOR, description: base}
  - {type: b, severity: MAJOR, description: base}
`))
	writeFile(t, dir, "tenant.yaml", []byte(`include:
  - path: base.yaml
alarmMapping:
  - {type: b, severity: NONE}
  - {type: c, severity: WARNING}
`))

	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	assert.Equal(t, []AlarmMappingSpec{
		{Type: "a", Severity: "MINOR", Description: "base"},
		{Type: "b", Severity: "NONE"},
		{Type: "c", Severity: "WARNING"},
	}, manifest.AlarmMapping.Mappings)
}

func TestAlarmMappingSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    severity: MINOR\n    description: some text\n  - type: bar\n    severity: NONE\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    severity: minor\n  - type: bar\n    severity: Warning\n")), "severities are case-insensitive")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    severity: minr\n")), "unknown severity")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    severity: 1\n")), "severity is a string")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n")), "severity is required")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - severity: MINOR\n")), "type is required")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: \"\"\n    severity: MINOR\n")), "type must not be empty")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  foo:\n    severity: MINOR\n")), "the map form is not accepted")
}

func TestValidateAlarmMappingState(t *testing.T) {
	m := &Manifest{AlarmMapping: &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
		{Type: "gone", State: "absent"},
		{Type: "kept", Severity: "MINOR", State: "present"},
		{Type: "kept fields", Severity: "MINOR", Description: "x", State: "absent"},
		{Type: "typo", Severity: "MINOR", State: "deleted"},
		{Type: "bad severity", Severity: "SEVERE", State: "absent"},
	}}}
	err := m.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, `alarmMapping[3]: unknown state "deleted"`)
	assert.ErrorContains(t, err, `alarmMapping[4]: unknown severity "SEVERE"`, "a severity which is set is checked, also when absent")
	assert.NotContains(t, err.Error(), "alarmMapping[0]", "an absent mapping needs no severity")
	assert.NotContains(t, err.Error(), "alarmMapping[1]")
	assert.NotContains(t, err.Error(), "alarmMapping[2]", "an absent mapping may keep its severity and description")
}

func TestSyncAlarmMappingStateAbsent(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"gone": "MAJOR|old", "kept": "MINOR|x"})
	f.JSON("DELETE /tenant/options/alarm.type.mapping/gone", http.StatusNoContent, nil)
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
		{Type: "gone", Severity: "CRITICAL", Description: "ignored", State: "absent"},
		{Type: "missing", State: "absent"},
		{Type: "kept", Severity: "MINOR", Description: "x"},
	}}))

	deletes := f.Requests(http.MethodDelete, "/tenant/options")
	require.Len(t, deletes, 1, "only mappings which exist are deleted")
	assert.Equal(t, "/tenant/options/alarm.type.mapping/gone", deletes[0].Path)
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"), "nothing else changed")

	assert.Equal(t, []ChangeResult{
		{Section: SectionAlarmMapping, Item: "gone", Action: ActionDeleted, Detail: `was severity MAJOR, description "old"`},
		{Section: SectionAlarmMapping, Item: "missing", Action: ActionUnchanged, Detail: "absent"},
		{Section: SectionAlarmMapping, Item: "kept", Action: ActionUnchanged, Detail: `severity MINOR, description "x"`},
	}, syncer.Results)
}

func TestSyncAlarmMappingStateAbsentDeleteFails(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"gone": "MAJOR|"})
	f.JSON("DELETE /tenant/options/alarm.type.mapping/gone", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{{Type: "gone", State: "absent"}}}))
	require.Len(t, syncer.Results, 1)
	assert.Equal(t, ActionFailed, syncer.Results[0].Action)
	assert.ErrorContains(t, syncer.Results[0].Err, "denied")
}

func TestSyncAlarmMappingStateAbsentDryRun(t *testing.T) {
	t.Run("without a session", func(t *testing.T) {
		syncer := &Syncer{DryRun: true}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{{Type: "gone", State: "absent"}}}))
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "gone", Action: ActionPlanned, Detail: "delete if it exists"},
		}, syncer.Results)
	})

	t.Run("compared with the tenant", func(t *testing.T) {
		f := fakeAlarmMapping(t, map[string]string{"gone": "NONE|"})
		syncer := &Syncer{DryRun: true, DryRunReader: f.Client()}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mappings: []AlarmMappingSpec{
			{Type: "gone", State: "absent"},
			{Type: "missing", State: "absent"},
		}}))
		assert.Empty(t, f.Requests(http.MethodDelete, "/tenant/options"), "a dry run never deletes")
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "gone", Action: ActionPlanned, Detail: "delete (was severity NONE (keep description))"},
			{Section: SectionAlarmMapping, Item: "missing", Action: ActionUnchanged, Detail: "absent"},
		}, syncer.Results)
	})
}

func TestAlarmMappingStateSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    state: absent\n  - type: bar\n    severity: MINOR\n    state: present\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    severity: MINOR\n    description: kept\n    state: absent\n")), "an absent mapping may keep its fields")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    severity: minr\n    state: absent\n")), "a severity which is set must be valid")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    state: present\n")), "a present mapping needs a severity")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  - type: foo\n    state: deleted\n")), "unknown state")
}

func TestLoadManifestAlarmMappingObjectForm(t *testing.T) {
	path := writeManifest(t, `
alarmMapping:
  mode: exact
  mappings:
    - type: c8y_NoisyAlarm
      severity: none
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, &AlarmMappingSectionSpec{
		Mode:     AlarmMappingModeExact,
		Mappings: []AlarmMappingSpec{{Type: "c8y_NoisyAlarm", Severity: "NONE"}},
	}, manifest.AlarmMapping)
	assert.True(t, manifest.AlarmMapping.Exact())

	// The list is the short form of mode additive
	list, err := LoadManifest(writeManifest(t, "alarmMapping:\n  - type: c8y_NoisyAlarm\n    severity: NONE\n"))
	require.NoError(t, err)
	assert.Equal(t, manifest.AlarmMapping.Mappings, list.AlarmMapping.Mappings)
	assert.False(t, list.AlarmMapping.Exact())
}

func TestLoadManifestAlarmMappingObjectFormErrors(t *testing.T) {
	_, err := LoadManifest(writeManifest(t, "alarmMapping:\n  mode: exact\n  mapping: []\n"))
	assert.ErrorContains(t, err, "mapping", "unknown fields of the object are errors")

	_, err = LoadManifest(writeManifest(t, "alarmMapping:\n  mappings:\n    - type: foo\n      severity: MINOR\n      text: typo\n"))
	assert.ErrorContains(t, err, "text", "unknown fields of a mapping are errors")

	_, err = LoadManifest(writeManifest(t, "alarmMapping: MINOR\n"))
	assert.ErrorContains(t, err, "alarmMapping must be a list of mappings or an object with mode and mappings")

	_, err = LoadManifest(writeManifest(t, "alarmMapping:\n  mode: strict\n  mappings:\n    - {type: foo}\n"))
	assert.ErrorContains(t, err, `alarmMapping.mode: unknown mode "strict"`)
	assert.ErrorContains(t, err, "alarmMapping.mappings[0]: severity is required", "errors name the path of the object form")
}

func TestValidateAlarmMappingExactTenantOptions(t *testing.T) {
	m := &Manifest{
		AlarmMapping: &AlarmMappingSectionSpec{Mode: AlarmMappingModeExact},
		TenantOptions: []TenantOptionSpec{
			{Category: AlarmMappingCategory, Key: "other", Value: "MAJOR|"},
		},
	}
	assert.ErrorContains(t, m.Validate(), "tenantOptions[0]: alarm.type.mapping/other would be deleted by alarmMapping mode exact")
}

func TestSyncAlarmMappingExact(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{
		"kept":      "MINOR|x",
		"changed":   "MAJOR|",
		"unlisted":  "CRITICAL|from the UI",
		"absent":    "NONE|",
		"unlisted2": "WARNING",
	})
	for _, alarmType := range []string{"absent", "unlisted", "unlisted2"} {
		f.JSON("DELETE /tenant/options/alarm.type.mapping/"+alarmType, http.StatusNoContent, nil)
	}
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{
		Mode: AlarmMappingModeExact,
		Mappings: []AlarmMappingSpec{
			{Type: "kept", Severity: "MINOR", Description: "x"},
			{Type: "changed", Severity: "CRITICAL"},
			{Type: "absent", State: "absent"},
		},
	}))

	puts := f.Requests(http.MethodPut, "/tenant/options/alarm.type.mapping")
	require.Len(t, puts, 1)
	assert.Equal(t, map[string]any{"changed": "CRITICAL|"}, puts[0].Body)

	var deleted []string
	for _, r := range f.Requests(http.MethodDelete, "/tenant/options") {
		deleted = append(deleted, r.Path)
	}
	assert.ElementsMatch(t, []string{
		"/tenant/options/alarm.type.mapping/absent",
		"/tenant/options/alarm.type.mapping/unlisted",
		"/tenant/options/alarm.type.mapping/unlisted2",
	}, deleted)

	assert.Equal(t, []ChangeResult{
		{Section: SectionAlarmMapping, Item: "kept", Action: ActionUnchanged, Detail: `severity MINOR, description "x"`},
		{Section: SectionAlarmMapping, Item: "changed", Action: ActionUpdated, Detail: "severity MAJOR → CRITICAL"},
		{Section: SectionAlarmMapping, Item: "absent", Action: ActionDeleted, Detail: "was severity NONE (keep description)"},
		{Section: SectionAlarmMapping, Item: "unlisted", Action: ActionDeleted, Detail: `not in the manifest, was severity CRITICAL, description "from the UI"`},
		{Section: SectionAlarmMapping, Item: "unlisted2", Action: ActionDeleted, Detail: "not in the manifest, was severity WARNING (keep description)"},
	}, syncer.Results)
}

func TestSyncAlarmMappingExactEmptyDeletesAll(t *testing.T) {
	f := fakeAlarmMapping(t, map[string]string{"a": "MINOR|", "b": "MAJOR|"})
	f.JSON("DELETE /tenant/options/alarm.type.mapping/a", http.StatusNoContent, nil)
	f.JSON("DELETE /tenant/options/alarm.type.mapping/b", http.StatusNoContent, nil)
	syncer := &Syncer{Client: f.Client()}

	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{Mode: AlarmMappingModeExact}))
	assert.Len(t, f.Requests(http.MethodDelete, "/tenant/options"), 2)
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/options"))
}

func TestSyncAlarmMappingAdditiveEmptyDoesNothing(t *testing.T) {
	f := newFakeC8y(t)
	syncer := &Syncer{Client: f.Client()}
	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), &AlarmMappingSectionSpec{}))
	require.NoError(t, syncer.SyncAlarmMapping(context.Background(), nil))
	assert.Empty(t, f.Requests(http.MethodGet, "/tenant/options"), "nothing is read")
	assert.Empty(t, syncer.Results)
}

func TestSyncAlarmMappingExactDryRun(t *testing.T) {
	section := &AlarmMappingSectionSpec{
		Mode:     AlarmMappingModeExact,
		Mappings: []AlarmMappingSpec{{Type: "kept", Severity: "MINOR"}},
	}

	t.Run("without a session", func(t *testing.T) {
		syncer := &Syncer{DryRun: true}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), section))
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "kept", Action: ActionPlanned, Detail: "set severity MINOR (keep description)"},
			{Section: SectionAlarmMapping, Item: AlarmMappingCategory, Action: ActionPlanned, Detail: "delete the mappings of alarm types which are not listed (mode exact)"},
		}, syncer.Results)
	})

	t.Run("compared with the tenant", func(t *testing.T) {
		f := fakeAlarmMapping(t, map[string]string{"kept": "MINOR|", "unlisted": "MAJOR|x"})
		syncer := &Syncer{DryRun: true, DryRunReader: f.Client()}
		require.NoError(t, syncer.SyncAlarmMapping(context.Background(), section))
		assert.Empty(t, f.Requests(http.MethodDelete, "/tenant/options"), "a dry run never deletes")
		assert.Equal(t, []ChangeResult{
			{Section: SectionAlarmMapping, Item: "kept", Action: ActionUnchanged, Detail: "severity MINOR (keep description)"},
			{Section: SectionAlarmMapping, Item: "unlisted", Action: ActionPlanned, Detail: `delete (not in the manifest, was severity MAJOR, description "x")`},
		}, syncer.Results)
	})
}

func TestIncludeMergesAlarmMappingMode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "base.yaml", []byte(`alarmMapping:
  mode: exact
  mappings:
    - {type: a, severity: MINOR}
`))
	writeFile(t, dir, "overlay.yaml", []byte(`include:
  - path: base.yaml
alarmMapping:
  - {type: b, severity: MAJOR}
`))
	writeFile(t, dir, "additive.yaml", []byte(`include:
  - path: base.yaml
alarmMapping:
  mode: additive
  mappings: []
`))

	manifest, err := LoadManifest(filepath.Join(dir, "overlay.yaml"))
	require.NoError(t, err)
	assert.True(t, manifest.AlarmMapping.Exact(), "the list form does not change the mode of the base file")
	assert.Equal(t, []AlarmMappingSpec{{Type: "a", Severity: "MINOR"}, {Type: "b", Severity: "MAJOR"}}, manifest.AlarmMapping.Mappings)

	manifest, err = LoadManifest(filepath.Join(dir, "additive.yaml"))
	require.NoError(t, err)
	assert.False(t, manifest.AlarmMapping.Exact(), "a later file which sets the mode wins")
}

func TestMarshalAlarmMapping(t *testing.T) {
	mappings := []AlarmMappingSpec{{Type: "a", Severity: "MINOR"}}

	additive, err := MarshalManifest(&Manifest{AlarmMapping: &AlarmMappingSectionSpec{Mappings: mappings}}, "test")
	require.NoError(t, err)
	assert.Contains(t, string(additive), "alarmMapping:\n  - type: a\n    severity: MINOR\n", "additive mappings are written as a list")

	exact, err := MarshalManifest(&Manifest{AlarmMapping: &AlarmMappingSectionSpec{Mode: AlarmMappingModeExact, Mappings: mappings}}, "test")
	require.NoError(t, err)
	assert.Contains(t, string(exact), "alarmMapping:\n  mode: exact\n  mappings:\n    - type: a\n      severity: MINOR\n")

	// Both round trip
	for _, content := range [][]byte{additive, exact} {
		path := filepath.Join(t.TempDir(), "tenant.yaml")
		require.NoError(t, os.WriteFile(path, content, 0o644))
		loaded, err := LoadManifest(path)
		require.NoError(t, err)
		assert.Equal(t, mappings, loaded.AlarmMapping.Mappings)
	}
}

func TestAlarmMappingObjectSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  mode: exact\n  mappings:\n    - type: foo\n      severity: MINOR\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("alarmMapping:\n  mode: exact\n")), "exact without mappings deletes all mappings")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  mode: strict\n")), "unknown mode")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  mode: exact\n  mapping: []\n")), "unknown property")
	assert.Error(t, validateYAML(t, schema, []byte("alarmMapping:\n  mappings:\n    - type: foo\n")), "the mappings are checked too")
}
