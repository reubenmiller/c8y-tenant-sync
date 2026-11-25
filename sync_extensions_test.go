package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExtensionTenant serves the applications (web applications and
// extensions) of tenant t123, filtered like the platform by name, type,
// availability and hasVersions
func fakeExtensionTenant(t *testing.T, apps ...map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	f.Handle("GET /application/applications", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		found := []map[string]any{}
		for _, app := range apps {
			switch {
			case q.Get("name") != "" && app["name"] != q.Get("name"):
			case q.Get("type") != "" && app["type"] != q.Get("type"):
			case q.Get("availability") != "" && app["availability"] != q.Get("availability"):
			case q.Get("hasVersions") == "true" && app["applicationVersions"] == nil:
			default:
				found = append(found, app)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"applications": found, "statistics": stats()})
	})
	for _, app := range apps {
		app := app
		f.JSON("GET /application/applications/"+app["id"].(string), http.StatusOK, app)
		// The cumulocity.json of each extension version, which exports the
		// module <contextPath>Module
		versions, _ := app["applicationVersions"].([]map[string]any)
		for _, v := range versions {
			contextPath := app["contextPath"].(string)
			f.JSON("GET /apps/"+contextPath+"@"+v["version"].(string)+"/cumulocity.json", http.StatusOK, map[string]any{
				"name": app["name"], "contextPath": contextPath, "version": v["version"], "isPackage": true,
				"exports": []map[string]any{{"name": "Module", "module": contextPath + "Module", "path": "./module.ts"}},
			})
		}
	}
	return f
}

// extension is an uploaded extension with an active version. Like the
// platform's extensions, its application manifest does not list the modules.
func extension(id, name, contextPath, owner string, versions ...map[string]any) map[string]any {
	return map[string]any{
		"id": id, "name": name, "contextPath": contextPath, "type": "HOSTED",
		"owner":               map[string]any{"tenant": map[string]any{"id": owner}},
		"manifest":            map[string]any{"isPackage": true, "package": "plugin"},
		"activeVersionId":     "bin-active",
		"applicationVersions": versions,
	}
}

// inactive removes the active version of an extension (as uploaded by
// earlier versions of the tool)
func inactive(ext map[string]any) map[string]any {
	delete(ext, "activeVersionId")
	return ext
}

func extVersion(v string, tags ...string) map[string]any {
	return map[string]any{"version": v, "binaryId": "bin-" + v, "tags": tags}
}

// writeExtensionZip writes an extension archive with a cumulocity.json
func writeExtensionZip(t *testing.T, filename string, manifest map[string]any) string {
	path := filepath.Join(t.TempDir(), filename)
	file, err := os.Create(path)
	require.NoError(t, err)
	archive := zip.NewWriter(file)
	w, err := archive.Create("cumulocity.json")
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(w).Encode(manifest))
	require.NoError(t, archive.Close())
	require.NoError(t, file.Close())
	return path
}

func widgetZip(t *testing.T, name, v string) string {
	return writeExtensionZip(t, name+"-"+v+".zip", map[string]any{
		"name": name, "key": name + "-key", "contextPath": "my-widget",
		"version": v, "isPackage": true, "package": "plugin",
		"remotes": map[string]any{"my-widget": []string{"WidgetModule"}},
	})
}

