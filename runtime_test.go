package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/currenttenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeFromLookup(t *testing.T) {
	env := map[string]string{
		"C8Y_SETTINGS_DEFAULTS_DRY":      "true",
		"C8Y_SETTINGS_DEFAULTS_INSECURE": "false",
		"C8Y_HEADER_AUTHORIZATION":       " Basic abc ",
	}
	r := runtimeFromLookup(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	assert.True(t, r.DryRun)
	assert.False(t, r.Insecure)
	assert.Equal(t, "Basic abc", r.AuthorizationHeader)

	empty := runtimeFromLookup(func(string) (string, bool) { return "", false })
	assert.Equal(t, Runtime{}, empty)
}

func TestAuthorizationHeaderMiddleware(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
	}))
	defer server.Close()

	client := &http.Client{Transport: authorizationHeaderMiddleware("Bearer session")(http.DefaultTransport)}

	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	_, err := client.Do(req)
	require.NoError(t, err)

	// Requests with their own credentials (e.g. other tenants) keep them
	req, _ = http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "Basic other")
	_, err = client.Do(req)
	require.NoError(t, err)

	assert.Equal(t, []string{"Bearer session", "Basic other"}, got)
}

// TestNewClientUsesAuthorizationHeader checks that a session without token or
// password (as passed by c8y to extensions) authenticates with the header
func TestNewClientUsesAuthorizationHeader(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"t123"}`))
	}))
	defer server.Close()

	t.Setenv("C8Y_HOST", server.URL)
	t.Setenv("C8Y_TENANT", "t123")
	t.Setenv("C8Y_USER", "user")
	t.Setenv("C8Y_PASSWORD", "")
	t.Setenv("C8Y_TOKEN", "")

	client, err := Runtime{AuthorizationHeader: "Basic c2Vzc2lvbg=="}.NewClient(false)
	require.NoError(t, err)

	tenant := client.Tenants.Current.Get(context.Background(), currenttenant.GetOptions{})
	require.NoError(t, tenant.Err)
	assert.Equal(t, "Basic c2Vzc2lvbg==", got)
}

func TestNewClientRequiresHost(t *testing.T) {
	for _, key := range []string{"C8Y_BASEURL", "C8Y_URL", "C8Y_HOST", "C8Y_TOKEN"} {
		t.Setenv(key, "")
	}
	_, err := Runtime{}.NewClient(false)
	assert.ErrorContains(t, err, "no active session")
}

func TestRunHonoursC8yDryRun(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("tenantOptions:\n  - category: c\n    key: k\n    value: v\n"), 0o644))

	// No session at all: only works if the dry run from c8y is honoured
	for _, key := range []string{"C8Y_BASEURL", "C8Y_URL", "C8Y_HOST", "C8Y_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "-f", manifest})
	cmd.SetOut(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())
}

func TestRunFlagValidation(t *testing.T) {
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("{}\n"), 0o644))

	for name, args := range map[string][]string{
		"unknown section":   {"run", manifest, "--only", "nope"},
		"concurrency range": {"run", manifest, "--concurrency", "0"},
		"manifest twice":    {"run", "-f", manifest, manifest},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(args)
			assert.Error(t, cmd.Execute())
		})
	}
}
