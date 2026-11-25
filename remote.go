package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// maxRemoteManifestSize limits the size of a manifest fetched from a URL
const maxRemoteManifestSize = 10 << 20

// remoteManifestClient fetches manifests given as http(s) URLs (replaced in
// tests)
var remoteManifestClient = &http.Client{Timeout: 60 * time.Second}

// remoteNotFoundError is returned when a remote manifest does not exist
// (HTTP 404), so optional includes can be skipped
type remoteNotFoundError struct {
	URL    string
	Status string
}

func (e *remoteNotFoundError) Error() string {
	return fmt.Sprintf("failed to read manifest: %s: %s", e.URL, e.Status)
}

// isRemoteNotFound reports whether err is the remote manifest location not
// being found (and not a file it includes)
func isRemoteNotFound(err error, location string) bool {
	var notFound *remoteNotFoundError
	return errors.As(err, &notFound) && notFound.URL == location
}

// isRemoteManifest reports whether a manifest path is an http(s) URL
func isRemoteManifest(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// manifestKey returns the identity of a manifest: the URL of a remote
// manifest, "-" for stdin, or the absolute path of a local file
func manifestKey(value string) (string, error) {
	if isStdinManifest(value) {
		return stdinManifest, nil
	}
	if isRemoteManifest(value) {
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err != nil {
			return "", fmt.Errorf("invalid manifest url %q: %w", value, err)
		}
		if parsed.Host == "" {
			return "", fmt.Errorf("invalid manifest url %q: no host", value)
		}
		return parsed.String(), nil
	}
	return filepath.Abs(value)
}

// manifestBaseDir returns the directory which relative local paths (sources,
// hooks, commands) of the main manifest are resolved against: the manifest
// directory, or the current directory for a remote manifest or stdin
func manifestBaseDir(manifestPath string) string {
	if isRemoteManifest(manifestPath) || isStdinManifest(manifestPath) {
		return "."
	}
	return filepath.Dir(manifestPath)
}

// remoteManifestName returns the file name of a remote manifest from its URL
// path, e.g. https://example.com/sync/prod.yaml?ref=main -> prod.yaml
// (tenant.yaml when the path has no name)
func remoteManifestName(value string) string {
	if parsed, err := url.Parse(value); err == nil {
		if name := path.Base(parsed.Path); name != "" && name != "." && name != "/" {
			return name
		}
	}
	return "tenant.yaml"
}

// fetchRemoteManifest downloads a manifest. GitHub hosts get the GITHUB_TOKEN
// (or GH_TOKEN) so manifests in private repositories work.
func fetchRemoteManifest(location string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	if token := githubToken(&GitHubSource{}); token != "" && isGitHubHost(req.URL.Hostname()) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := remoteManifestClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, &remoteNotFoundError{URL: location, Status: resp.Status}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("failed to read manifest: %s: %s", location, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %s: %w", location, err)
	}
	if len(raw) > maxRemoteManifestSize {
		return nil, fmt.Errorf("failed to read manifest: %s is larger than %d MiB", location, maxRemoteManifestSize>>20)
	}
	return raw, nil
}

// isGitHubHost reports whether the host serves GitHub repository content
func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || host == "raw.githubusercontent.com" || host == "api.github.com"
}

// decryptRemoteSops decrypts a sops encrypted remote manifest via a
// temporary file (sops decrypts files)
func decryptRemoteSops(location string, raw []byte) ([]byte, error) {
	return decryptSopsData(location, path.Ext(remoteManifestName(location)), raw)
}

// decryptSopsData decrypts sops encrypted content which is not in a local
// file (a remote manifest or stdin) via a temporary file with the extension
// ext (sops decrypts files and picks the format by the extension). Errors
// name the content by name instead of the temporary file.
func decryptSopsData(name, ext string, raw []byte) ([]byte, error) {
	if ext == "" {
		ext = ".yaml"
	}
	file, err := os.CreateTemp("", "tenant-sync-*"+ext)
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	decrypted, err := decryptSops(file.Name())
	if err != nil {
		return nil, errors.New(strings.ReplaceAll(err.Error(), file.Name(), name))
	}
	return decrypted, nil
}

