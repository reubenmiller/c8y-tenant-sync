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

// writeFiles writes files (relative path -> content) below dir
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

func TestIncludeMergeOrderAndOverride(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml": `
targets:
  allChildren: true
certificateAuthority:
  enabled: true
tenantOptions:
  - {category: base, key: a, value: "1"}
  - {category: base, key: b, value: "1"}
retentionRules:
  - {dataType: ALARM, maximumAge: 30}
userGroups:
  - name: operators
    roles: [ROLE_INVENTORY_READ]
trustedCertificates:
  - source: {path: certs/base.pem}
hooks:
  pre:
    - run: echo base-pre
  sections:
    software:
      pre:
        - run: echo base-software
`,
		"extra.yaml": `
tenantOptions:
  - {category: extra, key: x, value: "1"}
`,
		"tenant.yaml": `
include:
  - base.yaml
  - path: extra.yaml
targets:
  current: true
tenantOptions:
  - {category: base, key: b, value: "2"}
  - {category: own, key: c, value: "3"}
retentionRules:
  - {dataType: ALARM, maximumAge: 90}
userGroups:
  - name: operators
    roles: [ROLE_ALARM_READ]
  - name: viewers
trustedCertificates:
  - source: {path: certs/own.pem}
hooks:
  pre:
    - run: echo own-pre
  sections:
    software:
      pre:
        - run: echo own-software
`,
	})

	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)

	assert.Equal(t, []string{"base.yaml", "extra.yaml"}, manifest.IncludedFiles)
	assert.Nil(t, manifest.Include)

	// Includes first (in order), the later definition of the same option
	// replaces the earlier one in place
	var options []string
	for _, opt := range manifest.TenantOptions {
		options = append(options, opt.Category+"."+opt.Key+"="+opt.Value)
	}
	assert.Equal(t, []string{"base.a=1", "base.b=2", "extra.x=1", "own.c=3"}, options)

	require.Len(t, manifest.RetentionRules, 1)
	assert.Equal(t, int64(90), manifest.RetentionRules[0].MaximumAge)

	require.Len(t, manifest.UserGroups, 2)
	assert.Equal(t, []string{"ROLE_ALARM_READ"}, manifest.UserGroups[0].Roles, "the whole group definition is replaced")
	assert.Equal(t, "viewers", manifest.UserGroups[1].Name)

	// Entries without an identity are concatenated
	require.Len(t, manifest.TrustedCertificates, 2)
	assert.Equal(t, "certs/base.pem", manifest.TrustedCertificates[0].Source.Path)
	assert.Equal(t, "certs/own.pem", manifest.TrustedCertificates[1].Source.Path)

	// Struct sections of the including file replace the included ones
	require.NotNil(t, manifest.Targets)
	assert.False(t, manifest.Targets.AllChildren)
	require.NotNil(t, manifest.Targets.Current)
	require.NotNil(t, manifest.CertificateAuthority, "kept from the include when not overridden")

	// Hooks are concatenated, also per section
	require.Len(t, manifest.Hooks.Pre, 2)
	assert.Equal(t, "echo base-pre", manifest.Hooks.Pre[0].Run)
	assert.Equal(t, "echo own-pre", manifest.Hooks.Pre[1].Run)
	require.Len(t, manifest.Hooks.Sections["software"].Pre, 2)
	assert.Equal(t, "echo base-software", manifest.Hooks.Sections["software"].Pre[0].Run)
	assert.Equal(t, "echo own-software", manifest.Hooks.Sections["software"].Pre[1].Run)
}

func TestIncludeNestedRelativeSourcePaths(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"envs/prod.yaml": `
include: [../shared/common.yaml]
software:
  - source: {path: packages}
`,
		"envs/packages/app_1.0.0_all.deb":         "own",
		"shared/common.yaml":                      "include: [base/base.yaml]\n",
		"shared/base/base.yaml":                   "software:\n  - source: {path: packages, patterns: ['*.deb']}\nsmartrestTemplates:\n  - source: {path: /abs/templates}\n",
		"shared/base/packages/base_2.0.0_all.deb": "base",
	})

	manifestPath := filepath.Join(dir, "envs", "prod.yaml")
	manifest, err := LoadManifest(manifestPath)
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join("..", "shared", "common.yaml"), filepath.Join("..", "shared", "base", "base.yaml")}, manifest.IncludedFiles)

	require.Len(t, manifest.Software, 2)
	assert.Equal(t, filepath.Join("..", "shared", "base", "packages"), manifest.Software[0].Source.Path,
		"relative paths of included files are rewritten relative to the main manifest")
	assert.Equal(t, "packages", manifest.Software[1].Source.Path, "paths of the main manifest are unchanged")
	assert.Equal(t, "/abs/templates", manifest.SmartRestTemplates[0].Source.Path, "absolute paths are unchanged")

	// The source resolves to the file next to the included manifest
	resolver := NewSourceResolver(filepath.Dir(manifestPath), "", true)
	files, err := resolver.Resolve(manifest.Software[0].Source)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "base_2.0.0_all.deb", files[0].Filename)

	files, err = resolver.Resolve(manifest.Software[1].Source)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "app_1.0.0_all.deb", files[0].Filename)
}

func TestIncludeCycle(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "include: [b.yaml]\n",
		"b.yaml":     "include: [sub/c.yaml]\n",
		"sub/c.yaml": "include: [../a.yaml]\n",
	})
	_, err := LoadManifest(filepath.Join(dir, "a.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "include cycle: a.yaml -> b.yaml -> "+filepath.Join("sub", "c.yaml")+" -> a.yaml")

	writeFiles(t, dir, map[string]string{"self.yaml": "include: [self.yaml]\n"})
	_, err = LoadManifest(filepath.Join(dir, "self.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "include cycle: self.yaml -> self.yaml")
}

func TestIncludeSharedFileMergedOnce(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml":   "software:\n  - name: tool\n    version: '1.0'\n",
		"a.yaml":      "include: [base.yaml]\n",
		"b.yaml":      "include: [base.yaml]\n",
		"tenant.yaml": "include: [a.yaml, b.yaml]\n",
	})
	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	assert.Len(t, manifest.Software, 1, "a file reached twice is merged once")
	assert.Equal(t, []string{"a.yaml", "base.yaml", "b.yaml"}, manifest.IncludedFiles)
}

func TestIncludeGlob(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"common/20-groups.yaml": "userGroups:\n  - name: second\n",
		"common/10-groups.yaml": "userGroups:\n  - name: first\n",
		"common/readme.txt":     "not matched",
		"tenant.yaml":           "include: ['common/*.yaml']\nuserGroups:\n  - name: own\n",
		"all.yaml":              "include: ['*.yaml']\nfeatures:\n  - key: f\n",
		"none.yaml":             "include: ['missing/*.yaml']\n",
		"none-optional.yaml":    "include:\n  - path: 'missing/*.yaml'\n    optional: true\n",
		"missing.yaml":          "include: [does-not-exist.yaml]\n",
		"missing-optional.yaml": "include:\n  - {path: does-not-exist.yaml, optional: true}\n",
		"directory.yaml":        "include: [common]\n",
	})

	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	var names []string
	for _, group := range manifest.UserGroups {
		names = append(names, group.Name)
	}
	assert.Equal(t, []string{"first", "second", "own"}, names, "glob matches are merged sorted by name")

	_, err = LoadManifest(filepath.Join(dir, "none.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "none.yaml: include[0]: glob pattern missing/*.yaml matches no files")

	_, err = LoadManifest(filepath.Join(dir, "none-optional.yaml"))
	assert.NoError(t, err)

	_, err = LoadManifest(filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.yaml: include[0]: included file does-not-exist.yaml not found")

	_, err = LoadManifest(filepath.Join(dir, "missing-optional.yaml"))
	assert.NoError(t, err)

	_, err = LoadManifest(filepath.Join(dir, "directory.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "common is a directory")
}

func TestIncludeValidationNamesFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base/rules.yaml":   "retentionRules:\n  - dataType: ALARM\n",
		"tenant.yaml":       "include: [base/rules.yaml]\n",
		"unknown.yaml":      "include: [base/unknown.yaml]\n",
		"base/unknown.yaml": "tenantOption: []\n",
		"own.yaml":          "include: [base/ok.yaml]\nfeatures:\n  - enabled: true\n",
		"base/ok.yaml":      "features:\n  - key: ok\n",
		"bad-entry.yaml":    "include:\n  - {file: base/ok.yaml}\n",
	})

	_, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join("base", "rules.yaml")+": invalid manifest:")
	assert.Contains(t, err.Error(), "retentionRules[0]: maximumAge must be at least 1")

	_, err = LoadManifest(filepath.Join(dir, "unknown.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join("base", "unknown.yaml")+": failed to parse manifest")

	_, err = LoadManifest(filepath.Join(dir, "own.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "own.yaml: invalid manifest:", "errors of a manifest with includes name the file")
	assert.Contains(t, err.Error(), "features[0]: key is required")

	_, err = LoadManifest(filepath.Join(dir, "bad-entry.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field file not found in type include")
}

func TestIncludeExpandsVariablesAndGeneratedValues(t *testing.T) {
	t.Setenv("TS_INCLUDE_REGION", "eu")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"region.yaml": `
tenantOptions:
  - {category: region, key: name, value: "${TS_INCLUDE_REGION}"}
  - {category: region, key: secret, value: "${generate:shared-secret:12}"}
`,
		"tenant.yaml": "include: ['${TS_INCLUDE_REGION}-region.yaml']\n",
	})
	require.NoError(t, os.Rename(filepath.Join(dir, "region.yaml"), filepath.Join(dir, "eu-region.yaml")))

	secrets, err := OpenSecretStore(filepath.Join(dir, "tenant.secrets.yaml"), SecretsEncryptionNone)
	require.NoError(t, err)
	manifest, err := LoadManifestWith(filepath.Join(dir, "tenant.yaml"), LoadOptions{Secrets: secrets})
	require.NoError(t, err)
	require.Len(t, manifest.TenantOptions, 2)
	assert.Equal(t, "eu", manifest.TenantOptions[0].Value)
	assert.Len(t, manifest.TenantOptions[1].Value, 12)
	assert.Contains(t, secrets.Values(), manifest.TenantOptions[1].Value, "generated values of includes use the same secrets store")
}

func TestIncludeSopsEncrypted(t *testing.T) {
	fakeSops(t, `cat <<'EOF'
tenantOptions:
  - category: secrets
    key: apiKey
    value: s3cr3t
EOF`)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"secrets.enc.yaml": encryptedManifest,
		"tenant.yaml":      "include: [secrets.enc.yaml]\nfeatures:\n  - key: f\n",
	})
	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	require.Len(t, manifest.TenantOptions, 1)
	assert.Equal(t, "s3cr3t", manifest.TenantOptions[0].Value)
	assert.Len(t, manifest.Features, 1)
}

func TestIncludeSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("include:\n  - base.yaml\n  - path: 'common/*.yaml'\n    optional: true\n")))
	assert.Error(t, validateYAML(t, schema, []byte("include:\n  - file: base.yaml\n")))
	assert.Error(t, validateYAML(t, schema, []byte("include:\n  - optional: true\n")))
}

func TestRunIncludedHooksAndCommandsUseTheirDirectory(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	setupRunAgainst(t, f)

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"shared/hooks.yaml": `
hooks:
  pre:
    - run: pwd > pre.txt
commands:
  - name: shared
    actions: [pwd > command.txt]
`,
		"tenant.yaml": `
include: [shared/hooks.yaml]
hooks:
  post:
    - run: pwd > post.txt
`,
	})

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", filepath.Join(dir, "tenant.yaml"))
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "includes "+filepath.Join("shared", "hooks.yaml"))

	readDir := func(path string) string {
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		resolved, err := filepath.EvalSymlinks(strings.TrimSpace(string(content)))
		require.NoError(t, err)
		return resolved
	}
	shared, err := filepath.EvalSymlinks(filepath.Join(dir, "shared"))
	require.NoError(t, err)
	root, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, shared, readDir(filepath.Join(dir, "shared", "pre.txt")))
	assert.Equal(t, shared, readDir(filepath.Join(dir, "shared", "command.txt")))
	assert.Equal(t, root, readDir(filepath.Join(dir, "post.txt")))
}

func TestValidateShowsIncludes(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml":   "features:\n  - key: f\n",
		"tenant.yaml": "include: [base.yaml]\n",
	})
	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "validate", filepath.Join(dir, "tenant.yaml"))
	})
	require.NoError(t, runErr)
	assert.Contains(t, stdout, "includes base.yaml")
}
