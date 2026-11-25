package main

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is the declarative description of the desired tenant state.
// Sections are applied in a fixed order so that later sections (e.g. device
// profiles) can reference items created by earlier ones (firmware, software,
// configuration).
type Manifest struct {
	Include []IncludeSpec `yaml:"include" json:"include,omitempty" jsonschema:"description=Other manifest files merged before this file's content: paths or glob patterns relative to this file. List sections are concatenated (includes first); entries with the same identity (e.g. tenant option category+key or user group name) and the targets and certificateAuthority sections of a later file replace earlier ones"`

	Targets *TargetsSpec `yaml:"targets" json:"targets,omitempty" jsonschema:"description=Which tenants the manifest is applied to (defaults to the current tenant); applying to other tenants requires parent/management tenant credentials"`

	Tenants []ChildTenantSpec `yaml:"tenants" json:"tenants,omitempty" jsonschema:"description=Child tenants of the current (parent/management) tenant to create or update (matched by domain); runs once with the base credentials before the targets are resolved; tenants are never deleted or suspended"`

	TenantOptions []TenantOptionSpec `yaml:"tenantOptions" json:"tenantOptions,omitempty" jsonschema:"description=Tenant options to set (created if missing and updated when the value differs)"`
	Features      []FeatureSpec      `yaml:"features" json:"features,omitempty" jsonschema:"description=Feature toggles to enable or disable for the current tenant"`

	RetentionRules []RetentionRuleSpec `yaml:"retentionRules" json:"retentionRules,omitempty" jsonschema:"description=Retention rules to ensure exist (matched by dataType/fragmentType/type/source; maximumAge updated when it differs)"`

	CertificateAuthority *CertificateAuthoritySpec `yaml:"certificateAuthority" json:"certificateAuthority,omitempty" jsonschema:"description=Enable or disable the tenant's Cumulocity certificate authority (CA) certificate"`

	TrustedCertificates        []TrustedCertificateSpec        `yaml:"trustedCertificates" json:"trustedCertificates,omitempty" jsonschema:"description=Trusted device certificates (PEM/DER) to upload into the tenant trust store (matched by fingerprint; name/status/autoRegistrationEnabled updated when they differ)"`
	CertificateRevocationLists []CertificateRevocationListSpec `yaml:"certificateRevocationLists" json:"certificateRevocationLists,omitempty" jsonschema:"description=Certificate revocation entries to upload from CSV files (columns SERIALNO and optional DATE); additive: existing entries are never removed"`

	Applications []ApplicationSpec `yaml:"applications" json:"applications,omitempty" jsonschema:"description=Applications to subscribe to the current tenant (optionally created/uploaded from a source)"`
	DeviceGroups []DeviceGroupSpec `yaml:"deviceGroups" json:"deviceGroups,omitempty" jsonschema:"description=Tree of device groups to create/update (a group is identified by its name within its parent; groups are never deleted or unlinked)"`
	UserGroups   []UserGroupSpec   `yaml:"userGroups" json:"userGroups,omitempty" jsonschema:"description=User groups (global roles) to create/update with roles assigned (additive: existing roles are never removed)"`
	// InventoryRoles are permission sets which users get on specific device
	// groups through users[].inventoryAssignments
	InventoryRoles []InventoryRoleSpec `yaml:"inventoryRoles" json:"inventoryRoles,omitempty" jsonschema:"description=Inventory roles to create/update (matched by name); users get them on device groups through users[].inventoryAssignments"`
	Users          []UserSpec          `yaml:"users" json:"users,omitempty" jsonschema:"description=Users to create/update and assign to user groups (additive: existing memberships are never removed)"`
	Software       []SoftwareSpec      `yaml:"software" json:"software,omitempty" jsonschema:"description=Software packages to sync into the software repository"`
	Firmware       []FirmwareSpec      `yaml:"firmware" json:"firmware,omitempty" jsonschema:"description=Firmware (OS images) to sync into the firmware repository"`
	Configuration  []ConfigurationSpec `yaml:"configuration" json:"configuration,omitempty" jsonschema:"description=Configuration files to sync into the configuration repository"`

	SmartRestTemplates []SmartRestTemplateSpec `yaml:"smartrestTemplates" json:"smartrestTemplates,omitempty" jsonschema:"description=SmartREST 2.0 template collections to sync from exported JSON files (one collection per file)"`

	DigitalTwin    *DigitalTwinSpec    `yaml:"digitalTwin" json:"digitalTwin,omitempty" jsonschema:"description=Digital Twin Manager (DTM) property and asset definitions to create/update (matched by identifier, and contexts for property definitions); only written when they differ"`
	SmartFunctions []SmartFunctionSpec `yaml:"smartFunctions" json:"smartFunctions,omitempty" jsonschema:"description=Data Preparation smart functions (rules) to deploy from rule package folders or exported .tar.gz archives; only deployed when the code, configuration or tests differ"`

	DeviceProfiles []DeviceProfileSpec `yaml:"deviceProfiles" json:"deviceProfiles,omitempty" jsonschema:"description=Device profiles referencing firmware/software/configuration in the tenant"`
	Commands       []CommandGroupSpec  `yaml:"commands" json:"commands,omitempty" jsonschema:"description=Named groups of shell commands run as the last section; actions within a group run in sequence and groups run concurrently"`
	Hooks          HooksSpec           `yaml:"hooks" json:"hooks,omitempty" jsonschema:"description=Commands executed before and after the sync and around individual sections (e.g. go-c8y-cli calls)"`

	// IncludedFiles are the files merged via include, relative to the
	// manifest directory where possible (set by LoadManifestWith)
	IncludedFiles []string `yaml:"-" json:"-"`
}

// ChildTenantSpec declares a child tenant of the current tenant. It is matched
// by domain among the child tenants: a missing tenant is created (with the
// admin credentials), an existing tenant only gets its company, contact and
// custom properties updated. The domain and admin credentials of an existing
// tenant are never changed, and tenants are never deleted or suspended.
type ChildTenantSpec struct {
	Domain  string `yaml:"domain" json:"domain" jsonschema:"description=Full domain name of the tenant; the unique key matched case-insensitively against the domains of the child tenants,example=customer-a.eu-latest.cumulocity.com"`
	Company string `yaml:"company" json:"company,omitempty" jsonschema:"description=Company name (required to create the tenant; updated when it differs)"`

	// ID is optional: the platform generates a tenant ID when it is not set.
	// It is only used when creating the tenant and can never be changed.
	ID string `yaml:"id" json:"id,omitempty" jsonschema:"description=Tenant ID to create the tenant with (optional; generated by the platform when omitted). Only used when creating the tenant: an existing tenant with the domain but another ID fails instead of being changed,example=customer-a"`

	AdminName  string `yaml:"adminName" json:"adminName,omitempty" jsonschema:"description=Username of the tenant's first administrator (only used when creating the tenant)"`
	AdminEmail string `yaml:"adminEmail" json:"adminEmail,omitempty" jsonschema:"description=Email address of the tenant's first administrator (only used when creating the tenant; required with sendPasswordResetEmail)"`

	// AdminPass is only used when creating the tenant. Inject secrets with
	// ${VAR} expansion or use AdminPasswordFrom.
	AdminPass string `yaml:"adminPass" json:"adminPass,omitempty" jsonschema:"description=Initial password of the administrator used only when creating the tenant; supports ${VAR} expansion; mutually exclusive with adminPasswordFrom and sendPasswordResetEmail"`

	// AdminPasswordFrom takes the initial admin password from an environment
	// variable or a generated value
	AdminPasswordFrom *SecretRef `yaml:"adminPasswordFrom" json:"adminPasswordFrom,omitempty" jsonschema:"description=Take the initial administrator password from an environment variable (env) or a generated value (generate); mutually exclusive with adminPass and sendPasswordResetEmail"`

	// SendPasswordResetEmail lets the platform email a password reset link to
	// the administrator instead of setting a password
	SendPasswordResetEmail bool `yaml:"sendPasswordResetEmail" json:"sendPasswordResetEmail,omitempty" jsonschema:"description=Email a password reset link to adminEmail when creating the tenant (instead of adminPass or adminPasswordFrom)"`

	ContactName  string `yaml:"contactName" json:"contactName,omitempty" jsonschema:"description=Name of the contact person (updated when it differs)"`
	ContactPhone string `yaml:"contactPhone" json:"contactPhone,omitempty" jsonschema:"description=Phone number of the contact person (updated when it differs)"`

	// CustomProperties are merged into the tenant's custom properties: keys
	// not in the manifest are kept
	CustomProperties map[string]any `yaml:"customProperties" json:"customProperties,omitempty" jsonschema:"description=Custom properties of the tenant (declared keys are set when they differ; other keys are kept)"`

	// Applications are subscribed to the tenant (additive: subscriptions not
	// listed are kept)
	Applications []string `yaml:"applications" json:"applications,omitempty" jsonschema:"description=Names of applications to subscribe to the tenant (additive; missing subscriptions are also added to existing tenants),example=devicemanagement"`
}

// Credential modes for applying the manifest to other tenants
const (
	// CredentialsModeServiceUser provisions a placeholder microservice and
	// uses its per-tenant service users (the go-c8y-cli "microservice service
	// user" pattern)
	CredentialsModeServiceUser = "serviceUser"
	// CredentialsModeSessions reads credentials from go-c8y-cli session files
	CredentialsModeSessions = "sessions"
	// CredentialsModeTenantAdmin logs in to each target with the admin
	// credentials of its entry in the tenants section (e.g. a generated
	// password stored in the secrets file)
	CredentialsModeTenantAdmin = "tenantAdmin"
)

// DefaultCredentialsApplication is the name of the placeholder microservice
// created in serviceUser mode
const DefaultCredentialsApplication = "tenant-sync"

// TargetsSpec selects which tenants the manifest is applied to. The selection
// modes are additive (the union of all matching tenants, deduplicated).
// Without a targets section the manifest applies to the current tenant only.
type TargetsSpec struct {
	// Current includes the current tenant. Defaults to true when no other
	// selection mode is set, false otherwise.
	Current *bool `yaml:"current" json:"current,omitempty" jsonschema:"description=Include the current tenant (defaults to true only when no other selection mode is set)"`

	// AllChildren selects every child tenant of the current tenant
	AllChildren bool `yaml:"allChildren" json:"allChildren,omitempty" jsonschema:"description=Apply to all child tenants of the current tenant (requires parent/management tenant credentials)"`

	// Tenants lists tenants explicitly by ID or domain (entries containing a
	// "." are treated as domains)
	Tenants []string `yaml:"tenants" json:"tenants,omitempty" jsonschema:"description=Tenants referenced by ID or by domain (entries containing a dot are treated as domains)"`

	// Selector matches tenants by criteria
	Selector *TenantSelector `yaml:"selector" json:"selector,omitempty" jsonschema:"description=Select tenants matching criteria (domain glob and/or company name)"`

	// Credentials configures how per-tenant credentials are obtained for
	// tenants other than the current one
	Credentials *TargetCredentials `yaml:"credentials" json:"credentials,omitempty" jsonschema:"description=How credentials for the other tenants are obtained (defaults to the serviceUser mode)"`
}

