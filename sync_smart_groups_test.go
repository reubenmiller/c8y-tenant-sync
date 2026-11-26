package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(v string) *string { return &v }

// smartGroup adds an existing smart group to the fake inventory, like the
// UI creates it
func (inv *fakeInventory) smartGroup(id, name string, extra map[string]any) {
	obj := map[string]any{
		"name":                  name,
		"type":                  smartGroupType,
		smartGroupFragment:      map[string]any{},
		smartGroupQueryFragment: "",
		smartGroupColumns: map[string]any{
			"columns":    []any{},
			"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(20)},
		},
		"assetParents": map[string]any{"references": []any{}},
		"childAssets":  map[string]any{"references": []any{}},
	}
	for k, v := range extra {
		obj[k] = v
	}
	inv.add(id, obj)
}

func smartGroupResults(s *Syncer) map[string]ChangeResult {
	out := map[string]ChangeResult{}
	for _, r := range s.Results {
		if r.Section == SectionSmartGroups {
			out[r.Item] = r
		}
	}
	return out
}

// createRequests returns the bodies of the managed objects created
func createRequests(f *fakeC8y) []map[string]any {
	var bodies []map[string]any
	for _, req := range f.Requests(http.MethodPost, "/inventory/managedObjects") {
		if req.Path == "/inventory/managedObjects" {
			bodies = append(bodies, req.Body)
		}
	}
	return bodies
}

func TestSyncSmartGroupsCreates(t *testing.T) {
	inv := newFakeInventory(t)
	// A device group with the same name is not a smart group
	inv.group("1", "Linux devices", deviceGroupType, "", nil)
	s := inv.syncer(t)

	specs := []SmartGroupSpec{
		{
			Name:        "Linux devices",
			Query:       strPtr("has(c8y_IsLinux) and name eq 'rpi*'"),
			Description: "All Raspberry Pis",
			Fragments:   map[string]any{"customer_Owner": map[string]any{"team": "ops"}},
		},
		{
			Name:      "Bulk helper",
			Invisible: boolPtr(true),
			Table:     &SmartGroupTableSpec{Columns: []map[string]any{{"name": "name"}}},
		},
	}
	require.NoError(t, s.SyncSmartGroups(context.Background(), specs))

	results := smartGroupResults(s)
	require.Len(t, results, 2, "results: %+v", s.Results)
	assert.Equal(t, ActionCreated, results["Linux devices"].Action)
	assert.Equal(t, ActionCreated, results["Bulk helper"].Action)

	bodies := createRequests(inv.f)
	require.Len(t, bodies, 2)
	first := bodies[0]
	assert.Equal(t, "Linux devices", first["name"])
	assert.Equal(t, "c8y_DynamicGroup", first["type"])
	assert.Equal(t, map[string]any{}, first["c8y_IsDynamicGroup"])
	assert.Equal(t, "has(c8y_IsLinux) and name eq 'rpi*'", first["c8y_DeviceQueryString"])
	assert.Equal(t, "All Raspberry Pis", first["description"])
	assert.Equal(t, map[string]any{"team": "ops"}, first["customer_Owner"])
	assert.Contains(t, first, SyncToolFragment)
	assert.NotContains(t, first, "c8y_IsDeviceGroup")
	// The UI default device list configuration
	assert.Equal(t, map[string]any{
		"columns":    []any{},
		"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(20)},
	}, first["c8y_DeviceColumnsConfig"])

	second := bodies[1]
	assert.Equal(t, map[string]any{"invisible": map[string]any{}}, second["c8y_IsDynamicGroup"])
	assert.Equal(t, "", second["c8y_DeviceQueryString"])
	// The declared columns are merged into the UI default
	assert.Equal(t, map[string]any{
		"columns":    []any{map[string]any{"name": "name"}},
		"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(20)},
	}, second["c8y_DeviceColumnsConfig"])
	assert.NotContains(t, second, "description")

	// Smart groups are flat: nothing is linked
	assert.Empty(t, inv.children)

	// The lookup is a smart group query by name
	queries := inv.f.Requests(http.MethodGet, "/inventory/managedObjects")
	require.NotEmpty(t, queries)
	assert.Contains(t, queries[0].Query, "has%28c8y_IsDynamicGroup%29")
	assert.Contains(t, queries[0].Query, "name+eq+%27Linux+devices%27")

	// A second run changes nothing
	again := inv.syncer(t)
	require.NoError(t, again.SyncSmartGroups(context.Background(), specs))
	for item, result := range smartGroupResults(again) {
		assert.Equal(t, ActionUnchanged, result.Action, item)
	}
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"))
	assert.Len(t, createRequests(inv.f), 2)
}

