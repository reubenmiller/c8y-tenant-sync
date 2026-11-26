package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/ui/plugins"
	"github.com/spf13/cobra"
)

func newValidateCmd() *cobra.Command {
	var manifestFlags []string
	var checkSources, verbose, allowRemoteCommands bool
	cmd := &cobra.Command{
		Use:   "validate [manifest.yaml...]",
		Short: "Validate a manifest file",
		Long: `Validate a manifest file without connecting to Cumulocity.

Unknown fields are errors. Use --check-sources to also resolve every source (local
paths must exist, GitHub releases must have matching assets).

Several manifests (repeated -f flags or several arguments) are validated and merged
in order, like files included in that order: later files win.

A manifest can also be an http(s) URL. Hooks and commands of remote manifests
are errors unless --allow-remote-commands is set.

Use - to read a manifest from stdin. Its relative paths are based on the
current directory.`,
		Example: formatExamples(
			example{"validate tenant.yaml", "Validate the manifest schema (unknown fields are errors)"},
			example{"validate base.yaml prod.yaml", "Validate the merge of two manifests (prod.yaml wins)"},
			example{"validate https://example.com/tenant.yaml", "Validate a manifest fetched from a URL"},
			example{"validate -f - < tenant.yaml", "Validate a manifest read from stdin"},
			example{"validate tenant.yaml --check-sources", "Also check that every local path exists and every GitHub release has matching assets"},
		),
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: completeManifestFileList,
		RunE: func(cmd *cobra.Command, args []string) error {
			configureLogging(verbose, false)
			manifestPaths, err := manifestPathsFromArgs(manifestFlags, args)
			if err != nil {
				return err
			}
			return runValidate(manifestPaths, checkSources, allowRemoteCommands)
		},
	}
	cmd.Flags().StringArrayVarP(&manifestFlags, "manifest", "f", nil, "Path or URL of the tenant manifest file, or - to read it from stdin (default: tenant.yaml); repeat to merge several manifests in order (later files win)")
	cmd.Flags().BoolVar(&checkSources, "check-sources", false, "Also resolve every source (local paths must exist, GitHub releases must have matching assets)")
	cmd.Flags().BoolVar(&allowRemoteCommands, "allow-remote-commands", false, "Allow hooks and commands in manifests fetched from a URL")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Enable detailed logging")
	_ = cmd.RegisterFlagCompletionFunc("manifest", completeManifestFileList)
	return cmd
}