// IncludesCurrent reports whether the current tenant is part of the targets
func (t *TargetsSpec) IncludesCurrent() bool {
	if t == nil {
		return true
	}
	if t.Current != nil {
		return *t.Current
	}
	return !t.HasRemoteSelection()
}

// HasRemoteSelection reports whether any selection mode other than the
// current tenant is configured
func (t *TargetsSpec) HasRemoteSelection() bool {
	return t != nil && (t.AllChildren || len(t.Tenants) > 0 || t.Selector != nil)
}

// CredentialsMode returns the configured credentials mode (defaulting to
// serviceUser)
func (t *TargetsSpec) CredentialsMode() string {
	if t == nil || t.Credentials == nil || t.Credentials.Mode == "" {
		return CredentialsModeServiceUser
	}
	return t.Credentials.Mode
}

// TenantSelector matches tenants by criteria. All given criteria must match.
type TenantSelector struct {
	// Domain is a glob pattern matched against the tenant domain
	Domain string `yaml:"domain" json:"domain,omitempty" jsonschema:"description=Glob pattern matched against the tenant domain,example=*.iot.example.com"`

	// Company is the exact company name of the tenant
	Company string `yaml:"company" json:"company,omitempty" jsonschema:"description=Exact company name of the tenant"`
}

// TargetCredentials configures how credentials for tenants other than the
// current one are obtained
type TargetCredentials struct {
	// Mode selects the credential source: serviceUser (default) provisions a
	// placeholder microservice owned by the current tenant, subscribes it to
	// each target and uses the resulting per-tenant service users; sessions
	// reads go-c8y-cli session files.
	Mode string `yaml:"mode" json:"mode,omitempty" jsonschema:"description=Credential source for other tenants (defaults to serviceUser). tenantAdmin uses the admin credentials of the matching tenants entry,enum=serviceUser,enum=sessions,enum=tenantAdmin"`

	// Application is the name of the placeholder microservice used in
	// serviceUser mode (created if missing; defaults to tenant-sync)
	Application string `yaml:"application" json:"application,omitempty" jsonschema:"description=Name of the placeholder microservice used in serviceUser mode (created if missing; defaults to tenant-sync)"`

	// SessionHome is the directory containing go-c8y-cli session files used
	// in sessions mode (defaults to $C8Y_SESSION_HOME and then ~/.cumulocity)
	SessionHome string `yaml:"sessionHome" json:"sessionHome,omitempty" jsonschema:"description=Directory containing go-c8y-cli session files for the sessions mode (defaults to $C8Y_SESSION_HOME and then ~/.cumulocity)"`
}

func (t *TargetsSpec) Validate() error {
	if t == nil {
		return nil
	}
	if t.Selector != nil && t.Selector.Domain == "" && t.Selector.Company == "" {
		return fmt.Errorf("selector requires at least one of: domain, company")
	}
	if t.Current != nil && !*t.Current && !t.HasRemoteSelection() {
		return fmt.Errorf("no tenants selected: current is false and no other selection mode is set")
	}
	if t.Credentials != nil {
		switch t.Credentials.Mode {
		case "", CredentialsModeServiceUser, CredentialsModeSessions, CredentialsModeTenantAdmin:
		default:
			return fmt.Errorf("unknown credentials mode %q: supported modes are %s, %s and %s",
				t.Credentials.Mode, CredentialsModeServiceUser, CredentialsModeSessions, CredentialsModeTenantAdmin)
		}
	}
	return nil
}

// HooksSpec defines commands executed around the sync and around individual
// sections
type HooksSpec struct {
	Pre  []HookSpec `yaml:"pre" json:"pre,omitempty" jsonschema:"description=Commands executed before the sync; a failing pre hook aborts the run"`
	Post []HookSpec `yaml:"post" json:"post,omitempty" jsonschema:"description=Commands executed after the sync (also when sections failed); failures are reported but do not abort"`

	// Sections holds hooks running around individual sections (keyed by the
	// section name). Unlike the global hooks they are skipped when the
	// section is excluded by --only.
	Sections map[string]SectionHooksSpec `yaml:"sections" json:"sections,omitempty" jsonschema:"description=Hooks executed around individual sections (keyed by section name e.g. software); skipped when the section is excluded by --only"`
}

// SectionHooksSpec defines commands executed around a single section
type SectionHooksSpec struct {
	Pre  []HookSpec `yaml:"pre" json:"pre,omitempty" jsonschema:"description=Commands executed before the section; a failing pre hook aborts the run"`
	Post []HookSpec `yaml:"post" json:"post,omitempty" jsonschema:"description=Commands executed after the section (also when items failed); failures are reported but do not abort"`
}

// CommandGroupSpec is a named group of shell commands. The actions of a group
// run in sequence (stopping at the first failure); separate groups run
// concurrently with each other.
type CommandGroupSpec struct {
	// Name of the group (used in the output)
	Name string `yaml:"name" json:"name" jsonschema:"description=Name of the command group (used in the output)"`

	// Actions are shell commands executed in sequence via 'sh -c' from the
	// manifest directory. A failing action skips the rest of its group.
	Actions []string `yaml:"actions" json:"actions" jsonschema:"description=Shell commands executed in sequence via 'sh -c' from the manifest directory; a failing action skips the rest of the group"`

	// Dir is the working directory of the actions: the directory of the
	// included manifest file which declares the group (empty: the manifest
	// directory)
	Dir string `yaml:"-" json:"-"`
}

// HookSpec is a single command executed via the shell with the manifest
// directory as the working directory and the current environment (including
// C8Y_* session variables) passed through
type HookSpec struct {
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Display name of the hook"`
	Run  string `yaml:"run" json:"run" jsonschema:"description=Command executed with 'sh -c' from the manifest directory"`

	// Dir is the working directory of the command: the directory of the
	// included manifest file which declares the hook (empty: the manifest
	// directory)
	Dir string `yaml:"-" json:"-"`
}

// TenantOptionSpec sets a tenant option (category/key/value). The value is
// either given literally (value) or resolved from the tenant (valueFrom).
type TenantOptionSpec struct {
	Category string `yaml:"category" json:"category" jsonschema:"description=Tenant option category"`
	Key      string `yaml:"key" json:"key" jsonschema:"description=Tenant option key"`
	Value    string `yaml:"value" json:"value,omitempty" jsonschema:"description=Desired value of the tenant option (mutually exclusive with valueFrom)"`

	// ValueFrom resolves the value by a named lookup at apply time,
	// e.g. an application ID looked up by application name
	ValueFrom *TenantOptionValueFrom `yaml:"valueFrom" json:"valueFrom,omitempty" jsonschema:"description=Resolve the value by a named lookup at apply time (mutually exclusive with value)"`
}

// TenantOptionValueFrom resolves a tenant option value by reference.
// Exactly one field must be set.
type TenantOptionValueFrom struct {
	// Application resolves to the application ID looked up by name
	Application string `yaml:"application" json:"application,omitempty" jsonschema:"description=Application name resolved to its application ID,example=devicemanagement"`

	// Device resolves to the managed object ID of a device looked up by name
	Device string `yaml:"device" json:"device,omitempty" jsonschema:"description=Device name resolved to its managed object ID"`

	// Env takes the value from an environment variable
	Env string `yaml:"env" json:"env,omitempty" jsonschema:"description=Name of an environment variable which holds the value (it must be set and not empty),example=MY_SECRET"`

	// Generate takes a generated value (NAME or NAME:LENGTH)
	Generate string `yaml:"generate" json:"generate,omitempty" jsonschema:"description=Name of a generated value (NAME or NAME:LENGTH) stored in the secrets file; the same as ${generate:NAME},example=integration-secret"`
}

// FeatureSpec enables or disables a feature toggle on the current tenant
type FeatureSpec struct {
	Key     string `yaml:"key" json:"key" jsonschema:"description=Feature toggle key"`
	Enabled *bool  `yaml:"enabled" json:"enabled,omitempty" jsonschema:"description=Desired feature state (defaults to true when omitted)"` // defaults to true when omitted
}

func (f FeatureSpec) IsEnabled() bool {
	return f.Enabled == nil || *f.Enabled
}

// RetentionRuleDataTypes are the data types accepted by the retention rules API
var RetentionRuleDataTypes = []string{"ALARM", "AUDIT", "BULK_OPERATION", "EVENT", "MEASUREMENT", "OPERATION", "*"}

// RetentionRuleSpec ensures a retention rule exists with the desired maximum
// age. Retention rules have no name: a rule is identified by its selector
// combination (dataType, fragmentType, type, source — wildcards default to
// "*"), and maximumAge (plus editable) is the managed value.
type RetentionRuleSpec struct {
	// DataType the rule applies to
	DataType string `yaml:"dataType" json:"dataType" jsonschema:"description=Data type the rule applies to,enum=ALARM,enum=AUDIT,enum=BULK_OPERATION,enum=EVENT,enum=MEASUREMENT,enum=OPERATION,enum=*"`

	// FragmentType restricts the rule to documents with the fragment (default "*")
	FragmentType string `yaml:"fragmentType" json:"fragmentType,omitempty" jsonschema:"description=Restrict the rule to documents carrying this fragment (defaults to *)"`

	// Type restricts the rule to documents of the type (default "*")
	Type string `yaml:"type" json:"type,omitempty" jsonschema:"description=Restrict the rule to documents of this type (defaults to *)"`

	// Source restricts the rule to documents of a source device (default "*")
	Source string `yaml:"source" json:"source,omitempty" jsonschema:"description=Restrict the rule to documents of this source device ID (defaults to *)"`

	// MaximumAge is the retention period in days
	MaximumAge int64 `yaml:"maximumAge" json:"maximumAge" jsonschema:"description=Retention period in days,minimum=1"`

	// Editable marks the rule as editable in the UI (only written when set)
	Editable *bool `yaml:"editable" json:"editable,omitempty" jsonschema:"description=Whether the rule is editable in the UI (only written when set)"`
}

// Selector returns the rule identity (wildcards applied), used for matching
// existing rules and as the display label
func (r RetentionRuleSpec) Selector() string {
	return orWildcard(r.DataType) + "/" + orWildcard(r.FragmentType) + "/" + orWildcard(r.Type) + "/" + orWildcard(r.Source)
}

func orWildcard(value string) string {
	if value == "" {
		return "*"
	}
	return value
}

// TrustedCertificateStatuses are the statuses accepted by the trusted
// certificates API
var TrustedCertificateStatuses = []string{"ENABLED", "DISABLED"}

