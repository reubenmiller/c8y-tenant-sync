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

func TestParseVersionSelector(t *testing.T) {
	for _, valid := range []string{"latest", "LATEST", " latest ", "latest-1", "latest-10", "^1.2", "~1.6.0", ">=1.0, <2.0", ">=1.0 <2.0", "1.x", "1.2.3", "*"} {
		_, err := ParseVersionSelector(valid)
		assert.NoError(t, err, valid)
	}

	tests := map[string]string{
		"":          "versionSelector is empty",
		"latest-0":  "latest-N requires a positive number",
		"latest-x":  "latest-N requires a positive number",
		"latest--1": "latest-N requires a positive number",
		"newest":    "expected latest, latest-N or a semver constraint",
		">=one":     "expected latest, latest-N or a semver constraint",
	}
	for value, wantErr := range tests {
		_, err := ParseVersionSelector(value)
		assert.ErrorContains(t, err, wantErr, value)
	}
}

func TestVersionSelectorSelect(t *testing.T) {
	tests := []struct {
		name               string
		selector           string
		versions           []repositoryVersion
		includePrereleases bool
		want               string
		wantErr            string
	}{
		// latest
		{name: "semver order, not string order", selector: "latest", versions: versions("1.9.0", "1.10.0", "1.2.0"), want: "1.10.0"},
		{name: "v prefix and short versions", selector: "latest", versions: versions("v1.2.3", "1.3", "1.2.10"), want: "1.3"},
		{name: "build timestamps", selector: "latest", versions: versions("20261104.2250", "20261021.1010", "20261104.0900"), want: "20261104.2250"},
		{name: "a version literally named latest is not semver", selector: "latest", versions: versions("latest", "1.0.0"), want: "1.0.0"},
		{name: "prereleases excluded by default", selector: "latest", versions: versions("1.0.0", "1.1.0-rc1"), want: "1.0.0"},
		{name: "prereleases included", selector: "latest", versions: versions("1.0.0", "1.1.0-rc1", "1.1.0-alpha"), includePrereleases: true, want: "1.1.0-rc1"},
		{name: "release beats its prerelease", selector: "latest", versions: versions("1.1.0-rc1", "1.1.0"), includePrereleases: true, want: "1.1.0"},
		{name: "build metadata", selector: "latest", versions: versions("1.2.3+build.5", "1.2.2"), want: "1.2.3+build.5"},
		{name: "equal versions keep the first", selector: "latest", versions: versions("1.2", "1.2.0"), want: "1.2"},

		// latest-N
		{name: "second latest", selector: "latest-1", versions: versions("1.0.0", "3.0.0", "2.0.0"), want: "2.0.0"},
		{name: "third latest", selector: "latest-2", versions: versions("1.0.0", "3.0.0", "2.0.0"), want: "1.0.0"},
		{name: "equal versions count once", selector: "latest-1", versions: versions("2.0", "2.0.0", "1.0.0"), want: "1.0.0"},
		{name: "prereleases are skipped for latest-N", selector: "latest-1", versions: versions("2.0.0", "2.1.0-rc1", "1.0.0"), want: "1.0.0"},
		{name: "not enough versions", selector: "latest-3", versions: versions("1.0.0", "2.0.0"), wantErr: "versionSelector latest-3 needs at least 4 versions, but only 2 qualify"},

		// constraints
		{name: "caret", selector: "^1.2", versions: versions("1.1.0", "1.2.5", "1.9.0", "2.0.0"), want: "1.9.0"},
		{name: "tilde", selector: "~1.2.0", versions: versions("1.2.0", "1.2.9", "1.3.0"), want: "1.2.9"},
		{name: "range", selector: ">=1.0, <2.0", versions: versions("0.9.0", "1.5.0", "2.0.0"), want: "1.5.0"},
		{name: "wildcard", selector: "1.x", versions: versions("1.4.0", "2.0.0"), want: "1.4.0"},
		{name: "exact semver match", selector: "1.2.3", versions: versions("v1.2.3", "1.2.4"), want: "v1.2.3"},
		{name: "prerelease matched by its release", selector: "^1.0", versions: versions("1.4.0", "1.5.0-rc1", "2.0.0-rc1"), includePrereleases: true, want: "1.5.0-rc1"},
		{name: "upper bound excludes the next major's prereleases", selector: "<2.0.0", versions: versions("1.9.0", "2.0.0-rc1"), includePrereleases: true, want: "1.9.0"},
		{name: "nothing matches", selector: "^3", versions: versions("1.0.0", "2.0.0"), wantErr: "no version matches versionSelector ^3 (2 version(s), 2 not matching ^3)"},

		// errors
		{name: "no versions", selector: "latest", versions: nil, wantErr: "no version matches versionSelector latest (0 version(s))"},
		{name: "only invalid", selector: "latest", versions: versions("nightly", "dev"), wantErr: "2 not valid semver"},
		{name: "only prereleases", selector: "latest", versions: versions("1.0.0-rc1"), wantErr: "1 prerelease(s) excluded (set includePrereleases: true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector, err := ParseVersionSelector(tt.selector)
			require.NoError(t, err)
			got, err := selector.Select(tt.versions, tt.includePrereleases)
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

func TestResolveVersion(t *testing.T) {
	listCalled, lookupCalled := 0, 0
	list := func() ([]repositoryVersion, error) { listCalled++; return versions("1.0.0", "2.0.0"), nil }
	lookup := func() (string, error) { lookupCalled++; return "https://example.com/exact", nil }

	// An exact version is used as-is (even "latest") with the lazy lookup
	version, resolvedLookup, err := resolveVersion("latest", "", false, list, lookup)
	require.NoError(t, err)
	assert.Equal(t, "latest", version)
	assert.Equal(t, 0, listCalled)
	url, _ := resolvedLookup()
	assert.Equal(t, "https://example.com/exact", url)

	// A selector lists the versions once and uses the url of the selected one
	version, resolvedLookup, err = resolveVersion("", "latest-1", false, list, lookup)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", version)
	url, _ = resolvedLookup()
	assert.Equal(t, "https://example.com/1.0.0", url)
	assert.Equal(t, 1, listCalled)
	assert.Equal(t, 1, lookupCalled)

	// Errors
	_, _, err = resolveVersion("", "latest", false, func() ([]repositoryVersion, error) { return nil, errors.New("boom") }, lookup)
	assert.ErrorContains(t, err, "boom")
	_, _, err = resolveVersion("", "newest", false, list, lookup)
	assert.ErrorContains(t, err, "invalid versionSelector")
}

func TestVersionLabel(t *testing.T) {
	assert.Equal(t, "1.0.0", versionLabel("1.0.0", ""))
	assert.Equal(t, "versionSelector ^1.2", versionLabel("", "^1.2"))
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
			if version := extractVersionFilter(query, "c8y_Firmware.version"); version != "" {
				items = filterBinaries(items, "c8y_Firmware", version)
			}
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

// extractVersionFilter returns the value of "<field> eq '<value>'" in an
// inventory query
func extractVersionFilter(query, field string) string {
	marker := field + " eq '"
	start := strings.Index(query, marker)
	if start < 0 {
		return ""
	}
	rest := query[start+len(marker):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func filterBinaries(items []map[string]any, fragment, version string) []map[string]any {
	var out []map[string]any
	for _, item := range items {
		if item[fragment].(map[string]any)["version"] == version {
			out = append(out, item)
		}
	}
	return out
}

func TestBuildProfileBodyVersionSelectors(t *testing.T) {
	f := fakeRepository(t,
		[]string{"1.9.0", "1.10.0", "2.0.0-rc1"},
		[]string{"1.6.2", "1.7.0", "1.7.0-rc3", "2.0.0", "nightly"},
	)
	s := newFakeSyncer(t, f)

	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4-latest",
		Firmware: &ProfileFirmwareRef{Name: "core-image", VersionSelector: "latest"},
		Software: []ProfileSoftwareRef{{Name: "tedge", Type: "apt", VersionSelector: "^1.6"}},
	})
	require.NoError(t, err)

	profile := body["c8y_DeviceProfile"].(map[string]any)
	assert.Equal(t, map[string]any{
		"name":    "core-image",
		"version": "1.10.0",
		"url":     "https://example.com/c8y_Firmware/1.10.0",
	}, profile["firmware"], "the resolved version is written, not the selector")
	assert.Equal(t, []map[string]any{{
		"name":         "tedge",
		"version":      "1.7.0",
		"softwareType": "apt",
		"url":          "https://example.com/c8y_Software/1.7.0",
		"action":       "install",
	}}, profile["software"])
}

func TestBuildProfileBodyLiteralLatestVersion(t *testing.T) {
	// A version which is literally called "latest" is looked up as-is
	f := fakeRepository(t, []string{"1.0.0", "latest"}, nil)
	s := newFakeSyncer(t, f)

	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Firmware: &ProfileFirmwareRef{Name: "core-image", Version: "latest"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"name":    "core-image",
		"version": "latest",
		"url":     "https://example.com/c8y_Firmware/latest",
	}, body["c8y_DeviceProfile"].(map[string]any)["firmware"])
}

func TestBuildProfileBodySelectorIncludingPrereleases(t *testing.T) {
	f := fakeRepository(t, []string{"1.10.0", "2.0.0-rc1"}, nil)
	s := newFakeSyncer(t, f)

	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4-preview",
		Firmware: &ProfileFirmwareRef{Name: "core-image", VersionSelector: "latest", IncludePrereleases: true},
	})
	require.NoError(t, err)
	firmware := body["c8y_DeviceProfile"].(map[string]any)["firmware"].(map[string]any)
	assert.Equal(t, "2.0.0-rc1", firmware["version"])
}

func TestBuildProfileBodySelectorWithoutURL(t *testing.T) {
	f := fakeRepository(t, []string{"1.0.0", "1.1.0"}, nil)
	s := newFakeSyncer(t, f)

	// url: none still resolves the version, but leaves out the url
	body, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Firmware: &ProfileFirmwareRef{Name: "core-image", VersionSelector: "latest-1", URL: "none"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"name": "core-image", "version": "1.0.0"},
		body["c8y_DeviceProfile"].(map[string]any)["firmware"])
}

