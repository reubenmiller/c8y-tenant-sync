package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/applications"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/core"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/inventory/managedobjects"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/currenttenant"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/tenantoptions"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// SyncTenantOptions ensures each tenant option exists with the desired value.
// Values given as valueFrom references are resolved against the tenant first.
func (s *Syncer) SyncTenantOptions(ctx context.Context, specs []TenantOptionSpec) error {
	for _, spec := range specs {
		item := fmt.Sprintf("%s/%s", spec.Category, spec.Key)

		if s.DryRun {
			detail := "set value"
			if spec.ValueFrom != nil {
				detail = "set value (resolved by lookup at apply time)"
			}
			s.record(SectionTenantOptions, item, ActionPlanned, detail, nil)
			continue
		}

		value := spec.Value
		if spec.ValueFrom != nil {
			resolved, err := s.resolveOptionValue(ctx, spec.ValueFrom)
			if err != nil {
				s.record(SectionTenantOptions, item, ActionFailed, "resolve valueFrom", err)
				continue
			}
			value = resolved
		}

		existing := s.Client.Tenants.Options.Get(ctx, tenantoptions.GetOption{
			Category: spec.Category,
			Key:      spec.Key,
		})

		switch {
		case existing.Err == nil && existing.Data.Value() == value:
			s.record(SectionTenantOptions, item, ActionUnchanged, "", nil)
		case existing.Err == nil:
			result := s.Client.Tenants.Options.Update(ctx, tenantoptions.UpdateOption{
				Category: spec.Category,
				Key:      spec.Key,
				Body:     map[string]any{"value": value},
			})
			s.record(SectionTenantOptions, item, ActionUpdated, "", result.Err)
		default:
			// Not found (or not readable, e.g. password options): create it
			result := s.Client.Tenants.Options.Create(ctx, map[string]any{
				"category": spec.Category,
				"key":      spec.Key,
				"value":    value,
			})
			s.record(SectionTenantOptions, item, ActionCreated, "", result.Err)
		}
	}
	return nil
}

// resolveOptionValue resolves a valueFrom reference to its value
func (s *Syncer) resolveOptionValue(ctx context.Context, ref *TenantOptionValueFrom) (string, error) {
	switch {
	case ref.Application != "":
		id, err := s.Client.Applications.ResolveID(ctx, s.Client.Applications.ByName(ref.Application, ""), nil)
		if err != nil {
			return "", fmt.Errorf("application %q: %w", ref.Application, err)
		}
		return id, nil
	case ref.Device != "":
		query := fmt.Sprintf("has(c8y_IsDevice) and name eq '%s'", escapeQueryValue(ref.Device))
		result := s.Client.ManagedObjects.List(ctx, managedobjects.ListOptions{
			Query: query,
			PaginationOptions: pagination.PaginationOptions{
				PageSize: 1,
			},
		})
		if result.Err != nil {
			return "", fmt.Errorf("device %q: %w", ref.Device, result.Err)
		}
		for item := range result.Data.Iter() {
			return jsonmodels.NewManagedObject(item.Bytes()).ID(), nil
		}
		return "", fmt.Errorf("device %q: not found", ref.Device)
	default:
		return "", fmt.Errorf("valueFrom reference is empty")
	}
}

// SyncFeatures enables/disables feature toggles for the target tenant. The
// current tenant is managed via the regular features API; other tenants via
// the per-tenant overrides API (which requires management tenant credentials,
// so the call is made with the base credentials rather than the target's).
func (s *Syncer) SyncFeatures(ctx context.Context, specs []FeatureSpec) error {
	for _, spec := range specs {
		desired := "enabled"
		if !spec.IsEnabled() {
			desired = "disabled"
		}

		if s.DryRun {
			s.record(SectionFeatures, spec.Key, ActionPlanned, desired, nil)
			continue
		}

		if s.activeTarget == nil || s.activeTarget.Current {
			existing := s.Client.Features.Get(ctx, spec.Key)
			if existing.Err == nil && existing.Data.Active() == spec.IsEnabled() {
				s.record(SectionFeatures, spec.Key, ActionUnchanged, desired, nil)
				continue
			}

			var err error
			if spec.IsEnabled() {
				err = s.Client.Features.Enable(ctx, spec.Key).Err
			} else {
				err = s.Client.Features.Disable(ctx, spec.Key).Err
			}
			s.record(SectionFeatures, spec.Key, ActionUpdated, desired, err)
			continue
		}

		tenantID := s.activeTarget.TenantID
		if active, known := s.featureOverride(ctx, spec.Key, tenantID); known && active == spec.IsEnabled() {
			s.record(SectionFeatures, spec.Key, ActionUnchanged, desired, nil)
			continue
		}
		result := s.Client.Features.Tenants.SetForTenant(ctx, spec.Key, tenantID, spec.IsEnabled())
		s.record(SectionFeatures, spec.Key, ActionUpdated, desired, result.Err)
	}
	return nil
}

