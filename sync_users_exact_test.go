package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDeleteAssignments answers the delete of the user's (u1) assignments
// with the given status (204 on success)
func fakeDeleteAssignments(f *fakeC8y, status int, ids ...int) {
	for _, id := range ids {
		f.Handle(fmt.Sprintf("DELETE /user/t123/users/u1/roles/inventory/%d", id), func(w http.ResponseWriter, r *http.Request) {
			if status == http.StatusNoContent {
				w.WriteHeader(status)
				return
			}
			writeJSON(w, status, map[string]any{"error": "security/Forbidden", "message": "not allowed"})
		})
	}
}

// fakeManagedObject answers the read of a single managed object
func fakeManagedObject(f *fakeC8y, id, name string) {
	f.JSON("GET /inventory/managedObjects/"+id, http.StatusOK, map[string]any{"id": id, "name": name, "c8y_IsDeviceGroup": map[string]any{}})
}

// fakeExactTenant is a tenant in which berlin-ops (u1) has:
//   - Site operator and Reader on Plant Berlin (assignment 31), plus a
//     second assignment on Plant Berlin with Reader (32)
//   - Reader on Plant Hamburg (33), which the manifest does not list
func fakeExactTenant(t *testing.T) *fakeC8y {
	f := fakeUsers(t, existingUser)
	fakeCurrentUser(f, "tenant-sync")
	fakeInventoryRoles(f, siteOperatorRole(), map[string]any{"id": 8, "name": "Reader"})
	fakeDeviceGroups(f,
		fakeGroup{ID: "100", Name: "Plant Berlin"},
		fakeGroup{ID: "200", Name: "Plant Paris"},
		fakeGroup{ID: "300", Name: "Plant Hamburg"},
	)
	fakeManagedObject(f, "300", "Plant Hamburg")
	fakeAssignments(f,
		map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}, {"id": 8}}},
		map[string]any{"id": 32, "managedObject": "100", "roles": []map[string]any{{"id": 8}}},
		map[string]any{"id": 33, "managedObject": "300", "roles": []map[string]any{{"id": 8}}},
	)
	fakeDeleteAssignments(f, http.StatusNoContent, 31, 32, 33)
	return f
}

// exactSpec gives berlin-ops Site operator on Plant Berlin and Reader on
// Plant Paris, as its full inventory access
var exactSpec = UserSpec{
	Username:           "berlin-ops@example.com",
	InventoryRolesMode: UserInventoryRolesExact,
	InventoryRoles: []InventoryAssignmentSpec{
		{Group: "Plant Berlin", Roles: []string{"Site operator"}},
		{Group: "Plant Paris", Roles: []string{"Reader"}},
	},
}

func TestLoadManifestInventoryRolesMode(t *testing.T) {
	manifest, err := LoadManifest(writeManifest(t, `
users:
  - email: berlin-ops@example.com
    inventoryRolesMode: exact
    inventoryRoles:
      - group: Plant Berlin
        roles: [Site operator]
  - email: paris-ops@example.com
    inventoryRolesMode: additive
  - email: other@example.com
`))
	require.NoError(t, err)
	require.Len(t, manifest.Users, 3)
	assert.True(t, manifest.Users[0].ExactInventoryRoles())
	assert.False(t, manifest.Users[1].ExactInventoryRoles())
	assert.False(t, manifest.Users[2].ExactInventoryRoles(), "additive is the default")
}

func TestValidateInventoryRolesMode(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"unknown mode", "users:\n  - userName: a\n    inventoryRolesMode: replace\n", `users[0]: unknown inventoryRolesMode "replace": supported values are additive, exact`},
		{"absent with mode", "users:\n  - userName: a\n    state: absent\n    inventoryRolesMode: exact\n", "users[0]: a user with state absent may only have userName (remove inventoryRolesMode)"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}
}

func TestSchemaInventoryRolesMode(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("users:\n  - userName: a\n    inventoryRolesMode: exact\n  - userName: b\n    inventoryRolesMode: additive\n")))
	assert.Error(t, validateYAML(t, schema, []byte("users:\n  - userName: a\n    inventoryRolesMode: replace\n")))
}

func TestUserWarningsExactWithoutInventoryRoles(t *testing.T) {
	manifest, err := LoadManifest(writeManifest(t, `
users:
  - userName: a
    inventoryRolesMode: exact
  - userName: b
    inventoryRolesMode: exact
    inventoryRoles: []
  - userName: c
    inventoryRolesMode: exact
    inventoryRoles:
      - group: Plant Berlin
        roles: [Reader]
  - userName: d
`))
	require.NoError(t, err, "an empty exact user is valid, but warned about")
	assert.Equal(t, []string{
		`users[0]: inventoryRolesMode exact without inventoryRoles removes all inventory roles of "a"`,
		`users[1]: inventoryRolesMode exact without inventoryRoles removes all inventory roles of "b"`,
	}, manifest.UserWarnings())
}

func TestSyncUsersExactInventoryRoles(t *testing.T) {
	f := fakeExactTenant(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{exactSpec}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: -Reader, merge 2 assignments; "+
		"inventory roles on Plant Paris: +Reader; "+
		"inventory roles on Plant Hamburg (not listed): -Reader", r.Detail)

	// Plant Berlin: the first assignment keeps only Site operator, the
	// second one is merged into it (deleted)
	updates := f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory/31")
	require.Len(t, updates, 1)
	assert.Equal(t, []any{map[string]any{"id": float64(7)}}, updates[0].Body["roles"])
	assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory/32"), 1)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory/31"))

	// Plant Paris: a new assignment
	created := f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory")
	require.Len(t, created, 1)
	assert.Equal(t, "200", created[0].Body["managedObject"])

	// Plant Hamburg is not listed: its assignment is deleted
	assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory/33"), 1)
}