func TestExtensionIsCreatedAndVersionUploaded(t *testing.T) {
	f := fakeExtensionTenant(t)
	f.JSON("POST /application/applications", http.StatusCreated, extension("77", "my-widget-plugin", "my-widget", "t123"))
	f.JSON("POST /application/applications/77/versions", http.StatusCreated, extVersion("1.2.0", "latest"))
	f.JSON("PUT /application/applications/77", http.StatusOK, map[string]any{"id": "77"})
	s := newFakeSyncer(t, f)

	spec := ExtensionSpec{Name: "my-widget-plugin", Source: Source{Path: widgetZip(t, "my-widget-plugin", "1.2.0")}}
	require.NoError(t, s.SyncExtensions(context.Background(), []ExtensionSpec{spec}))

	r := resultFor(t, s, SectionExtensions)
	assert.Equal(t, ActionCreated, r.Action, r.Err)
	assert.Equal(t, "created and uploaded version 1.2.0 (my-widget-plugin-1.2.0.zip)", r.Detail)

	created := exactRequests(f, http.MethodPost, "/application/applications")
	require.Len(t, created, 1)
	assert.Equal(t, "my-widget-plugin", created[0].Body["name"])
	assert.Equal(t, "my-widget", created[0].Body["contextPath"])
	assert.Equal(t, true, created[0].Body["manifest"].(map[string]any)["isPackage"])

	uploads := f.Requests(http.MethodPost, "/application/applications/77/versions")
	require.Len(t, uploads, 1)
	assert.Contains(t, string(uploads[0].RawBody), `"version":"1.2.0"`)
	assert.Contains(t, string(uploads[0].RawBody), `"tags":["latest"]`)

	// The latest version is activated, like extensions uploaded in the
	// Administration application
	activations := putsWith(f, "77", "activeVersionId")
	require.Len(t, activations, 1)
	assert.Equal(t, "bin-1.2.0", activations[0].Body["activeVersionId"])

	// Extensions belong to the tenant running the sync: another target does
	// not upload again
	require.NoError(t, s.SyncExtensions(context.Background(), []ExtensionSpec{spec}))
	assert.Len(t, f.Requests(http.MethodPost, "/application/applications/77/versions"), 1)
}

func TestExtensionVersionAlreadyUploadedIsUnchanged(t *testing.T) {
	f := fakeExtensionTenant(t,
		// An extension of the same name owned by another tenant is ignored
		extension("5", "my-widget-plugin", "my-widget", "management", extVersion("9.9.9", "latest")),
		extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.0.0"), extVersion("1.2.0", "latest")),
	)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncExtensions(context.Background(), []ExtensionSpec{
		{Name: "my-widget-plugin", Source: Source{Path: widgetZip(t, "my-widget-plugin", "1.2.0")}},
	}))

	r := resultFor(t, s, SectionExtensions)
	assert.Equal(t, ActionUnchanged, r.Action, r.Err)
	assert.Equal(t, "version 1.2.0 already uploaded", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications"))
}

func TestExtensionNewVersionIsUploaded(t *testing.T) {
	f := fakeExtensionTenant(t, extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.0.0", "latest")))
	f.JSON("POST /application/applications/77/versions", http.StatusCreated, extVersion("1.2.0", "stable"))
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncExtensions(context.Background(), []ExtensionSpec{
		{Name: "my-widget-plugin", Tags: []string{"stable"}, Source: Source{Path: widgetZip(t, "my-widget-plugin", "1.2.0")}},
	}))

	r := resultFor(t, s, SectionExtensions)
	assert.Equal(t, ActionUpdated, r.Action, r.Err)
	assert.Equal(t, "uploaded version 1.2.0 (my-widget-plugin-1.2.0.zip)", r.Detail)
	uploads := f.Requests(http.MethodPost, "/application/applications/77/versions")
	require.Len(t, uploads, 1)
	assert.Contains(t, string(uploads[0].RawBody), `"tags":["stable"]`)
	assert.Empty(t, exactRequests(f, http.MethodPost, "/application/applications"))
	assert.Empty(t, f.Requests(http.MethodPut, "/application/applications/77"), "a version without the latest tag is not activated")
}

func TestExtensionArchiveMustMatchTheName(t *testing.T) {
	f := fakeExtensionTenant(t)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncExtensions(context.Background(), []ExtensionSpec{
		{Name: "my-widget-plugin", Source: Source{Path: widgetZip(t, "other-plugin", "1.2.0")}},
	}))

	r := resultFor(t, s, SectionExtensions)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `contains the extension "other-plugin", not "my-widget-plugin"`)
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications"))
}

func ownWebApp(remotes map[string]any) map[string]any {
	return map[string]any{
		"id": "42", "name": "devicemanagement", "type": "HOSTED", "contextPath": "devicemanagement",
		"owner":  map[string]any{"tenant": map[string]any{"id": "t123"}},
		"config": map[string]any{"remotes": remotes, "other": "kept"},
	}
}

