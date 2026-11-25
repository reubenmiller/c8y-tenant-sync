package main

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Preferred login modes (loginOptions.preferred)
const (
	LoginModeOAISecure = "oaiSecure"
	LoginModeBasic     = "basic"
	LoginModeSSO       = "sso"
)

// LoginModes are the accepted values of loginOptions.preferred
var LoginModes = []string{LoginModeOAISecure, LoginModeBasic, LoginModeSSO}

// Login option types of the /tenant/loginOptions API
const (
	LoginTypeBasic     = "BASIC"
	LoginTypeOAISecure = "OAUTH2_INTERNAL"
	LoginTypeSSO       = "OAUTH2"
)

// BasicForbiddenClients are the accepted values of
// loginOptions.basic.forbiddenClients
var BasicForbiddenClients = []string{"WEB_BROWSERS"}

// SSOPredicateOperators are the accepted operators of an access mapping
// condition
var SSOPredicateOperators = []string{"EQ", "NEQ", "GT", "LT", "GTE", "LTE", "IN"}

// minOAISecureSessionTimeout is the shortest accepted OAI-Secure session
// timeout: shorter sessions would log users (and the sync) out constantly
const minOAISecureSessionTimeout = 15 * time.Minute

// LoginOptionsSpec configures how users log in to the tenant: the preferred
// login mode shown on the login page, and the settings of the basic,
// OAI-Secure and single sign-on (SSO) login options. The section is additive:
// login options are never deleted, and the preferred mode is only switched
// once the new option exists and is configured.
type LoginOptionsSpec struct {
	Preferred string                `yaml:"preferred" json:"preferred,omitempty" jsonschema:"enum=oaiSecure,enum=basic,enum=sso,description=Preferred login mode of the login page: oaiSecure (recommended) or basic or sso (single sign-on redirect which hides the username/password form); omitted leaves it unchanged"`
	Basic     *BasicLoginOptionSpec `yaml:"basic" json:"basic,omitempty" jsonschema:"description=Restrictions of basic authentication (the BASIC login option)"`
	OAISecure *OAISecureLoginSpec   `yaml:"oaiSecure" json:"oaiSecure,omitempty" jsonschema:"description=Settings of the OAI-Secure login option (OAUTH2_INTERNAL)"`
	SSO       *SSOLoginSpec         `yaml:"sso" json:"sso,omitempty" jsonschema:"description=Single sign-on (OAUTH2) login option generated from an OpenID Connect provider's discovery document"`
}

// BasicLoginOptionSpec sets the authentication restrictions of the BASIC
// login option. A list which is omitted is left unchanged; an empty list
// clears it.
type BasicLoginOptionSpec struct {
	ForbiddenClients    []string `yaml:"forbiddenClients" json:"forbiddenClients,omitempty" jsonschema:"description=Clients which may not use basic authentication; the only supported value is WEB_BROWSERS"`
	TrustedUserAgents   []string `yaml:"trustedUserAgents" json:"trustedUserAgents,omitempty" jsonschema:"description=User agents (User-Agent header) which may always use basic authentication"`
	ForbiddenUserAgents []string `yaml:"forbiddenUserAgents" json:"forbiddenUserAgents,omitempty" jsonschema:"description=User agents (User-Agent header) which may not use basic authentication"`
}

// OAISecureLoginSpec configures the OAI-Secure login option
type OAISecureLoginSpec struct {
	Session *OAISecureSessionSpec `yaml:"session" json:"session,omitempty" jsonschema:"description=OAI-Secure session configuration"`
}

// OAISecureSessionSpec is the OAI-Secure session configuration. Omitted
// fields are left unchanged.
type OAISecureSessionSpec struct {
	AbsoluteTimeout             string `yaml:"absoluteTimeout" json:"absoluteTimeout,omitempty" jsonschema:"description=Maximum session duration after which users have to log in again (Go duration; at least 15m),example=336h"`
	RenewalTimeout              string `yaml:"renewalTimeout" json:"renewalTimeout,omitempty" jsonschema:"description=Time before the token expires during which it may be renewed (Go duration; shorter than absoluteTimeout),example=24h"`
	MaximumParallelSessions     *int   `yaml:"maximumParallelSessions" json:"maximumParallelSessions,omitempty" jsonschema:"description=Maximum number of parallel sessions of one user (at least 1)"`
	UserAgentValidationRequired *bool  `yaml:"userAgentValidationRequired" json:"userAgentValidationRequired,omitempty" jsonschema:"description=Check the user agent during the session"`
}

