package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllCommandsHaveExamples ensures every command supports --examples and
// has examples in the c8y format
func TestAllCommandsHaveExamples(t *testing.T) {
	root := newRootCmd()
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Name() == "help" {
			return
		}
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			require.NotEmpty(t, cmd.Example, "command has no examples")
			assert.True(t, strings.HasPrefix(cmd.Example, "$ "+commandPrefix+" "), "examples must start with '$ %s'", commandPrefix)
			assert.NotNil(t, cmd.Flag("examples"), "command does not support --examples")
		})
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

func TestExamplesFlag(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(wd) })

	for _, args := range [][]string{
		{"--examples"},
		{"init", "--examples"},
		{"validate", "--examples"},
		{"run", "--examples"},
		{"apply", "--examples"},
		{"schema", "--examples"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newRootCmd()
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			cmd.SetArgs(args)
			err := cmd.Execute()
			assert.True(t, errors.Is(err, errExamplesShown), "unexpected error: %v", err)
			assert.Contains(t, out.String(), "$ "+commandPrefix+" ")
		})
	}

	// The command must not run: init --examples does not create a manifest
	_, err = os.Stat(filepath.Join(dir, "tenant.yaml"))
	assert.True(t, os.IsNotExist(err), "init ran although --examples was given")
}
