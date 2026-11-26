package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// SecretRef takes a secret value from an environment variable or from a
// generated value, instead of writing it into the manifest. Exactly one
// field must be set. Because the reference is a separate field, sops rules
// such as encrypted_regex: ^(value|password|token)$ only encrypt literal
// secrets, and references stay readable.
type SecretRef struct {
	// Env is the name of an environment variable holding the value
	Env string `yaml:"env" json:"env,omitempty" jsonschema:"description=Name of an environment variable which holds the value (it must be set and not empty),example=MY_PASSWORD"`

	// Generate is the name of a generated value (NAME or NAME:LENGTH), the
	// same as ${generate:NAME}
	Generate string `yaml:"generate" json:"generate,omitempty" jsonschema:"description=Name of a generated value (NAME or NAME:LENGTH) stored in the secrets file; the same as ${generate:NAME},example=svc-password"`
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validate checks that exactly one valid reference is set
func (r SecretRef) validate(label string) []string {
	var errs []string
	switch {
	case r.Env == "" && r.Generate == "":
		errs = append(errs, label+" requires one of: env, generate")
	case r.Env != "" && r.Generate != "":
		errs = append(errs, label+": env and generate are mutually exclusive")
	case r.Env != "" && !envNamePattern.MatchString(r.Env):
		errs = append(errs, fmt.Sprintf("%s.env: invalid environment variable name %q", label, r.Env))
	case r.Generate != "":
		if _, err := parseGenerateRef(r.Generate); err != nil {
			errs = append(errs, fmt.Sprintf("%s.generate: %v", label, err))
		}
	}
	return errs
}

// resolve returns the referenced value
func (r SecretRef) resolve(secrets *SecretStore) (string, error) {
	if r.Env != "" {
		value, ok := os.LookupEnv(r.Env)
		if !ok || value == "" {
			return "", fmt.Errorf("environment variable %s is not set or empty", r.Env)
		}
		return value, nil
	}
	ref, err := parseGenerateRef(r.Generate)
	if err != nil {
		return "", err
	}
	if secrets == nil {
		return "", fmt.Errorf("generated value %s requires a secrets file", ref.Name)
	}
	return secrets.Value(ref)
}

// allSources returns the sources of all manifest entries
func (m *Manifest) allSources() []*Source {
	var sources []*Source
	for i := range m.Software {
		sources = append(sources, &m.Software[i].Source)
	}
	for i := range m.Firmware {
		sources = append(sources, &m.Firmware[i].Source)
	}
	for i := range m.Configuration {
		sources = append(sources, &m.Configuration[i].Source)
	}
	for i := range m.TrustedCertificates {
		sources = append(sources, &m.TrustedCertificates[i].Source)
	}
	for i := range m.CertificateRevocationLists {
		sources = append(sources, &m.CertificateRevocationLists[i].Source)
	}
	for i := range m.SmartRestTemplates {
		sources = append(sources, &m.SmartRestTemplates[i].Source)
	}
	for i := range m.Microservices {
		if m.Microservices[i].Source != nil {
			sources = append(sources, m.Microservices[i].Source)
		}
	}
	for i := range m.Extensions {
		sources = append(sources, &m.Extensions[i].Source)
	}
	for i := range m.Applications {
		if m.Applications[i].Source != nil {
			sources = append(sources, m.Applications[i].Source)
		}
	}
	return sources
}

// ssoLogin returns the SSO login option of the manifest (nil when none)
func (m *Manifest) ssoLogin() *SSOLoginSpec {
	if m.LoginOptions == nil {
		return nil
	}
	return m.LoginOptions.SSO
}

// ResolveSecretRefs replaces the env and generate references (valueFrom,
// passwordFrom, adminPasswordFrom, tokenFrom, clientSecretFrom) with their values. It returns
// the resolved values, so they can be masked in the output. The application
// and device references of valueFrom are resolved when the tenant options are
// applied.
func (m *Manifest) ResolveSecretRefs(secrets *SecretStore) ([]string, error) {
	var values, errs []string
	resolve := func(label string, ref SecretRef) (string, bool) {
		value, err := ref.resolve(secrets)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			return "", false
		}
		values = append(values, value)
		return value, true
	}

	for i := range m.TenantOptions {
		from := m.TenantOptions[i].ValueFrom
		if from == nil || (from.Env == "" && from.Generate == "") {
			continue
		}
		if value, ok := resolve(fmt.Sprintf("tenantOptions[%d].valueFrom", i), SecretRef{Env: from.Env, Generate: from.Generate}); ok {
			m.TenantOptions[i].Value = value
			m.TenantOptions[i].ValueFrom = nil
		}
	}
	for i := range m.Users {
		// An absent user is deleted, so its password is never needed
		if m.Users[i].PasswordFrom == nil || m.Users[i].Absent() {
			continue
		}
		if value, ok := resolve(fmt.Sprintf("users[%d].passwordFrom", i), *m.Users[i].PasswordFrom); ok {
			m.Users[i].Password = value
			m.Users[i].PasswordFrom = nil
		}
	}
	for i := range m.Tenants {
		if m.Tenants[i].AdminPasswordFrom == nil {
			continue
		}
		if value, ok := resolve(fmt.Sprintf("tenants[%d].adminPasswordFrom", i), *m.Tenants[i].AdminPasswordFrom); ok {
			m.Tenants[i].AdminPass = value
			m.Tenants[i].AdminPasswordFrom = nil
		}
	}
	if sso := m.ssoLogin(); sso != nil && sso.ClientSecretFrom != nil {
		if value, ok := resolve("loginOptions.sso.clientSecretFrom", *sso.ClientSecretFrom); ok {
			sso.ClientSecret = value
			sso.ClientSecretFrom = nil
			values = append(values, ssoSecretForms(sso)...)
		}
	}
	for _, source := range m.allSources() {
		if source.GitHub == nil || source.GitHub.TokenFrom == nil {
			continue
		}
		if value, ok := resolve("github.tokenFrom ("+source.GitHub.Repo+")", *source.GitHub.TokenFrom); ok {
			source.GitHub.Token = value
			source.GitHub.TokenFrom = nil
		}
	}

	if len(errs) > 0 {
		return values, fmt.Errorf("failed to resolve secret references:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return values, nil
}
