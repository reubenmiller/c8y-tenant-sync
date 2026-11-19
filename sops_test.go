package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const encryptedManifest = `tenantOptions:
    - category: ENC[AES256_GCM,data:abc,iv:x,tag:y,type:str]
      key: ENC[AES256_GCM,data:def,iv:x,tag:y,type:str]
      value: ENC[AES256_GCM,data:ghi,iv:x,tag:y,type:str]
sops:
    age:
        - recipient: age1example
    lastmodified: "2026-11-19T00:00:00Z"
    mac: ENC[AES256_GCM,data:mac,iv:x,tag:y,type:str]
    version: 3.9.0
`

// fakeSops installs a fake sops script (via TENANT_SYNC_SOPS) which records
// its arguments and runs the given shell body
func fakeSops(t *testing.T, body string) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake sops script needs a POSIX shell")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := filepath.Join(dir, "sops")
	content := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n" + body + "\n"
	require.NoError(t, os.WriteFile(script, []byte(content), 0o755))
	t.Setenv(EnvSopsBinary, script)
	return argsFile
}

func TestIsSopsEncrypted(t *testing.T) {
	assert.True(t, isSopsEncrypted([]byte(encryptedManifest)))
	assert.False(t, isSopsEncrypted([]byte("tenantOptions: []\n")))
	assert.False(t, isSopsEncrypted([]byte("sops: {}\n")), "a sops section without a MAC is not encrypted")
	assert.False(t, isSopsEncrypted([]byte("- not\n- a mapping\n")))
	assert.False(t, isSopsEncrypted([]byte(":\tinvalid")))
}

func TestLoadManifestDecryptsSops(t *testing.T) {
	argsFile := fakeSops(t, `cat <<'EOF'
tenantOptions:
  - category: secrets
    key: apiKey
    value: s3cr3t-${TS_REGION}
EOF`)
	t.Setenv("TS_REGION", "eu")

	path := writeManifest(t, encryptedManifest)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	require.Len(t, manifest.TenantOptions, 1)
	assert.Equal(t, "s3cr3t-eu", manifest.TenantOptions[0].Value, "environment variables are expanded after decryption")

	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Equal(t, "--decrypt --input-type yaml --output-type yaml "+path, strings.TrimSpace(string(args)))
}

func TestLoadManifestPlainDoesNotCallSops(t *testing.T) {
	t.Setenv(EnvSopsBinary, filepath.Join(t.TempDir(), "does-not-exist"))
	path := writeManifest(t, "features:\n  - key: f\n")
	_, err := LoadManifest(path)
	assert.NoError(t, err)
}

func TestLoadManifestSopsFailure(t *testing.T) {
	fakeSops(t, `echo "Failed to get the data key required to decrypt the SOPS file." >&2; exit 128`)

	_, err := LoadManifest(writeManifest(t, encryptedManifest))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to decrypt")
	assert.Contains(t, err.Error(), "Failed to get the data key", "the sops error is shown")
}

func TestLoadManifestSopsMissing(t *testing.T) {
	t.Setenv(EnvSopsBinary, filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := LoadManifest(writeManifest(t, encryptedManifest))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sops executable was not found")
	assert.Contains(t, err.Error(), EnvSopsBinary)
}

func TestSchemaAcceptsSopsMetadata(t *testing.T) {
	assert.NoError(t, validateYAML(t, compileSchema(t), []byte(encryptedManifest)))
}

// TestSopsAgeRoundTrip encrypts a manifest with the real sops and age tools
// (skipped when they are not installed)
func TestSopsAgeRoundTrip(t *testing.T) {
	sops, err := exec.LookPath("sops")
	if err != nil {
		t.Skip("sops is not installed")
	}
	keygen, err := exec.LookPath("age-keygen")
	if err != nil {
		t.Skip("age-keygen is not installed")
	}

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	out, err := exec.Command(keygen, "-o", keyFile).CombinedOutput()
	require.NoError(t, err, string(out))
	key, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	var recipient string
	for _, line := range strings.Split(string(key), "\n") {
		if strings.HasPrefix(line, "# public key: ") {
			recipient = strings.TrimPrefix(line, "# public key: ")
		}
	}
	require.NotEmpty(t, recipient)

	plain := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(plain, []byte(`tenantOptions:
  - category: secrets
    key: apiKey
    value: s3cr3t
users:
  - userName: jdoe
    password: ${TS_DEFAULT_PASSWORD}
`), 0o644))

	encrypt := exec.Command(sops, "--encrypt", "--age", recipient, "--encrypted-regex", "^(value|password)$",
		"--input-type", "yaml", "--output-type", "yaml", plain)
	encrypted, err := encrypt.Output()
	require.NoError(t, err)
	assert.NotContains(t, string(encrypted), "s3cr3t", "the secret is encrypted")
	assert.Contains(t, string(encrypted), "apiKey", "values outside encrypted_regex stay readable")
	path := filepath.Join(dir, "tenant.sops.yaml")
	require.NoError(t, os.WriteFile(path, encrypted, 0o644))

	t.Setenv(EnvSopsBinary, "")
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	t.Setenv("TS_DEFAULT_PASSWORD", "from-env")
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", manifest.TenantOptions[0].Value)
	assert.Equal(t, "from-env", manifest.Users[0].Password)

	// Without the key, decryption fails with the sops error
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(dir, "missing.txt"))
	_, err = LoadManifest(path)
	assert.ErrorContains(t, err, "failed to decrypt")
}
