package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeApp is an application known to the fake server
type fakeApp struct {
	ID    string
	Name  string
	Type  string
	Owner string
}

func (a fakeApp) json(baseURL string) map[string]any {
	return map[string]any{
		"id":    a.ID,
		"name":  a.Name,
		"type":  a.Type,
		"self":  baseURL + "/application/applications/" + a.ID,
		"owner": map[string]any{"tenant": map[string]any{"id": a.Owner}},
	}
}

// fakeApplicationTenant serves the current tenant t123 with the given
// subscribed applications, plus application lookups by name. The
// (un)subscribe responses are set per test.
func fakeApplicationTenant(t *testing.T, apps []fakeApp, subscribed ...string) *fakeC8y {
	f := newFakeC8y(t)
	url := f.server.URL

	refs := []map[string]any{}
	for _, name := range subscribed {
		for _, app := range apps {
			if app.Name == name {
				refs = append(refs, map[string]any{"application": app.json(url)})
			}
		}
	}
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{
		"name":         "t123",
		"applications": map[string]any{"references": refs},
	})
	f.JSON("GET /tenant/tenants/t456/applications", http.StatusOK, map[string]any{
		"references": refs,
		"statistics": stats(),
	})

	for _, app := range apps {
		app := app
		body := app.json(url)
		f.JSON("GET /application/applicationsByName/"+app.Name, http.StatusOK, map[string]any{
			"applications": []map[string]any{body},
			"statistics":   stats(),
		})
		f.JSON("GET /application/applications/"+app.ID, http.StatusOK, body)
	}
	f.Handle("GET /application/applications", func(w http.ResponseWriter, r *http.Request) {
		// The SDK lists all applications (optionally by type) and matches the
		// name on the client
		found := []map[string]any{}
		for _, app := range apps {
			if appType := r.URL.Query().Get("type"); appType == "" || appType == app.Type {
				found = append(found, app.json(url))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"applications": found, "statistics": stats()})
	})
	return f
}

var forbidden = map[string]any{"error": "security/Forbidden", "message": "Access is denied"}

func builtinApps() []fakeApp {
	return []fakeApp{
		{ID: "1", Name: "smartrule", Type: "MICROSERVICE", Owner: "management"},
		{ID: "2", Name: "own-service", Type: "MICROSERVICE", Owner: "t123"},
		{ID: "3", Name: "web-app", Type: "HOSTED", Owner: "management"},
		{ID: "4", Name: "no-owner", Type: "MICROSERVICE", Owner: ""},
	}
}

func resultsByItem(s *Syncer) map[string]ChangeResult {
	out := map[string]ChangeResult{}
	for _, r := range s.Results {
		out[r.Item] = r
	}
	return out
}

func TestApplicationsAlreadySubscribedAreNotSubscribedAgain(t *testing.T) {
	f := fakeApplicationTenant(t, builtinApps(), "smartrule", "web-app")
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{
		{Name: "smartrule"},
		{Name: "web-app", Type: "HOSTED"},
	}))

	results := resultsByItem(s)
	assert.Equal(t, ActionUnchanged, results["smartrule"].Action)
	assert.Equal(t, "already subscribed", results["smartrule"].Detail)
	assert.Equal(t, ActionUnchanged, results["web-app"].Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants"), "subscribed applications must not be subscribed again")
	assert.Len(t, f.Requests(http.MethodGet, "/tenant/currentTenant"), 1, "the subscriptions are listed once")
}

func TestApplicationTypeMustMatchSubscription(t *testing.T) {
	f := fakeApplicationTenant(t, builtinApps(), "web-app")
	f.JSON("POST /tenant/tenants/t123/applications", http.StatusCreated, map[string]any{})
	s := newFakeSyncer(t, f)

	// web-app is subscribed as HOSTED: a MICROSERVICE web-app is not, so it is
	// looked up (and fails, since there is no such microservice)
	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{{Name: "web-app", Type: "MICROSERVICE"}}))
	r := resultsByItem(s)["web-app"]
	assert.Equal(t, ActionFailed, r.Action)
	assert.Equal(t, "lookup", r.Detail)

	// With the matching type it is already subscribed
	s = newFakeSyncer(t, f)
	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{{Name: "web-app", Type: "hosted"}}))
	assert.Equal(t, ActionUnchanged, resultsByItem(s)["web-app"].Action)
	assert.Empty(t, f.Requests(http.MethodPost, "/tenant/tenants/t123/applications"))
}

func TestApplicationSubscribe(t *testing.T) {
	f := fakeApplicationTenant(t, builtinApps())
	f.JSON("POST /tenant/tenants/t123/applications", http.StatusCreated, map[string]any{})
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{{Name: "smartrule"}}))
	r := resultsByItem(s)["smartrule"]
	assert.Equal(t, ActionUpdated, r.Action)
	assert.Equal(t, "subscribed", r.Detail)
}

