package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUserTenant is a tenant with device groups, inventory roles and users:
//
//   - device groups Europe > Plant A, Europe > Berlin and Asia > Plant A (so
//     "Plant A" is not unique)
//   - berlin-ops (u1) has the global roles operators and business, Site
//     operator on Europe/Plant A plus a second assignment there with Reader,
//     Reader on Berlin, Reader on a device (not a group) and an unknown role
//     on Asia/Plant A
//   - jdoe (u2) is disabled, without email and roles
//   - service_smartrule (s1) is a microservice service user
func fakeUserTenant(t *testing.T) *fakeInventory {
	inv := newFakeInventory(t)
	inv.group("101", "Europe", "c8y_DeviceGroup", "", nil)
	inv.group("102", "Plant A", "c8y_DeviceSubgroup", "101", nil)
	inv.group("103", "Asia", "c8y_DeviceGroup", "", nil)
	inv.group("104", "Plant A", "c8y_DeviceSubgroup", "103", nil)
	inv.group("105", "Berlin", "c8y_DeviceSubgroup", "101", nil)

	f := inv.f
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	f.JSON("GET /user/inventoryroles", http.StatusOK, map[string]any{"roles": []map[string]any{
		{"id": 7, "name": "Site operator", "description": "Alarms and operations for one site", "permissions": []map[string]any{
			{"id": 1, "scope": "ALARM", "type": "*", "permission": "ADMIN"},
		}},
		{"id": 8, "name": "Reader", "permissions": []map[string]any{
			{"id": 2, "scope": "*", "type": "*", "permission": "READ"},
		}},
	}, "statistics": stats()})

	berlinOps := map[string]any{
		"id": "u1", "userName": "berlin-ops@example.com", "email": "berlin-ops@example.com",
		"firstName": "Berlin", "lastName": "Ops", "phone": "+49 30 1234", "enabled": true,
		"self": "http://example/user/t123/users/u1",
	}
	jdoe := map[string]any{"id": "u2", "userName": "jdoe", "enabled": false, "self": "http://example/user/t123/users/u2"}
	service := map[string]any{"id": "s1", "userName": "service_smartrule", "enabled": true, "self": "http://example/user/t123/users/s1"}
	f.JSON("GET /user/t123/users", http.StatusOK, map[string]any{
		"users": []map[string]any{jdoe, service, berlinOps}, "statistics": stats(),
	})
	for _, user := range []map[string]any{berlinOps, jdoe, service} {
		f.JSON("GET /user/t123/userByName/"+user["userName"].(string), http.StatusOK, user)
	}

	f.JSON("GET /user/t123/users/u1/groups", http.StatusOK, map[string]any{
		"references": []map[string]any{
			{"group": map[string]any{"id": 2, "name": "operators"}},
			{"group": map[string]any{"id": 1, "name": "business"}},
		},
		"statistics": stats(),
	})
	f.JSON("GET /user/t123/users/u2/groups", http.StatusOK, map[string]any{"references": []any{}, "statistics": stats()})
	f.JSON("GET /user/t123/users/u1/roles/inventory", http.StatusOK, map[string]any{"inventoryAssignments": []map[string]any{
		{"id": 31, "managedObject": "102", "roles": []map[string]any{{"id": 7}}},
		{"id": 32, "managedObject": "102", "roles": []map[string]any{{"id": 8}, {"id": 7}}},
		{"id": 33, "managedObject": "105", "roles": []map[string]any{{"id": 8}}},
		{"id": 34, "managedObject": "900", "roles": []map[string]any{{"id": 8}}},
		{"id": 35, "managedObject": "104", "roles": []map[string]any{{"id": 99}}},
	}, "statistics": stats()})
	f.JSON("GET /user/t123/users/u2/roles/inventory", http.StatusOK, map[string]any{"inventoryAssignments": []any{}, "statistics": stats()})
	return inv
}

func TestExportUsers(t *testing.T) {
	inv := fakeUserTenant(t)
	exporter := &Exporter{Client: inv.f.Client(), Sections: []string{SectionUsers}}

	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []UserSpec{
		{
			Username:    "berlin-ops@example.com",
			IfMissing:   UserIfMissingSkip,
			Email:       "berlin-ops@example.com",
			FirstName:   "Berlin",
			LastName:    "Ops",
			GlobalRoles: []string{"business", "operators"},
			InventoryRoles: []InventoryAssignmentSpec{
				{Group: "Berlin", Roles: []string{"Reader"}},
				// The name is not unique: the path is written, and both
				// assignments on the group are merged
				{Group: "Europe/Plant A", Roles: []string{"Reader", "Site operator"}},
			},
		},
		{Username: "jdoe", IfMissing: UserIfMissingSkip, Enabled: boolPtr(false)},
	}, manifest.Users, "service users, the phone, devices and unknown roles are not exported")
	assert.Empty(t, manifest.InventoryRoles, "only the selected section is exported")
	assert.Empty(t, manifest.DeviceGroups)
}

func TestExportUsersIsOptIn(t *testing.T) {
	f := fakeExportTenant(t)
	exporter := &Exporter{Client: f.Client()}

	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)
	assert.Empty(t, manifest.Users)
	assert.Empty(t, f.Requests(http.MethodGet, "/user/t123/users"), "a plain export does not read users")

	assert.NotContains(t, defaultExportSections(), SectionUsers)
	assert.Contains(t, ExportSections, SectionUsers, "users can be named in --only")
}

