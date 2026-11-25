package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsEmailAddress(t *testing.T) {
	for _, valid := range []string{"jdoe@example.com", "j.doe+ops@sub.example.co"} {
		assert.True(t, isEmailAddress(valid), valid)
	}
	for _, invalid := range []string{"", "svc-dashboard", "jdoe", "@example.com", "jdoe@", "Jane <jdoe@example.com>", "a b@example.com", "jdoe@@example.com"} {
		assert.False(t, isEmailAddress(invalid), invalid)
	}
}

func TestUserCreateEmail(t *testing.T) {
	assert.Equal(t, "jane@corp.com", UserSpec{Username: "jdoe@example.com", Email: "jane@corp.com"}.CreateEmail(), "an explicit email wins")
	assert.Equal(t, "jdoe@example.com", UserSpec{Username: "jdoe@example.com"}.CreateEmail(), "an email-like username is used")
	assert.Equal(t, "", UserSpec{Username: "svc-dashboard"}.CreateEmail(), "a plain username is not an email")
}

func TestUserNameDefaultsToEmail(t *testing.T) {
	path := writeManifest(t, `users:
  - email: jdoe@example.com
    globalRoles: [operators]
  - userName: svc-dashboard
    email: ops@example.com
  - userName: admin@example.com
    sendPasswordResetEmail: true
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, "jdoe@example.com", manifest.Users[0].Username, "the userName defaults to the email")
	assert.Equal(t, "svc-dashboard", manifest.Users[1].Username, "an explicit userName is kept")
	assert.Equal(t, "", manifest.Users[2].Email, "the email is only derived when creating a user")

	// Duplicates are detected after the defaults are applied
	path = writeManifest(t, "users:\n  - email: jdoe@example.com\n  - userName: jdoe@example.com\n")
	_, err = LoadManifest(path)
	assert.ErrorContains(t, err, `users[1]: duplicate userName "jdoe@example.com"`)

	// A password reset email needs an email address
	path = writeManifest(t, "users:\n  - userName: svc-dashboard\n    sendPasswordResetEmail: true\n")
	_, err = LoadManifest(path)
	assert.ErrorContains(t, err, "users[0]: sendPasswordResetEmail requires email (or an email address as userName)")
}

func TestUserSchemaAcceptsEmailOnly(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("users:\n  - email: jdoe@example.com\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("users:\n  - userName: svc\n")))
	assert.Error(t, validateYAML(t, schema, []byte("users:\n  - firstName: Jane\n")))
}

// fakeUsers serves a tenant (t123) in which the given users exist
func fakeUsers(t *testing.T, existing ...map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	for _, user := range existing {
		f.JSON("GET /user/t123/userByName/"+user["userName"].(string), http.StatusOK, user)
		f.JSON("PUT /user/t123/users/"+user["id"].(string), http.StatusOK, user)
	}
	f.JSON("POST /user/t123/users", http.StatusCreated, map[string]any{"id": "new", "self": f.server.URL + "/user/t123/users/new"})
	return f
}

func TestCreateUserWithUsernameAsEmail(t *testing.T) {
	f := fakeUsers(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "jdoe@example.com", FirstName: "Jane"}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionCreated, r.Action)
	created := f.Requests(http.MethodPost, "/user/t123/users")
	require.Len(t, created, 1)
	assert.Equal(t, "jdoe@example.com", created[0].Body["userName"])
	assert.Equal(t, "jdoe@example.com", created[0].Body["email"], "the email-like username is used as email")
}

func TestCreateUserWithoutEmailFails(t *testing.T) {
	f := fakeUsers(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "svc-dashboard"}}))

	r := resultFor(t, s, SectionUsers)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, "an email is required to create a user: set email, or use an email address as userName")
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users"), "the user is not created")
}

func TestExistingUserEmailIsNotReplacedByUsername(t *testing.T) {
	// An existing user without an email in the manifest: the username is not
	// written as email, so the user's real email is kept
	f := fakeUsers(t, map[string]any{
		"id": "u1", "userName": "jdoe@example.com", "email": "jane@corp.com", "enabled": true,
		"self": "http://example/user/t123/users/u1",
	})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "jdoe@example.com"}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, f.Requests(http.MethodPut, "/user/t123/users/u1"), "the email is not changed")
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users"))
}

func TestExistingUserWithoutEmailCanBeUpdated(t *testing.T) {
	// Disabling an existing user does not need an email
	f := fakeUsers(t, map[string]any{
		"id": "u2", "userName": "svc-old", "enabled": true, "self": "http://example/user/t123/users/u2",
	})
	s := newFakeSyncer(t, f)
	disabled := false

	require.NoError(t, s.SyncUsers(context.Background(), []UserSpec{{Username: "svc-old", Enabled: &disabled}}))

	r := resultFor(t, s, SectionUsers)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionUpdated, r.Action)
	puts := f.Requests(http.MethodPut, "/user/t123/users/u2")
	require.Len(t, puts, 1)
	assert.Equal(t, false, puts[0].Body["enabled"])
	assert.NotContains(t, puts[0].Body, "email")
}
