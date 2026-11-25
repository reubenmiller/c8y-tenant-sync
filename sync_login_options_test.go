package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSSOSecret = "s3cr3t+value/with=chars"

// fakeOIDC is an OpenID Connect provider serving a discovery document and
// JWKS under /realms/acme
type fakeOIDC struct {
	server *httptest.Server
	// discovery overrides fields of the discovery document
	discovery map[string]any
	// keys is the number of JWKS keys
	keys int
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	p := &fakeOIDC{keys: 1, discovery: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/acme/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := p.server.URL + "/realms/acme"
		doc := map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/protocol/openid-connect/auth",
			"token_endpoint":                        base + "/protocol/openid-connect/token",
			"end_session_endpoint":                  base + "/protocol/openid-connect/logout",
			"jwks_uri":                              base + "/protocol/openid-connect/certs",
			"response_types_supported":              []string{"code", "id_token"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		}
		for key, value := range p.discovery {
			if value == nil {
				delete(doc, key)
			} else {
				doc[key] = value
			}
		}
		writeJSON(w, http.StatusOK, doc)
	})
	mux.HandleFunc("/realms/acme/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		keys := []map[string]any{}
		for i := 0; i < p.keys; i++ {
			keys = append(keys, map[string]any{"kty": "RSA", "kid": "k1"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *fakeOIDC) discoveryURL() string {
	return p.server.URL + "/realms/acme/.well-known/openid-configuration"
}

// fakeLoginOptions keeps the login options of the fake tenant: GET lists
// them, POST creates one and PUT replaces one
type fakeLoginOptions struct {
	f       *fakeC8y
	mu      sync.Mutex
	options []map[string]any
	// keepVisibility lists types whose visibleOnLoginPage the fake ignores
	// on PUT (to simulate an update which does not take effect)
	keepVisibility map[string]bool
}

func newFakeLoginOptions(f *fakeC8y, options ...map[string]any) *fakeLoginOptions {
	l := &fakeLoginOptions{f: f, options: options, keepVisibility: map[string]bool{}}
	f.Handle("GET /tenant/loginOptions", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"loginOptions": l.options, "statistics": stats()})
	})
	f.Handle("POST /tenant/loginOptions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		l.mu.Lock()
		body["id"] = strings.ToLower(body["type"].(string)) + "-id"
		l.options = append(l.options, body)
		l.mu.Unlock()
		l.handlePut(body["id"].(string))
		writeJSON(w, http.StatusCreated, body)
	})
	for _, option := range options {
		l.handlePut(option["id"].(string))
	}
	return l
}

func (l *fakeLoginOptions) handlePut(id string) {
	l.f.Handle("PUT /tenant/loginOptions/"+id, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, option := range l.options {
			if option["id"] == id {
				if l.keepVisibility[option["type"].(string)] {
					body["visibleOnLoginPage"] = option["visibleOnLoginPage"]
				}
				body["id"] = id
				l.options[i] = body
				writeJSON(w, http.StatusOK, body)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "notFound"})
	})
}

func (l *fakeLoginOptions) get(loginType string) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, option := range l.options {
		if option["type"] == loginType {
			return option
		}
	}
	return nil
}

// defaultLoginOptions are the login options of a new tenant: OAI-Secure
// preferred
func defaultLoginOptions() []map[string]any {
	return []map[string]any{
		{"id": "basic-id", "type": "BASIC", "visibleOnLoginPage": false, "userManagementSource": "INTERNAL", "self": "https://t123/tenant/loginOptions/basic-id"},
		{"id": "oai-id", "type": "OAUTH2_INTERNAL", "visibleOnLoginPage": true, "grantType": "PASSWORD", "userManagementSource": "INTERNAL",
			"sessionConfiguration": map[string]any{"absoluteTimeoutMillis": 1209600000, "renewalTimeoutMillis": 86400000, "maximumNumberOfParallelSessions": 5, "userAgentValidationRequired": false}},
	}
}

// fakeSSOGroupsAndApps answers the user group and application lookups of the
// SSO access mappings
func fakeSSOGroupsAndApps(f *fakeC8y) {
	f.JSON("GET /user/t123/groupByName/admins", http.StatusOK, map[string]any{"id": 3, "name": "admins"})
	f.JSON("GET /application/applicationsByName/devicemanagement", http.StatusOK, map[string]any{
		"applications": []map[string]any{{"id": "7", "name": "devicemanagement", "type": "HOSTED"}},
		"statistics":   stats(),
	})
	f.JSON("GET /application/applications", http.StatusOK, map[string]any{
		"applications": []map[string]any{{"id": "7", "name": "devicemanagement", "type": "HOSTED"}},
		"statistics":   stats(),
	})
}

func ssoSpec(p *fakeOIDC) *SSOLoginSpec {
	return &SSOLoginSpec{
		DiscoveryURL: p.discoveryURL(),
		ClientID:     "cumulocity",
		ClientSecret: testSSOSecret,
		ProviderName: "Acme",
		UserIDClaim:  "preferred_username",
		Access: &SSOAccessMappingSpec{Rules: []SSOAccessRuleSpec{{
			When:         []SSOConditionSpec{{Claim: "groups", Operator: "IN", Value: "c8y-admins"}},
			Groups:       []string{"admins"},
			Applications: []string{"devicemanagement"},
		}}},
	}
}

func loginResults(s *Syncer) map[string]ChangeResult {
	out := map[string]ChangeResult{}
	for _, r := range s.Results {
		if r.Section == SectionLoginOptions {
			out[r.Item] = r
		}
	}
	return out
}