func runValidate(manifestPaths []string, checkSources, allowRemoteCommands bool) error {
	manifest, err := LoadManifestsWith(manifestPaths, LoadOptions{AllowRemoteCommands: allowRemoteCommands})
	if err != nil {
		return err
	}
	defer manifest.Cleanup()
	// Relative source paths are based on the first manifest
	manifestPath := manifestPaths[0]

	names := make([]string, 0, len(manifestPaths))
	for _, path := range manifestPaths {
		names = append(names, manifestDisplayName(path))
	}
	if len(names) == 1 {
		fmt.Printf("✓ %s is valid\n", names[0])
	} else {
		fmt.Printf("✓ %s are valid\n", strings.Join(names, ", "))
	}
	for _, file := range manifest.IncludedFiles {
		fmt.Printf("  includes %s\n", file)
	}
	fmt.Println()
	printSectionCount(SectionTenantOptions, len(manifest.TenantOptions))
	alarmMappingCount := 0
	if manifest.AlarmMapping != nil {
		alarmMappingCount = len(manifest.AlarmMapping.Mappings)
	}
	printSectionCount(SectionAlarmMapping, alarmMappingCount)
	printSectionCount(SectionFeatures, len(manifest.Features))
	printSectionCount(SectionRetentionRules, len(manifest.RetentionRules))
	caCount := 0
	if manifest.CertificateAuthority != nil {
		caCount = 1
	}
	printSectionCount(SectionCertificateAuthority, caCount)
	printSectionCount(SectionTrustedCertificates, len(manifest.TrustedCertificates))
	printSectionCount(SectionCertificateRevocations, len(manifest.CertificateRevocationLists))
	printSectionCount(SectionMicroservices, len(manifest.Microservices))
	printSectionCount(SectionExtensions, len(manifest.Extensions))
	printSectionCount(SectionApplications, len(manifest.Applications))
	printSectionCount(SectionDeviceGroups, len(manifest.DeviceGroups))
	printSectionCount(SectionGlobalRoles, len(manifest.GlobalRoles))
	printSectionCount(SectionInventoryRoles, len(manifest.InventoryRoles))
	printSectionCount(SectionUsers, len(manifest.Users))
	loginCount := 0
	if manifest.LoginOptions != nil {
		loginCount = 1
	}
	printSectionCount(SectionLoginOptions, loginCount)
	printSectionCount(SectionSoftware, len(manifest.Software))
	printSectionCount(SectionFirmware, len(manifest.Firmware))
	printSectionCount(SectionConfiguration, len(manifest.Configuration))
	printSectionCount(SectionSmartRest, len(manifest.SmartRestTemplates))
	dtmCount := 0
	if manifest.DigitalTwin != nil {
		dtmCount = len(manifest.DigitalTwin.PropertyDefinitions) + len(manifest.DigitalTwin.AssetDefinitions)
	}
	printSectionCount(SectionDigitalTwin, dtmCount)
	printSectionCount(SectionSmartFunctions, len(manifest.SmartFunctions))
	printSectionCount(SectionDeviceProfiles, len(manifest.DeviceProfiles))

	if !checkSources {
		printWarnings(append(manifest.ExtensionReferenceWarnings(nil), manifest.UserWarnings()...))
		return nil
	}

	fmt.Println("\nChecking sources...")
	resolver := NewSourceResolver(manifestBaseDir(manifestPath), "", true)

	failures := 0
	check := func(section string, index int, source Source) {
		label := fmt.Sprintf("%s[%d]", section, index)
		files, err := resolver.Resolve(source)
		if err != nil {
			failures++
			fmt.Printf("  ✗ %s: %v\n", label, err)
			return
		}
		fmt.Printf("  ✓ %s: %d file(s)\n", label, len(files))
	}

	for i, spec := range manifest.Software {
		if spec.IsExplicit() {
			continue // no source to resolve
		}
		check(SectionSoftware, i, spec.Source)
	}
	for i, spec := range manifest.Firmware {
		if spec.IsExplicit() {
			continue // no source to resolve
		}
		check(SectionFirmware, i, spec.resolvedSource())
	}
	for i, spec := range manifest.Configuration {
		if spec.IsExplicit() {
			continue // no source to resolve
		}
		check(SectionConfiguration, i, spec.Source)
	}
	for i, spec := range manifest.SmartRestTemplates {
		check(SectionSmartRest, i, spec.resolvedSource())
	}
	if manifest.DigitalTwin != nil {
		for _, key := range manifest.DigitalTwin.keys() {
			for i, entry := range key.specs {
				if entry.Source != nil {
					check(SectionDigitalTwin+"."+key.name, i, resolvedDigitalTwinSource(*entry.Source))
				}
			}
		}
	}
	for i, spec := range manifest.SmartFunctions {
		if spec.IsInline() {
			continue // no source to resolve
		}
		check(SectionSmartFunctions, i, spec.resolvedSource())
	}
	for i, spec := range manifest.Microservices {
		if spec.Source != nil {
			check(SectionMicroservices, i, *spec.Source)
		}
	}
	// Extension archives are downloaded to compare the name in their
	// cumulocity.json; their context paths can also be referenced
	contextPaths := map[string]string{}
	if len(manifest.Extensions) > 0 {
		workDir, err := os.MkdirTemp("", "tenant-sync-validate-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(workDir)
		downloader := NewSourceResolver(manifestBaseDir(manifestPath), workDir, false)
		for i, spec := range manifest.Extensions {
			label := fmt.Sprintf("%s[%d]", SectionExtensions, i)
			info, err := checkExtensionArchive(downloader, spec)
			if info.Name != "" && info.ContextPath != "" {
				// Also when the name does not match, for the suggestions
				contextPaths[info.ContextPath] = info.Name
			}
			if err != nil {
				failures++
				fmt.Printf("  ✗ %s: %v\n", label, err)
				continue
			}
			fmt.Printf("  ✓ %s: %s %s (context path %s)\n", label, info.Name, info.Version, info.ContextPath)
		}
	}
	for i, spec := range manifest.Applications {
		if spec.Source != nil {
			check(SectionApplications, i, *spec.Source)
		}
	}
	for i, spec := range manifest.TrustedCertificates {
		check(SectionTrustedCertificates, i, spec.resolvedSource())
	}
	for i, spec := range manifest.CertificateRevocationLists {
		check(SectionCertificateRevocations, i, spec.resolvedSource())
	}

	printWarnings(append(manifest.ExtensionReferenceWarnings(contextPaths), manifest.UserWarnings()...))

	if failures > 0 {
		return fmt.Errorf("%d source(s) failed to resolve", failures)
	}
	fmt.Println("\n✓ All sources resolved")
	return nil
}

func printSectionCount(section string, count int) {
	fmt.Printf("  %-27s %d item(s)\n", section+":", count)
}

// checkExtensionArchive downloads the extension archive and checks that the
// name in its cumulocity.json matches the entry
func checkExtensionArchive(resolver *SourceResolver, spec ExtensionSpec) (plugins.ManifestFile, error) {
	info := plugins.ManifestFile{}
	files, err := resolver.Resolve(spec.Source)
	if err != nil {
		return info, err
	}
	if len(files) != 1 || files[0].Path == "" {
		return info, fmt.Errorf("must resolve to exactly one local file, got %d", len(files))
	}
	if err := plugins.GetManifestContents(files[0].Path, &info); err != nil {
		return info, fmt.Errorf("%s: %w", files[0].Filename, err)
	}
	if !sameName(info.Name, spec.Name) {
		return info, fmt.Errorf("%s contains the extension %q, not %q: set name to %q", files[0].Filename, info.Name, spec.Name, info.Name)
	}
	return info, nil
}

// printWarnings prints manifest warnings (which do not fail validation)
func printWarnings(warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Println()
	for _, warning := range warnings {
		fmt.Printf("  ⚠ %s\n", warning)
	}
}
