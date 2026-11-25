package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditDistance(t *testing.T) {
	assert.Equal(t, 0, editDistance("Cloud HTTP proxy", "cloud http PROXY"), "case is ignored")
	assert.Equal(t, 1, editDistance("my-plugin", "my-plugn"))
	assert.Equal(t, 3, editDistance("kitten", "sitting"))
	assert.Equal(t, 4, editDistance("", "abcd"))
}

func TestSuggestNames(t *testing.T) {
	candidates := []string{"Cloud HTTP proxy", "sag-pkg-remote-access-cloud-http-proxy", "my-widget-plugin", "other"}

	assert.Equal(t, []string{"Cloud HTTP proxy"}, suggestNames("Cloud HTTP Proxi", candidates, 3))
	assert.Equal(t, []string{"Cloud HTTP proxy", "sag-pkg-remote-access-cloud-http-proxy"}, suggestNames("Cloud-HTTP-proxy", candidates, 3), "the closest first")
	assert.Equal(t, []string{"my-widget-plugin"}, suggestNames("my-widget", candidates, 3), "a name contained in a candidate")
	assert.Contains(t, suggestNames("cloud-http-proxy", candidates, 3), "sag-pkg-remote-access-cloud-http-proxy")
	assert.Empty(t, suggestNames("something-else", candidates, 3))
	assert.Empty(t, suggestNames("", candidates, 3))
	assert.Len(t, suggestNames("a", []string{"b", "c", "d", "e"}, 2), 2, "limited")
}

func TestExtensionReferenceWarnings(t *testing.T) {
	notInstalled := false
	m := &Manifest{
		Extensions: []ExtensionSpec{{Name: "Cloud HTTP proxy"}},
		Applications: []ApplicationSpec{{Name: "devicemanagement", Extensions: []ApplicationExtensionSpec{
			{Name: "Cloud HTTP proxy"},                           // declared
			{Name: "cloud http PROXY"},                           // declared, other case
			{Name: "Cloud HTTP Proxi"},                           // typo
			{Name: "datahub-plugin"},                             // not declared, nothing close
			{Name: "Cloud HTTP Proxi", Installed: &notInstalled}, // removals are not checked
			{Name: "sag-pkg-remote-access-cloud-http-proxy"},     // context path, unknown offline
		}}},
	}

	warnings := m.ExtensionReferenceWarnings(nil)
	require.Len(t, warnings, 3, warnings)
	assert.Equal(t, `applications[0].extensions[2]: "Cloud HTTP Proxi" is not uploaded by the extensions section, so it must already be on the tenant (matched by name or context path); did you mean "Cloud HTTP proxy"?`, warnings[0])
	assert.Contains(t, warnings[1], `"datahub-plugin" is not uploaded by the extensions section`)
	assert.NotContains(t, warnings[1], "did you mean")
	assert.Contains(t, warnings[2], `"sag-pkg-remote-access-cloud-http-proxy"`)

	// With the context paths read from the archives (validate --check-sources)
	warnings = m.ExtensionReferenceWarnings(map[string]string{"sag-pkg-remote-access-cloud-http-proxy": "Cloud HTTP proxy"})
	assert.Len(t, warnings, 2, warnings)

	// The name in the archive counts (and is suggested) even when the entry's
	// name is wrong, which --check-sources reports as a failure
	m.Extensions[0].Name = "cloud-http-proxy-ui"
	warnings = m.ExtensionReferenceWarnings(map[string]string{"sag-pkg-remote-access-cloud-http-proxy": "Cloud HTTP proxy"})
	require.Len(t, warnings, 2, warnings)
	assert.Contains(t, warnings[0], `"Cloud HTTP Proxi" is not uploaded`)
	assert.Contains(t, warnings[0], `did you mean "Cloud HTTP proxy"?`)
}

func TestValidateChecksTheExtensionArchiveName(t *testing.T) {
	dir := t.TempDir()
	zip := widgetZip(t, "my-widget-plugin", "1.2.0")
	require.NoError(t, os.Rename(zip, filepath.Join(dir, "widget.zip")))

	write := func(name string) string {
		path := filepath.Join(dir, "tenant.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`
extensions:
  - name: `+name+`
    source:
      path: ./widget.zip
applications:
  - name: devicemanagement
    extensions: [my-widget]
`), 0o600))
		return path
	}

	// The context path of the archive (my-widget) can be referenced
	assert.NoError(t, runValidate([]string{write("my-widget-plugin")}, true, false))
	assert.NoError(t, runValidate([]string{write("MY-WIDGET-PLUGIN")}, true, false), "the name is matched ignoring case")

	err := runValidate([]string{write("my-widget")}, true, false)
	assert.ErrorContains(t, err, "1 source(s) failed to resolve")
	info, err := checkExtensionArchive(NewSourceResolver(dir, t.TempDir(), false), ExtensionSpec{Name: "my-widget", Source: Source{Path: "./widget.zip"}})
	assert.ErrorContains(t, err, `widget.zip contains the extension "my-widget-plugin", not "my-widget": set name to "my-widget-plugin"`)
	assert.Equal(t, "my-widget", info.ContextPath)
}

func TestApplicationExtensionsMatchIgnoringCaseAndSuggestNames(t *testing.T) {
	f := fakeExtensionTenant(t,
		ownWebApp(map[string]any{"gone@1.0.0": []string{"GoneModule"}}),
		extension("77", "Cloud HTTP proxy", "sag-pkg-remote-access-cloud-http-proxy", "t123", extVersion("2.13.1", "latest")),
	)
	f.JSON("PUT /application/applications/42", http.StatusOK, map[string]any{"id": "42"})
	s := newFakeSyncer(t, f)

	notInstalled := false
	s.syncApplicationExtensions(context.Background(), ApplicationSpec{
		Name: "devicemanagement",
		Extensions: []ApplicationExtensionSpec{
			{Name: "cloud http PROXY"},               // other case
			{Name: "Cloud HTTP Proxi"},               // typo
			{Name: "gone", Installed: &notInstalled}, // no longer exists, removed by its context path
		},
	})

	results := resultsByItem(s)
	assert.Equal(t, ActionUpdated, results["devicemanagement/cloud http PROXY"].Action, results["devicemanagement/cloud http PROXY"].Err)
	assert.ErrorContains(t, results["devicemanagement/Cloud HTTP Proxi"].Err, `extension "Cloud HTTP Proxi" not found; did you mean "Cloud HTTP proxy"?`)
	assert.Equal(t, ActionUpdated, results["devicemanagement/gone"].Action, results["devicemanagement/gone"].Err)
	assert.Equal(t, "removed version 1.0.0", results["devicemanagement/gone"].Detail)

	puts := exactRequests(f, http.MethodPut, "/application/applications/42")
	require.Len(t, puts, 1)
	remotes := puts[0].Body["config"].(map[string]any)["remotes"].(map[string]any)
	assert.Equal(t, []any{"sag-pkg-remote-access-cloud-http-proxyModule"}, remotes["sag-pkg-remote-access-cloud-http-proxy@2.13.1"])
	assert.NotContains(t, remotes, "gone@1.0.0")

	// The catalog (own and shared extensions) is read once for all entries.
	// The go-c8y update of the application reads its own (with currentPage).
	var catalogReads int
	for _, r := range exactRequests(f, http.MethodGet, "/application/applications") {
		if strings.Contains(r.Query, "hasVersions=true") && !strings.Contains(r.Query, "currentPage") {
			catalogReads++
		}
	}
	assert.Equal(t, 2, catalogReads)
}
