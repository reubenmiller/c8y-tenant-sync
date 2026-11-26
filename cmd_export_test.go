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

func stats() map[string]any {
	return map[string]any{"pageSize": 2000, "currentPage": 1}
}

// fakeExportTenant registers the endpoints read by export
func fakeExportTenant(t *testing.T) *fakeC8y {
	f := newFakeC8y(t)
	stubExportDiscovery(t, nil)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123", "domainName": "example.c8y.io"})
	f.JSON("GET /tenant/options", http.StatusOK, map[string]any{
		"options": []map[string]any{
			{"category": "zeta", "key": "b", "value": "2"},
			{"category": "alpha", "key": "enabled", "value": "true"},
			{"category": "sso", "key": "credentials.secret", "value": "<<Encrypted>>"},
			{"category": "alpha", "key": "path", "value": "/home/$USER"},
			{"category": "alarm.type.mapping", "key": "foo1", "value": "MINOR|Some foo1 mapping"},
		},
		"statistics": stats(),
	})
	f.JSON("GET /tenant/options/alarm.type.mapping", http.StatusOK, map[string]any{
		"foo2": "NONE|example 2",
		"foo1": "MINOR|Some foo1 mapping",
		"bar":  "MAJOR|",
	})
	f.JSON("GET /features", http.StatusOK, []map[string]any{
		{"key": "feature-on", "active": true, "strategy": "TENANT", "phase": "PUBLIC_PREVIEW"},
		{"key": "feature-off", "active": false, "strategy": "TENANT", "phase": "PUBLIC_PREVIEW"},
		{"key": "feature-default", "active": true, "strategy": "DEFAULT", "phase": "GENERALLY_AVAILABLE"},
	})
	f.JSON("GET /retention/retentions", http.StatusOK, map[string]any{
		"retentionRules": []map[string]any{
			{"id": "1", "dataType": "MEASUREMENT", "fragmentType": "*", "type": "*", "source": "*", "maximumAge": 90, "editable": true},
			{"id": "2", "dataType": "ALARM", "fragmentType": "c8y_Temp", "type": "*", "source": "12345", "maximumAge": 30, "editable": false},
		},
		"statistics": stats(),
	})
	f.JSON("GET /tenant/tenants/t123/trusted-certificates", http.StatusOK, caCollection("ENABLED", true))
	f.Handle("GET /application/applications", func(w http.ResponseWriter, r *http.Request) {
		apps := []map[string]any{}
		if strings.Contains(r.URL.Query().Get("type"), "MICROSERVICE") {
			apps = append(apps,
				map[string]any{"id": "11", "name": "smartrule", "type": "MICROSERVICE"},
				map[string]any{"id": "10", "name": "devicesimulator", "type": "MICROSERVICE"})
		}
		if strings.Contains(r.URL.Query().Get("type"), "HOSTED") {
			apps = append(apps,
				map[string]any{"id": "21", "name": "devicemanagement", "type": "HOSTED"},
				map[string]any{"id": "20", "name": "cockpit", "type": "HOSTED"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"applications": apps, "statistics": stats()})
	})
	f.JSON("GET /inventory/managedObjects", http.StatusOK, map[string]any{
		"managedObjects": []map[string]any{
			{"id": "1", "name": "Europe", "type": "c8y_DeviceGroup", "c8y_IsDeviceGroup": map[string]any{},
				"description": "European sites", "c8y_TenantSync": map[string]any{"tool": "tenant-sync"},
				"assetParents": map[string]any{"references": []any{}}},
			{"id": "2", "name": "Plant Berlin", "type": "c8y_DeviceSubgroup", "c8y_IsDeviceGroup": map[string]any{},
				"assetParents": map[string]any{"references": []any{map[string]any{"managedObject": map[string]any{"id": "1"}}}}},
		},
		"statistics": stats(),
	})
	f.JSON("GET /user/inventoryroles", http.StatusOK, map[string]any{
		"roles": []map[string]any{
			{"id": 2, "name": "Site operator", "description": "Alarms and operations for one site", "permissions": []map[string]any{
				{"id": 5, "scope": "OPERATION", "type": "*", "permission": "ADMIN"},
				{"id": 4, "scope": "ALARM", "type": "*", "permission": "ADMIN"},
			}},
			{"id": 1, "name": "Restart only", "permissions": []map[string]any{
				{"id": 3, "scope": "OPERATION", "type": "c8y_Restart", "permission": "ADMIN"},
			}},
		},
		"statistics": stats(),
	})
	f.JSON("GET /tenant/loginOptions", http.StatusOK, map[string]any{
		"loginOptions": exportLoginOptions(),
		"statistics":   stats(),
	})
	f.JSON("GET /user/t123/groups/3", http.StatusOK, map[string]any{"id": 3, "name": "admins"})
	f.JSON("GET /application/applications/11", http.StatusOK, map[string]any{"id": "11", "name": "smartrule", "type": "MICROSERVICE"})
	return f
}

// stubExportDiscovery replaces the check of exported SSO discovery URLs
// (the fake tenant's provider does not exist)
func stubExportDiscovery(t *testing.T, err error) {
	t.Helper()
	original := exportDiscoverOIDC
	exportDiscoverOIDC = func(ctx context.Context, url string) error { return err }
	t.Cleanup(func() { exportDiscoverOIDC = original })
}

// exportLoginOptions are the login options of the exported fake tenant:
// OAI-Secure is preferred and SSO is configured (with its client secret)
func exportLoginOptions() []map[string]any {
	return []map[string]any{
		{"id": "b1", "type": "BASIC", "visibleOnLoginPage": false, "userManagementSource": "INTERNAL",
			"authenticationRestrictions": map[string]any{"forbiddenClients": "WEB_BROWSERS"}},
		{"id": "o1", "type": "OAUTH2_INTERNAL", "visibleOnLoginPage": true, "grantType": "PASSWORD",
			"sessionConfiguration": map[string]any{"absoluteTimeoutMillis": 1209600000, "renewalTimeoutMillis": 86400000,
				"maximumNumberOfParallelSessions": 5, "userAgentValidationRequired": false}},
		{"id": "s1", "type": "OAUTH2", "visibleOnLoginPage": true, "providerName": "Acme", "buttonName": "Log in with Acme",
			"issuer": "https://idp.example.com/realms/acme", "clientId": "cumulocity", "useIdToken": true,
			"authorizationRequest":         map[string]any{"requestParams": map[string]any{"scope": "openid profile email groups"}},
			"tokenRequest":                 map[string]any{"body": "grant_type=authorization_code&client_secret=top-secret-value"},
			"userIdConfig":                 map[string]any{"jwtField": "preferred_username"},
			"accessTokenToUserDataMapping": map[string]any{"emailClaimName": "email", "firstNameClaimName": "given_name", "lastNameClaimName": "family_name"},
			"onNewUser": map[string]any{"dynamicMapping": map[string]any{
				"configuration": map[string]any{"mapRolesOnlyForNewUser": false, "manageRolesOnlyFromAccessMapping": true, "mapFromIdToken": true},
				"mappings": []any{map[string]any{
					"id":               "m1",
					"when":             map[string]any{"operator": "AND", "childPredicates": []any{map[string]any{"parameterPath": "groups", "operator": "IN", "value": "c8y-admins"}}},
					"thenGroups":       []any{3},
					"thenApplications": []any{11},
				}},
			}}},
	}
}

func TestExportAllSections(t *testing.T) {
	f := fakeExportTenant(t)
	exporter := &Exporter{Client: f.Client()}

	manifest, tenantID, err := exporter.Export(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "t123", tenantID)

	// Credentials are never exported; the rest is sorted by category and key
	assert.Equal(t, []TenantOptionSpec{
		{Category: "alpha", Key: "enabled", Value: "true"},
		{Category: "alpha", Key: "path", Value: "/home/$USER"},
		{Category: "zeta", Key: "b", Value: "2"},
	}, manifest.TenantOptions)

	// The alarm mapping is exported in its own section, not in tenantOptions
	assert.Equal(t, []AlarmMappingSpec{
		{Type: "bar", Severity: "MAJOR"},
		{Type: "foo1", Severity: "MINOR", Description: "Some foo1 mapping"},
		{Type: "foo2", Severity: "NONE", Description: "example 2"},
	}, manifest.AlarmMapping.Mappings)

	// Only the toggles set for the tenant
	assert.Equal(t, []FeatureSpec{
		{Key: "feature-off", Enabled: boolPtr(false)},
		{Key: "feature-on", Enabled: boolPtr(true)},
	}, manifest.Features)

	// "*" selectors are left out, editable is only written when false
	assert.Equal(t, []RetentionRuleSpec{
		{DataType: "MEASUREMENT", MaximumAge: 90},
		{DataType: "ALARM", FragmentType: "c8y_Temp", Source: "12345", MaximumAge: 30, Editable: boolPtr(false)},
	}, manifest.RetentionRules)

	assert.Equal(t, &CertificateAuthoritySpec{Enabled: boolPtr(true), AutoRegistrationEnabled: boolPtr(true)}, manifest.CertificateAuthority)

	assert.Equal(t, []MicroserviceSpec{{Name: "devicesimulator"}, {Name: "smartrule"}}, manifest.Microservices)
	assert.Equal(t, []ApplicationSpec{{Name: "cockpit"}, {Name: "devicemanagement"}}, manifest.Applications)

	// The device group tree without bookkeeping fragments
	assert.Equal(t, []DeviceGroupSpec{
		{Name: "Europe", Description: "European sites", Groups: []DeviceGroupSpec{{Name: "Plant Berlin"}}},
	}, manifest.DeviceGroups)

	// Subscribed (not owned) microservices and web applications of the
	// current tenant (the application of the SSO access mapping is read by
	// ID)
	var apps []recordedRequest
	for _, r := range f.Requests(http.MethodGet, "/application/applications") {
		if r.Path == "/application/applications" {
			apps = append(apps, r)
		}
	}
	require.Len(t, apps, 2)
	assert.Contains(t, apps[0].Query, "providedFor=t123")
	assert.Contains(t, apps[0].Query, "type=MICROSERVICE")
	assert.Contains(t, apps[1].Query, "providedFor=t123")
	assert.Contains(t, apps[1].Query, "type=HOSTED%2CEXTERNAL")

	// Inventory roles sorted by name, permissions sorted, type "*" left out
	assert.Equal(t, []InventoryRoleSpec{
		{Name: "Restart only", Permissions: []InventoryRolePermissionSpec{
			{Scope: "OPERATION", Type: "c8y_Restart", Permission: "ADMIN"},
		}},
		{Name: "Site operator", Description: "Alarms and operations for one site", Permissions: []InventoryRolePermissionSpec{
			{Scope: "ALARM", Permission: "ADMIN"},
			{Scope: "OPERATION", Permission: "ADMIN"},
		}},
	}, manifest.InventoryRoles)

	// Login options: the SSO client secret is replaced by an env reference,
	// group and application IDs by names
	five := 5
	assert.Equal(t, &LoginOptionsSpec{
		Preferred: LoginModeOAISecure,
		Basic:     &BasicLoginOptionSpec{ForbiddenClients: []string{"WEB_BROWSERS"}},
		OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{
			AbsoluteTimeout: "336h", RenewalTimeout: "24h", MaximumParallelSessions: &five, UserAgentValidationRequired: boolPtr(false),
		}},
		SSO: &SSOLoginSpec{
			DiscoveryURL:     "https://idp.example.com/realms/acme/.well-known/openid-configuration",
			ClientID:         "cumulocity",
			ClientSecretFrom: &SecretRef{Env: "SSO_CLIENT_SECRET"},
			Scopes:           []string{"openid", "profile", "email", "groups"},
			ProviderName:     "Acme",
			ButtonName:       "Log in with Acme",
			UserIDClaim:      "preferred_username",
			Access: &SSOAccessMappingSpec{
				ManageRolesOnlyFromAccessMapping: true,
				Rules: []SSOAccessRuleSpec{{
					When:         []SSOConditionSpec{{Claim: "groups", Operator: "IN", Value: "c8y-admins"}},
					GlobalRoles:  []string{"admins"},
					Applications: []string{"smartrule"},
				}},
			},
		},
	}, manifest.LoginOptions)
}