func TestSyncUsersExactDryRun(t *testing.T) {
	f := fakeExactTenant(t)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{exactSpec}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: -Reader, merge 2 assignments; "+
		"inventory roles on Plant Paris: +Reader; "+
		"inventory roles on Plant Hamburg (not listed): -Reader", r.Detail)
	assertOnlyReads(t, f)
}

func TestSyncUsersAdditiveKeepsOtherRoles(t *testing.T) {
	f := fakeExactTenant(t)
	s := newFakeSyncer(t, f)
	spec := exactSpec
	spec.InventoryRolesMode = ""

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{spec}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, "inventory roles on Plant Paris: +Reader", r.Detail)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory"))
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory"))
	assert.Empty(t, f.Requests(http.MethodGet, "/user/currentUser"), "additive users do not need the sync user")
}

func TestSyncUsersExactUnchanged(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeCurrentUser(f, "tenant-sync")
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}}})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:           "berlin-ops@example.com",
		InventoryRolesMode: UserInventoryRolesExact,
		InventoryRoles:     []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Site operator"}}},
	}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, r.Detail)
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory"))
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory"))
}

func TestSyncUsersExactWithoutInventoryRolesRemovesAll(t *testing.T) {
	f := fakeExactTenant(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:           "berlin-ops@example.com",
		InventoryRolesMode: UserInventoryRolesExact,
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, "inventory roles on device group 100 (not listed): -Site operator, -Reader; "+
		"inventory roles on Plant Hamburg (not listed): -Reader", r.Detail, "a group which cannot be read is shown by ID")
	for _, id := range []string{"31", "32", "33"} {
		assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory/"+id), 1, "assignment %s", id)
	}
}

func TestSyncUsersExactUnresolvedGroupKeepsUnlisted(t *testing.T) {
	// A listed group which cannot be found could be one of the unlisted
	// assignments, so none of them is deleted
	f := fakeExactTenant(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:           "berlin-ops@example.com",
		InventoryRolesMode: UserInventoryRolesExact,
		InventoryRoles: []InventoryAssignmentSpec{
			{Group: "Plant Berlin", Roles: []string{"Site operator", "Reader"}},
			{Group: "Plant Rome", Roles: []string{"Reader"}},
		},
	}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `device group "Plant Rome": not found`)
	assert.Equal(t, "inventory roles on Plant Berlin: merge 2 assignments; assign inventory roles", r.Detail)
	assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory/32"), 1, "the listed group is still applied")
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory/33"))
}

func TestSyncUsersExactDeleteRefused(t *testing.T) {
	f := fakeExactTenant(t)
	fakeDeleteAssignments(f, http.StatusForbidden, 33)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{exactSpec}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `inventory assignment for group "Plant Hamburg"`)
	assert.ErrorContains(t, r.Err, "not allowed")
	assert.Equal(t, "inventory roles on Plant Berlin: -Reader, merge 2 assignments; "+
		"inventory roles on Plant Paris: +Reader; assign inventory roles", r.Detail, "the other changes are applied")
}

func TestSyncUsersExactRefusedForSyncUser(t *testing.T) {
	f := fakeExactTenant(t)
	fakeCurrentUser(f, "Berlin-Ops@example.com")
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry run %v", dryRun), func(t *testing.T) {
			s := newFakeSyncer(t, f)
			if dryRun {
				s = newDryRunReaderSyncer(f)
			}

			require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{exactSpec}))

			r := resultFor(t, s, SectionUsers)
			assert.Equal(t, ActionFailed, r.Action)
			assert.Equal(t, "inventoryRolesMode exact", r.Detail)
			assert.ErrorIs(t, r.Err, errSyncUserExact)
		})
	}
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users/u1/roles/inventory"))
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory"))
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory"))
}

func TestSyncUsersExactNewUser(t *testing.T) {
	// A new user has no assignments to remove and needs no sync user check
	f := fakeUsers(t)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{
		{Username: "new-hire@example.com", InventoryRolesMode: UserInventoryRolesExact, InventoryRoles: []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Site operator"}}}},
		{Username: "other@example.com", InventoryRolesMode: UserInventoryRolesExact},
	}))

	results := resultsFor(s, SectionUsers)
	require.NoError(t, results["new-hire@example.com"].Err)
	assert.Equal(t, "inventory roles on Plant Berlin: +Site operator", results["new-hire@example.com"].Detail)
	assert.Equal(t, ActionCreated, results["other@example.com"].Action)
	assert.Empty(t, results["other@example.com"].Detail)
	assert.Empty(t, f.Requests(http.MethodGet, "/user/currentUser"))
}

func TestSyncUsersExactDryRunIgnoresDeletedRoles(t *testing.T) {
	// A role which the inventoryRoles section deletes in the same run is
	// not listed as a revocation (deleting the role removes it)
	f := fakeUsers(t, existingUser)
	fakeCurrentUser(f, "tenant-sync")
	fakeInventoryRoles(f, siteOperatorRole(), map[string]any{"id": 9, "name": "Legacy viewer"})
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}, {"id": 9}}})
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Legacy viewer", State: InventoryRoleStateAbsent}}))
	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:           "berlin-ops@example.com",
		InventoryRolesMode: UserInventoryRolesExact,
		InventoryRoles:     []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Site operator"}}},
	}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionUnchanged, r.Action)
	assertOnlyReads(t, f)
}

func TestSyncUsersExactDryRunWithoutSession(t *testing.T) {
	s := &Syncer{DryRun: true}

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{exactSpec}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "ensure user; inventory roles: Site operator on Plant Berlin; Reader on Plant Paris; "+
		"inventoryRolesMode exact: other inventory roles are removed", r.Detail)
}
