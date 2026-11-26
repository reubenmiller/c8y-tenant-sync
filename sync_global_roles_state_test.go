package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGlobalRole serves a global role (user group) by name and answers its
// delete
func fakeGlobalRole(f *fakeC8y, id, name string) {
	f.JSON("GET /user/t123/groupByName/"+name, http.StatusOK, map[string]any{"id": id, "name": name, "self": "http://example/user/t123/groups/" + id})
	f.Handle("DELETE /user/t123/groups/"+id, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

// fakeSyncUserGroups serves the user running the sync and its global roles
func fakeSyncUserGroups(f *fakeC8y, userName string, groups ...map[string]any) {
	fakeCurrentUser(f, userName)
	references := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		references = append(references, map[string]any{"group": group})
	}
	f.JSON("GET /user/t123/users/"+userName+"/groups", http.StatusOK, map[string]any{"references": references, "statistics": stats()})
}

func TestValidateGlobalRoleState(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"unknown state", "globalRoles:\n  - name: a\n    state: deleted\n", `globalRoles[0]: unknown state "deleted": supported values are present, absent`},
		{"absent built-in", "globalRoles:\n  - name: Admins\n    state: absent\n", `globalRoles[0]: "Admins" is a built-in global role and can not have state absent`},
		{"user references absent", "globalRoles:\n  - name: a\n    state: absent\nusers:\n  - email: u@example.com\n    globalRoles: [a]\n", `users[0].globalRoles[0]: global role "a" has state absent in globalRoles`},
		{"sso rule references absent", `globalRoles:
  - name: a
    state: absent
loginOptions:
  sso:
    template: CUSTOM
    accessMappings:
      rules:
        - when: [{claim: groups, value: x}]
          globalRoles: [a]
`, `loginOptions.sso.accessMappings.rules[0].globalRoles[0]: global role "a" has state absent in globalRoles`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}

	_, err := LoadManifest(writeManifest(t, "globalRoles:\n  - name: a\n    state: present\n    permissions: [ROLE_INVENTORY_READ]\n  - name: b\n    state: absent\n"))
	assert.NoError(t, err)

	// An absent global role may keep its description and permissions (they
	// are ignored), and an absent user may still refer to it
	_, err = LoadManifest(writeManifest(t, "globalRoles:\n  - name: a\n    state: absent\n    description: d\n    permissions: [ROLE_INVENTORY_READ]\nusers:\n  - email: u@example.com\n    state: absent\n    globalRoles: [a]\n"))
	assert.NoError(t, err)
}

func TestSchemaGlobalRoleState(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("globalRoles:\n  - name: a\n    state: absent\n")))
	assert.Error(t, validateYAML(t, schema, []byte("globalRoles:\n  - name: a\n    state: deleted\n")))
}

func TestSyncGlobalRoleAbsentDeletedAfterUsers(t *testing.T) {
	f := fakeUsers(t, contractorUser)
	fakeDeleteUser(f, "contractor@example.com", http.StatusNoContent)
	fakeSyncUserGroups(f, "tenant-sync", map[string]any{"id": "2", "name": "admins"})
	fakeGlobalRole(f, "42", "Site staff")
	f.Handle("DELETE /user/t123/groups/42", func(w http.ResponseWriter, r *http.Request) {
		assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/users/"), 1, "the users are deleted before the global role")
		w.WriteHeader(http.StatusNoContent)
	})
	s := newFakeSyncer(t, f)

	manifest := &Manifest{
		GlobalRoles: []GlobalRoleSpec{{Name: "Site staff", State: GlobalRoleStateAbsent}},
		Users:       []UserSpec{{Username: "contractor@example.com", State: UserStateAbsent}},
	}
	require.NoError(t, s.applyTarget(context.Background(), context.Background(), manifest, nil))

	r := resultFor(t, s, SectionGlobalRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionDeleted, r.Action)
	assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/groups/42"), 1)
}

