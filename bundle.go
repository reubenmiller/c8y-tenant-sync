package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// A bundle is an archive (.tar.gz, .tgz or .zip) with a manifest and the
// files it uses (included manifests, certificates, packages, scripts). It
// is included like a manifest: from a local path, a URL or a GitHub
// release asset.

const (
	// BundleMarkerFile names the entry manifest of a bundle
	BundleMarkerFile = ".tenant-sync-bundle.yaml"

	// maxBundleSize limits the size of a downloaded bundle archive
	maxBundleSize = 2 << 30
	// maxBundleExtractedSize limits the size of the files of a bundle
	maxBundleExtractedSize = 4 << 30
	// maxBundleFiles limits the number of files of a bundle
	maxBundleFiles = 20000
)

// bundleMarker is the content of BundleMarkerFile
type bundleMarker struct {
	Manifest string `yaml:"manifest"`
}

// IncludeGitHub selects a bundle from a GitHub release: exactly one asset
// of exactly one release
type IncludeGitHub struct {
	Repo               string     `yaml:"repo" json:"repo"`
	Release            string     `yaml:"release" json:"release,omitempty"`
	Tags               []string   `yaml:"tags" json:"tags,omitempty"`
	Assets             []string   `yaml:"assets" json:"assets,omitempty"`
	IncludePrereleases bool       `yaml:"includePrereleases" json:"includePrereleases,omitempty"`
	Token              string     `yaml:"token" json:"token,omitempty"`
	TokenFrom          *SecretRef `yaml:"tokenFrom" json:"tokenFrom,omitempty"`
}

// validate checks a GitHub bundle include
func (g *IncludeGitHub) validate() error {
	var errs []string
	if parts := strings.Split(g.Repo, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		errs = append(errs, fmt.Sprintf("github.repo %q must have the form owner/name", g.Repo))
	}
	if g.Release == "all" || isLatestN(g.Release) {
		errs = append(errs, fmt.Sprintf("github.release %q selects several releases; a bundle needs latest or a tag", g.Release))
	}
	if g.Token != "" && g.TokenFrom != nil {
		errs = append(errs, "github.token and github.tokenFrom are mutually exclusive")
	}
	if g.TokenFrom != nil {
		if g.TokenFrom.Generate != "" {
			errs = append(errs, "github.tokenFrom of an include supports env only")
		}
		errs = append(errs, g.TokenFrom.validate("github.tokenFrom")...)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// isLatestN reports whether a release selector is "latest-N"
func isLatestN(selector string) bool {
	_, ok := latestCount(selector)
	return ok
}

// source returns the GitHub source used to look up the release
func (g *IncludeGitHub) source() (*GitHubSource, error) {
	source := &GitHubSource{
		Repo:               g.Repo,
		Release:            g.Release,
		Tags:               g.Tags,
		Assets:             g.Assets,
		IncludePrereleases: g.IncludePrereleases,
		Token:              g.Token,
	}
	if g.TokenFrom != nil {
		value := os.Getenv(g.TokenFrom.Env)
		if value == "" {
			return nil, fmt.Errorf("github.tokenFrom: environment variable %s is not set", g.TokenFrom.Env)
		}
		source.Token = value
	}
	if len(source.Assets) == 0 {
		source.Assets = []string{"*.tar.gz", "*.tgz", "*.zip"}
	}
	return source, nil
}

// isBundleArchive reports whether a path or URL names a bundle archive
func isBundleArchive(name string) bool {
	if isRemoteManifest(name) {
		name = remoteManifestName(name)
	}
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".zip")
}

// extractedBundle is a bundle unpacked into the work directory
type extractedBundle struct {
	// root is the directory the archive was extracted to
	root string
	// entry is the absolute path of the entry manifest
	entry string
	// display names the bundle in the output, e.g. org/repo@v1.0.0/b.tar.gz
	display string
}

// workDir returns the directory bundles are downloaded and extracted to
// (the run's work directory, or a temp directory removed by Cleanup)
func (l *manifestLoader) bundleWorkDir() (string, error) {
	if l.bundleDir != "" {
		return l.bundleDir, nil
	}
	if l.opts.WorkDir != "" {
		l.bundleDir = filepath.Join(l.opts.WorkDir, "bundles")
		if err := os.MkdirAll(l.bundleDir, 0o755); err != nil {
			return "", err
		}
		return l.bundleDir, nil
	}
	dir, err := os.MkdirTemp("", "tenant-sync-bundles-*")
	if err != nil {
		return "", err
	}
	l.bundleDir = dir
	l.tempDirs = append(l.tempDirs, dir)
	return dir, nil
}

// openBundle extracts a bundle archive (once per source) and finds its
// entry manifest
func (l *manifestLoader) openBundle(key, archive, display string, include IncludeSpec) (*extractedBundle, error) {
	if bundle, ok := l.bundles[key]; ok {
		return bundle, nil
	}
	if include.SHA256 != "" {
		if err := verifySHA256(archive, include.SHA256); err != nil {
			return nil, err
		}
	}
	workDir, err := l.bundleWorkDir()
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(workDir, "bundle-*")
	if err != nil {
		return nil, err
	}
	if err := extractBundle(archive, root); err != nil {
		return nil, fmt.Errorf("failed to extract bundle %s: %w", display, err)
	}
	entry, err := findBundleEntry(root, include.Manifest)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", display, err)
	}
	bundle := &extractedBundle{root: root, entry: entry, display: display}
	l.bundles[key] = bundle
	l.bundleRoots[root] = display
	return bundle, nil
}

