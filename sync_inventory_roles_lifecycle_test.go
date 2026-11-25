package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDeleteInventoryRole answers the delete of an inventory role with the
// given status (204 on success)
func fakeDeleteInventoryRole(f *fakeC8y, id string, status int) {
	f.Handle("DELETE /user/inventoryroles/"+id, func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, map[string]any{"error": "security/Forbidden", "message": "role is still assigned"})
	})
}

func legacyViewerRole() map[string]any {
	return map[string]any{"id": 12, "name": "Legacy viewer", "self": "http://example/user/inventoryroles/12"}
}

func TestLoadManifestInventoryRoleLifecycle(t *testing.T) {
	manifest, err := LoadManifest(writeManifest(t, `
inventoryRoles:
  - name: Site operator
    renamedFrom: [Site operators, Operator (site)]
    permissions:
      - {scope: ALARM, permission: ADMIN}
  - name: Legacy viewer
    state: absent
`))
	require.NoError(t, err)
	require.Len(t, manifest.InventoryRoles, 2)
	assert.Equal(t, []string{"Site operators", "Operator (site)"}, manifest.InventoryRoles[0].RenamedFrom)
	assert.False(t, manifest.InventoryRoles[0].Absent())
	assert.True(t, manifest.InventoryRoles[1].Absent())
}

func TestValidateInventoryRoleLifecycle(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"unknown state", "inventoryRoles:\n  - name: a\n    state: deleted\n", `inventoryRoles[0]: unknown state "deleted": supported values are present, absent`},
		{"absent with permissions", "inventoryRoles:\n  - name: a\n    state: absent\n    permissions: [{scope: ALARM, permission: READ}]\n", "inventoryRoles[0]: a role with state absent must not have permissions"},
		{"absent with renamedFrom", "inventoryRoles:\n  - name: a\n    state: absent\n    renamedFrom: [b]\n", "inventoryRoles[0]: a role with state absent must not have renamedFrom"},
		{"empty previous name", "inventoryRoles:\n  - name: a\n    renamedFrom: ['']\n", "inventoryRoles[0].renamedFrom[0]: previous name must not be empty"},
		{"previous name is the name", "inventoryRoles:\n  - name: a\n    renamedFrom: [a]\n", `inventoryRoles[0].renamedFrom[0]: previous name "a" is the name of the role`},
		{"duplicate previous name", "inventoryRoles:\n  - name: a\n    renamedFrom: [b, b]\n", `inventoryRoles[0].renamedFrom[1]: duplicate previous name "b"`},
		{"previous name declared", "inventoryRoles:\n  - name: a\n    renamedFrom: [b]\n  - name: b\n", `inventoryRoles[0].renamedFrom[0]: previous name "b" is also declared as an inventory role`},
		{"previous name of two roles", "inventoryRoles:\n  - name: a\n    renamedFrom: [c]\n  - name: b\n    renamedFrom: [c]\n", `inventoryRoles[1].renamedFrom[0]: previous name "c" is also a previous name of "a"`},
		{"user references absent role", "inventoryRoles:\n  - name: a\n    state: absent\nusers:\n  - email: u@example.com\n    inventoryRoles: [{group: g, roles: [a]}]\n", `users[0].inventoryRoles[0].roles[0]: inventory role "a" has state absent in inventoryRoles`},
		{"user references previous name", "inventoryRoles:\n  - name: a\n    renamedFrom: [b]\nusers:\n  - email: u@example.com\n    inventoryRoles: [{group: g, roles: [b]}]\n", `users[0].inventoryRoles[0].roles[0]: inventory role "b" is renamed to "a" in inventoryRoles`},
		{"present and absent with one name", "inventoryRoles:\n  - name: a\n  - name: a\n    state: absent\n", `inventoryRoles[1]: duplicate inventory role name "a"`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}

	_, err := LoadManifest(writeManifest(t, "inventoryRoles:\n  - name: a\n    state: present\n    renamedFrom: [b]\n  - name: c\n    state: absent\n    description: kept for the record\n"))
	assert.NoError(t, err, "present is the default state and an absent role may keep its description")
}

func TestSchemaInventoryRoleLifecycle(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("inventoryRoles:\n  - name: a\n    renamedFrom: [b]\n  - name: c\n    state: absent\n")))
	assert.Error(t, validateYAML(t, schema, []byte("inventoryRoles:\n  - name: a\n    state: deleted\n")))
}

