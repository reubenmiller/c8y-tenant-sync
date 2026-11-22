package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const customerDomain = "customer-a.example.invalid"

// fakeTenantStore serves the child tenants of t123: listing (by parent or
// domain), creating (as t456, t457, ...) and updating them
type fakeTenantStore struct {
	mu      sync.Mutex
	tenants []map[string]any
	nextID  int
}

func fakeChildTenants(t *testing.T, apps []fakeApp, existing ...map[string]any) (*fakeC8y, *fakeTenantStore) {
	f := fakeApplicationTenant(t, apps)
	store := &fakeTenantStore{tenants: existing, nextID: 456}

	f.Handle("GET /tenant/tenants", func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		defer store.mu.Unlock()
		found := []map[string]any{}
		domain := r.URL.Query().Get("domain")
		for _, tenant := range store.tenants {
			if domain == "" || strings.EqualFold(domain, tenant["domain"].(string)) {
				found = append(found, tenant)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"tenants": found, "statistics": stats()})
	})
	f.Handle("POST /tenant/tenants", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		store.mu.Lock()
		defer store.mu.Unlock()
		tenant := map[string]any{"id": "t" + strconv.Itoa(store.nextID), "domain": body["domain"], "company": body["company"]}
		store.nextID++
		store.tenants = append(store.tenants, tenant)
		writeJSON(w, http.StatusCreated, tenant)
	})
	return f, store
}

// assertNoAPICalls checks that only the client login (if any) reached the
// fake server
func assertNoAPICalls(t *testing.T, f *fakeC8y) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		assert.True(t, strings.HasPrefix(r.Path, "/tenant/oauth"), "a dry run makes no API calls, got %s %s", r.Method, r.Path)
	}
}

func existingCustomerTenant() map[string]any {
	return map[string]any{
		"id":               "t456",
		"domain":           customerDomain,
		"company":          "Customer A",
		"contactName":      "Jane Doe",
		"customProperties": map[string]any{"externalReference": "CRM-1234", "region": "eu"},
	}
}

func TestChildTenantsDryRun(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	s.DryRun = true

	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{
		{Domain: customerDomain, Company: "Customer A", AdminPass: "x", Applications: []string{"cockpit"}},
	}))
	r := resultFor(t, s, SectionTenants)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, customerDomain, r.Item)
	assert.Contains(t, r.Detail, "subscribe: cockpit")
	assertNoAPICalls(t, f)
}

func TestChildTenantCreate(t *testing.T) {
	f, _ := fakeChildTenants(t, builtinApps())
	f.JSON("POST /tenant/tenants/t456/applications", http.StatusCreated, map[string]any{})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{{
		Domain:           customerDomain,
		Company:          "Customer A",
		AdminName:        "admin",
		AdminEmail:       "admin@customer-a.example.invalid",
		AdminPass:        "Secret-Pass-123",
		ContactName:      "Jane Doe",
		ContactPhone:     "+49 123",
		CustomProperties: map[string]any{"externalReference": "CRM-1234"},
		Applications:     []string{"smartrule", "web-app"},
	}}))

	r := resultFor(t, s, SectionTenants)
	require.NoError(t, r.Err)
	assert.Equal(t, ActionCreated, r.Action)
	assert.Equal(t, "created: t456; subscribed: smartrule, web-app", r.Detail)

	created := f.Requests(http.MethodPost, "/tenant/tenants")
	require.NotEmpty(t, created)
	assert.Equal(t, "/tenant/tenants", created[0].Path)
	assert.Equal(t, map[string]any{
		"domain":           customerDomain,
		"company":          "Customer A",
		"adminName":        "admin",
		"adminEmail":       "admin@customer-a.example.invalid",
		"adminPass":        "Secret-Pass-123",
		"contactName":      "Jane Doe",
		"contactPhone":     "+49 123",
		"customProperties": map[string]any{"externalReference": "CRM-1234"},
	}, created[0].Body)
	assert.Len(t, f.Requests(http.MethodPost, "/tenant/tenants/t456/applications"), 2)
	// Child tenants are listed from the current tenant
	lists := f.Requests(http.MethodGet, "/tenant/tenants")
	require.NotEmpty(t, lists)
	assert.Contains(t, lists[0].Query, "parent=t123")
}