// localBundle opens a bundle archive from a local file
func (l *manifestLoader) localBundle(archive string, include IncludeSpec) (*extractedBundle, error) {
	return l.openBundle("file:"+archive, archive, l.displayPath(archive), include)
}

// urlBundle downloads and opens a bundle archive from a URL. A missing
// archive (HTTP 404) returns a *remoteNotFoundError.
func (l *manifestLoader) urlBundle(location string, include IncludeSpec) (*extractedBundle, error) {
	key := "url:" + location
	if bundle, ok := l.bundles[key]; ok {
		return bundle, nil
	}
	workDir, err := l.bundleWorkDir()
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(workDir, "download-*-"+path.Base(remoteManifestName(location)))
	if err != nil {
		return nil, err
	}
	file.Close()
	if err := downloadRemoteFile(location, file.Name(), maxBundleSize); err != nil {
		return nil, err
	}
	return l.openBundle(key, file.Name(), location, include)
}

// githubBundle downloads and opens a bundle from a GitHub release asset.
// It returns nil (and no error) when an optional include has no matching
// release or asset.
func (l *manifestLoader) githubBundle(include IncludeSpec) (*extractedBundle, error) {
	source, err := include.GitHub.source()
	if err != nil {
		return nil, err
	}
	workDir, err := l.bundleWorkDir()
	if err != nil {
		return nil, err
	}
	client := l.opts.HTTPClient
	if client == nil {
		client = remoteManifestClient
	}
	resolver := &SourceResolver{WorkDir: filepath.Join(workDir, "github"), HTTPClient: client, GitHubAPI: l.opts.GitHubAPI}

	releases, err := resolver.fetchReleases(source)
	if err != nil {
		if include.Optional && errors.Is(err, ErrNothingToDo) {
			return nil, nil
		}
		return nil, fmt.Errorf("github %s: %w", source.Repo, err)
	}
	if len(releases) > 1 {
		tags := make([]string, 0, len(releases))
		for _, release := range releases {
			tags = append(tags, release.TagName)
		}
		return nil, fmt.Errorf("github %s: a bundle needs exactly one release, found %s (narrow release or tags)", source.Repo, strings.Join(tags, ", "))
	}
	if len(releases) == 0 {
		if include.Optional {
			return nil, nil
		}
		return nil, fmt.Errorf("github %s: no release found", source.Repo)
	}
	release := releases[0]

	var matches []githubAsset
	for _, asset := range release.Assets {
		for _, pattern := range source.Assets {
			if ok, _ := filepath.Match(pattern, asset.Name); ok {
				matches = append(matches, asset)
				break
			}
		}
	}
	switch {
	case len(matches) == 0 && include.Optional:
		return nil, nil
	case len(matches) == 0:
		return nil, fmt.Errorf("github %s@%s: no asset matches %s", source.Repo, release.TagName, strings.Join(source.Assets, ", "))
	case len(matches) > 1:
		names := make([]string, 0, len(matches))
		for _, asset := range matches {
			names = append(names, asset.Name)
		}
		return nil, fmt.Errorf("github %s@%s: a bundle needs exactly one asset, %s match %s (set assets)", source.Repo, release.TagName, strings.Join(source.Assets, ", "), strings.Join(names, ", "))
	}
	asset := matches[0]
	if !isBundleArchive(asset.Name) {
		return nil, fmt.Errorf("github %s@%s: asset %s is not a bundle archive (.tar.gz, .tgz or .zip)", source.Repo, release.TagName, asset.Name)
	}

	display := fmt.Sprintf("%s@%s/%s", source.Repo, release.TagName, asset.Name)
	key := "github:" + display
	if bundle, ok := l.bundles[key]; ok {
		return bundle, nil
	}
	if asset.Size > maxBundleSize {
		return nil, fmt.Errorf("github %s: bundle is larger than %d GiB", display, maxBundleSize>>30)
	}
	archive, err := resolver.downloadAsset(source, release.TagName, asset)
	if err != nil {
		return nil, fmt.Errorf("failed to download bundle %s: %w", display, err)
	}
	return l.openBundle(key, archive, display, include)
}