func TestApplicationSubscribeDeniedForForeignOwnerIsSkipped(t *testing.T) {
	f := fakeApplicationTenant(t, builtinApps())
	f.JSON("POST /tenant/tenants/t123/applications", http.StatusForbidden, forbidden)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{
		{Name: "smartrule"},   // owned by management
		{Name: "own-service"}, // owned by the tenant itself
		{Name: "no-owner"},    // owner unknown
	}))

	results := resultsByItem(s)
	assert.Equal(t, ActionSkipped, results["smartrule"].Action)
	assert.NoError(t, results["smartrule"].Err)
	assert.Equal(t, "subscribe denied: the application is owned by tenant management; subscribe it from tenant management (e.g. with a targets section selecting t123)", results["smartrule"].Detail)

	assert.Equal(t, ActionFailed, results["own-service"].Action, "a 403 for the tenant's own application is a real permission problem")
	assert.ErrorContains(t, results["own-service"].Err, "access denied")
	assert.Equal(t, ActionFailed, results["no-owner"].Action, "an unknown owner is not skipped")

	_, failures := s.Summary()
	assert.Len(t, failures, 2)
}

func TestApplicationUnsubscribe(t *testing.T) {
	f := fakeApplicationTenant(t, builtinApps(), "smartrule", "own-service")
	f.JSON("DELETE /tenant/tenants/t123/applications/1", http.StatusForbidden, forbidden)
	f.JSON("DELETE /tenant/tenants/t123/applications/2", http.StatusNoContent, nil)
	s := newFakeSyncer(t, f)

	notSubscribed := false
	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{
		{Name: "smartrule", Subscribed: &notSubscribed},
		{Name: "own-service", Subscribed: &notSubscribed},
		{Name: "web-app", Subscribed: &notSubscribed},
	}))

	results := resultsByItem(s)
	assert.Equal(t, ActionSkipped, results["smartrule"].Action)
	assert.Contains(t, results["smartrule"].Detail, "unsubscribe denied: the application is owned by tenant management")
	assert.Equal(t, ActionUpdated, results["own-service"].Action)
	assert.Equal(t, ActionUnchanged, results["web-app"].Action)
	assert.Equal(t, "not subscribed", results["web-app"].Detail)
	assert.Empty(t, f.Requests(http.MethodDelete, "/tenant/tenants/t123/applications/3"), "unsubscribed applications are not unsubscribed again")
}

func TestApplicationSubscriptionsUnknownFallsBack(t *testing.T) {
	f := fakeApplicationTenant(t, builtinApps())
	// The current tenant can't be read: the subscription state is unknown,
	// so the subscribe call decides (409 = already subscribed)
	f.JSON("GET /tenant/currentTenant", http.StatusForbidden, forbidden)
	f.JSON("POST /tenant/tenants/t123/applications", http.StatusConflict, map[string]any{"error": "conflict"})
	s := newFakeSyncer(t, f)
	s.activeTarget = &Target{TenantID: "t123", Current: true}

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{{Name: "smartrule"}}))
	assert.Equal(t, ActionUnchanged, resultsByItem(s)["smartrule"].Action)
	assert.Len(t, f.Requests(http.MethodPost, "/tenant/tenants/t123/applications"), 1)
}

func TestApplicationsOtherTargetTenant(t *testing.T) {
	// The parent (t123) applies to child t456: subscriptions are read from
	// the parent's tenant API
	f := fakeApplicationTenant(t, builtinApps(), "web-app")
	f.JSON("POST /tenant/tenants/t456/applications", http.StatusForbidden, forbidden)
	s := newFakeSyncer(t, f)
	s.activeTarget = &Target{TenantID: "t456"}

	require.NoError(t, s.SyncApplications(context.Background(), []ApplicationSpec{
		{Name: "web-app"},     // already subscribed
		{Name: "smartrule"},   // owned by management (neither the target nor the parent)
		{Name: "own-service"}, // owned by the parent t123: a real permission problem
	}))

	results := resultsByItem(s)
	assert.Equal(t, ActionUnchanged, results["web-app"].Action)
	assert.Equal(t, ActionSkipped, results["smartrule"].Action)
	assert.Contains(t, results["smartrule"].Detail, "targets section selecting t456")
	assert.Equal(t, ActionFailed, results["own-service"].Action)
	assert.Len(t, f.Requests(http.MethodGet, "/tenant/tenants/t456/applications"), 1)
}

func TestSubscriptionsFind(t *testing.T) {
	subs := subscriptions{{Name: "a", Type: "MICROSERVICE"}, {Name: "b", Type: "HOSTED"}}
	_, ok := subs.find("a", "")
	assert.True(t, ok)
	_, ok = subs.find("a", "microservice")
	assert.True(t, ok, "types match case-insensitively")
	_, ok = subs.find("b", "MICROSERVICE")
	assert.False(t, ok)
	_, ok = subs.find("c", "")
	assert.False(t, ok)
}