func TestExportAllFeaturesAndSelectedSections(t *testing.T) {
	f := fakeExportTenant(t)
	exporter := &Exporter{
		Client:      f.Client(),
		Sections:    []string{SectionFeatures, SectionApplications},
		AllFeatures: true,
	}
	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)
	assert.Len(t, manifest.Features, 3)
	assert.Empty(t, manifest.TenantOptions, "sections not selected must not be exported")
	assert.Nil(t, manifest.CertificateAuthority)
	assert.Empty(t, manifest.Microservices)
	assert.Empty(t, f.Requests(http.MethodGet, "/tenant/options"))

	apps := f.Requests(http.MethodGet, "/application/applications")
	require.Len(t, apps, 1)
	assert.Contains(t, apps[0].Query, "type=HOSTED%2CEXTERNAL")
}

func TestExportWithoutCertificateAuthority(t *testing.T) {
	f := fakeExportTenant(t)
	f.JSON("GET /tenant/tenants/t123/trusted-certificates", http.StatusOK, emptyCollection())

	exporter := &Exporter{Client: f.Client(), Sections: []string{SectionCertificateAuthority}}
	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)
	assert.Nil(t, manifest.CertificateAuthority)
}

func TestExportFailsOnAPIError(t *testing.T) {
	f := fakeExportTenant(t)
	f.JSON("GET /retention/retentions", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})

	exporter := &Exporter{Client: f.Client()}
	_, _, err := exporter.Export(context.Background())
	assert.ErrorContains(t, err, "failed to export retentionRules")
}