func TestSyncInventoryRoleRename(t *testing.T) {
	f := newFakeC8y(t)
	old := siteOperatorRole(
		map[string]any{"scope": "ALARM", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "OPERATION", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "MEASUREMENT", "type": "*", "permission": "READ"},
		map[string]any{"scope": "MANAGED_OBJECT", "type": "*", "permission": "READ"},
	)
	old["name"] = "Site operators"
	fakeInventoryRoles(f, old)
	s := newFakeSyncer(t, f)

	spec := siteOperatorSpec
	spec.RenamedFrom = []string{"Operator (site)", "Site operators"}
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{spec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, `name: "Site operators" → "Site operator"`, r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/user/inventoryroles"), "the role is renamed, not created")
	updates := f.Requests(http.MethodPut, "/user/inventoryroles/7")
	require.Len(t, updates, 1)
	assert.Equal(t, "Site operator", updates[0].Body["name"])
	assert.NotContains(t, updates[0].Body, "permissions", "unchanged permissions are not sent")
}

func TestSyncInventoryRoleRenameWithChanges(t *testing.T) {
	f := newFakeC8y(t)
	old := siteOperatorRole(map[string]any{"scope": "ALARM", "type": "*", "permission": "READ"})
	old["name"] = "Operator (site)"
	old["description"] = "old"
	fakeInventoryRoles(f, old)
	s := newFakeSyncer(t, f)

	spec := InventoryRoleSpec{
		Name: "Site operator", Description: "new", RenamedFrom: []string{"Operator (site)"},
		Permissions: []InventoryRolePermissionSpec{{Scope: "ALARM", Permission: "ADMIN"}},
	}
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{spec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, `name: "Operator (site)" → "Site operator"; description: "old" → "new"; permissions: +ALARM:*:ADMIN, -ALARM:*:READ`, r.Detail)
	updates := f.Requests(http.MethodPut, "/user/inventoryroles/7")
	require.Len(t, updates, 1)
	assert.Equal(t, "Site operator", updates[0].Body["name"])
	assert.Equal(t, "new", updates[0].Body["description"])
	assert.Len(t, updates[0].Body["permissions"], 1)
}

func TestSyncInventoryRoleRenameNotNeeded(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, siteOperatorRole(
		map[string]any{"scope": "ALARM", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "OPERATION", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "MEASUREMENT", "type": "*", "permission": "READ"},
		map[string]any{"scope": "MANAGED_OBJECT", "type": "*", "permission": "READ"},
	))
	s := newFakeSyncer(t, f)

	// The role already has its new name and no role has the old one
	spec := siteOperatorSpec
	spec.RenamedFrom = []string{"Site operators"}
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{spec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, f.Requests(http.MethodPut, "/user/inventoryroles"))
}

func TestSyncInventoryRoleRenameCreatesWhenNoneExists(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f)
	s := newFakeSyncer(t, f)

	spec := siteOperatorSpec
	spec.RenamedFrom = []string{"Site operators"}
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{spec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionCreated, r.Action)
	require.Len(t, f.Requests(http.MethodPost, "/user/inventoryroles"), 1)
}

func TestSyncInventoryRoleRenameAmbiguous(t *testing.T) {
	testCases := []struct {
		name     string
		roles    []map[string]any
		expected string
	}{
		{
			name:     "new and old name exist",
			roles:    []map[string]any{siteOperatorRole(), {"id": 8, "name": "Site operators"}},
			expected: `ambiguous inventory role: roles named "Site operator" and "Site operators" (renamedFrom) both exist; delete the role which is no longer needed, or remove renamedFrom to keep both`,
		},
		{
			name:     "two old names exist",
			roles:    []map[string]any{{"id": 8, "name": "Site operators"}, {"id": 9, "name": "Operator (site)"}},
			expected: `ambiguous inventory role: 2 roles are named "Site operators", "Operator (site)" (renamedFrom); delete all but the one to rename`,
		},
		{
			name:     "old name exists twice",
			roles:    []map[string]any{{"id": 8, "name": "Site operators"}, {"id": 9, "name": "Site operators"}},
			expected: `ambiguous inventory role: 2 roles are named "Site operators" (renamedFrom); delete all but the one to rename`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeC8y(t)
			fakeInventoryRoles(f, tc.roles...)
			s := newFakeSyncer(t, f)

			spec := siteOperatorSpec
			spec.RenamedFrom = []string{"Site operators", "Operator (site)"}
			require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{spec}))

			r := resultFor(t, s, SectionInventoryRoles)
			assert.Equal(t, ActionFailed, r.Action)
			assert.EqualError(t, r.Err, tc.expected)
			assert.Empty(t, f.Requests(http.MethodPut, "/user/inventoryroles"))
			assert.Empty(t, f.Requests(http.MethodPost, "/user/inventoryroles"))
		})
	}
}

