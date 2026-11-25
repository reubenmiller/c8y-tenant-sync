package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"
	"github.com/spf13/cobra"
)

// SchemaID is the canonical URL of the published schema. Reference it from a
// manifest to get IDE completion and validation:
//
//	# yaml-language-server: $schema=<SchemaID>
const SchemaID = "https://raw.githubusercontent.com/reubenmiller/c8y-tenant-sync/main/tenant.schema.json"

// JSONSchemaExtend makes adminPass and adminPasswordFrom mutually exclusive,
// mirroring the manifest validation
func (ChildTenantSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.Not = &jsonschema.Schema{Required: []string{"adminPass", "adminPasswordFrom"}}
}

// JSONSchemaExtend requires a userName or an email (the userName defaults to
// the email)
func (UserSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.AnyOf = []*jsonschema.Schema{
		{Required: []string{"userName"}},
		{Required: []string{"email"}},
	}
}

// JSONSchemaExtend requires either a source or an explicit entry (name and
// version), mirroring the manifest validation
func (SoftwareSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"source"}, Not: &jsonschema.Schema{Required: []string{"name"}}},
		{Required: []string{"name", "version"}, Not: &jsonschema.Schema{Required: []string{"source"}}},
	}
}

// JSONSchemaExtend requires either a source or an explicit entry (name and
// version), mirroring the manifest validation
func (FirmwareSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"source"}},
		{Required: []string{"name", "version"}, Not: &jsonschema.Schema{
			AnyOf: []*jsonschema.Schema{{Required: []string{"source"}}, {Required: []string{"versionPattern"}}},
		}},
	}
}

// JSONSchemaExtend requires either a source or an explicit entry (name with
// a url, inline content or neither), mirroring the manifest validation
func (ConfigurationSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"source"}, Not: &jsonschema.Schema{
			AnyOf: []*jsonschema.Schema{{Required: []string{"url"}}, {Required: []string{"content"}}, {Required: []string{"filename"}}},
		}},
		{Required: []string{"name"}, Not: &jsonschema.Schema{
			AnyOf: []*jsonschema.Schema{{Required: []string{"source"}}, {Required: []string{"url", "content"}}},
		}},
	}
}

// JSONSchemaExtend enforces that exactly one of path, url or github is set,
// mirroring Source.Validate
func (Source) JSONSchemaExtend(schema *jsonschema.Schema) {
	// Exactly one of the source kinds
	kinds := []string{"path", "url", "github", "cloudsmith"}
	schema.OneOf = nil
	for _, kind := range kinds {
		var others []*jsonschema.Schema
		for _, other := range kinds {
			if other != kind {
				others = append(others, &jsonschema.Schema{Required: []string{other}})
			}
		}
		schema.OneOf = append(schema.OneOf, &jsonschema.Schema{
			Required: []string{kind},
			Not:      &jsonschema.Schema{AnyOf: others},
		})
	}
}

// JSONSchemaExtend enforces that exactly one of value or valueFrom is set,
// mirroring the Manifest.Validate rules
func (TenantOptionSpec) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"value"}, Not: &jsonschema.Schema{Required: []string{"valueFrom"}}},
		{Required: []string{"valueFrom"}, Not: &jsonschema.Schema{Required: []string{"value"}}},
	}
}

// JSONSchemaExtend enforces that exactly one lookup reference is set
func (TenantOptionValueFrom) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = exactlyOneOf("application", "device", "env", "generate")
}

// JSONSchemaExtend requires exactly one of env and generate
func (SecretRef) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = exactlyOneOf("env", "generate")
}

// exactlyOneOf returns oneOf alternatives which require exactly one of the
// properties
func exactlyOneOf(properties ...string) []*jsonschema.Schema {
	var alternatives []*jsonschema.Schema
	for _, property := range properties {
		var others []*jsonschema.Schema
		for _, other := range properties {
			if other != property {
				others = append(others, &jsonschema.Schema{Required: []string{other}})
			}
		}
		alternatives = append(alternatives, &jsonschema.Schema{
			Required: []string{property},
			Not:      &jsonschema.Schema{AnyOf: others},
		})
	}
	return alternatives
}

// GenerateSchema builds the JSON schema for the manifest from the Go types
func GenerateSchema() ([]byte, error) {
	reflector := &jsonschema.Reflector{
		// Inline the Manifest type at the top level instead of a $ref
		ExpandedStruct: true,
	}

	schema := reflector.Reflect(&Manifest{})
	// Allow the metadata which sops adds to encrypted manifests, so editors
	// accept them (tenant-sync decrypts them before parsing)
	schema.Properties.Set("sops", &jsonschema.Schema{
		Type:        "object",
		Description: "Metadata added by sops to encrypted manifests (tenant-sync decrypts the manifest with the sops CLI before applying it)",
	})
	schema.ID = SchemaID
	schema.Title = "tenant-sync manifest"
	schema.Description = "Declarative description of the desired state of a Cumulocity IoT tenant, applied with the c8y tenant-sync extension"

	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func newSchemaCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Generate the JSON schema for the manifest file",
		Long: `Generate the JSON schema for the manifest file.

Reference the schema from a manifest for IDE completion (VS Code YAML extension, JetBrains):

  # yaml-language-server: $schema=` + SchemaID,
		Example: formatExamples(
			example{"schema", "Print the JSON schema of the manifest"},
			example{"schema -o tenant.schema.json", "Write the JSON schema to a file, for IDE completion"},
		),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchema(output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "Write the schema to a file instead of stdout")
	_ = cmd.RegisterFlagCompletionFunc("output", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"json"}, cobra.ShellCompDirectiveFilterFileExt
	})
	return cmd
}

func runSchema(output string) error {
	schema, err := GenerateSchema()
	if err != nil {
		return fmt.Errorf("failed to generate schema: %w", err)
	}

	if output == "" {
		_, err := os.Stdout.Write(schema)
		return err
	}

	if err := os.WriteFile(output, schema, 0o644); err != nil {
		return fmt.Errorf("failed to write schema: %w", err)
	}
	fmt.Printf("✓ Wrote schema to %s\n", output)
	return nil
}

// JSONSchema describes an extension of a web application as
// "<name>[@<version or tag>]" or an object
func (ApplicationExtensionSpec) JSONSchema() *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set("name", &jsonschema.Schema{Type: "string", MinLength: ptrUint64(1), Description: "Extension name (or context path)"})
	props.Set("version", &jsonschema.Schema{Type: "string", Description: "Version or tag to install (defaults to latest)", Examples: []any{"latest", "1.2.0"}})
	props.Set("installed", &jsonschema.Schema{Type: "boolean", Description: "Desired state (defaults to true); false removes the extension from the application"})
	return &jsonschema.Schema{
		Description: "UI extension installed into the web application: \"<name>[@<version or tag>]\" or an object",
		OneOf: []*jsonschema.Schema{
			{Type: "string", Pattern: "^[^@\\s]+(@[^@\\s]+)?$", Examples: []any{"my-widget-plugin", "my-widget-plugin@1.2.0"}},
			{Type: "object", Properties: props, Required: []string{"name"}, AdditionalProperties: jsonschema.FalseSchema},
		},
	}
}

func ptrUint64(v uint64) *uint64 { return &v }
