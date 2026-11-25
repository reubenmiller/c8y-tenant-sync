package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withStdin makes a manifest given as "-" read content
func withStdin(t *testing.T, content string) {
	t.Helper()
	resetStdinManifest(strings.NewReader(content))
	t.Cleanup(func() { resetStdinManifest(os.Stdin) })
}

func TestStdinManifestPaths(t *testing.T) {
	assert.True(t, isStdinManifest("-"))
	assert.True(t, isStdinManifest(" - "))
	assert.False(t, isStdinManifest("-.yaml"))
	assert.False(t, isStdinManifest("tenant.yaml"))

	key, err := manifestKey("-")
	require.NoError(t, err)
	assert.Equal(t, "-", key)

	assert.Equal(t, ".", manifestBaseDir("-"))
	assert.Equal(t, "tenant.secrets.yaml", DefaultSecretsPath("-"))
	assert.Equal(t, "<stdin>", manifestDisplayName("-"))
	assert.Equal(t, "tenant.yaml", manifestDisplayName("tenant.yaml"))
}

func TestLoadStdinManifestWithLocalInclude(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.yaml"), []byte("tenantOptions:\n  - {category: a, key: base, value: \"1\"}\n"), 0o600))
	withStdin(t, `include:
  - base.yaml
tenantOptions:
  - {category: a, key: own, value: "2"}
hooks:
  pre:
    - run: echo hi
commands:
  - name: x
    actions: [echo hi]
`)

	// Relative includes are based on the current directory, and hooks and
	// commands are allowed: stdin is not a remote manifest
	manifest, err := LoadManifest("-")
	require.NoError(t, err)
	require.Len(t, manifest.TenantOptions, 2)
	assert.Equal(t, "base", manifest.TenantOptions[0].Key)
	assert.Equal(t, "own", manifest.TenantOptions[1].Key)
	assert.Equal(t, []string{"base.yaml"}, manifest.IncludedFiles)
	require.Len(t, manifest.Hooks.Pre, 1)
	require.Len(t, manifest.Commands, 1)
}

func TestLoadStdinManifestMergedWithFiles(t *testing.T) {
	base := writeManifest(t, "tenantOptions:\n  - {category: a, key: x, value: base}\n")
	withStdin(t, "tenantOptions:\n  - {category: a, key: x, value: stdin}\n")

	// stdin is merged in its position like any other manifest; given twice,
	// it is merged once
	manifest, err := LoadManifestsWith([]string{base, "-", "-"}, LoadOptions{})
	require.NoError(t, err)
	require.Len(t, manifest.TenantOptions, 1)
	assert.Equal(t, "stdin", manifest.TenantOptions[0].Value)
}

func TestLoadStdinManifestErrors(t *testing.T) {
	t.Run("invalid content is labelled <stdin>", func(t *testing.T) {
		base := writeManifest(t, "tenantOptions: []\n")
		withStdin(t, "unknownField: true\n")
		_, err := LoadManifestsWith([]string{base, "-"}, LoadOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "<stdin>: failed to parse manifest")
	})

	t.Run("read error", func(t *testing.T) {
		resetStdinManifest(iotest.ErrReader(errors.New("boom")))
		t.Cleanup(func() { resetStdinManifest(os.Stdin) })
		_, err := LoadManifest("-")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read manifest from stdin: boom")
	})

	t.Run("missing include", func(t *testing.T) {
		t.Chdir(t.TempDir())
		withStdin(t, "include:\n  - missing.yaml\n")
		_, err := LoadManifest("-")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "included file missing.yaml not found")
	})
}

func TestLoadStdinManifestDecryptsSops(t *testing.T) {
	argsFile := fakeSops(t, `cat <<'EOF'
tenantOptions:
  - category: secrets
    key: apiKey
    value: s3cr3t
EOF`)
	withStdin(t, encryptedManifest)

	manifest, err := LoadManifest("-")
	require.NoError(t, err)
	require.Len(t, manifest.TenantOptions, 1)
	assert.Equal(t, "s3cr3t", manifest.TenantOptions[0].Value)

	// sops decrypts a temporary .yaml file with the content of stdin
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	fields := strings.Fields(string(args))
	require.NotEmpty(t, fields)
	assert.Equal(t, ".yaml", filepath.Ext(fields[len(fields)-1]))
}

func TestLoadStdinManifestSopsFailureNamesStdin(t *testing.T) {
	fakeSops(t, `echo "Failed to decrypt $5" >&2; exit 128`)
	withStdin(t, encryptedManifest)

	_, err := LoadManifest("-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "<stdin>")
	assert.NotContains(t, err.Error(), "tenant-sync-")
}

func TestRunValidateStdinManifest(t *testing.T) {
	withStdin(t, "tenantOptions:\n  - {category: a, key: b, value: c}\n")
	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "validate", "-f", "-")
	})
	require.NoError(t, runErr)
	assert.Contains(t, stdout, "✓ <stdin> is valid")
}

func TestBundleRejectsStdinManifest(t *testing.T) {
	err := runBundle("-", filepath.Join(t.TempDir(), "bundle.tar.gz"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle a local manifest file")
}
