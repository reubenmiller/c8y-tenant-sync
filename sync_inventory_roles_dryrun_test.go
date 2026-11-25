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

// newDryRunReaderSyncer returns a dry-run syncer which reads the fake tenant
// through its dry-run reader and has no client to write with
func newDryRunReaderSyncer(f *fakeC8y) *Syncer {
	return &Syncer{DryRun: true, DryRunReader: f.Client()}
}

// assertOnlyReads checks that the fake tenant received no writes
func assertOnlyReads(t *testing.T, f *fakeC8y) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.Path == "/tenant/oauth/token" {
			continue
		}
		assert.Equal(t, http.MethodGet, r.Method, "dry run writes %s %s", r.Method, r.Path)
	}
}

// resultsFor returns the results of a section by item
func resultsFor(s *Syncer, section string) map[string]ChangeResult {
	results := map[string]ChangeResult{}
	for _, r := range s.Results {
		if r.Section == section {
			results[r.Item] = r
		}
	}
	return results
}

func TestSyncInventoryRolesDryRunComparesWithTenant(t *testing.T) {
	f := newFakeC8y(t)
	restartOnly := map[string]any{
		"id": 8, "name": "Restart only",
		"permissions": []map[string]any{{"scope": "OPERATION", "type": "c8y_Restart", "permission": "ADMIN"}},
	}
	fakeInventoryRoles(f,
		siteOperatorRole(
			map[string]any{"scope": "ALARM", "type": "*", "permission": "READ"},
			map[string]any{"scope": "OPERATION", "type": "*", "permission": "ADMIN"},
			map[string]any{"scope": "MEASUREMENT", "type": "*", "permission": "READ"},
			map[string]any{"scope": "MANAGED_OBJECT", "type": "*", "permission": "READ"},
		),
		restartOnly,
	)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{
		siteOperatorSpec,
		{Name: "Restart only", Description: "Restart devices", Permissions: []InventoryRolePermissionSpec{
			{Scope: "OPERATION", Type: "c8y_Restart", Permission: "ADMIN"},
		}},
		{Name: "Reader", Permissions: []InventoryRolePermissionSpec{{Scope: "*", Permission: "READ"}}},
		{Name: "Empty"},
	}))

	results := resultsFor(s, SectionInventoryRoles)
	require.Len(t, results, 4)

	assert.Equal(t, ActionPlanned, results["Site operator"].Action)
	assert.Equal(t, "permissions: +ALARM:*:ADMIN, -ALARM:*:READ", results["Site operator"].Detail)

	assert.Equal(t, ActionPlanned, results["Restart only"].Action)
	assert.Equal(t, `description: "" → "Restart devices"`, results["Restart only"].Detail)

	assert.Equal(t, ActionPlanned, results["Reader"].Action)
	assert.Equal(t, "create with permissions: *:*:READ", results["Reader"].Detail)
	assert.Equal(t, "create", results["Empty"].Detail)

	assert.True(t, s.pending().inventoryRoles["Reader"], "a role to create is pending")
	assert.False(t, s.pending().inventoryRoles["Site operator"], "an existing role is not pending")
	assertOnlyReads(t, f)
}

func TestSyncInventoryRolesDryRunUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, siteOperatorRole(
		map[string]any{"scope": "ALARM", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "OPERATION", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "MEASUREMENT", "type": "*", "permission": "READ"},
		map[string]any{"scope": "MANAGED_OBJECT", "type": "*", "permission": "READ"},
	))
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, r.Detail)
	assertOnlyReads(t, f)
}

func TestSyncInventoryRolesDryRunAmbiguous(t *testing.T) {
	f := newFakeC8y(t)
	other := siteOperatorRole()
	other["id"] = 8
	fakeInventoryRoles(f, siteOperatorRole(), other)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, "ambiguous inventory role name")
}

func TestSyncInventoryRolesDryRunOtherTenant(t *testing.T) {
	f := newFakeC8y(t)
	s := newDryRunReaderSyncer(f)
	// The credentials of other tenants are only resolved in a real run
	s.activeTarget = &Target{TenantID: "t456"}

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "ensure inventory role with permissions: ALARM:*:ADMIN, MANAGED_OBJECT:*:READ, MEASUREMENT:*:READ, OPERATION:*:ADMIN", r.Detail)
	assert.Empty(t, f.Requests(http.MethodGet, "/user/inventoryroles"))
}

