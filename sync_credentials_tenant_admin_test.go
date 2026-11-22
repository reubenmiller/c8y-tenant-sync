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

const tenantAdminManifest = childTenantRunManifest + `
targets:
  tenants: [` + customerDomain + `]
  credentials:
    mode: tenantAdmin

tenantOptions:
  - category: onboarding
    key: crm
    value: CRM-1234
`

// fakeTenantAdminLogin accepts the admin login of t456 with the password
// returned by password(), and records the credentials of tenant options
func fakeTenantAdminLogin(t *testing.T, f *fakeC8y, password func() string) *[]string {
	f.Handle("GET /user/currentUser", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok && strings.TrimPrefix(user, "t456/") == "admin" && pass == password() {
			writeJSON(w, http.StatusOK, map[string]any{"userName": "admin"})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "security/Unauthorized"})
	})
	var optionUsers []string
	f.Handle("POST /tenant/options", func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		optionUsers = append(optionUsers, user+":"+pass)
		writeJSON(w, http.StatusCreated, map[string]any{})
	})
	return &optionUsers
}

func noTenantAdminRetryDelay(t *testing.T) {
	previous := tenantAdminRetryDelay
	tenantAdminRetryDelay = 0
	t.Cleanup(func() { tenantAdminRetryDelay = previous })
}

// TestRunTenantAdminCreatesAndConfiguresTenant creates a tenant with a
// generated admin password and configures it with that password, then runs
// again (e.g. a later CI job) and reuses the password from the secrets file
func TestRunTenantAdminCreatesAndConfiguresTenant(t *testing.T) {
	noTenantAdminRetryDelay(t)
	f, store := fakeChildTenants(t, nil)
	dir := t.TempDir()
	secretsFile := filepath.Join(dir, "tenant.secrets.yaml")
	// The admin password is the one the tenant was created with
	optionUsers := fakeTenantAdminLogin(t, f, func() string {
		created := f.Requests(http.MethodPost, "/tenant/tenants")
		if len(created) == 0 {
			return ""
		}
		password, _ := created[0].Body["adminPass"].(string)
		return password
	})
	setupRunAgainst(t, f)
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(tenantAdminManifest), 0o644))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)
	password := readSecretsFile(t, secretsFile)["customer-a-admin"].Value
	require.NotEmpty(t, password)

	assert.Contains(t, stdout, "(created): created: t456")
	assert.Contains(t, stdout, "[tenantOptions] t456: onboarding/crm (created)")
	require.Len(t, *optionUsers, 1)
	assert.Equal(t, "t456/admin:"+password, (*optionUsers)[0],
		"the option is set with the generated admin credentials of the new tenant")
	assert.NotContains(t, stdout+stderr, password, "the password is masked")
	assert.Empty(t, f.Requests(http.MethodGet, "/application/applications"), "no placeholder microservice")

	// Second run: the tenant exists and the password comes from the secrets file
	require.Len(t, store.tenants, 1)
	stdout, stderr = captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "[tenants] "+customerDomain+" (unchanged)")
	require.Len(t, *optionUsers, 2)
	assert.Equal(t, (*optionUsers)[0], (*optionUsers)[1], "the same stored credentials are used again")
	assert.Equal(t, password, readSecretsFile(t, secretsFile)["customer-a-admin"].Value, "nothing is regenerated")
}

func TestRunTenantAdminFailsWhenSecretsFileWasLost(t *testing.T) {
	noTenantAdminRetryDelay(t)
	// The tenant exists, but the secrets file with its password is missing
	f, _ := fakeChildTenants(t, nil, map[string]any{"id": "t456", "domain": customerDomain, "company": "Customer A"})
	fakeTenantAdminLogin(t, f, func() string { return "the-real-password" })
	setupRunAgainst(t, f)
	manifest := writeManifest(t, tenantAdminManifest)

	var runErr error
	captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "the admin password ${generate:customer-a-admin} was generated in this run but the tenant already existed")
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/options"), "nothing is applied to the tenant")
}

func TestTenantAdminCredentials(t *testing.T) {
	noTenantAdminRetryDelay(t)
	f := newFakeC8y(t)
	fakeTenantAdminLogin(t, f, func() string { return "Admin-Pass-1" })
	s := newFakeSyncer(t, f)
	s.tenantSpecs = []ChildTenantSpec{
		{Domain: "a.example.invalid", ID: "t456", AdminName: "admin", AdminPass: "Admin-Pass-1"},
		{Domain: "b.example.invalid", AdminName: "admin", AdminPass: "changed-since"},
		{Domain: "c.example.invalid", AdminEmail: "admin@c.example.invalid", SendPasswordResetEmail: true},
		{Domain: "d.example.invalid", AdminPass: "x"},
	}

	// matched by id
	targets := []Target{{TenantID: "t456", Domain: "other.example.invalid"}}
	require.NoError(t, s.resolveTenantAdminCredentials(context.Background(), targets))
	require.NotNil(t, targets[0].Auth)
	assert.Equal(t, "t456", targets[0].Auth.Tenant)
	assert.Equal(t, "admin", targets[0].Auth.Username)

	targets = []Target{
		{TenantID: "t500", Domain: "B.example.invalid"}, // matched by domain, password rejected
		{TenantID: "t600", Domain: "c.example.invalid"},
		{TenantID: "t700", Domain: "d.example.invalid"},
		{TenantID: "t800", Domain: "unknown.example.invalid"},
		{Current: true},
	}
	err := s.resolveTenantAdminCredentials(context.Background(), targets)
	require.Error(t, err)
	for _, msg := range []string{
		"t500: the admin credentials (admin) were rejected",
		"sendPasswordResetEmail), so it is not known",
		"the tenants entry has no adminName",
		"no entry in the tenants section matches this tenant",
	} {
		assert.Contains(t, err.Error(), msg)
	}
	assert.Nil(t, targets[4].Auth, "the current tenant keeps the base credentials")
}

func TestTenantAdminCredentialsModeIsValid(t *testing.T) {
	_, err := LoadManifest(writeManifest(t, "targets:\n  allChildren: true\n  credentials:\n    mode: tenantAdmin\n"))
	assert.NoError(t, err)
	_, err = LoadManifest(writeManifest(t, "targets:\n  allChildren: true\n  credentials:\n    mode: admin\n"))
	assert.ErrorContains(t, err, "supported modes are serviceUser, sessions and tenantAdmin")
}
