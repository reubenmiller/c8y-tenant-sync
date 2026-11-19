package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// errExamplesShown stops a command after --examples printed its examples
var errExamplesShown = errors.New("examples shown")

// example is a command line and a description of what it does
type example struct {
	Command     string
	Description string
}

// formatExamples renders examples in the same format as the c8y built-in
// commands ("$ <command>" followed by its description)
func formatExamples(items ...example) string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("$ %s %s\n%s", commandPrefix, item.Command, item.Description))
	}
	return strings.Join(lines, "\n\n")
}

// addExamplesFlag adds the --examples flag (like the c8y built-in commands)
// to cmd and all its subcommands. With the flag set, the examples of the
// command are printed instead of running it
func addExamplesFlag(root *cobra.Command) {
	root.PersistentFlags().Bool("examples", false, "Show examples for the current command")

	next := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if show, _ := cmd.Flags().GetBool("examples"); show {
			printExamples(cmd)
			return errExamplesShown
		}
		if next != nil {
			return next(cmd, args)
		}
		return nil
	}
}

func printExamples(cmd *cobra.Command) {
	out := cmd.OutOrStdout()
	if cmd.Example == "" {
		fmt.Fprintf(out, "No examples for %s\n", cmd.CommandPath())
		return
	}
	fmt.Fprintln(out, cmd.Example)
}