// featureOverride looks up the per-tenant override state of a feature toggle.
// The override list is fetched once per key and cached for the whole run; an
// unknown state (no override set, or the list not being readable) simply
// means the toggle is written without change detection.
func (s *Syncer) featureOverride(ctx context.Context, key, tenantID string) (active bool, known bool) {
	if s.featureOverrides == nil {
		s.featureOverrides = make(map[string]map[string]bool)
	}
	states, ok := s.featureOverrides[key]
	if !ok {
		states = make(map[string]bool)
		result := s.Client.Features.Tenants.List(ctx, key)
		if result.Err == nil {
			for item, err := range result.Items() {
				if err != nil {
					break
				}
				states[item.TenantId()] = item.Active()
			}
		}
		s.featureOverrides[key] = states
	}
	active, known = states[tenantID]
	return active, known
}

// appEntry is a microservice or web application entry of the manifest, in the
// shape shared by the subscription logic
type appEntry struct {
	section    string
	name       string
	appType    string // lookup filter: MICROSERVICE, HOSTED or "" (any)
	subscribed bool
	key        string
	context    string
	source     *Source

	// hosted is the web application entry when it has a source: it is kept
	// as a tenant-owned copy at the version of the source
	hosted *ApplicationSpec
}

func microserviceEntry(spec MicroserviceSpec) appEntry {
	return appEntry{
		section:    SectionMicroservices,
		name:       spec.Name,
		appType:    "MICROSERVICE",
		subscribed: spec.IsSubscribed(),
		key:        spec.Key,
		context:    spec.ContextPath,
		source:     spec.Source,
	}
}

func applicationEntry(spec ApplicationSpec) appEntry {
	entry := appEntry{
		section:    SectionApplications,
		name:       spec.Name,
		subscribed: spec.IsSubscribed(),
		key:        spec.Key,
		context:    spec.ContextPath,
		source:     spec.Source,
	}
	if spec.Source != nil {
		entry.appType = "HOSTED"
		hosted := spec
		entry.hosted = &hosted
	}
	return entry
}

// SyncMicroservices ensures microservices exist (creating and uploading their
// binary from a source when one is given) and subscribes them to the target
// tenant, or unsubscribes them when subscribed: false
func (s *Syncer) SyncMicroservices(ctx context.Context, specs []MicroserviceSpec) error {
	entries := make([]appEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, microserviceEntry(spec))
	}
	return s.syncSubscriptions(ctx, entries)
}

// SyncApplications keeps web applications with a source at the version of
// the source, subscribes the applications to the target tenant (or
// unsubscribes them when subscribed: false) and installs their extensions
func (s *Syncer) SyncApplications(ctx context.Context, specs []ApplicationSpec) error {
	entries := make([]appEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, applicationEntry(spec))
	}
	if err := s.syncSubscriptions(ctx, entries); err != nil {
		return err
	}
	for _, spec := range specs {
		if len(spec.Extensions) > 0 {
			s.syncApplicationExtensions(ctx, spec)
		}
	}
	return nil
}