// TrustedCertificateSpec uploads trusted device certificates into the tenant
// trust store. Certificates are matched by their fingerprint: missing ones
// are uploaded, and the name, status and autoRegistrationEnabled of existing
// ones are updated when they differ. Certificates are never deleted.
// CertificateAuthoritySpec manages the tenant's Cumulocity certificate
// authority (CA), which issues device certificates (e.g. for certificate
// based device registration)
type CertificateAuthoritySpec struct {
	// Enabled creates the CA when it does not exist and sets its trusted
	// certificate to ENABLED. false sets it to DISABLED; the CA is never
	// deleted, since that would invalidate every certificate it issued
	Enabled *bool `yaml:"enabled" json:"enabled" jsonschema:"description=true: create the Cumulocity CA if it does not exist and enable it. false: disable the CA certificate (it is never deleted)"`

	// AutoRegistrationEnabled lets devices presenting certificates signed by
	// the CA register automatically (only managed when set)
	AutoRegistrationEnabled *bool `yaml:"autoRegistrationEnabled" json:"autoRegistrationEnabled,omitempty" jsonschema:"description=Allow devices presenting certificates signed by the CA to register automatically (left untouched when omitted)"`
}

type TrustedCertificateSpec struct {
	// Name overrides the certificate name (only sensible for sources that
	// resolve to a single certificate). Defaults to the certificate subject
	// common name, then the filename.
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Override the certificate name (only sensible for sources resolving to a single certificate); defaults to the certificate subject common name and then the filename"`

	// Status is the desired certificate status (defaults to ENABLED)
	Status string `yaml:"status" json:"status,omitempty" jsonschema:"description=Desired certificate status (defaults to ENABLED),enum=ENABLED,enum=DISABLED"`

	// AutoRegistrationEnabled lets devices presenting certificates signed by
	// this certificate register automatically (only managed when set)
	AutoRegistrationEnabled *bool `yaml:"autoRegistrationEnabled" json:"autoRegistrationEnabled,omitempty" jsonschema:"description=Allow devices presenting certificates signed by this certificate to register automatically (left untouched when omitted)"`

	// Source provides the certificate files (PEM with one or more
	// certificates, or a single DER/base64 encoded certificate per file)
	Source Source `yaml:"source" json:"source" jsonschema:"description=Where the certificate files come from (PEM with one or more certificates or a single DER/base64 certificate per file; directories are scanned for *.pem and *.crt and *.cer by default)"`
}

// CertificateRevocationListSpec uploads certificate revocation entries from
// CSV files (header SERIALNO[,DATE]; one certificate serial number in hex per
// row). Uploads are additive: entries already on the tenant revocation list
// are never removed.
type CertificateRevocationListSpec struct {
	// Source provides the CSV files (directories are scanned for *.csv)
	Source Source `yaml:"source" json:"source" jsonschema:"description=Where the revocation list CSV files come from (columns SERIALNO and optional DATE; directories are scanned for *.csv by default)"`
}

// ApplicationSpec subscribes an application (by name) to the target tenant,
// or unsubscribes it with subscribed: false. With a source, the application
// is also created if missing and its binary (e.g. a microservice or web
// application zip) uploaded.
type ApplicationSpec struct {
	Name string `yaml:"name" json:"name" jsonschema:"description=Application name"`
	Type string `yaml:"type" json:"type,omitempty" jsonschema:"description=Application type (lookup filter; also used when creating from a source where it defaults to MICROSERVICE),example=MICROSERVICE,example=HOSTED"` // optional application type filter (e.g. MICROSERVICE, HOSTED)

	// Subscribed declares the desired subscription state (defaults to true).
	// With subscribed: false the application is unsubscribed from the target
	// tenant (and never created/uploaded, even when a source is given).
	Subscribed *bool `yaml:"subscribed" json:"subscribed,omitempty" jsonschema:"description=Desired subscription state (defaults to true when omitted); false unsubscribes the application from the target tenant"`

	// ContextPath used when creating the application (defaults to the name)
	ContextPath string `yaml:"contextPath" json:"contextPath,omitempty" jsonschema:"description=Context path used when creating the application (defaults to the name)"`

	// Source provides the application binary (a single zip file). The
	// application is created if missing and the binary uploaded on creation
	// (or on every run with --force).
	Source *Source `yaml:"source" json:"source,omitempty" jsonschema:"description=Where the application binary (zip) comes from; must resolve to a single file"`
}

func (a ApplicationSpec) IsSubscribed() bool {
	return a.Subscribed == nil || *a.Subscribed
}

// DeviceGroupSpec ensures a device group exists below its parent (or at the
// root level) together with its child groups
type DeviceGroupSpec struct {
	Name        string `yaml:"name" json:"name" jsonschema:"description=Name of the device group; identifies the group among the groups of its parent (or among the root groups),minLength=1,example=Europe"`
	Description string `yaml:"description" json:"description,omitempty" jsonschema:"description=Description of the device group (left alone when omitted)"`

	// Fragments are merged into the managed object; a fragment is only
	// updated when the existing value does not contain the desired values
	Fragments map[string]any `yaml:"fragments" json:"fragments,omitempty" jsonschema:"description=Additional fragments merged into the group managed object (updated when the existing value does not contain the desired values)"`

	Groups []DeviceGroupSpec `yaml:"groups" json:"groups,omitempty" jsonschema:"description=Child groups linked to this group as child assets"`
}

// UserGroupSpec ensures a user group (called "global role" in the Cumulocity
// UI) exists with the given description and roles. Role assignments are
// additive: roles already on the group but absent from the manifest are left
// alone.
type UserGroupSpec struct {
	Name        string `yaml:"name" json:"name" jsonschema:"description=Name of the user group,example=operators"`
	Description string `yaml:"description" json:"description,omitempty" jsonschema:"description=Description of the user group"`

	// Roles lists role names assigned to the group. Missing roles are
	// assigned; roles not listed are never removed.
	Roles []string `yaml:"roles" json:"roles,omitempty" jsonschema:"description=Role names assigned to the group (additive; roles not listed are never removed),example=ROLE_INVENTORY_READ"`
}

// UserSpec ensures a user exists with the desired profile fields and group
// memberships. The password is only used when creating the user (existing
// passwords are never overwritten) and group memberships are additive.
type UserSpec struct {
	Username  string `yaml:"userName" json:"userName,omitempty" jsonschema:"description=Username (login) of the user (defaults to the email),example=jdoe@example.com"`
	Email     string `yaml:"email" json:"email,omitempty" jsonschema:"description=Email address of the user (required to create a user; defaults to the userName when it is an email address)"`
	FirstName string `yaml:"firstName" json:"firstName,omitempty" jsonschema:"description=First name of the user"`
	LastName  string `yaml:"lastName" json:"lastName,omitempty" jsonschema:"description=Last name of the user"`
	Phone     string `yaml:"phone" json:"phone,omitempty" jsonschema:"description=Phone number of the user"`

	// Enabled declares the desired account state (defaults to true)
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty" jsonschema:"description=Desired account state (defaults to true when omitted); false disables the user"`

	// Password is only used when creating the user; existing passwords are
	// never overwritten. Inject secrets with ${VAR} expansion.
	Password string `yaml:"password" json:"password,omitempty" jsonschema:"description=Initial password used only when creating the user (never updated afterwards); supports ${VAR} expansion; mutually exclusive with passwordFrom"`

	// PasswordFrom takes the initial password from an environment variable or
	// a generated value
	PasswordFrom *SecretRef `yaml:"passwordFrom" json:"passwordFrom,omitempty" jsonschema:"description=Take the initial password from an environment variable (env) or a generated value (generate); mutually exclusive with password"`

	// SendPasswordResetEmail asks the platform to email a password reset
	// link on creation instead of (or in addition to) setting a password
	SendPasswordResetEmail bool `yaml:"sendPasswordResetEmail" json:"sendPasswordResetEmail,omitempty" jsonschema:"description=Send a password reset email when creating the user (requires email; alternative to password)"`

	// Groups lists user group names the user is assigned to. Missing
	// memberships are added; memberships not listed are never removed.
	Groups []string `yaml:"groups" json:"groups,omitempty" jsonschema:"description=User group names the user is assigned to (additive; memberships not listed are never removed),example=operators"`

	// InventoryAssignments give the user inventory roles on device groups.
	// Missing roles are added; roles and assignments not listed are never
	// removed.
	InventoryAssignments []InventoryAssignmentSpec `yaml:"inventoryAssignments" json:"inventoryAssignments,omitempty" jsonschema:"description=Inventory roles the user gets on device groups (additive; roles and assignments not listed are never removed)"`
}

// InventoryRoleScopes are the scopes of inventory role permissions
var InventoryRoleScopes = []string{"ALARM", "AUDIT", "EVENT", "MANAGED_OBJECT", "MEASUREMENT", "OPERATION", "*"}

// InventoryRolePermissionLevels are the levels of inventory role permissions.
// ALL is an alias of "*" (the name used in the Cumulocity UI).
var InventoryRolePermissionLevels = []string{"READ", "ADMIN", "*", "ALL"}

// InventoryRoleSpec ensures an inventory role exists with the given
// description and permissions. The role is matched by name; its permissions
// are replaced when the set differs from the manifest (the manifest owns the
// role definition).
type InventoryRoleSpec struct {
	Name        string `yaml:"name" json:"name" jsonschema:"description=Name of the inventory role (used to match an existing role),example=Site operator"`
	Description string `yaml:"description" json:"description,omitempty" jsonschema:"description=Description of the inventory role (only updated when set)"`

	// Permissions is the full set of permissions of the role
	Permissions []InventoryRolePermissionSpec `yaml:"permissions" json:"permissions,omitempty" jsonschema:"description=Permissions of the role (compared as a set; the role is updated when the set differs)"`
}

// InventoryRolePermissionSpec is a single permission of an inventory role,
// e.g. ADMIN on operations of type c8y_Restart
type InventoryRolePermissionSpec struct {
	Scope      string `yaml:"scope" json:"scope" jsonschema:"description=Data the permission applies to,enum=ALARM,enum=AUDIT,enum=EVENT,enum=MANAGED_OBJECT,enum=MEASUREMENT,enum=OPERATION,enum=*"`
	Type       string `yaml:"type" json:"type,omitempty" jsonschema:"description=Restrict the permission to a type or fragment name (defaults to *),example=c8y_Restart"`
	Permission string `yaml:"permission" json:"permission" jsonschema:"description=Permission level (ALL is an alias of *),enum=READ,enum=ADMIN,enum=*,enum=ALL"`
}

// Normalized returns the permission with defaults applied: type "*" and
// ALL written as "*"
func (p InventoryRolePermissionSpec) Normalized() InventoryRolePermissionSpec {
	out := InventoryRolePermissionSpec{
		Scope:      strings.TrimSpace(p.Scope),
		Type:       strings.TrimSpace(p.Type),
		Permission: strings.TrimSpace(p.Permission),
	}
	if out.Type == "" {
		out.Type = "*"
	}
	if out.Permission == "ALL" {
		out.Permission = "*"
	}
	return out
}

