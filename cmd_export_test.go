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
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123", "domainName": "example.c8y.io"})
	f.JSON("GET /tenant/options", http.StatusOK, map[string]any{
		"options": []map[string]any{
			{"category": "zeta", "key": "b", "value": "2"},
			{"category": "alpha", "key": "enabled", "value": "true"},
			{"category": "sso", "key": "credentials.secret", "value": "<<Encrypted>>"},
			{"category": "alpha", "key": "path", "value": "/home/$USER"},
		},
		"statistics": stats(),
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
	f.JSON("GET /application/applications", http.StatusOK, map[string]any{
		"applications": []map[string]any{
			{"id": "11", "name": "smartrule", "type": "MICROSERVICE"},
			{"id": "10", "name": "devicesimulator", "type": "MICROSERVICE"},
		},
		"statistics": stats(),
	})
	return f
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

	assert.Equal(t, []ApplicationSpec{
		{Name: "devicesimulator", Type: "MICROSERVICE"},
		{Name: "smartrule", Type: "MICROSERVICE"},
	}, manifest.Applications)

	// Subscribed (not owned) microservices of the current tenant
	apps := f.Requests(http.MethodGet, "/application/applications")
	require.Len(t, apps, 1)
	assert.Contains(t, apps[0].Query, "providedFor=t123")
	assert.Contains(t, apps[0].Query, "type=MICROSERVICE")
}

func TestExportAllFeaturesAndApplicationTypes(t *testing.T) {
	f := fakeExportTenant(t)
	exporter := &Exporter{
		Client:           f.Client(),
		Sections:         []string{SectionFeatures, SectionApplications},
		AllFeatures:      true,
		ApplicationTypes: []string{"microservice", "HOSTED"},
	}
	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)
	assert.Len(t, manifest.Features, 3)
	assert.Empty(t, manifest.TenantOptions, "sections not selected must not be exported")
	assert.Nil(t, manifest.CertificateAuthority)
	assert.Empty(t, f.Requests(http.MethodGet, "/tenant/options"))

	apps := f.Requests(http.MethodGet, "/application/applications")
	require.Len(t, apps, 1)
	assert.Contains(t, apps[0].Query, "type=MICROSERVICE%2CHOSTED")
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
	require.NoError(t, runRoot(t, "export", "-o", output, "--sections", "tenantOptions"))
	loaded, err := LoadManifest(output)
	require.NoError(t, err)
	assert.Len(t, loaded.TenantOptions, 3)
	assert.Empty(t, loaded.Features)

	// The file is not overwritten without --force
	assert.ErrorContains(t, runRoot(t, "export", "-o", output), "already exists")
	assert.NoError(t, runRoot(t, "export", "-o", output, "--force", "--sections", "features"))
}

func TestExportCommandRejectsUnknownSection(t *testing.T) {
	assert.ErrorContains(t, runRoot(t, "export", "--sections", "software"), "unknown section")
}
