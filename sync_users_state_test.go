package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCurrentUser serves the user running the sync
func fakeCurrentUser(f *fakeC8y, userName string) {
	f.JSON("GET /user/currentUser", http.StatusOK, map[string]any{"id": userName, "userName": userName})
}

// fakeDeleteUser answers the delete of a user with the given status (204 on
// success)
func fakeDeleteUser(f *fakeC8y, id string, status int) {
	f.Handle("DELETE /user/t123/users/"+id, func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, map[string]any{"error": "security/Forbidden", "message": "not allowed"})
	})
}

var contractorUser = map[string]any{
	"id": "contractor@example.com", "userName": "contractor@example.com", "email": "contractor@example.com",
	"enabled": true, "self": "http://example/user/t123/users/contractor@example.com",
}

func TestLoadManifestUserStateAndIfMissing(t *testing.T) {
	manifest, err := LoadManifest(writeManifest(t, `
users:
  - userName: new-hire@example.com
    sendPasswordResetEmail: true
  - userName: jdoe@example.com
    ifMissing: skip
    inventoryRoles:
      - group: Plant Berlin
        roles: [Site operator]
  - userName: svc-dashboard
    ifMissing: fail
    sendPasswordResetEmail: true
    globalRoles: [dashboard-readers]
  - userName: contractor@example.com
    state: absent
`))
	require.NoError(t, err)
	require.Len(t, manifest.Users, 4)
	assert.Equal(t, UserIfMissingCreate, manifest.Users[0].MissingAction())
	assert.True(t, manifest.Users[0].CreatesMissing())
	assert.Equal(t, UserIfMissingSkip, manifest.Users[1].MissingAction())
	assert.False(t, manifest.Users[1].CreatesMissing())
	assert.Equal(t, UserIfMissingFail, manifest.Users[2].MissingAction())
	assert.True(t, manifest.Users[3].Absent())
	assert.False(t, manifest.Users[3].CreatesMissing())
}

func TestValidateUserStateAndIfMissing(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"unknown state", "users:\n  - userName: a\n    state: deleted\n", `users[0]: unknown state "deleted": supported values are present, absent`},
		{"unknown ifMissing", "users:\n  - userName: a\n    ifMissing: ignore\n", `users[0]: unknown ifMissing "ignore": supported values are create, skip, fail`},
		{"absent with invalid ifMissing", "users:\n  - userName: a\n    state: absent\n    ifMissing: ignore\n", `users[0]: unknown ifMissing "ignore"`},
		{"create needs email for reset", "users:\n  - userName: svc\n    ifMissing: create\n    sendPasswordResetEmail: true\n", "users[0]: sendPasswordResetEmail requires email"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			assert.ErrorContains(t, err, tc.expected)
		})
	}

	_, err := LoadManifest(writeManifest(t, "users:\n  - userName: svc\n    ifMissing: skip\n    sendPasswordResetEmail: true\n  - userName: svc2\n    ifMissing: fail\n  - userName: old\n    state: absent\n  - userName: kept\n    state: present\n    ifMissing: create\n    email: kept@example.com\n"))
	assert.NoError(t, err, "skip and fail do not need an email, and absent with only userName is valid")

	// An absent user may keep its other fields (they are ignored), so
	// switching between the states is a one-line change
	manifest, err := LoadManifest(writeManifest(t, "users:\n  - userName: a\n    state: absent\n    ifMissing: skip\n    email: a@example.com\n    firstName: A\n    enabled: false\n    password: secret\n    sendPasswordResetEmail: true\n    globalRoles: [ops]\n    inventoryRoles: [{group: g, roles: [r]}]\n    inventoryRolesMode: exact\n  - email: b@example.com\n    state: absent\n"))
	require.NoError(t, err)
	assert.True(t, manifest.Users[0].Absent())
	assert.Equal(t, "b@example.com", manifest.Users[1].Username, "the email is the user name")
	assert.Empty(t, manifest.UserWarnings(), "exact mode without roles is no warning on an absent user")
}

