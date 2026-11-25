package main

import (
	"fmt"
	"strings"

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
are errors unless --allow-remote-commands is set.`,
		Example: formatExamples(
			example{"validate tenant.yaml", "Validate the manifest schema (unknown fields are errors)"},
			example{"validate base.yaml prod.yaml", "Validate the merge of two manifests (prod.yaml wins)"},
			example{"validate https://example.com/tenant.yaml", "Validate a manifest fetched from a URL"},
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
	cmd.Flags().StringArrayVarP(&manifestFlags, "manifest", "f", nil, "Path to the tenant manifest file (default: tenant.yaml); repeat to merge several manifests in order (later files win)")
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

	if len(manifestPaths) == 1 {
		fmt.Printf("✓ %s is valid\n", manifestPath)
	} else {
		fmt.Printf("✓ %s are valid\n", strings.Join(manifestPaths, ", "))
	}
	for _, file := range manifest.IncludedFiles {
		fmt.Printf("  includes %s\n", file)
	}
	fmt.Println()
	printSectionCount(SectionTenantOptions, len(manifest.TenantOptions))
	printSectionCount(SectionFeatures, len(manifest.Features))
	printSectionCount(SectionRetentionRules, len(manifest.RetentionRules))
	caCount := 0
	if manifest.CertificateAuthority != nil {
		caCount = 1
	}
	printSectionCount(SectionCertificateAuthority, caCount)
	printSectionCount(SectionTrustedCertificates, len(manifest.TrustedCertificates))
	printSectionCount(SectionCertificateRevocations, len(manifest.CertificateRevocationLists))
	printSectionCount(SectionApplications, len(manifest.Applications))
	printSectionCount(SectionDeviceGroups, len(manifest.DeviceGroups))
	printSectionCount(SectionUserGroups, len(manifest.UserGroups))
	printSectionCount(SectionInventoryRoles, len(manifest.InventoryRoles))
	printSectionCount(SectionUsers, len(manifest.Users))
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
	for i, spec := range manifest.TrustedCertificates {
		check(SectionTrustedCertificates, i, spec.resolvedSource())
	}
	for i, spec := range manifest.CertificateRevocationLists {
		check(SectionCertificateRevocations, i, spec.resolvedSource())
	}

	if failures > 0 {
		return fmt.Errorf("%d source(s) failed to resolve", failures)
	}
	fmt.Println("\n✓ All sources resolved")
	return nil
}

func printSectionCount(section string, count int) {
	fmt.Printf("  %-27s %d item(s)\n", section+":", count)
}
