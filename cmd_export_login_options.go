package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/usergroups"
)

// exportedSSOSecretEnv is the environment variable the exported SSO login
// option takes its client secret from (the secret itself is never exported)
const exportedSSOSecretEnv = "SSO_CLIENT_SECRET"

// loginOptions exports the login options. The SSO login option is exported
// as the discovery URL of its issuer with the client ID and the claim and
// access mappings; the endpoints are generated again from the discovery
// document when the manifest is applied.
func (e *Exporter) loginOptions(ctx context.Context) (*LoginOptionsSpec, error) {
	syncer := &Syncer{Client: e.Client}
	state, err := syncer.loginOptions(ctx)
	if err != nil {
		return nil, err
	}
	if len(state) == 0 {
		return nil, nil
	}

	spec := &LoginOptionsSpec{Preferred: state.preferredMode()}

	if basic, ok := state[LoginTypeBasic]; ok {
		restrictions, _ := basic["authenticationRestrictions"].(map[string]any)
		basicSpec := &BasicLoginOptionSpec{
			ForbiddenClients:    stringList(restrictions["forbiddenClients"]),
			TrustedUserAgents:   stringList(restrictions["trustedUserAgents"]),
			ForbiddenUserAgents: stringList(restrictions["forbiddenUserAgents"]),
		}
		if len(basicSpec.ForbiddenClients)+len(basicSpec.TrustedUserAgents)+len(basicSpec.ForbiddenUserAgents) > 0 {
			spec.Basic = basicSpec
		}
	}

	if oai, ok := state[LoginTypeOAISecure]; ok {
		if session, ok := oai["sessionConfiguration"].(map[string]any); ok {
			sessionSpec := &OAISecureSessionSpec{}
			if millis, ok := session["absoluteTimeoutMillis"].(float64); ok && millis > 0 {
				sessionSpec.AbsoluteTimeout = formatDuration(time.Duration(millis) * time.Millisecond)
			}
			if millis, ok := session["renewalTimeoutMillis"].(float64); ok && millis > 0 {
				sessionSpec.RenewalTimeout = formatDuration(time.Duration(millis) * time.Millisecond)
			}
			if count, ok := session["maximumNumberOfParallelSessions"].(float64); ok && count > 0 {
				value := int(count)
				sessionSpec.MaximumParallelSessions = &value
			}
			if value, ok := session["userAgentValidationRequired"].(bool); ok {
				sessionSpec.UserAgentValidationRequired = &value
			}
			// Timeouts which validate would refuse are left out (and kept
			// as they are when the manifest is applied)
			if errs := sessionSpec.validate(); len(errs) > 0 {
				exportNote(SectionLoginOptions, fmt.Sprintf("the OAI-Secure session timeouts (absoluteTimeout %s, renewalTimeout %s) are not exported: %s",
					firstNonEmpty(sessionSpec.AbsoluteTimeout, "unset"), firstNonEmpty(sessionSpec.RenewalTimeout, "unset"), strings.Join(errs, "; ")))
				sessionSpec.AbsoluteTimeout = ""
				sessionSpec.RenewalTimeout = ""
			}
			if *sessionSpec != (OAISecureSessionSpec{}) {
				spec.OAISecure = &OAISecureLoginSpec{Session: sessionSpec}
			}
		}
	}

	if sso, ok := state[LoginTypeSSO]; ok {
		if spec.SSO, err = e.ssoLoginSpec(ctx, sso, spec.Preferred); err != nil {
			return nil, err
		}
	}

	// preferred: sso only validates with an sso section which has access
	// mapping rules; otherwise the mode is left out (and kept as it is when
	// the manifest is applied)
	if spec.Preferred == LoginModeSSO && (spec.SSO == nil || spec.SSO.Access == nil || len(spec.SSO.Access.Rules) == 0) {
		exportNote(SectionLoginOptions, "preferred: sso is not exported because the sso login option has no exportable access mapping rules; add rules and set preferred: sso")
		spec.Preferred = ""
	}
	return spec, nil
}

// exportDiscoverOIDC checks an exported SSO discovery URL (replaced in tests,
// which have no provider to reach)
var exportDiscoverOIDC = func(ctx context.Context, url string) error {
	_, err := discoverOIDCProvider(ctx, url)
	return err
}

// exportNote prints a note about an exported section to stderr
func exportNote(section, message string) {
	fmt.Fprintf(os.Stderr, "⚠ %s: %s\n", section, message)
}

