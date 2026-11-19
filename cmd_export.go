package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ExportSections are the manifest sections which export can read from a tenant
var ExportSections = []string{
	SectionTenantOptions,
	SectionFeatures,
	SectionRetentionRules,
	SectionCertificateAuthority,
	SectionApplications,
}

// exportOptions holds the flags of the export command
type exportOptions struct {
	output           string
	sections         []string
	allFeatures      bool
	applicationTypes []string
	force            bool
	verbose          bool
	debug            bool
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
- applications: applications the tenant is subscribed to but does not own (by default
  microservices; use --application-type to change)

Review the manifest before applying it to other tenants.`,
		Example: formatExamples(
			example{"export", "Print a manifest of the current tenant"},
			example{"export -o tenant.yaml", "Write the manifest of the current tenant to a file"},
			example{"export --sections tenantOptions,features", "Export only the tenant options and feature toggles"},
			example{"export --application-type MICROSERVICE,HOSTED", "Also export subscribed web applications"},
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&opts.output, "output", "o", "", "Write the manifest to a file instead of stdout")
	flags.StringSliceVar(&opts.sections, "sections", nil, "Comma-separated list of sections to export (default: all). Sections: "+strings.Join(ExportSections, ", "))
	flags.BoolVar(&opts.allFeatures, "all-features", false, "Export all feature toggles, not only the ones set for the tenant")
	flags.StringSliceVar(&opts.applicationTypes, "application-type", []string{"MICROSERVICE"}, "Types of subscribed applications to export (e.g. MICROSERVICE, HOSTED, EXTERNAL)")
	flags.BoolVar(&opts.force, "force", false, "Overwrite the output file if it already exists")
	flags.BoolVar(&opts.verbose, "verbose", false, "Enable detailed logging")
	flags.BoolVar(&opts.debug, "debug", false, "Enable debug mode (verbose logging + HTTP debug)")

	_ = cmd.RegisterFlagCompletionFunc("sections", cobra.FixedCompletions(ExportSections, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("application-type", cobra.FixedCompletions(
		[]string{"MICROSERVICE", "HOSTED", "EXTERNAL"}, cobra.ShellCompDirectiveNoFileComp))
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

	for _, section := range opts.sections {
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
		Client:           client,
		Sections:         opts.sections,
		AllFeatures:      opts.allFeatures,
		ApplicationTypes: opts.applicationTypes,
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
	// ApplicationTypes are the types of subscribed applications to export
	ApplicationTypes []string
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
		{SectionApplications, func() (err error) { manifest.Applications, err = e.applications(ctx, tenantID); return }},
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

func (e *Exporter) applications(ctx context.Context, tenantID string) ([]ApplicationSpec, error) {
	types := e.ApplicationTypes
	if len(types) == 0 {
		types = []string{"MICROSERVICE"}
	}
	var specs []ApplicationSpec
	result := e.Client.Applications.List(ctx, applications.ListOptions{
		// Applications the tenant is subscribed to but does not own, such
		// as the built-in microservices
		ProvidedFor:       tenantID,
		Type:              strings.ToUpper(strings.Join(types, ",")),
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	})
	for app, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		specs = append(specs, ApplicationSpec{Name: app.Name(), Type: app.Type()})
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
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
	node.Style = 0
	switch node.Kind {
	case yaml.ScalarNode:
		node.Value = escapeEnv(node.Value)
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			pruneYAMLNode(child)
		}
	case yaml.MappingNode:
		content := node.Content[:0]
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			pruneYAMLNode(key)
			pruneYAMLNode(value)
			if (value.Kind == yaml.MappingNode || value.Kind == yaml.SequenceNode) && len(value.Content) == 0 {
				continue
			}
			content = append(content, key, value)
		}
		node.Content = content
	}
}