// writes returns the POST and PUT requests to the login options, in order
func writes(f *fakeC8y) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.HasPrefix(r.Path, "/tenant/loginOptions") {
			visible := ""
			if v, ok := r.Body["visibleOnLoginPage"].(bool); ok {
				if v {
					visible = " visible"
				} else {
					visible = " hidden"
				}
			}
			out = append(out, r.Method+" "+strings.TrimPrefix(r.Path, "/tenant/loginOptions")+visible)
		}
	}
	return out
}

func TestLoginOptionsValidate(t *testing.T) {
	p := newFakeOIDC(t)
	tests := []struct {
		name string
		spec *LoginOptionsSpec
		want []string
	}{
		{"valid", &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}, nil},
		{"unknown mode", &LoginOptionsSpec{Preferred: "saml"}, []string{`unknown login mode "saml"`}},
		{"sso preferred without sso", &LoginOptionsSpec{Preferred: LoginModeSSO}, []string{"preferred sso requires the sso login option"}},
		{"sso preferred without rules", &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: func() *SSOLoginSpec {
			s := ssoSpec(p)
			s.Access = nil
			return s
		}()}, []string{"requires at least one accessMappings rule"}},
		{"sso preferred but hidden", &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: func() *SSOLoginSpec {
			s := ssoSpec(p)
			s.VisibleOnLoginPage = boolPtr(false)
			return s
		}()}, []string{"visibleOnLoginPage cannot be false"}},
		{"basic preferred but forbidden for browsers", &LoginOptionsSpec{Preferred: LoginModeBasic,
			Basic: &BasicLoginOptionSpec{ForbiddenClients: []string{"WEB_BROWSERS"}}}, []string{"preferred basic cannot be combined"}},
		{"unknown client", &LoginOptionsSpec{Basic: &BasicLoginOptionSpec{ForbiddenClients: []string{"CURL"}}}, []string{`unknown client "CURL"`}},
		{"session timeouts", &LoginOptionsSpec{OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{
			AbsoluteTimeout: "5m", RenewalTimeout: "1h", MaximumParallelSessions: intPtr(0)}}},
			[]string{"absoluteTimeout: must be at least 15m", "maximumParallelSessions: must be at least 1"}},
		{"renewal longer than absolute", &LoginOptionsSpec{OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{
			AbsoluteTimeout: "1h", RenewalTimeout: "2h"}}}, []string{"renewalTimeout must be shorter than absoluteTimeout"}},
		{"invalid duration", &LoginOptionsSpec{OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{AbsoluteTimeout: "two weeks"}}},
			[]string{"absoluteTimeout: time: invalid duration"}},
		{"sso fields", &LoginOptionsSpec{SSO: &SSOLoginSpec{DiscoveryURL: "http://idp.example.com/.well-known/openid-configuration"}},
			[]string{"must be an https URL", "clientId is required", "one of clientSecret or clientSecretFrom is required"}},
		{"both secrets", &LoginOptionsSpec{SSO: func() *SSOLoginSpec {
			s := ssoSpec(p)
			s.ClientSecretFrom = &SecretRef{Env: "SSO_SECRET"}
			return s
		}()}, []string{"clientSecret and clientSecretFrom are mutually exclusive"}},
		{"rule without conditions", &LoginOptionsSpec{SSO: func() *SSOLoginSpec {
			s := ssoSpec(p)
			s.Access.Rules = append(s.Access.Rules, SSOAccessRuleSpec{Match: "some", When: []SSOConditionSpec{{Operator: "LIKE"}}})
			return s
		}()}, []string{`match: unknown value "some"`, "when[0].claim is required", `unknown operator "LIKE"`, "requires groups or applications"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := strings.Join(tt.spec.validate(), "\n")
			if tt.want == nil {
				assert.Empty(t, errs)
			}
			for _, want := range tt.want {
				assert.Contains(t, errs, want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

func TestLoginOptionsValidateViaManifest(t *testing.T) {
	m := &Manifest{LoginOptions: &LoginOptionsSpec{Preferred: LoginModeSSO}}
	assert.ErrorContains(t, m.Validate(), "preferred sso requires the sso login option")
}

func TestSyncLoginOptionsDryRun(t *testing.T) {
	p := newFakeOIDC(t)
	s := &Syncer{DryRun: true}
	spec := &LoginOptionsSpec{
		Preferred: LoginModeSSO,
		Basic:     &BasicLoginOptionSpec{ForbiddenClients: []string{"WEB_BROWSERS"}},
		OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{AbsoluteTimeout: "8h"}},
		SSO:       ssoSpec(p),
	}
	require.NoError(t, s.SyncLoginOptions(context.Background(), spec))

	results := loginResults(s)
	require.Len(t, results, 4)
	for _, r := range results {
		assert.Equal(t, ActionPlanned, r.Action, r.Item)
		assert.NotContains(t, r.Detail, "s3cr3t", "the client secret is never shown")
	}
	assert.Equal(t, `authenticationRestrictions.forbiddenClients=["WEB_BROWSERS"]`, results[LoginTypeBasic].Detail)
	assert.Equal(t, "sessionConfiguration.absoluteTimeoutMillis=28800000", results[LoginTypeOAISecure].Detail)
	assert.Contains(t, results[LoginTypeSSO].Detail, "token "+p.server.URL+"/realms/acme/protocol/openid-connect/token")
	assert.Contains(t, results[LoginTypeSSO].Detail, "client secret in the request body")
	assert.Contains(t, results[loginItemPreferred].Detail, "set to sso")
}

func TestSyncLoginOptionsDryRunReportsBrokenProvider(t *testing.T) {
	p := newFakeOIDC(t)
	p.keys = 0
	s := &Syncer{DryRun: true}
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))

	results := loginResults(s)
	assert.Equal(t, ActionFailed, results[LoginTypeSSO].Action)
	assert.ErrorContains(t, results[LoginTypeSSO].Err, "has no keys")
	assert.Equal(t, ActionFailed, results[loginItemPreferred].Action)
	assert.ErrorContains(t, results[loginItemPreferred].Err, "would stay unchanged")
}

func TestSyncLoginOptionsSwitchesToSSO(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	options := newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))

	results := loginResults(s)
	assert.Equal(t, ActionCreated, results[LoginTypeSSO].Action, results[LoginTypeSSO].Err)
	assert.Equal(t, ActionUpdated, results[loginItemPreferred].Action, results[loginItemPreferred].Err)
	assert.Equal(t, "login mode oaiSecure → sso", results[loginItemPreferred].Detail)
	for _, r := range results {
		assert.NotContains(t, r.Detail, "s3cr3t")
	}

	// The SSO option is created (visible) before OAI-Secure is hidden (basic
	// is hidden already)
	assert.Equal(t, []string{
		"POST  visible",
		"PUT /oai-id hidden",
	}, writes(f))

	sso := options.get(LoginTypeSSO)
	require.NotNil(t, sso)
	issuer := p.server.URL + "/realms/acme"
	assert.Equal(t, issuer, sso["issuer"])
	assert.Equal(t, "AUTHORIZATION_CODE", sso["grantType"])
	assert.Equal(t, "Log in with Acme", sso["buttonName"])
	assert.Equal(t, map[string]any{"jwks": map[string]any{"jwksUrl": issuer + "/protocol/openid-connect/certs"}}, sso["signatureVerificationConfig"])
	assert.Equal(t, "preferred_username", sso["userIdConfig"].(map[string]any)["jwtField"])

	auth := sso["authorizationRequest"].(map[string]any)
	assert.Equal(t, issuer+"/protocol/openid-connect/auth", auth["url"])
	assert.Equal(t, map[string]any{"response_type": "code", "client_id": "cumulocity", "scope": "openid profile email", "redirect_uri": "${redirectUri}"}, auth["requestParams"])

	token := sso["tokenRequest"].(map[string]any)
	assert.Equal(t, issuer+"/protocol/openid-connect/token", token["url"])
	assert.Equal(t, "grant_type=authorization_code&code=${code}&redirect_uri=${redirectUri}&client_id=cumulocity&client_secret=s3cr3t%2Bvalue%2Fwith%3Dchars", token["body"])
	refresh := sso["refreshRequest"].(map[string]any)
	assert.Equal(t, "grant_type=refresh_token&refresh_token=${refreshToken}&client_id=cumulocity&client_secret=s3cr3t%2Bvalue%2Fwith%3Dchars", refresh["body"])
	logout := sso["logoutRequest"].(map[string]any)
	assert.Equal(t, issuer+"/protocol/openid-connect/logout", logout["url"])

	// Group and application names are resolved to IDs
	mapping := sso["onNewUser"].(map[string]any)["dynamicMapping"].(map[string]any)
	assert.Equal(t, []any{map[string]any{
		"when": map[string]any{"operator": "AND", "childPredicates": []any{
			map[string]any{"parameterPath": "groups", "operator": "IN", "value": "c8y-admins"},
		}},
		"thenGroups":       []any{float64(3)},
		"thenApplications": []any{float64(7)},
	}}, mapping["mappings"])

	// A second run changes nothing
	s2 := newFakeSyncer(t, f)
	require.NoError(t, s2.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))
	for item, r := range loginResults(s2) {
		assert.Equal(t, ActionUnchanged, r.Action, "%s: %s %v", item, r.Detail, r.Err)
	}
	assert.Len(t, writes(f), 2, "no further writes")
}

