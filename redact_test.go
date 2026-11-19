package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactor(t *testing.T) {
	r := &Redactor{}
	r.Add("s3cret-value", "abc", "", "s3cret-value-long", "multi\nline", "s3cret-value")
	assert.Equal(t, "password=*** and ***, abc stays", r.Redact("password=s3cret-value and s3cret-value-long, abc stays"),
		"longer values are masked first; short values are not masked")
	assert.Equal(t, "multi\nline", r.Redact("multi\nline"), "values with line breaks are ignored")
}

func TestRedactingWriterSplitWrites(t *testing.T) {
	r := &Redactor{}
	r.Add("Abc-123-xyz")
	out := &bytes.Buffer{}
	w := &redactingWriter{redactor: r, out: out}

	// The secret is split across writes, and the last line has no newline
	for _, chunk := range []string{"line 1: Abc-", "123", "-xyz done\nline 2: Ab", "c-123-xyz"} {
		_, err := w.Write([]byte(chunk))
		require.NoError(t, err)
	}
	assert.Equal(t, "line 1: *** done\n", out.String(), "complete lines are written; partial lines are held back")
	require.NoError(t, w.Flush())
	assert.Equal(t, "line 1: *** done\nline 2: ***", out.String())
}

// captureStdio redirects os.Stdout and os.Stderr to files for the duration
// of fn and returns what was written
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.Create(filepath.Join(dir, "stdout"))
	require.NoError(t, err)
	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	require.NoError(t, err)
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
	}()
	fn()
	outFile.Close()
	errFile.Close()
	outBytes, _ := os.ReadFile(outFile.Name())
	errBytes, _ := os.ReadFile(errFile.Name())
	return string(outBytes), string(errBytes)
}

func TestRedactStdio(t *testing.T) {
	stdout, stderr := captureStdio(t, func() {
		r := &Redactor{}
		restore, err := RedactStdio(r)
		require.NoError(t, err)
		// Values added after the setup are masked too
		r.Add("Gen-Value-42")
		fmt.Println("stdout Gen-Value-42")
		fmt.Fprintf(os.Stderr, "BODY: {\"password\":\"Gen-Value-42\"}")
		restore()
		restore() // idempotent
	})
	assert.Equal(t, "stdout ***\n", stdout)
	assert.Equal(t, "BODY: {\"password\":\"***\"}", stderr, "partial lines are flushed on restore")
}
