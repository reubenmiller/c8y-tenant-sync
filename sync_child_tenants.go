package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// SyncTenants ensures the child tenants of the current tenant exist. Tenants
// are matched by domain among the child tenants: a missing tenant is created
// with its admin credentials; an existing tenant only gets its company,
// contact and custom properties updated when they differ (the domain and
// admin credentials are never changed). Tenants are never deleted or
// suspended. The declared applications are subscribed when missing.
//
// The section runs once with the base credentials (not per target).
func (s *Syncer) SyncTenants(ctx context.Context, specs []ChildTenantSpec) error {
	if len(specs) == 0 {
		return nil
	}

	if s.DryRun {
		for _, spec := range specs {
			s.record(SectionTenants, spec.Domain, ActionPlanned, childTenantPlan(spec), nil)
		}
		return nil
	}

	parentID := s.baseTenant(ctx)
	if parentID == "" {
		return fmt.Errorf("failed to get the current tenant")
	}
	children, err := s.listChildTenants(ctx, parentID)
	if err != nil {
		return err
	}
	byDomain := make(map[string]jsonmodels.Tenant, len(children))
	byID := make(map[string]jsonmodels.Tenant, len(children))
	for _, child := range children {
		byDomain[strings.ToLower(child.Domain())] = child
		byID[strings.ToLower(child.ID())] = child
	}

	for _, spec := range specs {
		existing, found := byDomain[strings.ToLower(strings.TrimSpace(spec.Domain))]
		switch {
		case found && spec.ID != "" && !strings.EqualFold(existing.ID(), spec.ID):
			// The tenant ID can never be changed
			s.record(SectionTenants, spec.Domain, ActionFailed, "id: "+existing.ID(),
				fmt.Errorf("the tenant with this domain has the id %q, not %q (a tenant id cannot be changed)", existing.ID(), spec.ID))
		case found:
			s.updateChildTenant(ctx, spec, existing)
		default:
			if other, taken := byID[strings.ToLower(spec.ID)]; spec.ID != "" && taken {
				s.record(SectionTenants, spec.Domain, ActionFailed, "create tenant",
					fmt.Errorf("the id %q is already used by the tenant with the domain %q (a tenant domain is not changed)", spec.ID, other.Domain()))
				continue
			}
			s.createChildTenant(ctx, spec)
		}
	}
	return nil
}

// childTenantPlan describes the planned work for a dry run
func childTenantPlan(spec ChildTenantSpec) string {
	detail := "create if missing, otherwise update company, contact and custom properties when they differ"
	if spec.ID != "" {
		detail = "create with id " + spec.ID + " if missing, otherwise update company, contact and custom properties when they differ"
	}
	if len(spec.Applications) > 0 {
		detail += "; subscribe: " + strings.Join(spec.Applications, ", ")
	}
	return detail
}

// createChildTenant creates a missing tenant and subscribes its applications
func (s *Syncer) createChildTenant(ctx context.Context, spec ChildTenantSpec) {
	var missing []string
	if strings.TrimSpace(spec.Company) == "" {
		missing = append(missing, "company")
	}
	if spec.AdminPass == "" && !spec.SendPasswordResetEmail {
		missing = append(missing, "one of adminPass, adminPasswordFrom or sendPasswordResetEmail")
	}
	if len(missing) > 0 {
		s.record(SectionTenants, spec.Domain, ActionFailed, "create tenant",
			fmt.Errorf("the tenant does not exist and creating it requires %s", strings.Join(missing, " and ")))
		return
	}

	created := s.Client.Tenants.Create(ctx, childTenantCreateBody(spec))
	if created.Err != nil {
		s.record(SectionTenants, spec.Domain, ActionFailed, "create tenant", created.Err)
		return
	}
	tenantID := created.Data.ID()

	details := []string{"created: " + tenantID}
	if name, ok := s.generatedName(spec.AdminPass); ok {
		details = append(details, "admin password from ${generate:"+name+"}")
	}
	if spec.SendPasswordResetEmail {
		details = append(details, "password reset email sent to the admin")
	}

	// A new tenant gets the default subscriptions of the platform, which are
	// unknown here: subscribe all declared applications (409 = subscribed)
	subscribed, err := s.subscribeChildApplications(ctx, tenantID, spec.Applications, nil)
	if len(subscribed) > 0 {
		details = append(details, "subscribed: "+strings.Join(subscribed, ", "))
	}
	if err != nil {
		s.record(SectionTenants, spec.Domain, ActionFailed, strings.Join(details, "; "), err)
		return
	}
	s.record(SectionTenants, spec.Domain, ActionCreated, strings.Join(details, "; "), nil)
}

