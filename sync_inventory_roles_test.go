package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInventoryRoles registers the inventory role endpoints with the given
// existing roles
func fakeInventoryRoles(f *fakeC8y, roles ...map[string]any) {
	f.JSON("GET /user/inventoryroles", http.StatusOK, map[string]any{"roles": roles, "statistics": stats()})
	f.JSON("POST /user/inventoryroles", http.StatusCreated, map[string]any{"id": 99, "name": "new"})
	for _, role := range roles {
		f.JSON("PUT /user/inventoryroles/"+fmt.Sprint(role["id"]), http.StatusOK, role)
	}
}

// fakeGroup is a device group of the fake inventory
type fakeGroup struct {
	ID, Name, Parent string
}

// fakeDeviceGroups answers device group queries by name (name eq '...'),
// including the direct parents when withParents is requested
func fakeDeviceGroups(f *fakeC8y, groups ...fakeGroup) {
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		name := ""
		if i := strings.Index(query, "name eq '"); i >= 0 {
			name = strings.TrimSuffix(query[i+len("name eq '"):], "'")
			name = strings.ReplaceAll(name, "''", "'")
		}
		withParents := r.URL.Query().Get("withParents") == "true"
		items := []map[string]any{}
		for _, group := range groups {
			if group.Name != name {
				continue
			}
			item := map[string]any{"id": group.ID, "name": group.Name, "c8y_IsDeviceGroup": map[string]any{}}
			if withParents && group.Parent != "" {
				item["assetParents"] = map[string]any{"references": []map[string]any{
					{"managedObject": map[string]any{"id": group.Parent}},
				}}
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": items, "statistics": stats()})
	})
}

// fakeAssignments registers the inventory assignment endpoints of user u1
func fakeAssignments(f *fakeC8y, assignments ...map[string]any) {
	f.JSON("GET /user/t123/users/u1/roles/inventory", http.StatusOK, map[string]any{"inventoryAssignments": assignments, "statistics": stats()})
	f.JSON("POST /user/t123/users/u1/roles/inventory", http.StatusCreated, map[string]any{"id": 50})
	f.JSON("POST /user/t123/users/new/roles/inventory", http.StatusCreated, map[string]any{"id": 51})
	for _, assignment := range assignments {
		f.JSON("PUT /user/t123/users/u1/roles/inventory/"+fmt.Sprint(assignment["id"]), http.StatusOK, assignment)
	}
}

var existingUser = map[string]any{
	"id": "u1", "userName": "berlin-ops@example.com", "email": "berlin-ops@example.com",
	"enabled": true, "self": "http://example/user/t123/users/u1",
}

var siteOperatorSpec = InventoryRoleSpec{
	Name:        "Site operator",
	Description: "Alarms and operations for one site",
	Permissions: []InventoryRolePermissionSpec{
		{Scope: "ALARM", Permission: "ADMIN"},
		{Scope: "OPERATION", Permission: "ADMIN"},
		{Scope: "MEASUREMENT", Permission: "READ"},
		{Scope: "MANAGED_OBJECT", Type: "*", Permission: "READ"},
	},
}

func siteOperatorRole(permissions ...map[string]any) map[string]any {
	return map[string]any{
		"id": 7, "name": "Site operator", "description": "Alarms and operations for one site",
		"self": "http://example/user/inventoryroles/7", "permissions": permissions,
	}
}

