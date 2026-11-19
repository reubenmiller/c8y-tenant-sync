package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Generated values are referenced in a manifest as ${generate:NAME} or
// ${generate:NAME:LENGTH}. Each name gets a random value once; the value is
// stored in the secrets file and reused on later runs, so runs stay
// idempotent (e.g. a tenant option is not changed on every run).
const (
	generatePrefix        = "generate:"
	defaultGenerateLength = 24
	minGenerateLength     = 12
	maxGenerateLength     = 128
)

// SecretsEncryption controls whether the secrets file is encrypted with sops
type SecretsEncryption string

const (
	// SecretsEncryptionAuto encrypts with sops when it is installed and a
	// .sops.yaml with creation rules is found
	SecretsEncryptionAuto SecretsEncryption = "auto"
	// SecretsEncryptionSops requires sops encryption
	SecretsEncryptionSops SecretsEncryption = "sops"
	// SecretsEncryptionNone writes a plain file (permissions 0600)
	SecretsEncryptionNone SecretsEncryption = "none"
)

// SecretsEncryptionModes are the valid --secrets-encryption values
var SecretsEncryptionModes = []string{string(SecretsEncryptionAuto), string(SecretsEncryptionSops), string(SecretsEncryptionNone)}

var generateNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// generateRef is a parsed ${generate:NAME[:LENGTH]} reference
type generateRef struct {
	Name   string
	Length int
}

// parseGenerateRef parses the part of a ${generate:...} reference after
// "generate:"
func parseGenerateRef(value string) (generateRef, error) {
	ref := generateRef{Length: defaultGenerateLength}
	name, length, hasLength := strings.Cut(value, ":")
	if !generateNamePattern.MatchString(name) {
		return ref, fmt.Errorf("invalid name %q in ${generate:%s}: use letters, numbers, '.', '_' and '-'", name, value)
	}
	ref.Name = name
	if hasLength {
		n, err := strconv.Atoi(length)
		if err != nil || n < minGenerateLength || n > maxGenerateLength {
			return ref, fmt.Errorf("invalid length %q in ${generate:%s}: use a number from %d to %d", length, value, minGenerateLength, maxGenerateLength)
		}
		ref.Length = n
	}
	return ref, nil
}

// Characters of generated values. They are safe in YAML (quoted or not),
// are not "$" (environment expansion), and the special characters satisfy
// password policies which require them. Values start with a letter, so they
// never start a YAML token.
const (
	generateLower   = "abcdefghijkmnopqrstuvwxyz" // without l
	generateUpper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"  // without I and O
	generateDigits  = "23456789"                  // without 0 and 1
	generateSpecial = "-_.+=^~"
)

// generateValue returns a random value with at least one lowercase and one
// uppercase letter, a digit and a special character
func generateValue(length int) (string, error) {
	if length < minGenerateLength {
		length = minGenerateLength
	}
	all := generateLower + generateUpper + generateDigits + generateSpecial
	pick := func(set string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[n.Int64()], nil
	}

	for {
		out := make([]byte, length)
		var err error
		if out[0], err = pick(generateLower + generateUpper); err != nil {
			return "", err
		}
		for i := 1; i < length; i++ {
			if out[i], err = pick(all); err != nil {
				return "", err
			}
		}
		value := string(out)
		if strings.ContainsAny(value, generateLower) && strings.ContainsAny(value, generateUpper) &&
			strings.ContainsAny(value, generateDigits) && strings.ContainsAny(value, generateSpecial) {
			return value, nil
		}
	}
}

// SecretEntry is a generated value in the secrets file
type SecretEntry struct {
	Value       string `yaml:"value"`
	GeneratedAt string `yaml:"generatedAt"`
	Host        string `yaml:"host,omitempty"`
	Tenant      string `yaml:"tenant,omitempty"`
}

type secretsFile struct {
	Secrets map[string]SecretEntry `yaml:"secrets"`
}

// SecretStore holds the generated values of a manifest, backed by the
// secrets file
type SecretStore struct {
	Path       string
	Encryption SecretsEncryption
	// Host and Tenant are recorded with new values
	Host   string
	Tenant string
	// DryRun leaves the values unchanged: missing values are not generated
	// and the file is not written
	DryRun bool

	mu        sync.Mutex
	entries   map[string]SecretEntry
	generated []string
	encrypted bool
}

// DefaultSecretsPath returns the secrets file of a manifest, e.g.
// tenant.yaml -> tenant.secrets.yaml
func DefaultSecretsPath(manifestPath string) string {
	ext := filepath.Ext(manifestPath)
	return strings.TrimSuffix(manifestPath, ext) + ".secrets.yaml"
}

// OpenSecretStore reads the secrets file (decrypting it with sops when it is
// encrypted). A missing file is an empty store.
func OpenSecretStore(path string, encryption SecretsEncryption) (*SecretStore, error) {
	store := &SecretStore{Path: path, Encryption: encryption, entries: map[string]SecretEntry{}}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "⚠ %s can be read by other users (permissions %v); it should be 0600\n", path, info.Mode().Perm())
	}

	if isSopsEncrypted(raw) {
		if encryption == SecretsEncryptionNone {
			return nil, fmt.Errorf("%s is encrypted with sops, but --secrets-encryption is none", path)
		}
		store.encrypted = true
		if raw, err = decryptSops(path); err != nil {
			return nil, err
		}
	}

	var file secretsFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("failed to parse secrets file %s: %w", path, err)
	}
	for name, entry := range file.Secrets {
		store.entries[name] = entry
	}
	return store, nil
}

