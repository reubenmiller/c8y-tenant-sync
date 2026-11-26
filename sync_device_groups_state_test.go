package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// absentEuropeTree is the group tree of europeGroups with every group
// absent
func absentEuropeTree() []DeviceGroupSpec {
	return []DeviceGroupSpec{{
		Name:  "Europe",
		State: DeviceGroupStateAbsent,
		Groups: []DeviceGroupSpec{
			{Name: "Plant Berlin", State: DeviceGroupStateAbsent},
			{Name: "Plant Paris", State: DeviceGroupStateAbsent},
		},
	}}
}

// europeGroups adds Europe (100) with Plant Berlin (101) and Plant Paris
// (102) to the fake inventory
func europeGroups(inv *fakeInventory) {
	inv.group("100", "Europe", deviceGroupType, "", nil)
	inv.group("101", "Plant Berlin", deviceSubgroupType, "100", nil)
	inv.group("102", "Plant Paris", deviceSubgroupType, "100", nil)
}

// deletedIDs returns the IDs of the deleted managed objects in order
func deletedIDs(f *fakeC8y) []string {
	var ids []string
	for _, r := range f.Requests(http.MethodDelete, "/inventory/managedObjects/") {
		ids = append(ids, r.Path[len("/inventory/managedObjects/"):])
	}
	return ids
}

func TestValidateDeviceGroupState(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"unknown state", "deviceGroups:\n  - name: a\n    state: deleted\n", `deviceGroups[0]: unknown state "deleted": supported values are present, absent`},
		{"absent with fields", "deviceGroups:\n  - name: a\n    state: absent\n    description: d\n    fragments: {x: 1}\n", "deviceGroups[0]: a group with state absent may only have name and groups (remove description, fragments)"},
		{"present child of absent", "deviceGroups:\n  - name: a\n    state: absent\n    groups:\n      - name: b\n", `deviceGroups[0].groups[0]: the parent group has state absent, so group "b" must have state absent too (or be removed from the manifest)`},
		{"user references absent path", "deviceGroups:\n  - name: a\n    groups:\n      - name: b\n        state: absent\nusers:\n  - email: u@example.com\n    inventoryRoles: [{group: a/b, roles: [r]}]\n", `users[0].inventoryRoles[0]: device group "a/b" has state absent in deviceGroups`},
		{"user references absent name", "deviceGroups:\n  - name: a\n    groups:\n      - name: b\n        state: absent\nusers:\n  - email: u@example.com\n    inventoryRoles: [{group: b, roles: [r]}]\n", `users[0].inventoryRoles[0]: device group "b" has state absent in deviceGroups`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}

	_, err := LoadManifest(writeManifest(t, "deviceGroups:\n  - name: a\n    state: present\n    groups:\n      - name: b\n        state: absent\n        groups:\n          - name: c\n            state: absent\n"))
	assert.NoError(t, err, "an absent group may be below a present one")
}

func TestSchemaDeviceGroupState(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("deviceGroups:\n  - name: a\n    state: absent\n    groups:\n      - name: b\n        state: absent\n")))
	assert.Error(t, validateYAML(t, schema, []byte("deviceGroups:\n  - name: a\n    state: deleted\n")))
}

func TestSyncDeviceGroupsAbsentDeletesChildrenFirst(t *testing.T) {
	inv := newFakeInventory(t)
	europeGroups(inv)
	s := inv.syncer(t)

	require.NoError(t, s.SyncDeviceGroups(context.Background(), absentEuropeTree()))
	assert.Empty(t, deletedIDs(inv.f), "groups are deleted after the users section")
	s.runDeferredDeletes(context.Background())

	results := deviceGroupResults(s)
	for _, path := range []string{"Europe", "Europe/Plant Berlin", "Europe/Plant Paris"} {
		require.NoError(t, results[path].Err, path)
		assert.Equal(t, ActionDeleted, results[path].Action, path)
	}
	assert.Equal(t, []string{"101", "102", "100"}, deletedIDs(inv.f))
	for _, r := range inv.f.Requests(http.MethodDelete, "/inventory/managedObjects/") {
		assert.Empty(t, r.Query, "groups are never deleted with cascade")
	}

	// A second run finds nothing to delete
	s2 := inv.syncer(t)
	require.NoError(t, s2.SyncDeviceGroups(context.Background(), absentEuropeTree()))
	s2.runDeferredDeletes(context.Background())
	for path, r := range deviceGroupResults(s2) {
		assert.Equal(t, ActionUnchanged, r.Action, path)
		assert.Equal(t, "absent", r.Detail, path)
	}
	assert.Len(t, deviceGroupResults(s2), 3)
}

