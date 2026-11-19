package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/spf13/cobra"
)

// runOptions holds the flags of the run command
type runOptions struct {
	manifest        string
	only            []string
	dryRun          bool
	force           bool
	concurrency     int
	verbose         bool
	debug           bool
	targets         []string
	allChildren     bool
	targetSelector  string
	includeCurrent  bool
	credentialsMode string
}

func newRunCmd() *cobra.Command {
	opts := &runOptions{}
	cmd := &cobra.Command{
		Use:     "run [manifest.yaml]",
		Aliases: []string{"apply"},
		Short:   "Apply a manifest to the tenant",
		Long: `Apply a manifest to the tenant of the active go-c8y-cli session.

Every operation is idempotent, so the same manifest can be applied repeatedly. Use
--dry-run (or "c8y --dry tenant-sync run") to preview the changes.`,
		Example: formatExamples(
			example{"run -f tenant.yaml --dry-run", "Preview the changes without applying them"},
			example{"run -f tenant.yaml", "Apply the manifest to the tenant of the active session"},
			example{"run -f tenant.yaml --only firmware,deviceProfiles", "Apply only the firmware and device profile sections"},
			example{"run -f tenant.yaml --force", "Replace existing version binaries and configuration files"},
			example{"run -f tenant.yaml --all-children", "Apply the manifest to all child tenants of the current tenant"},
			example{"run -f tenant.yaml --target-selector \"domain=*.example.com\" --include-current", "Apply the manifest to matching tenants and the current tenant"},
		),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeManifestFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(opts, args)
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&opts.manifest, "manifest", "f", "", "Path to the tenant manifest file (default: tenant.yaml)")
	flags.StringSliceVar(&opts.only, "only", nil, "Comma-separated list of sections to apply (default: all). Sections: "+strings.Join(SectionOrder, ", "))
	flags.BoolVar(&opts.dryRun, "dry-run", false, "Preview the changes without applying them")
	flags.BoolVar(&opts.dryRun, "dry", false, "Alias for --dry-run")
	flags.BoolVar(&opts.force, "force", false, "Replace existing version binaries and configuration files")
	flags.IntVar(&opts.concurrency, "concurrency", 5, "Number of concurrent software version uploads (1-20)")
	flags.BoolVar(&opts.verbose, "verbose", false, "Enable detailed logging")
	flags.BoolVar(&opts.debug, "debug", false, "Enable debug mode (verbose logging + HTTP debug)")
	flags.BoolVar(&opts.allChildren, "all-children", false, "Apply to all child tenants of the current tenant (overrides the manifest targets section)")
	flags.StringVar(&opts.targetSelector, "target-selector", "", "Apply to tenants matching criteria, e.g. domain=*.example.com,company=ACME (overrides the manifest targets section)")
	flags.BoolVar(&opts.includeCurrent, "include-current", false, "Also include the current tenant when other target flags are used")
	flags.StringVar(&opts.credentialsMode, "credentials-mode", "", "How credentials for other tenants are obtained: "+CredentialsModeServiceUser+" or "+CredentialsModeSessions+" (overrides the manifest)")
	flags.StringArrayVar(&opts.targets, "target", nil, "Apply to a tenant referenced by ID or domain; repeatable or comma-separated (overrides the manifest targets section)")

	_ = cmd.RegisterFlagCompletionFunc("manifest", completeManifestFiles)
	_ = cmd.RegisterFlagCompletionFunc("only", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return SectionOrder, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("credentials-mode", cobra.FixedCompletions(
		[]string{CredentialsModeServiceUser, CredentialsModeSessions}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func runSync(opts *runOptions, args []string) error {
	runtime := RuntimeFromEnv()
	dryRun := opts.dryRun || runtime.DryRun
	configureLogging(opts.verbose, opts.debug)

	manifestPath, err := manifestPathFromArgs(opts.manifest, args)
	if err != nil {
		return err
	}

	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return err
	}

	targets, err := targetsFromFlags(opts.targets, opts.allChildren, opts.targetSelector, opts.includeCurrent)
	if err != nil {
		return err
	}
	targets = mergeTargetOverrides(manifest.Targets, targets, opts.credentialsMode)
	if err := targets.Validate(); err != nil {
		return fmt.Errorf("invalid targets: %w", err)
	}

	var onlySections []string
	for _, section := range opts.only {
		if !isKnownSection(section) {
			return fmt.Errorf("unknown section %q. Valid sections: %s", strings.TrimSpace(section), strings.Join(SectionOrder, ", "))
		}
		onlySections = append(onlySections, section)
	}

	if opts.concurrency < 1 || opts.concurrency > 20 {
		return fmt.Errorf("--concurrency must be between 1 and 20, got %d", opts.concurrency)
	}

	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║        Tenant Sync for Cumulocity        ║")
	fmt.Println("╚══════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("📄 Manifest: %s\n", manifestPath)
	if dryRun {
		fmt.Println("🔎 Mode: dry-run (no changes will be made)")
	}
	fmt.Println()

	ctx := context.Background()

	var client *api.Client
	if !dryRun {
		client, err = runtime.NewClient(opts.debug)
		if err != nil {
			return err
		}
	}

	// Work directory for downloaded artifacts
	workDir, err := os.MkdirTemp("", "tenant-sync-*")
	if err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	syncer := &Syncer{
		Client:      client,
		Resolver:    NewSourceResolver(filepath.Dir(manifestPath), workDir, dryRun),
		DryRun:      dryRun,
		Force:       opts.force,
		Concurrency: opts.concurrency,
		Targets:     targets,
	}

	startTime := time.Now()
	if err := syncer.Apply(ctx, manifest, onlySections); err != nil {
		return err
	}

	syncer.PrintSummary(time.Since(startTime))

	if _, failures := syncer.Summary(); len(failures) > 0 {
		return errExitFailures
	}

	if dryRun {
		fmt.Println("\n✓ Dry run complete - no changes made")
	}
	return nil
}

// targetsFromFlags builds a targets spec from the CLI flags. It returns nil
// when no target flag was given (the manifest targets section then applies).
func targetsFromFlags(targets []string, allChildren bool, selector string, includeCurrent bool) (*TargetsSpec, error) {
	if len(targets) == 0 && !allChildren && selector == "" && !includeCurrent {
		return nil, nil
	}

	spec := &TargetsSpec{AllChildren: allChildren}
	for _, entry := range targets {
		for _, part := range strings.Split(entry, ",") {
			if part = strings.TrimSpace(part); part != "" {
				spec.Tenants = append(spec.Tenants, part)
			}
		}
	}
	if selector != "" {
		sel := &TenantSelector{}
		for _, pair := range strings.Split(selector, ",") {
			key, value, found := strings.Cut(pair, "=")
			if !found {
				return nil, fmt.Errorf("invalid --target-selector entry %q: expected key=value", pair)
			}
			switch strings.TrimSpace(key) {
			case "domain":
				sel.Domain = strings.TrimSpace(value)
			case "company":
				sel.Company = strings.TrimSpace(value)
			default:
				return nil, fmt.Errorf("unknown --target-selector key %q: supported keys are domain and company", strings.TrimSpace(key))
			}
		}
		spec.Selector = sel
	}
	if includeCurrent {
		include := true
		spec.Current = &include
	}
	return spec, nil
}

// mergeTargetOverrides combines the manifest targets section with the CLI
// overrides: any target selection flag replaces the manifest's selection
// entirely (but keeps its credentials config), and --credentials-mode
// overrides the credentials mode. Returns nil when nothing overrides the
// manifest.
func mergeTargetOverrides(fromManifest, fromFlags *TargetsSpec, credentialsMode string) *TargetsSpec {
	spec := fromFlags
	if spec != nil && fromManifest != nil {
		spec.Credentials = fromManifest.Credentials
	}

	if credentialsMode != "" {
		if spec == nil {
			if fromManifest != nil {
				clone := *fromManifest
				spec = &clone
			} else {
				spec = &TargetsSpec{}
			}
		}
		if spec.Credentials != nil {
			creds := *spec.Credentials
			creds.Mode = credentialsMode
			spec.Credentials = &creds
		} else {
			spec.Credentials = &TargetCredentials{Mode: credentialsMode}
		}
	}
	return spec
}

func isKnownSection(name string) bool {
	for _, section := range SectionOrder {
		if strings.EqualFold(strings.TrimSpace(name), section) {
			return true
		}
	}
	return false
}
