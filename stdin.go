package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// stdinManifest is the manifest path which reads the manifest from stdin,
// e.g. "c8y tenant-sync run -f - < tenant.yaml"
const stdinManifest = "-"

// stdinManifestName is the name of a manifest read from stdin in the output
const stdinManifestName = "<stdin>"

// stdinReader is where a manifest given as "-" is read from (replaced in
// tests)
var stdinReader io.Reader = os.Stdin

var (
	stdinOnce sync.Once
	stdinData []byte
	stdinErr  error
)

// isStdinManifest reports whether a manifest path reads the manifest from
// stdin
func isStdinManifest(value string) bool {
	return strings.TrimSpace(value) == stdinManifest
}

// readStdinManifest reads the manifest from stdin. stdin can only be read
// once, so the content is kept for later calls.
func readStdinManifest() ([]byte, error) {
	stdinOnce.Do(func() {
		stdinData, stdinErr = io.ReadAll(stdinReader)
	})
	if stdinErr != nil {
		return nil, fmt.Errorf("failed to read manifest from stdin: %w", stdinErr)
	}
	return stdinData, nil
}

// resetStdinManifest makes the next readStdinManifest read from r (tests)
func resetStdinManifest(r io.Reader) {
	stdinReader = r
	stdinOnce = sync.Once{}
	stdinData, stdinErr = nil, nil
}

// manifestDisplayName returns the name of a manifest path given on the
// command line for the output
func manifestDisplayName(path string) string {
	if isStdinManifest(path) {
		return stdinManifestName
	}
	return path
}