func TestSyncSmartGroupsUnchanged(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("4413742", "testme", map[string]any{
		"c8y_DeviceQueryString": "has(c8y_IsLinux)",
		"description":           "d",
		// Additional values are allowed in fragments
		"customer_Owner": map[string]any{"team": "ops", "manager": "someone"},
	})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{
		Name:        "testme",
		Query:       strPtr("has(c8y_IsLinux)"),
		Description: "d",
		Table:       &SmartGroupTableSpec{Columns: []map[string]any{}, PageSize: intPtr(20)},
		Invisible:   boolPtr(false),
		Fragments:   map[string]any{"customer_Owner": map[string]any{"team": "ops"}},
	}}))

	result := resultFor(t, s, SectionSmartGroups)
	assert.Equal(t, ActionUnchanged, result.Action)
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"))
	assert.Empty(t, createRequests(inv.f))
}

func TestSyncSmartGroupsOmittedFieldsAreLeftAlone(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", map[string]any{
		"c8y_DeviceQueryString": "has(c8y_IsLinux)",
		"description":           "from the UI",
		"c8y_IsDynamicGroup":    map[string]any{"invisible": map[string]any{}},
	})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{Name: "testme"}}))
	assert.Equal(t, ActionUnchanged, resultFor(t, s, SectionSmartGroups).Action)
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"))
}

func TestSyncSmartGroupsUpdatesWhenDifferent(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", map[string]any{
		"c8y_DeviceQueryString": "has(c8y_IsLinux)",
		"description":           "old",
		"c8y_IsDynamicGroup":    map[string]any{"invisible": map[string]any{}, "other": "kept"},
		"customer_Owner":        map[string]any{"team": "dev"},
	})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{
		Name:        "testme",
		Query:       strPtr("has(c8y_IsLinux) and name eq 'rpi*'"),
		Description: "new",
		Table:       &SmartGroupTableSpec{Columns: []map[string]any{{"name": "name"}}},
		Invisible:   boolPtr(false),
		Fragments:   map[string]any{"customer_Owner": map[string]any{"team": "ops"}},
	}}))

	result := resultFor(t, s, SectionSmartGroups)
	assert.Equal(t, ActionUpdated, result.Action)
	assert.Equal(t, "updated customer_Owner, description, invisible, query, table", result.Detail)

	updates := inv.f.Requests(http.MethodPut, "/inventory/managedObjects/1")
	require.Len(t, updates, 1)
	body := updates[0].Body
	assert.Equal(t, "has(c8y_IsLinux) and name eq 'rpi*'", body["c8y_DeviceQueryString"])
	assert.Equal(t, "new", body["description"])
	// The declared columns are merged into the existing configuration (the
	// page size is kept)
	assert.Equal(t, map[string]any{
		"columns":    []any{map[string]any{"name": "name"}},
		"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(20)},
	}, body["c8y_DeviceColumnsConfig"])
	// invisible is removed, the other marker properties are kept
	assert.Equal(t, map[string]any{"other": "kept"}, body["c8y_IsDynamicGroup"])
	assert.Equal(t, map[string]any{"team": "ops"}, body["customer_Owner"])
	assert.Contains(t, body, SyncToolFragment)
	assert.NotContains(t, body, "name")
	assert.NotContains(t, body, "type")
}

func TestSyncSmartGroupsTablePageSizeKeepsColumns(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", map[string]any{
		"c8y_DeviceColumnsConfig": map[string]any{
			"columns":    []any{map[string]any{"name": "name", "visible": true}},
			"pagination": map[string]any{"currentPage": float64(3), "pageSize": float64(20)},
		},
	})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{
		Name:  "testme",
		Table: &SmartGroupTableSpec{PageSize: intPtr(50)},
	}}))
	assert.Equal(t, "updated table", resultFor(t, s, SectionSmartGroups).Detail)
	// Only the page size changes: the UI columns and current page are kept
	assert.Equal(t, map[string]any{
		"columns":    []any{map[string]any{"name": "name", "visible": true}},
		"pagination": map[string]any{"currentPage": float64(3), "pageSize": float64(50)},
	}, inv.get("1")["c8y_DeviceColumnsConfig"])
}

