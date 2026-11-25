package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/applications"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/retentionrules"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/currenttenant"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/tenantoptions"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/trustedcertificates/certificateauthority"
	inventoryroles "github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/userroles/inventory"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ExportSections are the manifest sections which export can read from a tenant
var ExportSections = []string{
	SectionTenantOptions,
	SectionFeatures,
	SectionRetentionRules,
	SectionCertificateAuthority,
	SectionMicroservices,
	SectionApplications,
	SectionDeviceGroups,
	SectionInventoryRoles,
	SectionDigitalTwin,
	SectionSmartFunctions,
}

// exportOptions holds the flags of the export command
type exportOptions struct {
	output      string
	only        []string
	allFeatures bool
	force       bool
	verbose     bool
	debug       bool
}

func newExportCmd() *cobra.Command {
	opts := &exportOptions{}
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Create a manifest from the current state of a tenant",
		Long: `Read the tenant of the active go-c8y-cli session and write a manifest which describes it.

Exported sections: ` + strings.Join(ExportSections, ", ") + `.

- tenantOptions: all options except credentials (credentials.* keys are never exported)
- features: feature toggles set for the tenant (use --all-features for all toggles)
- retentionRules: all retention rules
- certificateAuthority: the state of the Cumulocity CA, if the tenant has one
- microservices: microservices the tenant is subscribed to but does not own (such as the
  built-in microservices)
- applications: web applications the tenant is subscribed to but does not own (such as the
  built-in Cockpit and Device Management applications)
- deviceGroups: the tree of device groups with name and description (other fragments
  and the devices in the groups are not exported)
- inventoryRoles: all inventory roles with their permissions (users' inventory assignments
  are not exported)
- digitalTwin: the tenant's own property and asset definitions (library definitions and
  definitions shared from the enterprise tenant are not exported)
- smartFunctions: the deployed Data Preparation rules as inline rules with their code and
  tests (drafts and the AI chat history are not exported)

Review the manifest before applying it to other tenants.`,
		Example: formatExamples(
			example{"export", "Print a manifest of the current tenant"},
			example{"export -o tenant.yaml", "Write the manifest of the current tenant to a file"},
			example{"export --only tenantOptions,features", "Export only the tenant options and feature toggles"},
			example{"export --only microservices,applications", "Export only the subscribed microservices and web applications"},
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&opts.output, "output", "o", "", "Write the manifest to a file instead of stdout")
	flags.StringSliceVar(&opts.only, "only", nil, "Comma-separated list of sections to export (default: all). Sections: "+strings.Join(ExportSections, ", "))
	// --sections is the earlier name of --only
	flags.StringSliceVar(&opts.only, "sections", nil, "Comma-separated list of sections to export")
	_ = flags.MarkDeprecated("sections", "use --only instead")
	flags.BoolVar(&opts.allFeatures, "all-features", false, "Export all feature toggles, not only the ones set for the tenant")
	flags.BoolVar(&opts.force, "force", false, "Overwrite the output file if it already exists")
	flags.BoolVar(&opts.verbose, "verbose", false, "Enable detailed logging")
	flags.BoolVar(&opts.debug, "debug", false, "Enable debug mode (verbose logging + HTTP debug)")

	_ = cmd.RegisterFlagCompletionFunc("only", cobra.FixedCompletions(ExportSections, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("output", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
	})
	return cmd
}

func runExport(ctx context.Context, opts *exportOptions, stdout io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	configureLogging(opts.verbose, opts.debug)

	for _, section := range opts.only {
		if !isExportSection(section) {
			return fmt.Errorf("unknown section %q. Valid sections: %s", strings.TrimSpace(section), strings.Join(ExportSections, ", "))
		}
	}
	if opts.output != "" && !opts.force {
		if _, err := os.Stat(opts.output); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", opts.output)
		}
	}

	client, err := RuntimeFromEnv().NewClient(opts.debug)
	if err != nil {
		return err
	}

	exporter := &Exporter{
		Client:      client,
		Sections:    opts.only,
		AllFeatures: opts.allFeatures,
	}
	manifest, tenantID, err := exporter.Export(ctx)
	if err != nil {
		return err
	}

	content, err := MarshalManifest(manifest, fmt.Sprintf("Exported from tenant %s (%s) on %s",
		tenantID, client.BaseURL.Host, time.Now().UTC().Format(time.RFC3339)))
	if err != nil {
		return err
	}

	if opts.output == "" {
		_, err := stdout.Write(content)
		return err
	}
	if err := os.WriteFile(opts.output, content, 0o644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	fmt.Fprintf(os.Stderr, "✓ Wrote %s\n", opts.output)
	return nil
}

func isExportSection(name string) bool {
	for _, section := range ExportSections {
		if strings.EqualFold(strings.TrimSpace(name), section) {
			return true
		}
	}
	return false
}

// Exporter reads the state of a tenant into a manifest
type Exporter struct {
	Client *api.Client
	// Sections to export (all ExportSections when empty)
	Sections []string
	// AllFeatures exports all feature toggles instead of only the ones set
	// for the tenant
	AllFeatures bool
}

func (e *Exporter) enabled(section string) bool {
	if len(e.Sections) == 0 {
		return true
	}
	for _, name := range e.Sections {
		if strings.EqualFold(strings.TrimSpace(name), section) {
			return true
		}
	}
	return false
}

// Export reads the selected sections of the current tenant and returns the
// manifest and the tenant ID
func (e *Exporter) Export(ctx context.Context) (*Manifest, string, error) {
	tenant := e.Client.Tenants.Current.Get(ctx, currenttenant.GetOptions{})
	if tenant.Err != nil {
		return nil, "", fmt.Errorf("failed to get current tenant: %w", tenant.Err)
	}
	tenantID := tenant.Data.Name()

	manifest := &Manifest{}
	steps := []struct {
		section string
		run     func() error
	}{
		{SectionTenantOptions, func() (err error) { manifest.TenantOptions, err = e.tenantOptions(ctx); return }},
		{SectionFeatures, func() (err error) { manifest.Features, err = e.features(ctx); return }},
		{SectionRetentionRules, func() (err error) { manifest.RetentionRules, err = e.retentionRules(ctx); return }},
		{SectionCertificateAuthority, func() (err error) { manifest.CertificateAuthority, err = e.certificateAuthority(ctx); return }},
		{SectionMicroservices, func() (err error) { manifest.Microservices, err = e.microservices(ctx, tenantID); return }},
		{SectionApplications, func() (err error) { manifest.Applications, err = e.applications(ctx, tenantID); return }},
		{SectionDeviceGroups, func() (err error) {
			manifest.DeviceGroups, err = listDeviceGroupTree(ctx, e.Client.DeviceGroups)
			return
		}},
		{SectionInventoryRoles, func() (err error) { manifest.InventoryRoles, err = e.inventoryRoles(ctx); return }},
		{SectionDigitalTwin, func() (err error) { manifest.DigitalTwin, err = e.digitalTwin(ctx); return }},
		{SectionSmartFunctions, func() (err error) { manifest.SmartFunctions, err = e.smartFunctions(ctx); return }},
	}
	for _, step := range steps {
		if !e.enabled(step.section) {
			continue
		}
		if err := step.run(); err != nil {
			return nil, tenantID, fmt.Errorf("failed to export %s: %w", step.section, err)
		}
	}
	return manifest, tenantID, nil
}

func (e *Exporter) tenantOptions(ctx context.Context) ([]TenantOptionSpec, error) {
	var specs []TenantOptionSpec
	result := e.Client.Tenants.Options.List(ctx, tenantoptions.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for option, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(option.Key(), "credentials.") {
			continue
		}
		specs = append(specs, TenantOptionSpec{Category: option.Category(), Key: option.Key(), Value: option.Value()})
	}
	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].Category != specs[j].Category {
			return specs[i].Category < specs[j].Category
		}
		return specs[i].Key < specs[j].Key
	})
	return specs, nil
}