// Key identifies the permission for set comparison
func (p InventoryRolePermissionSpec) Key() string {
	n := p.Normalized()
	return n.Scope + ":" + n.Type + ":" + n.Permission
}

// InventoryAssignmentSpec gives a user inventory roles on a device group
type InventoryAssignmentSpec struct {
	// Group is the device group name, or a path of group names separated by
	// "/" (e.g. Europe/Plant Berlin)
	Group string `yaml:"group" json:"group" jsonschema:"description=Device group name or a path of group names separated by / (e.g. Europe/Plant Berlin); the name must match exactly one group,example=Plant Berlin"`

	// Roles are inventory role names; roles not declared in the manifest
	// must exist in the tenant
	Roles []string `yaml:"roles" json:"roles" jsonschema:"description=Inventory role names the user gets on the group (declared in inventoryRoles or existing in the tenant),example=Site operator"`
}

func (u UserSpec) IsEnabled() bool {
	return u.Enabled == nil || *u.Enabled
}

// Source describes where artifacts come from. Exactly one of Path, URL or
// GitHub should be set.
type Source struct {
	// Path is a local file or directory. Directories are searched recursively
	// using Patterns.
	Path     string   `yaml:"path" json:"path,omitempty" jsonschema:"description=Local file or directory (relative paths are resolved against the directory of the manifest file which declares it)"`
	Patterns []string `yaml:"patterns" json:"patterns,omitempty" jsonschema:"description=Glob patterns matched against filenames when path is a directory (default: *)"`

	// URL references an external file without uploading a binary.
	URL string `yaml:"url" json:"url,omitempty" jsonschema:"description=External URL referenced without uploading a binary"`

	// GitHub pulls release assets from a GitHub repository.
	GitHub *GitHubSource `yaml:"github" json:"github,omitempty" jsonschema:"description=Pull release assets from a GitHub repository"`

	// Cloudsmith pulls packages from a Cloudsmith repository.
	Cloudsmith *CloudsmithSource `yaml:"cloudsmith" json:"cloudsmith,omitempty" jsonschema:"description=Pull packages from a Cloudsmith repository (e.g. the public thin-edge.io repositories); the name and version and architecture come from the package metadata"`

	// Optional marks the source as optional: when the local path does not
	// exist or no files/assets match, the entry is skipped instead of failing.
	Optional bool `yaml:"optional" json:"optional,omitempty" jsonschema:"description=Skip this entry instead of failing when the path does not exist or nothing matches"`
}

// IsLinkOnly reports whether the GitHub assets are referenced by URL instead
// of being downloaded and uploaded
func (g *GitHubSource) IsLinkOnly() bool {
	return g != nil && g.LinkOnly != nil && *g.LinkOnly
}

// withLinkOnlyDefault returns the source with github.linkOnly set to
// defaultValue when the manifest does not set it
func (s Source) withLinkOnlyDefault(defaultValue bool) Source {
	if s.GitHub == nil || s.GitHub.LinkOnly != nil {
		return s
	}
	gh := *s.GitHub
	gh.LinkOnly = &defaultValue
	s.GitHub = &gh
	return s
}

func (s *Source) IsSet() bool {
	return s != nil && (s.Path != "" || s.URL != "" || s.GitHub != nil || s.Cloudsmith != nil)
}

func (s *Source) Validate() error {
	count := 0
	if s.Path != "" {
		count++
	}
	if s.URL != "" {
		count++
	}
	if s.GitHub != nil {
		count++
	}
	if s.Cloudsmith != nil {
		count++
	}
	if count == 0 {
		return fmt.Errorf("source requires one of: path, url, github, cloudsmith")
	}
	if count > 1 {
		return fmt.Errorf("source fields path, url, github and cloudsmith are mutually exclusive")
	}
	if s.Cloudsmith != nil {
		if err := s.Cloudsmith.validate(); err != nil {
			return err
		}
	}
	if s.GitHub != nil && !strings.Contains(s.GitHub.Repo, "/") {
		return fmt.Errorf("github.repo must be in the form owner/repo, got %q", s.GitHub.Repo)
	}
	if s.GitHub != nil && len(s.GitHub.Tags) > 0 {
		if _, err := compileTagPatterns(s.GitHub.Tags); err != nil {
			return err
		}
		if selector := releaseSelector(s.GitHub); selector != "latest" && selector != "all" {
			if _, ok := latestCount(selector); !ok {
				return fmt.Errorf("github.tags can't be combined with the release tag %q: use release latest, latest-N or all", selector)
			}
		}
	}
	if s.GitHub != nil && s.GitHub.TokenFrom != nil {
		if s.GitHub.Token != "" {
			return fmt.Errorf("github.token and github.tokenFrom are mutually exclusive")
		}
		if errs := s.GitHub.TokenFrom.validate("github.tokenFrom"); len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
	}
	return nil
}

// GitHubSource selects release assets from a GitHub repository
type GitHubSource struct {
	// Repo in the form "owner/name"
	Repo string `yaml:"repo" json:"repo" jsonschema:"description=Repository in the form owner/name,pattern=^[^/]+/[^/]+$"`

	// Release selects which release(s) to use:
	//   "latest" (default) - the latest non-prerelease release
	//   "latest-N"         - the latest N releases (e.g. "latest-5")
	//   "all"              - every release (use with care)
	//   "<tag>"            - a specific release tag (e.g. "v1.2.3")
	Release string `yaml:"release" json:"release,omitempty" jsonschema:"description=Which release(s) to use: latest (default) or latest-N for the latest N releases or all or a specific tag. With tags: per tag pattern,example=latest,example=latest-5,example=all,example=v1.2.3"`

	// Tags limits the releases to those whose tag matches one of the glob
	// patterns. release (latest, latest-N, all) then selects per pattern,
	// e.g. the latest release of each component of a monorepo which
	// publishes separate releases per component (release-please)
	Tags []string `yaml:"tags" json:"tags,omitempty" jsonschema:"description=Glob patterns matched against the release tags (e.g. c8y-deploy-poll-v*); release then selects the latest or latest-N or all releases of each pattern (e.g. per component of a monorepo with release-please). A {component} placeholder selects per component instead (e.g. {component}-v* gives the latest release of every component). The part of the tag before the first wildcard is removed for the version fallback,example=c8y-deploy-poll-v*,example={component}-v*"`

	// Assets are glob patterns matched against asset filenames (default: ["*"])
	Assets []string `yaml:"assets" json:"assets,omitempty" jsonschema:"description=Glob patterns matched against release asset filenames (default: *)"`

	// IncludePrereleases includes prereleases when Release is "latest" or "all"
	IncludePrereleases bool `yaml:"includePrereleases" json:"includePrereleases,omitempty" jsonschema:"description=Include prereleases when release is latest or all"`

	// LinkOnly references the asset download URL instead of downloading and
	// uploading the binary to Cumulocity. Only useful for public repositories
	// since devices must be able to fetch the URL. When unset, firmware
	// entries link (true) and all other entries upload (false).
	LinkOnly *bool `yaml:"linkOnly" json:"linkOnly,omitempty" jsonschema:"description=Reference the asset download URL instead of downloading and uploading the binary (public repositories only). Default: true for firmware and false for all other entries. Set false on a firmware entry to upload the binary to Cumulocity"`

	// Token for the GitHub API (falls back to GITHUB_TOKEN / GH_TOKEN env vars).
	// Supports ${VAR} expansion, e.g. token: ${MY_GITHUB_TOKEN}
	Token string `yaml:"token" json:"token,omitempty" jsonschema:"description=GitHub API token (falls back to GITHUB_TOKEN / GH_TOKEN env vars; supports ${VAR} expansion); mutually exclusive with tokenFrom"`

	// TokenFrom takes the GitHub API token from an environment variable
	TokenFrom *SecretRef `yaml:"tokenFrom" json:"tokenFrom,omitempty" jsonschema:"description=Take the GitHub API token from an environment variable (env) or a generated value (generate); mutually exclusive with token"`
}

// SoftwareSpec syncs software packages into the software repository.
// Name, version, architecture and software type are parsed from filenames
// (same logic as the software-uploader example).
type SoftwareSpec struct {
	// Name declares a single software version explicitly (with version,
	// type and an optional url) instead of parsing packages from a source
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Software name of an explicit entry: a single version declared with name and version (and optionally type and url) instead of packages from a source; mutually exclusive with source"`

	// URL is the binary url of an explicit entry. Without it, the version
	// is created with an empty url (e.g. for packages which devices install
	// from their own package repositories)
	URL string `yaml:"url" json:"url,omitempty" jsonschema:"description=Binary url of an explicit entry (optional; without it the version has an empty url e.g. for packages which devices install from their package repositories)"`

	// NamePrefix is added to every parsed software name
	NamePrefix string `yaml:"namePrefix" json:"namePrefix,omitempty" jsonschema:"description=Prefix added to every parsed software name"`

	// Type forces the softwareType for all packages from this source, or
	// sets the softwareType of an explicit entry
	Type string `yaml:"type" json:"type,omitempty" jsonschema:"description=Software type: forces the softwareType for all packages from the source (overrides auto-detection) or sets it for an explicit entry,example=apt"`

	// TypeMap maps filename glob patterns to software types, first match wins.
	// Example: ["*.bin=firmware", ".deb=custom-apt"]
	TypeMap []string `yaml:"typeMap" json:"typeMap,omitempty" jsonschema:"description=Map filename glob patterns to software types (pattern=softwaretype; first match wins),example=*.bin=firmware"`

	// Version overrides the parsed version (useful for single-file sources),
	// or is the exact version of an explicit entry
	Version string `yaml:"version" json:"version,omitempty" jsonschema:"description=Exact version of an explicit entry (used as-is e.g. latest) or an override of the parsed version (useful for single-file sources)"`

	Source Source `yaml:"source" json:"source,omitempty" jsonschema:"description=Where the software packages come from; mutually exclusive with name"`
}

// IsExplicit reports whether the entry declares a single version by name
// instead of parsing packages from a source
func (spec SoftwareSpec) IsExplicit() bool {
	return spec.Name != ""
}