func TestSyncInventoryRoleDelete(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, legacyViewerRole())
	fakeDeleteInventoryRole(f, "12", http.StatusNoContent)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))
	assert.Empty(t, s.Results, "the delete waits for the users section")
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/inventoryroles/12"))

	s.DeleteInventoryRoles(context.Background())

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionDeleted, r.Action)
	assert.Len(t, f.Requests(http.MethodDelete, "/user/inventoryroles/12"), 1)
	assert.Empty(t, s.roleDeletes)
}

func TestSyncInventoryRoleDeleteAlreadyAbsent(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, siteOperatorRole())
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Equal(t, "absent", r.Detail)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/inventoryroles"))
}

func TestSyncInventoryRoleDeleteRefused(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, legacyViewerRole())
	fakeDeleteInventoryRole(f, "12", http.StatusForbidden)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))
	s.DeleteInventoryRoles(context.Background())

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "delete inventory role", r.Detail)
	require.Error(t, r.Err)
	assert.Contains(t, r.Err.Error(), "role is still assigned", "the role fails with the error of the API")
	assert.Empty(t, f.Requests(http.MethodGet, "/user/t123/users"), "holders are only looked up when the role is in use (422)")
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users"), "assignments of users are not revoked")
}

// fakeRoleInUse answers the delete of an inventory role like Cumulocity does
// for a role which is still assigned
func fakeRoleInUse(f *fakeC8y, id string) {
	f.JSON("DELETE /user/inventoryroles/"+id, http.StatusUnprocessableEntity, map[string]any{
		"error": "undefined/validationError", "message": "Cannot remove used InventoryRole",
	})
}

func TestSyncInventoryRoleDeleteInUseNamesHolders(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, legacyViewerRole())
	fakeRoleInUse(f, "12")
	f.JSON("GET /user/t123/users", http.StatusOK, map[string]any{
		"users": []map[string]any{
			existingUser,
			{"id": "u2", "userName": "jdoe", "self": "http://example/user/t123/users/u2"},
		},
		"statistics": stats(),
	})
	fakeAssignments(f,
		map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}, {"id": 12}}},
		map[string]any{"id": 32, "managedObject": "200", "roles": []map[string]any{{"id": 7}}},
	)
	f.JSON("GET /user/t123/users/u2/roles/inventory", http.StatusOK, map[string]any{
		"inventoryAssignments": []map[string]any{{"id": 40, "managedObject": "300", "roles": []map[string]any{{"id": 12}}}},
		"statistics":           stats(),
	})
	fakeManagedObject(f, "100", "Plant Berlin")
	fakeManagedObject(f, "300", "Plant Paris")
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))
	s.DeleteInventoryRoles(context.Background())

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.EqualError(t, r.Err, "cannot delete inventory role: still assigned to berlin-ops@example.com on Plant Berlin, jdoe on Plant Paris; revoke it there or declare those users with the role removed")
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory/31"), "assignments are never revoked to delete a role")
	assert.Empty(t, f.Requests(http.MethodGet, "/inventory/managedObjects/200"), "groups without the role are not read")
}

