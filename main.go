// c8y-tenant-sync is a go-c8y-cli binary extension which keeps Cumulocity
// tenants in sync with a declarative manifest.
//
// Build it into the extension folder and install it locally:
//
//	go build -o bin/c8y-tenant-sync .
//	c8y extensions install .
//	c8y tenant-sync --help
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// commandPrefix is how users call the extension, used in help and hint messages
const commandPrefix = "c8y tenant-sync"

// errExitFailures signals that the command already reported its failures and
// only a non-zero exit code is needed
var errExitFailures = errors.New("one or more items failed")

// version is set at build time by GoReleaser
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "c8y-tenant-sync",
		Short: "GitOps-style tenant synchronisation for Cumulocity",
		// Show the command as users call it (c8y tenant-sync) in help and usage
		Annotations: map[string]string{
			cobra.CommandDisplayNameAnnotation: commandPrefix,
		},
		Long: `Keep Cumulocity tenants in sync with a declarative manifest.

Describe the desired tenant state in a YAML file (software, firmware and configuration
repositories, tenant options, feature toggles, retention rules, application subscriptions,
users and user groups, SmartREST 2.0 templates, device profiles) and apply it idempotently
to one or more tenants.

The "run" command uses the active go-c8y-cli session.`,
		Example: formatExamples(
			example{"init", "Create a minimal tenant.yaml manifest"},
			example{"validate tenant.yaml", "Validate a manifest without connecting to Cumulocity"},
			example{"run -f tenant.yaml --dry", "Preview the changes the manifest would make"},
			example{"run -f tenant.yaml --only firmware,deviceProfiles", "Apply only the firmware and device profile sections"},
			example{"schema -o tenant.schema.json", "Write the JSON schema of the manifest to a file"},
			example{"export -o tenant.yaml", "Create a manifest from the current state of the tenant"},
		),
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	// Make the root runnable so its persistent pre-run (--examples) is called
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	}

	root.AddCommand(
		newInitCmd(),
		newValidateCmd(),
		newRunCmd(),
		newSchemaCmd(),
		newExportCmd(),
		newBundleCmd(),
	)
	addExamplesFlag(root)
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		if errors.Is(err, errExamplesShown) {
			return
		}
		if !errors.Is(err, errExitFailures) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(1)
	}
}

// configureLogging sets the log level based on the verbose/debug flags
func configureLogging(verbose, debug bool) {
	level := slog.LevelError
	if verbose || debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}

// manifestPathFromArgs returns the manifest path from the -f flag or a single
// positional argument, defaulting to tenant.yaml when neither is given
func manifestPathFromArgs(flagValue string, args []string) (string, error) {
	if flagValue != "" && len(args) > 0 {
		return "", fmt.Errorf("specify the manifest either with -f or as an argument, not both")
	}
	if flagValue != "" {
		return flagValue, nil
	}
	switch len(args) {
	case 0:
		return "tenant.yaml", nil
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("unexpected arguments: %s", strings.Join(args[1:], " "))
	}
}

// manifestPathsFromArgs returns the manifest paths of run and validate: the
// repeatable -f flag or the positional arguments (not both), defaulting to
// tenant.yaml. Values are not split on commas, since filenames may contain
// them.
func manifestPathsFromArgs(flagValues []string, args []string) ([]string, error) {
	if len(flagValues) > 0 && len(args) > 0 {
		return nil, fmt.Errorf("specify the manifests either with -f or as arguments, not both")
	}
	paths := flagValues
	if len(paths) == 0 {
		paths = args
	}
	if len(paths) == 0 {
		return []string{"tenant.yaml"}, nil
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("manifest path must not be empty")
		}
	}
	return paths, nil
}

// completeManifestFileList completes YAML files for every manifest argument
// (commands which accept several manifests)
func completeManifestFileList(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
}

// completeManifestFiles completes YAML files for the manifest argument
func completeManifestFiles(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
}
