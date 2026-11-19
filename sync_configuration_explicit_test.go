package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConfigurationRepository serves an empty configuration repository
// which records created items (id 700) and uploaded binaries (id 800)
func fakeConfigurationRepository(t *testing.T) *fakeC8y {
	f := newFakeC8y(t)
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": []any{}, "statistics": stats()})
	})
	f.JSON("POST /inventory/managedObjects", http.StatusCreated, map[string]any{"id": "700", "type": "c8y_ConfigurationDump"})
	f.JSON("POST /inventory/binaries", http.StatusCreated, map[string]any{"id": "800", "self": f.server.URL + "/inventory/binaries/800"})
	f.JSON("POST /inventory/managedObjects/700/childAdditions", http.StatusCreated, map[string]any{})
	return f
}

func TestExplicitConfigurationInlineContent(t *testing.T) {
	f := fakeConfigurationRepository(t)
	s := newFakeSyncer(t, f)
	content := "listener 1883\nallow_anonymous false\n"

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{{
		Name:              "mosquitto",
		ConfigurationType: "mosquitto.conf",
		DeviceType:        "thin-edge.io",
		Content:           content,
		Filename:          "mosquitto.conf",
	}}))

	r := resultFor(t, s, SectionConfiguration)
	assert.Equal(t, "mosquitto (mosquitto.conf)", r.Item)
	assert.Equal(t, ActionCreated, r.Action)
	assert.Equal(t, "inline content as mosquitto.conf (36 bytes)", r.Detail)

	uploads := f.Requests(http.MethodPost, "/inventory/binaries")
	require.Len(t, uploads, 1)
	assert.Contains(t, string(uploads[0].RawBody), content, "the inline content is uploaded")
	assert.Contains(t, string(uploads[0].RawBody), `filename="mosquitto.conf"`)

	items := f.Requests(http.MethodPost, "/inventory/managedObjects")
	require.NotEmpty(t, items)
	assert.Equal(t, "mosquitto", items[0].Body["name"])
	assert.Equal(t, "mosquitto.conf", items[0].Body["configurationType"])
	assert.Equal(t, f.server.URL+"/inventory/binaries/800", items[0].Body["url"])

	// The temporary file is removed again
	entries, err := os.ReadDir(s.Resolver.WorkDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestExplicitConfigurationFilenameDefaultsToName(t *testing.T) {
	f := fakeConfigurationRepository(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{{
		Name: "tedge.toml", ConfigurationType: "tedge.toml", Content: "[mqtt]\n",
	}}))
	uploads := f.Requests(http.MethodPost, "/inventory/binaries")
	require.Len(t, uploads, 1)
	assert.Contains(t, string(uploads[0].RawBody), `filename="tedge.toml"`)
}

func TestExplicitConfigurationURL(t *testing.T) {
	f := fakeConfigurationRepository(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{{
		Name: "collectd", ConfigurationType: "collectd.conf", URL: "https://example.com/collectd.conf",
	}}))

	assert.Equal(t, "link → https://example.com/collectd.conf", resultFor(t, s, SectionConfiguration).Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"), "nothing is uploaded")
	items := f.Requests(http.MethodPost, "/inventory/managedObjects")
	require.NotEmpty(t, items)
	assert.Equal(t, "https://example.com/collectd.conf", items[0].Body["url"])
}

func TestExplicitConfigurationWithoutFile(t *testing.T) {
	f := fakeConfigurationRepository(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{{Name: "placeholder", ConfigurationType: "x"}}))
	r := resultFor(t, s, SectionConfiguration)
	assert.Equal(t, ActionCreated, r.Action)
	assert.Equal(t, "no file (no url)", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"))
}

func TestExplicitConfigurationDryRun(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	s.DryRun = true

	require.NoError(t, s.SyncConfiguration(context.Background(), []ConfigurationSpec{
		{Name: "mosquitto", ConfigurationType: "mosquitto.conf", Content: "listener 1883\n"},
	}))
	r := resultFor(t, s, SectionConfiguration)
	assert.Equal(t, ActionPlanned, r.Action)
	assert.Equal(t, "inline content as mosquitto (14 bytes)", r.Detail)
	assert.Empty(t, f.Requests(http.MethodGet, "/inventory"))
}

func TestWriteInlineFile(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := writeInlineFile(dir, "a.conf", "x=1\n")
	require.NoError(t, err)
	assert.Equal(t, "a.conf", filepath.Base(path))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "x=1\n", string(content))

	cleanup()
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestExplicitConfigurationValidation(t *testing.T) {
	t.Setenv("TS_BROKER_PORT", "8883")
	path := writeManifest(t, `configuration:
  - name: mosquitto
    configurationType: mosquitto.conf
    filename: mosquitto.conf
    content: |
      listener ${TS_BROKER_PORT}
      allow_anonymous false
  - name: collectd
    configurationType: collectd.conf
    url: https://example.com/collectd.conf
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, "listener 8883\nallow_anonymous false\n", manifest.Configuration[0].Content, "${VAR} is expanded in inline content")
	assert.True(t, manifest.Configuration[1].IsExplicit())

	path = writeManifest(t, `configuration:
  - configurationType: no-name
  - name: both
    configurationType: x
    url: https://example.com/a.conf
    content: a
  - name: filename-without-content
    configurationType: x
    filename: a.conf
  - name: path-in-filename
    configurationType: x
    content: a
    filename: ../a.conf
  - name: relative-url
    configurationType: x
    url: a.conf
  - name: source-with-content
    configurationType: x
    content: a
    source:
      path: ./config/a.conf
`)
	_, err = LoadManifest(path)
	require.Error(t, err)
	for _, want := range []string{
		"configuration[0]: source, or name (an explicit entry), is required",
		"configuration[1]: url and content are mutually exclusive",
		"configuration[2]: filename only applies to inline content",
		`configuration[3]: filename "../a.conf" must not contain a path`,
		`configuration[4]: url "a.conf" is not an absolute url`,
		"configuration[5]: url, content and filename require an entry without a source",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestExplicitConfigurationSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("configuration:\n  - name: a\n    configurationType: x\n    content: |\n      key=value\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("configuration:\n  - configurationType: x\n    source:\n      path: ./a.conf\n")))
	assert.Error(t, validateYAML(t, schema, []byte("configuration:\n  - name: a\n    configurationType: x\n    url: https://example.com/a\n    content: b\n")))
	assert.Error(t, validateYAML(t, schema, []byte("configuration:\n  - configurationType: x\n    content: b\n    source:\n      path: ./a.conf\n")))
}

func TestValidateCheckSourcesSkipsExplicitConfiguration(t *testing.T) {
	path := writeManifest(t, "configuration:\n  - name: a\n    configurationType: x\n    content: b\n")
	assert.NoError(t, runRoot(t, "validate", path, "--check-sources"))
}