// downloadRemoteFile downloads a URL to a file. GitHub hosts get the
// GITHUB_TOKEN (or GH_TOKEN).
func downloadRemoteFile(location, target string, limit int64) error {
	req, err := http.NewRequest(http.MethodGet, location, nil)
	if err != nil {
		return err
	}
	if token := githubToken(&GitHubSource{}); token != "" && isGitHubHost(req.URL.Hostname()) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := remoteManifestClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download %s: %w", location, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &remoteNotFoundError{URL: location, Status: resp.Status}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("failed to download %s: %s", location, resp.Status)
	}
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	written, err := io.Copy(out, io.LimitReader(resp.Body, limit+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("failed to download %s: %w", location, err)
	}
	if written > limit {
		return fmt.Errorf("failed to download %s: larger than %d GiB", location, limit>>30)
	}
	return nil
}

// verifySHA256 checks the SHA-256 checksum of a file (hex, optionally
// prefixed with "sha256:")
func verifySHA256(file, want string) error {
	want = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(want), "sha256:"))
	got, err := fileSHA256(file)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: expected sha256 %s, got %s", filepath.Base(file), want, got)
	}
	return nil
}

// fileSHA256 returns the hex SHA-256 checksum of a file
func fileSHA256(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// extractBundle unpacks a .tar.gz, .tgz or .zip archive into dest. Only
// regular files and directories are extracted; entries which would land
// outside dest are errors, and links are skipped.
func extractBundle(archive, dest string) error {
	lower := strings.ToLower(archive)
	if strings.HasSuffix(lower, ".zip") {
		return extractZip(archive, dest)
	}
	return extractTarGz(archive, dest)
}

// bundleExtractor writes the files of an archive below dest within the
// size and file count limits
type bundleExtractor struct {
	dest  string
	files int
	size  int64
}

// target returns the path of an archive entry below dest
func (e *bundleExtractor) target(name string) (string, error) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	if path.IsAbs(normalized) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("entry %q is an absolute path", name)
	}
	if slices.Contains(strings.Split(normalized, "/"), "..") {
		return "", fmt.Errorf("entry %q leaves the bundle directory", name)
	}
	clean := path.Clean("/" + normalized)
	if clean == "/" {
		return "", nil
	}
	return filepath.Join(e.dest, filepath.FromSlash(clean[1:])), nil
}

