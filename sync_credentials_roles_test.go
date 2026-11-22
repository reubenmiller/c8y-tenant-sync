package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServiceUserRolesCoverEverySection makes sure every section declares
// the roles it needs in a target tenant, so a new section can't silently
// fail with 403 in serviceUser mode
func TestServiceUserRolesCoverEverySection(t *testing.T) {
	for _, section := range SectionOrder {
		_, ok := sectionServiceUserRoles[section]
		assert.True(t, ok, "section %s has no entry in sectionServiceUserRoles", section)
	}
	for section, roles := range sectionServiceUserRoles {
		for _, role := range roles {
			assert.True(t, slices.Contains(serviceUserRequiredRoles, role), "role %s of %s is not requested", role, section)
		}
	}
}

func TestServiceUserRequiredRoles(t *testing.T) {
	for _, role := range []string{
		"ROLE_INVENTORY_ADMIN",
		"ROLE_OPTION_MANAGEMENT_ADMIN",
		"ROLE_RETENTION_RULE_ADMIN",
		"ROLE_TENANT_MANAGEMENT_ADMIN",
		"ROLE_USER_MANAGEMENT_ADMIN",
		"ROLE_IDENTITY_ADMIN",
	} {
		assert.Contains(t, serviceUserRequiredRoles, role)
	}
	assert.True(t, slices.IsSorted(serviceUserRequiredRoles), "the list is stable")
	assert.Len(t, serviceUserRequiredRoles, len(slicesCompact(serviceUserRequiredRoles)), "no duplicates")
}

func slicesCompact(s []string) []string {
	return slices.Compact(slices.Clone(s))
}

// fakeServiceUsers serves the placeholder application, its subscriptions and
// the service users. Each subscription gets a new service user, which has
// the roles the application requires at the time of subscribing.
type fakeServiceUsers struct {
	mu            sync.Mutex
	requiredRoles []string
	// subscriptions holds the generation of the subscription per tenant
	subscriptions map[string]int
	// userRoles holds the roles of each service user by username
	userRoles map[string][]string
}

func (s *fakeServiceUsers) username(tenant string) string {
	return "service_tenant-sync_" + tenant + "_" + string(rune('0'+s.subscriptions[tenant]))
}

func newFakeServiceUsers(t *testing.T, requiredRoles []string, subscribed map[string][]string) (*fakeC8y, *fakeServiceUsers) {
	f := newFakeC8y(t)
	state := &fakeServiceUsers{requiredRoles: requiredRoles, subscriptions: map[string]int{}, userRoles: map[string][]string{}}
	for tenant, roles := range subscribed {
		state.subscriptions[tenant] = 1
		state.userRoles[state.username(tenant)] = roles
	}
	app := func() map[string]any {
		return map[string]any{
			"id": "100", "name": DefaultCredentialsApplication, "type": "MICROSERVICE",
			"self": f.server.URL + "/application/applications/100", "requiredRoles": state.requiredRoles,
		}
	}

	f.Handle("GET /application/applications", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"applications": []any{app()}, "statistics": stats()})
	})
	f.Handle("PUT /application/applications/100", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		state.mu.Lock()
		defer state.mu.Unlock()
		state.requiredRoles = nil
		for _, role := range body["requiredRoles"].([]any) {
			state.requiredRoles = append(state.requiredRoles, role.(string))
		}
		writeJSON(w, http.StatusOK, app())
	})
	f.Handle("GET /application/applications/100/bootstrapUser", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tenant": "t123", "name": "servicebootstrap_tenant-sync", "password": "bootstrap"})
	})
	subscribe := func(tenant string) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			state.mu.Lock()
			defer state.mu.Unlock()
			if _, ok := state.subscriptions[tenant]; ok {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "already subscribed"})
				return
			}
			state.subscriptions[tenant] = state.subscriptions[tenant+"#gen"] + 1
			state.subscriptions[tenant+"#gen"] = state.subscriptions[tenant]
			state.userRoles[state.username(tenant)] = slices.Clone(state.requiredRoles)
			writeJSON(w, http.StatusCreated, map[string]any{"application": app()})
		}
	}
	unsubscribe := func(tenant string) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			state.mu.Lock()
			defer state.mu.Unlock()
			state.subscriptions[tenant+"#gen"] = state.subscriptions[tenant]
			delete(state.subscriptions, tenant)
			w.WriteHeader(http.StatusNoContent)
		}
	}
	for _, tenant := range []string{"t200", "t300"} {
		f.Handle("POST /tenant/tenants/"+tenant+"/applications", subscribe(tenant))
		f.Handle("DELETE /tenant/tenants/"+tenant+"/applications/100", unsubscribe(tenant))
	}
	f.Handle("GET /application/currentApplication/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		users := []map[string]any{}
		for tenant := range state.subscriptions {
			if len(tenant) > 4 && tenant[4] == '#' {
				continue
			}
			users = append(users, map[string]any{"tenant": tenant, "name": state.username(tenant), "password": "pw-" + tenant})
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
	})
	f.Handle("GET /user/currentUser", func(w http.ResponseWriter, r *http.Request) {
		username, _, _ := r.BasicAuth()
		state.mu.Lock()
		defer state.mu.Unlock()
		var effective []map[string]any
		for _, role := range state.userRoles[trimTenantPrefix(username)] {
			effective = append(effective, map[string]any{"id": role})
		}
		writeJSON(w, http.StatusOK, map[string]any{"userName": username, "effectiveRoles": effective})
	})
	return f, state
}