func TestSyncSmartGroupsTableColumnsReplacedAsWhole(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", map[string]any{
		"c8y_DeviceColumnsConfig": map[string]any{
			"columns":    []any{map[string]any{"name": "name"}, map[string]any{"name": "status"}},
			"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(50)},
		},
	})
	s := inv.syncer(t)

	// A list which is only part of the existing one is a change
	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{
		Name:  "testme",
		Table: &SmartGroupTableSpec{Columns: []map[string]any{{"name": "name"}}},
	}}))
	assert.Equal(t, "updated table", resultFor(t, s, SectionSmartGroups).Detail)
	assert.Equal(t, map[string]any{
		"columns":    []any{map[string]any{"name": "name"}},
		"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(50)},
	}, inv.get("1")["c8y_DeviceColumnsConfig"])

	// An empty table changes nothing
	again := inv.syncer(t)
	require.NoError(t, again.SyncSmartGroups(context.Background(), []SmartGroupSpec{{Name: "testme", Table: &SmartGroupTableSpec{}}}))
	assert.Equal(t, ActionUnchanged, resultFor(t, again, SectionSmartGroups).Action)
}

func TestSyncSmartGroupsCreateMergesTableIntoDefault(t *testing.T) {
	inv := newFakeInventory(t)
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{
		Name:  "new",
		Table: &SmartGroupTableSpec{PageSize: intPtr(50)},
	}}))
	bodies := createRequests(inv.f)
	require.Len(t, bodies, 1)
	assert.Equal(t, map[string]any{
		"columns":    []any{},
		"pagination": map[string]any{"currentPage": float64(1), "pageSize": float64(50)},
	}, bodies[0]["c8y_DeviceColumnsConfig"])
}

func TestSyncSmartGroupsMissingQueryEqualsEmpty(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", nil)
	delete(inv.get("1"), smartGroupQueryFragment)
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{Name: "testme", Query: strPtr("")}}))
	assert.Equal(t, ActionUnchanged, resultFor(t, s, SectionSmartGroups).Action)
}

func TestSyncSmartGroupsPrefersVisibleOverHiddenHelper(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", map[string]any{"c8y_DeviceQueryString": "old"})
	inv.smartGroup("2", "testme", map[string]any{"c8y_IsDynamicGroup": map[string]any{"invisible": map[string]any{}}})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{Name: "testme", Query: strPtr("new")}}))
	assert.Equal(t, ActionUpdated, resultFor(t, s, SectionSmartGroups).Action)
	assert.Equal(t, "new", inv.get("1")["c8y_DeviceQueryString"])
	assert.Equal(t, "", inv.get("2")["c8y_DeviceQueryString"], "the hidden helper is not changed")
}

func TestSyncSmartGroupsMakesGroupInvisible(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "helper", nil)
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{Name: "helper", Invisible: boolPtr(true)}}))
	result := resultFor(t, s, SectionSmartGroups)
	assert.Equal(t, ActionUpdated, result.Action)
	assert.Equal(t, "updated invisible", result.Detail)
	assert.Equal(t, map[string]any{"invisible": map[string]any{}}, inv.get("1")["c8y_IsDynamicGroup"])
}

func TestSyncSmartGroupsClearsQuery(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "testme", map[string]any{"c8y_DeviceQueryString": "has(c8y_IsLinux)"})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{{Name: "testme", Query: strPtr("")}}))
	assert.Equal(t, "updated query", resultFor(t, s, SectionSmartGroups).Detail)
	assert.Equal(t, "", inv.get("1")["c8y_DeviceQueryString"])
}

func TestSyncSmartGroupsDuplicateNamesFail(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("7", "testme", nil)
	inv.smartGroup("3", "testme", nil)
	// A different case is not a match
	inv.smartGroup("4", "TestMe", nil)
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{
		{Name: "testme", Query: strPtr("x")},
		{Name: "testme", State: SmartGroupStateAbsent},
	}))
	for _, r := range s.Results {
		require.Equal(t, ActionFailed, r.Action)
		assert.ErrorContains(t, r.Err, `2 smart groups named "testme" (ids: 3, 7): rename or remove the duplicates`)
	}
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"))
	assert.Empty(t, inv.f.Requests(http.MethodDelete, "/inventory/managedObjects"))
	assert.Empty(t, createRequests(inv.f))
}