func TestApplicationExtensionsAreInstalledUpgradedAndRemoved(t *testing.T) {
	f := fakeExtensionTenant(t,
		ownWebApp(map[string]any{
			"my-widget@1.0.0":      []string{"my-widgetModule"},
			"old-widget@2.0.0":     []string{"old-widgetModule"},
			"hand-installed@1.0.0": []string{"hand-installedModule"},
			"pinned@1.0.0":         []string{"pinnedModule"},
			"legacy-remote":        []string{"LegacyModule"},
		}),
		extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.0.0"), extVersion("1.2.0", "latest")),
		extension("78", "old-widget-plugin", "old-widget", "t123", extVersion("2.0.0", "latest")),
		extension("79", "new-plugin", "new", "t123", extVersion("3.0.0", "latest")),
		extension("80", "pinned-plugin", "pinned", "t123", extVersion("1.0.0"), extVersion("1.1.0", "latest")),
		extension("81", "hand-installed", "hand-installed", "t123", extVersion("1.0.0", "latest")),
	)
	f.Handle("PUT /application/applications/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": "42"})
	})
	s := newFakeSyncer(t, f)

	notInstalled := false
	spec := ApplicationSpec{Name: "devicemanagement", Extensions: []ApplicationExtensionSpec{
		{Name: "my-widget-plugin"},                // latest: 1.0.0 -> 1.2.0
		{Name: "new-plugin", Version: "3.0.0"},    // installed
		{Name: "pinned-plugin", Version: "1.0.0"}, // already at the pinned version
		{Name: "old-widget-plugin", Installed: &notInstalled},
		{Name: "gone-plugin", Installed: &notInstalled},
	}}
	s.syncApplicationExtensions(context.Background(), spec)

	results := resultsByItem(s)
	assert.Equal(t, ActionUpdated, results["devicemanagement/my-widget-plugin"].Action, results["devicemanagement/my-widget-plugin"].Err)
	assert.Equal(t, "version 1.0.0 -> 1.2.0", results["devicemanagement/my-widget-plugin"].Detail)
	assert.Equal(t, ActionUpdated, results["devicemanagement/new-plugin"].Action)
	assert.Equal(t, "installed version 3.0.0", results["devicemanagement/new-plugin"].Detail)
	assert.Equal(t, ActionUnchanged, results["devicemanagement/pinned-plugin"].Action)
	assert.Equal(t, "version 1.0.0 installed", results["devicemanagement/pinned-plugin"].Detail)
	assert.Equal(t, ActionUpdated, results["devicemanagement/old-widget-plugin"].Action)
	assert.Equal(t, "removed version 2.0.0", results["devicemanagement/old-widget-plugin"].Detail)
	assert.Equal(t, ActionUnchanged, results["devicemanagement/gone-plugin"].Action)
	assert.Equal(t, "not installed", results["devicemanagement/gone-plugin"].Detail)

	puts := exactRequests(f, http.MethodPut, "/application/applications/42")
	require.Len(t, puts, 1, "the changes are written in one update")
	config := puts[0].Body["config"].(map[string]any)
	assert.Equal(t, "kept", config["other"])
	remotes := config["remotes"].(map[string]any)
	assert.Equal(t, []any{"my-widgetModule"}, remotes["my-widget@1.2.0"], "the modules come from the extension manifest")
	assert.Contains(t, remotes, "new@3.0.0")
	assert.Contains(t, remotes, "pinned@1.0.0")
	assert.Contains(t, remotes, "hand-installed@1.0.0", "extensions which are not listed are kept")
	assert.Contains(t, remotes, "legacy-remote", "remotes which are not extensions are kept")
	assert.NotContains(t, remotes, "my-widget@1.0.0")
	assert.NotContains(t, remotes, "old-widget@2.0.0")

	// The application belongs to the tenant running the sync: another target
	// does not change it again
	s.syncApplicationExtensions(context.Background(), spec)
	assert.Len(t, exactRequests(f, http.MethodPut, "/application/applications/42"), 1)
}