// write writes a regular file
func (e *bundleExtractor) write(name string, mode os.FileMode, r io.Reader) error {
	target, err := e.target(name)
	if err != nil || target == "" {
		return err
	}
	e.files++
	if e.files > maxBundleFiles {
		return fmt.Errorf("more than %d files", maxBundleFiles)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	remaining := int64(maxBundleExtractedSize) - e.size
	written, err := io.Copy(out, io.LimitReader(r, remaining+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	e.size += written
	if e.size > maxBundleExtractedSize {
		return fmt.Errorf("extracted files are larger than %d GiB", maxBundleExtractedSize>>30)
	}
	return nil
}

// mkdir creates a directory entry
func (e *bundleExtractor) mkdir(name string) error {
	target, err := e.target(name)
	if err != nil || target == "" {
		return err
	}
	return os.MkdirAll(target, 0o755)
}

func extractTarGz(archive, dest string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	extractor := &bundleExtractor{dest: dest}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = extractor.mkdir(header.Name)
		case tar.TypeReg:
			err = extractor.write(header.Name, header.FileInfo().Mode(), reader)
		case tar.TypeXGlobalHeader:
		default:
			slog.Warn("Skipping bundle entry which is not a file or directory", "entry", header.Name)
		}
		if err != nil {
			return err
		}
	}
}

func extractZip(archive, dest string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	extractor := &bundleExtractor{dest: dest}
	for _, file := range reader.File {
		mode := file.Mode()
		switch {
		case mode.IsDir():
			err = extractor.mkdir(file.Name)
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = file.Open(); err == nil {
				err = extractor.write(file.Name, mode, rc)
				rc.Close()
			}
		default:
			slog.Warn("Skipping bundle entry which is not a file or directory", "entry", file.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// findBundleEntry returns the entry manifest of an extracted bundle: the
// given name, else the manifest named in BundleMarkerFile, else tenant.yaml,
// else the only YAML file at the top level. An archive which wraps
// everything in a single directory (e.g. a GitHub source archive) is
// searched in that directory.
func findBundleEntry(root, name string) (string, error) {
	dirs := []string{root}
	if entries, err := os.ReadDir(root); err == nil && len(entries) == 1 && entries[0].IsDir() {
		dirs = append(dirs, filepath.Join(root, entries[0].Name()))
	}

	if name != "" {
		for _, dir := range dirs {
			candidate := filepath.Join(dir, filepath.FromSlash(path.Clean("/" + name)[1:]))
			if isRegularFile(candidate) {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("manifest %s not found in the bundle", name)
	}

	for _, dir := range dirs {
		if raw, err := os.ReadFile(filepath.Join(dir, BundleMarkerFile)); err == nil {
			var marker bundleMarker
			if err := yaml.Unmarshal(raw, &marker); err != nil {
				return "", fmt.Errorf("invalid %s: %w", BundleMarkerFile, err)
			}
			if marker.Manifest == "" {
				return "", fmt.Errorf("%s has no manifest", BundleMarkerFile)
			}
			candidate := filepath.Join(dir, filepath.FromSlash(path.Clean("/" + marker.Manifest)[1:]))
			if !isRegularFile(candidate) {
				return "", fmt.Errorf("manifest %s (from %s) not found in the bundle", marker.Manifest, BundleMarkerFile)
			}
			return candidate, nil
		}
	}
	for _, dir := range dirs {
		if candidate := filepath.Join(dir, "tenant.yaml"); isRegularFile(candidate) {
			return candidate, nil
		}
	}
	for _, dir := range dirs {
		var yamlFiles []string
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			lower := strings.ToLower(entry.Name())
			if entry.Type().IsRegular() && (strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")) {
				yamlFiles = append(yamlFiles, entry.Name())
			}
		}
		switch len(yamlFiles) {
		case 0:
			continue
		case 1:
			return filepath.Join(dir, yamlFiles[0]), nil
		default:
			return "", fmt.Errorf("the bundle has several manifests (%s); set manifest in the include, or add %s", strings.Join(yamlFiles, ", "), BundleMarkerFile)
		}
	}
	return "", errors.New("no manifest found in the bundle (expected tenant.yaml)")
}

// isRegularFile reports whether path is an existing regular file
func isRegularFile(file string) bool {
	info, err := os.Stat(file)
	return err == nil && info.Mode().IsRegular()
}

// isWithin reports whether target is root or below it
func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
