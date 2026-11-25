package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWebAppReleases serves the latest release of a web application
// repository (acme/devicemanagement) with a single zip asset and counts the
// asset downloads
func fakeWebAppReleases(t *testing.T, version string) (api string, downloads *int) {
	t.Helper()
	count := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/devicemanagement/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v" + version,
				"assets": []map[string]any{{
					"name":                 "devicemanagement-" + version + ".zip",
					"browser_download_url": "https://example.invalid/devicemanagement-" + version + ".zip",
					"url":                  server.URL + "/asset",
				}},
			})
		case "/asset":
			count++
			_, _ = w.Write([]byte("PK zip"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, &count
}

// fakeHostedTenant serves the current tenant t123 subscribed to the built-in
// devicemanagement application (owned by management) plus the given
// applications, and accepts application creation and binary uploads
func fakeHostedTenant(t *testing.T, apps ...map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	url := f.server.URL
	builtin := map[string]any{
		"id": "1", "name": "devicemanagement", "type": "HOSTED", "contextPath": "devicemanagement",
		"self":        url + "/application/applications/1",
		"owner":       map[string]any{"tenant": map[string]any{"id": "management"}},
		"manifest":    map[string]any{"version": "1025.14.0"},
		"description": builtinDescription,
	}
	all := append([]map[string]any{builtin}, apps...)
	// The tenant is subscribed to the built-in application and its own copies
	refs := []map[string]any{}
	for _, app := range all {
		refs = append(refs, map[string]any{"application": app})
	}
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{
		"name":         "t123",
		"applications": map[string]any{"references": refs},
	})
	f.JSON("POST /tenant/tenants/t123/applications", http.StatusCreated, map[string]any{})
	f.JSON("GET /application/applications", http.StatusOK, map[string]any{"applications": all, "statistics": stats()})
	f.JSON("POST /application/applications", http.StatusCreated, map[string]any{
		"id": "99", "name": "devicemanagement", "type": "HOSTED",
		"self":  url + "/application/applications/99",
		"owner": map[string]any{"tenant": map[string]any{"id": "t123"}},
	})
	for _, app := range apps {
		app := app
		f.JSON("GET /application/applications/"+app["id"].(string), http.StatusOK, app)
	}
	for _, id := range []string{"99", "42"} {
		f.JSON("POST /application/applications/"+id+"/binaries", http.StatusCreated, map[string]any{"id": "bin-" + id})
		f.JSON("PUT /application/applications/"+id, http.StatusOK, map[string]any{
			"id": id, "name": "devicemanagement", "type": "HOSTED",
			"self":  url + "/application/applications/" + id,
			"owner": map[string]any{"tenant": map[string]any{"id": "t123"}},
		})
	}
	return f
}

const builtinDescription = "The Device Management application"

// putsWith returns the PUT requests to the application with the given field
func putsWith(f *fakeC8y, id, field string) []recordedRequest {
	var out []recordedRequest
	for _, r := range exactRequests(f, http.MethodPut, "/application/applications/"+id) {
		if _, ok := r.Body[field]; ok {
			out = append(out, r)
		}
	}
	return out
}

func hostedSpec() ApplicationSpec {
	return ApplicationSpec{
		Name: "devicemanagement",
		Source: &Source{GitHub: &GitHubSource{
			Repo:   "acme/devicemanagement",
			Assets: []string{"devicemanagement-*.zip"},
		}},
	}
}

func hostedSyncer(t *testing.T, f *fakeC8y, version string) (*Syncer, *int) {
	api, downloads := fakeWebAppReleases(t, version)
	s := newFakeSyncer(t, f)
	s.Resolver.GitHubAPI = api
	return s, downloads
}

