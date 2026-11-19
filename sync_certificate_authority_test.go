package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	caListPath = "/tenant/tenants/t123/trusted-certificates"
	caCertPath = "/tenant/tenants/t123/trusted-certificates/abc123"
)

func caCollection(status string, autoRegistration bool) map[string]any {
	return map[string]any{
		"certificates": []map[string]any{{
			"fingerprint":                "abc123",
			"name":                       "Cumulocity CA",
			"status":                     status,
			"autoRegistrationEnabled":    autoRegistration,
			"tenantCertificateAuthority": true,
		}},
		"statistics": map[string]any{"pageSize": 5, "currentPage": 1},
	}
}

func emptyCollection() map[string]any {
	return map[string]any{"certificates": []any{}, "statistics": map[string]any{"pageSize": 5, "currentPage": 1}}
}

func TestCertificateAuthorityDryRun(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	s.DryRun = true

	require.NoError(t, s.SyncCertificateAuthority(context.Background(), &CertificateAuthoritySpec{Enabled: boolPtr(true)}))
	r := resultFor(t, s, SectionCertificateAuthority)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "status=ENABLED", r.Detail)
	assert.Empty(t, f.Requests(http.MethodGet, caListPath), "dry run must not call the API")
}

func TestCertificateAuthorityNilSpec(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), nil))
	assert.Empty(t, s.Results)
}

func TestCertificateAuthorityCreatesWhenMissing(t *testing.T) {
	f := newFakeC8y(t)
	created := false
	f.Handle("GET "+caListPath, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "true", r.URL.Query().Get("certificateAuthority"))
		if created {
			writeJSON(w, http.StatusOK, caCollection("ENABLED", false))
			return
		}
		writeJSON(w, http.StatusOK, emptyCollection())
	})
	f.Handle("POST /certificate-authority", func(w http.ResponseWriter, r *http.Request) {
		created = true
		writeJSON(w, http.StatusCreated, caCollection("ENABLED", false)["certificates"].([]map[string]any)[0])
	})

	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), &CertificateAuthoritySpec{Enabled: boolPtr(true)}))

	r := resultFor(t, s, SectionCertificateAuthority)
	assert.Equal(t, ActionCreated, r.Action)
	assert.NoError(t, r.Err)
	assert.Len(t, f.Requests(http.MethodPost, "/certificate-authority"), 1)
	assert.Empty(t, f.Requests(http.MethodPut, caCertPath), "a new CA which matches needs no update")
}

func TestCertificateAuthorityCreateThenSetAutoRegistration(t *testing.T) {
	f := newFakeC8y(t)
	created := false
	f.Handle("GET "+caListPath, func(w http.ResponseWriter, r *http.Request) {
		if created {
			writeJSON(w, http.StatusOK, caCollection("ENABLED", false))
			return
		}
		writeJSON(w, http.StatusOK, emptyCollection())
	})
	f.Handle("POST /certificate-authority", func(w http.ResponseWriter, r *http.Request) {
		created = true
		writeJSON(w, http.StatusCreated, map[string]any{"fingerprint": "abc123", "status": "ENABLED"})
	})
	f.JSON("PUT "+caCertPath, http.StatusOK, map[string]any{"fingerprint": "abc123"})

	s := newFakeSyncer(t, f)
	spec := &CertificateAuthoritySpec{Enabled: boolPtr(true), AutoRegistrationEnabled: boolPtr(true)}
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), spec))

	assert.Equal(t, ActionCreated, resultFor(t, s, SectionCertificateAuthority).Action)
	puts := f.Requests(http.MethodPut, caCertPath)
	require.Len(t, puts, 1)
	assert.Equal(t, map[string]any{"status": "ENABLED", "autoRegistrationEnabled": true}, puts[0].Body)
}

func TestCertificateAuthorityUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET "+caListPath, http.StatusOK, caCollection("ENABLED", true))

	s := newFakeSyncer(t, f)
	spec := &CertificateAuthoritySpec{Enabled: boolPtr(true), AutoRegistrationEnabled: boolPtr(true)}
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), spec))

	assert.Equal(t, ActionUnchanged, resultFor(t, s, SectionCertificateAuthority).Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/certificate-authority"))
	assert.Empty(t, f.Requests(http.MethodPut, caListPath))
}

func TestCertificateAuthorityDisable(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET "+caListPath, http.StatusOK, caCollection("ENABLED", false))
	f.JSON("PUT "+caCertPath, http.StatusOK, map[string]any{"fingerprint": "abc123"})

	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), &CertificateAuthoritySpec{Enabled: boolPtr(false)}))

	r := resultFor(t, s, SectionCertificateAuthority)
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "status=DISABLED", r.Detail)
	puts := f.Requests(http.MethodPut, caCertPath)
	require.Len(t, puts, 1)
	assert.Equal(t, map[string]any{"status": "DISABLED"}, puts[0].Body)
	assert.Empty(t, f.Requests(http.MethodDelete, caListPath), "the CA must never be deleted")
}

func TestCertificateAuthorityDisableWhenMissing(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET "+caListPath, http.StatusOK, emptyCollection())

	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), &CertificateAuthoritySpec{Enabled: boolPtr(false)}))

	r := resultFor(t, s, SectionCertificateAuthority)
	assert.Equal(t, ActionUnchanged, r.Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/certificate-authority"), "disabling must not create a CA")
}

func TestCertificateAuthorityGetError(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET "+caListPath, http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})

	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncCertificateAuthority(context.Background(), &CertificateAuthoritySpec{Enabled: boolPtr(true)}))

	r := resultFor(t, s, SectionCertificateAuthority)
	assert.Equal(t, ActionFailed, r.Action)
	assert.Error(t, r.Err)
	assert.Empty(t, f.Requests(http.MethodPost, "/certificate-authority"))
}

func TestCertificateAuthorityRequiresEnabled(t *testing.T) {
	path := writeManifest(t, "certificateAuthority:\n  autoRegistrationEnabled: true\n")
	_, err := LoadManifest(path)
	assert.ErrorContains(t, err, "certificateAuthority.enabled is required")

	path = writeManifest(t, "certificateAuthority:\n  enabled: false\n")
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	require.NotNil(t, manifest.CertificateAuthority)
	assert.False(t, *manifest.CertificateAuthority.Enabled)
}
