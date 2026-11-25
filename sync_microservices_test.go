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

// fakeMicroserviceTenant serves tenant t123, which may host microservices,
// with the given microservices (none subscribed) and accepts creating,
// uploading and subscribing
func fakeMicroserviceTenant(t *testing.T, apps ...map[string]any) *fakeC8y {
	return fakeHostingTenant(t, true, apps...)
}

// fakeHostingTenant is fakeMicroserviceTenant, with or without the
// subscription to feature-microservice-hosting
func fakeHostingTenant(t *testing.T, hosting bool, apps ...map[string]any) *fakeC8y {
	f := newFakeC8y(t)
	url := f.server.URL
	refs := []any{}
	if hosting {
		refs = append(refs, map[string]any{"application": map[string]any{
			"id": "4", "name": FeatureMicroserviceHosting, "type": "HOSTED",
			"owner": map[string]any{"tenant": map[string]any{"id": "management"}},
		}})
	}
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{
		"name": "t123", "applications": map[string]any{"references": refs},
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
	assert.ErrorContains(t, r.Err, "requires the tenant to be subscribed to feature-microservice-hosting")
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

func TestMicroserviceWithoutHostingFailsBeforeDownloading(t *testing.T) {
	f := fakeHostingTenant(t, false)
	zip := serviceZip(t, "my-service-1.1.0.zip")
	r := syncService(t, f, Source{Path: zip})

	assert.Equal(t, ActionFailed, r.Action)
	assert.EqualError(t, r.Err, "the tenant t123 is not subscribed to feature-microservice-hosting, so it can't host microservices: ask its parent tenant (or your Cumulocity provider) to subscribe it, or use --force to try the upload anyway")
	assert.Empty(t, exactRequests(f, http.MethodPost, "/application/applications"), "nothing is created")
	assert.Empty(t, f.Requests(http.MethodPost, "/application/applications/50/binaries"))
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants/t123/applications"), "nothing is subscribed")

	// --force tries the upload anyway
	s := newFakeSyncer(t, f)
	s.Force = true
	require.NoError(t, s.SyncMicroservices(context.Background(), []MicroserviceSpec{{Name: "my-service", Source: &Source{Path: zip}}}))
	assert.Equal(t, ActionCreated, firstResult(s, "my-service").Action, firstResult(s, "my-service").Err)
}

func TestMicroserviceHostingCheckNeedsAnUpload(t *testing.T) {
	// Already at the source version: no upload, so no hosting check
	f := newFakeC8y(t)
	f = fakeHostingTenant(t, false, microservice(f.server.URL, map[string]any{
		"activeVersionId": "9", "manifest": map[string]any{"version": "1.1.0"},
	}))
	r := syncService(t, f, Source{Path: serviceZip(t, "my-service-1.1.0.zip")})
	assert.Equal(t, ActionUnchanged, r.Action, r.Err)
}

func TestMicroserviceHostingUnknownTriesTheUpload(t *testing.T) {
	f := fakeHostingTenant(t, false)
	s := newFakeSyncer(t, f)
	// The subscriptions could not be read: the upload decides
	s.baseTenantID = "t123"
	s.activeTarget = &Target{TenantID: "t123", Current: true}
	f.JSON("GET /tenant/currentTenant", http.StatusForbidden, forbidden)
	require.NoError(t, s.SyncMicroservices(context.Background(), []MicroserviceSpec{{Name: "my-service", Source: &Source{Path: serviceZip(t, "my-service.zip")}}}))
	assert.Equal(t, ActionCreated, firstResult(s, "my-service").Action, firstResult(s, "my-service").Err)
}