func TestHostedApplicationCreatesTenantOwnedCopyOfBuiltin(t *testing.T) {
	f := fakeHostedTenant(t)
	s, downloads := hostedSyncer(t, f, "1025.15.3")

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{hostedSpec()}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionCreated, r.Action, r.Detail)
	assert.Contains(t, r.Detail, "tenant-owned copy")
	assert.Contains(t, r.Detail, "devicemanagement-1025.15.3.zip")

	created := exactRequests(f, http.MethodPost, "/application/applications")
	require.Len(t, created, 1)
	assert.Equal(t, "devicemanagement", created[0].Body["contextPath"])
	assert.Equal(t, "HOSTED", created[0].Body["type"])
	assert.Equal(t, "t123-devicemanagement-application-key", created[0].Body["key"], "the built-in key is taken")
	assert.Len(t, f.Requests(http.MethodPost, "/application/applications/99/binaries"), 1)
	assert.Equal(t, 1, *downloads)
	activations := putsWith(f, "99", "activeVersionId")
	require.Len(t, activations, 1, "the uploaded binary is activated")
	assert.Equal(t, "bin-99", activations[0].Body["activeVersionId"])
	descriptions := putsWith(f, "99", "description")
	require.Len(t, descriptions, 1, "the description of the built-in application is inherited")
	assert.Equal(t, builtinDescription, descriptions[0].Body["description"])
	assert.Contains(t, r.Detail, "description inherited from the application of tenant management")

	// The subscription to the built-in application does not count
	subscribe := f.Requests(http.MethodPost, "/tenant/tenants/t123/applications")
	require.Len(t, subscribe, 1, "the tenant is subscribed to its copy")
	assert.Contains(t, subscribe[0].Body["application"].(map[string]any)["self"], "/application/applications/99")
	assert.Equal(t, ActionUpdated, s.Results[len(s.Results)-1].Action)
}

func ownDeviceManagement(url, version string) map[string]any {
	return map[string]any{
		"id": "42", "name": "devicemanagement", "type": "HOSTED", "contextPath": "devicemanagement",
		"self":        url + "/application/applications/42",
		"owner":       map[string]any{"tenant": map[string]any{"id": "t123"}},
		"manifest":    map[string]any{"version": version},
		"description": builtinDescription,
	}
}

func TestHostedApplicationAtSourceVersionIsUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeHostedTenant(t, ownDeviceManagement(f.server.URL, "1025.15.3"))
	s, downloads := hostedSyncer(t, f, "1025.15.3")

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{hostedSpec()}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUnchanged, r.Action, r.Detail)
	assert.Equal(t, "version 1025.15.3 already active", r.Detail)
	assert.Equal(t, 0, *downloads, "the archive is not downloaded when the version is active")
	assert.Empty(t, exactRequests(f, http.MethodPost, "/application/applications"))
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications/42/binaries"))
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants/t123/applications"), "the copy is already subscribed")
}

func TestHostedApplicationUploadsNewerSourceVersion(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeHostedTenant(t, ownDeviceManagement(f.server.URL, "1025.14.0"))
	s, downloads := hostedSyncer(t, f, "1025.15.3")

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{hostedSpec()}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUpdated, r.Action, r.Detail)
	assert.Equal(t, "version 1025.14.0 -> 1025.15.3 (uploaded devicemanagement-1025.15.3.zip)", r.Detail)
	assert.Equal(t, 1, *downloads)
	assert.Empty(t, exactRequests(f, http.MethodPost, "/application/applications"), "the existing copy is reused")
	assert.Len(t, f.Requests(http.MethodPost, "/application/applications/42/binaries"), 1)
	activations := putsWith(f, "42", "activeVersionId")
	require.Len(t, activations, 1)
	assert.Equal(t, "bin-42", activations[0].Body["activeVersionId"])
}

func TestHostedApplicationForceReuploadsSameVersion(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeHostedTenant(t, ownDeviceManagement(f.server.URL, "1025.15.3"))
	s, _ := hostedSyncer(t, f, "1025.15.3")
	s.Force = true

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{hostedSpec()}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUpdated, r.Action, r.Detail)
	assert.Equal(t, "re-uploaded devicemanagement-1025.15.3.zip", r.Detail)
}