// resolveRemoteInclude returns the URL of an include entry of a remote
// manifest: relative paths are resolved against the URL of the including
// manifest. Local files and glob patterns can't be included by a remote
// manifest.
func resolveRemoteInclude(base string, include IncludeSpec) (string, error) {
	value := strings.TrimSpace(include.Path)
	if isRemoteManifest(value) {
		return manifestKey(value)
	}
	if strings.ContainsAny(value, "*?[") {
		return "", fmt.Errorf("glob pattern %s is not supported in a remote manifest (list the files)", include.Path)
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "://") {
		return "", fmt.Errorf("%s: a remote manifest can only include relative paths or http(s) urls", include.Path)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(filepath.ToSlash(value))
	if err != nil {
		return "", fmt.Errorf("invalid include path %q: %w", include.Path, err)
	}
	return baseURL.ResolveReference(ref).String(), nil
}

// forEachSource calls fn with every source of the manifest
func (m *Manifest) forEachSource(fn func(label string, source *Source)) {
	for i := range m.TrustedCertificates {
		fn(fmt.Sprintf("trustedCertificates[%d]", i), &m.TrustedCertificates[i].Source)
	}
	for i := range m.CertificateRevocationLists {
		fn(fmt.Sprintf("certificateRevocationLists[%d]", i), &m.CertificateRevocationLists[i].Source)
	}
	for i := range m.Microservices {
		if m.Microservices[i].Source != nil {
			fn(fmt.Sprintf("microservices[%d]", i), m.Microservices[i].Source)
		}
	}
	for i := range m.Extensions {
		fn(fmt.Sprintf("extensions[%d]", i), &m.Extensions[i].Source)
	}
	for i := range m.Applications {
		if m.Applications[i].Source != nil {
			fn(fmt.Sprintf("applications[%d]", i), m.Applications[i].Source)
		}
	}
	for i := range m.Software {
		fn(fmt.Sprintf("software[%d]", i), &m.Software[i].Source)
	}
	for i := range m.Firmware {
		fn(fmt.Sprintf("firmware[%d]", i), &m.Firmware[i].Source)
	}
	for i := range m.Configuration {
		fn(fmt.Sprintf("configuration[%d]", i), &m.Configuration[i].Source)
	}
	for i := range m.SmartRestTemplates {
		fn(fmt.Sprintf("smartrestTemplates[%d]", i), &m.SmartRestTemplates[i].Source)
	}
	for i := range m.SmartFunctions {
		fn(fmt.Sprintf("smartFunctions[%d]", i), &m.SmartFunctions[i].Source)
		if file := m.SmartFunctions[i].CodeFile; file != "" {
			// A local file like a source path (changes are not kept)
			fn(fmt.Sprintf("smartFunctions[%d].codeFile", i), &Source{Path: file})
		}
	}
	if m.DigitalTwin != nil {
		sections := []struct {
			name string
			defs []DigitalTwinDefinitionSpec
		}{
			{"digitalTwin.propertyDefinitions", m.DigitalTwin.PropertyDefinitions},
			{"digitalTwin.assetDefinitions", m.DigitalTwin.AssetDefinitions},
		}
		for _, section := range sections {
			for i := range section.defs {
				if section.defs[i].Source != nil {
					fn(fmt.Sprintf("%s[%d]", section.name, i), section.defs[i].Source)
				}
			}
		}
	}
}

// checkRemoteManifest rejects what a manifest fetched from a URL or a
// GitHub release can't (or may not) use:
//   - local source paths: outside a bundle there is no directory to resolve
//     them against, and inside a bundle they must stay in the bundle
//   - hooks and commands (shell commands), unless allowed
func (m *Manifest) checkRemoteManifest(origin manifestOrigin, dir string, allowCommands bool) error {
	var errs []string
	m.forEachSource(func(label string, source *Source) {
		switch {
		case source.Path == "":
		case origin.bundleRoot == "":
			errs = append(errs, fmt.Sprintf("%s: source.path %q is not supported in a remote manifest (use url, github or cloudsmith, or publish a bundle)", label, source.Path))
		case filepath.IsAbs(source.Path) || !isWithin(origin.bundleRoot, filepath.Join(dir, source.Path)):
			errs = append(errs, fmt.Sprintf("%s: source.path %q is outside the bundle", label, source.Path))
		}
	})

	if !allowCommands {
		hasHooks := len(m.Hooks.Pre) > 0 || len(m.Hooks.Post) > 0
		for _, section := range m.Hooks.Sections {
			hasHooks = hasHooks || len(section.Pre) > 0 || len(section.Post) > 0
		}
		if hasHooks {
			errs = append(errs, "hooks: a remote manifest runs no shell commands unless --allow-remote-commands is set")
		}
		if len(m.Commands) > 0 {
			errs = append(errs, "commands: a remote manifest runs no shell commands unless --allow-remote-commands is set")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid remote manifest:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