func TestAbsentUserPasswordFromIsNotResolved(t *testing.T) {
	t.Setenv("TENANT_SYNC_TEST_UNSET_PASSWORD", "")
	manifest, err := LoadManifest(writeManifest(t, "users:\n  - userName: a\n    state: absent\n    passwordFrom: {env: TENANT_SYNC_TEST_UNSET_PASSWORD}\n"))
	require.NoError(t, err)
	_, err = manifest.ResolveSecretRefs(nil)
	assert.NoError(t, err, "the variable of an absent user is not needed")
	assert.Empty(t, manifest.Users[0].Password)
}

func TestSchemaUserStateAndIfMissing(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("users:\n  - userName: a\n    ifMissing: skip\n  - userName: b\n    state: absent\n")))
	assert.Error(t, validateYAML(t, schema, []byte("users:\n  - userName: a\n    state: deleted\n")))
	assert.Error(t, validateYAML(t, schema, []byte("users:\n  - userName: a\n    ifMissing: ignore\n")))
}

func TestSyncUserIfMissingSkip(t *testing.T) {
	f := fakeUsers(t, existingUser)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{
		{Username: "jdoe", IfMissing: UserIfMissingSkip, GlobalRoles: []string{"operators"}},
		{Username: "berlin-ops@example.com", IfMissing: UserIfMissingSkip, FirstName: "Berlin"},
	}))

	results := resultsFor(s, SectionUsers)
	require.Len(t, results, 2)
	assert.Equal(t, ActionSkipped, results["jdoe"].Action)
	assert.Equal(t, "user does not exist (ifMissing: skip)", results["jdoe"].Detail)
	assert.NoError(t, results["jdoe"].Err)

	assert.Equal(t, ActionUpdated, results["berlin-ops@example.com"].Action, "an existing user is updated")
	assert.Equal(t, "updated firstName", results["berlin-ops@example.com"].Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users"), "no user is created")
}

func TestSyncUserIfMissingFail(t *testing.T) {
	f := fakeUsers(t, existingUser)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{
		{Username: "svc-dashboard", IfMissing: UserIfMissingFail},
		{Username: "berlin-ops@example.com", IfMissing: UserIfMissingFail},
	}))

	results := resultsFor(s, SectionUsers)
	assert.Equal(t, ActionFailed, results["svc-dashboard"].Action)
	assert.Equal(t, "find user", results["svc-dashboard"].Detail)
	assert.ErrorContains(t, results["svc-dashboard"].Err, "the user does not exist (ifMissing: fail)")
	assert.Equal(t, ActionUnchanged, results["berlin-ops@example.com"].Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users"))
}

func TestSyncUserReadErrorIsNotMissing(t *testing.T) {
	// A user that can't be read is not created (or skipped)
	f := fakeUsers(t)
	f.JSON("GET /user/t123/userByName/jdoe@example.com", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "jdoe@example.com"}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "read user", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users"))
}

func TestSyncUserAbsentDeletes(t *testing.T) {
	f := fakeUsers(t, contractorUser)
	fakeCurrentUser(f, "tenant-sync")
	fakeDeleteUser(f, "contractor@example.com", http.StatusNoContent)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{
		{Username: "contractor@example.com", State: UserStateAbsent},
		{Username: "gone@example.com", State: UserStateAbsent},
	}))

	results := resultsFor(s, SectionUsers)
	assert.Equal(t, ActionDeleted, results["contractor@example.com"].Action)
	assert.NoError(t, results["contractor@example.com"].Err)
	assert.Len(t, f.Requests(http.MethodDelete, "/user/t123/users/contractor@example.com"), 1)

	assert.Equal(t, ActionUnchanged, results["gone@example.com"].Action, "a missing user is already absent")
	assert.Equal(t, "absent", results["gone@example.com"].Detail)
	assert.Len(t, f.Requests(http.MethodGet, "/user/currentUser"), 1, "the user running the sync is looked up once")
}

func TestSyncUserAbsentDeleteRefused(t *testing.T) {
	f := fakeUsers(t, contractorUser)
	fakeCurrentUser(f, "tenant-sync")
	fakeDeleteUser(f, "contractor@example.com", http.StatusForbidden)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "contractor@example.com", State: UserStateAbsent}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "delete user", r.Detail)
	assert.ErrorContains(t, r.Err, "not allowed")
}

