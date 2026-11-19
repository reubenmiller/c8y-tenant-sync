package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretRefValidation(t *testing.T) {
	path := writeManifest(t, `tenantOptions:
  - category: a
    key: from-env
    valueFrom:
      env: INTEGRATION_SECRET
  - category: a
    key: generated
    valueFrom:
      generate: integration-token:32
  - category: a
    key: app
    valueFrom:
      application: devicemanagement
users:
  - email: svc@example.invalid
    passwordFrom:
      generate: svc-password
software:
  - source:
      github:
        repo: owner/private
        tokenFrom:
          env: MY_GITHUB_TOKEN
`)
	_, err := LoadManifest(path)
	require.NoError(t, err)

	path = writeManifest(t, `tenantOptions:
  - category: a
    key: both
    valueFrom:
      env: A
      generate: b
  - category: a
    key: app-and-env
    valueFrom:
      application: x
      env: A
  - category: a
    key: bad-env
    valueFrom:
      env: "1-invalid"
  - category: a
    key: bad-generate
    valueFrom:
      generate: "bad name"
users:
  - email: a@example.invalid
    password: literal
    passwordFrom:
      env: PW
  - email: b@example.invalid
    passwordFrom: {}
software:
  - source:
      github:
        repo: owner/private
        token: literal
        tokenFrom:
          env: T
`)
	_, err = LoadManifest(path)
	require.Error(t, err)
	for _, want := range []string{
		"tenantOptions[0]: valueFrom requires exactly one of: application, device, env, generate",
		"tenantOptions[1]: valueFrom requires exactly one of: application, device, env, generate",
		`tenantOptions[2].valueFrom.env: invalid environment variable name "1-invalid"`,
		`tenantOptions[3].valueFrom.generate: invalid name "bad name"`,
		"users[0]: password and passwordFrom are mutually exclusive",
		"users[1].passwordFrom requires one of: env, generate",
		"software[0]: github.token and github.tokenFrom are mutually exclusive",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestResolveSecretRefs(t *testing.T) {
	t.Setenv("TS_INTEGRATION_SECRET", "Env-Secret-Value")
	t.Setenv("TS_GITHUB_TOKEN", "ghp_example_token")
	path := writeManifest(t, `tenantOptions:
  - category: a
    key: from-env
    valueFrom:
      env: TS_INTEGRATION_SECRET
  - category: a
    key: generated
    valueFrom:
      generate: integration-token:32
  - category: a
    key: app
    valueFrom:
      application: devicemanagement
users:
  - email: svc@example.invalid
    passwordFrom:
      generate: svc-password
  - email: other@example.invalid
    passwordFrom:
      generate: svc-password
software:
  - source:
      github:
        repo: owner/private
        tokenFrom:
          env: TS_GITHUB_TOKEN
applications:
  - name: my-service
    source:
      github:
        repo: owner/service
        tokenFrom:
          env: TS_GITHUB_TOKEN
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	store, err := OpenSecretStore(filepath.Join(t.TempDir(), "s.yaml"), SecretsEncryptionNone)
	require.NoError(t, err)

	values, err := manifest.ResolveSecretRefs(store)
	require.NoError(t, err)

	assert.Equal(t, "Env-Secret-Value", manifest.TenantOptions[0].Value)
	assert.Nil(t, manifest.TenantOptions[0].ValueFrom)
	generated, _ := store.Value(generateRef{Name: "integration-token", Length: 32})
	assert.Len(t, generated, 32)
	assert.Equal(t, generated, manifest.TenantOptions[1].Value)
	assert.Equal(t, "devicemanagement", manifest.TenantOptions[2].ValueFrom.Application, "application references are resolved when applying")
	assert.Empty(t, manifest.TenantOptions[2].Value)

	password, _ := store.Value(generateRef{Name: "svc-password"})
	assert.Equal(t, password, manifest.Users[0].Password)
	assert.Equal(t, password, manifest.Users[1].Password, "the same name gets the same value")
	assert.Nil(t, manifest.Users[0].PasswordFrom)

	assert.Equal(t, "ghp_example_token", manifest.Software[0].Source.GitHub.Token)
	assert.Nil(t, manifest.Software[0].Source.GitHub.TokenFrom)
	assert.Equal(t, "ghp_example_token", manifest.Applications[0].Source.GitHub.Token)

	assert.ElementsMatch(t, []string{"Env-Secret-Value", generated, password, password, "ghp_example_token", "ghp_example_token"}, values,
		"all resolved values are returned for masking")
	assert.ElementsMatch(t, []string{"integration-token", "svc-password"}, store.Generated())
}

func TestResolveSecretRefsMissingEnv(t *testing.T) {
	os.Unsetenv("TS_MISSING")
	t.Setenv("TS_EMPTY", "")
	path := writeManifest(t, `tenantOptions:
  - category: a
    key: missing
    valueFrom:
      env: TS_MISSING
users:
  - email: svc@example.invalid
    passwordFrom:
      env: TS_EMPTY
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err, "validate does not need the variables")

	_, err = manifest.ResolveSecretRefs(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenantOptions[0].valueFrom: environment variable TS_MISSING is not set or empty")
	assert.Contains(t, err.Error(), "users[0].passwordFrom: environment variable TS_EMPTY is not set or empty")

	// generate needs a secrets store
	manifest, err = LoadManifest(writeManifest(t, "users:\n  - email: a@example.invalid\n    passwordFrom:\n      generate: pw\n"))
	require.NoError(t, err)
	_, err = manifest.ResolveSecretRefs(nil)
	assert.ErrorContains(t, err, "generated value pw requires a secrets file")
}

func TestSecretRefSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("tenantOptions:\n  - category: a\n    key: b\n    valueFrom:\n      env: MY_SECRET\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("users:\n  - email: a@example.invalid\n    passwordFrom:\n      generate: pw\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("software:\n  - source:\n      github:\n        repo: o/r\n        tokenFrom:\n          env: T\n")))
	assert.Error(t, validateYAML(t, schema, []byte("tenantOptions:\n  - category: a\n    key: b\n    valueFrom:\n      env: A\n      generate: b\n")))
	assert.Error(t, validateYAML(t, schema, []byte("tenantOptions:\n  - category: a\n    key: b\n    valueFrom:\n      application: x\n      env: A\n")))
	assert.Error(t, validateYAML(t, schema, []byte("users:\n  - email: a@example.invalid\n    passwordFrom: {}\n")))
}

func TestRunResolvesSecretRefsAndMasksThem(t *testing.T) {
	f := fakeUsers(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	f.JSON("POST /tenant/options", http.StatusCreated, map[string]any{})
	setupRunAgainst(t, f)
	t.Setenv("TS_INTEGRATION_SECRET", "Env-Secret-Value-42")

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`tenantOptions:
  - category: integration
    key: api.secret
    valueFrom:
      env: TS_INTEGRATION_SECRET
users:
  - email: svc@example.invalid
    passwordFrom:
      generate: svc-password
`), 0o644))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--debug")
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)

	password := readSecretsFile(t, filepath.Join(dir, "tenant.secrets.yaml"))["svc-password"].Value
	require.NotEmpty(t, password)

	options := f.Requests(http.MethodPost, "/tenant/options")
	require.Len(t, options, 1)
	assert.Equal(t, "Env-Secret-Value-42", options[0].Body["value"], "the option is set to the environment variable")
	users := f.Requests(http.MethodPost, "/user/t123/users")
	require.Len(t, users, 1)
	assert.Equal(t, password, users[0].Body["password"])

	for _, secret := range []string{"Env-Secret-Value-42", password} {
		assert.NotContains(t, stdout, secret)
		assert.NotContains(t, stderr, secret, "masked in the --debug request bodies")
	}
	assert.Contains(t, stdout, "initial password from ${generate:svc-password}")
}

