package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvSopsBinary overrides the sops executable used to decrypt manifests
const EnvSopsBinary = "TENANT_SYNC_SOPS"

// sopsMetadata is the "sops" section which sops adds to encrypted files
type sopsMetadata struct {
	Sops *struct {
		Mac     string `yaml:"mac"`
		Version string `yaml:"version"`
	} `yaml:"sops"`
}

// isSopsEncrypted reports whether a YAML document was encrypted with sops
// (it has a top-level "sops" section with a MAC)
func isSopsEncrypted(raw []byte) bool {
	var meta sopsMetadata
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return false
	}
	return meta.Sops != nil && meta.Sops.Mac != ""
}

// sopsBinary returns the sops executable to use
func sopsBinary() string {
	if value := strings.TrimSpace(os.Getenv(EnvSopsBinary)); value != "" {
		return value
	}
	return "sops"
}

// decryptSops decrypts a sops encrypted YAML file with the sops CLI, so all
// key types supported by sops (age, PGP, cloud KMS, Vault) work with the
// user's existing sops configuration (e.g. SOPS_AGE_KEY_FILE)
func decryptSops(path string) ([]byte, error) {
	binary := sopsBinary()
	executable, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("%s is encrypted with sops, but the sops executable was not found (install it from https://github.com/getsops/sops or set %s): %w", path, EnvSopsBinary, err)
	}

	// --decrypt (instead of the "decrypt" subcommand) also works with sops < 3.9
	cmd := exec.Command(executable, "--decrypt", "--input-type", "yaml", "--output-type", "yaml", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && detail != "" {
			return nil, fmt.Errorf("failed to decrypt %s with sops: %s", path, detail)
		}
		return nil, fmt.Errorf("failed to decrypt %s with sops: %w", path, err)
	}
	return stdout.Bytes(), nil
}

// readManifestFile reads a manifest, decrypting it first when it is
// encrypted with sops
func readManifestFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	if !isSopsEncrypted(raw) {
		return raw, nil
	}
	return decryptSops(path)
}