func TestApplicationExtensionsUnchangedNeedNoUpdate(t *testing.T) {
	f := fakeExtensionTenant(t,
		ownWebApp(map[string]any{"my-widget@1.2.0": []string{"my-widgetModule"}}),
		extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.2.0", "latest")),
	)
	s := newFakeSyncer(t, f)

	s.syncApplicationExtensions(context.Background(), ApplicationSpec{
		Name:       "devicemanagement",
		Extensions: []ApplicationExtensionSpec{{Name: "my-widget-plugin", Version: "latest"}},
	})
	r := resultFor(t, s, SectionApplications)
	assert.Equal(t, ActionUnchanged, r.Action, r.Err)
	assert.Empty(t, f.Requests(http.MethodPut, "/application/applications"))
}

func TestApplicationExtensionsRequireATenantOwnedApplication(t *testing.T) {
	builtin := ownWebApp(nil)
	builtin["owner"] = map[string]any{"tenant": map[string]any{"id": "management"}}
	f := fakeExtensionTenant(t, builtin, extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.2.0", "latest")))
	s := newFakeSyncer(t, f)

	s.syncApplicationExtensions(context.Background(), ApplicationSpec{
		Name:       "devicemanagement",
		Extensions: []ApplicationExtensionSpec{{Name: "my-widget-plugin"}},
	})
	r := resultFor(t, s, SectionApplications)
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, `no web application "devicemanagement" owned by tenant t123`)
	assert.ErrorContains(t, r.Err, "add a source")
	assert.Empty(t, f.Requests(http.MethodPut, "/application/applications"))
}

func TestApplicationExtensionsMissingVersionFails(t *testing.T) {
	f := fakeExtensionTenant(t,
		ownWebApp(map[string]any{}),
		extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.2.0", "latest")),
	)
	s := newFakeSyncer(t, f)

	s.syncApplicationExtensions(context.Background(), ApplicationSpec{
		Name: "devicemanagement",
		Extensions: []ApplicationExtensionSpec{
			{Name: "my-widget-plugin", Version: "2.0.0"},
			{Name: "unknown-plugin"},
		},
	})
	results := resultsByItem(s)
	assert.ErrorContains(t, results["devicemanagement/my-widget-plugin"].Err, `has no version or tag "2.0.0"`)
	assert.ErrorContains(t, results["devicemanagement/unknown-plugin"].Err, `extension "unknown-plugin" not found`)
	assert.Empty(t, f.Requests(http.MethodPut, "/application/applications"))
}

func TestApplicationExtensionsDryRun(t *testing.T) {
	s := &Syncer{DryRun: true}
	notInstalled := false
	s.syncApplicationExtensions(context.Background(), ApplicationSpec{
		Name: "devicemanagement",
		Extensions: []ApplicationExtensionSpec{
			{Name: "my-widget-plugin"},
			{Name: "old-plugin", Installed: &notInstalled},
		},
	})
	results := resultsByItem(s)
	assert.Equal(t, "install latest", results["devicemanagement/my-widget-plugin"].Detail)
	assert.Equal(t, "remove", results["devicemanagement/old-plugin"].Detail)
}

func TestLoadManifestApplicationSections(t *testing.T) {
	path := writeManifest(t, `
microservices:
  - name: advanced-software-mgmt
extensions:
  - name: my-widget-plugin
    source:
      path: ./my-widget-plugin.zip
applications:
  - name: devicemanagement
    source:
      path: ./devicemanagement.zip
    extensions:
      - my-widget-plugin
      - other-plugin@1.2.0
      - name: old-plugin
        installed: false
`)
	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	assert.Equal(t, []MicroserviceSpec{{Name: "advanced-software-mgmt"}}, manifest.Microservices)
	require.Len(t, manifest.Extensions, 1)
	assert.Equal(t, "my-widget-plugin", manifest.Extensions[0].Name)
	require.Len(t, manifest.Applications, 1)
	exts := manifest.Applications[0].Extensions
	require.Len(t, exts, 3)
	assert.Equal(t, ApplicationExtensionSpec{Name: "my-widget-plugin"}, exts[0])
	assert.Equal(t, "latest", exts[0].VersionOrLatest())
	assert.Equal(t, ApplicationExtensionSpec{Name: "other-plugin", Version: "1.2.0"}, exts[1])
	assert.Equal(t, "old-plugin", exts[2].Name)
	assert.False(t, exts[2].IsInstalled())
}