func trimTenantPrefix(username string) string {
	for i := range username {
		if username[i] == '/' {
			return username[i+1:]
		}
	}
	return username
}

func serviceUserTargets() []Target {
	return []Target{{TenantID: "t200"}, {TenantID: "t300"}}
}

func TestServiceUserCredentialsWithCurrentRoles(t *testing.T) {
	noServiceUserRetryDelay(t)
	f, _ := newFakeServiceUsers(t, serviceUserRequiredRoles, map[string][]string{
		"t200": serviceUserRequiredRoles,
	})
	s := newFakeSyncer(t, f)
	targets := serviceUserTargets()

	var err error
	captureStdio(t, func() {
		err = s.resolveServiceUserCredentials(context.Background(), targets, &TargetsSpec{})
	})
	require.NoError(t, err)
	require.NotNil(t, targets[0].Auth)
	assert.Equal(t, "service_tenant-sync_t200_1", targets[0].Auth.Username, "the existing subscription is kept")
	assert.Equal(t, "service_tenant-sync_t300_1", targets[1].Auth.Username, "a missing subscription is created")
	assert.Empty(t, f.Requests(http.MethodPut, "/application/applications/100"), "the roles are up to date")
	assert.Empty(t, f.Requests(http.MethodDelete, "/tenant/tenants/t200/applications/100"))
}

func TestServiceUserCredentialsResubscribesOutdatedTenants(t *testing.T) {
	noServiceUserRetryDelay(t)
	oldRoles := []string{"ROLE_INVENTORY_READ", "ROLE_INVENTORY_ADMIN", "ROLE_OPTION_MANAGEMENT_ADMIN"}
	f, state := newFakeServiceUsers(t, oldRoles, map[string][]string{
		"t200": oldRoles,                 // subscribed by an older version
		"t300": serviceUserRequiredRoles, // already up to date
	})
	s := newFakeSyncer(t, f)
	targets := serviceUserTargets()

	var err error
	stdout, _ := captureStdio(t, func() {
		err = s.resolveServiceUserCredentials(context.Background(), targets, &TargetsSpec{})
	})
	require.NoError(t, err)

	update := f.Requests(http.MethodPut, "/application/applications/100")
	require.Len(t, update, 1, "the required roles of the application are updated")
	assert.ElementsMatch(t, serviceUserRequiredRoles, state.requiredRoles)

	assert.Len(t, f.Requests(http.MethodDelete, "/tenant/tenants/t200/applications/100"), 1, "the outdated tenant is subscribed again")
	assert.Empty(t, f.Requests(http.MethodDelete, "/tenant/tenants/t300/applications/100"), "an up-to-date tenant is left alone")
	require.NotNil(t, targets[0].Auth)
	assert.Equal(t, "service_tenant-sync_t200_2", targets[0].Auth.Username, "the new service user is used")
	assert.ElementsMatch(t, serviceUserRequiredRoles, state.userRoles["service_tenant-sync_t200_2"])
	assert.Equal(t, "service_tenant-sync_t300_1", targets[1].Auth.Username)
	assert.Contains(t, stdout, `Re-subscribing credentials application "tenant-sync" to tenant t200`)
}

func TestServiceUserCredentialsUnreadableRolesAreNotResubscribed(t *testing.T) {
	noServiceUserRetryDelay(t)
	f, _ := newFakeServiceUsers(t, serviceUserRequiredRoles, map[string][]string{"t200": nil, "t300": nil})
	f.JSON("GET /user/currentUser", http.StatusForbidden, map[string]any{"error": "forbidden"})
	s := newFakeSyncer(t, f)
	targets := serviceUserTargets()

	var err error
	captureStdio(t, func() {
		err = s.resolveServiceUserCredentials(context.Background(), targets, &TargetsSpec{})
	})
	require.NoError(t, err)
	assert.Empty(t, f.Requests(http.MethodDelete, "/tenant/tenants/t200/applications/100"))
	assert.NotNil(t, targets[0].Auth)
}

func noServiceUserRetryDelay(t *testing.T) {
	previous := serviceUserRetryDelay
	serviceUserRetryDelay = 0
	t.Cleanup(func() { serviceUserRetryDelay = previous })
}
