package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/core"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/currenttenant"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/tidwall/gjson"
)

// subscribedApplication is an application a tenant is subscribed to
type subscribedApplication struct {
	Name        string
	Type        string
	OwnerTenant string
}

// subscriptions are the applications a tenant is subscribed to
type subscriptions []subscribedApplication

// find returns the subscription of the application, matching the name and,
// when given, the application type
func (s subscriptions) find(name, appType string) (subscribedApplication, bool) {
	for _, app := range s {
		if app.Name == name && (appType == "" || strings.EqualFold(app.Type, appType)) {
			return app, true
		}
	}
	return subscribedApplication{}, false
}

// findOwned returns the subscription of the application owned by the owner
// tenant, matching the name and, when given, the application type
func (s subscriptions) findOwned(name, appType, owner string) (subscribedApplication, bool) {
	for _, app := range s {
		if app.Name == name && app.OwnerTenant == owner && (appType == "" || strings.EqualFold(app.Type, appType)) {
			return app, true
		}
	}
	return subscribedApplication{}, false
}

func subscribedFromApplication(app jsonmodels.Application) subscribedApplication {
	return subscribedApplication{
		Name:        app.Name(),
		Type:        app.Type(),
		OwnerTenant: app.Get("owner.tenant.id").String(),
	}
}

// tenantSubscriptions lists the applications the target tenant is subscribed
// to. The current tenant uses the current tenant API (the same as
// "c8y currenttenant listApplications"); other tenants are read from the
// parent tenant. The list is fetched once per tenant and run. When it can't be
// read, ok is false and the subscription state is treated as unknown.
func (s *Syncer) tenantSubscriptions(ctx context.Context, tenantID string, current bool) (subscriptions, bool) {
	if cached, ok := s.subscriptionCache[tenantID]; ok {
		return cached, true
	}

	var subs subscriptions
	if current {
		// The same request as "c8y currenttenant listApplications"
		result := s.Client.Tenants.Current.ListApplications(ctx)
		for app, err := range result.Items() {
			if err != nil {
				slog.Debug("Could not list the subscribed applications of the current tenant", "err", err)
				return nil, false
			}
			subs = append(subs, subscribedFromApplication(app))
		}
	} else {
		result := s.Client.Tenants.ListApplicationReferences(ctx, tenantID, tenants.ListApplicationReferencesOptions{
			PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
		})
		for ref, err := range result.Items() {
			if err != nil {
				slog.Debug("Could not list the subscribed applications", "tenant", tenantID, "err", err)
				return nil, false
			}
			subs = append(subs, subscribedFromApplication(ref.Application()))
		}
	}

	s.cacheSubscriptions(tenantID, subs)
	return subs, true
}

func (s *Syncer) cacheSubscriptions(tenantID string, subs subscriptions) {
	if s.subscriptionCache == nil {
		s.subscriptionCache = map[string]subscriptions{}
	}
	s.subscriptionCache[tenantID] = subs
}

// subscriptionsFromCurrentTenant reads the subscriptions from the
// applications.references of a current tenant response
func subscriptionsFromCurrentTenant(references []gjson.Result) subscriptions {
	subs := subscriptions{}
	for _, ref := range references {
		subs = append(subs, subscribedFromApplication(jsonmodels.NewApplication([]byte(ref.Get("application").Raw))))
	}
	return subs
}

// baseTenant returns the tenant of the credentials running the sync
func (s *Syncer) baseTenant(ctx context.Context) string {
	if s.baseTenantID == "" {
		if tenant := s.Client.Tenants.Current.Get(ctx, currenttenant.GetOptions{}); tenant.Err == nil {
			s.baseTenantID = tenant.Data.Name()
		}
	}
	return s.baseTenantID
}

// foreignOwnerSkip reports whether a failed (un)subscription is skipped: the
// request was denied (403) and the application is owned by a tenant which is
// neither the target tenant nor the tenant running the sync. Such
// subscriptions can only be changed by the owner tenant (e.g. "management"
// for the built-in microservices).
func (s *Syncer) foreignOwnerSkip(ctx context.Context, err error, owner, tenantID, action string) (string, bool) {
	if !core.ErrHasStatus(err, http.StatusForbidden) || owner == "" || owner == tenantID {
		return "", false
	}
	if base := s.baseTenant(ctx); base == "" || owner == base {
		return "", false
	}
	return fmt.Sprintf("%s denied: the application is owned by tenant %s; %s it from tenant %s (e.g. with a targets section selecting %s)",
		action, owner, action, owner, tenantID), true
}