// SSOLoginSpec declares a single sign-on login option. The endpoints are
// generated from the OpenID Connect discovery document of the provider
// (discoveryUrl); the client ID, client secret and the claim and access
// mappings come from the manifest.
type SSOLoginSpec struct {
	DiscoveryURL string `yaml:"discoveryUrl" json:"discoveryUrl" jsonschema:"description=OpenID Connect discovery document of the provider (https://.../.well-known/openid-configuration); the issuer and the authorization/token/logout/JWKS endpoints are taken from it,example=https://idp.example.com/realms/acme/.well-known/openid-configuration"`
	ClientID     string `yaml:"clientId" json:"clientId" jsonschema:"description=Client ID of Cumulocity at the provider"`
	// ClientSecret supports ${VAR} expansion and sops-encrypted values
	ClientSecret     string     `yaml:"clientSecret" json:"clientSecret,omitempty" jsonschema:"description=Client secret; supports ${VAR} expansion and sops-encrypted values; mutually exclusive with clientSecretFrom"`
	ClientSecretFrom *SecretRef `yaml:"clientSecretFrom" json:"clientSecretFrom,omitempty" jsonschema:"description=Take the client secret from an environment variable (env) or a generated value (generate); mutually exclusive with clientSecret"`

	Scopes       []string `yaml:"scopes" json:"scopes,omitempty" jsonschema:"description=Scopes requested at login (default: openid profile email)"`
	ProviderName string   `yaml:"providerName" json:"providerName,omitempty" jsonschema:"description=Name of the provider (default: the host of the issuer)"`
	ButtonName   string   `yaml:"buttonName" json:"buttonName,omitempty" jsonschema:"description=Text of the login button (default: Log in with <providerName>)"`
	Audience     string   `yaml:"audience" json:"audience,omitempty" jsonschema:"description=Expected audience of the tokens"`
	Template     string   `yaml:"template" json:"template,omitempty" jsonschema:"description=Template name shown by the Administration application (left unset by default)"`
	// RedirectToPlatform is left unset by default, so the UI applications
	// control the SSO flow
	RedirectToPlatform string `yaml:"redirectToPlatform" json:"redirectToPlatform,omitempty" jsonschema:"description=URL to redirect to after login; leave unset to let the UI applications control the SSO flow"`
	VisibleOnLoginPage *bool  `yaml:"visibleOnLoginPage" json:"visibleOnLoginPage,omitempty" jsonschema:"description=Show the SSO button on the login page (default: true; always true when preferred is sso)"`

	UseIDToken  *bool                 `yaml:"useIdToken" json:"useIdToken,omitempty" jsonschema:"description=Read the user ID and user data from the ID token instead of the access token (default: true)"`
	UserIDClaim string                `yaml:"userIdClaim" json:"userIdClaim,omitempty" jsonschema:"description=Claim used as the Cumulocity username (default: sub),example=preferred_username"`
	UserData    *SSOUserDataSpec      `yaml:"userData" json:"userData,omitempty" jsonschema:"description=Claims holding the user data (defaults: email / given_name / family_name)"`
	Access      *SSOAccessMappingSpec `yaml:"accessMappings" json:"accessMappings,omitempty" jsonschema:"description=Dynamic access mapping: which user groups and applications users get based on their token claims"`

	// Overrides are merged last into the generated login option, for fields
	// which are not modelled. Placeholders of the platform must be escaped
	// as $${code}.
	Overrides map[string]any `yaml:"overrides" json:"overrides,omitempty" jsonschema:"description=Raw login option fields merged last into the generated configuration (escape platform placeholders as $${code})"`
}

// SSOUserDataSpec names the claims which hold the user data
type SSOUserDataSpec struct {
	Email       string `yaml:"email" json:"email,omitempty" jsonschema:"description=Claim holding the email address (default: email)"`
	FirstName   string `yaml:"firstName" json:"firstName,omitempty" jsonschema:"description=Claim holding the first name (default: given_name)"`
	LastName    string `yaml:"lastName" json:"lastName,omitempty" jsonschema:"description=Claim holding the last name (default: family_name)"`
	PhoneNumber string `yaml:"phoneNumber" json:"phoneNumber,omitempty" jsonschema:"description=Claim holding the phone number"`
}

