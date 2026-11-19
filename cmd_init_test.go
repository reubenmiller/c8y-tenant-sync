package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runRoot(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestInitCreatesPackagesFolder(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "sub", "tenant.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(manifest), 0o755))

	require.NoError(t, runRoot(t, "init", manifest))

	// Created next to the manifest, not in the working directory
	info, err := os.Stat(filepath.Join(dir, "sub", packagesFolder))
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// The new manifest runs as is
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")
	assert.NoError(t, runRoot(t, "run", "-f", manifest))
}

func TestInitKeepsExistingPackagesFolder(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, packagesFolder, "mypkg_1.0.0_arm64.deb")
	require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
	require.NoError(t, os.WriteFile(existing, []byte("x"), 0o644))

	require.NoError(t, runRoot(t, "init", filepath.Join(dir, "tenant.yaml")))
	assert.FileExists(t, existing)
}

func TestInitFailsWhenPackagesIsAFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, packagesFolder), []byte("x"), 0o644))

	err := runRoot(t, "init", filepath.Join(dir, "tenant.yaml"))
	assert.ErrorContains(t, err, "a file with that name exists")
}

func TestInitFullDoesNotCreatePackagesFolder(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, runRoot(t, "init", "--full", filepath.Join(dir, "tenant.yaml")))
	assert.NoDirExists(t, filepath.Join(dir, packagesFolder))
}
