package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestParseGenerateRef(t *testing.T) {
	ref, err := parseGenerateRef("svc-password")
	require.NoError(t, err)
	assert.Equal(t, generateRef{Name: "svc-password", Length: defaultGenerateLength}, ref)

	ref, err = parseGenerateRef("api.key_1:32")
	require.NoError(t, err)
	assert.Equal(t, generateRef{Name: "api.key_1", Length: 32}, ref)

	for value, wantErr := range map[string]string{
		"":          "invalid name",
		"-leading":  "invalid name",
		"has space": "invalid name",
		"a/b":       "invalid name",
		"pw:abc":    "invalid length",
		"pw:8":      "invalid length",
		"pw:500":    "invalid length",
	} {
		_, err := parseGenerateRef(value)
		assert.ErrorContains(t, err, wantErr, value)
	}
}

func TestGenerateValue(t *testing.T) {
	allowed := generateLower + generateUpper + generateDigits + generateSpecial
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		value, err := generateValue(defaultGenerateLength)
		require.NoError(t, err)
		require.Len(t, value, defaultGenerateLength)
		assert.False(t, seen[value], "values are random")
		seen[value] = true

		assert.True(t, strings.ContainsAny(value[:1], generateLower+generateUpper), "starts with a letter: %s", value)
		for _, set := range []string{generateLower, generateUpper, generateDigits, generateSpecial} {
			assert.True(t, strings.ContainsAny(value, set), "contains a character of %q: %s", set, value)
		}
		for _, c := range value {
			assert.True(t, strings.ContainsRune(allowed, c), "allowed character %q in %s", c, value)
		}

		// The value is inserted into the manifest text before parsing, so it
		// must survive YAML unchanged in every context
		for _, doc := range []string{"v: %s\n", "v: \"%s\"\n", "v: '%s'\n", "v: [%s]\n", "v: {k: %s}\n", "v:\n  - %s\n"} {
			var parsed map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(strings.Replace(doc, "%s", value, 1)), &parsed), value)
			var got any = parsed["v"]
			switch v := got.(type) {
			case []any:
				got = v[0]
			case map[string]any:
				got = v["k"]
			}
			assert.Equal(t, value, got, "YAML round trip of %s in %q", value, doc)
		}
	}

	short, err := generateValue(4)
	require.NoError(t, err)
	assert.Len(t, short, minGenerateLength, "the minimum length is enforced")
}

func TestDefaultSecretsPath(t *testing.T) {
	assert.Equal(t, "tenant.secrets.yaml", DefaultSecretsPath("tenant.yaml"))
	assert.Equal(t, filepath.Join("dir", "prod.secrets.yaml"), DefaultSecretsPath(filepath.Join("dir", "prod.yml")))
}

func TestSecretStoreGenerateReuseAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant.secrets.yaml")
	store, err := OpenSecretStore(path, SecretsEncryptionNone)
	require.NoError(t, err)
	store.Host = "https://example.c8y.io"
	store.Tenant = "t12345"

	first, err := store.Value(generateRef{Name: "pw", Length: 16})
	require.NoError(t, err)
	assert.Len(t, first, 16)
	again, err := store.Value(generateRef{Name: "pw", Length: 16})
	require.NoError(t, err)
	assert.Equal(t, first, again, "the same name gets the same value")

	assert.Equal(t, []string{"pw"}, store.Generated())
	name, ok := store.IsGenerated(first)
	assert.True(t, ok)
	assert.Equal(t, "pw", name)

	encrypted, err := store.Save()
	require.NoError(t, err)
	assert.False(t, encrypted)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The next run reuses the stored value
	reopened, err := OpenSecretStore(path, SecretsEncryptionNone)
	require.NoError(t, err)
	value, err := reopened.Value(generateRef{Name: "pw", Length: 16})
	require.NoError(t, err)
	assert.Equal(t, first, value)
	assert.Empty(t, reopened.Generated(), "nothing new was generated")
	_, ok = reopened.IsGenerated(first)
	assert.False(t, ok)
	name, ok = reopened.NameOf(first)
	assert.True(t, ok)
	assert.Equal(t, "pw", name)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "host: https://example.c8y.io")
	assert.Contains(t, string(raw), "tenant: t12345")
	assert.Contains(t, string(raw), "generatedAt:")

	// Nothing generated: the file is not rewritten
	before, _ := os.Stat(path)
	_, err = reopened.Save()
	require.NoError(t, err)
	after, _ := os.Stat(path)
	assert.Equal(t, before.ModTime(), after.ModTime())
}

func TestSecretStoreSetTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant.secrets.yaml")
	require.NoError(t, os.WriteFile(path, []byte("secrets:\n  old:\n    value: Old-Value-123\n    generatedAt: \"2026-01-01T00:00:00Z\"\n    tenant: t1\n"), 0o600))
	store, err := OpenSecretStore(path, SecretsEncryptionNone)
	require.NoError(t, err)

	// The tenant is not known yet when the value is generated
	_, err = store.Value(generateRef{Name: "new", Length: 16})
	require.NoError(t, err)
	store.SetTenant("t2")
	_, err = store.Save()
	require.NoError(t, err)

	reopened, err := OpenSecretStore(path, SecretsEncryptionNone)
	require.NoError(t, err)
	assert.Equal(t, "t2", reopened.entries["new"].Tenant, "values generated in this run get the tenant")
	assert.Equal(t, "t1", reopened.entries["old"].Tenant, "stored values keep their tenant")
}

func TestSecretStoreDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant.secrets.yaml")
	store, err := OpenSecretStore(path, SecretsEncryptionNone)
	require.NoError(t, err)
	store.DryRun = true

	value, err := store.Value(generateRef{Name: "pw", Length: 24})
	require.NoError(t, err)
	assert.Equal(t, "<generated:pw>", value)
	_, err = store.Save()
	require.NoError(t, err)
	assert.NoFileExists(t, path, "a dry run writes nothing")
}