func TestSyncLoginOptionsClientSecretBasic(t *testing.T) {
	p := newFakeOIDC(t)
	p.discovery["token_endpoint_auth_methods_supported"] = []string{"client_secret_basic"}
	option, err := buildSSOLoginOption(context.Background(), ssoSpec(p), "")
	require.NoError(t, err)

	token := option.body["tokenRequest"].(map[string]any)
	assert.Equal(t, "grant_type=authorization_code&code=${code}&redirect_uri=${redirectUri}&client_id=cumulocity", token["body"])
	assert.Equal(t, clientSecretBasicHeader("cumulocity", testSSOSecret), token["headers"].(map[string]any)["Authorization"])
	assert.Contains(t, option.describe(), "client secret in the Authorization header")
}

func TestSyncLoginOptionsBrokenProviderChangesNothing(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(p *fakeOIDC, spec *SSOLoginSpec)
		want    string
	}{
		{"unreachable", func(p *fakeOIDC, spec *SSOLoginSpec) {
			spec.DiscoveryURL = p.server.URL + "/realms/other/.well-known/openid-configuration"
		}, "404"},
		{"issuer mismatch", func(p *fakeOIDC, spec *SSOLoginSpec) {
			p.discovery["issuer"] = "https://evil.example.com"
		}, "does not match"},
		{"missing endpoint", func(p *fakeOIDC, spec *SSOLoginSpec) {
			p.discovery["token_endpoint"] = nil
		}, "missing: token_endpoint"},
		{"no code flow", func(p *fakeOIDC, spec *SSOLoginSpec) {
			p.discovery["response_types_supported"] = []string{"id_token"}
		}, "authorization code flow"},
		{"empty secret", func(p *fakeOIDC, spec *SSOLoginSpec) {
			spec.ClientSecret = " "
		}, "client secret is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newFakeOIDC(t)
			f := newFakeC8y(t)
			fakeSSOGroupsAndApps(f)
			newFakeLoginOptions(f, defaultLoginOptions()...)
			s := newFakeSyncer(t, f)
			spec := ssoSpec(p)
			tt.prepare(p, spec)

			require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: spec}))
			results := loginResults(s)
			assert.ErrorContains(t, results[LoginTypeSSO].Err, tt.want)
			assert.ErrorContains(t, results[loginItemPreferred].Err, "kept login mode oaiSecure")
			assert.Empty(t, writes(f), "nothing is written")
		})
	}
}