func TestSyncSmartGroupsAbsentDeletes(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "old", nil)
	inv.smartGroup("2", "unlisted", nil)
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{
		{Name: "old", State: SmartGroupStateAbsent},
		{Name: "missing", State: SmartGroupStateAbsent},
	}))

	results := smartGroupResults(s)
	assert.Equal(t, ActionDeleted, results["old"].Action)
	assert.Equal(t, ActionUnchanged, results["missing"].Action)
	assert.Equal(t, "absent", results["missing"].Detail)
	assert.Equal(t, []string{"1"}, deletedIDs(inv.f))
	for _, r := range inv.f.Requests(http.MethodDelete, "/inventory/managedObjects/") {
		assert.Empty(t, r.Query, "smart groups are never deleted with cascade")
	}
	// Smart groups which are not listed are never touched
	assert.NotNil(t, inv.get("2"))
}

func TestSyncSmartGroupsAbsentIgnoresOtherFields(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "linux", map[string]any{"c8y_DeviceQueryString": "has(c8y_IsLinux)"})
	inv.smartGroup("2", "helper", nil)
	inv.smartGroup("3", "helper", map[string]any{"c8y_IsDynamicGroup": map[string]any{"invisible": map[string]any{}}})
	s := inv.syncer(t)

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{
		// The fields differ from the tenant but are ignored
		{Name: "linux", State: SmartGroupStateAbsent, Query: strPtr("other"), Description: "d", Fragments: map[string]any{"x": 1}},
		// invisible selects which of the groups with the name is deleted
		{Name: "helper", State: SmartGroupStateAbsent, Invisible: boolPtr(true)},
	}))

	results := smartGroupResults(s)
	assert.Equal(t, ActionDeleted, results["linux"].Action)
	assert.Equal(t, ActionDeleted, results["helper"].Action)
	assert.ElementsMatch(t, []string{"1", "3"}, deletedIDs(inv.f))
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/inventory/managedObjects"))
	assert.NotNil(t, inv.get("2"), "the visible group of the same name is kept")
}

func TestSyncSmartGroupsDryRunWithoutTenant(t *testing.T) {
	inv := newFakeInventory(t)
	s := inv.syncer(t)
	s.DryRun = true

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{
		{Name: "new", Query: strPtr("has(c8y_IsLinux)")},
		{Name: "old", State: SmartGroupStateAbsent},
	}))
	assert.Equal(t, []ChangeResult{
		{Section: SectionSmartGroups, Item: "new", Action: ActionPlanned, Detail: "ensure smart group"},
		{Section: SectionSmartGroups, Item: "old", Action: ActionPlanned, Detail: "delete if it exists"},
	}, s.Results)
	assert.Empty(t, inv.f.Requests(http.MethodGet, "/"), "a dry run without a session reads nothing")
}

func TestSyncSmartGroupsDryRunComparesWithTenant(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "same", map[string]any{"c8y_DeviceQueryString": "q"})
	inv.smartGroup("2", "changed", map[string]any{"c8y_DeviceQueryString": "old"})
	inv.smartGroup("3", "gone", nil)
	s := inv.syncer(t)
	s.DryRun = true
	s.DryRunReader = inv.f.Client()

	require.NoError(t, s.SyncSmartGroups(context.Background(), []SmartGroupSpec{
		{Name: "same", Query: strPtr("q")},
		{Name: "changed", Query: strPtr("new")},
		{Name: "new"},
		{Name: "gone", State: SmartGroupStateAbsent},
		{Name: "missing", State: SmartGroupStateAbsent},
	}))
	assert.Equal(t, []ChangeResult{
		{Section: SectionSmartGroups, Item: "same", Action: ActionUnchanged},
		{Section: SectionSmartGroups, Item: "changed", Action: ActionPlanned, Detail: "update query"},
		{Section: SectionSmartGroups, Item: "new", Action: ActionPlanned, Detail: "create"},
		{Section: SectionSmartGroups, Item: "gone", Action: ActionPlanned, Detail: "delete"},
		{Section: SectionSmartGroups, Item: "missing", Action: ActionUnchanged, Detail: "absent"},
	}, s.Results)
	assert.Empty(t, inv.f.Requests(http.MethodPut, "/"))
	assert.Empty(t, inv.f.Requests(http.MethodDelete, "/"))
	assert.Empty(t, createRequests(inv.f))
}

func TestDryRunReadsTenantForSmartGroups(t *testing.T) {
	manifest := &Manifest{SmartGroups: []SmartGroupSpec{{Name: "a"}}}
	assert.True(t, dryRunReadsTenant(manifest, nil))
	assert.False(t, dryRunReadsTenant(manifest, []string{SectionDeviceGroups}))
}

