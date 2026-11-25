package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNothingToDo marks resolution failures that mean "no artifacts available"
// (missing path, no matching files or assets) rather than a hard error.
// Sources marked optional are skipped instead of failing on these.
var ErrNothingToDo = errors.New("nothing to do")

// ResolvedFile is a single artifact produced by resolving a Source.
// Either Path (a local file, possibly downloaded to a temp dir) or URL
// (external reference, no binary upload) is set.
type ResolvedFile struct {
	Path     string
	Filename string
	URL      string

	// VersionHint is a version derived from the source itself (e.g. the
	// GitHub release tag) used as a fallback when the filename does not
	// contain a version.
	VersionHint string

	// Package is the metadata of a package from a package repository
	// (cloudsmith), used instead of parsing the filename
	Package *PackageInfo
}

// SourceResolver resolves manifest sources into local files / URL references
type SourceResolver struct {
	// BaseDir is the directory relative local paths are resolved against
	// (the directory containing the manifest file)
	BaseDir string

	// WorkDir is where remote artifacts are downloaded (a temp dir)
	WorkDir string

	// SkipDownload lists remote assets without downloading them (dry-run)
	SkipDownload bool

	// HTTPClient used for GitHub API and asset downloads
	HTTPClient *http.Client

	// CloudsmithAPI overrides the Cloudsmith API base URL (tests)
	CloudsmithAPI string

	// GitHubAPI overrides the GitHub API base URL (tests)
	GitHubAPI string
}

func NewSourceResolver(baseDir, workDir string, skipDownload bool) *SourceResolver {
	return &SourceResolver{
		BaseDir:      baseDir,
		WorkDir:      workDir,
		SkipDownload: skipDownload,
		HTTPClient:   &http.Client{Timeout: 30 * time.Minute},
	}
}

// Resolve returns the artifacts described by the source
func (r *SourceResolver) Resolve(source Source) ([]ResolvedFile, error) {
	switch {
	case source.Path != "":
		return r.resolveLocal(source)
	case source.URL != "":
		return []ResolvedFile{{
			URL:      source.URL,
			Filename: filepath.Base(source.URL),
		}}, nil
	case source.GitHub != nil:
		return r.resolveGitHub(source.GitHub)
	case source.Cloudsmith != nil:
		return r.resolveCloudsmith(source.Cloudsmith)
	default:
		return nil, fmt.Errorf("source is empty")
	}
}