// TestExportedManifestRoundTrip checks that the exported YAML is a valid
// manifest with the same content
func TestExportedManifestRoundTrip(t *testing.T) {
	f := fakeExportTenant(t)
	exporter := &Exporter{Client: f.Client()}
	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)

	content, err := MarshalManifest(manifest, "Exported for a test")
	require.NoError(t, err)
	text := string(content)
	assert.True(t, strings.HasPrefix(text, "# yaml-language-server: $schema="+SchemaID+"\n# Exported for a test\n"))
	assert.NotContains(t, text, "hooks", "empty sections must be left out")
	assert.NotContains(t, text, "{", "block style is used")
	assert.Contains(t, text, "value: \"true\"", "string values which look like booleans stay strings")

	assert.Contains(t, text, "/home/$$USER", "$ is escaped")
	assert.Contains(t, text, "alarmMapping:\n  - type: bar\n    severity: MAJOR\n  - type: foo1\n    severity: MINOR\n    description: Some foo1 mapping\n  - type: foo2\n    severity: NONE\n    description: example 2\n",
		"the alarm mapping is a list sorted by type")
	assert.NotContains(t, text, "alarm.type.mapping", "the alarm mapping options are not in tenantOptions")
	assert.NotContains(t, text, "top-secret-value", "the SSO client secret is never exported")
	assert.Contains(t, text, "globalRoles:\n", "SSO access rules list global roles (thenGroups in the API)")
	assert.NotContains(t, text, "thenGroups")

	// The escaped value is not expanded, even when the variable is set
	t.Setenv("USER", "someone")
	path := filepath.Join(t.TempDir(), "tenant.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	loaded, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, manifest, loaded)

	// And it satisfies the JSON schema
	assert.NoError(t, validateYAML(t, compileSchema(t), content))
}

