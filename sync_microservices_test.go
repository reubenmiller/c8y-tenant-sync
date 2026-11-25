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

// fakeMicroserviceTenant serves tenant t123 with the given microservices
// (none subscribed) and accepts creating, uploading and subscribing
func fakeMicroserviceTenant(t *testing.T, apps ...map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	url := f.server.URL
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{
		"name": "t123", "applications": map[string]any{"references": []any{}},
	})
	f.Handle("GET /application/applications", func(w http.ResponseWriter, r *http.Request) {
		found := []map[string]any{}
		for _, app := range apps {
			if name := r.URL.Query().Get("name"); name == "" || app["name"] == name {
				found = append(found, app)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"applications": found, "statistics": stats()})
	})
	f.JSON("POST /application/applications", http.StatusCreated, map[string]any{
		"id": "50", "name": "my-service", "type": "MICROSERVICE",
		"self": url + "/application/applications/50", "owner": map[string]any{"tenant": map[string]any{"id": "t123"}},
	})
	for _, app := range apps {
		f.JSON("GET /application/applications/"+app["id"].(string), http.StatusOK, app)
	}
	for _, id := range []string{"50", "51"} {
		f.JSON("POST /application/applications/"+id+"/binaries", http.StatusCreated, map[string]any{"id": id})
	}
	f.JSON("POST /tenant/tenants/t123/applications", http.StatusCreated, map[string]any{})
	return f
}

func microservice(url string, fields map[string]any) map[string]any {
	app := map[string]any{
		"id": "51", "name": "my-service", "type": "MICROSERVICE",
		"self": url + "/application/applications/51", "owner": map[string]any{"tenant": map[string]any{"id": "t123"}},
	}
	for k, v := range fields {
		app[k] = v
	}
	return app
}

func serviceZip(t *testing.T, filename string) string {
	path := filepath.Join(t.TempDir(), filename)
	require.NoError(t, os.WriteFile(path, []byte("zip"), 0o600))
	return path
}

func syncService(t *testing.T, f *fakeC8y, source Source) ChangeResult {
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncMicroservices(context.Background(), []MicroserviceSpec{{Name: "my-service", Source: &source}}))
	return firstResult(s, "my-service")
}

func TestMicroserviceIsCreatedAndUploaded(t *testing.T) {
	f := fakeMicroserviceTenant(t)
	r := syncService(t, f, Source{Path: serviceZip(t, "my-service-1.1.0.zip")})

	assert.Equal(t, ActionCreated, r.Action, r.Err)
	assert.Equal(t, "created and uploaded my-service-1.1.0.zip", r.Detail)
	assert.Len(t, f.Requests(http.MethodPost, "/application/applications/50/binaries"), 1)
}

func TestMicroserviceIsNotCreatedWhenTheSourceFails(t *testing.T) {
	f := fakeMicroserviceTenant(t)
	r := syncService(t, f, Source{Path: filepath.Join(t.TempDir(), "missing.zip")})

	assert.Equal(t, ActionFailed, r.Action)
	assert.Empty(t, exactRequests(f, http.MethodPost, "/application/applications"), "no application without a binary is left behind")
}

func TestMicroserviceUploadDeniedExplainsHosting(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeMicroserviceTenant(t, microservice(f.server.URL, nil))
	f.JSON("POST /application/applications/51/binaries", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "Access Denied"})
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncMicroservices(context.Background(), []MicroserviceSpec{{Name: "my-service", Source: &Source{Path: serviceZip(t, "my-service.zip")}}}))

	r := firstResult(s, "my-service")
	assert.Equal(t, ActionFailed, r.Action)
	assert.ErrorContains(t, r.Err, "microservice hosting to be enabled for the tenant")
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants/t123/applications"), "a microservice without a binary is not subscribed")
}

func TestMicroserviceWithoutBinaryIsUploaded(t *testing.T) {
	f := newFakeC8y(t)
	// Created by an earlier run whose upload failed: no binary, no version
	f = fakeMicroserviceTenant(t, microservice(f.server.URL, map[string]any{"manifest": map[string]any{"roles": []any{}}}))
	r := syncService(t, f, Source{Path: serviceZip(t, "my-service.zip")})

	assert.Equal(t, ActionUpdated, r.Action, r.Err)
	assert.Equal(t, "uploaded my-service.zip (the application had no binary)", r.Detail)
	assert.Len(t, f.Requests(http.MethodPost, "/application/applications/51/binaries"), 1)
	assert.Empty(t, exactRequests(f, http.MethodPost, "/application/applications"))
}

func TestMicroserviceNewVersionIsUploaded(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeMicroserviceTenant(t, microservice(f.server.URL, map[string]any{
		"activeVersionId": "9", "manifest": map[string]any{"version": "1.0.0"},
	}))
	r := syncService(t, f, Source{Path: serviceZip(t, "my-service-1.1.0.zip")})

	assert.Equal(t, ActionUpdated, r.Action, r.Err)
	assert.Equal(t, "version 1.0.0 -> 1.1.0 (uploaded my-service-1.1.0.zip)", r.Detail)
	assert.Len(t, f.Requests(http.MethodPost, "/application/applications/51/binaries"), 1)
}

func TestMicroserviceAtSourceVersionIsUnchanged(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeMicroserviceTenant(t, microservice(f.server.URL, map[string]any{
		"activeVersionId": "9", "manifest": map[string]any{"version": "1.1.0"},
	}))
	r := syncService(t, f, Source{Path: serviceZip(t, "my-service-1.1.0.zip")})

	assert.Equal(t, ActionUnchanged, r.Action, r.Err)
	assert.Equal(t, "version 1.1.0 already active", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications/51/binaries"))
}

func TestMicroserviceWithoutSourceVersionIsOnlyUploadedWithForce(t *testing.T) {
	f := newFakeC8y(t)
	f = fakeMicroserviceTenant(t, microservice(f.server.URL, map[string]any{
		"activeVersionId": "9", "manifest": map[string]any{"version": "1.0.0"},
	}))
	zip := serviceZip(t, "my-service.zip")

	r := syncService(t, f, Source{Path: zip})
	assert.Equal(t, ActionUnchanged, r.Action, r.Err)
	assert.Equal(t, "binary upload skipped (no version in the source; use --force to re-upload)", r.Detail)
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications/51/binaries"))

	s := newFakeSyncer(t, f)
	s.Force = true
	require.NoError(t, s.SyncMicroservices(context.Background(), []MicroserviceSpec{{Name: "my-service", Source: &Source{Path: zip}}}))
	r = firstResult(s, "my-service")
	assert.Equal(t, ActionUpdated, r.Action, r.Err)
	assert.Equal(t, "re-uploaded my-service.zip", r.Detail)
}