func TestChildTenantCreateWithPasswordResetEmail(t *testing.T) {
	f, _ := fakeChildTenants(t, nil)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{{
		Domain: customerDomain, Company: "Customer A", AdminEmail: "admin@customer-a.example.invalid", SendPasswordResetEmail: true,
	}}))
	r := resultFor(t, s, SectionTenants)
	assert.Equal(t, ActionCreated, r.Action)
	assert.Contains(t, r.Detail, "password reset email sent")
	created := f.Requests(http.MethodPost, "/tenant/tenants")
	require.Len(t, created, 1)
	assert.Equal(t, true, created[0].Body["sendPasswordResetEmail"])
	assert.NotContains(t, created[0].Body, "adminPass")
}

func TestChildTenantCreateRequirements(t *testing.T) {
	f, _ := fakeChildTenants(t, nil)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{{Domain: customerDomain}}))
	r := resultFor(t, s, SectionTenants)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, "creating it requires company and one of adminPass, adminPasswordFrom or sendPasswordResetEmail")
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants"))
}

func TestChildTenantExistingUnchanged(t *testing.T) {
	f, _ := fakeChildTenants(t, builtinApps(), existingCustomerTenant())
	s := newFakeSyncer(t, f)

	// Existing tenants are not required to declare admin credentials
	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{{
		Domain:           strings.ToUpper(customerDomain), // matched case-insensitively
		Company:          "Customer A",
		ContactName:      "Jane Doe",
		CustomProperties: map[string]any{"externalReference": "CRM-1234"},
	}}))
	r := resultFor(t, s, SectionTenants)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Equal(t, "id: t456", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants"))
	assert.Empty(t, f.Requests(http.MethodPut, "/tenant/tenants"))
}

func TestChildTenantExistingUpdated(t *testing.T) {
	f, _ := fakeChildTenants(t, builtinApps(), existingCustomerTenant())
	f.JSON("PUT /tenant/tenants/t456", http.StatusOK, map[string]any{"id": "t456"})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{{
		Domain:           customerDomain,
		Company:          "Customer A GmbH",
		AdminName:        "other-admin", // never changed on an existing tenant
		AdminPass:        "Other-Pass-123",
		ContactName:      "Jane Doe",
		CustomProperties: map[string]any{"externalReference": "CRM-9999"},
	}}))
	r := resultFor(t, s, SectionTenants)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "id: t456; updated company, customProperties", r.Detail)

	updates := f.Requests(http.MethodPut, "/tenant/tenants/t456")
	require.Len(t, updates, 1)
	assert.Equal(t, map[string]any{
		"company": "Customer A GmbH",
		// Keys not in the manifest are kept
		"customProperties": map[string]any{"externalReference": "CRM-9999", "region": "eu"},
	}, updates[0].Body)
}

func TestChildTenantExistingSubscribesMissingApplications(t *testing.T) {
	// t456 is subscribed to smartrule (served by fakeApplicationTenant)
	f := fakeApplicationTenant(t, builtinApps(), "smartrule")
	f.JSON("GET /tenant/tenants", http.StatusOK, map[string]any{"tenants": []any{existingCustomerTenant()}, "statistics": stats()})
	f.JSON("POST /tenant/tenants/t456/applications", http.StatusCreated, map[string]any{})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncTenants(context.Background(), []ChildTenantSpec{{
		Domain: customerDomain, Applications: []string{"smartrule", "web-app"},
	}}))
	r := resultFor(t, s, SectionTenants)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "id: t456; subscribed: web-app", r.Detail)
	subscribe := f.Requests(http.MethodPost, "/tenant/tenants/t456/applications")
	require.Len(t, subscribe, 1)
	assert.Contains(t, string(subscribe[0].RawBody), "/application/applications/3")
}