// syncSubscriptions ensures the applications exist (creating and uploading
// their binary from a source when one is given) and subscribes them to the
// target tenant, or unsubscribes them when subscribed: false. Subscriptions
// address the target tenant by ID, so the calls run with the base
// credentials.
func (s *Syncer) syncSubscriptions(ctx context.Context, entries []appEntry) error {
	if len(entries) == 0 {
		return nil
	}

	if s.DryRun {
		for _, entry := range entries {
			detail := "subscribe"
			if !entry.subscribed {
				detail = "unsubscribe"
			} else if entry.hosted != nil {
				detail = "create tenant-owned copy if missing, upload the source when its version differs, then subscribe"
			} else if entry.source != nil {
				detail = "create/upload if missing, then subscribe"
			}
			s.record(entry.section, entry.name, ActionPlanned, detail, nil)
		}
		return nil
	}

	tenantID := ""
	current := s.activeTarget == nil || s.activeTarget.Current
	if s.activeTarget != nil {
		tenantID = s.activeTarget.TenantID
	}
	if tenantID == "" && current && s.baseTenantID != "" {
		// Already read by an earlier section (microservices before
		// applications) together with its subscriptions
		if _, ok := s.subscriptionCache[s.baseTenantID]; ok {
			tenantID = s.baseTenantID
		}
	}
	if tenantID == "" {
		// No targets configured: the target is the current tenant
		tenant := s.Client.Tenants.Current.Get(ctx, currenttenant.GetOptions{})
		if tenant.Err != nil {
			return fmt.Errorf("failed to get current tenant: %w", tenant.Err)
		}
		tenantID = tenant.Data.Name()
		if s.baseTenantID == "" && current {
			s.baseTenantID = tenantID
		}
		if current {
			// The current tenant response also lists its subscriptions
			s.cacheSubscriptions(tenantID, subscriptionsFromCurrentTenant(tenant.Data.Get("applications.references").Array()))
		}
		if s.activeTarget != nil {
			s.activeTarget.TenantID = tenantID
		}
	}

	// The current subscriptions, so subscribed applications are not
	// subscribed again (which the owner tenant may deny)
	subs, known := s.tenantSubscriptions(ctx, tenantID, current)

	for _, entry := range entries {
		subscribed, isSubscribed := subs.find(entry.name, entry.appType)
		if !entry.subscribed {
			if known && !isSubscribed {
				s.record(entry.section, entry.name, ActionUnchanged, "not subscribed", nil)
				continue
			}
			s.unsubscribeApplication(ctx, entry, tenantID, subscribed.OwnerTenant)
			continue
		}

		// Applications without a source need no further work when they are
		// subscribed. With a source, the binary is still created/uploaded
		if known && isSubscribed && entry.source == nil {
			s.record(entry.section, entry.name, ActionUnchanged, "already subscribed", nil)
			continue
		}

		selfLink, ok := s.ensureApplication(ctx, entry)
		if !ok {
			continue
		}
		if entry.hosted != nil {
			// Only a subscription to the tenant-owned copy counts, not one to
			// a built-in application of the same name
			isSubscribed = isSubscribed && subscribed.OwnerTenant == s.appOwners[entry.name]
			if !isSubscribed {
				_, isSubscribed = subs.findOwned(entry.name, entry.appType, s.appOwners[entry.name])
			}
		}
		if known && isSubscribed {
			s.record(entry.section, entry.name, ActionUnchanged, "already subscribed", nil)
			continue
		}

		result := s.Client.Applications.Subscribe(ctx, tenantID, selfLink)
		if result.Err != nil {
			if detail, skip := s.foreignOwnerSkip(ctx, result.Err, s.appOwners[entry.name], tenantID, "subscribe"); skip {
				s.record(entry.section, entry.name, ActionSkipped, detail, nil)
				continue
			}
			s.record(entry.section, entry.name, ActionFailed, "subscribe", subscriptionErrorHint(result.Err, tenantID))
			continue
		}

		// A 409 conflict means the tenant is already subscribed
		if result.Status == op.StatusDuplicate || result.HTTPStatus == http.StatusConflict {
			s.record(entry.section, entry.name, ActionUnchanged, "already subscribed", nil)
		} else {
			s.record(entry.section, entry.name, ActionUpdated, "subscribed", nil)
		}
	}
	return nil
}

// unsubscribeApplication removes the application subscription from the target
// tenant. A missing application or subscription both count as the desired
// state being reached.
func (s *Syncer) unsubscribeApplication(ctx context.Context, entry appEntry, tenantID, owner string) {
	result := s.Client.Applications.Unsubscribe(ctx, tenantID, s.Client.Applications.ByName(entry.name, entry.appType))
	if result.Err != nil {
		if detail, skip := s.foreignOwnerSkip(ctx, result.Err, owner, tenantID, "unsubscribe"); skip {
			s.record(entry.section, entry.name, ActionSkipped, detail, nil)
			return
		}
		s.record(entry.section, entry.name, ActionFailed, "unsubscribe", subscriptionErrorHint(result.Err, tenantID))
		return
	}
	if result.Status == op.StatusSkipped {
		// Application not found, or not subscribed (404)
		s.record(entry.section, entry.name, ActionUnchanged, "not subscribed", nil)
		return
	}
	s.record(entry.section, entry.name, ActionUpdated, "unsubscribed", nil)
}