func TestBuildProfileBodySelectorErrors(t *testing.T) {
	f := fakeRepository(t, []string{"2.0.0-rc1"}, nil)
	s := newFakeSyncer(t, f)

	_, err := s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Firmware: &ProfileFirmwareRef{Name: "core-image", VersionSelector: "latest"},
	})
	assert.ErrorContains(t, err, "firmware core-image (versionSelector latest): no version matches versionSelector latest")

	_, err = s.buildProfileBody(context.Background(), DeviceProfileSpec{
		Name:     "rpi4",
		Software: []ProfileSoftwareRef{{Name: "missing", VersionSelector: "latest"}},
	})
	assert.ErrorContains(t, err, "software missing (versionSelector latest): software item not found")
}

func TestValidateVersionRefs(t *testing.T) {
	path := writeManifest(t, `deviceProfiles:
  - name: p
    firmware:
      name: core-image
      version: 1.0.0
      versionSelector: latest
    software:
      - name: tedge
        version: 1.0.0
        includePrereleases: true
      - name: agent
        versionSelector: newest
      - name: missing-version
`)
	_, err := LoadManifest(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deviceProfiles[0].firmware: version and versionSelector are mutually exclusive")
	assert.Contains(t, err.Error(), "deviceProfiles[0].software[0]: includePrereleases requires versionSelector")
	assert.Contains(t, err.Error(), `deviceProfiles[0].software[1]: invalid versionSelector "newest"`)
	assert.Contains(t, err.Error(), "deviceProfiles[0].software[2]: version or versionSelector is required")

	path = writeManifest(t, `deviceProfiles:
  - name: p
    firmware:
      name: core-image
      versionSelector: latest-1
      includePrereleases: true
    software:
      - name: tedge
        versionSelector: ">=1.0, <2.0"
      - name: literal
        version: latest
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, "latest", manifest.DeviceProfiles[0].Software[1].Version)
}