func TestSyncLoginOptionsUnknownGroupKeepsMode(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))
	results := loginResults(s)
	assert.ErrorContains(t, results[LoginTypeSSO].Err, `user group "admins"`)
	assert.ErrorContains(t, results[loginItemPreferred].Err, "kept login mode oaiSecure")
	assert.Empty(t, writes(f))
}

func TestSyncLoginOptionsRestoresOnFailedVerification(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	options := newFakeLoginOptions(f, defaultLoginOptions()...)
	// The basic option ignores visibility changes
	options.keepVisibility[LoginTypeBasic] = true
	options.options[0]["visibleOnLoginPage"] = true
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))
	result := loginResults(s)[loginItemPreferred]
	assert.Equal(t, ActionFailed, result.Action)
	assert.ErrorContains(t, result.Err, "verify the login options")
	assert.ErrorContains(t, result.Err, "restored login mode oaiSecure")

	// OAI-Secure is visible again
	assert.Equal(t, true, options.get(LoginTypeOAISecure)["visibleOnLoginPage"])
	assert.Equal(t, []string{
		"POST  visible",
		"PUT /oai-id hidden",
		"PUT /basic-id hidden",
		// restore in reverse order
		"PUT /basic-id visible",
		"PUT /oai-id visible",
	}, writes(f))
}

func TestSyncLoginOptionsRefusesToBlockItself(t *testing.T) {
	f := newFakeC8y(t)
	newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)
	s.Client.UserAgent = "c8y-tenant-sync/1.0 go-client-v2"

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
		Basic: &BasicLoginOptionSpec{ForbiddenUserAgents: []string{"curl", "Tenant-Sync"}},
	}))
	result := loginResults(s)[LoginTypeBasic]
	assert.ErrorContains(t, result.Err, "refusing to forbid the user agent(s) Tenant-Sync")
	assert.Empty(t, writes(f))
}

func TestSyncLoginOptionsBasicRestrictionsOrder(t *testing.T) {
	t.Run("forbidding browsers waits for the switch away from basic", func(t *testing.T) {
		f := newFakeC8y(t)
		initial := defaultLoginOptions()
		initial[0]["visibleOnLoginPage"] = true
		initial[1]["visibleOnLoginPage"] = false
		options := newFakeLoginOptions(f, initial...)
		s := newFakeSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
			Preferred: LoginModeOAISecure,
			Basic:     &BasicLoginOptionSpec{ForbiddenClients: []string{"WEB_BROWSERS"}},
		}))
		results := loginResults(s)
		assert.Equal(t, ActionUpdated, results[loginItemPreferred].Action, results[loginItemPreferred].Err)
		assert.Equal(t, ActionUpdated, results[LoginTypeBasic].Action, results[LoginTypeBasic].Err)
		assert.Equal(t, []string{"PUT /oai-id visible", "PUT /basic-id hidden", "PUT /basic-id hidden"}, writes(f))
		assert.Equal(t, []any{"WEB_BROWSERS"}, options.get(LoginTypeBasic)["authenticationRestrictions"].(map[string]any)["forbiddenClients"])
	})

	t.Run("refused while basic stays preferred", func(t *testing.T) {
		f := newFakeC8y(t)
		initial := defaultLoginOptions()
		initial[0]["visibleOnLoginPage"] = true
		initial[1]["visibleOnLoginPage"] = false
		newFakeLoginOptions(f, initial...)
		s := newFakeSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
			Basic: &BasicLoginOptionSpec{ForbiddenClients: []string{"WEB_BROWSERS"}},
		}))
		assert.ErrorContains(t, loginResults(s)[LoginTypeBasic].Err, "while basic is the preferred login mode")
		assert.Empty(t, writes(f))
	})

	t.Run("basic cannot become preferred while forbidden for browsers", func(t *testing.T) {
		f := newFakeC8y(t)
		initial := defaultLoginOptions()
		initial[0]["authenticationRestrictions"] = map[string]any{"forbiddenClients": "WEB_BROWSERS"}
		newFakeLoginOptions(f, initial...)
		s := newFakeSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeBasic}))
		assert.ErrorContains(t, loginResults(s)[loginItemPreferred].Err, "kept login mode oaiSecure: basic authentication is forbidden for web browsers")
		assert.Empty(t, writes(f))
	})

	t.Run("lifting the restriction comes before the switch to basic", func(t *testing.T) {
		f := newFakeC8y(t)
		initial := defaultLoginOptions()
		initial[0]["authenticationRestrictions"] = map[string]any{"forbiddenClients": "WEB_BROWSERS"}
		newFakeLoginOptions(f, initial...)
		s := newFakeSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
			Preferred: LoginModeBasic,
			Basic:     &BasicLoginOptionSpec{ForbiddenClients: []string{}},
		}))
		results := loginResults(s)
		assert.Equal(t, `authenticationRestrictions.forbiddenClients: "WEB_BROWSERS" → []`, results[LoginTypeBasic].Detail)
		assert.Equal(t, ActionUpdated, results[loginItemPreferred].Action, results[loginItemPreferred].Err)
		assert.Equal(t, []string{"PUT /basic-id hidden", "PUT /basic-id visible", "PUT /oai-id hidden"}, writes(f))
	})
}