// FirmwareSpec syncs firmware (OS images) into the firmware repository.
// Supports common build system outputs (Yocto, Buildroot, Rugix Bakery,
// SWUpdate, RAUC, Mender, ...) plus custom naming via versionPattern.
type FirmwareSpec struct {
	// Name overrides the parsed firmware name. When set, all files from the
	// source are uploaded as versions of this single firmware item.
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Override the parsed firmware name (all files become versions of this single firmware item)"`

	Description string `yaml:"description" json:"description,omitempty" jsonschema:"description=Description of the firmware item"`

	// DeviceType restricts the firmware to a device type (c8y_Filter.type).
	// Supports placeholders derived from each parsed artifact: {name},
	// {version} and {filename}, e.g. "linux-{name}".
	DeviceType string `yaml:"deviceType" json:"deviceType,omitempty" jsonschema:"description=Restrict the firmware to a device type (c8y_Filter.type); supports {name} {version} and {filename} placeholders derived from the artifact filename"`

	// Version overrides the version for all files (only sensible for
	// single-file sources). Defaults to: parsed from filename, then the
	// GitHub release tag.
	Version string `yaml:"version" json:"version,omitempty" jsonschema:"description=Override the version for all files (defaults to the version parsed from the filename and then the GitHub release tag)"`

	// VersionPattern is a regular expression with a single capture group used
	// to extract the version from the filename for custom naming schemes.
	// Example: 'myimage-(\d+\.\d+\.\d+)\.custom$'
	VersionPattern string `yaml:"versionPattern" json:"versionPattern,omitempty" jsonschema:"description=Regular expression with a single capture group to extract the version from the filename for custom naming schemes"`

	// URL is the binary url of an explicit entry (an entry without a source)
	URL string `yaml:"url" json:"url,omitempty" jsonschema:"description=Binary url of an explicit entry (name and version without a source); optional - without it the version has an empty url"`

	Source Source `yaml:"source" json:"source,omitempty" jsonschema:"description=Where the firmware images come from; without a source the entry declares a single version explicitly with name and version (and optionally url)"`
}

// IsExplicit reports whether the entry declares a single version with name
// and version instead of parsing images from a source
func (spec FirmwareSpec) IsExplicit() bool {
	return !spec.Source.IsSet()
}

// ConfigurationSpec syncs configuration files into the configuration repository
type ConfigurationSpec struct {
	// Name of the configuration item (defaults to the filename)
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Name of the configuration item (defaults to the filename without extension)"`

	// ConfigurationType (e.g. "mosquitto.conf", "properties")
	ConfigurationType string `yaml:"configurationType" json:"configurationType" jsonschema:"description=Configuration type,example=mosquitto.conf,example=properties"`

	Description string `yaml:"description" json:"description,omitempty" jsonschema:"description=Description of the configuration item"`

	// DeviceType restricts the configuration to a device type
	DeviceType string `yaml:"deviceType" json:"deviceType,omitempty" jsonschema:"description=Restrict the configuration to a device type"`

	// URL is the file url of an explicit entry (an entry without a source)
	URL string `yaml:"url" json:"url,omitempty" jsonschema:"description=File url of an explicit entry (name without a source); mutually exclusive with content"`

	// Content is inline file content of an explicit entry, uploaded as the
	// configuration file
	Content string `yaml:"content" json:"content,omitempty" jsonschema:"description=Inline file content of an explicit entry (name without a source) which is uploaded as the configuration file; supports ${VAR} expansion and sops encryption; mutually exclusive with url"`

	// Filename is the name of the uploaded file for inline content
	Filename string `yaml:"filename" json:"filename,omitempty" jsonschema:"description=Filename of the uploaded inline content (defaults to the name),example=mosquitto.conf"`

	// Force replaces the configuration item and re-uploads its file on every
	// run (like --force, but only for this entry)
	Force bool `yaml:"force" json:"force,omitempty" jsonschema:"description=Replace the configuration item and re-upload its file on every run (like --force but only for this entry) so changed content is applied"`

	Source Source `yaml:"source" json:"source,omitempty" jsonschema:"description=Where the configuration file comes from; without a source the entry declares a single configuration explicitly with name and url or inline content"`
}

// IsExplicit reports whether the entry declares a single configuration
// (with a url, inline content or neither) instead of files from a source
func (spec ConfigurationSpec) IsExplicit() bool {
	return !spec.Source.IsSet()
}

// UploadFilename is the filename used for inline content
func (spec ConfigurationSpec) UploadFilename() string {
	if spec.Filename != "" {
		return spec.Filename
	}
	return spec.Name
}

// SmartRestTemplateSpec syncs SmartREST 2.0 template collections from JSON
// files as exported by the platform (one collection per file). The collection
// name is taken from the file's "name" field (falling back to "__externalId")
// and doubles as the external identity (X-Id) the upsert matches on. The
// order of the request/response templates inside a file does not matter.
type SmartRestTemplateSpec struct {
	// Name overrides the collection name from the file (only sensible for
	// single-file sources)
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Override the collection name from the file (only sensible for single-file sources)"`

	// Source provides the exported collection JSON files. Directories are
	// scanned for *.json files by default.
	Source Source `yaml:"source" json:"source" jsonschema:"description=Where the exported SmartREST 2.0 collection JSON files come from (directories are scanned for *.json by default)"`
}

// SmartFunctionSpec deploys Data Preparation smart functions (rules) from
// rule packages: folders laid out like the package archive (data-prep.yaml,
// the bundled smart function and tests/*.yaml) or .tar.gz archives exported
// by the Data Preparation application. A rule is only deployed when its code,
// configuration or tests differ from the deployed rule.
type SmartFunctionSpec struct {
	// Name overrides the rule name, which defaults to the package folder
	// name or the archive name without .tar.gz/.tgz
	Name string `yaml:"name" json:"name,omitempty" jsonschema:"description=Rule name (defaults to the package folder name or the archive filename without .tar.gz/.tgz; only sensible for sources with a single package)"`

	// Source provides the packages. Directories are scanned for
	// data-prep.yaml files (each marks a package folder) and .tar.gz/.tgz
	// archives by default.
	Source Source `yaml:"source" json:"source" jsonschema:"description=Where the rule packages come from: a package folder with data-prep.yaml or a folder of package folders and archives or a .tar.gz archive or GitHub release assets (directories are scanned for data-prep.yaml and *.tar.gz/*.tgz by default)"`

	// WaitTimeout is how long to wait for the deployment to finish so
	// deployment errors are reported
	WaitTimeout string `yaml:"waitTimeout" json:"waitTimeout,omitempty" jsonschema:"description=How long to wait for each deployment to finish so engine errors are reported (Go duration; default 60s; 0 does not wait),example=2m"`
}

// DefaultSmartFunctionWaitTimeout is how long a deployment is awaited when
// waitTimeout is not set
const DefaultSmartFunctionWaitTimeout = 60 * time.Second

// waitTimeout returns the parsed wait timeout (validated by Manifest.Validate)
func (spec SmartFunctionSpec) waitTimeout() time.Duration {
	if spec.WaitTimeout == "" {
		return DefaultSmartFunctionWaitTimeout
	}
	d, err := time.ParseDuration(spec.WaitTimeout)
	if err != nil {
		return DefaultSmartFunctionWaitTimeout
	}
	return d
}

// DigitalTwinSpec declares Digital Twin Manager definitions, inline and/or
// from model files. All definitions are applied together in dependency
// order: property definitions first, then asset definitions (a definition
// after the asset definitions its composition allows as sub-assets).
type DigitalTwinSpec struct {
	// ManagedBy is a tag added to every definition the sync writes
	ManagedBy string `yaml:"managedBy" json:"managedBy,omitempty" jsonschema:"description=Tag added to every definition written by the sync; required by prune to find the definitions the manifest owns,example=tenant-sync"`

	// Prune deletes definitions tagged managedBy which the manifest no
	// longer declares
	Prune *bool `yaml:"prune" json:"prune,omitempty" jsonschema:"description=Delete definitions tagged managedBy which are no longer declared (requires managedBy; default false)"`

	// Sources are DTM model files (YAML or JSON documents with a kind field)
	Sources []Source `yaml:"sources" json:"sources,omitempty" jsonschema:"description=Model files with definitions: YAML/JSON documents with a kind field (PropertyDefinition or AssetDefinition or MeasurementDefinition or EventDefinition or AlarmDefinition); directories are scanned for *.yaml and *.yml and *.json by default"`

	PropertyDefinitions []DigitalTwinDefinitionSpec `yaml:"propertyDefinitions" json:"propertyDefinitions,omitempty" jsonschema:"description=Property definitions (matched by identifier and contexts)"`
	AssetDefinitions    []DigitalTwinDefinitionSpec `yaml:"assetDefinitions" json:"assetDefinitions,omitempty" jsonschema:"description=Asset definitions (matched by identifier); their structure is described by composition"`
}

// PruneEnabled reports whether pruning is enabled
func (spec *DigitalTwinSpec) PruneEnabled() bool {
	return spec != nil && spec.Prune != nil && *spec.Prune
}

// DigitalTwinDefinitionSpec is a property or asset definition
type DigitalTwinDefinitionSpec struct {
	Identifier  string                      `yaml:"identifier" json:"identifier" jsonschema:"description=Unique identifier of the definition (also the fragment name of a property),example=maxFlow"`
	Contexts    []string                    `yaml:"contexts" json:"contexts,omitempty" jsonschema:"description=Domain entities a property definition applies to (property definitions only),example=asset"`
	Tags        []string                    `yaml:"tags" json:"tags,omitempty" jsonschema:"description=Tags of the definition (compared as a set)"`
	JSONSchema  map[string]any              `yaml:"jsonSchema" json:"jsonSchema,omitempty" jsonschema:"description=JSON schema of the definition (e.g. title and type); fields generated by the server are kept"`
	Composition *DigitalTwinCompositionSpec `yaml:"composition" json:"composition,omitempty" jsonschema:"description=Properties and sub-assets the asset definition allows (asset definitions only)"`
	Fragments   map[string]any              `yaml:"fragments" json:"fragments,omitempty" jsonschema:"description=Additional custom fragments of the definition"`
}

// DigitalTwinCompositionSpec lists the properties and sub-assets an asset
// definition allows
type DigitalTwinCompositionSpec struct {
	AllowedProperties    []DigitalTwinAllowedItem `yaml:"allowedProperties" json:"allowedProperties,omitempty" jsonschema:"description=Property definitions an asset of this type can have"`
	AllowedSubAssets     []DigitalTwinAllowedItem `yaml:"allowedSubAssets" json:"allowedSubAssets,omitempty" jsonschema:"description=Asset definitions allowed as sub-assets"`
	AdditionalProperties *bool                    `yaml:"additionalProperties" json:"additionalProperties,omitempty" jsonschema:"description=Allow properties which are not listed"`
	AdditionalSubAssets  *bool                    `yaml:"additionalSubAssets" json:"additionalSubAssets,omitempty" jsonschema:"description=Allow sub-assets which are not listed"`
}