func (e *Exporter) features(ctx context.Context) ([]FeatureSpec, error) {
	var specs []FeatureSpec
	result := e.Client.Features.List(ctx)
	for feature, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		// Strategy TENANT means the toggle is set for the tenant; other
		// toggles follow the platform default
		if !e.AllFeatures && !strings.EqualFold(feature.Strategy(), "TENANT") {
			continue
		}
		active := feature.Active()
		specs = append(specs, FeatureSpec{Key: feature.Key(), Enabled: &active})
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Key < specs[j].Key })
	return specs, nil
}

func (e *Exporter) retentionRules(ctx context.Context) ([]RetentionRuleSpec, error) {
	var specs []RetentionRuleSpec
	result := e.Client.RetentionRules.List(ctx, retentionrules.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for rule, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		spec := RetentionRuleSpec{
			DataType:     rule.DataType(),
			FragmentType: wildcardToEmpty(rule.FragmentType()),
			Type:         wildcardToEmpty(rule.Get("type").String()),
			Source:       wildcardToEmpty(rule.Get("source").String()),
			MaximumAge:   rule.MaximumAge(),
		}
		if rule.Get("editable").Exists() && !rule.Editable() {
			editable := false
			spec.Editable = &editable
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// inventoryRoles exports the inventory roles sorted by name. The permissions
// are sorted and the default type "*" is left out.
func (e *Exporter) inventoryRoles(ctx context.Context) ([]InventoryRoleSpec, error) {
	var specs []InventoryRoleSpec
	result := e.Client.InventoryRoles.List(ctx, inventoryroles.ListOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for role, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		spec := InventoryRoleSpec{Name: role.Name(), Description: role.Description()}
		for _, permission := range existingPermissions(role) {
			permission.Type = wildcardToEmpty(permission.Type)
			spec.Permissions = append(spec.Permissions, permission)
		}
		specs = append(specs, spec)
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

func wildcardToEmpty(value string) string {
	if value == "*" {
		return ""
	}
	return value
}

func (e *Exporter) certificateAuthority(ctx context.Context) (*CertificateAuthoritySpec, error) {
	result := e.Client.TrustedCertificates.CertificateAuthority.Get(ctx, certificateauthority.GetOptions{})
	if result.Err != nil {
		if result.HTTPStatus == http.StatusNotFound {
			return nil, nil
		}
		return nil, result.Err
	}
	enabled := strings.EqualFold(result.Data.Status(), "ENABLED")
	spec := &CertificateAuthoritySpec{Enabled: &enabled}
	if value := result.Data.Get("autoRegistrationEnabled"); value.Exists() {
		autoRegistration := value.Bool()
		spec.AutoRegistrationEnabled = &autoRegistration
	}
	return spec, nil
}

// subscribedNames lists the names of the applications of the given types
// which the tenant is subscribed to but does not own, such as the built-in
// microservices and web applications
func (e *Exporter) subscribedNames(ctx context.Context, tenantID string, types ...string) ([]string, error) {
	var names []string
	result := e.Client.Applications.List(ctx, applications.ListOptions{
		ProvidedFor:       tenantID,
		Type:              strings.Join(types, ","),
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for app, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		names = append(names, app.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (e *Exporter) microservices(ctx context.Context, tenantID string) ([]MicroserviceSpec, error) {
	names, err := e.subscribedNames(ctx, tenantID, "MICROSERVICE")
	var specs []MicroserviceSpec
	for _, name := range names {
		specs = append(specs, MicroserviceSpec{Name: name})
	}
	return specs, err
}

func (e *Exporter) applications(ctx context.Context, tenantID string) ([]ApplicationSpec, error) {
	names, err := e.subscribedNames(ctx, tenantID, "HOSTED", "EXTERNAL")
	var specs []ApplicationSpec
	for _, name := range names {
		specs = append(specs, ApplicationSpec{Name: name})
	}
	return specs, err
}

// MarshalManifest renders a manifest as YAML with the schema reference and a
// header comment. Fields which are not set are left out, and "$" is escaped
// as "$$" so values are loaded unchanged.
func MarshalManifest(manifest *Manifest, header string) ([]byte, error) {
	// The json tags omit unset fields, so convert via JSON and keep the
	// field order with a yaml.Node
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	pruneYAMLNode(&node)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# yaml-language-server: $schema=%s\n", SchemaID)
	for _, line := range strings.Split(header, "\n") {
		fmt.Fprintf(&buf, "# %s\n", line)
	}
	fmt.Fprintf(&buf, "#\n# Review it before applying it, e.g.: %s run -f <file> --dry-run\n\n", commandPrefix)

	if len(node.Content) == 0 || len(node.Content[0].Content) == 0 {
		buf.WriteString("{}\n")
		return buf.Bytes(), nil
	}
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pruneYAMLNode switches JSON (flow) styles to block style, removes empty
// mappings and sequences, and escapes "$" in values so they are not expanded
// as environment variables when the manifest is loaded
func pruneYAMLNode(node *yaml.Node) {
	pruneYAMLNodeKeep(node, false)
}

// keepEmptyValuesUnder are keys whose values are data compared with the
// tenant (JSON schemas, custom fragments, expected test output), where an
// empty object or list is meaningful and must be kept
var keepEmptyValuesUnder = []string{"jsonSchema", "fragments", "expectedOutput"}

func pruneYAMLNodeKeep(node *yaml.Node, keepEmpty bool) {
	node.Style = 0
	switch node.Kind {
	case yaml.ScalarNode:
		node.Value = escapeEnv(node.Value)
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			pruneYAMLNodeKeep(child, keepEmpty)
		}
	case yaml.MappingNode:
		content := node.Content[:0]
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			keepChild := keepEmpty || slices.Contains(keepEmptyValuesUnder, key.Value)
			pruneYAMLNodeKeep(key, keepEmpty)
			pruneYAMLNodeKeep(value, keepChild)
			if !keepEmpty && (value.Kind == yaml.MappingNode || value.Kind == yaml.SequenceNode) && len(value.Content) == 0 {
				continue
			}
			if keepChild && len(value.Content) == 0 {
				// An empty value must stay an empty value, not null
				if value.Kind == yaml.MappingNode || value.Kind == yaml.SequenceNode {
					value.Style = yaml.FlowStyle
				}
			}
			content = append(content, key, value)
		}
		node.Content = content
	}
}