func TestArchiveVersion(t *testing.T) {
	assert.Equal(t, "1025.15.3", archiveVersion(ResolvedFile{Filename: "devicemanagement-1025.15.3.zip"}))
	assert.Equal(t, "1.2.0-rc.1", archiveVersion(ResolvedFile{Filename: "app-1.2.0-rc.1.zip"}))
	assert.Equal(t, "2.0.0", archiveVersion(ResolvedFile{Filename: "app.zip", VersionHint: "v2.0.0"}))
	assert.Equal(t, "", archiveVersion(ResolvedFile{Filename: "app.zip"}))
}

// firstResult returns the first result of the item (the subscription step
// records a second one)
func firstResult(s *Syncer, item string) ChangeResult {
	for _, r := range s.Results {
		if r.Item == item {
			return r
		}
	}
	return ChangeResult{}
}

// exactRequests returns the requests to exactly the given path
func exactRequests(f *fakeC8y, method, path string) []recordedRequest {
	var out []recordedRequest
	for _, r := range f.Requests(method, path) {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func TestHostedApplicationKeepsTheLatestBinaries(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeHostedTenant(t, ownDeviceManagement(f.server.URL, "1025.14.0"))
	f.JSON("GET /application/applications/42/binaries", http.StatusOK, map[string]any{
		"attachments": []map[string]any{
			{"id": "old-1", "created": "2026-01-01T00:00:00Z"},
			{"id": "old-2", "created": "2026-06-01T00:00:00Z"},
			{"id": "bin-42", "created": "2026-11-25T00:00:00Z"},
		},
		"statistics": stats(),
	})
	f.JSON("DELETE /application/applications/42/binaries/old-1", http.StatusNoContent, nil)
	s, _ := hostedSyncer(t, f, "1025.15.3")
	spec := hostedSpec()
	spec.KeepBinaries = 2

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{spec}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUpdated, r.Action, r.Detail)
	assert.Equal(t, "version 1025.14.0 -> 1025.15.3 (uploaded devicemanagement-1025.15.3.zip); deleted 1 old binaries", r.Detail)
	assert.Len(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/old-1"), 1, "the oldest binary is deleted")
	assert.Empty(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/old-2"))
	assert.Empty(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/bin-42"))
}

func TestKeepBinariesValidation(t *testing.T) {
	spec := hostedSpec()
	spec.KeepBinaries = -1
	err := (&Manifest{Applications: []ApplicationSpec{spec}}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keepBinaries must not be negative")

	err = (&Manifest{Applications: []ApplicationSpec{{Name: "svc", KeepBinaries: 2}}}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keepBinaries requires a source")

	spec.KeepBinaries = 3
	assert.NoError(t, (&Manifest{Applications: []ApplicationSpec{spec}}).Validate())
}

func TestHostedApplicationFillsInMissingDescription(t *testing.T) {
	f := newFakeC8y(t)
	own := ownDeviceManagement(f.server.URL, "1025.15.3")
	delete(own, "description")
	f = fakeHostedTenant(t, own)
	s, _ := hostedSyncer(t, f, "1025.15.3")

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{hostedSpec()}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUpdated, r.Action, r.Detail)
	assert.Equal(t, "description inherited from the application of tenant management; version 1025.15.3 already active", r.Detail)
	descriptions := putsWith(f, "42", "description")
	require.Len(t, descriptions, 1)
	assert.Equal(t, builtinDescription, descriptions[0].Body["description"])
}

func TestHostedApplicationKeepsCustomisedDescription(t *testing.T) {
	f := newFakeC8y(t)
	own := ownDeviceManagement(f.server.URL, "1025.15.3")
	own["description"] = "Changed in the UI"
	f = fakeHostedTenant(t, own)
	s, _ := hostedSyncer(t, f, "1025.15.3")

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{hostedSpec()}))

	assert.Equal(t, ActionUnchanged, firstResult(s, "devicemanagement").Action)
	assert.Empty(t, putsWith(f, "42", "description"), "an inherited description does not overwrite an existing one")
}

func TestHostedApplicationExplicitDescription(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeHostedTenant(t, ownDeviceManagement(f.server.URL, "1025.15.3"))
	s, _ := hostedSyncer(t, f, "1025.15.3")
	spec := hostedSpec()
	spec.Description = "Device Management (ACME build)"

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{spec}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUpdated, r.Action, r.Detail)
	assert.Equal(t, "description updated; version 1025.15.3 already active", r.Detail)
	descriptions := putsWith(f, "42", "description")
	require.Len(t, descriptions, 1)
	assert.Equal(t, "Device Management (ACME build)", descriptions[0].Body["description"])
}

func TestDescriptionRequiresSource(t *testing.T) {
	err := (&Manifest{Applications: []ApplicationSpec{{Name: "svc", Description: "x"}}}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "description requires a source")
}

// fakeBinaries serves the binaries of application 42 and accepts deleting any
// of them
func fakeBinaries(f *fakeC8y, ids ...string) {
	attachments := []map[string]any{}
	for i, id := range ids {
		attachments = append(attachments, map[string]any{"id": id, "created": "2026-0" + string(rune('1'+i)) + "-01T00:00:00Z"})
		f.JSON("DELETE /application/applications/42/binaries/"+id, http.StatusNoContent, nil)
	}
	f.JSON("GET /application/applications/42/binaries", http.StatusOK, map[string]any{"attachments": attachments, "statistics": stats()})
}

func TestHostedApplicationRotatesBinariesWhenUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	own := ownDeviceManagement(f.server.URL, "1025.15.3")
	own["activeVersionId"] = "b3"
	f = fakeHostedTenant(t, own)
	fakeBinaries(f, "b1", "b2", "b3") // oldest first
	s, downloads := hostedSyncer(t, f, "1025.15.3")
	spec := hostedSpec()
	spec.KeepBinaries = 1

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{spec}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUpdated, r.Action, r.Detail)
	assert.Equal(t, "version 1025.15.3 already active; deleted 2 old binaries", r.Detail)
	assert.Equal(t, 0, *downloads)
	assert.Len(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/b1"), 1)
	assert.Len(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/b2"), 1)
	assert.Empty(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/b3"), "the active binary is kept")
}

func TestHostedApplicationRotationKeepsOldestActiveBinary(t *testing.T) {
	f := newFakeC8y(t)
	own := ownDeviceManagement(f.server.URL, "1025.15.3")
	own["activeVersionId"] = "b1"
	f = fakeHostedTenant(t, own)
	fakeBinaries(f, "b1", "b2", "b3")
	s, _ := hostedSyncer(t, f, "1025.15.3")
	spec := hostedSpec()
	spec.KeepBinaries = 2

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{spec}))

	assert.Equal(t, "version 1025.15.3 already active; deleted 1 old binaries", firstResult(s, "devicemanagement").Detail)
	assert.Empty(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/b1"), "the active binary is kept even when it is the oldest")
	assert.Len(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/b2"), 1)
	assert.Empty(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/b3"))
}

func TestHostedApplicationWithinKeepBinariesIsUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	own := ownDeviceManagement(f.server.URL, "1025.15.3")
	own["activeVersionId"] = "b2"
	f = fakeHostedTenant(t, own)
	fakeBinaries(f, "b1", "b2")
	s, _ := hostedSyncer(t, f, "1025.15.3")
	spec := hostedSpec()
	spec.KeepBinaries = 3

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{spec}))

	r := firstResult(s, "devicemanagement")
	assert.Equal(t, ActionUnchanged, r.Action, r.Detail)
	assert.Empty(t, f.Requests(http.MethodDelete, "/application/applications/42/binaries/"))
}