func TestSyncLoginOptionsUpdatesOAISecureSession(t *testing.T) {
	f := newFakeC8y(t)
	options := newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
		Preferred: LoginModeOAISecure,
		OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{AbsoluteTimeout: "336h", RenewalTimeout: "2h", MaximumParallelSessions: intPtr(5)}},
	}))
	results := loginResults(s)
	assert.Equal(t, ActionUpdated, results[LoginTypeOAISecure].Action)
	assert.Equal(t, "sessionConfiguration.renewalTimeoutMillis: 86400000 → 7200000", results[LoginTypeOAISecure].Detail)
	assert.Equal(t, ActionUnchanged, results[loginItemPreferred].Action)

	session := options.get(LoginTypeOAISecure)["sessionConfiguration"].(map[string]any)
	assert.Equal(t, float64(7200000), session["renewalTimeoutMillis"])
	assert.Equal(t, false, session["userAgentValidationRequired"], "other fields are kept")
	assert.NotContains(t, f.Requests(http.MethodPut, "/tenant/loginOptions/oai-id")[0].Body, "self")
}

func TestSyncLoginOptionsCreatesMissingOAISecure(t *testing.T) {
	f := newFakeC8y(t)
	initial := defaultLoginOptions()[:1]
	initial[0]["visibleOnLoginPage"] = true
	options := newFakeLoginOptions(f, initial...)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeOAISecure}))
	results := loginResults(s)
	assert.Equal(t, ActionCreated, results[LoginTypeOAISecure].Action)
	assert.Equal(t, ActionUpdated, results[loginItemPreferred].Action, results[loginItemPreferred].Err)
	assert.Equal(t, "PASSWORD", options.get(LoginTypeOAISecure)["grantType"])
	assert.Equal(t, []string{"POST  hidden", "PUT /oauth2_internal-id visible", "PUT /basic-id hidden"}, writes(f))
}

func TestSyncLoginOptionsHidesSSOAfterSwitchingAway(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	options := newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))
	require.Equal(t, ActionUpdated, loginResults(s)[loginItemPreferred].Action)

	// Hiding the SSO button without a new preferred mode is refused
	spec := ssoSpec(p)
	spec.VisibleOnLoginPage = boolPtr(false)
	s = newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: spec}))
	assert.ErrorContains(t, loginResults(s)[LoginTypeSSO+" visibility"].Err, "sso is the preferred login mode")
	assert.Equal(t, true, options.get(LoginTypeSSO)["visibleOnLoginPage"])

	// With a new preferred mode it is hidden once the mode switched
	s = newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeOAISecure, SSO: spec}))
	results := loginResults(s)
	assert.Equal(t, ActionUpdated, results[loginItemPreferred].Action, results[loginItemPreferred].Err)
	assert.Equal(t, ActionUpdated, results[LoginTypeSSO+" visibility"].Action, results[LoginTypeSSO+" visibility"].Err)
	assert.Equal(t, false, options.get(LoginTypeSSO)["visibleOnLoginPage"])
	assert.Equal(t, true, options.get(LoginTypeOAISecure)["visibleOnLoginPage"])
}

func TestSSOClientSecretFromIsResolvedAndMasked(t *testing.T) {
	t.Setenv("TEST_SSO_SECRET", testSSOSecret)
	m := &Manifest{LoginOptions: &LoginOptionsSpec{SSO: &SSOLoginSpec{ClientID: "cumulocity", ClientSecretFrom: &SecretRef{Env: "TEST_SSO_SECRET"}}}}
	values, err := m.ResolveSecretRefs(nil)
	require.NoError(t, err)
	assert.Equal(t, testSSOSecret, m.LoginOptions.SSO.ClientSecret)
	assert.Nil(t, m.LoginOptions.SSO.ClientSecretFrom)

	redactor := &Redactor{}
	redactor.Add(values...)
	body := "grant_type=authorization_code&client_id=cumulocity&client_secret=s3cr3t%2Bvalue%2Fwith%3Dchars"
	assert.NotContains(t, redactor.Redact(body), "s3cr3t")
	assert.NotContains(t, redactor.Redact(clientSecretBasicHeader("cumulocity", testSSOSecret)), "Y3VtdWxvY2l0eTpzM2NyM3Q")
}

func TestLoginOptionsFromIncludesReplace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.yaml"), []byte("loginOptions:\n  preferred: basic\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tenant.yaml"), []byte("include: [base.yaml]\nloginOptions:\n  preferred: oaiSecure\n"), 0o644))
	m, err := LoadManifest(filepath.Join(dir, "tenant.yaml"))
	require.NoError(t, err)
	assert.Equal(t, LoginModeOAISecure, m.LoginOptions.Preferred)
}

func TestSSOOverridesAreMerged(t *testing.T) {
	p := newFakeOIDC(t)
	spec := ssoSpec(p)
	spec.Overrides = map[string]any{
		"tokenRequest": map[string]any{"headers": map[string]any{"X-Extra": "1"}},
		"audience":     "api://cumulocity",
	}
	option, err := buildSSOLoginOption(context.Background(), spec, "")
	require.NoError(t, err)
	headers := option.body["tokenRequest"].(map[string]any)["headers"].(map[string]any)
	assert.Equal(t, "1", headers["X-Extra"])
	assert.Equal(t, "application/x-www-form-urlencoded", headers["Content-Type"], "generated fields are kept")
	assert.Equal(t, "api://cumulocity", option.body["audience"])
}

// newDryRunLoginSyncer returns a dry-run syncer which reads the fake tenant
// through its dry-run reader (and has no client to write with)
func newDryRunLoginSyncer(t *testing.T, f *fakeC8y) *Syncer {
	t.Helper()
	return &Syncer{DryRun: true, DryRunReader: f.Client()}
}