// updateChildTenant updates the changed fields of an existing tenant and
// subscribes the missing applications
func (s *Syncer) updateChildTenant(ctx context.Context, spec ChildTenantSpec, existing jsonmodels.Tenant) {
	tenantID := existing.ID()
	action := ActionUnchanged
	details := []string{"id: " + tenantID}

	if name, ok := s.newlyGeneratedName(spec.AdminPass); ok {
		// Admin credentials are only set when creating the tenant
		details = append(details, "generated password ${generate:"+name+"} not applied (the tenant already exists)")
	}

	if changes := childTenantDiff(spec, existing); len(changes) > 0 {
		result := s.Client.Tenants.Update(ctx, tenantID, changes)
		if result.Err != nil {
			s.record(SectionTenants, spec.Domain, ActionFailed, strings.Join(append(details, "update tenant"), "; "), result.Err)
			return
		}
		action = ActionUpdated
		details = append(details, "updated "+strings.Join(sortedKeys(changes), ", "))
	}

	if len(spec.Applications) > 0 {
		subs, known := s.tenantSubscriptions(ctx, tenantID, false)
		if !known {
			subs = nil
		}
		subscribed, err := s.subscribeChildApplications(ctx, tenantID, spec.Applications, subs)
		if len(subscribed) > 0 {
			action = ActionUpdated
			details = append(details, "subscribed: "+strings.Join(subscribed, ", "))
		}
		if err != nil {
			s.record(SectionTenants, spec.Domain, ActionFailed, strings.Join(details, "; "), err)
			return
		}
	}
	s.record(SectionTenants, spec.Domain, action, strings.Join(details, "; "), nil)
}

// subscribeChildApplications subscribes the applications which are not in the
// known subscriptions and returns the names of the applications actually
// subscribed. The first error stops the subscriptions.
func (s *Syncer) subscribeChildApplications(ctx context.Context, tenantID string, names []string, existing subscriptions) ([]string, error) {
	var subscribed []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if _, ok := existing.find(name, ""); ok {
			continue
		}
		selfLink, ok := s.appSelfLinks[name]
		if !ok {
			app := s.Client.Applications.Get(ctx, s.Client.Applications.ByName(name, ""))
			if app.Err != nil {
				return subscribed, fmt.Errorf("application %q: %w", name, app.Err)
			}
			selfLink = s.cacheAppSelfLink(name, app.Data.Self())
		}
		result := s.Client.Applications.Subscribe(ctx, tenantID, selfLink)
		if result.Err != nil {
			return subscribed, fmt.Errorf("subscribe %q: %w", name, subscriptionErrorHint(result.Err, tenantID))
		}
		// The subscriptions of the tenant changed: later sections list them again
		delete(s.subscriptionCache, tenantID)
		if result.Status != op.StatusDuplicate {
			subscribed = append(subscribed, name)
		}
	}
	return subscribed, nil
}

// childTenantCreateBody builds the create request body of a tenant
func childTenantCreateBody(spec ChildTenantSpec) map[string]any {
	body := map[string]any{
		"domain":  strings.TrimSpace(spec.Domain),
		"company": spec.Company,
	}
	if spec.ID != "" {
		body["id"] = spec.ID
	}
	for key, value := range map[string]string{
		"adminName":    spec.AdminName,
		"adminEmail":   spec.AdminEmail,
		"adminPass":    spec.AdminPass,
		"contactName":  spec.ContactName,
		"contactPhone": spec.ContactPhone,
	} {
		if value != "" {
			body[key] = value
		}
	}
	if spec.SendPasswordResetEmail {
		body["sendPasswordResetEmail"] = true
	}
	if len(spec.CustomProperties) > 0 {
		body["customProperties"] = spec.CustomProperties
	}
	return body
}

// childTenantDiff returns the fields of an existing tenant which differ from
// the manifest. Only fields set in the manifest are managed (empty fields are
// left alone). Custom properties are merged: the update carries the existing
// properties with the declared keys set, so keys not in the manifest are kept.
func childTenantDiff(spec ChildTenantSpec, existing jsonmodels.Tenant) map[string]any {
	changes := map[string]any{}
	if spec.Company != "" && existing.Get("company").String() != spec.Company {
		changes["company"] = spec.Company
	}
	if spec.ContactName != "" && existing.Get("contactName").String() != spec.ContactName {
		changes["contactName"] = spec.ContactName
	}
	if spec.ContactPhone != "" && existing.Get("contactPhone").String() != spec.ContactPhone {
		changes["contactPhone"] = spec.ContactPhone
	}

	if len(spec.CustomProperties) > 0 {
		current := map[string]json.RawMessage{}
		if raw := existing.Get("customProperties").Raw; raw != "" {
			_ = json.Unmarshal([]byte(raw), &current)
		}
		differs := false
		for key, value := range spec.CustomProperties {
			if raw, ok := current[key]; !ok || !jsonEqual(value, string(raw)) {
				differs = true
				break
			}
		}
		if differs {
			merged := make(map[string]any, len(current)+len(spec.CustomProperties))
			for key, raw := range current {
				merged[key] = raw
			}
			for key, value := range spec.CustomProperties {
				merged[key] = value
			}
			changes["customProperties"] = merged
		}
	}
	return changes
}
