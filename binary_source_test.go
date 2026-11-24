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

func TestBinarySourceChanged(t *testing.T) {
	const binary = "https://t123.example.com/inventory/binaries/42"
	const link = "https://github.com/org/repo/releases/download/v1/image.img"

	tests := []struct {
		name        string
		existingURL string
		desiredURL  string
		uploading   bool
		want        bool
	}{
		{"link unchanged", link, link, false, false},
		{"link changed", link, link + ".xz", false, true},
		{"uploaded to linked", binary, link, false, true},
		{"linked to uploaded", link, "", true, true},
		{"empty url to uploaded", "", "", true, true},
		{"uploaded stays uploaded", binary, "", true, false},
		{"relative binary stays uploaded", "/inventory/binaries/42", "", true, false},
		{"explicit without url", link, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, binarySourceChanged(tt.existingURL, tt.desiredURL, tt.uploading))
		})
	}
}

// fakeBinaryRepository serves a firmware or software repository with one
// existing version (id 601, child of item 600) and records uploads, updates
// and binary deletions
func fakeBinaryRepository(t *testing.T, versionType string, existingVersion map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		items := []any{}
		if strings.Contains(r.URL.Query().Get("query"), versionType) {
			items = append(items, existingVersion)
		}
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": items, "statistics": stats()})
	})
	f.JSON("POST /inventory/managedObjects", http.StatusCreated, map[string]any{"id": "600"})
	f.JSON("POST /inventory/binaries", http.StatusCreated, map[string]any{"id": "801", "self": f.server.URL + "/inventory/binaries/801"})
	f.JSON("PUT /inventory/managedObjects/601", http.StatusOK, map[string]any{"id": "601", "type": versionType})
	f.Handle("DELETE /inventory/binaries/700", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return f
}

func writeTempFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte("image"), 0o644))
	return path
}

func firmwareVersionResult(t *testing.T, s *Syncer, item string) ChangeResult {
	t.Helper()
	for _, r := range s.Results {
		if r.Item == item {
			require.NoError(t, r.Err)
			return r
		}
	}
	t.Fatalf("no result for %s: %+v", item, s.Results)
	return ChangeResult{}
}

func TestFirmwareLinkedVersionIsUploadedWhenLinkOnlyDisabled(t *testing.T) {
	const link = "https://github.com/org/repo/releases/download/v1/image_1.0.0.img"
	f := fakeBinaryRepository(t, "c8y_FirmwareBinary", map[string]any{
		"id": "601", "type": "c8y_FirmwareBinary",
		"c8y_Firmware": map[string]any{"version": "1.0.0", "url": link},
	})
	s := newFakeSyncer(t, f)

	s.syncFirmwareItem(context.Background(), FirmwareSpec{}, "image", []*FirmwareInfo{{
		Name: "image", Version: "1.0.0", Filename: "image_1.0.0.img",
		FilePath: writeTempFile(t, "image_1.0.0.img"),
	}})

	assert.Equal(t, ActionUpdated, firmwareVersionResult(t, s, "image 1.0.0").Action)
	assert.Len(t, f.Requests(http.MethodPost, "/inventory/binaries"), 1, "the image is uploaded")
	updates := f.Requests(http.MethodPut, "/inventory/managedObjects/601")
	require.Len(t, updates, 1)
	assert.Equal(t, f.server.URL+"/inventory/binaries/801", updates[0].Body["c8y_Firmware"].(map[string]any)["url"])
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/managedObjects/600/childAdditions"), "the version is not created again")
}

func TestFirmwareUploadedVersionIsLinkedWhenLinkOnlyEnabled(t *testing.T) {
	const link = "https://github.com/org/repo/releases/download/v1/image_1.0.0.img"
	f := fakeBinaryRepository(t, "c8y_FirmwareBinary", map[string]any{
		"id": "601", "type": "c8y_FirmwareBinary",
		"c8y_Firmware": map[string]any{"version": "1.0.0", "url": "/inventory/binaries/700"},
	})
	s := newFakeSyncer(t, f)

	s.syncFirmwareItem(context.Background(), FirmwareSpec{}, "image", []*FirmwareInfo{{
		Name: "image", Version: "1.0.0", Filename: "image_1.0.0.img", URL: link,
	}})

	assert.Equal(t, ActionUpdated, firmwareVersionResult(t, s, "image 1.0.0").Action)
	updates := f.Requests(http.MethodPut, "/inventory/managedObjects/601")
	require.Len(t, updates, 1)
	assert.Equal(t, link, updates[0].Body["c8y_Firmware"].(map[string]any)["url"])
	assert.Len(t, f.Requests(http.MethodDelete, "/inventory/binaries/700"), 1, "the old binary is removed")
	assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"), "nothing is uploaded")
}

func TestFirmwareUnchangedSourceIsLeftAlone(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		info     FirmwareInfo
	}{
		{"same link", "https://example.com/image.img", FirmwareInfo{URL: "https://example.com/image.img"}},
		{"already uploaded", "/inventory/binaries/700", FirmwareInfo{Filename: "image.img"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fakeBinaryRepository(t, "c8y_FirmwareBinary", map[string]any{
				"id": "601", "type": "c8y_FirmwareBinary",
				"c8y_Firmware": map[string]any{"version": "1.0.0", "url": tt.existing},
			})
			s := newFakeSyncer(t, f)
			info := tt.info
			info.Name, info.Version = "image", "1.0.0"
			if info.Filename != "" {
				info.FilePath = writeTempFile(t, info.Filename)
			}

			s.syncFirmwareItem(context.Background(), FirmwareSpec{}, "image", []*FirmwareInfo{&info})

			assert.Equal(t, ActionUnchanged, firmwareVersionResult(t, s, "image 1.0.0").Action)
			assert.Empty(t, f.Requests(http.MethodPut, "/inventory/managedObjects/601"))
			assert.Empty(t, f.Requests(http.MethodPost, "/inventory/binaries"))
		})
	}
}

func TestSoftwareLinkedVersionIsUploadedWhenLinkOnlyDisabled(t *testing.T) {
	const link = "https://github.com/org/repo/releases/download/v1/tool_1.0.0_arm64.deb"
	f := fakeBinaryRepository(t, "c8y_SoftwareBinary", map[string]any{
		"id": "601", "type": "c8y_SoftwareBinary",
		"c8y_Software": map[string]any{"version": "1.0.0", "url": link},
	})
	s := newFakeSyncer(t, f)

	action, err := s.uploadSoftwareVersion(context.Background(), &SoftwareInfo{
		Name: "tool", Version: "1.0.0", SoftwareType: "apt", Filename: "tool_1.0.0_arm64.deb",
		FilePath: writeTempFile(t, "tool_1.0.0_arm64.deb"),
	}, "600")
	require.NoError(t, err)

	assert.Equal(t, ActionUpdated, action)
	assert.Len(t, f.Requests(http.MethodPost, "/inventory/binaries"), 1, "the package is uploaded")
	updates := f.Requests(http.MethodPut, "/inventory/managedObjects/601")
	require.Len(t, updates, 1)
	assert.Equal(t, f.server.URL+"/inventory/binaries/801", updates[0].Body["c8y_Software"].(map[string]any)["url"])
}