func TestExportUsersFailsOnAPIError(t *testing.T) {
	inv := fakeUserTenant(t)
	inv.f.JSON("GET /user/t123/users/u1/groups", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})

	exporter := &Exporter{Client: inv.f.Client(), Sections: []string{SectionUsers}}
	_, _, err := exporter.Export(context.Background())
	assert.ErrorContains(t, err, "failed to export users")
	assert.ErrorContains(t, err, `user "berlin-ops@example.com": list global roles`)
}

// TestExportedUsersRoundTrip applies the exported users (and inventory
// roles) to the same tenant: nothing changes
func TestExportedUsersRoundTrip(t *testing.T) {
	inv := fakeUserTenant(t)
	exporter := &Exporter{Client: inv.f.Client(), Sections: []string{SectionInventoryRoles, SectionUsers}}
	exported, _, err := exporter.Export(context.Background())
	require.NoError(t, err)

	content, err := MarshalManifest(exported, "Exported for a test")
	require.NoError(t, err)
	text := string(content)
	assert.Contains(t, text, "ifMissing: skip")
	assert.Contains(t, text, "globalRoles:")
	assert.Contains(t, text, "inventoryRoles:")
	assert.NotContains(t, text, "password")
	assert.NotContains(t, text, "phone")
	assert.NotContains(t, text, "inventoryRolesMode")
	assert.NotContains(t, text, "state:")
	assert.NotContains(t, text, "service_")

	path := filepath.Join(t.TempDir(), "users.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	require.NoError(t, manifest.Validate())
	assert.NoError(t, validateYAML(t, compileSchema(t), content))

	before := len(inv.f.requests)
	s := newFakeSyncer(t, inv.f)
	require.NoError(t, s.SyncInventoryRoles(context.Background(), manifest.InventoryRoles))
	require.NoError(t, s.SyncUsers(context.Background(), manifest.Users))

	require.Len(t, s.Results, 4)
	for _, r := range s.Results {
		require.NoError(t, r.Err, "%s %s", r.Section, r.Item)
		assert.Equal(t, ActionUnchanged, r.Action, "%s %s: %s", r.Section, r.Item, r.Detail)
	}
	for _, r := range inv.f.requests[before:] {
		if r.Path == "/tenant/oauth/token" {
			continue
		}
		assert.Equal(t, http.MethodGet, r.Method, "the sync writes %s %s", r.Method, r.Path)
	}
}

func TestExportCommandUsersOnly(t *testing.T) {
	inv := fakeUserTenant(t)
	t.Setenv("C8Y_HOST", inv.f.server.URL)
	t.Setenv("C8Y_TENANT", "t123")
	t.Setenv("C8Y_USER", "user")
	t.Setenv("C8Y_PASSWORD", "pass")
	t.Setenv("C8Y_TOKEN", "")

	output := filepath.Join(t.TempDir(), "users.yaml")
	require.NoError(t, runRoot(t, "export", "-o", output, "--only", "users"))
	loaded, err := LoadManifest(output)
	require.NoError(t, err)
	require.Len(t, loaded.Users, 2)
	assert.Equal(t, "berlin-ops@example.com", loaded.Users[0].Username)
	assert.Equal(t, UserIfMissingSkip, loaded.Users[0].IfMissing)
}

func TestDeviceGroupIndexLabel(t *testing.T) {
	index := newDeviceGroupIndex([]deviceGroupNode{
		{id: "1", name: "Europe"},
		{id: "2", name: "Asia"},
		{id: "3", name: "Site", parents: []string{"1"}},
		{id: "4", name: "Site", parents: []string{"2"}},
		{id: "5", name: "Line 1", parents: []string{"3"}},
		{id: "6", name: "Line 1", parents: []string{"4"}},
		{id: "7", name: "Unique", parents: []string{"3"}},
		// Two root groups with the same name cannot be told apart
		{id: "8", name: "Twin"},
		{id: "9", name: "Twin"},
		// A group with a parent which is not a device group is a root
		{id: "10", name: "Orphan", parents: []string{"999"}},
	})

	tests := []struct {
		id, label string
		unique    bool
	}{
		{"1", "Europe", true},
		{"3", "Europe/Site", true},
		{"4", "Asia/Site", true},
		// Site/Line 1 matches both lines: the full path is needed
		{"5", "Europe/Site/Line 1", true},
		{"6", "Asia/Site/Line 1", true},
		{"7", "Unique", true},
		{"8", "Twin", false},
		{"10", "Orphan", true},
	}
	for _, tt := range tests {
		label, unique := index.label(tt.id)
		assert.Equal(t, tt.label, label, "group %s", tt.id)
		assert.Equal(t, tt.unique, unique, "group %s", tt.id)
	}

	// The labels resolve like the users section resolves groups
	assert.Equal(t, []string{"5"}, index.resolve("Europe/Site/Line 1"))
	assert.Len(t, index.resolve("Site/Line 1"), 2)
	assert.Empty(t, index.resolve("Nowhere/Line 1"))
	assert.True(t, strings.HasPrefix(strings.Join(index.path("6"), "/"), "Asia"))
}
