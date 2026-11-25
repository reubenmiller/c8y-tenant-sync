package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// --decrypt (instead of the "decrypt" subcommand) also works with sops < 3.9.
	// Decrypting needs no .sops.yaml, but sops < 3.8 loads the one it finds from
	// its working directory upwards and fails when none of its creation rules
	// matches the file. So sops runs in the temp folder, away from any config
	cmd := exec.Command(executable, "--decrypt", "--input-type", "yaml", "--output-type", "yaml", abs)
	cmd.Dir = os.TempDir()
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
	if isRemoteManifest(path) {
		return readRemoteManifest(path)
	}
	if isStdinManifest(path) {
		return readStdinManifestDecrypted()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	if !isSopsEncrypted(raw) {
		if err := checkSopsValues(path, raw, false); err != nil {
			return nil, err
		}
		return raw, nil
	}
	decrypted, err := decryptSops(path)
	if err != nil {
		return nil, err
	}
	if err := checkSopsValues(path, decrypted, true); err != nil {
		return nil, err
	}
	return decrypted, nil
}

// readRemoteManifest fetches a manifest from a URL, decrypting it with sops
// when it is encrypted
func readRemoteManifest(location string) ([]byte, error) {
	raw, err := fetchRemoteManifest(location)
	if err != nil {
		return nil, err
	}
	if !isSopsEncrypted(raw) {
		if err := checkSopsValues(location, raw, false); err != nil {
			return nil, err
		}
		return raw, nil
	}
	decrypted, err := decryptRemoteSops(location, raw)
	if err != nil {
		return nil, err
	}
	if err := checkSopsValues(location, decrypted, true); err != nil {
		return nil, err
	}
	return decrypted, nil
}

// readStdinManifestDecrypted reads the manifest from stdin, decrypting it
// with sops when it is encrypted
func readStdinManifestDecrypted() ([]byte, error) {
	raw, err := readStdinManifest()
	if err != nil {
		return nil, err
	}
	if !isSopsEncrypted(raw) {
		if err := checkSopsValues(stdinManifestName, raw, false); err != nil {
			return nil, err
		}
		return raw, nil
	}
	decrypted, err := decryptSopsData(stdinManifestName, ".yaml", raw)
	if err != nil {
		return nil, err
	}
	if err := checkSopsValues(stdinManifestName, decrypted, true); err != nil {
		return nil, err
	}
	return decrypted, nil
}

// checkSopsValues fails when a file which is not (or no longer) encrypted
// still has sops encrypted ENC[...] values, which would otherwise be used as
// literal text (e.g. as a token or password):
//   - in a file without a sops section, the values were copied from another
//     file and can't be decrypted
//   - after decrypting, a value was encrypted twice (ENC[...] text pasted
//     into the decrypted file)
func checkSopsValues(path string, raw []byte, decrypted bool) error {
	values := sopsEncryptedValues(raw)
	if len(values) == 0 {
		return nil
	}
	if decrypted {
		return fmt.Errorf("%s has values which are still encrypted after decrypting it, i.e. encrypted twice "+
			"(e.g. ENC[...] text pasted into the decrypted file): %s. Set them to the plain text with sops (e.g. sops %s)",
			path, strings.Join(values, ", "), filepath.Base(path))
	}
	return fmt.Errorf("%s contains sops encrypted values but no sops metadata, so they can't be decrypted: %s. "+
		"An ENC[...] value only decrypts in the file it was encrypted in (with that file's sops section): "+
		"replace the values with the plain text and encrypt the file with sops, or use a secret reference such as tokenFrom: {env: NAME}",
		path, strings.Join(values, ", "))
}

// sopsEncryptedValues returns the locations (YAML path and line) of values
// which are sops encrypted (ENC[...]). Comments are not values, so they
// can mention ENC[...] freely.
func sopsEncryptedValues(raw []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var found []string
	var walk func(node *yaml.Node, path string)
	walk = func(node *yaml.Node, path string) {
		switch node.Kind {
		case yaml.DocumentNode:
			for _, child := range node.Content {
				walk(child, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(node.Content); i += 2 {
				key := node.Content[i].Value
				if path == "" && key == "sops" {
					continue
				}
				child := key
				if path != "" {
					child = path + "." + key
				}
				walk(node.Content[i+1], child)
			}
		case yaml.SequenceNode:
			for i, child := range node.Content {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		case yaml.ScalarNode:
			if strings.HasPrefix(node.Value, "ENC[AES256_GCM,") {
				found = append(found, fmt.Sprintf("%s (line %d)", path, node.Line))
			}
		}
	}
	walk(&doc, "")
	return found
}