func TestLoadManifestApplicationSectionErrors(t *testing.T) {
	testCases := []struct {
		name, content, errorMsg string
	}{
		{
			name: "microservice in applications",
			content: `
applications:
  - name: my-service
    type: MICROSERVICE
`,
			errorMsg: `move the microservice "my-service" to the microservices section`,
		},
		{
			name: "type on a web application",
			content: `
applications:
  - name: devicemanagement
    type: HOSTED
`,
			errorMsg: "the applications section only holds web applications, remove type",
		},
		{
			name: "duplicate application extension",
			content: `
applications:
  - name: devicemanagement
    extensions: [a, a@1.0.0]
`,
			errorMsg: `duplicate extension "a"`,
		},
		{
			name: "version with installed false",
			content: `
applications:
  - name: devicemanagement
    extensions:
      - {name: a, version: 1.0.0, installed: false}
`,
			errorMsg: "version can't be used with installed: false",
		},
		{
			name: "unknown extension field",
			content: `
applications:
  - name: devicemanagement
    extensions:
      - {name: a, tag: latest}
`,
			errorMsg: "field tag not found",
		},
		{
			name: "extension without source",
			content: `
extensions:
  - name: my-widget-plugin
`,
			errorMsg: "extensions[0]",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadManifest(writeManifest(t, tc.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errorMsg)
		})
	}
}

func TestExtensionWithoutActiveVersionIsActivated(t *testing.T) {
	f := fakeExtensionTenant(t, inactive(extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.0.0"), extVersion("1.2.0", "latest"))))
	f.JSON("PUT /application/applications/77", http.StatusOK, map[string]any{"id": "77"})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncExtensions(context.Background(), []ExtensionSpec{
		{Name: "my-widget-plugin", Source: Source{Path: widgetZip(t, "my-widget-plugin", "1.2.0")}},
	}))

	r := resultFor(t, s, SectionExtensions)
	assert.Equal(t, ActionUpdated, r.Action, r.Err)
	assert.Equal(t, "version 1.2.0 already uploaded; activated version 1.2.0", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications"), "no version is uploaded")
	activations := putsWith(f, "77", "activeVersionId")
	require.Len(t, activations, 1)
	assert.Equal(t, "bin-1.2.0", activations[0].Body["activeVersionId"])
}

func TestApplicationExtensionWithWrongModulesIsReinstalled(t *testing.T) {
	f := fakeExtensionTenant(t,
		// Installed by an earlier version of the tool without modules
		ownWebApp(map[string]any{"my-widget@1.2.0": []string{}}),
		extension("77", "my-widget-plugin", "my-widget", "t123", extVersion("1.2.0", "latest")),
	)
	f.JSON("PUT /application/applications/42", http.StatusOK, map[string]any{"id": "42"})
	s := newFakeSyncer(t, f)

	s.syncApplicationExtensions(context.Background(), ApplicationSpec{
		Name:       "devicemanagement",
		Extensions: []ApplicationExtensionSpec{{Name: "my-widget-plugin"}},
	})
	r := resultFor(t, s, SectionApplications)
	assert.Equal(t, ActionUpdated, r.Action, r.Err)
	assert.Equal(t, "version 1.2.0: modules updated (my-widgetModule)", r.Detail)
	puts := exactRequests(f, http.MethodPut, "/application/applications/42")
	require.Len(t, puts, 1)
	remotes := puts[0].Body["config"].(map[string]any)["remotes"].(map[string]any)
	assert.Equal(t, []any{"my-widgetModule"}, remotes["my-widget@1.2.0"])
}

func TestExtensionModulesFallBackToRemotes(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /apps/old-style@1.0.0/cumulocity.json", http.StatusOK, map[string]any{
		"remotes": map[string]any{"old-style": []string{"B", "A"}},
	})
	s := newFakeSyncer(t, f)

	modules, err := s.extensionModules(context.Background(), "old-style", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, []string{"A", "B"}, modules)

	_, err = s.extensionModules(context.Background(), "missing", "1.0.0")
	assert.ErrorContains(t, err, "read the cumulocity.json of missing@1.0.0")
}