func TestValidateSmartGroups(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"missing name", "smartGroups:\n  - query: x\n", "smartGroups[0]: name is required"},
		{"spaces", "smartGroups:\n  - name: ' a'\n", `smartGroups[0]: name " a" must not start or end with spaces`},
		{"duplicate", "smartGroups:\n  - name: a\n  - name: a\n", `smartGroups[1]: duplicate smart group name "a"`},
		{"reserved fragment", "smartGroups:\n  - name: a\n    fragments: {c8y_DeviceQueryString: x}\n", `smartGroups[0].fragments: "c8y_DeviceQueryString" can not be set as a fragment`},
		{"reserved device group marker", "smartGroups:\n  - name: a\n    fragments: {c8y_IsDeviceGroup: {}}\n", `smartGroups[0].fragments: "c8y_IsDeviceGroup" can not be set as a fragment`},
		{"reserved bookkeeping", "smartGroups:\n  - name: a\n    fragments: {c8y_TenantSync: {}}\n", `smartGroups[0].fragments: "c8y_TenantSync" can not be set as a fragment`},
		{"page size", "smartGroups:\n  - name: a\n    table: {pageSize: 0}\n", "smartGroups[0].table.pageSize: must be at least 1 (got 0)"},
		{"unknown state", "smartGroups:\n  - name: a\n    state: deleted\n", `smartGroups[0]: unknown state "deleted": supported values are present, absent`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}

	manifest, err := LoadManifest(writeManifest(t, "smartGroups:\n  - name: Linux devices\n    query: \"has(c8y_IsLinux) and name eq 'rpi*'\"\n    table:\n      columns: []\n      pageSize: 50\n    invisible: false\n  - name: old\n    state: absent\n"))
	require.NoError(t, err)
	require.Len(t, manifest.SmartGroups, 2)
	assert.Equal(t, "has(c8y_IsLinux) and name eq 'rpi*'", *manifest.SmartGroups[0].Query)
	assert.False(t, *manifest.SmartGroups[0].Invisible)
	assert.True(t, manifest.SmartGroups[1].Absent())

	// An absent entry may keep its other fields (they are ignored), so
	// switching between the states is a one-line change
	manifest, err = LoadManifest(writeManifest(t, "smartGroups:\n  - name: a\n    state: absent\n    description: d\n    query: x\n    table: {columns: []}\n    invisible: true\n    fragments: {x: 1}\n"))
	require.NoError(t, err)
	assert.True(t, manifest.SmartGroups[0].Absent())
}

func TestSchemaSmartGroups(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("smartGroups:\n  - name: a\n    query: has(c8y_IsLinux)\n    table: {columns: [{name: name}], pageSize: 50}\n    invisible: true\n    fragments: {x: 1}\n  - name: b\n    state: absent\n")))
	assert.Error(t, validateYAML(t, schema, []byte("smartGroups:\n  - name: a\n    state: deleted\n")))
	assert.Error(t, validateYAML(t, schema, []byte("smartGroups:\n  - name: a\n    unknown: 1\n")))
	assert.Error(t, validateYAML(t, schema, []byte("smartGroups:\n  - name: a\n    table: {pageSize: 0}\n")))
	assert.Error(t, validateYAML(t, schema, []byte("smartGroups:\n  - name: a\n    columns: {columns: []}\n")))
}

func TestExportSmartGroupsRoundTrip(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("1", "no query", nil)
	delete(inv.get("1"), smartGroupQueryFragment)
	inv.smartGroup("2", "linux", map[string]any{"c8y_DeviceQueryString": "has(c8y_IsLinux)"})
	// A hidden helper with the name of a visible group is left out and does
	// not clash with it on sync
	inv.smartGroup("3", "linux", map[string]any{"c8y_IsDynamicGroup": map[string]any{"invisible": map[string]any{}}})

	specs, err := listSmartGroups(context.Background(), inv.f.Client())
	require.NoError(t, err)
	assert.Equal(t, []SmartGroupSpec{
		{Name: "linux", Query: strPtr("has(c8y_IsLinux)")},
		{Name: "no query"},
	}, specs)

	s := inv.syncer(t)
	require.NoError(t, s.SyncSmartGroups(context.Background(), specs))
	for _, r := range s.Results {
		assert.Equal(t, ActionUnchanged, r.Action, r.Item)
	}
}

func TestExportSmartGroupsDuplicateNamesFail(t *testing.T) {
	inv := newFakeInventory(t)
	inv.smartGroup("7", "linux", nil)
	inv.smartGroup("3", "linux", nil)

	_, err := listSmartGroups(context.Background(), inv.f.Client())
	assert.ErrorContains(t, err, `2 smart groups named "linux" (ids: 3, 7): rename or remove the duplicates`)
}