// DigitalTwinAllowedItem references a property or asset definition
type DigitalTwinAllowedItem struct {
	Identifier string `yaml:"identifier" json:"identifier" jsonschema:"description=Identifier of the property or asset definition"`
	MinOccurs  string `yaml:"minOccurs" json:"minOccurs,omitempty" jsonschema:"description=Minimum occurrences (server default when empty),example=1"`
	MaxOccurs  string `yaml:"maxOccurs" json:"maxOccurs,omitempty" jsonschema:"description=Maximum occurrences; * for unbounded (server default when empty),example=*"`
}

// Key identifies the definition within its kind: the identifier and, for
// property definitions, the sorted contexts
func (d DigitalTwinDefinitionSpec) Key() string {
	if d.Identifier == "" {
		return ""
	}
	if len(d.Contexts) == 0 {
		return d.Identifier
	}
	contexts := slices.Clone(d.Contexts)
	slices.Sort(contexts)
	return d.Identifier + "[" + strings.Join(contexts, ",") + "]"
}

// DeviceProfileSpec creates a device profile referencing firmware, software
// and configuration already present in the tenant (typically synced by the
// earlier sections of the same manifest).
type DeviceProfileSpec struct {
	Name       string `yaml:"name" json:"name" jsonschema:"description=Name of the device profile"`
	DeviceType string `yaml:"deviceType" json:"deviceType,omitempty" jsonschema:"description=Restrict the profile to a device type (c8y_Filter.type)"`

	Firmware      *ProfileFirmwareRef       `yaml:"firmware" json:"firmware,omitempty" jsonschema:"description=Firmware version included in the profile"`
	Software      []ProfileSoftwareRef      `yaml:"software" json:"software,omitempty" jsonschema:"description=Software versions included in the profile"`
	Configuration []ProfileConfigurationRef `yaml:"configuration" json:"configuration,omitempty" jsonschema:"description=Configuration items included in the profile"`
}

// Profile references resolve their binary URL automatically from the
// repository item in the tenant (which is why the deviceProfiles section runs
// after the repository sections), or take an explicit url that skips the
// lookup entirely (e.g. for externally hosted binaries). The special url
// values "-" and "none" disable the lookup AND leave the url off the profile
// entry, for devices that resolve artifacts by name/version themselves.

type ProfileFirmwareRef struct {
	Name    string `yaml:"name" json:"name" jsonschema:"description=Firmware name (must exist in the tenant unless url is set)"`
	Version string `yaml:"version" json:"version,omitempty" jsonschema:"description=Exact firmware version (must exist in the tenant unless url is set); mutually exclusive with versionSelector"`

	// VersionSelector selects the version by semver when the manifest is applied
	VersionSelector string `yaml:"versionSelector" json:"versionSelector,omitempty" jsonschema:"description=Select the version from the versions in the tenant by semver: latest or latest-N (e.g. latest-1 for the second latest) or a semver constraint selecting the highest matching version (e.g. ^1.2 or >=1.0 <2.0); mutually exclusive with version,example=latest,example=latest-1,example=^1.2"`

	// IncludePrereleases considers prerelease versions (e.g. 1.2.0-rc1) for
	// versionSelector
	IncludePrereleases bool   `yaml:"includePrereleases" json:"includePrereleases,omitempty" jsonschema:"description=Consider prerelease versions (e.g. 1.2.0-rc1) for versionSelector"`
	URL                string `yaml:"url" json:"url,omitempty" jsonschema:"description=Explicit binary URL skipping the lookup of the firmware version in the tenant; the special values - and none omit the url from the profile entirely,example=none"`
}

type ProfileSoftwareRef struct {
	Name    string `yaml:"name" json:"name" jsonschema:"description=Software name (must exist in the tenant unless url is set)"`
	Version string `yaml:"version" json:"version,omitempty" jsonschema:"description=Exact software version (must exist in the tenant unless url is set); mutually exclusive with versionSelector"`

	// VersionSelector selects the version by semver when the manifest is applied
	VersionSelector string `yaml:"versionSelector" json:"versionSelector,omitempty" jsonschema:"description=Select the version from the versions in the tenant by semver: latest or latest-N (e.g. latest-1 for the second latest) or a semver constraint selecting the highest matching version (e.g. ^1.2 or >=1.0 <2.0); mutually exclusive with version,example=latest,example=latest-1,example=^1.2"`

	// IncludePrereleases considers prerelease versions (e.g. 1.2.0-rc1) for
	// versionSelector
	IncludePrereleases bool   `yaml:"includePrereleases" json:"includePrereleases,omitempty" jsonschema:"description=Consider prerelease versions (e.g. 1.2.0-rc1) for versionSelector"`
	Type               string `yaml:"type" json:"type,omitempty" jsonschema:"description=Software type of the referenced item (disambiguates the lookup and is written to the profile entry),example=apt"`
	URL                string `yaml:"url" json:"url,omitempty" jsonschema:"description=Explicit binary URL skipping the lookup of the software version in the tenant; the special values - and none omit the url from the profile entirely,example=none"`
	Action             string `yaml:"action" json:"action,omitempty" jsonschema:"description=Software action (defaults to install),example=install"` // defaults to "install"
}

type ProfileConfigurationRef struct {
	Name string `yaml:"name" json:"name" jsonschema:"description=Configuration item name (must exist in the tenant unless url is set)"`
	Type string `yaml:"type" json:"type,omitempty" jsonschema:"description=Configuration type of the referenced item"`
	URL  string `yaml:"url" json:"url,omitempty" jsonschema:"description=Explicit file URL skipping the lookup of the configuration item in the tenant; the special values - and none omit the url from the profile entirely,example=none"`
}

// escapedDollarPlaceholder temporarily replaces "$$" during expansion. It
// contains a NUL byte, which can not appear in a YAML document
const escapedDollarPlaceholder = "\x00tenant-sync-dollar\x00"

// expandEnv expands ${VAR} and $VAR environment variable references. Unknown
// references are left in place (as ${VAR}) so they are easy to spot in
// errors. "$$" is an escaped literal "$".
func expandEnv(raw string) string {
	expanded, _ := expandManifest(raw, nil)
	return expanded
}

// expandManifest expands environment variables and ${generate:NAME[:LENGTH]}
// references. Generated values come from the secrets store; without a store
// the references are only checked and left in place.
func expandManifest(raw string, secrets *SecretStore) (string, error) {
	var errs []string
	escaped := strings.ReplaceAll(raw, "$$", escapedDollarPlaceholder)
	expanded := os.Expand(escaped, func(name string) string {
		if strings.HasPrefix(name, generatePrefix) {
			if _, err := parseGenerateRef(strings.TrimPrefix(name, generatePrefix)); err != nil {
				errs = append(errs, err.Error())
				return "${" + name + "}"
			}
			if secrets == nil {
				return "${" + name + "}"
			}
			// Replaced in the YAML values below, so references in comments
			// don't generate values
			return generateMarkerStart + strings.TrimPrefix(name, generatePrefix) + generateMarkerEnd
		}
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return "${" + name + "}"
	})
	expanded = strings.ReplaceAll(expanded, escapedDollarPlaceholder, "$")
	if len(errs) > 0 {
		return expanded, fmt.Errorf("invalid manifest:\n  - %s", strings.Join(errs, "\n  - "))
	}
	if secrets != nil && strings.Contains(expanded, generateMarkerStart) {
		return generateInValues(expanded, secrets)
	}
	return expanded, nil
}

// Markers of ${generate:...} references between the text expansion and the
// replacement in the YAML values (Unicode private use characters, which are
// valid in YAML but don't appear in manifests)
const (
	generateMarkerStart = "\uE000"
	generateMarkerEnd   = "\uE001"
)

var generateMarkerPattern = regexp.MustCompile(generateMarkerStart + `([^` + generateMarkerEnd + `]*)` + generateMarkerEnd)