func TestMarshalEmptyManifest(t *testing.T) {
	content, err := MarshalManifest(&Manifest{}, "nothing")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(content), "{}\n"))

	path := filepath.Join(t.TempDir(), "tenant.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))
	_, err = LoadManifest(path)
	assert.NoError(t, err)
}

func TestExportCommandWritesFile(t *testing.T) {
	f := fakeExportTenant(t)
	t.Setenv("C8Y_HOST", f.server.URL)
	t.Setenv("C8Y_TENANT", "t123")
	t.Setenv("C8Y_USER", "user")
	t.Setenv("C8Y_PASSWORD", "pass")
	t.Setenv("C8Y_TOKEN", "")

	output := filepath.Join(t.TempDir(), "exported.yaml")
	require.NoError(t, runRoot(t, "export", "-o", output, "--only", "tenantOptions"))
	loaded, err := LoadManifest(output)
	require.NoError(t, err)
	assert.Len(t, loaded.TenantOptions, 3)
	assert.Empty(t, loaded.Features)

	// The file is not overwritten without --force
	assert.ErrorContains(t, runRoot(t, "export", "-o", output), "already exists")
	assert.NoError(t, runRoot(t, "export", "-o", output, "--force", "--only", "features"))
}

func TestExportCommandRejectsUnknownSection(t *testing.T) {
	assert.ErrorContains(t, runRoot(t, "export", "--only", "software"), "unknown section")
}
