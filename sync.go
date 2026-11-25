package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// Action describes the outcome of syncing a single item
type Action string

const (
	ActionCreated   Action = "created"
	ActionUpdated   Action = "updated"
	ActionUnchanged Action = "unchanged"
	ActionSkipped   Action = "skipped"  // nothing to do here, e.g. an optional source without files or a subscription only the owner tenant can change
	ActionExecuted  Action = "executed" // hook commands
	ActionPlanned   Action = "planned"  // dry-run
	ActionDeleted   Action = "deleted"  // only where the manifest opts in (state: absent, digitalTwin.prune)
	ActionFailed    Action = "failed"
)

// SyncToolFragment marks managed objects created by this tool so they can be
// queried, e.g. c8y inventory list --query "has(c8y_TenantSync)"
const SyncToolFragment = "c8y_TenantSync"

// Section names, used by --only filtering and in the apply order
const (
	SectionTenants                = "tenants"
	SectionTenantOptions          = "tenantOptions"
	SectionFeatures               = "features"
	SectionRetentionRules         = "retentionRules"
	SectionCertificateAuthority   = "certificateAuthority"
	SectionTrustedCertificates    = "trustedCertificates"
	SectionCertificateRevocations = "certificateRevocationLists"
	SectionMicroservices          = "microservices"
	SectionExtensions             = "extensions"
	SectionApplications           = "applications"
	SectionDeviceGroups           = "deviceGroups"
	SectionGlobalRoles            = "globalRoles"
	SectionInventoryRoles         = "inventoryRoles"
	SectionUsers                  = "users"
	SectionLoginOptions           = "loginOptions"
	SectionSoftware               = "software"
	SectionFirmware               = "firmware"
	SectionConfiguration          = "configuration"
	SectionSmartRest              = "smartrestTemplates"
	SectionDigitalTwin            = "digitalTwin"
	SectionSmartFunctions         = "smartFunctions"
	SectionDeviceProfiles         = "deviceProfiles"
	SectionCommands               = "commands"
)

// SectionOrder is the order sections are applied in. Global roles are synced
// before users so users can be given global roles created in the same run;
// inventory roles are synced after global roles and before users so the
// users' inventory roles can reference roles created in the same run;
// login options follow the users, so the global roles of SSO access mappings
// exist and the login changes come after the users who log in;
// repositories are synced before device profiles so profiles can reference
// the synced items; custom commands run last so they can build on everything
// the manifest declares. Device groups are synced before global roles and
// users so roles and permissions can later be assigned on groups created in
// the same run. UI extensions are uploaded before the web applications so
// the applications can install extensions uploaded in the same run.
//
// Child tenants come first: the section runs once with the base credentials,
// before the targets are resolved (see Syncer.Apply), so tenants created in
// the run can be selected by the targets section and configured by all other
// sections in the same run.
var SectionOrder = []string{
	SectionTenants,
	SectionTenantOptions,
	SectionFeatures,
	SectionRetentionRules,
	SectionCertificateAuthority,
	SectionTrustedCertificates,
	SectionCertificateRevocations,
	SectionMicroservices,
	SectionExtensions,
	SectionApplications,
	SectionDeviceGroups,
	SectionGlobalRoles,
	SectionInventoryRoles,
	SectionUsers,
	SectionLoginOptions,
	SectionSoftware,
	SectionFirmware,
	SectionConfiguration,
	SectionSmartRest,
	SectionDigitalTwin,
	SectionSmartFunctions,
	SectionDeviceProfiles,
	SectionCommands,
}

// ChangeResult records what happened to a single item
type ChangeResult struct {
	// Tenant is the label of the target tenant. It is only set when the run
	// covers tenants other than the current one.
	Tenant  string
	Section string
	Item    string
	Action  Action
	Detail  string
	Err     error
}

// DisplayItem returns the item prefixed with the tenant label (when set), as
// shown in the console output
func (r ChangeResult) DisplayItem() string {
	if r.Tenant != "" {
		return r.Tenant + ": " + r.Item
	}
	return r.Item
}