// SSOAccessMappingSpec is the dynamic access mapping of the SSO login option
type SSOAccessMappingSpec struct {
	MapRolesOnlyForNewUser           bool `yaml:"mapRolesOnlyForNewUser" json:"mapRolesOnlyForNewUser,omitempty" jsonschema:"description=Only map the roles when a user logs in for the first time"`
	ManageRolesOnlyFromAccessMapping bool `yaml:"manageRolesOnlyFromAccessMapping" json:"manageRolesOnlyFromAccessMapping,omitempty" jsonschema:"description=Only manage the groups and applications listed in the rules; others assigned in Cumulocity are kept"`
	// MapFromIDToken defaults to useIdToken
	MapFromIDToken *bool               `yaml:"mapFromIdToken" json:"mapFromIdToken,omitempty" jsonschema:"description=Evaluate the rules against the ID token instead of the access token (default: useIdToken)"`
	Rules          []SSOAccessRuleSpec `yaml:"rules" json:"rules,omitempty" jsonschema:"description=Rules which assign user groups and applications when their conditions match"`
}

// SSOAccessRuleSpec assigns groups and applications when its conditions match
type SSOAccessRuleSpec struct {
	Match        string             `yaml:"match" json:"match,omitempty" jsonschema:"enum=all,enum=any,description=Whether all (default) or any of the conditions must match"`
	When         []SSOConditionSpec `yaml:"when" json:"when" jsonschema:"description=Conditions on token claims"`
	Groups       []string           `yaml:"groups" json:"groups,omitempty" jsonschema:"description=User group names to assign"`
	Applications []string           `yaml:"applications" json:"applications,omitempty" jsonschema:"description=Application names to give access to"`
}

// SSOConditionSpec is a condition on a token claim
type SSOConditionSpec struct {
	Claim    string `yaml:"claim" json:"claim" jsonschema:"description=Path of the claim in the token,example=groups"`
	Operator string `yaml:"operator" json:"operator,omitempty" jsonschema:"enum=EQ,enum=NEQ,enum=GT,enum=LT,enum=GTE,enum=LTE,enum=IN,description=Comparison operator (default: EQ; IN matches a value in a list claim)"`
	Value    string `yaml:"value" json:"value" jsonschema:"description=Value compared with the claim"`
}

// visible reports whether the SSO button is shown on the login page
func (s *SSOLoginSpec) visible(preferred string) bool {
	if preferred == LoginModeSSO {
		return true
	}
	return s.VisibleOnLoginPage == nil || *s.VisibleOnLoginPage
}

func (s *SSOLoginSpec) useIDToken() bool {
	return s.UseIDToken == nil || *s.UseIDToken
}

// validate checks the loginOptions section. A nil section is valid.
func (l *LoginOptionsSpec) validate() []string {
	if l == nil {
		return nil
	}
	var errs []string
	if l.Preferred != "" && !slices.Contains(LoginModes, l.Preferred) {
		errs = append(errs, fmt.Sprintf("loginOptions.preferred: unknown login mode %q: supported values are %s", l.Preferred, strings.Join(LoginModes, ", ")))
	}

	if l.Basic != nil {
		for i, client := range l.Basic.ForbiddenClients {
			if !slices.Contains(BasicForbiddenClients, client) {
				errs = append(errs, fmt.Sprintf("loginOptions.basic.forbiddenClients[%d]: unknown client %q: supported values are %s", i, client, strings.Join(BasicForbiddenClients, ", ")))
			}
		}
		if l.Preferred == LoginModeBasic && slices.Contains(l.Basic.ForbiddenClients, "WEB_BROWSERS") {
			errs = append(errs, "loginOptions: preferred basic cannot be combined with basic.forbiddenClients WEB_BROWSERS (nobody could log in on the login page)")
		}
		for i, agent := range l.Basic.ForbiddenUserAgents {
			if strings.TrimSpace(agent) == "" {
				errs = append(errs, fmt.Sprintf("loginOptions.basic.forbiddenUserAgents[%d]: must not be empty", i))
			}
		}
	}

	if l.OAISecure != nil && l.OAISecure.Session != nil {
		errs = append(errs, l.OAISecure.Session.validate()...)
	}

	if l.SSO != nil {
		errs = append(errs, l.SSO.validate(l.Preferred)...)
	} else if l.Preferred == LoginModeSSO {
		errs = append(errs, "loginOptions: preferred sso requires the sso login option to be declared (so it can be checked before it becomes the only login on the login page)")
	}
	return errs
}

