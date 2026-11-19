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

// JSONSchemaExtend enforces that exactly one of path, url or github is set,
// mirroring Source.Validate
func (Source) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"path"}, Not: &jsonschema.Schema{
			AnyOf: []*jsonschema.Schema{
				{Required: []string{"url"}},
				{Required: []string{"github"}},
			},
		}},
		{Required: []string{"url"}, Not: &jsonschema.Schema{
			AnyOf: []*jsonschema.Schema{
				{Required: []string{"path"}},
				{Required: []string{"github"}},
			},
		}},
		{Required: []string{"github"}, Not: &jsonschema.Schema{
			AnyOf: []*jsonschema.Schema{
				{Required: []string{"path"}},
				{Required: []string{"url"}},
			},
		}},
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
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"application"}, Not: &jsonschema.Schema{Required: []string{"device"}}},
		{Required: []string{"device"}, Not: &jsonschema.Schema{Required: []string{"application"}}},
	}
}

// GenerateSchema builds the JSON schema for the manifest from the Go types
func GenerateSchema() ([]byte, error) {
	reflector := &jsonschema.Reflector{
		// Inline the Manifest type at the top level instead of a $ref
		ExpandedStruct: true,
	}

	schema := reflector.Reflect(&Manifest{})
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