func TestChildTenantsValidation(t *testing.T) {
	_, err := LoadManifest(writeManifest(t, `
tenants:
  - company: no domain
  - domain: a.example.invalid
    adminPass: x
    adminPasswordFrom: {generate: a-admin}
  - domain: A.example.invalid
  - domain: b.example.invalid
    adminPass: x
    sendPasswordResetEmail: true
  - domain: c.example.invalid
    sendPasswordResetEmail: true
  - domain: d.example.invalid
    adminPasswordFrom: {}
  - domain: https://e.example.invalid
    applications: [" "]
`))
	require.Error(t, err)
	for _, msg := range []string{
		"tenants[0]: domain is required",
		"tenants[1]: adminPass, adminPasswordFrom and sendPasswordResetEmail are mutually exclusive",
		`tenants[2]: duplicate domain "A.example.invalid"`,
		"tenants[3]: adminPass, adminPasswordFrom and sendPasswordResetEmail are mutually exclusive",
		"tenants[4]: sendPasswordResetEmail requires adminEmail",
		"tenants[5].adminPasswordFrom requires one of: env, generate",
		`tenants[6]: domain "https://e.example.invalid" must be a domain name`,
		"tenants[6].applications[0]: application name must not be empty",
	} {
		assert.Contains(t, err.Error(), msg)
	}

	manifest, err := LoadManifest(writeManifest(t, `
tenants:
  - domain: customer-a.example.invalid
    company: Customer A
    adminPasswordFrom: {generate: customer-a-admin}
    customProperties: {externalReference: CRM-1234, tier: 2}
`))
	require.NoError(t, err)
	require.Len(t, manifest.Tenants, 1)
	assert.Equal(t, "customer-a-admin", manifest.Tenants[0].AdminPasswordFrom.Generate)
}

func TestChildTenantsAdminPassEnv(t *testing.T) {
	t.Setenv("TS_CUSTOMER_ADMIN", "From-Env-123")
	manifest, err := LoadManifest(writeManifest(t, "tenants:\n  - domain: a.example.invalid\n    adminPass: ${TS_CUSTOMER_ADMIN}\n  - domain: b.example.invalid\n    adminPasswordFrom: {env: TS_CUSTOMER_ADMIN}\n"))
	require.NoError(t, err)
	assert.Equal(t, "From-Env-123", manifest.Tenants[0].AdminPass)

	values, err := manifest.ResolveSecretRefs(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"From-Env-123"}, values)
	assert.Equal(t, "From-Env-123", manifest.Tenants[1].AdminPass)
	assert.Nil(t, manifest.Tenants[1].AdminPasswordFrom)
}

func TestChildTenantsSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte(`
tenants:
  - domain: customer-a.example.invalid
    company: Customer A
    adminName: admin
    adminEmail: admin@customer-a.example.invalid
    adminPasswordFrom: {generate: customer-a-admin}
    contactName: Jane Doe
    contactPhone: "+49 123"
    customProperties: {externalReference: CRM-1234}
    applications: [devicemanagement, cockpit]
`)))
	assert.Error(t, validateYAML(t, schema, []byte("tenants:\n  - company: no domain\n")))
	assert.Error(t, validateYAML(t, schema, []byte("tenants:\n  - domain: a.example.invalid\n    adminPass: x\n    adminPasswordFrom: {generate: y}\n")))
	assert.Error(t, validateYAML(t, schema, []byte("tenants:\n  - domain: a.example.invalid\n    unknown: x\n")))
}

func TestSectionFilters(t *testing.T) {
	assert.True(t, sectionEnabled(nil, SectionTenants))
	assert.True(t, sectionEnabled([]string{" Tenants "}, SectionTenants))
	assert.False(t, sectionEnabled([]string{"users"}, SectionTenants))
	assert.False(t, onlySections(nil, SectionTenants))
	assert.True(t, onlySections([]string{"tenants"}, SectionTenants))
	assert.False(t, onlySections([]string{"tenants", "users"}, SectionTenants))
}

const childTenantRunManifest = `tenants:
  - domain: ` + customerDomain + `
    company: Customer A
    adminName: admin
    adminEmail: admin@customer-a.example.invalid
    adminPasswordFrom:
      generate: customer-a-admin
`

// TestRunCreatesChildTenantWithGeneratedPassword runs the command: the
// generated admin password is stored in the secrets file, sent on creation and
// masked in all output
func TestRunCreatesChildTenantWithGeneratedPassword(t *testing.T) {
	f, _ := fakeChildTenants(t, nil)
	setupRunAgainst(t, f)

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(childTenantRunManifest), 0o644))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--debug")
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)

	secrets := readSecretsFile(t, filepath.Join(dir, "tenant.secrets.yaml"))
	require.Contains(t, secrets, "customer-a-admin")
	value := secrets["customer-a-admin"].Value
	require.Len(t, value, defaultGenerateLength)

	created := f.Requests(http.MethodPost, "/tenant/tenants")
	require.Len(t, created, 1)
	assert.Equal(t, value, created[0].Body["adminPass"])

	assert.NotContains(t, stdout, value)
	assert.NotContains(t, stderr, value)
	assert.Contains(t, stdout, "[tenants] "+customerDomain+" (created): created: t456; admin password from ${generate:customer-a-admin}")
	assert.Contains(t, stdout, "🔐 Generated 1 value(s): customer-a-admin")

	// A second run finds the tenant: nothing is created or generated
	stdout, _ = captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, stdout)
	assert.Contains(t, stdout, "[tenants] "+customerDomain+" (unchanged): id: t456")
	assert.NotContains(t, stdout, "🔐 Generated")
	assert.Len(t, f.Requests(http.MethodPost, "/tenant/tenants"), 1)
}