func (s *OAISecureSessionSpec) validate() []string {
	var errs []string
	label := "loginOptions.oaiSecure.session"
	absolute, absoluteErr := parseOptionalDuration(s.AbsoluteTimeout)
	if absoluteErr != nil {
		errs = append(errs, fmt.Sprintf("%s.absoluteTimeout: %v", label, absoluteErr))
	} else if s.AbsoluteTimeout != "" && absolute < minOAISecureSessionTimeout {
		errs = append(errs, fmt.Sprintf("%s.absoluteTimeout: must be at least %s", label, minOAISecureSessionTimeout))
	}
	renewal, renewalErr := parseOptionalDuration(s.RenewalTimeout)
	if renewalErr != nil {
		errs = append(errs, fmt.Sprintf("%s.renewalTimeout: %v", label, renewalErr))
	} else if s.RenewalTimeout != "" && renewal <= 0 {
		errs = append(errs, fmt.Sprintf("%s.renewalTimeout: must be positive", label))
	}
	if absoluteErr == nil && renewalErr == nil && s.AbsoluteTimeout != "" && s.RenewalTimeout != "" && renewal >= absolute {
		errs = append(errs, fmt.Sprintf("%s: renewalTimeout must be shorter than absoluteTimeout", label))
	}
	if s.MaximumParallelSessions != nil && *s.MaximumParallelSessions < 1 {
		errs = append(errs, fmt.Sprintf("%s.maximumParallelSessions: must be at least 1", label))
	}
	return errs
}

func (s *SSOLoginSpec) validate(preferred string) []string {
	var errs []string
	label := "loginOptions.sso"
	if s.DiscoveryURL == "" {
		errs = append(errs, label+".discoveryUrl is required")
	} else if err := validateDiscoveryURL(s.DiscoveryURL); err != nil {
		errs = append(errs, fmt.Sprintf("%s.discoveryUrl: %v", label, err))
	}
	if strings.TrimSpace(s.ClientID) == "" {
		errs = append(errs, label+".clientId is required")
	}
	switch {
	case s.ClientSecret != "" && s.ClientSecretFrom != nil:
		errs = append(errs, label+": clientSecret and clientSecretFrom are mutually exclusive")
	case s.ClientSecret == "" && s.ClientSecretFrom == nil:
		errs = append(errs, label+": one of clientSecret or clientSecretFrom is required")
	case s.ClientSecretFrom != nil:
		errs = append(errs, s.ClientSecretFrom.validate(label+".clientSecretFrom")...)
	}
	if preferred == LoginModeSSO && s.VisibleOnLoginPage != nil && !*s.VisibleOnLoginPage {
		errs = append(errs, label+": visibleOnLoginPage cannot be false when preferred is sso")
	}
	if s.Access != nil {
		for i, rule := range s.Access.Rules {
			ruleLabel := fmt.Sprintf("%s.accessMappings.rules[%d]", label, i)
			if rule.Match != "" && rule.Match != "all" && rule.Match != "any" {
				errs = append(errs, fmt.Sprintf("%s.match: unknown value %q: supported values are all, any", ruleLabel, rule.Match))
			}
			if len(rule.When) == 0 {
				errs = append(errs, ruleLabel+".when requires at least one condition")
			}
			for j, cond := range rule.When {
				if cond.Claim == "" {
					errs = append(errs, fmt.Sprintf("%s.when[%d].claim is required", ruleLabel, j))
				}
				if cond.Operator != "" && !slices.Contains(SSOPredicateOperators, cond.Operator) {
					errs = append(errs, fmt.Sprintf("%s.when[%d].operator: unknown operator %q: supported values are %s", ruleLabel, j, cond.Operator, strings.Join(SSOPredicateOperators, ", ")))
				}
			}
			if len(rule.Groups) == 0 && len(rule.Applications) == 0 {
				errs = append(errs, ruleLabel+" requires groups or applications")
			}
		}
	}
	if preferred == LoginModeSSO && (s.Access == nil || len(s.Access.Rules) == 0) {
		errs = append(errs, label+": preferred sso requires at least one accessMappings rule (users without a matching rule are denied access, and the login page offers no other login)")
	}
	return errs
}

// validateDiscoveryURL requires an https URL (http only for local hosts)
func validateDiscoveryURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLocalHost(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("must be an https URL, got %q", raw)
}

func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseOptionalDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	return time.ParseDuration(value)
}

// formatDuration formats a duration without zero minutes/seconds (336h
// instead of 336h0m0s)
func formatDuration(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}