// Syncer applies a manifest to one or more tenants
type Syncer struct {
	Client      *api.Client
	Resolver    *SourceResolver
	DryRun      bool
	Force       bool
	Concurrency int
	// Secrets holds the generated values (${generate:NAME}), if any
	Secrets *SecretStore

	// DryRunReader is a client for read-only lookups in dry-run mode (nil
	// without a session). The loginOptions, inventoryRoles and users
	// sections use it to compare the manifest with the tenant; it never
	// writes.
	DryRunReader *api.Client

	// Targets overrides the manifest targets section (set from CLI flags)
	Targets *TargetsSpec

	Results []ChangeResult

	// activeTarget is the tenant currently being applied
	activeTarget *Target
	// dryRunPending holds the objects a dry run would create in the active
	// target, so later sections can refer to them (reset per target)
	dryRunPending *dryRunPending
	// roleDeletes are the inventory roles with state absent which the
	// inventoryRoles section found; they are deleted after the users section
	// (reset per target)
	roleDeletes []roleDelete
	// globalRoleDeletes are the global roles with state absent which the
	// globalRoles section found; they are deleted after the inventory roles
	// (reset per target)
	globalRoleDeletes []globalRoleDelete
	// deviceGroupDeletes are the device groups with state absent which the
	// deviceGroups section found; they are deleted last (reset per target)
	deviceGroupDeletes []deviceGroupDelete
	// multiTarget is set when the run covers tenants other than the current
	// one, enabling per-tenant labels in the output
	multiTarget bool
	// appSelfLinks caches application self links by name so create/upload
	// only happens once when applying to multiple tenants
	appSelfLinks map[string]string
	// appOwners caches the owner tenant of applications by name
	appOwners map[string]string
	// extensionsDone and appExtensionsDone hold the extensions uploaded and
	// the applications whose extensions were installed, which happens once
	// per run in the tenant running the sync (not once per target)
	extensionsDone    map[string]bool
	appExtensionsDone map[string]bool
	// extensionCache is the catalog of the extensions the tenant can install
	// (nil: not read yet, or changed since)
	extensionCache []jsonmodels.UIPlugin
	// subscriptionCache caches the subscribed applications by tenant ID
	subscriptionCache map[string]subscriptions
	// baseTenantID is the tenant of the credentials running the sync
	baseTenantID string
	// featureOverrides caches the per-tenant feature toggle overrides by
	// feature key (tenant ID -> active)
	featureOverrides map[string]map[string]bool

	// tenantSpecs are the child tenants of the manifest, and createdTenants
	// the IDs of those created in this run by domain (lower case)
	tenantSpecs    []ChildTenantSpec
	createdTenants map[string]string

	// mu guards Results and the printed output: command groups record
	// results from concurrent goroutines
	mu sync.Mutex
}

func (s *Syncer) record(section, item string, action Action, detail string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(section, item, action, detail, err)
}

func (s *Syncer) recordLocked(section, item string, action Action, detail string, err error) {
	if err != nil {
		action = ActionFailed
	}
	var tenant string
	if s.multiTarget && s.activeTarget != nil {
		tenant = s.activeTarget.Label()
	}
	result := ChangeResult{
		Tenant:  tenant,
		Section: section,
		Item:    item,
		Action:  action,
		Detail:  detail,
		Err:     err,
	}
	s.Results = append(s.Results, result)

	symbol := map[Action]string{
		ActionCreated:   "✚",
		ActionUpdated:   "↻",
		ActionUnchanged: "✓",
		ActionSkipped:   "⏭",
		ActionExecuted:  "▸",
		ActionPlanned:   "→",
		ActionDeleted:   "−",
		ActionFailed:    "✗",
	}[action]

	line := fmt.Sprintf("  %s [%s] %s (%s)", symbol, section, result.DisplayItem(), action)
	if detail != "" {
		line += ": " + detail
	}
	if err != nil {
		line += ": " + err.Error()
	}
	fmt.Println(line)
}