func TestSyncUsersDryRunInventoryRoles(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole(), map[string]any{"id": 8, "name": "Reader"})
	fakeDeviceGroups(f,
		fakeGroup{ID: "100", Name: "Plant Berlin"},
		fakeGroup{ID: "200", Name: "Plant Paris"},
	)
	// The user has Reader on Plant Berlin
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 8}}})
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username: "berlin-ops@example.com",
		InventoryRoles: []InventoryAssignmentSpec{
			{Group: "Plant Berlin", Roles: []string{"Reader", "Site operator"}},
			{Group: "Plant Paris", Roles: []string{"Reader"}},
		},
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: +Site operator; inventory roles on Plant Paris: +Reader", r.Detail)
	assertOnlyReads(t, f)
}

func TestSyncUsersDryRunInventoryRolesUnchanged(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}}})
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:       "berlin-ops@example.com",
		InventoryRoles: []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Site operator"}}},
	}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, r.Detail)
}

func TestSyncUsersDryRunNewUser(t *testing.T) {
	f := fakeUsers(t)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:       "new-hire@example.com",
		GlobalRoles:    []string{"operators"},
		InventoryRoles: []InventoryAssignmentSpec{{Group: "Plant Berlin", Roles: []string{"Site operator"}}},
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "create user; assign global roles: operators; inventory roles on Plant Berlin: +Site operator", r.Detail)
	assert.Empty(t, f.Requests(http.MethodGet, "/user/t123/users/new/roles/inventory"), "a new user has no assignments")
	assertOnlyReads(t, f)
}

func TestSyncUsersDryRunNewUserWithoutEmail(t *testing.T) {
	f := fakeUsers(t)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "svc-dashboard"}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "create user", r.Detail)
	assert.ErrorContains(t, r.Err, "an email is required to create a user")
}

func TestSyncUsersDryRunProfileAndGlobalRoles(t *testing.T) {
	f := fakeUsers(t, existingUser)
	f.JSON("GET /user/t123/users/u1/groups", http.StatusOK, map[string]any{
		"references": []map[string]any{{"group": map[string]any{"id": 1, "name": "operators"}}},
		"statistics": stats(),
	})
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:    "berlin-ops@example.com",
		FirstName:   "Berlin",
		GlobalRoles: []string{"operators", "readers"},
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "update firstName; assign global roles: readers", r.Detail)
	assertOnlyReads(t, f)
}

// TestSyncUsersDryRunPendingRoleAndGroup checks that a role or group which
// the same dry run would create is reported as new, not as not found
func TestSyncUsersDryRunPendingRoleAndGroup(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncDeviceGroups(context.Background(), []DeviceGroupSpec{
		{Name: "Europe", Groups: []DeviceGroupSpec{{Name: "Plant Paris"}}},
	}))
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{
		{Name: "Restart only", Permissions: []InventoryRolePermissionSpec{{Scope: "OPERATION", Type: "c8y_Restart", Permission: "ADMIN"}}},
	}))
	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username: "berlin-ops@example.com",
		InventoryRoles: []InventoryAssignmentSpec{
			{Group: "Plant Berlin", Roles: []string{"Site operator", "Restart only"}},
			{Group: "Europe/Plant Paris", Roles: []string{"Restart only"}},
		},
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: +Site operator, +Restart only (new role); "+
		"inventory roles on Europe/Plant Paris (new group): +Restart only (new role)", r.Detail)
	assertOnlyReads(t, f)
}

func TestSyncUsersDryRunInventoryRoleNotFound(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"}, fakeGroup{ID: "200", Name: "Plant Paris"})
	fakeAssignments(f)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username: "berlin-ops@example.com",
		InventoryRoles: []InventoryAssignmentSpec{
			{Group: "Plant Berlin", Roles: []string{"Site operator"}},
			{Group: "Plant Paris", Roles: []string{"Unknown role"}},
			{Group: "Plant Rome", Roles: []string{"Site operator"}},
		},
	}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: +Site operator; assign inventory roles", r.Detail)
	assert.ErrorContains(t, r.Err, `inventory role "Unknown role": not found`)
	assert.ErrorContains(t, r.Err, `device group "Plant Rome": not found`)
	assertOnlyReads(t, f)
}

