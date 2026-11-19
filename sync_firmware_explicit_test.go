package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFirmwareRepository serves an empty firmware repository which records
// created firmware items (id 600) and versions
func fakeFirmwareRepository(t *testing.T) *fakeC8y {
	f := newFakeC8y(t)
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": []any{}, "statistics": stats()})
	})
	f.JSON("POST /inventory/managedObjects", http.StatusCreated, map[string]any{"id": "600", "type": "c8y_Firmware"})
	f.JSON("PUT /inventory/managedObjects/600", http.StatusOK, map[string]any{"id": "600", "type": "c8y_Firmware"})
	f.JSON("POST /inventory/managedObjects/600/childAdditions", http.StatusCreated, map[string]any{"id": "601", "type": "c8y_FirmwareBinary"})
	return f
}

func TestExplicitFirmware(t *testing.T) {
	f := fakeFirmwareRepository(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncFirmware(context.Background(), []FirmwareSpec{{
		Name:       "rugix-image",
		Version:    "1.2.3",
		DeviceType: "rugix-{name}",
		URL:        "https://example.com/rugix-image_1.2.3.img.xz",
	}}))

	for _, r := range s.Results {
		require.NoError(t, r.Err, "%+v", r)
	}
	items := f.Requests(http.MethodPost, "/inventory/managedObjects")
	require.Len(t, items, 2, "firmware item and version")
	assert.Equal(t, "rugix-image", items[0].Body["name"])
	assert.Equal(t, map[string]any{"type": "rugix-rugix-image"}, items[0].Body["c8y_Filter"], "placeholders use the declared name")

	assert.Equal(t, "/inventory/managedObjects/600/childAdditions", items[1].Path)
	assert.Equal(t, map[string]any{"version": "1.2.3", "url": "https://example.com/rugix-image_1.2.3.img.xz"}, items[1].Body["c8y_Firmware"])
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"), "nothing is uploaded")
}

func TestExplicitFirmwareWithoutURL(t *testing.T) {
	f := fakeFirmwareRepository(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncFirmware(context.Background(), []FirmwareSpec{{Name: "os", Version: "2026.11"}}))

	versions := f.Requests(http.MethodPost, "/inventory/managedObjects/600/childAdditions")
	require.Len(t, versions, 1)
	assert.Equal(t, map[string]any{"version": "2026.11", "url": ""}, versions[0].Body["c8y_Firmware"])
	for _, r := range s.Results {
		if r.Item == "os 2026.11" {
			assert.Equal(t, "no binary (empty url)", r.Detail)
		}
	}
}

func TestExplicitFirmwareDryRun(t *testing.T) {
	f := newFakeC8y(t)
	s := newFakeSyncer(t, f)
	s.DryRun = true

	require.NoError(t, s.SyncFirmware(context.Background(), []FirmwareSpec{{
		Name: "os", Version: "1.0.0", URL: "https://example.com/os.img",
	}}))
	details := map[string]string{}
	for _, r := range s.Results {
		assert.Equal(t, ActionPlanned, r.Action)
		details[r.Item] = r.Detail
	}
	assert.Equal(t, "link → https://example.com/os.img", details["os 1.0.0"])
	assert.Empty(t, f.Requests(http.MethodGet, "/inventory"))
}

func TestExplicitFirmwareValidation(t *testing.T) {
	path := writeManifest(t, `firmware:
  - name: os
    version: 1.0.0
  - name: os
    version: 1.0.1
    deviceType: "{name}"
    url: https://example.com/os.img
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.True(t, manifest.Firmware[0].IsExplicit())

	path = writeManifest(t, `firmware:
  - name: no-version
  - version: 1.0.0
  - name: pattern
    version: 1.0.0
    versionPattern: 'v(\d+)'
  - name: relative
    version: 1.0.0
    url: ./os.img
  - name: with-source
    url: https://example.com/os.img
    source:
      path: ./firmware
`)
	_, err = LoadManifest(path)
	require.Error(t, err)
	for _, want := range []string{
		"firmware[0]: source, or name and version (an explicit entry), is required",
		"firmware[1]: source, or name and version (an explicit entry), is required",
		"firmware[2]: versionPattern only applies to entries with a source",
		`firmware[3]: url "./os.img" is not an absolute url`,
		"firmware[4]: url requires an entry without a source (for a source, use source.url)",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestExplicitFirmwareSchema(t *testing.T) {
	schema := compileSchema(t)
	assert.NoError(t, validateYAML(t, schema, []byte("firmware:\n  - name: os\n    version: 1.0.0\n    url: https://example.com/os.img\n")))
	assert.NoError(t, validateYAML(t, schema, []byte("firmware:\n  - name: os\n    source:\n      path: ./firmware\n")), "name stays an override for source entries")
	assert.Error(t, validateYAML(t, schema, []byte("firmware:\n  - name: os\n")), "an explicit entry needs a version")
	assert.Error(t, validateYAML(t, schema, []byte("firmware:\n  - name: os\n    version: 1.0.0\n    versionPattern: x\n")))
}

func TestValidateCheckSourcesSkipsExplicitFirmware(t *testing.T) {
	path := writeManifest(t, "firmware:\n  - name: os\n    version: 1.0.0\n")
	assert.NoError(t, runRoot(t, "validate", path, "--check-sources"))
}