func (r *SourceResolver) resolveLocal(source Source) ([]ResolvedFile, error) {
	// Relative paths are resolved against the manifest directory so the
	// manifest works regardless of the current working directory
	path := source.Path
	if !filepath.IsAbs(path) && r.BaseDir != "" {
		path = filepath.Join(r.BaseDir, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("path not found: %s: %w", path, ErrNothingToDo)
	}

	if !info.IsDir() {
		return []ResolvedFile{{
			Path:     path,
			Filename: filepath.Base(path),
		}}, nil
	}

	patterns := source.Patterns
	if len(patterns) == 0 {
		patterns = []string{"*"}
	}

	var files []ResolvedFile
	err = filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		filename := filepath.Base(path)
		for _, pattern := range patterns {
			matched, err := filepath.Match(pattern, filename)
			if err != nil {
				return err
			}
			if matched {
				files = append(files, ResolvedFile{Path: path, Filename: filename})
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files in %s matched patterns: %s: %w", path, strings.Join(patterns, ", "), ErrNothingToDo)
	}
	return files, nil
}

// GitHub release API models (subset)
type githubRelease struct {
	TagName    string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	URL                string `json:"url"` // API URL, download with Accept: application/octet-stream
	BrowserDownloadURL string `json:"browser_download_url"`
	// Digest is the checksum GitHub computed for the asset, e.g.
	// "sha256:<hex>" (empty for assets uploaded before GitHub added it)
	Digest string `json:"digest"`
}

func (r *SourceResolver) resolveGitHub(source *GitHubSource) ([]ResolvedFile, error) {
	releases, err := r.fetchReleases(source)
	if err != nil {
		return nil, err
	}

	assetPatterns := source.Assets
	if len(assetPatterns) == 0 {
		assetPatterns = []string{"*"}
	}

	var files []ResolvedFile
	for _, release := range releases {
		versionHint := strings.TrimPrefix(tagVersion(release.TagName, source.Tags), "v")

		for _, asset := range release.Assets {
			matched := false
			for _, pattern := range assetPatterns {
				if ok, err := filepath.Match(pattern, asset.Name); err == nil && ok {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}

			if source.IsLinkOnly() {
				files = append(files, ResolvedFile{
					URL:         asset.BrowserDownloadURL,
					Filename:    asset.Name,
					VersionHint: versionHint,
				})
				continue
			}

			if r.SkipDownload {
				// Dry-run: record the asset without downloading it
				files = append(files, ResolvedFile{
					Path:        filepath.Join(r.WorkDir, asset.Name),
					Filename:    asset.Name,
					VersionHint: versionHint,
				})
				continue
			}

			localPath, err := r.downloadAsset(source, release.TagName, asset)
			if err != nil {
				return nil, fmt.Errorf("failed to download %s from %s@%s: %w", asset.Name, source.Repo, release.TagName, err)
			}
			files = append(files, ResolvedFile{
				Path:        localPath,
				Filename:    asset.Name,
				VersionHint: versionHint,
			})
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no assets in %s (release: %s) matched patterns: %s: %w",
			source.Repo, releaseSelector(source), strings.Join(assetPatterns, ", "), ErrNothingToDo)
	}
	return files, nil
}

func releaseSelector(source *GitHubSource) string {
	if source.Release == "" {
		return "latest"
	}
	return source.Release
}

// latestCountPattern matches "latest-N" selectors, e.g. "latest-5"
var latestCountPattern = regexp.MustCompile(`^latest-([1-9]\d*)$`)

// latestCount returns N when the selector has the form "latest-N"
func latestCount(selector string) (int, bool) {
	match := latestCountPattern.FindStringSubmatch(selector)
	if match == nil {
		return 0, false
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// filterReleases removes drafts (and prereleases unless included) and limits
// the result to the first `limit` releases (0 = no limit). GitHub returns
// releases ordered newest first.
func filterReleases(releases []githubRelease, includePrereleases bool, limit int) []githubRelease {
	var selected []githubRelease
	for _, release := range releases {
		if release.Draft {
			continue
		}
		if release.Prerelease && !includePrereleases {
			continue
		}
		selected = append(selected, release)
		if limit > 0 && len(selected) >= limit {
			break
		}
	}
	return selected
}

func (r *SourceResolver) fetchReleases(source *GitHubSource) ([]githubRelease, error) {
	selector := releaseSelector(source)

	// Selectors that need the full release list: "all", "latest-N", and
	// "latest" with prereleases (the /releases/latest endpoint never returns
	// prereleases)
	limit := 0
	listNeeded := false
	switch {
	case selector == "all":
		listNeeded = true
	case selector == "latest" && source.IncludePrereleases:
		listNeeded = true
		limit = 1
	default:
		if n, ok := latestCount(selector); ok {
			listNeeded = true
			limit = n
		}
	}

	if len(source.Tags) > 0 {
		return r.fetchTaggedReleases(source, selector)
	}

	if listNeeded {
		releases, err := r.listReleases(source)
		if err != nil {
			return nil, err
		}
		selected := filterReleases(releases, source.IncludePrereleases, limit)
		if len(selected) == 0 {
			return nil, fmt.Errorf("no releases found in %s: %w", source.Repo, ErrNothingToDo)
		}
		return selected, nil
	}

	if selector == "latest" {
		release := githubRelease{}
		if err := r.githubGet(source, fmt.Sprintf("%s/repos/%s/releases/latest", r.githubAPI(), source.Repo), &release); err != nil {
			return nil, err
		}
		return []githubRelease{release}, nil
	}

	// A specific tag
	release := githubRelease{}
	if err := r.githubGet(source, fmt.Sprintf("%s/repos/%s/releases/tags/%s", r.githubAPI(), source.Repo, selector), &release); err != nil {
		return nil, err
	}
	return []githubRelease{release}, nil
}

func (r *SourceResolver) listReleases(source *GitHubSource) ([]githubRelease, error) {
	var releases []githubRelease
	err := r.githubGet(source, fmt.Sprintf("%s/repos/%s/releases?per_page=100", r.githubAPI(), source.Repo), &releases)
	return releases, err
}

// githubReleasesPageSize is the number of releases per page (the maximum)
const githubReleasesPageSize = 100

// fetchTaggedReleases selects releases per tag pattern: the latest,
// latest-N or all releases whose tag matches each pattern (newest first,
// as GitHub lists them), and per component for patterns with {component}.
// The release list is read page by page until every pattern has its
// releases (to the end for {component}, since any page can have a new
// component).
func (r *SourceResolver) fetchTaggedReleases(source *GitHubSource, selector string) ([]githubRelease, error) {
	patterns, err := compileTagPatterns(source.Tags)
	if err != nil {
		return nil, err
	}
	limit := 0 // all
	if selector == "latest" {
		limit = 1
	} else if n, ok := latestCount(selector); ok {
		limit = n
	}

	// counts are the selected releases by pattern (and component)
	counts := map[string]int{}
	complete := func() bool {
		if limit == 0 {
			return false
		}
		for i, pattern := range patterns {
			if pattern.component > 0 || counts[fmt.Sprint(i)] < limit {
				return false
			}
		}
		return true
	}

	var selected []githubRelease
	for page := 1; ; page++ {
		var releases []githubRelease
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", r.githubAPI(), source.Repo, githubReleasesPageSize, page)
		if err := r.githubGet(source, url, &releases); err != nil {
			return nil, err
		}
		for _, release := range filterReleases(releases, source.IncludePrereleases, 0) {
			wanted := false
			for i, pattern := range patterns {
				component, _, ok := pattern.Match(release.TagName)
				if !ok {
					continue
				}
				key := fmt.Sprint(i)
				if pattern.component > 0 {
					key += "\x00" + component
				}
				if limit == 0 || counts[key] < limit {
					counts[key]++
					wanted = true
				}
			}
			if wanted {
				selected = append(selected, release)
			}
		}
		if len(releases) < githubReleasesPageSize || complete() {
			break
		}
	}

	if len(selected) == 0 {
		return nil, fmt.Errorf("no releases in %s matched tags: %s: %w", source.Repo, strings.Join(source.Tags, ", "), ErrNothingToDo)
	}
	return selected, nil
}

// tagVersion returns the version part of a release tag: the tag from the
// first wildcard of the tag pattern it matches (e.g. c8y-deploy-poll-v0.6.1
// with the pattern c8y-deploy-poll-v* or {component}-v* gives 0.6.1)
func tagVersion(tag string, patterns []string) string {
	compiled, err := compileTagPatterns(patterns)
	if err != nil {
		return tag
	}
	for _, pattern := range compiled {
		if _, version, ok := pattern.Match(tag); ok {
			return version
		}
	}
	return tag
}

// githubAPI returns the GitHub API base URL
func (r *SourceResolver) githubAPI() string {
	if r.GitHubAPI != "" {
		return strings.TrimSuffix(r.GitHubAPI, "/")
	}
	return "https://api.github.com"
}

func githubToken(source *GitHubSource) string {
	if source.Token != "" && !strings.HasPrefix(source.Token, "${") {
		return source.Token
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token
	}
	return os.Getenv("GH_TOKEN")
}

func (r *SourceResolver) githubGet(source *GitHubSource, url string, out any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token := githubToken(source); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github api request failed: %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// downloadAsset downloads a release asset to the resolver work directory.
// Files are placed under {workdir}/{repo}/{tag}/ so the same asset name from
// different releases does not collide.
func (r *SourceResolver) downloadAsset(source *GitHubSource, tag string, asset githubAsset) (string, error) {
	targetDir := filepath.Join(r.WorkDir, strings.ReplaceAll(source.Repo, "/", "_"), tag)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}
	targetPath := filepath.Join(targetDir, asset.Name)

	slog.Info("Downloading release asset",
		"repo", source.Repo,
		"tag", tag,
		"asset", asset.Name,
		"size", asset.Size)

	// Use the API asset URL with the octet-stream accept header so private
	// repositories work too (browser_download_url does not accept tokens).
	req, err := http.NewRequest(http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token := githubToken(source); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hash), resp.Body); err != nil {
		os.Remove(targetPath)
		return "", err
	}

	// Verify the download against the checksum GitHub published
	if want, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok {
		if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, want) {
			os.Remove(targetPath)
			return "", fmt.Errorf("checksum mismatch: expected sha256 %s, got %s", want, got)
		}
	}

	return targetPath, nil
}

// Fetch downloads a file which only references a URL (a url source or a
// linkOnly asset) into the work directory, for sections which need the file
// content. Local files are returned as they are, and nothing is downloaded
// when SkipDownload is set (dry-run).
func (r *SourceResolver) Fetch(file ResolvedFile) (ResolvedFile, error) {
	if file.Path != "" || file.URL == "" || r.SkipDownload {
		return file, nil
	}
	name := file.Filename
	if parsed, err := url.Parse(file.URL); err == nil {
		if base := path.Base(parsed.Path); base != "" && base != "." && base != "/" {
			name = base
		}
	}
	// URLs with the same filename (e.g. one per branch) must not overwrite
	// each other
	sum := sha256.Sum256([]byte(file.URL))
	dir := filepath.Join(r.WorkDir, "url", hex.EncodeToString(sum[:])[:12])
	target := filepath.Join(dir, name)
	if err := r.downloadURL(file.URL, dir, target); err != nil {
		return file, fmt.Errorf("failed to download %s: %w", file.URL, err)
	}
	file.Path = target
	file.Filename = name
	return file, nil
}