func TestSyncDeviceGroupsAbsentNotEmpty(t *testing.T) {
	inv := newFakeInventory(t)
	europeGroups(inv)
	for i := 1; i <= 7; i++ {
		id := fmt.Sprintf("50%d", i)
		inv.add(id, map[string]any{"name": fmt.Sprintf("Sensor %d", i), "c8y_IsDevice": map[string]any{}})
		inv.link("101", id)
	}
	inv.add("600", map[string]any{"name": "Gateway"})
	inv.childDevices["101"] = []string{"600"}
	s := inv.syncer(t)

	require.NoError(t, s.SyncDeviceGroups(context.Background(), absentEuropeTree()))
	s.runDeferredDeletes(context.Background())

	results := deviceGroupResults(s)
	berlin := results["Europe/Plant Berlin"]
	assert.Equal(t, ActionFailed, berlin.Action)
	assert.EqualError(t, berlin.Err, "cannot delete device group: it still has 7 child assets (Sensor 1, Sensor 2, Sensor 3, Sensor 4, Sensor 5, ...) and 1 child device (Gateway); move or delete them first (a group is never deleted with its content)")
	assert.Equal(t, ActionDeleted, results["Europe/Plant Paris"].Action)
	europe := results["Europe"]
	assert.Equal(t, ActionFailed, europe.Action)
	assert.ErrorContains(t, europe.Err, "it still has 1 child asset (Plant Berlin)")
	assert.Equal(t, []string{"102"}, deletedIDs(inv.f), "only the empty group is deleted")
}

func TestSyncDeviceGroupsAbsentBelowPresent(t *testing.T) {
	inv := newFakeInventory(t)
	europeGroups(inv)
	s := inv.syncer(t)

	specs := []DeviceGroupSpec{{
		Name:   "Europe",
		Groups: []DeviceGroupSpec{{Name: "Plant Berlin"}, {Name: "Plant Paris", State: DeviceGroupStateAbsent}, {Name: "Plant Rome", State: DeviceGroupStateAbsent}},
	}}
	require.NoError(t, s.SyncDeviceGroups(context.Background(), specs))
	s.runDeferredDeletes(context.Background())

	results := deviceGroupResults(s)
	assert.Equal(t, ActionUnchanged, results["Europe"].Action)
	assert.Equal(t, ActionUnchanged, results["Europe/Plant Berlin"].Action)
	assert.Equal(t, ActionDeleted, results["Europe/Plant Paris"].Action)
	assert.Equal(t, ActionUnchanged, results["Europe/Plant Rome"].Action)
	assert.Equal(t, "absent", results["Europe/Plant Rome"].Detail)
	assert.Equal(t, []string{"102"}, deletedIDs(inv.f))
}

func TestSyncDeviceGroupsAbsentOnlyDeviceGroups(t *testing.T) {
	// With --only deviceGroups the deletes run right after the section
	inv := newFakeInventory(t)
	europeGroups(inv)
	s := inv.syncer(t)

	manifest := &Manifest{DeviceGroups: absentEuropeTree()}
	require.NoError(t, s.applyTarget(context.Background(), context.Background(), manifest, []string{SectionDeviceGroups}))

	assert.Equal(t, []string{"101", "102", "100"}, deletedIDs(inv.f))
}

func TestSyncDeviceGroupsAbsentDryRun(t *testing.T) {
	inv := newFakeInventory(t)
	europeGroups(inv)
	inv.add("501", map[string]any{"name": "Sensor 1"})
	inv.link("102", "501")
	s := newDryRunReaderSyncer(inv.f)

	specs := absentEuropeTree()
	specs = append(specs, DeviceGroupSpec{Name: "Asia", Groups: []DeviceGroupSpec{{Name: "Plant Tokyo", State: DeviceGroupStateAbsent}}})
	require.NoError(t, s.SyncDeviceGroups(context.Background(), specs))
	s.runDeferredDeletes(context.Background())

	results := deviceGroupResults(s)
	assert.Equal(t, ActionPlanned, results["Europe/Plant Berlin"].Action)
	assert.Equal(t, "delete after the users section (fails if not empty)", results["Europe/Plant Berlin"].Detail)
	assert.Equal(t, ActionFailed, results["Europe/Plant Paris"].Action)
	assert.ErrorContains(t, results["Europe/Plant Paris"].Err, "it still has 1 child asset (Sensor 1)")
	europe := results["Europe"]
	assert.Equal(t, ActionFailed, europe.Action, "Plant Paris would remain in Europe")
	assert.ErrorContains(t, europe.Err, "it still has 1 child asset (Plant Paris)", "Plant Berlin would be deleted, so it is left out")
	assert.Equal(t, ActionPlanned, results["Asia"].Action)
	assert.Equal(t, ActionUnchanged, results["Asia/Plant Tokyo"].Action, "the parent would only be created by the run")
	assertOnlyReads(t, inv.f)

	// Without a session the dry run can not read the tenant
	offline := &Syncer{DryRun: true}
	require.NoError(t, offline.SyncDeviceGroups(context.Background(), absentEuropeTree()))
	offline.runDeferredDeletes(context.Background())
	var items []string
	for _, r := range offline.Results {
		assert.Equal(t, ActionPlanned, r.Action)
		assert.Equal(t, "delete if it exists and is empty, after the users section", r.Detail)
		items = append(items, r.Item)
	}
	assert.Equal(t, []string{"Europe/Plant Berlin", "Europe/Plant Paris", "Europe"}, items)
}