// generateInValues replaces the generate markers in the YAML values (not in
// comments) with the generated values
func generateInValues(text string, secrets *SecretStore) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", fmt.Errorf("failed to parse manifest: %w", err)
	}
	var walkErr error
	var walk func(node *yaml.Node)
	walk = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, generateMarkerStart) {
			node.Value = generateMarkerPattern.ReplaceAllStringFunc(node.Value, func(marker string) string {
				ref, err := parseGenerateRef(generateMarkerPattern.FindStringSubmatch(marker)[1])
				if err == nil {
					var value string
					if value, err = secrets.Value(ref); err == nil {
						return value
					}
				}
				if walkErr == nil {
					walkErr = err
				}
				return marker
			})
			// The value is a string, also if it looks like another type
			node.Tag = "!!str"
			node.Style = 0
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(&doc)
	if walkErr != nil {
		return "", walkErr
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// LoadOptions are options of LoadManifestWith
type LoadOptions struct {
	// Secrets provides the values of ${generate:NAME} references. Without
	// it, the references are only checked and left in place
	Secrets *SecretStore
}

// escapeEnv escapes "$" as "$$", so the value is not expanded when the
// manifest is loaded
func escapeEnv(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

// applyDefaults fills in values which are derived from other fields
func (m *Manifest) applyDefaults() {
	for i := range m.Users {
		// The email is a valid username, so a user can be declared by email
		if m.Users[i].Username == "" {
			m.Users[i].Username = strings.TrimSpace(m.Users[i].Email)
		}
	}
}

// isEmailAddress reports whether the value is a plain email address (without
// a display name), e.g. jdoe@example.com
func isEmailAddress(value string) bool {
	if value == "" || strings.ContainsAny(value, " <>") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

// CreateEmail is the email used when the user is created: the email, or the
// userName when it is an email address. It is not used to update existing
// users, so their email is never replaced by their username.
func (u UserSpec) CreateEmail() string {
	if u.Email != "" {
		return u.Email
	}
	if isEmailAddress(u.Username) {
		return u.Username
	}
	return ""
}

// validateExplicitSoftware checks an explicit software entry (name, version,
// type and an optional url instead of a source)
func validateExplicitSoftware(label string, spec SoftwareSpec) []string {
	var errs []string
	if spec.Source.IsSet() {
		errs = append(errs, label+": name and source are mutually exclusive (name declares a single version without a source)")
	}
	if strings.TrimSpace(spec.Version) == "" {
		errs = append(errs, label+": version is required for an explicit entry (name)")
	}
	if spec.NamePrefix != "" || len(spec.TypeMap) > 0 {
		errs = append(errs, label+": namePrefix and typeMap only apply to entries with a source")
	}
	if err := validateAbsoluteURL(spec.URL); err != nil {
		errs = append(errs, fmt.Sprintf("%s: %v", label, err))
	}
	return errs
}

// validateExplicitFirmware checks an explicit firmware entry (name,
// version and an optional url instead of a source)
func validateExplicitFirmware(label string, spec FirmwareSpec) []string {
	var errs []string
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Version) == "" {
		errs = append(errs, label+": source, or name and version (an explicit entry), is required")
	}
	if spec.VersionPattern != "" {
		errs = append(errs, label+": versionPattern only applies to entries with a source")
	}
	if err := validateAbsoluteURL(spec.URL); err != nil {
		errs = append(errs, fmt.Sprintf("%s: %v", label, err))
	}
	if err := validatePlaceholders(spec.DeviceType); err != nil {
		errs = append(errs, fmt.Sprintf("%s.deviceType: %v", label, err))
	}
	return errs
}

// validateExplicitConfiguration checks an explicit configuration entry
// (name with a url, inline content or neither, instead of a source)
func validateExplicitConfiguration(label string, spec ConfigurationSpec) []string {
	var errs []string
	if strings.TrimSpace(spec.Name) == "" {
		errs = append(errs, label+": source, or name (an explicit entry), is required")
	}
	if spec.URL != "" && spec.Content != "" {
		errs = append(errs, label+": url and content are mutually exclusive")
	}
	if spec.Filename != "" && spec.Content == "" {
		errs = append(errs, label+": filename only applies to inline content")
	}
	if strings.ContainsAny(spec.Filename, `/\`) {
		errs = append(errs, fmt.Sprintf("%s: filename %q must not contain a path", label, spec.Filename))
	}
	if err := validateAbsoluteURL(spec.URL); err != nil {
		errs = append(errs, fmt.Sprintf("%s: %v", label, err))
	}
	return errs
}

// validateDeviceGroups checks a level of the device group tree: names are
// required and unique among siblings, and fragments must not set reserved
// properties
func validateDeviceGroups(label string, groups []DeviceGroupSpec) []string {
	var errs []string
	names := make(map[string]bool, len(groups))
	for i, group := range groups {
		itemLabel := fmt.Sprintf("%s[%d]", label, i)
		name := strings.TrimSpace(group.Name)
		switch {
		case name == "":
			errs = append(errs, itemLabel+": name is required")
		case name != group.Name:
			errs = append(errs, fmt.Sprintf("%s: name %q must not start or end with spaces", itemLabel, group.Name))
		case names[name]:
			errs = append(errs, fmt.Sprintf("%s: duplicate group name %q (names must be unique within the parent)", itemLabel, name))
		}
		names[name] = true
		for _, key := range sortedKeys(group.Fragments) {
			if slices.Contains(reservedDeviceGroupFragments, key) {
				errs = append(errs, fmt.Sprintf("%s.fragments: %q can not be set as a fragment", itemLabel, key))
			}
		}
		errs = append(errs, validateDeviceGroups(itemLabel+".groups", group.Groups)...)
	}
	return errs
}

// validateAbsoluteURL checks that an optional url is absolute
func validateAbsoluteURL(value string) error {
	if value == "" {
		return nil
	}
	if parsed, err := url.Parse(value); err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("url %q is not an absolute url", value)
	}
	return nil
}

// validateVersionRef checks the version fields of a device profile
// firmware/software reference: exactly one of version and versionSelector,
// a valid selector, and includePrereleases only with a selector
func validateVersionRef(label, version, selector string, includePrereleases bool) []string {
	var errs []string
	switch {
	case version == "" && selector == "":
		errs = append(errs, label+": version or versionSelector is required")
	case version != "" && selector != "":
		errs = append(errs, label+": version and versionSelector are mutually exclusive")
	case selector != "":
		if _, err := ParseVersionSelector(selector); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
		}
	}
	if includePrereleases && selector == "" {
		errs = append(errs, label+": includePrereleases requires versionSelector")
	}
	return errs
}

// LoadManifest reads a manifest file (decrypting it with sops when it is
// encrypted), expanding ${VAR} environment variable
// references before parsing so secrets (e.g. GitHub tokens) can be injected.
func LoadManifest(path string) (*Manifest, error) {
	return LoadManifestWith(path, LoadOptions{})
}

// LoadManifestWith reads a manifest like LoadManifest, with generated values
// from opts.Secrets
func LoadManifestWith(path string, opts LoadOptions) (*Manifest, error) {
	return LoadManifestsWith([]string{path}, opts)
}

// LoadManifestsWith reads one or more manifests and merges them in order,
// like files included in that order (later files win). Relative paths are
// resolved against the directory of the first manifest.
func LoadManifestsWith(paths []string, opts LoadOptions) (*Manifest, error) {
	if len(paths) == 0 {
		return nil, errors.New("no manifest file given")
	}
	abs, err := filepath.Abs(paths[0])
	if err != nil {
		return nil, err
	}
	loader := &manifestLoader{
		opts:     opts,
		rootDir:  filepath.Dir(abs),
		rootFile: abs,
		loaded:   map[string]bool{},
	}
	manifest, err := loader.loadAll(paths)
	if err != nil {
		return nil, err
	}
	manifest.IncludedFiles = loader.files
	return manifest, nil
}

// Validate checks the manifest for structural errors before any API calls
func (m *Manifest) Validate() error {
	var errs []string

	if err := m.Targets.Validate(); err != nil {
		errs = append(errs, fmt.Sprintf("targets: %v", err))
	}

	errs = append(errs, validateChildTenants(m.Tenants)...)

	if m.CertificateAuthority != nil && m.CertificateAuthority.Enabled == nil {
		errs = append(errs, "certificateAuthority.enabled is required")
	}

	for i, opt := range m.TenantOptions {
		if opt.Category == "" || opt.Key == "" {
			errs = append(errs, fmt.Sprintf("tenantOptions[%d]: category and key are required", i))
		}
		if opt.Value != "" && opt.ValueFrom != nil {
			errs = append(errs, fmt.Sprintf("tenantOptions[%d]: value and valueFrom are mutually exclusive", i))
		}
		if opt.Value == "" && opt.ValueFrom == nil {
			errs = append(errs, fmt.Sprintf("tenantOptions[%d]: one of value or valueFrom is required", i))
		}
		if opt.ValueFrom != nil {
			refs := 0
			for _, set := range []bool{opt.ValueFrom.Application != "", opt.ValueFrom.Device != "", opt.ValueFrom.Env != "", opt.ValueFrom.Generate != ""} {
				if set {
					refs++
				}
			}
			if refs != 1 {
				errs = append(errs, fmt.Sprintf("tenantOptions[%d]: valueFrom requires exactly one of: application, device, env, generate", i))
			} else if opt.ValueFrom.Env != "" || opt.ValueFrom.Generate != "" {
				errs = append(errs, SecretRef{Env: opt.ValueFrom.Env, Generate: opt.ValueFrom.Generate}.validate(fmt.Sprintf("tenantOptions[%d].valueFrom", i))...)
			}
		}
	}
	for i, feature := range m.Features {
		if feature.Key == "" {
			errs = append(errs, fmt.Sprintf("features[%d]: key is required", i))
		}
	}
	selectors := make(map[string]bool, len(m.RetentionRules))
	for i, rule := range m.RetentionRules {
		if rule.DataType == "" {
			errs = append(errs, fmt.Sprintf("retentionRules[%d]: dataType is required", i))
		} else if !slices.Contains(RetentionRuleDataTypes, rule.DataType) {
			errs = append(errs, fmt.Sprintf("retentionRules[%d]: unknown dataType %q: supported values are %s", i, rule.DataType, strings.Join(RetentionRuleDataTypes, ", ")))
		}
		if rule.MaximumAge < 1 {
			errs = append(errs, fmt.Sprintf("retentionRules[%d]: maximumAge must be at least 1 (days)", i))
		}
		if selectors[rule.Selector()] {
			errs = append(errs, fmt.Sprintf("retentionRules[%d]: duplicate rule selector %s", i, rule.Selector()))
		}
		selectors[rule.Selector()] = true
	}
	for i, cert := range m.TrustedCertificates {
		if err := cert.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("trustedCertificates[%d]: %v", i, err))
		}
		if cert.Source.URL != "" {
			errs = append(errs, fmt.Sprintf("trustedCertificates[%d]: url sources are not supported (the certificate content must be available locally)", i))
		}
		if cert.Status != "" && !slices.Contains(TrustedCertificateStatuses, cert.Status) {
			errs = append(errs, fmt.Sprintf("trustedCertificates[%d]: unknown status %q: supported values are %s", i, cert.Status, strings.Join(TrustedCertificateStatuses, ", ")))
		}
	}
	for i, crl := range m.CertificateRevocationLists {
		if err := crl.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("certificateRevocationLists[%d]: %v", i, err))
		}
		if crl.Source.URL != "" {
			errs = append(errs, fmt.Sprintf("certificateRevocationLists[%d]: url sources are not supported (the CSV content must be available locally)", i))
		}
	}
	for i, app := range m.Applications {
		if app.Name == "" {
			errs = append(errs, fmt.Sprintf("applications[%d]: name is required", i))
		}
		if app.Source != nil {
			if err := app.Source.Validate(); err != nil {
				errs = append(errs, fmt.Sprintf("applications[%d]: %v", i, err))
			}
		}
	}
	errs = append(errs, validateDeviceGroups("deviceGroups", m.DeviceGroups)...)
	groupNames := make(map[string]bool, len(m.UserGroups))
	for i, group := range m.UserGroups {
		if group.Name == "" {
			errs = append(errs, fmt.Sprintf("userGroups[%d]: name is required", i))
		} else if groupNames[group.Name] {
			errs = append(errs, fmt.Sprintf("userGroups[%d]: duplicate group name %q", i, group.Name))
		}
		groupNames[group.Name] = true
		for j, role := range group.Roles {
			if strings.TrimSpace(role) == "" {
				errs = append(errs, fmt.Sprintf("userGroups[%d].roles[%d]: role must not be empty", i, j))
			}
		}
	}
	roleNames := make(map[string]bool, len(m.InventoryRoles))
	for i, role := range m.InventoryRoles {
		if strings.TrimSpace(role.Name) == "" {
			errs = append(errs, fmt.Sprintf("inventoryRoles[%d]: name is required", i))
		} else if roleNames[role.Name] {
			errs = append(errs, fmt.Sprintf("inventoryRoles[%d]: duplicate inventory role name %q", i, role.Name))
		}
		roleNames[role.Name] = true
		permissionKeys := make(map[string]bool, len(role.Permissions))
		for j, permission := range role.Permissions {
			prefix := fmt.Sprintf("inventoryRoles[%d].permissions[%d]", i, j)
			if permission.Scope == "" {
				errs = append(errs, prefix+": scope is required")
			} else if !slices.Contains(InventoryRoleScopes, permission.Scope) {
				errs = append(errs, fmt.Sprintf("%s: unknown scope %q: supported values are %s", prefix, permission.Scope, strings.Join(InventoryRoleScopes, ", ")))
			}
			if permission.Permission == "" {
				errs = append(errs, prefix+": permission is required")
			} else if !slices.Contains(InventoryRolePermissionLevels, permission.Permission) {
				errs = append(errs, fmt.Sprintf("%s: unknown permission %q: supported values are %s", prefix, permission.Permission, strings.Join(InventoryRolePermissionLevels, ", ")))
			}
			if permissionKeys[permission.Key()] {
				errs = append(errs, fmt.Sprintf("%s: duplicate permission %s", prefix, permission.Key()))
			}
			permissionKeys[permission.Key()] = true
		}
	}
	usernames := make(map[string]bool, len(m.Users))
	for i, user := range m.Users {
		if user.PasswordFrom != nil {
			if user.Password != "" {
				errs = append(errs, fmt.Sprintf("users[%d]: password and passwordFrom are mutually exclusive", i))
			}
			errs = append(errs, user.PasswordFrom.validate(fmt.Sprintf("users[%d].passwordFrom", i))...)
		}
		if user.Username == "" {
			errs = append(errs, fmt.Sprintf("users[%d]: userName or email is required", i))
		} else if usernames[user.Username] {
			errs = append(errs, fmt.Sprintf("users[%d]: duplicate userName %q", i, user.Username))
		}
		usernames[user.Username] = true
		if user.SendPasswordResetEmail && user.CreateEmail() == "" {
			errs = append(errs, fmt.Sprintf("users[%d]: sendPasswordResetEmail requires email (or an email address as userName)", i))
		}
		for j, group := range user.Groups {
			if strings.TrimSpace(group) == "" {
				errs = append(errs, fmt.Sprintf("users[%d].groups[%d]: group must not be empty", i, j))
			}
		}
		assignedGroups := make(map[string]bool, len(user.InventoryAssignments))
		for j, assignment := range user.InventoryAssignments {
			prefix := fmt.Sprintf("users[%d].inventoryAssignments[%d]", i, j)
			group := strings.TrimSpace(assignment.Group)
			if group == "" {
				errs = append(errs, prefix+": group is required")
			} else if slices.Contains(splitGroupPath(group), "") {
				errs = append(errs, fmt.Sprintf("%s: invalid group path %q: empty group name", prefix, assignment.Group))
			} else if assignedGroups[group] {
				errs = append(errs, fmt.Sprintf("%s: duplicate group %q (list all roles of a group in one entry)", prefix, assignment.Group))
			}
			assignedGroups[group] = true
			if len(assignment.Roles) == 0 {
				errs = append(errs, prefix+": at least one role is required")
			}
			for k, role := range assignment.Roles {
				if strings.TrimSpace(role) == "" {
					errs = append(errs, fmt.Sprintf("%s.roles[%d]: role must not be empty", prefix, k))
				}
			}
		}
	}
	for i, sw := range m.Software {
		if sw.IsExplicit() {
			errs = append(errs, validateExplicitSoftware(fmt.Sprintf("software[%d]", i), sw)...)
			continue
		}
		if sw.URL != "" {
			errs = append(errs, fmt.Sprintf("software[%d]: url requires name (for a source, use source.url)", i))
		}
		if err := sw.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("software[%d]: %v", i, err))
		}
		for _, mapping := range sw.TypeMap {
			if !strings.Contains(mapping, "=") {
				errs = append(errs, fmt.Sprintf("software[%d]: invalid typeMap entry %q: expected pattern=softwaretype", i, mapping))
			}
		}
	}
	for i, fw := range m.Firmware {
		if fw.IsExplicit() {
			errs = append(errs, validateExplicitFirmware(fmt.Sprintf("firmware[%d]", i), fw)...)
			continue
		}
		if fw.URL != "" {
			errs = append(errs, fmt.Sprintf("firmware[%d]: url requires an entry without a source (for a source, use source.url)", i))
		}
		if err := fw.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("firmware[%d]: %v", i, err))
		}
		if fw.Source.URL != "" && fw.Name == "" {
			errs = append(errs, fmt.Sprintf("firmware[%d]: name is required when using a url source", i))
		}
		if err := validatePlaceholders(fw.DeviceType); err != nil {
			errs = append(errs, fmt.Sprintf("firmware[%d].deviceType: %v", i, err))
		}
	}
	for i, cfg := range m.Configuration {
		label := fmt.Sprintf("configuration[%d]", i)
		if cfg.IsExplicit() {
			errs = append(errs, validateExplicitConfiguration(label, cfg)...)
		} else {
			if cfg.URL != "" || cfg.Content != "" || cfg.Filename != "" {
				errs = append(errs, label+": url, content and filename require an entry without a source (for a source, use source.url)")
			}
			if err := cfg.Source.Validate(); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			}
		}
		if cfg.ConfigurationType == "" {
			errs = append(errs, fmt.Sprintf("configuration[%d]: configurationType is required", i))
		}
	}
	for i, spec := range m.SmartRestTemplates {
		if err := spec.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("smartrestTemplates[%d]: %v", i, err))
		}
		if spec.Source.URL != "" {
			errs = append(errs, fmt.Sprintf("smartrestTemplates[%d]: url sources are not supported (the collection file content must be available locally)", i))
		}
	}
	errs = append(errs, validateSmartFunctions(m.SmartFunctions)...)
	errs = append(errs, m.DigitalTwin.validate()...)
	for i, profile := range m.DeviceProfiles {
		if profile.Name == "" {
			errs = append(errs, fmt.Sprintf("deviceProfiles[%d]: name is required", i))
		}
		if profile.Firmware != nil {
			label := fmt.Sprintf("deviceProfiles[%d].firmware", i)
			if profile.Firmware.Name == "" {
				errs = append(errs, label+": name is required")
			}
			errs = append(errs, validateVersionRef(label, profile.Firmware.Version, profile.Firmware.VersionSelector, profile.Firmware.IncludePrereleases)...)
		}
		for j, sw := range profile.Software {
			label := fmt.Sprintf("deviceProfiles[%d].software[%d]", i, j)
			if sw.Name == "" {
				errs = append(errs, label+": name is required")
			}
			errs = append(errs, validateVersionRef(label, sw.Version, sw.VersionSelector, sw.IncludePrereleases)...)
		}
		for j, cfg := range profile.Configuration {
			if cfg.Name == "" {
				errs = append(errs, fmt.Sprintf("deviceProfiles[%d].configuration[%d]: name is required", i, j))
			}
		}
	}

	names := make(map[string]bool, len(m.Commands))
	for i, group := range m.Commands {
		if group.Name == "" {
			errs = append(errs, fmt.Sprintf("commands[%d]: name is required", i))
		} else if names[group.Name] {
			errs = append(errs, fmt.Sprintf("commands[%d]: duplicate group name %q", i, group.Name))
		}
		names[group.Name] = true
		if len(group.Actions) == 0 {
			errs = append(errs, fmt.Sprintf("commands[%d]: at least one action is required", i))
		}
		for j, action := range group.Actions {
			if strings.TrimSpace(action) == "" {
				errs = append(errs, fmt.Sprintf("commands[%d].actions[%d]: action must not be empty", i, j))
			}
		}
	}

	hookStages := map[string][]HookSpec{"pre": m.Hooks.Pre, "post": m.Hooks.Post}
	for section, sectionHooks := range m.Hooks.Sections {
		if !isKnownSection(section) {
			errs = append(errs, fmt.Sprintf("hooks.sections.%s: unknown section. Valid sections: %s", section, strings.Join(SectionOrder, ", ")))
		}
		hookStages["sections."+section+".pre"] = sectionHooks.Pre
		hookStages["sections."+section+".post"] = sectionHooks.Post
	}
	for stage, hooks := range hookStages {
		for i, hook := range hooks {
			if hook.Run == "" {
				errs = append(errs, fmt.Sprintf("hooks.%s[%d]: run is required", stage, i))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid manifest:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// validateChildTenants checks the tenants section. Requirements which only
// apply when a tenant is created (company, an admin password option) are
// checked when the tenant turns out to be missing, so existing tenants can be
// declared with fewer fields.
func validateChildTenants(specs []ChildTenantSpec) []string {
	var errs []string
	domains := make(map[string]bool, len(specs))
	ids := make(map[string]bool, len(specs))
	for i, spec := range specs {
		label := fmt.Sprintf("tenants[%d]", i)
		domain := strings.ToLower(strings.TrimSpace(spec.Domain))
		switch {
		case domain == "":
			errs = append(errs, label+": domain is required")
		case strings.ContainsAny(domain, " /:"):
			errs = append(errs, fmt.Sprintf("%s: domain %q must be a domain name (without scheme, port or path)", label, spec.Domain))
		case domains[domain]:
			errs = append(errs, fmt.Sprintf("%s: duplicate domain %q", label, spec.Domain))
		}
		domains[domain] = true

		if id := spec.ID; id != "" {
			switch {
			case strings.TrimSpace(id) != id || strings.ContainsAny(id, " \t/.:"):
				errs = append(errs, fmt.Sprintf("%s: id %q must not contain spaces, dots, colons or slashes", label, id))
			case ids[strings.ToLower(id)]:
				errs = append(errs, fmt.Sprintf("%s: duplicate id %q", label, id))
			}
			ids[strings.ToLower(id)] = true
		}

		options := 0
		for _, set := range []bool{spec.AdminPass != "", spec.AdminPasswordFrom != nil, spec.SendPasswordResetEmail} {
			if set {
				options++
			}
		}
		if options > 1 {
			errs = append(errs, label+": adminPass, adminPasswordFrom and sendPasswordResetEmail are mutually exclusive")
		}
		if spec.AdminPasswordFrom != nil {
			errs = append(errs, spec.AdminPasswordFrom.validate(label+".adminPasswordFrom")...)
		}
		if spec.SendPasswordResetEmail && strings.TrimSpace(spec.AdminEmail) == "" {
			errs = append(errs, label+": sendPasswordResetEmail requires adminEmail")
		}
		for j, app := range spec.Applications {
			if strings.TrimSpace(app) == "" {
				errs = append(errs, fmt.Sprintf("%s.applications[%d]: application name must not be empty", label, j))
			}
		}
	}
	return errs
}

// placeholderPattern matches {placeholder} references in template strings
var placeholderPattern = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// validatePlaceholders checks a template string only references the
// placeholders derived from a parsed firmware artifact
func validatePlaceholders(template string) error {
	for _, match := range placeholderPattern.FindAllStringSubmatch(template, -1) {
		switch match[1] {
		case "name", "version", "filename":
		default:
			return fmt.Errorf("unknown placeholder {%s}: supported placeholders are {name}, {version} and {filename}", match[1])
		}
	}
	return nil
}

// TypeMappings converts the string typeMap entries into parser TypeMapping values
func (s *SoftwareSpec) TypeMappings() []TypeMapping {
	mappings := make([]TypeMapping, 0, len(s.TypeMap))
	for _, entry := range s.TypeMap {
		idx := strings.LastIndex(entry, "=")
		if idx <= 0 || idx == len(entry)-1 {
			continue // validated in Manifest.Validate
		}
		mappings = append(mappings, TypeMapping{
			Pattern:      entry[:idx],
			SoftwareType: entry[idx+1:],
		})
	}
	return mappings
}