// subscriptionErrorHint augments 403 errors from (un)subscribe calls, which
// typically mean the application is not owned by a tenant the current
// credentials can manage
func subscriptionErrorHint(err error, tenantID string) error {
	if core.ErrHasStatus(err, http.StatusForbidden) {
		return fmt.Errorf("%w (access denied: the application is likely not owned by a tenant you can manage; run with the credentials of the application's owner tenant, e.g. from the parent tenant with a targets section selecting %s)", err, tenantID)
	}
	return err
}

// ensureApplication looks up the application and, when the spec has a source,
// creates it if missing and uploads its binary. The binary is uploaded on
// creation and, for existing applications, only with --force (binary content
// cannot be compared, so unconditional uploads would break idempotency).
// Returns the application self link and whether to continue with subscription.
//
// Successful results are cached by name so that the create/upload work (and
// its result records) happen only once when applying to multiple tenants.
func (s *Syncer) ensureApplication(ctx context.Context, entry appEntry) (string, bool) {
	if entry.hosted != nil {
		return s.ensureHostedApplication(ctx, *entry.hosted)
	}
	if selfLink, ok := s.appSelfLinks[entry.name]; ok {
		return selfLink, true
	}

	app := s.Client.Applications.Get(ctx, s.Client.Applications.ByName(entry.name, entry.appType))
	if app.Err == nil {
		if s.appOwners == nil {
			s.appOwners = map[string]string{}
		}
		s.appOwners[entry.name] = app.Data.Get("owner.tenant.id").String()
	}

	if app.Err == nil && entry.source == nil {
		return s.cacheAppSelfLink(entry.name, app.Data.Self()), true
	}

	if app.Err != nil && entry.source == nil {
		s.record(entry.section, entry.name, ActionFailed, "lookup", app.Err)
		return "", false
	}

	// A source is given: the application binary (a single zip) is available
	created := false
	if app.Err != nil {
		// Application does not exist: create it
		appType := entry.appType
		if appType == "" {
			appType = "MICROSERVICE"
		}
		contextPath := entry.context
		if contextPath == "" {
			contextPath = entry.name
		}

		key := entry.key
		if key == "" {
			key = entry.name + "-application-key"
		}
		createResult := s.Client.Applications.Create(ctx, map[string]any{
			"name":        entry.name,
			"key":         key,
			"type":        appType,
			"contextPath": contextPath,
		})
		if createResult.Err != nil {
			s.record(entry.section, entry.name, ActionFailed, "create application", createResult.Err)
			return "", false
		}
		app = createResult
		created = true
	}

	if !created && !s.Force {
		s.record(entry.section, entry.name, ActionUnchanged,
			"binary upload skipped (application exists; use --force to re-upload)", nil)
		return s.cacheAppSelfLink(entry.name, app.Data.Self()), true
	}

	files, ok := s.resolveSource(entry.section, entry.name, *entry.source)
	if !ok {
		return "", false
	}
	if len(files) != 1 {
		s.record(entry.section, entry.name, ActionFailed, "",
			fmt.Errorf("application source must resolve to exactly one file, got %d", len(files)))
		return "", false
	}
	if files[0].Path == "" {
		s.record(entry.section, entry.name, ActionFailed, "",
			fmt.Errorf("application source must provide a local file (url/linkOnly sources are not supported for application binaries)"))
		return "", false
	}

	upload := s.Client.Applications.Upload(ctx, app.Data.ID(), applications.UploadFileOptions{
		FilePath:    files[0].Path,
		Name:        files[0].Filename,
		ContentType: "application/zip",
	})
	if upload.Err != nil {
		s.record(entry.section, entry.name, ActionFailed, "upload binary", upload.Err)
		return "", false
	}

	if created {
		s.record(entry.section, entry.name, ActionCreated, "created and uploaded "+files[0].Filename, nil)
	} else {
		s.record(entry.section, entry.name, ActionUpdated, "re-uploaded "+files[0].Filename, nil)
	}
	return s.cacheAppSelfLink(entry.name, app.Data.Self()), true
}

// cacheAppSelfLink stores an application self link for reuse by later targets
func (s *Syncer) cacheAppSelfLink(name, selfLink string) string {
	if s.appSelfLinks == nil {
		s.appSelfLinks = make(map[string]string)
	}
	s.appSelfLinks[name] = selfLink
	return selfLink
}