func TestLoadManifestInventoryRoles(t *testing.T) {
	path := writeManifest(t, `
inventoryRoles:
  - name: Site operator
    description: Alarms and operations for one site
    permissions:
      - {scope: ALARM, permission: ADMIN}
      - {scope: OPERATION, type: c8y_Restart, permission: ALL}
users:
  - email: berlin-ops@example.com
    inventoryAssignments:
      - group: Europe/Plant Berlin
        roles: [Site operator, Existing role]
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)

	require.Len(t, manifest.InventoryRoles, 1)
	assert.Equal(t, "Site operator", manifest.InventoryRoles[0].Name)
	assert.Equal(t, "OPERATION:c8y_Restart:*", manifest.InventoryRoles[0].Permissions[1].Key(), "ALL is an alias of *")
	assert.Equal(t, "ALARM:*:ADMIN", manifest.InventoryRoles[0].Permissions[0].Key(), "type defaults to *")
	require.Len(t, manifest.Users[0].InventoryAssignments, 1)
	assert.Equal(t, []string{"Site operator", "Existing role"}, manifest.Users[0].InventoryAssignments[0].Roles,
		"roles not declared in the manifest are allowed")
}

func TestValidateInventoryRoles(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"missing name", "inventoryRoles:\n  - description: x\n", "inventoryRoles[0]: name is required"},
		{"duplicate name", "inventoryRoles:\n  - name: a\n  - name: a\n", `inventoryRoles[1]: duplicate inventory role name "a"`},
		{"unknown scope", "inventoryRoles:\n  - name: a\n    permissions: [{scope: DEVICE, permission: READ}]\n", `inventoryRoles[0].permissions[0]: unknown scope "DEVICE"`},
		{"missing scope", "inventoryRoles:\n  - name: a\n    permissions: [{permission: READ}]\n", "inventoryRoles[0].permissions[0]: scope is required"},
		{"unknown permission", "inventoryRoles:\n  - name: a\n    permissions: [{scope: ALARM, permission: WRITE}]\n", `inventoryRoles[0].permissions[0]: unknown permission "WRITE"`},
		{"missing permission", "inventoryRoles:\n  - name: a\n    permissions: [{scope: ALARM}]\n", "inventoryRoles[0].permissions[0]: permission is required"},
		{"duplicate permission", "inventoryRoles:\n  - name: a\n    permissions: [{scope: ALARM, permission: ALL}, {scope: ALARM, type: '*', permission: '*'}]\n", "inventoryRoles[0].permissions[1]: duplicate permission ALARM:*:*"},
		{"assignment without group", "users:\n  - email: a@example.com\n    inventoryAssignments: [{roles: [x]}]\n", "users[0].inventoryAssignments[0]: group is required"},
		{"assignment without roles", "users:\n  - email: a@example.com\n    inventoryAssignments: [{group: g}]\n", "users[0].inventoryAssignments[0]: at least one role is required"},
		{"assignment with empty role", "users:\n  - email: a@example.com\n    inventoryAssignments: [{group: g, roles: ['']}]\n", "users[0].inventoryAssignments[0].roles[0]: role must not be empty"},
		{"assignment with empty path segment", "users:\n  - email: a@example.com\n    inventoryAssignments: [{group: 'Europe//Berlin', roles: [x]}]\n", `invalid group path "Europe//Berlin"`},
		{"duplicate group", "users:\n  - email: a@example.com\n    inventoryAssignments: [{group: g, roles: [x]}, {group: g, roles: [y]}]\n", `users[0].inventoryAssignments[1]: duplicate group "g"`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}
}

func TestSchemaRejectsInvalidInventoryRoles(t *testing.T) {
	schema := compileSchema(t)
	for _, content := range []string{
		"inventoryRoles:\n  - name: a\n    permissions: [{scope: DEVICE, permission: READ}]\n",
		"inventoryRoles:\n  - name: a\n    permissions: [{scope: ALARM}]\n",
		"users:\n  - email: a@example.com\n    inventoryAssignments: [{group: g}]\n",
	} {
		assert.Error(t, validateYAML(t, schema, []byte(content)), content)
	}
}

func TestInventoryRolesSectionOrder(t *testing.T) {
	index := func(section string) int {
		for i, name := range SectionOrder {
			if name == section {
				return i
			}
		}
		return -1
	}
	assert.Equal(t, index(SectionUserGroups)+1, index(SectionInventoryRoles))
	assert.Equal(t, index(SectionInventoryRoles)+1, index(SectionUsers))
}

func TestSyncInventoryRolesDryRun(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	s.DryRun = true

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec, {Name: "Empty"}}))
	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username: "berlin-ops@example.com",
		InventoryAssignments: []InventoryAssignmentSpec{
			{Group: "Plant Berlin", Roles: []string{"Site operator"}},
			{Group: "Europe/Plant Paris", Roles: []string{"Site operator", "Reader"}},
		},
	}}))

	require.Len(t, s.Results, 3)
	assert.Equal(t, ActionPlanned, s.Results[0].Action)
	assert.Equal(t, "ensure inventory role with permissions: ALARM:*:ADMIN, MANAGED_OBJECT:*:READ, MEASUREMENT:*:READ, OPERATION:*:ADMIN", s.Results[0].Detail)
	assert.Equal(t, "ensure inventory role", s.Results[1].Detail)
	assert.Equal(t, "ensure user; inventory roles: Site operator on Plant Berlin; Site operator, Reader on Europe/Plant Paris", s.Results[2].Detail)

	// The client may log in when it is created; nothing else is requested
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		assert.Equal(t, "/tenant/oauth/token", r.Path, "dry-run makes no API calls")
	}
}

func TestSyncInventoryRoleCreate(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionCreated, r.Action)
	created := f.Requests(http.MethodPost, "/user/inventoryroles")
	require.Len(t, created, 1)
	assert.Equal(t, "Site operator", created[0].Body["name"])
	assert.Equal(t, "Alarms and operations for one site", created[0].Body["description"])
	assert.Equal(t, []any{
		map[string]any{"scope": "ALARM", "type": "*", "permission": "ADMIN"},
		map[string]any{"scope": "MANAGED_OBJECT", "type": "*", "permission": "READ"},
		map[string]any{"scope": "MEASUREMENT", "type": "*", "permission": "READ"},
		map[string]any{"scope": "OPERATION", "type": "*", "permission": "ADMIN"},
	}, created[0].Body["permissions"])
}

func TestSyncInventoryRoleUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	// Same set in a different order, with permission IDs
	fakeInventoryRoles(f, siteOperatorRole(
		map[string]any{"id": 4, "scope": "OPERATION", "type": "*", "permission": "ADMIN"},
		map[string]any{"id": 1, "scope": "MANAGED_OBJECT", "type": "*", "permission": "READ"},
		map[string]any{"id": 2, "scope": "ALARM", "type": "*", "permission": "ADMIN"},
		map[string]any{"id": 3, "scope": "MEASUREMENT", "type": "*", "permission": "READ"},
	))
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, f.Requests(http.MethodPut, "/user/inventoryroles"))
	assert.Empty(t, f.Requests(http.MethodPost, "/user/inventoryroles"))
}

func TestSyncInventoryRoleUpdatePermissions(t *testing.T) {
	f := newFakeC8y(t)
	fakeInventoryRoles(f, siteOperatorRole(
		map[string]any{"id": 2, "scope": "ALARM", "type": "*", "permission": "READ"},
		map[string]any{"id": 9, "scope": "EVENT", "type": "*", "permission": "ADMIN"},
	))
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "updated permissions", r.Detail)
	updates := f.Requests(http.MethodPut, "/user/inventoryroles/7")
	require.Len(t, updates, 1)
	assert.NotContains(t, updates[0].Body, "description", "an unchanged description is not sent")
	assert.Len(t, updates[0].Body["permissions"], 4, "the permissions are replaced by the manifest set")
}

func TestSyncInventoryRoleUpdateDescriptionOnly(t *testing.T) {
	f := newFakeC8y(t)
	role := siteOperatorRole(map[string]any{"scope": "ALARM", "type": "*", "permission": "READ"})
	role["description"] = "old"
	fakeInventoryRoles(f, role)
	s := newFakeSyncer(t, f)

	// No permissions in the manifest: the role keeps its permissions
	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{{Name: "Site operator", Description: "new"}}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "updated description", r.Detail)
	updates := f.Requests(http.MethodPut, "/user/inventoryroles/7")
	require.Len(t, updates, 1)
	assert.NotContains(t, updates[0].Body, "permissions")
}

func TestSyncInventoryRoleAmbiguous(t *testing.T) {
	f := newFakeC8y(t)
	other := siteOperatorRole()
	other["id"] = 8
	fakeInventoryRoles(f, siteOperatorRole(), other)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncInventoryRoles(context.Background(), []InventoryRoleSpec{siteOperatorSpec}))

	r := resultFor(t, s, SectionInventoryRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, "ambiguous inventory role name")
}

// syncBerlinOps syncs the existing user with the given assignments
func syncBerlinOps(t *testing.T, f *fakeC8y, assignments ...InventoryAssignmentSpec) ChangeResult {
	t.Helper()
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{
		Username:             "berlin-ops@example.com",
		InventoryAssignments: assignments,
	}}))
	return resultFor(t, s, SectionUsers)
}

func TestInventoryAssignmentCreate(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole(), map[string]any{"id": 8, "name": "Reader"})
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 30, "managedObject": "999", "roles": []map[string]any{{"id": 7}}})

	r := syncBerlinOps(t, f, InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Site operator", "Reader"}})

	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: Site operator, Reader", r.Detail)
	created := f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory")
	require.Len(t, created, 1)
	assert.Equal(t, "100", created[0].Body["managedObject"])
	assert.Equal(t, []any{map[string]any{"id": float64(7)}, map[string]any{"id": float64(8)}}, created[0].Body["roles"])
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory"), "the assignment of another group is not touched")

	query := f.Requests(http.MethodGet, "/inventory/managedObjects")
	require.Len(t, query, 1)
	assert.Contains(t, query[0].Query, "has%28c8y_IsDeviceGroup%29")
}

func TestInventoryAssignmentAddsMissingRole(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole(), map[string]any{"id": 8, "name": "Reader"})
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	// The user already has Reader and an unmanaged role 12 on the group
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 8}, {"id": 12}}})

	r := syncBerlinOps(t, f, InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Reader", "Site operator"}})

	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "inventory roles on Plant Berlin: Site operator", r.Detail)
	updates := f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory/31")
	require.Len(t, updates, 1)
	assert.Equal(t, []any{
		map[string]any{"id": float64(8)},
		map[string]any{"id": float64(12)},
		map[string]any{"id": float64(7)},
	}, updates[0].Body["roles"], "existing roles are kept")
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory"))
}

func TestInventoryAssignmentUnchanged(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f, map[string]any{"id": 31, "managedObject": "100", "roles": []map[string]any{{"id": 7}}})

	r := syncBerlinOps(t, f, InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Site operator"}})

	require.NoError(t, r.Err)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1/roles/inventory"))
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory"))
}

func TestInventoryAssignmentForNewUser(t *testing.T) {
	f := fakeUsers(t)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f)

	r := syncBerlinOps(t, f, InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Site operator"}})

	require.NoError(t, r.Err)
	assert.Equal(t, ActionCreated, r.Action)
	assert.Len(t, f.Requests(http.MethodPost, "/user/t123/users/new/roles/inventory"), 1)
	assert.Empty(t, f.Requests(http.MethodGet, "/user/t123/users/new/roles/inventory"), "a new user has no assignments")
}

func TestInventoryAssignmentGroupPath(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f,
		fakeGroup{ID: "1", Name: "Europe"},
		fakeGroup{ID: "2", Name: "America"},
		fakeGroup{ID: "100", Name: "Plant Berlin", Parent: "1"},
		fakeGroup{ID: "200", Name: "Plant Berlin", Parent: "2"},
	)
	fakeAssignments(f)

	r := syncBerlinOps(t, f, InventoryAssignmentSpec{Group: "Europe/Plant Berlin", Roles: []string{"Site operator"}})

	require.NoError(t, r.Err)
	created := f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory")
	require.Len(t, created, 1)
	assert.Equal(t, "100", created[0].Body["managedObject"])
}

func TestInventoryAssignmentGroupNameWithSlash(t *testing.T) {
	f := fakeUsers(t, existingUser)
	fakeInventoryRoles(f, siteOperatorRole())
	fakeDeviceGroups(f, fakeGroup{ID: "300", Name: "Line 1/2"})
	fakeAssignments(f)

	r := syncBerlinOps(t, f, InventoryAssignmentSpec{Group: "Line 1/2", Roles: []string{"Site operator"}})

	require.NoError(t, r.Err)
	created := f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory")
	require.Len(t, created, 1)
	assert.Equal(t, "300", created[0].Body["managedObject"], "an exact name match wins over a path")
}

func TestInventoryAssignmentErrors(t *testing.T) {
	testCases := []struct {
		name     string
		groups   []fakeGroup
		spec     InventoryAssignmentSpec
		expected string
	}{
		{
			name:     "group missing",
			spec:     InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Site operator"}},
			expected: `device group "Plant Berlin": not found`,
		},
		{
			name:     "group ambiguous",
			groups:   []fakeGroup{{ID: "100", Name: "Plant Berlin"}, {ID: "200", Name: "Plant Berlin"}},
			spec:     InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Site operator"}},
			expected: `device group "Plant Berlin": ambiguous: 2 groups match (IDs 100, 200)`,
		},
		{
			name:     "group path missing",
			groups:   []fakeGroup{{ID: "1", Name: "Europe"}, {ID: "200", Name: "Plant Berlin", Parent: "2"}},
			spec:     InventoryAssignmentSpec{Group: "Europe/Plant Berlin", Roles: []string{"Site operator"}},
			expected: `device group "Europe/Plant Berlin": not found`,
		},
		{
			name:     "role missing",
			groups:   []fakeGroup{{ID: "100", Name: "Plant Berlin"}},
			spec:     InventoryAssignmentSpec{Group: "Plant Berlin", Roles: []string{"Unknown role"}},
			expected: `inventory role "Unknown role": not found`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeUsers(t, existingUser)
			fakeInventoryRoles(f, siteOperatorRole())
			fakeDeviceGroups(f, tc.groups...)
			fakeAssignments(f)

			r := syncBerlinOps(t, f, tc.spec)

			assert.Equal(t, ActionFailed, r.Action)
			assert.ErrorContains(t, r.Err, tc.expected)
			assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory"))
		})
	}
}

// TestRunInventoryRolesAndAssignments applies a manifest end to end: the role
// created in the inventoryRoles section is assigned in the users section
func TestRunInventoryRolesAndAssignments(t *testing.T) {
	f := fakeUsers(t, existingUser)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	roles := []map[string]any{}
	f.Handle("GET /user/inventoryroles", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"roles": roles, "statistics": stats()})
	})
	f.Handle("POST /user/inventoryroles", func(w http.ResponseWriter, r *http.Request) {
		roles = append(roles, map[string]any{"id": 7, "name": "Site operator"})
		writeJSON(w, http.StatusCreated, roles[0])
	})
	fakeDeviceGroups(f, fakeGroup{ID: "100", Name: "Plant Berlin"})
	fakeAssignments(f)
	setupRunAgainst(t, f)

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`
inventoryRoles:
  - name: Site operator
    permissions:
      - {scope: ALARM, permission: ADMIN}
users:
  - email: berlin-ops@example.com
    inventoryAssignments:
      - group: Plant Berlin
        roles: [Site operator]
`), 0o644))

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, stdout)
	assert.Contains(t, stdout, "[inventoryRoles] Site operator (created)")
	assert.Contains(t, stdout, "[users] berlin-ops@example.com (updated): inventory roles on Plant Berlin: Site operator")
	created := f.Requests(http.MethodPost, "/user/t123/users/u1/roles/inventory")
	require.Len(t, created, 1)
	assert.Equal(t, []any{map[string]any{"id": float64(7)}}, created[0].Body["roles"])
}
