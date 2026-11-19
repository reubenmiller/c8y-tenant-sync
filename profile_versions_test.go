package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func versions(values ...string) []repositoryVersion {
	out := make([]repositoryVersion, 0, len(values))
	for _, v := range values {
		out = append(out, repositoryVersion{Version: v, URL: "https://example.com/" + v})
	}
	return out
}

func TestSelectLatestVersion(t *testing.T) {
	tests := []struct {
		name               string
		versions           []repositoryVersion
		includePrereleases bool
		want               string
		wantErr            string
	}{
		{name: "semver order, not string order", versions: versions("1.9.0", "1.10.0", "1.2.0"), want: "1.10.0"},
		{name: "v prefix and short versions", versions: versions("v1.2.3", "1.3", "1.2.10"), want: "1.3"},
		{name: "build timestamps", versions: versions("20261104.2250", "20261021.1010", "20261104.0900"), want: "20261104.2250"},
		{name: "prereleases excluded by default", versions: versions("1.0.0", "1.1.0-rc1", "1.1.0-alpha"), want: "1.0.0"},
		{name: "prereleases included", versions: versions("1.0.0", "1.1.0-rc1", "1.1.0-alpha"), includePrereleases: true, want: "1.1.0-rc1"},
		{name: "release beats its prerelease", versions: versions("1.1.0-rc1", "1.1.0"), includePrereleases: true, want: "1.1.0"},
		{name: "invalid versions ignored", versions: versions("latest", "nightly", "2.0.0", "abc-1"), want: "2.0.0"},
		{name: "build metadata", versions: versions("1.2.3+build.5", "1.2.2"), want: "1.2.3+build.5"},
		{name: "equal versions keep the first", versions: versions("1.2", "1.2.0"), want: "1.2"},
		{name: "no versions", versions: nil, wantErr: "no version found to use as latest (0 version(s))"},
		{name: "only invalid", versions: versions("nightly", "dev"), wantErr: "2 not valid semver"},
		{name: "only prereleases", versions: versions("1.0.0-rc1"), wantErr: "1 prerelease(s) excluded (set includePrereleases: true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectLatestVersion(tt.versions, tt.includePrereleases)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Version)
			assert.Equal(t, "https://example.com/"+tt.want, got.URL, "the url of the selected version is used")
		})
	}
}

func TestIsLatestVersion(t *testing.T) {
	assert.True(t, isLatestVersion("latest"))
	assert.True(t, isLatestVersion(" Latest "))
	assert.False(t, isLatestVersion("1.0.0"))
	assert.False(t, isLatestVersion("latest-5"))
}

func TestResolveVersion(t *testing.T) {
	listCalled, lookupCalled := 0, 0
	list := func() ([]repositoryVersion, error) { listCalled++; return versions("1.0.0", "2.0.0"), nil }
	lookup := func() (string, error) { lookupCalled++; return "https://example.com/exact", nil }

	// A fixed version uses the exact lookup, lazily
	version, resolvedLookup, err := resolveVersion("1.0.0", false, list, lookup)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
	assert.Equal(t, 0, listCalled)
	url, _ := resolvedLookup()
	assert.Equal(t, "https://example.com/exact", url)

	// latest lists the versions once and uses the url of the selected one
	version, resolvedLookup, err = resolveVersion("latest", false, list, lookup)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", version)
	url, _ = resolvedLookup()
	assert.Equal(t, "https://example.com/2.0.0", url)
	assert.Equal(t, 1, listCalled)
	assert.Equal(t, 1, lookupCalled)

	// List errors are returned
	_, _, err = resolveVersion("latest", false, func() ([]repositoryVersion, error) { return nil, errors.New("boom") }, lookup)
	assert.ErrorContains(t, err, "boom")
}