// ssoLoginSpec exports the SSO login option (nil when it cannot be
// expressed as a discovery URL)
func (e *Exporter) ssoLoginSpec(ctx context.Context, option map[string]any, preferred string) (*SSOLoginSpec, error) {
	str := func(value any) string { s, _ := value.(string); return s }
	issuer := strings.TrimSuffix(str(option["issuer"]), "/")
	if issuer == "" {
		exportNote(SectionLoginOptions, "the sso login option is not exported: it has no issuer to derive its discovery URL from")
		return nil, nil
	}
	spec := &SSOLoginSpec{
		DiscoveryURL:       issuer + oidcDiscoverySuffix,
		ClientID:           str(option["clientId"]),
		ClientSecretFrom:   &SecretRef{Env: exportedSSOSecretEnv},
		ProviderName:       str(option["providerName"]),
		ButtonName:         str(option["buttonName"]),
		Audience:           str(option["audience"]),
		Template:           str(option["template"]),
		RedirectToPlatform: str(option["redirectToPlatform"]),
	}
	if visible, ok := option["visibleOnLoginPage"].(bool); ok && !visible && preferred != LoginModeSSO {
		spec.VisibleOnLoginPage = &visible
	}
	if useIDToken, ok := option["useIdToken"].(bool); ok && !useIDToken {
		spec.UseIDToken = &useIDToken
	}
	if userID, ok := option["userIdConfig"].(map[string]any); ok {
		if field := str(userID["jwtField"]); field != "" && field != "sub" {
			spec.UserIDClaim = field
		}
	}
	if auth, ok := option["authorizationRequest"].(map[string]any); ok {
		params, _ := auth["requestParams"].(map[string]any)
		if scope := strings.Fields(str(params["scope"])); len(scope) > 0 && strings.Join(scope, " ") != "openid profile email" {
			spec.Scopes = scope
		}
	}
	if mapping, ok := option["accessTokenToUserDataMapping"].(map[string]any); ok {
		data := SSOUserDataSpec{
			Email:       str(mapping["emailClaimName"]),
			FirstName:   str(mapping["firstNameClaimName"]),
			LastName:    str(mapping["lastNameClaimName"]),
			PhoneNumber: str(mapping["phoneNumberClaimName"]),
		}
		if data.Email == "email" {
			data.Email = ""
		}
		if data.FirstName == "given_name" {
			data.FirstName = ""
		}
		if data.LastName == "family_name" {
			data.LastName = ""
		}
		if data != (SSOUserDataSpec{}) {
			spec.UserData = &data
		}
	}

	onNewUser, _ := option["onNewUser"].(map[string]any)
	dynamic, _ := onNewUser["dynamicMapping"].(map[string]any)
	if dynamic == nil {
		e.checkDiscovery(ctx, spec.DiscoveryURL)
		return spec, nil
	}
	config, _ := dynamic["configuration"].(map[string]any)
	access := &SSOAccessMappingSpec{}
	access.MapRolesOnlyForNewUser, _ = config["mapRolesOnlyForNewUser"].(bool)
	access.ManageRolesOnlyFromAccessMapping, _ = config["manageRolesOnlyFromAccessMapping"].(bool)
	if fromIDToken, ok := config["mapFromIdToken"].(bool); ok && fromIDToken != spec.useIDToken() {
		access.MapFromIDToken = &fromIDToken
	}
	mappings, _ := dynamic["mappings"].([]any)
	for _, raw := range mappings {
		mapping, _ := raw.(map[string]any)
		when, _ := mapping["when"].(map[string]any)
		rule := SSOAccessRuleSpec{}
		if strings.EqualFold(str(when["operator"]), "OR") {
			rule.Match = "any"
		}
		predicates, _ := when["childPredicates"].([]any)
		for _, rawPredicate := range predicates {
			predicate, _ := rawPredicate.(map[string]any)
			cond := SSOConditionSpec{
				Claim:    str(predicate["parameterPath"]),
				Operator: str(predicate["operator"]),
				Value:    str(predicate["value"]),
			}
			if cond.Operator == "EQ" {
				cond.Operator = ""
			}
			rule.When = append(rule.When, cond)
		}
		for _, id := range idList(mapping["thenGroups"]) {
			name, err := e.groupName(ctx, id)
			if err != nil {
				return nil, err
			}
			rule.Groups = append(rule.Groups, name)
		}
		for _, id := range idList(mapping["thenApplications"]) {
			name, err := e.applicationName(ctx, id)
			if err != nil {
				return nil, err
			}
			rule.Applications = append(rule.Applications, name)
		}
		access.Rules = append(access.Rules, rule)
	}
	spec.Access = access
	e.checkDiscovery(ctx, spec.DiscoveryURL)
	return spec, nil
}

// checkDiscovery warns when the discovery URL derived from the issuer does
// not work (some providers serve it elsewhere, e.g. Azure AD v1 issuers)
func (e *Exporter) checkDiscovery(ctx context.Context, discoveryURL string) {
	if err := exportDiscoverOIDC(ctx, discoveryURL); err != nil {
		exportNote(SectionLoginOptions, fmt.Sprintf("check sso.discoveryUrl, it was derived from the issuer and does not work: %v", err))
	}
}

// groupName returns the name of a user group of an access mapping. A group
// which cannot be read fails the export: its ID is no name the manifest
// could reference.
func (e *Exporter) groupName(ctx context.Context, id string) (string, error) {
	group := e.Client.UserGroups.Get(ctx, usergroups.GetOptions{ID: usergroups.ByID(id)})
	if group.Err != nil {
		return "", fmt.Errorf("sso access mapping: user group %s: %w", id, group.Err)
	}
	if group.Data.Name() == "" {
		return "", fmt.Errorf("sso access mapping: user group %s has no name", id)
	}
	return group.Data.Name(), nil
}

// applicationName returns the name of an application of an access mapping
// (an application which cannot be read fails the export)
func (e *Exporter) applicationName(ctx context.Context, id string) (string, error) {
	app := e.Client.Applications.Get(ctx, id)
	if app.Err != nil {
		return "", fmt.Errorf("sso access mapping: application %s: %w", id, app.Err)
	}
	if app.Data.Name() == "" {
		return "", fmt.Errorf("sso access mapping: application %s has no name", id)
	}
	return app.Data.Name(), nil
}

// idList returns the IDs of a list of numbers or strings
func idList(value any) []string {
	list, _ := value.([]any)
	var ids []string
	for _, item := range list {
		switch v := item.(type) {
		case float64:
			ids = append(ids, strconv.FormatInt(int64(v), 10))
		case string:
			ids = append(ids, v)
		}
	}
	return ids
}