func TestSyncLoginOptionsDryRunComparesWithTenant(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		f := newFakeC8y(t)
		newFakeLoginOptions(f, defaultLoginOptions()...)
		s := newDryRunLoginSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
			Preferred: LoginModeOAISecure,
			OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{AbsoluteTimeout: "336h"}},
		}))
		results := loginResults(s)
		assert.Equal(t, ActionUnchanged, results[loginItemPreferred].Action)
		assert.Equal(t, ActionUnchanged, results[LoginTypeOAISecure].Action)
		assert.Empty(t, writes(f))
	})

	t.Run("changes are listed field by field", func(t *testing.T) {
		f := newFakeC8y(t)
		newFakeLoginOptions(f, defaultLoginOptions()...)
		s := newDryRunLoginSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
			Preferred: LoginModeBasic,
			OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{RenewalTimeout: "2h"}},
		}))
		results := loginResults(s)
		assert.Equal(t, ActionPlanned, results[LoginTypeOAISecure].Action)
		assert.Equal(t, "sessionConfiguration.renewalTimeoutMillis: 86400000 → 7200000", results[LoginTypeOAISecure].Detail)
		assert.Equal(t, ActionPlanned, results[loginItemPreferred].Action, results[loginItemPreferred].Err)
		assert.Equal(t, "login mode oaiSecure → basic (BASIC visibleOnLoginPage=true, OAUTH2_INTERNAL visibleOnLoginPage=false)", results[loginItemPreferred].Detail)
		assert.Empty(t, writes(f), "a dry run never writes")
	})

	t.Run("sso is created and resolved", func(t *testing.T) {
		p := newFakeOIDC(t)
		f := newFakeC8y(t)
		fakeSSOGroupsAndApps(f)
		newFakeLoginOptions(f, defaultLoginOptions()...)
		s := newDryRunLoginSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))
		results := loginResults(s)
		sso := results[LoginTypeSSO]
		assert.Equal(t, ActionPlanned, sso.Action, sso.Err)
		assert.True(t, strings.HasPrefix(sso.Detail, "create: "), sso.Detail)
		assert.Contains(t, sso.Detail, "tokenRequest.body=***")
		assert.Contains(t, sso.Detail, "thenGroups\":[3]")
		assert.NotContains(t, sso.Detail, "s3cr3t")
		assert.Equal(t, "login mode oaiSecure → sso (OAUTH2_INTERNAL visibleOnLoginPage=false)", results[loginItemPreferred].Detail)
		assert.Empty(t, writes(f))
	})

	t.Run("an unknown group fails like in a real run", func(t *testing.T) {
		p := newFakeOIDC(t)
		f := newFakeC8y(t)
		newFakeLoginOptions(f, defaultLoginOptions()...)
		s := newDryRunLoginSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: ssoSpec(p)}))
		results := loginResults(s)
		assert.ErrorContains(t, results[LoginTypeSSO].Err, `user group "admins"`)
		assert.ErrorContains(t, results[loginItemPreferred].Err, "kept login mode oaiSecure")
	})

	t.Run("refusals are shown", func(t *testing.T) {
		f := newFakeC8y(t)
		initial := defaultLoginOptions()
		initial[0]["visibleOnLoginPage"] = true
		initial[1]["visibleOnLoginPage"] = false
		newFakeLoginOptions(f, initial...)
		s := newDryRunLoginSyncer(t, f)

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
			Basic: &BasicLoginOptionSpec{ForbiddenClients: []string{"WEB_BROWSERS"}},
		}))
		assert.ErrorContains(t, loginResults(s)[LoginTypeBasic].Err, "while basic is the preferred login mode")
	})

	t.Run("other tenants are not compared", func(t *testing.T) {
		f := newFakeC8y(t)
		newFakeLoginOptions(f, defaultLoginOptions()...)
		s := newDryRunLoginSyncer(t, f)
		s.activeTarget = &Target{TenantID: "t456"}

		require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeOAISecure}))
		result := loginResults(s)[loginItemPreferred]
		assert.Equal(t, ActionPlanned, result.Action)
		assert.Contains(t, result.Detail, "not compared with the tenant")
		assert.Empty(t, f.Requests(http.MethodGet, "/tenant/loginOptions"))
	})
}

func TestLoginOptionsServiceUserRolesDoNotIncludeTenantAdmin(t *testing.T) {
	assert.NotContains(t, serviceUserRequiredRoles, "ROLE_TENANT_ADMIN")
	assert.Contains(t, sectionServiceUserRoles[SectionLoginOptions], "ROLE_TENANT_MANAGEMENT_ADMIN")
}

func TestLoginOptionsValidateRejectsGeneratedSecret(t *testing.T) {
	p := newFakeOIDC(t)
	spec := ssoSpec(p)
	spec.ClientSecret = ""
	spec.ClientSecretFrom = &SecretRef{Generate: "sso-secret"}
	errs := strings.Join((&LoginOptionsSpec{SSO: spec}).validate(), "\n")
	assert.Contains(t, errs, "clientSecretFrom: generate is not supported")

	spec.ClientSecretFrom = &SecretRef{Env: "SSO_SECRET"}
	assert.Empty(t, (&LoginOptionsSpec{SSO: spec}).validate())
}

func TestSyncLoginOptionsRejectsInsecureEndpoints(t *testing.T) {
	for _, field := range []string{"token_endpoint", "authorization_endpoint", "jwks_uri", "end_session_endpoint"} {
		t.Run(field, func(t *testing.T) {
			p := newFakeOIDC(t)
			p.discovery[field] = "http://idp.example.com/" + field
			_, err := buildSSOLoginOption(context.Background(), ssoSpec(p), "")
			assert.ErrorContains(t, err, field+": must be an https URL")
		})
	}
}