func TestSyncInventoryRoleDeletedAfterUsersRevokeIt(t *testing.T) {
	// The exact user loses the role in the users section, so the role can
	// be deleted in the same run
	f := fakeUsers(t, existingUser)
	fakeCurrentUser(f, "tenant-sync")
	fakeInventoryRoles(f, siteOperatorRole(), legacyViewerRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}, {"id": 12}}})
	f.Handle("DELETE /user/inventoryroles/12", func(w http.ResponseWriter, r *http.Request) {
		if len(f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory/31")) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "undefined/validationError", "message": "Cannot remove used InventoryRole"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s := newFakeSyncer(t, f)

	manifest := &Manifest{
		InventoryRoles: []InventoryRoleSpec{{Name: "Legacy viewer", State: InventoryRoleStateAbsent}},
		Users: []UserSpec{{
			Username:           "berlin-ops@example.com",
			InventoryRolesMode: UserInventoryRolesExact,
			InventoryRoles:     []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Site operator"}}},
		}},
	}
	require.NoError(t, s.applyTarget(context.Background(), context.Background(), manifest, nil))

	results := resultsFor(s, SectionInventoryRoles)
	require.NoError(t, results["Legacy viewer"].Err)
	assert.Equal(t, ActionDeleted, results["Legacy viewer"].Action)
	user := resultFor(t, s, SectionUsers)
	require.NoError(t, user.Err)
	assert.Contains(t, user.Detail, "-Legacy viewer")
}

func TestSyncInventoryRoleDeletedWithoutUsersSection(t *testing.T) {
	// With --only inventoryRoles the delete runs right after the section
	f := newFakeC8y(t)
	fakeInventoryRoles(f, legacyViewerRole())
	fakeDeleteInventoryRole(f, "12", http.StatusNoContent)
	s := newFakeSyncer(t, f)

	manifest := &Manifest{InventoryRoles: []InventoryRoleSpec{{Name: "Legacy viewer", State: InventoryRoleStateAbsent}}}
	require.NoError(t, s.applyTarget(context.Background(), context.Background(), manifest, []string{SectionInventoryRoles}))

	assert.Equal(t, ActionDeleted, resultFor(t, s, SectionInventoryRoles).Action)
}

func TestSyncInventoryRoleDeleteAmbiguous(t *testing.T) {
	f := newFakeC8y(t)
	other := legacyViewerRole()
	other["id"] = 13
	fakeInventoryRoles(f, legacyViewerRole(), other)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `ambiguous inventory role name: 2 roles are named "Legacy viewer"`)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/inventoryroles"))
}

func TestSyncInventoryRolesDryRunRenameAndDelete(t *testing.T) {
	f := newFakeC8y(t)
	old := siteOperatorRole(map[string]any{"scope": "ALARM", "type": "*", "permission": "ADMIN"})
	old["name"] = "Site operators"
	fakeInventoryRoles(f, old, legacyViewerRole())
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{
		{Name: "Site operator", RenamedFrom: []string{"Site operators"}, Permissions: []InventoryRolePermissionSpec{
			{Scope: "ALARM", Permission: "ADMIN"},
			{Scope: "OPERATION", Permission: "ADMIN"},
		}},
		{Name: "Legacy viewer", State: "absent"},
		{Name: "Gone already", State: "absent"},
	}))

	results := resultsFor(s, SectionInventoryRoles)
	require.Len(t, results, 3)
	assert.Equal(t, ActionPlanned, results["Site operator"].Action)
	assert.Equal(t, `name: "Site operators" → "Site operator"; permissions: +OPERATION:*:ADMIN`, results["Site operator"].Detail)
	assert.Equal(t, ActionPlanned, results["Legacy viewer"].Action)
	assert.Equal(t, "delete after the users section (fails if still assigned)", results["Legacy viewer"].Detail)
	assert.Equal(t, ActionUnchanged, results["Gone already"].Action)
	assert.Equal(t, "absent", results["Gone already"].Detail)

	assert.Equal(t, int64(7), s.pending().renamedRoles["Site operator"])
	assert.True(t, s.pending().removedRoles["Site operators"])
	assert.True(t, s.pending().removedRoles["Legacy viewer"])
	assert.False(t, s.pending().inventoryRoles["Site operator"], "a renamed role is not created")
	assertOnlyReads(t, f)
}

func TestSyncInventoryRolesDryRunWithoutSessionAbsent(t *testing.T) {
	s := &Syncer{DryRun: true}

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "delete if it exists, after the users section", r.Detail)
}

func TestSyncUsersDryRunRenamedRole(t *testing.T) {
	f := fakeUsers(t, existingUser)
	old := siteOperatorRole()
	old["name"] = "Site operators"
	fakeInventoryRoles(f, old)
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"}, fakeGroup{ID: "200", Name: "Plant Paris"})
	// The user already has the role (by ID) on Plant Berlin
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}}})
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{
		{Name: "Site operator", RenamedFrom: []string{"Site operators"}},
	}))
	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username: "berlin-ops@example.com",
		InventoryRoles: []InventoryAssignmentSpec{
			{Group: "Plant Berlin", Roles: []string{"Site operator"}},
			{Group: "Plant Paris", Roles: []string{"Site operator"}},
		},
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "inventory roles on Plant Paris: +Site operator", r.Detail,
		"the new name resolves to the renamed role, which the user keeps on Plant Berlin")
	assertOnlyReads(t, f)
}

func TestSyncUsersDryRunRemovedRoleNotFound(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, legacyViewerRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f)
	s := newDryRunReaderSyncer(f)

	// Validation prevents this manifest; the syncer still refuses to plan
	// an assignment of a role the same run deletes
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: "absent"}}))
	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:       "berlin-ops@example.com",
		InventoryRoles: []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Legacy viewer"}}},
	}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `inventory role "Legacy viewer": not found`)
}
