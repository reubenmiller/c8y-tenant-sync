package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// oidcDiscoverySuffix is the path of the OpenID Connect discovery document
const oidcDiscoverySuffix = "/.well-known/openid-configuration"

// oidcProvider holds the fields of an OpenID Connect discovery document
// which the SSO login option is generated from
type oidcProvider struct {
	Issuer                     string   `json:"issuer"`
	AuthorizationEndpoint      string   `json:"authorization_endpoint"`
	TokenEndpoint              string   `json:"token_endpoint"`
	EndSessionEndpoint         string   `json:"end_session_endpoint"`
	JWKSURI                    string   `json:"jwks_uri"`
	TokenEndpointAuthMethods   []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported            []string `json:"scopes_supported"`
	ResponseTypesSupported     []string `json:"response_types_supported"`
	GrantTypesSupported        []string `json:"grant_types_supported"`
	IDTokenSigningAlgSupported []string `json:"id_token_signing_alg_values_supported"`
}

// clientSecretBasic reports whether the client secret has to be sent in an
// Authorization header (client_secret_basic) rather than in the request body
// (client_secret_post). The body is used whenever the provider supports it,
// or does not say (client_secret_basic is the OIDC default, but providers
// which omit the list generally accept both).
func (p oidcProvider) clientSecretBasic() bool {
	if len(p.TokenEndpointAuthMethods) == 0 {
		return false
	}
	return !slices.Contains(p.TokenEndpointAuthMethods, "client_secret_post") &&
		slices.Contains(p.TokenEndpointAuthMethods, "client_secret_basic")
}

// oidcHTTPClient is used for the discovery and JWKS requests
var oidcHTTPClient = &http.Client{Timeout: 20 * time.Second}

// discoverOIDCProvider reads and checks the discovery document of an OpenID
// Connect provider: the issuer has to match the URL (as required by the
// OpenID Connect Discovery spec), the endpoints used by the login option have
// to be present, and the JWKS endpoint has to return at least one key.
func discoverOIDCProvider(ctx context.Context, discoveryURL string) (oidcProvider, error) {
	var provider oidcProvider
	if err := getJSON(ctx, discoveryURL, &provider); err != nil {
		return provider, fmt.Errorf("discovery document: %w", err)
	}

	var missing []string
	for _, field := range []struct{ name, value string }{
		{"issuer", provider.Issuer},
		{"authorization_endpoint", provider.AuthorizationEndpoint},
		{"token_endpoint", provider.TokenEndpoint},
		{"jwks_uri", provider.JWKSURI},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) > 0 {
		return provider, fmt.Errorf("discovery document %s is missing: %s", discoveryURL, strings.Join(missing, ", "))
	}

	if base, ok := strings.CutSuffix(discoveryURL, oidcDiscoverySuffix); ok {
		if strings.TrimSuffix(provider.Issuer, "/") != strings.TrimSuffix(base, "/") {
			return provider, fmt.Errorf("issuer %q of the discovery document does not match its URL %s", provider.Issuer, discoveryURL)
		}
	}
	if len(provider.ResponseTypesSupported) > 0 && !slices.Contains(provider.ResponseTypesSupported, "code") {
		return provider, fmt.Errorf("the provider does not support the authorization code flow (response_types_supported: %s)", strings.Join(provider.ResponseTypesSupported, ", "))
	}
	if len(provider.TokenEndpointAuthMethods) > 0 && !slices.Contains(provider.TokenEndpointAuthMethods, "client_secret_post") && !slices.Contains(provider.TokenEndpointAuthMethods, "client_secret_basic") {
		return provider, fmt.Errorf("the provider supports neither client_secret_post nor client_secret_basic (token_endpoint_auth_methods_supported: %s)", strings.Join(provider.TokenEndpointAuthMethods, ", "))
	}

	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := getJSON(ctx, provider.JWKSURI, &jwks); err != nil {
		return provider, fmt.Errorf("JWKS: %w", err)
	}
	if len(jwks.Keys) == 0 {
		return provider, fmt.Errorf("JWKS %s has no keys", provider.JWKSURI)
	}
	return provider, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := oidcHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: invalid JSON: %w", url, err)
	}
	return nil
}