// Value returns the value of a generated reference: the stored value, or a
// new random value. In dry run mode, a missing value is returned as a
// <generated:NAME> placeholder.
func (s *SecretStore) Value(ref generateRef) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[ref.Name]; ok {
		return entry.Value, nil
	}
	if s.DryRun {
		return "<generated:" + ref.Name + ">", nil
	}
	value, err := generateValue(ref.Length)
	if err != nil {
		return "", fmt.Errorf("failed to generate %s: %w", ref.Name, err)
	}
	s.entries[ref.Name] = SecretEntry{Value: value, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Host: s.Host, Tenant: s.Tenant}
	s.generated = append(s.generated, ref.Name)
	return value, nil
}

// SetTenant records the tenant ID, also on the values generated in this
// run which don't have one yet (when the tenant is only known after the
// manifest was loaded)
func (s *SecretStore) SetTenant(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tenant = tenant
	for _, name := range s.generated {
		if entry := s.entries[name]; entry.Tenant == "" {
			entry.Tenant = tenant
			s.entries[name] = entry
		}
	}
}

// Values returns all stored values (to mask them in the output)
func (s *SecretStore) Values() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]string, 0, len(s.entries))
	for _, entry := range s.entries {
		values = append(values, entry.Value)
	}
	return values
}

// Generated returns the names of the values generated in this run
func (s *SecretStore) Generated() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := append([]string(nil), s.generated...)
	sort.Strings(names)
	return names
}

// IsGenerated reports whether a value was generated in this run, and its name
func (s *SecretStore) IsGenerated(value string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.generated {
		if s.entries[name].Value == value {
			return name, true
		}
	}
	return "", false
}

// NameOf returns the name of a stored value
func (s *SecretStore) NameOf(value string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, entry := range s.entries {
		if entry.Value == value {
			return name, true
		}
	}
	return "", false
}

// Save writes the secrets file when values were generated. The file is
// written with permissions 0600 and then encrypted with sops (depending on
// the encryption mode). Values are never lost: if encryption fails, the file
// stays unencrypted (0600) and an error is returned.
func (s *SecretStore) Save() (encrypted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DryRun || len(s.generated) == 0 {
		return s.encrypted, nil
	}

	useSops, err := s.shouldEncrypt()
	if err != nil {
		return false, err
	}

	content, err := yaml.Marshal(secretsFile{Secrets: s.entries})
	if err != nil {
		return false, err
	}
	header := "# Generated values of ${generate:NAME} references, reused on every run.\n# Keep this file secret: do not commit it unless it is encrypted with sops.\n"
	if err := writeFileAtomic(s.Path, append([]byte(header), content...), 0o600); err != nil {
		return false, fmt.Errorf("failed to write secrets file: %w", err)
	}
	if !useSops {
		return false, nil
	}
	if err := encryptSopsInPlace(s.Path); err != nil {
		return false, fmt.Errorf("the secrets were saved unencrypted to %s (permissions 0600) because sops encryption failed: %w", s.Path, err)
	}
	return true, nil
}

// shouldEncrypt decides whether the secrets file is encrypted with sops
func (s *SecretStore) shouldEncrypt() (bool, error) {
	switch s.Encryption {
	case SecretsEncryptionNone:
		return false, nil
	case SecretsEncryptionSops:
		if _, err := exec.LookPath(sopsBinary()); err != nil {
			return false, fmt.Errorf("--secrets-encryption sops requires the sops executable: %w", err)
		}
		if findSopsConfig(filepath.Dir(s.Path)) == "" {
			return false, fmt.Errorf("--secrets-encryption sops requires a .sops.yaml with creation rules (in %s or a parent folder)", filepath.Dir(s.Path))
		}
		return true, nil
	default:
		if s.encrypted {
			// An encrypted file is kept encrypted (fails below if it can't be)
			return true, nil
		}
		if _, err := exec.LookPath(sopsBinary()); err != nil {
			return false, nil
		}
		return findSopsConfig(filepath.Dir(s.Path)) != "", nil
	}
}

// findSopsConfig returns the .sops.yaml which sops uses for files in dir (in
// dir or a parent folder), or "" when there is none
func findSopsConfig(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		for _, name := range []string{".sops.yaml", ".sops.yml"} {
			candidate := filepath.Join(abs, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// encryptSopsInPlace encrypts a file with sops using the creation rules of
// the .sops.yaml which applies to it
func encryptSopsInPlace(path string) error {
	executable, err := exec.LookPath(sopsBinary())
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// sops finds .sops.yaml from its working directory upwards, and matches
	// the creation rules' path_regex against the file path
	cmd := exec.Command(executable, "--encrypt", "--in-place", "--input-type", "yaml", "--output-type", "yaml", abs)
	cmd.Dir = filepath.Dir(abs)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return fmt.Errorf("%s", detail)
		}
		return err
	}
	return nil
}

// writeFileAtomic writes a file via a temporary file in the same folder
func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
