package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManifestPathsFromArgs(t *testing.T) {
	paths, err := manifestPathsFromArgs(nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"tenant.yaml"}, paths)

	paths, err = manifestPathsFromArgs([]string{"base.yaml", "a,b.yaml"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"base.yaml", "a,b.yaml"}, paths, "values are not split on commas")

	paths, err = manifestPathsFromArgs(nil, []string{"base.yaml", "prod.yaml"})
	require.NoError(t, err)
	assert.Equal(t, []string{"base.yaml", "prod.yaml"}, paths)

	_, err = manifestPathsFromArgs([]string{"base.yaml"}, []string{"prod.yaml"})
	assert.ErrorContains(t, err, "either with -f or as arguments, not both")

	_, err = manifestPathsFromArgs([]string{""}, nil)
	assert.Error(t, err)
}

func TestLoadManifestsMergesInOrder(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"common.yaml": "userGroups:\n  - name: operators\n    roles: [ROLE_A]\n",
		"base.yaml": `
include: [common.yaml]
targets:
  allChildren: true
retentionRules:
  - {dataType: ALARM, maximumAge: 30}
software:
  - source: {path: packages}
`,
		"envs/prod.yaml": `
retentionRules:
  - {dataType: ALARM, maximumAge: 90}
userGroups:
  - name: operators
    roles: [ROLE_B]
software:
  - source: {path: packages}
hooks:
  pre:
    - run: echo prod
`,
		"envs/packages/prod_1.0.0_all.deb": "prod",
	})

	base := filepath.Join(dir, "base.yaml")
	prod := filepath.Join(dir, "envs", "prod.yaml")
	manifest, err := LoadManifestsWith([]string{base, prod}, LoadOptions{})
	require.NoError(t, err)

	assert.Equal(t, []string{"common.yaml"}, manifest.IncludedFiles, "only files reached via include are listed")
	require.Len(t, manifest.RetentionRules, 1)
	assert.Equal(t, int64(90), manifest.RetentionRules[0].MaximumAge, "later files win")
	require.Len(t, manifest.UserGroups, 1)
	assert.Equal(t, []string{"ROLE_B"}, manifest.UserGroups[0].Roles)
	require.NotNil(t, manifest.Targets, "sections the later file does not set are kept")
	assert.True(t, manifest.Targets.AllChildren)

	// Relative paths of the second file resolve against its own directory
	require.Len(t, manifest.Software, 2)
	assert.Equal(t, "packages", manifest.Software[0].Source.Path)
	assert.Equal(t, filepath.Join("envs", "packages"), manifest.Software[1].Source.Path)
	resolver := NewSourceResolver(dir, "", true)
	files, err := resolver.Resolve(manifest.Software[1].Source)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "prod_1.0.0_all.deb", files[0].Filename)
	absEnvs, err := filepath.Abs(filepath.Join(dir, "envs"))
	require.NoError(t, err)
	require.Len(t, manifest.Hooks.Pre, 1)
	assert.Equal(t, absEnvs, manifest.Hooks.Pre[0].Dir)

	// A file given twice, or already included, is merged once
	manifest, err = LoadManifestsWith([]string{base, prod, base, filepath.Join(dir, "common.yaml")}, LoadOptions{})
	require.NoError(t, err)
	assert.Len(t, manifest.Software, 2)
	assert.Equal(t, []string{"ROLE_B"}, manifest.UserGroups[0].Roles)

	// Errors name the file
	_, err = LoadManifestsWith([]string{base, filepath.Join(dir, "missing.yaml")}, LoadOptions{})
	assert.ErrorContains(t, err, "missing.yaml")
	writeFiles(t, dir, map[string]string{"bad.yaml": "features:\n  - enabled: true\n"})
	_, err = LoadManifestsWith([]string{base, filepath.Join(dir, "bad.yaml")}, LoadOptions{})
	assert.ErrorContains(t, err, "bad.yaml: invalid manifest:")
}

func TestValidateMultipleManifests(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml": "features:\n  - key: a\n",
		"prod.yaml": "features:\n  - key: b\n",
	})
	base, prod := filepath.Join(dir, "base.yaml"), filepath.Join(dir, "prod.yaml")

	for _, args := range [][]string{
		{"validate", base, prod},
		{"validate", "-f", base, "-f", prod},
	} {
		var runErr error
		stdout, _ := captureStdio(t, func() {
			runErr = runRoot(t, args...)
		})
		require.NoError(t, runErr, args)
		assert.Contains(t, stdout, base+", "+prod+" are valid")
		assert.Contains(t, stdout, "features:                   2 item(s)")
	}

	var err error
	captureStdio(t, func() {
		err = runRoot(t, "validate", "-f", base, prod)
	})
	assert.ErrorContains(t, err, "not both")

	captureStdio(t, func() {
		err = runRoot(t, "validate", base, filepath.Join(dir, "missing.yaml"))
	})
	assert.ErrorContains(t, err, "missing.yaml")
}

func TestRunMultipleManifests(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	setupRunAgainst(t, f)

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml": "hooks:\n  pre:\n    - run: pwd > base.txt\n",
		"envs/prod.yaml": `
hooks:
  pre:
    - run: echo "${generate:prod-secret}" > secret.txt && pwd > prod.txt
`,
	})
	base, prod := filepath.Join(dir, "base.yaml"), filepath.Join(dir, "envs", "prod.yaml")

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", base, prod)
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "📄 Manifest: "+base)
	assert.Contains(t, stdout, "📄 Manifest: "+prod)

	// The secrets file defaults to the first manifest
	secrets := readSecretsFile(t, filepath.Join(dir, "base.secrets.yaml"))
	require.Contains(t, secrets, "prod-secret")
	assert.NoFileExists(t, filepath.Join(dir, "envs", "prod.secrets.yaml"))
	secret, err := os.ReadFile(filepath.Join(dir, "envs", "secret.txt"))
	require.NoError(t, err)
	assert.Equal(t, secrets["prod-secret"].Value, strings.TrimSpace(string(secret)))

	// Hooks run from the directory of the file which declares them
	assert.FileExists(t, filepath.Join(dir, "base.txt"))
	assert.FileExists(t, filepath.Join(dir, "envs", "prod.txt"))
}