func TestSyncGlobalRoleAbsentMissing(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncGlobalRoles(context.Background(), []GlobalRoleSpec{{Name: "Site staff", State: GlobalRoleStateAbsent}}))
	s.runDeferredDeletes(context.Background())

	r := resultFor(t, s, SectionGlobalRoles)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Equal(t, "absent", r.Detail)
	assert.Empty(t, f.Requests(http.MethodGet, "/user/currentUser"), "the sync user is only needed for an existing role")
}

func TestSyncGlobalRoleAbsentOfSyncUser(t *testing.T) {
	f := newFakeC8y(t)
	fakeGlobalRole(f, "42", "Site staff")
	fakeSyncUserGroups(f, "tenant-sync", map[string]any{"id": "42", "name": "Site staff"})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncGlobalRoles(context.Background(), []GlobalRoleSpec{{Name: "Site staff", State: GlobalRoleStateAbsent}}))
	s.runDeferredDeletes(context.Background())

	r := resultFor(t, s, SectionGlobalRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.EqualError(t, r.Err, `refusing to delete a global role of the user running the sync: tenant-sync belongs to "Site staff"; remove that user from the role first or run the sync as another user`)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/groups/42"))
}

func TestSyncGlobalRoleAbsentBuiltin(t *testing.T) {
	// Validation already refuses this; the sync refuses it as well
	f := newFakeC8y(t)
	fakeGlobalRole(f, "2", "admins")
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncGlobalRoles(context.Background(), []GlobalRoleSpec{{Name: "admins", State: GlobalRoleStateAbsent}}))
	s.runDeferredDeletes(context.Background())

	r := resultFor(t, s, SectionGlobalRoles)
	assert.Equal(t, ActionFailed, r.Action)
	assert.EqualError(t, r.Err, `refusing to delete the built-in global role "admins"`)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/groups/2"))
}

func TestSyncGlobalRoleAbsentOnlyGlobalRoles(t *testing.T) {
	// With --only globalRoles the delete runs right after the section
	f := newFakeC8y(t)
	fakeGlobalRole(f, "42", "Site staff")
	fakeSyncUserGroups(f, "tenant-sync")
	s := newFakeSyncer(t, f)

	manifest := &Manifest{GlobalRoles: []GlobalRoleSpec{{Name: "Site staff", State: GlobalRoleStateAbsent}}}
	require.NoError(t, s.applyTarget(context.Background(), context.Background(), manifest, []string{SectionGlobalRoles}))

	assert.Equal(t, ActionDeleted, resultFor(t, s, SectionGlobalRoles).Action)
}

func TestSyncGlobalRoleAbsentDryRun(t *testing.T) {
	f := newFakeC8y(t)
	fakeGlobalRole(f, "42", "Site staff")
	fakeSyncUserGroups(f, "tenant-sync")
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncGlobalRoles(context.Background(), []GlobalRoleSpec{
		{Name: "Site staff", State: GlobalRoleStateAbsent},
		{Name: "Gone", State: GlobalRoleStateAbsent},
	}))
	s.runDeferredDeletes(context.Background())

	results := resultsFor(s, SectionGlobalRoles)
	assert.Equal(t, ActionPlanned, results["Site staff"].Action)
	assert.Equal(t, "delete after the users section", results["Site staff"].Detail)
	assert.Equal(t, ActionUnchanged, results["Gone"].Action)
	assert.Equal(t, "absent", results["Gone"].Detail)
	assertOnlyReads(t, f)

	// Without a session the dry run can not read the tenant
	offline := &Syncer{DryRun: true}
	require.NoError(t, offline.SyncGlobalRoles(context.Background(), []GlobalRoleSpec{{Name: "Site staff", State: GlobalRoleStateAbsent}}))
	r := resultFor(t, offline, SectionGlobalRoles)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "delete if it exists, after the users section", r.Detail)
}