// TestInventoryAssignmentFailureKeepsOtherAssignments checks that a role
// which cannot be resolved fails the user, but the user's other assignments
// are still applied
func TestInventoryAssignmentFailureKeepsOtherAssignments(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"}, fakeGroup{ID: "200", Name: "Plant Paris"})
	fakeAssignments(f)

	r := syncBerlinOps(t, f,
		InventoryAssignmentSpec{Group: "Plant Paris", Roles: []string{"Unknown role"}},
		InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Site operator"}},
	)

	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `inventory role "Unknown role": not found`)
	assert.Equal(t, "inventory roles on Plant Berlin: +Site operator; assign inventory roles", r.Detail)
	created := f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory")
	require.Len(t, created, 1)
	assert.Equal(t, "100", created[0].Body["managedObject"])
}

func TestSyncUsersAssignsMissingGlobalRoles(t *testing.T) {
	f := fakeUsers(t, existingUser)
	f.JSON("GET /user/t123/users/u1/groups", http.StatusOK, map[string]any{
		"references": []map[string]any{{"group": map[string]any{"id": 1, "name": "operators"}}},
		"statistics": stats(),
	})
	f.JSON("GET /user/t123/groupByName/readers", http.StatusOK, map[string]any{"id": 2, "name": "readers"})
	f.JSON("POST /user/t123/groups/2/users", http.StatusCreated, map[string]any{})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:    "berlin-ops@example.com",
		GlobalRoles: []string{"operators", "readers"},
	}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "assigned global roles: readers", r.Detail)
	assigned := f.Requests(http.MethodPost, "/user/t123/groups/2/users")
	require.Len(t, assigned, 1)
	assert.Equal(t, map[string]any{"self": "http://example/user/t123/users/u1"}, assigned[0].Body["user"])
	assert.Empty(t, f.Requests(http.MethodGet, "/user/t123/groupByName/operators"), "a role the user has is not looked up")
}

func TestDryRunReadsTenant(t *testing.T) {
	assert.False(t, dryRunReadsTenant(&Manifest{}, nil))
	assert.True(t, dryRunReadsTenant(&Manifest{LoginOptions: &LoginOptionsSpec{}}, nil))
	assert.True(t, dryRunReadsTenant(&Manifest{InventoryRoles: []InventoryRoleSpec{{Name: "a"}}}, nil))
	assert.True(t, dryRunReadsTenant(&Manifest{Users: []UserSpec{{Username: "a"}}}, nil))
	assert.False(t, dryRunReadsTenant(&Manifest{Users: []UserSpec{{Username: "a"}}}, []string{SectionSoftware}))
	assert.True(t, dryRunReadsTenant(&Manifest{Users: []UserSpec{{Username: "a"}}}, []string{SectionUsers}))
}

// TestRunDryInventoryRolesAndAssignments runs a dry run end to end: the
// role which the inventoryRoles section would create is reported as new for
// the user, and nothing is written
func TestRunDryInventoryRolesAndAssignments(t *testing.T) {
	f := fakeUsers(t, existingUser)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	fakeInventoryRoles(f, map[string]any{
		"id": 8, "name": "Restart only",
		"permissions": []map[string]any{{"scope": "OPERATION", "type": "c8y_Restart", "permission": "READ"}},
	})
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 8}}})
	setupRunAgainst(t, f)

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`
inventoryRoles:
  - name: Site operator
    permissions:
      - {scope: ALARM, permission: ADMIN}
  - name: Restart only
    permissions:
      - {scope: OPERATION, type: c8y_Restart, permission: ADMIN}
users:
  - email: berlin-ops@example.com
    inventoryRoles:
      - group: Plant Berlin
        roles: [Site operator, Restart only]
`), 0o644))

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--dry")
	})
	require.NoError(t, runErr, stdout)
	assert.Contains(t, stdout, "[inventoryRoles] Site operator (planned): create with permissions: ALARM:*:ADMIN")
	assert.Contains(t, stdout, "[inventoryRoles] Restart only (planned): permissions: +OPERATION:c8y_Restart:ADMIN, -OPERATION:c8y_Restart:READ")
	assert.Contains(t, stdout, "[users] berlin-ops@example.com (planned): inventory roles on Plant Berlin: +Site operator (new role)")
	assertOnlyReads(t, f)
}
