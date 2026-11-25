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
		"self":     url + "/application/applications/1",
		"owner":    map[string]any{"tenant": map[string]any{"id": "management"}},
		"manifest": map[string]any{"version": "1025.14.0"},
	}
	all := append([]map[string]any{builtin}, apps...)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{
		"name":         "t123",
		"applications": map[string]any{"references": []map[string]any{{"application": builtin}}},
	})
	f.JSON("GET /application/applications", http.StatusOK, map[string]any{"applications": all, "statistics": stats()})
	f.JSON("POST /application/applications", http.StatusCreated, map[string]any{
		"id": "99", "name": "devicemanagement", "type": "HOSTED",
		"self":  url + "/application/applications/99",
		"owner": map[string]any{"tenant": map[string]any{"id": "t123"}},
	})
	for _, id := range []string{"99", "42"} {
		f.JSON("POST /application/applications/"+id+"/binaries", http.StatusCreated, map[string]any{"id": id})
	}
	return f
}

func hostedSpec() ApplicationSpec {
	return ApplicationSpec{
		Name: "devicemanagement",
		Type: "HOSTED",
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
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants/t123/applications"), "devicemanagement is already subscribed")
}

func ownDeviceManagement(url, version string) map[string]any {
	return map[string]any{
		"id": "42", "name": "devicemanagement", "type": "HOSTED", "contextPath": "devicemanagement",
		"self":     url + "/application/applications/42",
		"owner":    map[string]any{"tenant": map[string]any{"id": "t123"}},
		"manifest": map[string]any{"version": version},
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
