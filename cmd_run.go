package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/authentication"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/tenants/currenttenant"
	"github.com/spf13/cobra"
)

// runOptions holds the flags of the run command
type runOptions struct {
	manifests         []string
	only              []string
	dryRun            bool
	force             bool
	concurrency       int
	verbose           bool
	debug             bool
	targets           []string
	allChildren       bool
	targetSelector    string
	includeCurrent    bool
	credentialsMode   string
	secretsFile       string
	secretsEncryption string
	reports           []string
	reportFormat      string
	failOnChange      bool
	githubSummary     bool

	allowRemoteCommands bool
}

func newRunCmd() *cobra.Command {
	opts := &runOptions{}
	cmd := &cobra.Command{
		Use:     "run [manifest.yaml...]",
		Aliases: []string{"apply"},
		Short:   "Apply a manifest to the tenant",
		Long: `Apply a manifest to the tenant of the active go-c8y-cli session.

Every operation is idempotent, so the same manifest can be applied repeatedly. Use
--dry (or "c8y --dry tenant-sync run") to preview the changes.

Several manifests (repeated -f flags or several arguments) are merged in order,
like files included in that order: later files win.

A manifest can also be an http(s) URL. Hooks and commands of remote manifests
only run with --allow-remote-commands.

Use - to read a manifest from stdin. Its relative paths are based on the
current directory.`,
		Example: formatExamples(
			example{"run -f tenant.yaml --dry", "Preview the changes without applying them"},
			example{"run -f tenant.yaml", "Apply the manifest to the tenant of the active session"},
			example{"run -f base.yaml -f prod.yaml --dry", "Merge two manifests (prod.yaml wins) and preview the changes"},
			example{"run base.yaml customer-a.yaml", "Apply the merge of two manifests given as arguments"},
			example{"run https://example.com/tenant.yaml --dry", "Preview a manifest fetched from a URL"},
			example{"run -f - --dry < tenant.yaml", "Preview a manifest read from stdin"},
			example{"run -f tenant.yaml --only firmware,deviceProfiles", "Apply only the firmware and device profile sections"},
			example{"run -f tenant.yaml --force", "Replace existing version binaries and configuration files"},
			example{"run -f tenant.yaml --all-children", "Apply the manifest to all child tenants of the current tenant"},
			example{"run -f tenant.yaml --target-selector \"domain=*.example.com\" --include-current", "Apply the manifest to matching tenants and the current tenant"},
			example{"run -f tenant.yaml --report report.json --report report.md", "Write a JSON and a Markdown report of the run"},
			example{"run -f tenant.yaml --github-summary=false", "Do not append the report to the GitHub Actions step summary"},
			example{"run -f tenant.yaml --dry --fail-on-change", "Exit with an error when the tenant differs from the manifest (drift check)"},
		),
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: completeManifestFileList,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(opts, args)
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&opts.manifests, "manifest", "f", nil, "Path or URL of the tenant manifest file, or - to read it from stdin (default: tenant.yaml); repeat to merge several manifests in order (later files win)")
	flags.StringSliceVar(&opts.only, "only", nil, "Comma-separated list of sections to apply (default: all). Sections: "+strings.Join(SectionOrder, ", "))
	flags.BoolVar(&opts.dryRun, "dry", false, "Preview the changes without applying them")
	flags.BoolVar(&opts.force, "force", false, "Replace existing version binaries and configuration files")
	flags.IntVar(&opts.concurrency, "concurrency", 5, "Number of concurrent software version uploads (1-20)")
	flags.BoolVar(&opts.verbose, "verbose", false, "Enable detailed logging")
	flags.BoolVar(&opts.debug, "debug", false, "Enable debug mode (verbose logging + HTTP debug)")
	flags.BoolVar(&opts.allChildren, "all-children", false, "Apply to all child tenants of the current tenant (overrides the manifest targets section)")
	flags.StringVar(&opts.targetSelector, "target-selector", "", "Apply to tenants matching criteria, e.g. domain=*.example.com,company=ACME (overrides the manifest targets section)")
	flags.BoolVar(&opts.includeCurrent, "include-current", false, "Also include the current tenant when other target flags are used")
	flags.StringVar(&opts.credentialsMode, "credentials-mode", "", "How credentials for other tenants are obtained: "+CredentialsModeServiceUser+", "+CredentialsModeSessions+" or "+CredentialsModeTenantAdmin+" (overrides the manifest)")
	flags.StringVar(&opts.secretsFile, "secrets-file", "", "File which stores the values of ${generate:NAME} references (default: <manifest>.secrets.yaml)")
	flags.StringVar(&opts.secretsEncryption, "secrets-encryption", string(SecretsEncryptionAuto), "Encrypt the secrets file with sops: auto (when sops and a .sops.yaml are found), sops (required) or none")
	flags.StringArrayVar(&opts.reports, "report", nil, "Write a report of the run to a file (.json or .md); repeatable. \""+ReportGitHub+"\" appends Markdown to $"+EnvGitHubStepSummary)
	flags.StringVar(&opts.reportFormat, "report-format", "", "Format of the --report files: "+strings.Join(ReportFormats, " or ")+" (default: from the file extension)")
	flags.BoolVar(&opts.githubSummary, "github-summary", true, "Inside GitHub Actions, append the Markdown report to $"+EnvGitHubStepSummary+" (no effect elsewhere)")
	flags.BoolVar(&opts.failOnChange, "fail-on-change", false, "Exit with an error when any item was created, updated or planned (drift check)")
	flags.BoolVar(&opts.allowRemoteCommands, "allow-remote-commands", false, "Run the hooks and commands of manifests fetched from a URL")
	flags.StringArrayVar(&opts.targets, "target", nil, "Apply to a tenant referenced by ID or domain; repeatable or comma-separated (overrides the manifest targets section)")

	_ = cmd.RegisterFlagCompletionFunc("manifest", completeManifestFileList)
	_ = cmd.RegisterFlagCompletionFunc("only", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return SectionOrder, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("secrets-encryption", cobra.FixedCompletions(SecretsEncryptionModes, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("secrets-file", completeManifestFiles)
	_ = cmd.RegisterFlagCompletionFunc("report-format", cobra.FixedCompletions(ReportFormats, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("credentials-mode", cobra.FixedCompletions(
		[]string{CredentialsModeServiceUser, CredentialsModeSessions, CredentialsModeTenantAdmin}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func runSync(opts *runOptions, args []string) (err error) {
	runtime := RuntimeFromEnv()
	dryRun := opts.dryRun || runtime.DryRun

	// Mask secrets (generated values, passwords, tokens) in all output,
	// including the HTTP debug output. It is set up before logging, which
	// keeps a reference to os.Stderr
	redactor := &Redactor{}
	restoreStdio, err := RedactStdio(redactor)
	if err != nil {
		return fmt.Errorf("failed to set up output masking: %w", err)
	}
	defer restoreStdio()
	defer func() {
		// The error is printed after the output is restored
		if err != nil && !errors.Is(err, errExitFailures) {
			// Keep the error (and its chain) unless a secret was masked
			if masked := redactor.Redact(err.Error()); masked != err.Error() {
				err = errors.New(masked)
			}
		}
	}()
	configureLogging(opts.verbose, opts.debug)

	manifestPaths, err := manifestPathsFromArgs(opts.manifests, args)
	if err != nil {
		return err
	}
	// Relative paths and the default secrets file are based on the first
	// manifest (the current directory for a URL)
	manifestPath := manifestPaths[0]

	reportOutputs, err := parseReportOutputs(opts.reports, opts.reportFormat)
	if err != nil {
		return err
	}
	reportOutputs = withGitHubSummary(reportOutputs, opts.githubSummary)

	encryption := SecretsEncryption(strings.ToLower(strings.TrimSpace(opts.secretsEncryption)))
	switch encryption {
	case SecretsEncryptionAuto, SecretsEncryptionSops, SecretsEncryptionNone:
	default:
		return fmt.Errorf("invalid --secrets-encryption %q: use %s", opts.secretsEncryption, strings.Join(SecretsEncryptionModes, ", "))
	}
	secretsPath := opts.secretsFile
	if secretsPath == "" {
		secretsPath = DefaultSecretsPath(manifestPath)
	}
	secrets, err := OpenSecretStore(secretsPath, encryption)
	if err != nil {
		return err
	}
	secrets.DryRun = dryRun
	secrets.Host = authentication.HostFromEnvironment()
	secrets.Tenant = authentication.FromEnvironment().Tenant

	manifest, err := LoadManifestsWith(manifestPaths, LoadOptions{Secrets: secrets, AllowRemoteCommands: opts.allowRemoteCommands})
	// Values generated while loading are saved even when the run fails later,
	// since they may already have been used (e.g. as a user's password)
	defer func() {
		if saveErr := saveSecrets(secrets); saveErr != nil && err == nil {
			err = saveErr
		}
	}()
	if err != nil {
		return err
	}
	// Extracted bundles are used until the run ends
	defer manifest.Cleanup()
	// env and generate references (valueFrom, passwordFrom, tokenFrom)
	refValues, err := manifest.ResolveSecretRefs(secrets)
	if err != nil {
		return err
	}
	redactor.Add(refValues...)
	redactor.Add(secrets.Values()...)
	redactor.Add(manifestSecretValues(manifest)...)

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
	for _, path := range manifestPaths {
		fmt.Printf("📄 Manifest: %s\n", manifestDisplayName(path))
	}
	for _, file := range manifest.IncludedFiles {
		fmt.Printf("   includes %s\n", file)
	}
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
		if secrets.Tenant == "" && len(secrets.Generated()) > 0 {
			// The session has no tenant ID: look it up for the secrets file
			if tenant := client.Tenants.Current.Get(context.Background(), currenttenant.GetOptions{}); tenant.Err == nil {
				secrets.SetTenant(tenant.Data.Name())
			}
		}
	}

	// Work directory for downloaded artifacts
	workDir, err := os.MkdirTemp("", "tenant-sync-*")
	if err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	// In dry-run mode the login options are still read (never written), so
	// the preview shows what would change. Without a session the dry run
	// lists the desired settings only.
	var dryRunReader *api.Client
	if dryRun && manifest.LoginOptions != nil && sectionEnabled(onlySections, SectionLoginOptions) {
		if reader, err := runtime.NewClient(opts.debug); err == nil {
			dryRunReader = reader
		}
	}

	syncer := &Syncer{
		Client:       client,
		DryRunReader: dryRunReader,
		Resolver:     NewSourceResolver(manifestBaseDir(manifestPath), workDir, dryRun),
		DryRun:       dryRun,
		Force:        opts.force,
		Concurrency:  opts.concurrency,
		Targets:      targets,
		Secrets:      secrets,
	}

	startTime := time.Now()
	applyErr := syncer.Apply(ctx, manifest, onlySections)
	elapsed := time.Since(startTime)

	// The report is written for failed runs too, and a report which cannot
	// be written never hides the outcome of the sync
	defer func() {
		if len(reportOutputs) == 0 {
			return
		}
		report := BuildRunReport(syncer.Results, redactor, manifestDisplayName(manifestPath), secrets.Host, startTime, elapsed, dryRun, applyErr)
		if reportErr := WriteReports(report, reportOutputs); reportErr != nil {
			if err == nil {
				err = reportErr
			}
		}
	}()

	if applyErr != nil {
		return applyErr
	}

	syncer.PrintSummary(elapsed)

	if _, failures := syncer.Summary(); len(failures) > 0 {
		return errExitFailures
	}

	if dryRun {
		fmt.Println("\n✓ Dry run complete - no changes made")
	}
	if opts.failOnChange {
		if changes := countChanges(syncer.Results); changes > 0 {
			return fmt.Errorf("%w: %d item(s) created, updated or planned", errChangesDetected, changes)
		}
	}
	return nil
}

// errChangesDetected is returned by --fail-on-change when the run changed (or
// would change) the tenant
var errChangesDetected = errors.New("changes detected")

// saveSecrets writes the values generated in this run to the secrets file and
// reports their names (never the values)
func saveSecrets(secrets *SecretStore) error {
	generated := secrets.Generated()
	if secrets.DryRun || len(generated) == 0 {
		return nil
	}
	encrypted, err := secrets.Save()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n⚠ %v\n", err)
		return fmt.Errorf("failed to save the generated values: %w", err)
	}
	protection := "unencrypted, permissions 0600"
	if encrypted {
		protection = "encrypted with sops"
	}
	fmt.Printf("\n🔐 Generated %d value(s): %s\n   Saved to %s (%s)\n", len(generated), strings.Join(generated, ", "), secrets.Path, protection)
	return nil
}

// manifestSecretValues returns the secret values of a manifest which are
// masked in the output (user and tenant admin passwords and GitHub tokens)
func manifestSecretValues(manifest *Manifest) []string {
	var values []string
	for _, user := range manifest.Users {
		values = append(values, user.Password)
	}
	for _, tenant := range manifest.Tenants {
		values = append(values, tenant.AdminPass)
	}
	values = append(values, ssoSecretForms(manifest.ssoLogin())...)
	for _, source := range manifest.allSources() {
		if source.GitHub != nil {
			values = append(values, source.GitHub.Token)
		}
	}
	return values
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
