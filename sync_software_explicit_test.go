package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSoftwareRepository serves an empty software repository which records
// created software items (id 500) and versions. existingVersion, when set,
// is returned by the version lookup.
func fakeSoftwareRepository(t *testing.T, existingVersion map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		items := []any{}
		if existingVersion != nil && strings.Contains(r.URL.Query().Get("query"), "c8y_SoftwareBinary") {
			items = append(items, existingVersion)
		}
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": items, "statistics": stats()})
	})
	f.JSON("POST /inventory/managedObjects", http.StatusCreated, map[string]any{"id": "500", "type": "c8y_Software"})
	f.Handle("POST /inventory/managedObjects/500/childAdditions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": "501", "type": "c8y_SoftwareBinary"})
	})
	return f
}

func TestExplicitSoftwareWithoutURL(t *testing.T) {
	f := fakeSoftwareRepository(t, nil)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncSoftware(context.Background(), []SoftwareSpec{
		{Name: "tedge", Version: "latest", Type: "apt"},
	}))

	r := resultFor(t, s, SectionSoftware)
	assert.Equal(t, "tedge [apt] latest", r.Item)
	assert.Equal(t, ActionCreated, r.Action)
	assert.Equal(t, "no binary (empty url)", r.Detail)

	items := f.Requests(http.MethodPost, "/inventory/managedObjects")
	require.Len(t, items, 2, "software item and version")
	assert.Equal(t, "tedge", items[0].Body["name"])
	assert.Equal(t, "apt", items[0].Body["softwareType"])
	assert.Equal(t, "c8y_Software", items[0].Body["type"])

	version := items[1]
	assert.Equal(t, "/inventory/managedObjects/500/childAdditions", version.Path)
	assert.Equal(t, map[string]any{"version": "latest", "url": ""}, version.Body["c8y_Software"],
		"the version is used as-is and the url is empty")
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"), "nothing is uploaded")
}

func TestExplicitSoftwareWithURL(t *testing.T) {
	f := fakeSoftwareRepository(t, nil)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncSoftware(context.Background(), []SoftwareSpec{
		{Name: "tedge", Version: "1.6.2", Type: "apt", URL: "https://example.com/tedge_1.6.2_arm64.deb"},
	}))

	r := resultFor(t, s, SectionSoftware)
	assert.Equal(t, "link → https://example.com/tedge_1.6.2_arm64.deb", r.Detail)
	versions := f.Requests(http.MethodPost, "/inventory/managedObjects/500/childAdditions")
	require.Len(t, versions, 1)
	assert.Equal(t, map[string]any{"version": "1.6.2", "url": "https://example.com/tedge_1.6.2_arm64.deb"}, versions[0].Body["c8y_Software"])
}

func TestExplicitSoftwareExistingVersionUnchanged(t *testing.T) {
	f := fakeSoftwareRepository(t, map[string]any{
		"id":           "501",
		"type":         "c8y_SoftwareBinary",
		"c8y_Software": map[string]any{"version": "latest", "url": ""},
	})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncSoftware(context.Background(), []SoftwareSpec{
		{Name: "tedge", Version: "latest", Type: "apt"},
	}))
	assert.NotEqual(t, ActionFailed, resultFor(t, s, SectionSoftware).Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/managedObjects/500/childAdditions"), "an existing version is not created again")
}

func TestExplicitSoftwareDryRun(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	s.DryRun = true

	require.NoError(t, s.SyncSoftware(context.Background(), []SoftwareSpec{
		{Name: "tedge", Version: "latest", Type: "apt"},
		{Name: "agent", Version: "2.0.0", URL: "https://example.com/agent.deb"},
	}))

	details := map[string]string{}
	for _, r := range s.Results {
		assert.Equal(t, ActionPlanned, r.Action)
		details[r.Item] = r.Detail
	}
	assert.Equal(t, "no binary (empty url)", details["tedge [apt] latest"])
	assert.Equal(t, "link → https://example.com/agent.deb", details["agent 2.0.0"], "no empty type brackets")
	assert.Empty(t, f.Requests(http.MethodGet, "/inventory"), "dry run makes no API calls")
}

func TestExplicitSoftwareValidation(t *testing.T) {
	path := writeManifest(t, `software:
  - name: tedge
    version: latest
    type: apt
  - name: agent
    version: 2.0.0
    url: https://example.com/agent.deb
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	require.Len(t, manifest.Software, 2)
	assert.True(t, manifest.Software[0].IsExplicit())
	assert.Equal(t, "latest", manifest.Software[0].Version)

	path = writeManifest(t, `software:
  - name: no-version
  - name: both
    version: 1.0.0
    source:
      path: ./packages
  - name: parse-options
    version: 1.0.0
    namePrefix: x-
    typeMap: ["*.bin=firmware"]
  - name: relative-url
    version: 1.0.0
    url: /files/agent.deb
  - source:
      path: ./packages
    url: https://example.com/agent.deb
`)
	_, err = LoadManifest(path)
	require.Error(t, err)
	for _, want := range []string{
		"software[0]: version is required for an explicit entry (name)",
		"software[1]: name and source are mutually exclusive",
		"software[2]: namePrefix and typeMap only apply to entries with a source",
		`software[3]: url "/files/agent.deb" is not an absolute url`,
		"software[4]: url requires name (for a source, use source.url)",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestExplicitSoftwareSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("software:\n  - name: tedge\n    version: latest\n    type: apt\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("software:\n  - source:\n      path: ./packages\n")))
	assert.Error(t, validateYAML(t, schema, []byte("software:\n  - name: tedge\n")), "an explicit entry needs a version")
	assert.Error(t, validateYAML(t, schema, []byte("software:\n  - name: tedge\n    version: 1.0.0\n    source:\n      path: ./packages\n")), "name and source are mutually exclusive")
	assert.Error(t, validateYAML(t, schema, []byte("software:\n  - type: apt\n")), "either name or source is required")
}

func TestValidateCheckSourcesSkipsExplicitSoftware(t *testing.T) {
	path := writeManifest(t, "software:\n  - name: tedge\n    version: latest\n    type: apt\n")
	assert.NoError(t, runRoot(t, "validate", path, "--check-sources"))
}