// fakeRepository serves firmware "core-image" and software "tedge" (apt)
// with the given versions
func fakeRepository(t *testing.T, firmware, software []string) *fakeC8y {
	f := newFakeC8y(t)
	binaries := func(fragment string, values []string) []map[string]any {
		out := []map[string]any{}
		for _, v := range values {
			out = append(out, map[string]any{
				"id":     "v-" + v,
				fragment: map[string]any{"version": v, "url": "https://example.com/" + fragment + "/" + v},
			})
		}
		return out
	}
	f.Handle("GET /inventory/managedObjects", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		var items []map[string]any
		switch {
		case strings.Contains(query, "c8y_FirmwareBinary") && strings.Contains(query, "bygroupid(100)"):
			items = binaries("c8y_Firmware", firmware)
		case strings.Contains(query, "c8y_SoftwareBinary") && strings.Contains(query, "bygroupid(200)"):
			items = binaries("c8y_Software", software)
		case strings.Contains(query, "c8y_Firmware") && strings.Contains(query, "'core-image'"):
			items = []map[string]any{{"id": "100", "name": "core-image", "type": "c8y_Firmware"}}
		case strings.Contains(query, "c8y_Software") && strings.Contains(query, "'tedge'"):
			items = []map[string]any{{"id": "200", "name": "tedge", "type": "c8y_Software", "softwareType": "apt"}}
		}
		writeJSON(w, http.StatusOK, map[string]any{"managedObjects": items, "statistics": stats()})
	})
	f.JSON("GET /inventory/managedObjects/100", http.StatusOK, map[string]any{"id": "100", "name": "core-image", "type": "c8y_Firmware"})
	f.JSON("GET /inventory/managedObjects/200", http.StatusOK, map[string]any{"id": "200", "name": "tedge", "type": "c8y_Software", "softwareType": "apt"})
	return f
}

func TestBuildProfileBodyLatestVersions(t *testing.T) {
	f := fakeRepository(t,
		[]string{"1.9.0", "1.10.0", "2.0.0-rc1"},
		[]string{"1.6.2", "1.7.0", "1.7.0-rc3", "nightly"},
	)
	s := newFakeSyncer(t, f)

	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4-latest",
		Firmware: &ProfileFirmwareRef{Name: "core-image", Version: "latest"},
		Software: []ProfileSoftwareRef{{Name: "tedge", Type: "apt", Version: "latest"}},
	})
	require.NoError(t, err)

	profile := body["c8y_DeviceProfile"].(map[string]any)
	assert.Equal(t, map[string]any{
		"name":    "core-image",
		"version": "1.10.0",
		"url":     "https://example.com/c8y_Firmware/1.10.0",
	}, profile["firmware"], "the resolved version is written, not 'latest'")
	assert.Equal(t, []map[string]any{{
		"name":         "tedge",
		"version":      "1.7.0",
		"softwareType": "apt",
		"url":          "https://example.com/c8y_Software/1.7.0",
		"action":       "install",
	}}, profile["software"])
}

func TestBuildProfileBodyLatestIncludingPrereleases(t *testing.T) {
	f := fakeRepository(t, []string{"1.10.0", "2.0.0-rc1"}, nil)
	s := newFakeSyncer(t, f)

	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4-preview",
		Firmware: &ProfileFirmwareRef{Name: "core-image", Version: "latest", IncludePrereleases: true},
	})
	require.NoError(t, err)
	firmware := body["c8y_DeviceProfile"].(map[string]any)["firmware"].(map[string]any)
	assert.Equal(t, "2.0.0-rc1", firmware["version"])
}

func TestBuildProfileBodyLatestWithoutURL(t *testing.T) {
	f := fakeRepository(t, []string{"1.0.0", "1.1.0"}, nil)
	s := newFakeSyncer(t, f)

	// url: none still resolves the version, but leaves out the url
	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Firmware: &ProfileFirmwareRef{Name: "core-image", Version: "latest", URL: "none"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "core-image", "version": "1.1.0"},
		body["c8y_DeviceProfile"].(map[string]any)["firmware"])
}

func TestBuildProfileBodyLatestErrors(t *testing.T) {
	f := fakeRepository(t, []string{"2.0.0-rc1"}, nil)
	s := newFakeSyncer(t, f)

	_, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Firmware: &ProfileFirmwareRef{Name: "core-image", Version: "latest"},
	})
	assert.ErrorContains(t, err, "firmware core-image/latest: no version found to use as latest")

	_, err = s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Software: []ProfileSoftwareRef{{Name: "missing", Version: "latest"}},
	})
	assert.ErrorContains(t, err, "software missing/latest: software item not found")
}

func TestValidateIncludePrereleasesRequiresLatest(t *testing.T) {
	path := writeManifest(t, `deviceProfiles:
  - name: p
    firmware:
      name: core-image
      version: 1.0.0
      includePrereleases: true
    software:
      - name: tedge
        version: 1.0.0
        includePrereleases: true
`)
	_, err := LoadManifest(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deviceProfiles[0].firmware: includePrereleases requires version: latest")
	assert.Contains(t, err.Error(), "deviceProfiles[0].software[0]: includePrereleases requires version: latest")

	path = writeManifest(t, `deviceProfiles:
  - name: p
    firmware:
      name: core-image
      version: latest
      includePrereleases: true
`)
	_, err = LoadManifest(path)
	assert.NoError(t, err)
}
