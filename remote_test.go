package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// manifestServer serves files (URL path -> content) and records the
// Authorization headers it received
type manifestServer struct {
	*httptest.Server
	mu    sync.Mutex
	auth  map[string]string
	files map[string]string
}

func newManifestServer(t *testing.T, files map[string]string) *manifestServer {
	t.Helper()
	s := &manifestServer{files: files, auth: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auth[r.URL.Path] = r.Header.Get("Authorization")
		s.mu.Unlock()
		content, ok := s.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestIsRemoteManifest(t *testing.T) {
	assert.True(t, isRemoteManifest("https://example.com/tenant.yaml"))
	assert.True(t, isRemoteManifest("HTTP://example.com/tenant.yaml"))
	assert.False(t, isRemoteManifest("tenant.yaml"))
	assert.False(t, isRemoteManifest("/abs/https/tenant.yaml"))
	assert.False(t, isRemoteManifest("file:///tenant.yaml"))
}

func TestRemoteManifestNameAndSecretsPath(t *testing.T) {
	assert.Equal(t, "prod.yaml", remoteManifestName("https://example.com/sync/prod.yaml?ref=main"))
	assert.Equal(t, "tenant.yaml", remoteManifestName("https://example.com/"))
	assert.Equal(t, "prod.secrets.yaml", DefaultSecretsPath("https://example.com/sync/prod.yaml"))
	assert.Equal(t, ".", manifestBaseDir("https://example.com/sync/prod.yaml"))
}

func TestLoadRemoteManifestWithRelativeIncludes(t *testing.T) {
	server := newManifestServer(t, map[string]string{
		"/repo/tenant.yaml": `
include:
  - base/common.yaml
  - path: missing.yaml
    optional: true
tenantOptions:
  - {category: own, key: a, value: "1"}
`,
		"/repo/base/common.yaml": `
include:
  - ../shared.yaml
tenantOptions:
  - {category: common, key: a, value: "1"}
`,
		"/repo/shared.yaml": `
tenantOptions:
  - {category: shared, key: a, value: "1"}
`,
	})

	manifest, err := LoadManifest(server.URL + "/repo/tenant.yaml")
	require.NoError(t, err)

	var categories []string
	for _, option := range manifest.TenantOptions {
		categories = append(categories, option.Category)
	}
	assert.Equal(t, []string{"shared", "common", "own"}, categories)
	assert.Equal(t, []string{
		server.URL + "/repo/base/common.yaml",
		server.URL + "/repo/shared.yaml",
	}, manifest.IncludedFiles)
}

func TestLocalManifestIncludesURL(t *testing.T) {
	server := newManifestServer(t, map[string]string{
		"/base.yaml": `
retentionRules:
  - {dataType: ALARM, maximumAge: 30}
`,
	})
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"tenant.yaml": "include:\n  - " + server.URL + "/base.yaml\n  - path: " + server.URL + "/missing.yaml\n    optional: true\n",
	})

	manifest, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	require.Len(t, manifest.RetentionRules, 1)
	assert.Equal(t, []string{server.URL + "/base.yaml"}, manifest.IncludedFiles)
}

func TestRemoteManifestErrors(t *testing.T) {
	server := newManifestServer(t, map[string]string{
		"/glob.yaml":     "include:\n  - '*.yaml'\n",
		"/local.yaml":    "include:\n  - /etc/tenant.yaml\n",
		"/path.yaml":     "software:\n  - source: {path: ./packages}\n",
		"/hooks.yaml":    "hooks:\n  pre:\n    - run: echo hi\n",
		"/commands.yaml": "commands:\n  - name: x\n    actions: [echo hi]\n",
		"/nested.yaml":   "include:\n  - path: inner.yaml\n    optional: true\n",
		"/inner.yaml":    "include:\n  - gone.yaml\n",
		"/sf.yaml":       "smartFunctions:\n  - name: r\n    input: {topicPattern: 'x/*'}\n    codeFile: rule.js\n",
		"/dtm.yaml":      "digitalTwin:\n  propertyDefinitions:\n    - source: {path: ./dtm}\n",
	})

	cases := map[string]string{
		"/missing.yaml":  "404",
		"/glob.yaml":     "glob pattern *.yaml is not supported in a remote manifest",
		"/local.yaml":    "a remote manifest can only include relative paths or http(s) urls",
		"/path.yaml":     `source.path "./packages" is not supported in a remote manifest`,
		"/hooks.yaml":    "--allow-remote-commands",
		"/commands.yaml": "--allow-remote-commands",
		"/sf.yaml":       `smartFunctions[0].codeFile: source.path "rule.js" is not supported`,
		"/dtm.yaml":      `digitalTwin.propertyDefinitions[0]: source.path "./dtm" is not supported`,
		// optional only skips the include itself, not a file it includes
		"/nested.yaml": "/gone.yaml",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadManifest(server.URL + name)
			require.Error(t, err)
			assert.Contains(t, err.Error(), want)
		})
	}
}

func TestRemoteManifestAllowCommands(t *testing.T) {
	server := newManifestServer(t, map[string]string{
		"/tenant.yaml": "include:\n  - inc.yaml\nhooks:\n  pre:\n    - run: echo hi\n",
		"/inc.yaml":    "commands:\n  - name: x\n    actions: [echo hi]\n",
	})
	manifest, err := LoadManifestWith(server.URL+"/tenant.yaml", LoadOptions{AllowRemoteCommands: true})
	require.NoError(t, err)
	require.Len(t, manifest.Hooks.Pre, 1)
	require.Len(t, manifest.Commands, 1)

	// Commands of a remote include run from the main manifest directory
	cwd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, cwd, manifest.Commands[0].Dir)
	assert.Empty(t, manifest.Hooks.Pre[0].Dir)
}

func TestRemoteManifestGitHubToken(t *testing.T) {
	server := newManifestServer(t, map[string]string{"/tenant.yaml": "tenantOptions: []\n"})
	t.Setenv("GITHUB_TOKEN", "secret-token")

	// The token is only sent to GitHub hosts
	_, err := LoadManifest(server.URL + "/tenant.yaml")
	require.NoError(t, err)
	assert.Empty(t, server.auth["/tenant.yaml"])

	assert.True(t, isGitHubHost("raw.githubusercontent.com"))
	assert.True(t, isGitHubHost("GitHub.com"))
	assert.False(t, isGitHubHost("example.com"))
}

func TestRunValidateRemoteManifest(t *testing.T) {
	server := newManifestServer(t, map[string]string{
		"/tenant.yaml": "include:\n  - base.yaml\n",
		"/base.yaml":   "tenantOptions:\n  - {category: a, key: b, value: c}\n",
	})
	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "validate", server.URL+"/tenant.yaml")
	})
	require.NoError(t, runErr)
	assert.True(t, strings.Contains(stdout, "includes "+server.URL+"/base.yaml"), stdout)
}