func TestExpandManifestGenerate(t *testing.T) {
	// Without a store, references are only checked
	expanded, err := expandManifest("password: ${generate:pw}\n", nil)
	require.NoError(t, err)
	assert.Equal(t, "password: ${generate:pw}\n", expanded)

	_, err = expandManifest("a: ${generate:bad name}\nb: ${generate:pw:3}\n", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid name "bad name"`)
	assert.Contains(t, err.Error(), `invalid length "3"`)

	store, err := OpenSecretStore(filepath.Join(t.TempDir(), "s.yaml"), SecretsEncryptionNone)
	require.NoError(t, err)
	expanded, err = expandManifest("# ${generate:in-a-comment}\na: ${generate:pw}\nb: ${generate:pw}\nc: $${generate:pw}\nd: prefix-${generate:other:16}\n", store)
	require.NoError(t, err)
	value, _ := store.Value(generateRef{Name: "pw"})
	other, _ := store.Value(generateRef{Name: "other", Length: 16})
	var parsed map[string]string
	require.NoError(t, yaml.Unmarshal([]byte(expanded), &parsed))
	assert.Equal(t, map[string]string{
		"a": value,
		"b": value,
		"c": "${generate:pw}",
		"d": "prefix-" + other,
	}, parsed, "$$ escapes a reference; references can be part of a value")
	assert.Equal(t, []string{"other", "pw"}, store.Generated(), "references in comments don't generate values")
}

func TestLoadManifestWithGeneratedPassword(t *testing.T) {
	path := writeManifest(t, `users:
  - email: svc@example.invalid
    password: ${generate:svc-password:20}
tenantOptions:
  - category: integration
    key: api.secret
    value: ${generate:integration-secret}
`)
	// validate: the references are checked and left in place
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, "${generate:svc-password:20}", manifest.Users[0].Password)

	store, err := OpenSecretStore(filepath.Join(t.TempDir(), "s.yaml"), SecretsEncryptionNone)
	require.NoError(t, err)
	manifest, err = LoadManifestWith(path, LoadOptions{Secrets: store})
	require.NoError(t, err)
	assert.Len(t, manifest.Users[0].Password, 20)
	assert.Len(t, manifest.TenantOptions[0].Value, defaultGenerateLength)
	assert.Equal(t, []string{"integration-secret", "svc-password"}, store.Generated())

	_, err = LoadManifest(writeManifest(t, "users:\n  - email: a@example.com\n    password: ${generate:bad name}\n"))
	assert.ErrorContains(t, err, `invalid name "bad name"`)
}

func TestSecretStoreEncryptionModes(t *testing.T) {
	// sops missing: auto falls back to a plain file, sops fails
	t.Setenv(EnvSopsBinary, filepath.Join(t.TempDir(), "no-sops"))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules: []\n"), 0o644))

	auto, err := OpenSecretStore(filepath.Join(dir, "auto.secrets.yaml"), SecretsEncryptionAuto)
	require.NoError(t, err)
	_, _ = auto.Value(generateRef{Name: "pw", Length: 16})
	encrypted, err := auto.Save()
	require.NoError(t, err)
	assert.False(t, encrypted)

	required, err := OpenSecretStore(filepath.Join(dir, "required.secrets.yaml"), SecretsEncryptionSops)
	require.NoError(t, err)
	_, _ = required.Value(generateRef{Name: "pw", Length: 16})
	_, err = required.Save()
	assert.ErrorContains(t, err, "--secrets-encryption sops requires the sops executable")

	// An encrypted secrets file can't be used with encryption none
	encryptedPath := filepath.Join(dir, "enc.secrets.yaml")
	require.NoError(t, os.WriteFile(encryptedPath, []byte(encryptedManifest), 0o600))
	_, err = OpenSecretStore(encryptedPath, SecretsEncryptionNone)
	assert.ErrorContains(t, err, "is encrypted with sops, but --secrets-encryption is none")
}

func TestFindSopsConfig(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	assert.Equal(t, "", findSopsConfig(nested))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".sops.yaml"), []byte("creation_rules: []\n"), 0o644))
	assert.Equal(t, filepath.Join(root, ".sops.yaml"), mustEvalSymlinks(t, findSopsConfig(nested), root))
}

// mustEvalSymlinks resolves symlinks (e.g. /var -> /private/var on macOS)
// in got, relative to the expected root
func mustEvalSymlinks(t *testing.T, got, root string) string {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	resolvedGot, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	return filepath.Join(root, strings.TrimPrefix(resolvedGot, resolvedRoot))
}

// newAgeKey creates an age key pair and returns the key file and recipient
// (skips the test when sops or age-keygen are not installed)
func newAgeKey(t *testing.T) (keyFile, recipient string) {
	t.Helper()
	if _, err := exec.LookPath("sops"); err != nil {
		t.Skip("sops is not installed")
	}
	keygen, err := exec.LookPath("age-keygen")
	if err != nil {
		t.Skip("age-keygen is not installed")
	}
	keyFile = filepath.Join(t.TempDir(), "key.txt")
	out, err := exec.Command(keygen, "-o", keyFile).CombinedOutput()
	require.NoError(t, err, string(out))
	key, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	for _, line := range strings.Split(string(key), "\n") {
		if strings.HasPrefix(line, "# public key: ") {
			recipient = strings.TrimPrefix(line, "# public key: ")
		}
	}
	require.NotEmpty(t, recipient)
	return keyFile, recipient
}

func TestSecretStoreSopsRoundTrip(t *testing.T) {
	keyFile, recipient := newAgeKey(t)
	t.Setenv(EnvSopsBinary, "")
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	dir := t.TempDir()
	sopsConfig := "creation_rules:\n  - path_regex: \\.secrets\\.yaml$\n    age: " + recipient + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(sopsConfig), 0o644))
	path := filepath.Join(dir, "tenant.secrets.yaml")

	store, err := OpenSecretStore(path, SecretsEncryptionAuto)
	require.NoError(t, err)
	value, err := store.Value(generateRef{Name: "pw", Length: 24})
	require.NoError(t, err)
	encrypted, err := store.Save()
	require.NoError(t, err)
	assert.True(t, encrypted, "auto encrypts when sops and a .sops.yaml are found")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, isSopsEncrypted(raw))
	assert.NotContains(t, string(raw), value, "the value is encrypted")

	// The next run decrypts the file and reuses the value
	reopened, err := OpenSecretStore(path, SecretsEncryptionAuto)
	require.NoError(t, err)
	again, err := reopened.Value(generateRef{Name: "pw", Length: 24})
	require.NoError(t, err)
	assert.Equal(t, value, again)

	// Adding a value keeps the file encrypted
	_, err = reopened.Value(generateRef{Name: "second", Length: 16})
	require.NoError(t, err)
	encrypted, err = reopened.Save()
	require.NoError(t, err)
	assert.True(t, encrypted)
	final, err := OpenSecretStore(path, SecretsEncryptionSops)
	require.NoError(t, err)
	assert.Len(t, final.Values(), 2)

	// none never encrypts
	plainPath := filepath.Join(dir, "plain.secrets.yaml")
	plain, err := OpenSecretStore(plainPath, SecretsEncryptionNone)
	require.NoError(t, err)
	plainValue, _ := plain.Value(generateRef{Name: "pw", Length: 16})
	encrypted, err = plain.Save()
	require.NoError(t, err)
	assert.False(t, encrypted)
	raw, err = os.ReadFile(plainPath)
	require.NoError(t, err)
	assert.Contains(t, string(raw), plainValue)
}

func TestSecretStoreEncryptionFailureKeepsValues(t *testing.T) {
	_, _ = newAgeKey(t)
	t.Setenv(EnvSopsBinary, "")

	dir := t.TempDir()
	// A creation rule with an invalid recipient: sops fails to encrypt
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte("creation_rules:\n  - age: not-a-valid-recipient\n"), 0o644))
	path := filepath.Join(dir, "tenant.secrets.yaml")

	store, err := OpenSecretStore(path, SecretsEncryptionAuto)
	require.NoError(t, err)
	value, _ := store.Value(generateRef{Name: "pw", Length: 16})
	_, err = store.Save()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saved unencrypted")

	// The value is not lost
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), value)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
