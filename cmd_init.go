package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// The full annotated example manifest doubles as the init template
//
//go:embed tenant.example.yaml
var manifestTemplate string

const minimalTemplate = `# yaml-language-server: $schema=` + SchemaID + `
#
# tenant-sync manifest
#
# Validate with: c8y tenant-sync validate %[1]s
# Preview with:  c8y tenant-sync run -f %[1]s --dry-run
# Apply with:    c8y tenant-sync run -f %[1]s
#
# Run 'c8y tenant-sync init --full <path>' for a fully annotated example, or see:
# https://github.com/reubenmiller/c8y-tenant-sync

software:
  - source:
      path: ./` + packagesFolder + `
      patterns: ["*.deb", "*.rpm", "*.apk", "*.ipk"]
      optional: true # skip instead of failing while the folder is empty

# firmware:
#   - deviceType: mydevicetype
#     source:
#       github:
#         repo: owner/repo
#         release: latest
#         assets: ["*.wic.xz"]

# configuration:
#   - name: myconfig
#     configurationType: myconfig.toml
#     source:
#       path: ./config/myconfig.toml

# smartrestTemplates:
#   - source:
#       path: ./smartrest # collection JSON files exported by the platform

# digitalTwin:
#   propertyDefinitions:
#     - source:
#         path: ./dtm/properties # definition files (JSON/YAML as returned by the DTM API)
#     - identifier: maxFlow
#       contexts: [asset]
#       jsonSchema: {title: Maximum flow, type: number}

# smartFunctions:
#   - source:
#       path: ./smartfunctions # rule package folders (data-prep.yaml) or exported *.tar.gz

# tenantOptions:
#   - category: configuration
#     key: my.setting
#     value: "true"

# features:
#   - key: feature-branding

# retentionRules:
#   - dataType: MEASUREMENT
#     maximumAge: 365

# trustedCertificates:
#   - autoRegistrationEnabled: true
#     source:
#       path: ./certificates # *.pem, *.crt, *.cer

# certificateRevocationLists:
#   - source:
#       path: ./certificates/revoked.csv # SERIALNO[,DATE] per row

# applications:
#   - name: advanced-software-mgmt

# deviceGroups:
#   - name: Europe
#     groups:
#       - name: Plant Berlin

# userGroups:
#   - name: operators
#     roles: [ROLE_INVENTORY_READ, ROLE_ALARM_READ]

# users:
#   - userName: jdoe@example.com
#     email: jdoe@example.com
#     sendPasswordResetEmail: true
#     groups: [operators]

# deviceProfiles:
#   - name: base-profile
#     deviceType: mydevicetype
#     software:
#       - name: mypackage
#         version: 1.0.0
`

// packagesFolder is the software folder of the minimal manifest, which init creates
const packagesFolder = "packages"

func newInitCmd() *cobra.Command {
	var full, force bool
	cmd := &cobra.Command{
		Use:   "init [manifest.yaml]",
		Short: "Create a new tenant manifest file",
		Long:  "Create a new tenant manifest file (default: tenant.yaml).",
		Example: formatExamples(
			example{"init", "Create a minimal tenant.yaml manifest in the current folder"},
			example{"init --full examples/tenant.yaml", "Create a fully annotated example manifest"},
			example{"init tenant.yaml --force", "Overwrite an existing tenant.yaml with a new skeleton"},
		),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeManifestFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(args, full, force)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "Write the fully annotated example manifest instead of a minimal skeleton")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite the file if it already exists")
	return cmd
}

// secretsGitignorePattern matches the secrets files of manifests
const secretsGitignorePattern = "*.secrets.yaml"

// ensureGitignore adds a pattern to the .gitignore in dir (creating it if
// needed) unless it is already listed. It reports whether it was added.
func ensureGitignore(dir, pattern string) (bool, error) {
	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return false, nil
		}
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += pattern + "\n"
	return true, os.WriteFile(path, []byte(content), 0o644)
}

func runInit(args []string, full, force bool) error {
	path, err := manifestPathFromArgs("", args)
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}

	content := fmt.Sprintf(minimalTemplate, path)
	if full {
		content = manifestTemplate
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write manifest: %w", err)
	}

	// Sanity check: the generated manifest must validate
	if _, err := LoadManifest(path); err != nil {
		return fmt.Errorf("generated manifest failed validation (this is a bug): %w", err)
	}

	fmt.Printf("✓ Created %s\n", path)

	// The minimal manifest syncs software packages from ./packages (relative
	// to the manifest), so create it to give a manifest which runs as is
	if !full {
		packagesDir := filepath.Join(filepath.Dir(path), packagesFolder)
		if info, err := os.Stat(packagesDir); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("cannot create %s folder: a file with that name exists", packagesDir)
			}
			fmt.Printf("✓ Using existing %s folder\n", packagesDir)
		} else {
			if err := os.MkdirAll(packagesDir, 0o755); err != nil {
				return fmt.Errorf("failed to create %s folder: %w", packagesDir, err)
			}
			fmt.Printf("✓ Created %s folder (add your software packages here)\n", packagesDir)
		}
	}

	// Generated values (${generate:NAME}) are stored next to the manifest
	if added, err := ensureGitignore(filepath.Dir(path), secretsGitignorePattern); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ could not update .gitignore: %v\n", err)
	} else if added {
		fmt.Printf("✓ Added %s to %s (secrets files of generated values)\n", secretsGitignorePattern, filepath.Join(filepath.Dir(path), ".gitignore"))
	}

	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Edit %s to describe your tenant\n", path)
	fmt.Printf("  2. Validate it:          %s validate %s\n", commandPrefix, path)
	fmt.Printf("  3. Preview the changes:  %s run -f %s --dry-run\n", commandPrefix, path)
	fmt.Printf("  4. Apply it:             %s run -f %s\n", commandPrefix, path)
	return nil
}