func TestSyncLoginOptionsReplacesGeneratedRequests(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	options := newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: ssoSpec(p)}))
	require.Equal(t, ActionCreated, loginResults(s)[LoginTypeSSO].Action)

	// An old Authorization header (e.g. from client_secret_basic) and an
	// extra request parameter are left in the tenant's option
	sso := options.get(LoginTypeSSO)
	sso["tokenRequest"].(map[string]any)["headers"].(map[string]any)["Authorization"] = "Basic b2xkOnNlY3JldA=="
	sso["authorizationRequest"].(map[string]any)["requestParams"].(map[string]any)["prompt"] = "login"

	s = newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: ssoSpec(p)}))
	result := loginResults(s)[LoginTypeSSO]
	assert.Equal(t, ActionUpdated, result.Action, result.Err)
	assert.Contains(t, result.Detail, "tokenRequest.headers.Authorization removed")
	assert.Contains(t, result.Detail, `authorizationRequest.requestParams.prompt: "login" → (unset)`)
	assert.NotContains(t, result.Detail, "b2xk")

	updated := options.get(LoginTypeSSO)
	assert.NotContains(t, updated["tokenRequest"].(map[string]any)["headers"], "Authorization")
	assert.NotContains(t, updated["authorizationRequest"].(map[string]any)["requestParams"], "prompt")
}

func TestSyncLoginOptionsLeavesOutReadOnlyFields(t *testing.T) {
	f := newFakeC8y(t)
	initial := defaultLoginOptions()
	for _, option := range initial {
		option["_type"] = "OAuth2Config"
		option["tfaSupported"] = true
		option["initRequest"] = "https://t123/tenant/oauth?tenant_id=t123"
		option["loginRedirectDomain"] = "t123.example.com"
		option["greenMinLength"] = nil
		option["tfaStrategy"] = "TOTP"
		option["enforceStrength"] = true
		option["strengthValidity"] = true
	}
	newFakeLoginOptions(f, initial...)
	s := newFakeSyncer(t, f)

	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{
		Preferred: LoginModeBasic,
		OAISecure: &OAISecureLoginSpec{Session: &OAISecureSessionSpec{RenewalTimeout: "2h"}},
	}))
	assert.Equal(t, ActionUpdated, loginResults(s)[loginItemPreferred].Action, loginResults(s)[loginItemPreferred].Err)

	puts := f.Requests(http.MethodPut, "/tenant/loginOptions/")
	require.Len(t, puts, 3)
	for _, put := range puts {
		for _, field := range readOnlyLoginOptionFields {
			assert.NotContains(t, put.Body, field, "%s %s", put.Path, field)
		}
		assert.Contains(t, put.Body, "type")
		assert.Contains(t, put.Body, "visibleOnLoginPage")
		// The password strength and TFA settings are sent back unchanged
		assert.Equal(t, "TOTP", put.Body["tfaStrategy"], put.Path)
		assert.Equal(t, true, put.Body["enforceStrength"], put.Path)
		assert.Equal(t, true, put.Body["strengthValidity"], put.Path)
	}
}

// exportSSOTenant is a fake tenant in SSO mode with the given SSO option
func exportSSOTenant(t *testing.T, sso map[string]any) *fakeC8y {
	t.Helper()
	f := fakeExportTenant(t)
	options := exportLoginOptions()
	options[1]["visibleOnLoginPage"] = false // OAI-Secure hidden: sso mode
	if sso == nil {
		options = options[:2]
	} else {
		options[2] = sso
	}
	f.JSON("GET /tenant/loginOptions", http.StatusOK, map[string]any{"loginOptions": options, "statistics": stats()})
	return f
}

func TestExportLoginOptionsKeepsManifestValid(t *testing.T) {
	t.Run("sso without issuer is left out", func(t *testing.T) {
		f := exportSSOTenant(t, map[string]any{"id": "s1", "type": "OAUTH2", "visibleOnLoginPage": true, "clientId": "c"})
		var spec *LoginOptionsSpec
		var err error
		_, stderr := captureStdio(t, func() {
			spec, err = (&Exporter{Client: f.Client()}).loginOptions(context.Background())
		})
		require.NoError(t, err)
		assert.Nil(t, spec.SSO)
		assert.Empty(t, spec.Preferred, "preferred sso without an sso section would not validate")
		assert.Contains(t, stderr, "has no issuer")
		assert.Contains(t, stderr, "preferred: sso is not exported")
		assert.Empty(t, spec.validate())
	})

	t.Run("sso mode without rules leaves preferred out", func(t *testing.T) {
		sso := exportLoginOptions()[2]
		delete(sso, "onNewUser")
		f := exportSSOTenant(t, sso)
		var spec *LoginOptionsSpec
		var err error
		captureStdio(t, func() {
			spec, err = (&Exporter{Client: f.Client()}).loginOptions(context.Background())
		})
		require.NoError(t, err)
		require.NotNil(t, spec.SSO)
		assert.Empty(t, spec.Preferred)
		assert.Empty(t, spec.validate())
	})

	t.Run("sso mode with rules keeps preferred", func(t *testing.T) {
		f := exportSSOTenant(t, exportLoginOptions()[2])
		spec, err := (&Exporter{Client: f.Client()}).loginOptions(context.Background())
		require.NoError(t, err)
		assert.Equal(t, LoginModeSSO, spec.Preferred)
		assert.Empty(t, spec.validate())
	})

	t.Run("a discovery URL which does not work is flagged", func(t *testing.T) {
		f := fakeExportTenant(t)
		stubExportDiscovery(t, assert.AnError)
		_, stderr := captureStdio(t, func() {
			_, err := (&Exporter{Client: f.Client()}).loginOptions(context.Background())
			require.NoError(t, err)
		})
		assert.Contains(t, stderr, "check sso.discoveryUrl")
	})

	t.Run("an unreadable group fails the export", func(t *testing.T) {
		f := fakeExportTenant(t)
		f.JSON("GET /user/t123/groups/3", http.StatusForbidden, map[string]any{"error": "security/Forbidden", "message": "denied"})
		_, err := (&Exporter{Client: f.Client()}).loginOptions(context.Background())
		assert.ErrorContains(t, err, "sso access mapping: user group 3")
	})

	t.Run("an unreadable application fails the export", func(t *testing.T) {
		f := fakeExportTenant(t)
		f.JSON("GET /application/applications/11", http.StatusNotFound, map[string]any{"error": "notFound", "message": "gone"})
		_, err := (&Exporter{Client: f.Client()}).loginOptions(context.Background())
		assert.ErrorContains(t, err, "sso access mapping: application 11")
	})
}