func TestSyncUserAbsentNeverDeletesSyncUser(t *testing.T) {
	f := fakeUsers(t, map[string]any{
		"id": "tenant-sync", "userName": "tenant-sync", "enabled": true, "self": "http://example/user/t123/users/tenant-sync",
	})
	fakeCurrentUser(f, "Tenant-Sync")
	fakeDeleteUser(f, "tenant-sync", http.StatusNoContent)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "tenant-sync", State: UserStateAbsent}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "delete user", r.Detail)
	assert.ErrorIs(t, r.Err, errSyncUser)
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users"), "the user running the sync is not deleted")
}

func TestSyncUserAbsentUnknownSyncUser(t *testing.T) {
	// Without knowing who runs the sync, no user is deleted
	f := fakeUsers(t, contractorUser)
	f.JSON("GET /user/currentUser", http.StatusUnauthorized, map[string]any{"error": "security/Unauthorized", "message": "no"})
	fakeDeleteUser(f, "contractor@example.com", http.StatusNoContent)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "contractor@example.com", State: UserStateAbsent}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, "look up the user running the sync")
	assert.Empty(t, f.Requests(http.MethodDelete, "/user/t123/users"))
}

func TestSyncUserStateDryRun(t *testing.T) {
	f := fakeUsers(t, existingUser, contractorUser, map[string]any{
		"id": "tenant-sync", "userName": "tenant-sync", "enabled": true, "self": "http://example/user/t123/users/tenant-sync",
	})
	fakeCurrentUser(f, "tenant-sync")
	fakeAssignments(f)
	s := newDryRunReaderSyncer(f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{
		{Username: "jdoe", IfMissing: UserIfMissingSkip},
		{Username: "svc-dashboard", IfMissing: UserIfMissingFail},
		{Username: "berlin-ops@example.com", IfMissing: UserIfMissingSkip, LastName: "Ops"},
		{Username: "contractor@example.com", State: UserStateAbsent},
		{Username: "gone@example.com", State: UserStateAbsent},
		{Username: "tenant-sync", State: UserStateAbsent},
	}))

	results := resultsFor(s, SectionUsers)
	require.Len(t, results, 6)
	assert.Equal(t, ActionSkipped, results["jdoe"].Action)
	assert.Equal(t, ActionFailed, results["svc-dashboard"].Action)
	assert.ErrorIs(t, results["svc-dashboard"].Err, errUserMissing)
	assert.Equal(t, ActionPlanned, results["berlin-ops@example.com"].Action)
	assert.Equal(t, "update lastName", results["berlin-ops@example.com"].Detail)
	assert.Equal(t, ActionPlanned, results["contractor@example.com"].Action)
	assert.Equal(t, "delete", results["contractor@example.com"].Detail)
	assert.Equal(t, ActionUnchanged, results["gone@example.com"].Action)
	assert.Equal(t, "absent", results["gone@example.com"].Detail)
	assert.Equal(t, ActionFailed, results["tenant-sync"].Action)
	assert.ErrorIs(t, results["tenant-sync"].Err, errSyncUser)
	assertOnlyReads(t, f)
}

func TestSyncUserStateDryRunWithoutSession(t *testing.T) {
	s := &Syncer{DryRun: true}

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{
		{Username: "jdoe", IfMissing: UserIfMissingSkip, GlobalRoles: []string{"operators"}},
		{Username: "svc", IfMissing: UserIfMissingFail},
		{Username: "old", State: UserStateAbsent},
	}))

	results := resultsFor(s, SectionUsers)
	assert.Equal(t, "ensure user with global roles: operators; skipped if missing", results["jdoe"].Detail)
	assert.Equal(t, "ensure user; fails if missing", results["svc"].Detail)
	assert.Equal(t, "delete if it exists", results["old"].Detail)
	for _, r := range results {
		assert.Equal(t, ActionPlanned, r.Action)
	}
}