func TestRunChildTenantsDryRun(t *testing.T) {
	f, _ := fakeChildTenants(t, nil)
	setupRunAgainst(t, f)
	manifest := writeManifest(t, childTenantRunManifest+"targets:\n  tenants: ["+customerDomain+"]\n")

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--dry-run")
	})
	require.NoError(t, runErr, stdout)
	assert.Contains(t, stdout, "[tenants] "+customerDomain+" (planned)")
	assertNoAPICalls(t, f)
}

// childTenantTargetManifest creates a tenant and configures it in the same
// run, with credentials from a go-c8y-cli session matched by domain
func childTenantTargetManifest(sessionHome string) string {
	return childTenantRunManifest + `
targets:
  tenants: [` + customerDomain + `]
  credentials:
    mode: sessions
    sessionHome: ` + sessionHome + `

tenantOptions:
  - category: onboarding
    key: crm
    value: CRM-1234
`
}

func TestRunCreatesChildTenantAndAppliesManifestToIt(t *testing.T) {
	f, _ := fakeChildTenants(t, nil)
	var optionAuth []string
	f.Handle("POST /tenant/options", func(w http.ResponseWriter, r *http.Request) {
		optionAuth = append(optionAuth, r.Header.Get("Authorization"))
		writeJSON(w, http.StatusCreated, map[string]any{})
	})
	setupRunAgainst(t, f)

	sessionHome := t.TempDir()
	writeSessionFile(t, sessionHome, "customer-a.json", `{"host": "https://`+customerDomain+`", "username": "admin", "password": "customer-pass"}`)
	manifest := writeManifest(t, childTenantTargetManifest(sessionHome))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)

	assert.Contains(t, stdout, "(created): created: t456")
	assert.Contains(t, stdout, "🏢 Tenant: t456")
	assert.Contains(t, stdout, "[tenantOptions] t456: onboarding/crm (created)")
	// The tenant option is set inside the new tenant with its own credentials
	require.Len(t, optionAuth, 1)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:customer-pass")), optionAuth[0])
}

func TestRunOnlyChildTenants(t *testing.T) {
	f, _ := fakeChildTenants(t, nil)
	setupRunAgainst(t, f)
	manifest := writeManifest(t, childTenantTargetManifest(filepath.Join(t.TempDir(), "missing")))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--only", "tenants")
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "(created): created: t456")
	assert.NotContains(t, stdout, "🏢 Tenant", "the targets are not resolved")
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/options"))
	for _, list := range f.Requests(http.MethodGet, "/tenant/tenants") {
		assert.NotContains(t, list.Query, "domain=", "the targets are not looked up")
	}

	// Other sections skip the tenants section
	f2, _ := fakeChildTenants(t, nil)
	f2.JSON("POST /tenant/options", http.StatusCreated, map[string]any{})
	setupRunAgainst(t, f2)
	stdout, _ = captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", writeManifest(t, childTenantRunManifest+"tenantOptions:\n  - {category: a, key: b, value: c}\n"), "--only", "tenantOptions")
	})
	require.NoError(t, runErr, stdout)
	assert.NotContains(t, stdout, "[tenants]")
	assert.Empty(t, f2.Requests(http.MethodPost, "/tenant/tenants"))
}

func TestIncludeMergesChildTenantsByDomain(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml":   "tenants:\n  - domain: Customer-A.example.com\n    company: Old name\n  - domain: customer-b.example.com\n    company: B\n",
		"tenant.yaml": "include: [base.yaml]\ntenants:\n  - domain: customer-a.example.com\n    company: Customer A\n",
	})
	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	require.Len(t, manifest.Tenants, 2)
	assert.Equal(t, "Customer A", manifest.Tenants[0].Company, "the later file wins (domains match case-insensitively)")
	assert.Equal(t, "customer-b.example.com", manifest.Tenants[1].Domain)
}