// TestRunDryRunComparesLoginOptions checks the dry-run reader wiring of the
// run command: the login options are read (never written) and compared
func TestRunDryRunComparesLoginOptions(t *testing.T) {
	f := newFakeC8y(t)
	newFakeLoginOptions(f, defaultLoginOptions()...)
	setupRunAgainst(t, f)

	manifest := filepath.Join(t.TempDir(), "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("loginOptions:\n  preferred: oaiSecure\n  oaiSecure:\n    session:\n      renewalTimeout: 2h\n"), 0o644))

	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--dry")
	})
	require.NoError(t, runErr, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "[loginOptions] preferred (unchanged): oaiSecure")
	assert.Contains(t, stdout, "[loginOptions] OAUTH2_INTERNAL (planned): sessionConfiguration.renewalTimeoutMillis: 86400000 → 7200000")
	assert.NotEmpty(t, f.Requests(http.MethodGet, "/tenant/loginOptions"))
	assert.Empty(t, writes(f), "a dry run never writes")
}

func TestSyncLoginOptionsOverridesReplacingAccessMappingsFail(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	newFakeLoginOptions(f, defaultLoginOptions()...)
	for _, overrides := range []map[string]any{
		{"onNewUser": nil},
		{"onNewUser": map[string]any{"dynamicMapping": "none"}},
	} {
		s := newFakeSyncer(t, f)
		spec := ssoSpec(p)
		spec.Overrides = overrides
		require.NotPanics(t, func() {
			require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{Preferred: LoginModeSSO, SSO: spec}))
		})
		results := loginResults(s)
		assert.ErrorContains(t, results[LoginTypeSSO].Err, "overrides replace onNewUser")
		assert.ErrorContains(t, results[loginItemPreferred].Err, "kept login mode oaiSecure")
	}
	assert.Empty(t, writes(f))
}

func TestSyncLoginOptionsRemovesStaleLogoutRequest(t *testing.T) {
	p := newFakeOIDC(t)
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	options := newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: ssoSpec(p)}))
	require.NotNil(t, options.get(LoginTypeSSO)["logoutRequest"])

	// The provider no longer has a logout endpoint
	p.discovery["end_session_endpoint"] = nil
	s = newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: ssoSpec(p)}))
	result := loginResults(s)[LoginTypeSSO]
	assert.Equal(t, ActionUpdated, result.Action, result.Err)
	assert.Contains(t, result.Detail, "logoutRequest: {")
	assert.Contains(t, result.Detail, "→ (unset)")
	puts := f.Requests(http.MethodPut, "/tenant/loginOptions/oauth2-id")
	require.NotEmpty(t, puts)
	last := puts[len(puts)-1].Body
	assert.Contains(t, last, "logoutRequest")
	assert.Nil(t, last["logoutRequest"], "null removes the logout request")

	// Then nothing changes, and options without one never get a null
	s = newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: ssoSpec(p)}))
	assert.Equal(t, ActionUnchanged, loginResults(s)[LoginTypeSSO].Action, loginResults(s)[LoginTypeSSO].Detail)
}

func TestSyncLoginOptionsCreateWithoutLogoutEndpoint(t *testing.T) {
	p := newFakeOIDC(t)
	p.discovery["end_session_endpoint"] = nil
	f := newFakeC8y(t)
	fakeSSOGroupsAndApps(f)
	newFakeLoginOptions(f, defaultLoginOptions()...)
	s := newFakeSyncer(t, f)
	require.NoError(t, s.SyncLoginOptions(context.Background(), &LoginOptionsSpec{SSO: ssoSpec(p)}))
	assert.Equal(t, ActionCreated, loginResults(s)[LoginTypeSSO].Action)
	created := f.Requests(http.MethodPost, "/tenant/loginOptions")
	require.Len(t, created, 1)
	assert.NotContains(t, created[0].Body, "logoutRequest")
	assert.NotContains(t, loginResults(s)[LoginTypeSSO].Detail, "logoutRequest")
}

func TestExportLoginOptionsSkipsInvalidSessionTimeouts(t *testing.T) {
	f := fakeExportTenant(t)
	options := exportLoginOptions()
	options[1]["sessionConfiguration"] = map[string]any{"absoluteTimeoutMillis": 600000, "renewalTimeoutMillis": 300000, "maximumNumberOfParallelSessions": 3}
	f.JSON("GET /tenant/loginOptions", http.StatusOK, map[string]any{"loginOptions": options, "statistics": stats()})

	var spec *LoginOptionsSpec
	var err error
	_, stderr := captureStdio(t, func() {
		spec, err = (&Exporter{Client: f.Client()}).loginOptions(context.Background())
	})
	require.NoError(t, err)
	require.NotNil(t, spec.OAISecure)
	assert.Empty(t, spec.OAISecure.Session.AbsoluteTimeout)
	assert.Empty(t, spec.OAISecure.Session.RenewalTimeout)
	assert.Equal(t, 3, *spec.OAISecure.Session.MaximumParallelSessions, "other session settings are kept")
	assert.Contains(t, stderr, "OAI-Secure session timeouts (absoluteTimeout 10m, renewalTimeout 5m) are not exported")
	assert.Empty(t, spec.validate())
}