// syncMeta returns the bookkeeping fragment attached to created/updated
// items. It is passed as an upsert *annotation*, so it is written whenever
// something real changes but never participates in change detection — the
// syncedAt timestamp therefore records when the desired state last changed,
// without breaking idempotency.
func syncMeta() map[string]any {
	return map[string]any{
		"tool":     "tenant-sync",
		"syncedAt": time.Now().Format(time.RFC3339),
	}
}

// resolveSource resolves a source and reports how to proceed: ok means the
// files should be processed; otherwise the outcome (skipped for optional
// sources with nothing to do, failed for real errors) has been recorded.
func (s *Syncer) resolveSource(section, item string, source Source) (files []ResolvedFile, ok bool) {
	files, err := s.Resolver.Resolve(source)
	if err != nil {
		if source.Optional && errors.Is(err, ErrNothingToDo) {
			s.record(section, item, ActionSkipped, err.Error(), nil)
			return nil, false
		}
		s.record(section, item, ActionFailed, "resolve source", err)
		return nil, false
	}
	return files, true
}

// fetchFiles downloads the files which only reference a URL, for sections
// which read the file content. A failed download is recorded and the file
// left out; ok is false when any download failed.
func (s *Syncer) fetchFiles(section string, files []ResolvedFile) (fetched []ResolvedFile, ok bool) {
	ok = true
	fetched = make([]ResolvedFile, 0, len(files))
	for _, file := range files {
		if s.DryRun {
			fetched = append(fetched, file)
			continue
		}
		local, err := s.Resolver.Fetch(file)
		if err != nil {
			s.record(section, file.Filename, ActionFailed, "", err)
			ok = false
			continue
		}
		fetched = append(fetched, local)
	}
	return fetched, ok
}

// notDownloaded reports whether a resolved file is a remote asset which
// dry-run mode lists without downloading (so its content cannot be read)
func (s *Syncer) notDownloaded(file ResolvedFile) bool {
	if !s.DryRun {
		return false
	}
	if file.Path == "" {
		return true
	}
	_, err := os.Stat(file.Path)
	return err != nil
}

