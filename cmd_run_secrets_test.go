package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const generatedUserManifest = `users:
  - email: svc-dashboard@example.invalid
    password: ${generate:svc-dashboard-password}
`

// setupRunAgainst points the C8Y_* session variables at a fake server
func setupRunAgainst(t *testing.T, f *fakeC8y) {
	t.Helper()
	t.Setenv("C8Y_HOST", f.server.URL)
	t.Setenv("C8Y_TENANT", "t123")
	t.Setenv("C8Y_USER", "user")
	t.Setenv("C8Y_PASSWORD", "pass")
	t.Setenv("C8Y_TOKEN", "")
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "")
	t.Setenv(EnvSopsBinary, filepath.Join(t.TempDir(), "no-sops")) // plain secrets file
}

func readSecretsFile(t *testing.T, path string) map[string]SecretEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var file secretsFile
	require.NoError(t, yaml.Unmarshal(raw, &file))
	return file.Secrets
}

func TestRunGeneratesPasswordAndMasksOutput(t *testing.T) {
	f := fakeUsers(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	setupRunAgainst(t, f)

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(generatedUserManifest), 0o644))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		// --debug makes the go-c8y client print request bodies (including
		// the password) to stderr
		runErr = runRoot(t, "run", "-f", manifest, "--debug")
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)

	secretsPath := filepath.Join(dir, "tenant.secrets.yaml")
	secrets := readSecretsFile(t, secretsPath)
	require.Contains(t, secrets, "svc-dashboard-password")
	value := secrets["svc-dashboard-password"].Value
	require.Len(t, value, defaultGenerateLength)
	assert.Equal(t, f.server.URL, secrets["svc-dashboard-password"].Host)
	info, err := os.Stat(secretsPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The user is created with the generated password
	created := f.Requests(http.MethodPost, "/user/t123/users")
	require.Len(t, created, 1)
	assert.Equal(t, value, created[0].Body["password"])

	// The value never appears in the output, also not in the debug bodies
	assert.NotContains(t, stdout, value)
	assert.NotContains(t, stderr, value)
	assert.Contains(t, stderr, "***", "the debug output shows the masked password")
	assert.Contains(t, stdout, "initial password from ${generate:svc-dashboard-password}")
	assert.Contains(t, stdout, "🔐 Generated 1 value(s): svc-dashboard-password")
	assert.Contains(t, stdout, "Saved to "+secretsPath+" (unencrypted, permissions 0600)")
}

func TestRunReusesGeneratedValues(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(generatedUserManifest), 0o644))
	secretsPath := filepath.Join(dir, "tenant.secrets.yaml")
	require.NoError(t, os.WriteFile(secretsPath, []byte("secrets:\n  svc-dashboard-password:\n    value: Stored-Value-123\n    generatedAt: \"2026-11-19T00:00:00Z\"\n"), 0o600))
	before, err := os.ReadFile(secretsPath)
	require.NoError(t, err)

	// The user already exists: the password is not applied, nothing is generated
	f := fakeUsers(t, map[string]any{
		"id": "u1", "userName": "svc-dashboard@example.invalid", "email": "svc-dashboard@example.invalid",
		"enabled": true, "self": "http://example/user/t123/users/u1",
	})
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	setupRunAgainst(t, f)

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, stdout)
	assert.NotContains(t, stdout, "🔐 Generated", "no new values")
	assert.NotContains(t, stdout, "not applied", "the stored value was not generated in this run")
	after, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the secrets file is unchanged")
	assert.Empty(t, f.Requests(http.MethodPost, "/user/t123/users"))
}

func TestRunNewValueForExistingUserIsReportedAsNotApplied(t *testing.T) {
	f := fakeUsers(t, map[string]any{
		"id": "u1", "userName": "svc-dashboard@example.invalid", "email": "svc-dashboard@example.invalid",
		"enabled": true, "self": "http://example/user/t123/users/u1",
	})
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	setupRunAgainst(t, f)

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(generatedUserManifest), 0o644))

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, stdout)
	assert.Contains(t, stdout, "generated password ${generate:svc-dashboard-password} not applied (the user already exists)")
}

func TestRunDryRunDoesNotGenerate(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(generatedUserManifest+`tenantOptions:
  - category: integration
    key: api.secret
    value: ${generate:integration-secret}
`), 0o644))
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	require.NoError(t, runErr, stdout)
	assert.NoFileExists(t, filepath.Join(dir, "tenant.secrets.yaml"))
	assert.NotContains(t, stdout, "🔐")
}

func TestRunSavesGeneratedValuesWhenTheRunFails(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	f.JSON("POST /user/t123/users", http.StatusUnprocessableEntity, map[string]any{"error": "undefined/validationError", "message": "Password too weak"})
	setupRunAgainst(t, f)

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(generatedUserManifest), 0o644))

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	assert.ErrorIs(t, runErr, errExitFailures)
	assert.Contains(t, readSecretsFile(t, filepath.Join(dir, "tenant.secrets.yaml")), "svc-dashboard-password",
		"generated values are saved although the run failed")
	assert.Contains(t, stdout, "🔐 Generated 1 value(s)")
}

func TestRunSecretsFlags(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(generatedUserManifest), 0o644))
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")

	assert.ErrorContains(t, runRoot(t, "run", "-f", manifest, "--secrets-encryption", "aes"), `invalid --secrets-encryption "aes"`)

	// --secrets-file is read (an encrypted file needs sops unless none is set)
	custom := filepath.Join(dir, "custom.yaml")
	require.NoError(t, os.WriteFile(custom, []byte(encryptedManifest), 0o600))
	err := runRoot(t, "run", "-f", manifest, "--secrets-file", custom, "--secrets-encryption", "none")
	assert.ErrorContains(t, err, "custom.yaml is encrypted with sops")
}

func TestInitAddsSecretsToGitignore(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("bin/"), 0o644))
	require.NoError(t, runRoot(t, "init", filepath.Join(dir, "tenant.yaml")))

	content, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, "bin/\n*.secrets.yaml\n", string(content))

	// Not added twice
	require.NoError(t, runRoot(t, "init", filepath.Join(dir, "other.yaml")))
	content, err = os.ReadFile(filepath.Join(dir, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(content), "*.secrets.yaml"))
}
