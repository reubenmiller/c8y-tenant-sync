package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/authentication"
)

// tenantAdminLoginAttempts is how often the admin credentials of a tenant
// created in this run are tried (the new tenant can take a moment to accept
// logins), and tenantAdminRetryDelay the wait in between (shorter in tests)
var (
	tenantAdminLoginAttempts = 5
	tenantAdminRetryDelay    = 2 * time.Second
)

// resolveTenantAdminCredentials logs in to every target with the admin
// credentials of its entry in the tenants section: adminName and the admin
// password (adminPass, an env reference or a generated value stored in the
// secrets file). This works without a placeholder microservice or session
// files, e.g. from a CI runner, as long as the secrets file is kept between
// runs.
func (s *Syncer) resolveTenantAdminCredentials(ctx context.Context, targets []Target) error {
	var errs []string
	for i := range targets {
		target := &targets[i]
		if target.Current || target.Auth != nil {
			continue
		}
		auth, created, err := s.tenantAdminAuth(*target)
		if err == nil {
			err = s.checkTenantAdminLogin(ctx, auth, created)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", target.Label(), err))
			continue
		}
		target.Auth = &auth
	}
	if len(errs) > 0 {
		return fmt.Errorf("tenantAdmin credentials:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// tenantAdminAuth returns the admin credentials of the tenants entry which
// matches the target (by ID, then by domain), and whether the tenant was
// created in this run
func (s *Syncer) tenantAdminAuth(target Target) (authentication.AuthOptions, bool, error) {
	spec, createdID, ok := s.tenantSpecFor(target)
	if !ok {
		return authentication.AuthOptions{}, false, errors.New("no entry in the tenants section matches this tenant (by id or domain), so there are no admin credentials to use")
	}
	created := createdID != ""

	switch {
	case spec.SendPasswordResetEmail:
		return authentication.AuthOptions{}, false, errors.New("the admin password is set by email (sendPasswordResetEmail), so it is not known")
	case strings.TrimSpace(spec.AdminName) == "":
		return authentication.AuthOptions{}, false, errors.New("the tenants entry has no adminName")
	case spec.AdminPass == "":
		return authentication.AuthOptions{}, false, errors.New("the tenants entry has no admin password (adminPass or adminPasswordFrom)")
	}
	if name, generated := s.newlyGeneratedName(spec.AdminPass); generated && !created {
		// The tenant existed before this run, so the value generated now
		// was never set as its password (e.g. the secrets file was lost)
		return authentication.AuthOptions{}, false, fmt.Errorf("the admin password ${generate:%s} was generated in this run but the tenant already existed, so it is not the tenant's password (restore the secrets file in which it was stored)", name)
	}

	return authentication.AuthOptions{
		Tenant:   target.TenantID,
		Username: spec.AdminName,
		Password: spec.AdminPass,
	}, created, nil
}

// tenantSpecFor finds the tenants entry of a target, and the ID of the
// tenant if it was created in this run
func (s *Syncer) tenantSpecFor(target Target) (ChildTenantSpec, string, bool) {
	for _, spec := range s.tenantSpecs {
		domain := strings.ToLower(strings.TrimSpace(spec.Domain))
		createdID := s.createdTenants[domain]
		switch {
		case spec.ID != "" && strings.EqualFold(spec.ID, target.TenantID),
			createdID != "" && createdID == target.TenantID,
			target.Domain != "" && strings.EqualFold(domain, target.Domain):
			return spec, createdID, true
		}
	}
	return ChildTenantSpec{}, "", false
}

// checkTenantAdminLogin verifies the credentials before any section uses
// them, so a changed password fails once with a clear message instead of in
// every section. Logins to a tenant created in this run are retried, since a
// new tenant can take a moment to accept them.
func (s *Syncer) checkTenantAdminLogin(ctx context.Context, auth authentication.AuthOptions, created bool) error {
	attempts := 1
	if created {
		attempts = tenantAdminLoginAttempts
	}
	var err error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(tenantAdminRetryDelay):
			}
		}
		err = s.Client.Users.CurrentUser.Get(api.WithAuth(ctx, auth)).Err
		if err == nil {
			return nil
		}
		if !isUnauthorized(err) {
			return fmt.Errorf("failed to log in as %s: %w", auth.Username, err)
		}
	}
	return fmt.Errorf("the admin credentials (%s) were rejected: the password may have been changed, or two-factor authentication is enabled for the admin (use the serviceUser or sessions credentials mode for such tenants)", auth.Username)
}

func isUnauthorized(err error) bool {
	return api.ErrHasStatus(err, http.StatusUnauthorized)
}