// jsonEqual compares a desired Go value with an existing JSON fragment by
// normalising both through JSON. Used to decide whether an update is needed.
func jsonEqual(desired any, existingRaw string) bool {
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false
	}
	var a, b any
	if err := json.Unmarshal(desiredJSON, &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(existingRaw), &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// actionFromResult maps an op.Result status + meta to a sync action
func actionFromResult(status op.Status, meta map[string]any) Action {
	switch status {
	case op.StatusCreated:
		return ActionCreated
	case op.StatusUpdated:
		return ActionUpdated
	case op.StatusSkipped:
		return ActionUnchanged
	}
	if meta != nil {
		if found, ok := meta["found"].(bool); ok && !found {
			return ActionCreated
		}
	}
	return ActionUnchanged
}

// Apply provisions the child tenants, then resolves the target tenants and
// applies the manifest to each of them in turn. The tenants section runs once
// with the base credentials, before the targets are resolved, so a tenant
// created in this run can be a target of the same run. The applications and
// features sections always run with the base credentials (their APIs address
// other tenants by ID from the parent); all other sections run with the
// target tenant's own credentials.
func (s *Syncer) Apply(ctx context.Context, manifest *Manifest, only []string) error {
	// The tenantAdmin credentials mode logs in with the admin credentials of
	// the tenants section, also when the section itself is not applied
	s.tenantSpecs = manifest.Tenants
	if sectionEnabled(only, SectionTenants) {
		if err := s.applyChildTenants(ctx, manifest); err != nil {
			return err
		}
		if onlySections(only, SectionTenants) {
			// Nothing else to apply: the targets are not resolved (which
			// could provision credentials in the target tenants)
			return nil
		}
	}

	spec := s.Targets
	if spec == nil {
		spec = manifest.Targets
	}

	var targets []Target
	if s.DryRun {
		// No API calls in dry-run mode: describe the selection instead of
		// resolving it
		targets = dryRunTargets(spec)
	} else {
		var err error
		targets, err = s.resolveTargets(ctx, spec)
		if err != nil {
			return err
		}
		if err := s.resolveTargetCredentials(ctx, targets, spec); err != nil {
			return err
		}
	}

	s.multiTarget = len(targets) > 1 || (len(targets) == 1 && !targets[0].Current)

	for i := range targets {
		s.activeTarget = &targets[i]
		targetCtx := ctx
		if targets[i].Auth != nil {
			targetCtx = api.WithAuth(ctx, *targets[i].Auth)
		}
		if s.multiTarget {
			fmt.Printf("\n🏢 Tenant: %s\n", targets[i].Label())
		}
		if err := s.applyTarget(targetCtx, ctx, manifest, only); err != nil {
			return err
		}
	}
	return nil
}

// applyTarget runs the pre hooks, all requested sections of the manifest in
// order, and finally the post hooks for a single target tenant. Hooks always
// run when defined, regardless of any --only section filter; a failing pre
// hook aborts the run.
//
// ctx carries the target tenant's credentials; baseCtx the base credentials
// of the current tenant (used by the sections that address the target tenant
// by ID from the parent).
func (s *Syncer) applyTarget(ctx, baseCtx context.Context, manifest *Manifest, only []string) error {
	s.dryRunPending = nil
	s.roleDeletes = nil
	s.globalRoleDeletes = nil
	s.deviceGroupDeletes = nil
	deleteAfter := deferredDeleteSection(only)
	if err := s.runHooks(ctx, "pre", manifest.Hooks.Pre); err != nil {
		return err
	}
	// Post hooks run even when a section fails, so cleanup-style hooks
	// always get a chance to execute
	defer s.runHooks(ctx, "post", manifest.Hooks.Post)
	for _, section := range SectionOrder {
		if section == SectionTenants || !sectionEnabled(only, section) {
			// The tenants section runs once before the targets (see Apply)
			continue
		}

		// Section hooks run around the section (and only when the section
		// itself runs). Like the global hooks, a failing pre hook aborts and
		// post hooks always run.
		sectionHooks := manifest.Hooks.Sections[section]
		if err := s.runHooks(ctx, "sections."+section+".pre", sectionHooks.Pre); err != nil {
			return err
		}

		var err error
		switch section {
		case SectionTenantOptions:
			err = s.SyncTenantOptions(ctx, manifest.TenantOptions)
		case SectionFeatures:
			err = s.SyncFeatures(baseCtx, manifest.Features)
		case SectionRetentionRules:
			err = s.SyncRetentionRules(ctx, manifest.RetentionRules)
		case SectionCertificateAuthority:
			err = s.SyncCertificateAuthority(ctx, manifest.CertificateAuthority)
		case SectionTrustedCertificates:
			err = s.SyncTrustedCertificates(ctx, manifest.TrustedCertificates)
		case SectionCertificateRevocations:
			err = s.SyncCertificateRevocationLists(ctx, manifest.CertificateRevocationLists)
		case SectionMicroservices:
			err = s.SyncMicroservices(baseCtx, manifest.Microservices)
		case SectionExtensions:
			err = s.SyncExtensions(baseCtx, manifest.Extensions)
		case SectionApplications:
			err = s.SyncApplications(baseCtx, manifest.Applications)
		case SectionDeviceGroups:
			err = s.SyncDeviceGroups(ctx, manifest.DeviceGroups)
		case SectionGlobalRoles:
			err = s.SyncGlobalRoles(ctx, manifest.GlobalRoles)
		case SectionInventoryRoles:
			err = s.SyncInventoryRoles(ctx, manifest.InventoryRoles)
		case SectionUsers:
			err = s.SyncUsers(ctx, manifest.Users)
		case SectionLoginOptions:
			err = s.SyncLoginOptions(ctx, manifest.LoginOptions)
		case SectionSoftware:
			err = s.SyncSoftware(ctx, manifest.Software)
		case SectionFirmware:
			err = s.SyncFirmware(ctx, manifest.Firmware)
		case SectionConfiguration:
			err = s.SyncConfiguration(ctx, manifest.Configuration)
		case SectionSmartRest:
			err = s.SyncSmartRestTemplates(ctx, manifest.SmartRestTemplates)
		case SectionDigitalTwin:
			err = s.SyncDigitalTwin(ctx, manifest.DigitalTwin)
		case SectionSmartFunctions:
			err = s.SyncSmartFunctions(ctx, manifest.SmartFunctions)
		case SectionDeviceProfiles:
			err = s.SyncDeviceProfiles(ctx, manifest.DeviceProfiles)
		case SectionCommands:
			err = s.SyncCommands(ctx, manifest.Commands)
		}

		// Inventory roles, global roles and device groups with state
		// absent are deleted once the users section has run (or after the
		// last of these sections which --only selects), since Cumulocity
		// refuses to delete an inventory role which is still assigned and
		// the users may still use the roles and groups until then
		if err == nil && section == deleteAfter {
			s.runDeferredDeletes(ctx)
		}

		s.runHooks(ctx, "sections."+section+".post", sectionHooks.Post)

		if err != nil {
			return fmt.Errorf("%s: %w", section, err)
		}
	}
	return nil
}

// applyChildTenants runs the tenants section with its section hooks. It runs
// once per run with the base credentials (not per target).
func (s *Syncer) applyChildTenants(ctx context.Context, manifest *Manifest) error {
	sectionHooks := manifest.Hooks.Sections[SectionTenants]
	if err := s.runHooks(ctx, "sections."+SectionTenants+".pre", sectionHooks.Pre); err != nil {
		return err
	}
	err := s.SyncTenants(ctx, manifest.Tenants)
	s.runHooks(ctx, "sections."+SectionTenants+".post", sectionHooks.Post)
	if err != nil {
		return fmt.Errorf("%s: %w", SectionTenants, err)
	}
	return nil
}

// sectionEnabled reports whether a section is selected by the --only filter
// (an empty filter selects all sections)
func sectionEnabled(only []string, section string) bool {
	if len(only) == 0 {
		return true
	}
	for _, name := range only {
		if strings.EqualFold(strings.TrimSpace(name), section) {
			return true
		}
	}
	return false
}

// onlySections reports whether the --only filter selects nothing but the
// given section
func onlySections(only []string, section string) bool {
	if len(only) == 0 {
		return false
	}
	for _, name := range only {
		if !strings.EqualFold(strings.TrimSpace(name), section) {
			return false
		}
	}
	return true
}

// Summary aggregates results per action
func (s *Syncer) Summary() (counts map[Action]int, failures []ChangeResult) {
	counts = make(map[Action]int)
	for _, result := range s.Results {
		counts[result.Action]++
		if result.Action == ActionFailed {
			failures = append(failures, result)
		}
	}
	return counts, failures
}

// PrintSummary prints the aggregated results
func (s *Syncer) PrintSummary(elapsed time.Duration) {
	counts, failures := s.Summary()

	fmt.Println("\n" + strings.Repeat("─", 50))

	keys := make([]string, 0, len(counts))
	for action := range counts {
		keys = append(keys, string(action))
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	total := 0
	for _, key := range keys {
		count := counts[Action(key)]
		total += count
		parts = append(parts, fmt.Sprintf("%d %s", count, key))
	}
	fmt.Printf("Total: %d item(s) (%s)\n", total, strings.Join(parts, ", "))
	fmt.Printf("⏱️  Total time: %.1fs\n", elapsed.Seconds())

	if len(failures) > 0 {
		fmt.Printf("\n❌ Failures:\n")
		for _, failure := range failures {
			fmt.Printf("  • [%s] %s: %v\n", failure.Section, failure.DisplayItem(), failure.Err)
		}
	}
}