func TestRunFailsOnMissingEnvRef(t *testing.T) {
	f := newFakeC8y(t)
	setupRunAgainst(t, f)
	os.Unsetenv("TS_NOT_SET")

	manifest := writeManifest(t, "users:\n  - email: svc@example.invalid\n    passwordFrom:\n      env: TS_NOT_SET\n")
	var runErr error
	captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	assert.ErrorContains(t, runErr, "users[0].passwordFrom: environment variable TS_NOT_SET is not set or empty")
	assert.Empty(t, f.Requests(http.MethodPost, "/"), "nothing is applied")

	// Also in a dry run
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")
	captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest)
	})
	assert.ErrorContains(t, runErr, "TS_NOT_SET is not set or empty")
}

// TestSopsKeepsSecretRefsReadable encrypts a manifest with an encrypted_regex
// on the value fields: literal secrets are encrypted, the references stay
// readable, and the manifest loads with the decrypted values
func TestSopsKeepsSecretRefsReadable(t *testing.T) {
	keyFile, recipient := newAgeKey(t)
	sops, _ := exec.LookPath("sops")
	t.Setenv(EnvSopsBinary, "")
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.yaml")
	require.NoError(t, os.WriteFile(plain, []byte(`tenantOptions:
  - category: integration
    key: literal
    value: Literal-Secret-1
  - category: integration
    key: referenced
    valueFrom:
      env: TS_REFERENCED
users:
  - email: svc@example.invalid
    passwordFrom:
      generate: svc-password
`), 0o644))
	encrypt := exec.Command(sops, "--encrypt", "--age", recipient, "--encrypted-regex", "^(value|password|token)$",
		"--input-type", "yaml", "--output-type", "yaml", plain)
	encrypt.Dir = dir
	encrypted, err := encrypt.Output()
	require.NoError(t, err)

	text := string(encrypted)
	assert.NotContains(t, text, "Literal-Secret-1", "literal secrets are encrypted")
	assert.Contains(t, text, "env: TS_REFERENCED", "references stay readable")
	assert.Contains(t, text, "generate: svc-password")
	assert.Equal(t, 1, strings.Count(text, "ENC["+"AES256_GCM")-strings.Count(text, "mac: ENC["), "only the literal value is encrypted")

	path := filepath.Join(dir, "tenant.enc.yaml")
	require.NoError(t, os.WriteFile(path, encrypted, 0o644))
	t.Setenv("TS_REFERENCED", "From-Env-2")
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	_, err = manifest.ResolveSecretRefs(&SecretStore{Path: filepath.Join(dir, "s.yaml"), entries: map[string]SecretEntry{}})
	require.NoError(t, err)
	assert.Equal(t, "Literal-Secret-1", manifest.TenantOptions[0].Value)
	assert.Equal(t, "From-Env-2", manifest.TenantOptions[1].Value)
	assert.NotEmpty(t, manifest.Users[0].Password)
}
