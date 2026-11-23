package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CloudsmithSource selects packages from a Cloudsmith repository, e.g. the
// public thin-edge.io repositories (thinedge/tedge-release, thinedge/tedge-main
// and thinedge/community)
type CloudsmithSource struct {
	// Repo is "owner/repository", or the URL of the repository (e.g. its
	// broadcast page https://broadcasts.cloudsmith.com/thinedge/tedge-release)
	Repo string `yaml:"repo" json:"repo" jsonschema:"description=Repository as owner/repository or its URL (e.g. https://broadcasts.cloudsmith.com/thinedge/tedge-release),example=thinedge/tedge-release"`

	// Packages are package names or glob patterns
	Packages []string `yaml:"packages" json:"packages" jsonschema:"description=Package names or glob patterns (e.g. tedge or tedge-*). Exact names are faster: the repository is then only read until the selected versions are found while a glob reads every package whose name starts with the part before the first wildcard,minItems=1"`

	// Formats limits the package formats (default: deb, rpm, alpine and raw)
	Formats []string `yaml:"formats" json:"formats,omitempty" jsonschema:"description=Package formats (default: all of them),enum=deb,enum=rpm,enum=alpine,enum=raw"`

	// Architectures limits the package architectures (default: all)
	Architectures []string `yaml:"architectures" json:"architectures,omitempty" jsonschema:"description=Package architectures as named by the package format (e.g. arm64 and amd64 for deb or aarch64 and x86_64 for rpm and alpine); default: all,example=arm64"`

	// Version selects the versions of each package and format
	Version string `yaml:"version" json:"version,omitempty" jsonschema:"description=Which versions of each package and format to use: latest (default) or latest-N for the latest N versions or all or an exact version,example=latest,example=latest-3,example=2.1.0"`

	// Binary sets what the software versions reference: nothing (the
	// package manager installs the version from its repositories), the
	// Cloudsmith download URL, or a binary uploaded to Cumulocity
	Binary string `yaml:"binary" json:"binary,omitempty" jsonschema:"description=What each version references. auto (default): none for deb and rpm and alpine packages and link for raw packages. none: no url so the device's package manager installs the version from its configured repositories (one software item for all architectures). link: the Cloudsmith download URL. upload: download the package and upload it to Cumulocity,enum=auto,enum=none,enum=link,enum=upload"`
}

// cloudsmithFormats are the supported package formats and the Cumulocity
// software type of each ("" for raw: derived from the filename)
var cloudsmithFormats = map[string]string{
	"deb":    "apt",
	"rpm":    "rpm",
	"alpine": "apk",
	"raw":    "",
}

var cloudsmithDefaultFormats = []string{"deb", "rpm", "alpine", "raw"}

// cloudsmithPageSize is the number of packages requested per page
const cloudsmithPageSize = 100

// PackageInfo is the metadata of a package from a package repository, used
// instead of parsing the filename
type PackageInfo struct {
	Name         string
	Version      string
	Format       string
	Architecture string
}

// Binary modes of a cloudsmith source
const (
	CloudsmithBinaryAuto   = "auto"
	CloudsmithBinaryNone   = "none"
	CloudsmithBinaryLink   = "link"
	CloudsmithBinaryUpload = "upload"
)

// BinaryMode returns what the versions of a package of the format
// reference: none, link or upload. auto (the default) means none for
// packages installed by a package manager (deb, rpm, alpine) and link for
// raw packages.
func (c *CloudsmithSource) BinaryMode(format string) string {
	switch mode := strings.TrimSpace(c.Binary); mode {
	case "", CloudsmithBinaryAuto:
		if format == "raw" {
			return CloudsmithBinaryLink
		}
		return CloudsmithBinaryNone
	default:
		return mode
	}
}

// OwnerRepo returns the owner and repository, also from a repository URL
func (c *CloudsmithSource) OwnerRepo() (string, string, error) {
	value := strings.TrimSpace(c.Repo)
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil {
			return "", "", fmt.Errorf("invalid cloudsmith.repo %q: %w", c.Repo, err)
		}
		value = strings.Trim(u.Path, "/")
	}
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cloudsmith.repo must be owner/repository or a repository URL, got %q", c.Repo)
	}
	return parts[0], parts[1], nil
}

// versionSelection parses the version selector: limit is the number of
// latest versions (0 for all), exact an exact version
func (c *CloudsmithSource) versionSelection() (limit int, exact string, err error) {
	switch value := strings.TrimSpace(c.Version); {
	case value == "" || value == "latest":
		return 1, "", nil
	case value == "all":
		return 0, "", nil
	case strings.HasPrefix(value, "latest-"):
		n, err := strconv.Atoi(strings.TrimPrefix(value, "latest-"))
		if err != nil || n < 1 {
			return 0, "", fmt.Errorf("cloudsmith.version %q: latest-N requires a number of at least 1", value)
		}
		return n, "", nil
	default:
		return 0, value, nil
	}
}

func (c *CloudsmithSource) validate() error {
	if _, _, err := c.OwnerRepo(); err != nil {
		return err
	}
	if len(c.Packages) == 0 {
		return fmt.Errorf("cloudsmith.packages requires at least one package name or pattern")
	}
	for _, pattern := range c.Packages {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("cloudsmith.packages must not contain empty names")
		}
		if _, err := filepath.Match(pattern, ""); err != nil {
			return fmt.Errorf("cloudsmith.packages: invalid pattern %q", pattern)
		}
	}
	for _, format := range c.Formats {
		if _, ok := cloudsmithFormats[format]; !ok {
			return fmt.Errorf("cloudsmith.formats: unsupported format %q (supported: %s)", format, strings.Join(cloudsmithDefaultFormats, ", "))
		}
	}
	if _, _, err := c.versionSelection(); err != nil {
		return err
	}
	switch c.Binary {
	case "", CloudsmithBinaryAuto, CloudsmithBinaryNone, CloudsmithBinaryLink, CloudsmithBinaryUpload:
	default:
		return fmt.Errorf("cloudsmith.binary: unsupported value %q (use auto, none, link or upload)", c.Binary)
	}
	return nil
}

// cloudsmithPackage is the subset of the Cloudsmith package API used here
type cloudsmithPackage struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Format        string `json:"format"`
	Filename      string `json:"filename"`
	CDNURL        string `json:"cdn_url"`
	SlugPerm      string `json:"slug_perm"`
	SyncCompleted bool   `json:"is_sync_completed"`
	Architectures []struct {
		Name string `json:"name"`
	} `json:"architectures"`
}

func (p cloudsmithPackage) architecture() string {
	if len(p.Architectures) == 0 {
		return ""
	}
	return p.Architectures[0].Name
}

// resolveCloudsmith lists the selected packages: for each package name and
// format, the latest (latest-N, all or an exact) version(s), with every
// architecture of those versions. The repository is listed sorted by
// version (newest first), so for exact package names the listing stops as
// soon as the selected versions are complete.
func (r *SourceResolver) resolveCloudsmith(source *CloudsmithSource) ([]ResolvedFile, error) {
	owner, repo, err := source.OwnerRepo()
	if err != nil {
		return nil, err
	}
	limit, exact, err := source.versionSelection()
	if err != nil {
		return nil, err
	}
	formats := source.Formats
	if len(formats) == 0 {
		formats = cloudsmithDefaultFormats
	}

	var files []ResolvedFile
	seen := map[string]bool{}
	for _, pattern := range source.Packages {
		for _, format := range formats {
			packages, err := r.listCloudsmith(owner, repo, pattern, format, limit, exact)
			if err != nil {
				return nil, err
			}
			for _, pkg := range packages {
				if !cloudsmithArchitectureMatches(source.Architectures, pkg.architecture()) {
					continue
				}
				// Without a binary, a version is the same for every
				// architecture: the package manager picks the right one
				key := pkg.SlugPerm
				if source.BinaryMode(pkg.Format) == CloudsmithBinaryNone {
					key = pkg.Name + "\x00" + pkg.Format + "\x00" + pkg.Version
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				file, err := r.cloudsmithFile(owner, repo, source, pkg)
				if err != nil {
					return nil, err
				}
				files = append(files, file)
			}
		}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no packages in %s/%s matched %s (version: %s): %w",
			owner, repo, strings.Join(source.Packages, ", "), cloudsmithVersionLabel(source), ErrNothingToDo)
	}
	return files, nil
}

func cloudsmithVersionLabel(source *CloudsmithSource) string {
	if source.Version == "" {
		return "latest"
	}
	return source.Version
}

func cloudsmithArchitectureMatches(wanted []string, arch string) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, name := range wanted {
		if strings.EqualFold(name, arch) {
			return true
		}
	}
	return false
}

// listCloudsmith returns the packages of one name pattern and format with
// the selected versions (limit latest versions per package name, 0 for all,
// or the exact version)
func (r *SourceResolver) listCloudsmith(owner, repo, pattern, format string, limit int, exact string) ([]cloudsmithPackage, error) {
	var query []string
	if term := cloudsmithNameTerm(pattern); term != "" {
		query = append(query, term)
	}
	query = append(query, "format:"+format)
	if exact != "" {
		query = append(query, "version:^"+exact+"$")
	}
	// Exact names can stop early: the listing is sorted by version
	stopEarly := limit > 0 && !strings.ContainsAny(pattern, "*?[")

	versions := map[string][]string{} // selected versions by package name
	var selected []cloudsmithPackage
	for page := 1; ; page++ {
		params := url.Values{}
		params.Set("query", strings.Join(query, " AND "))
		params.Set("sort", "-version")
		params.Set("page", strconv.Itoa(page))
		params.Set("page_size", strconv.Itoa(cloudsmithPageSize))
		endpoint := fmt.Sprintf("%s/packages/%s/%s/?%s", r.cloudsmithAPI(), url.PathEscape(owner), url.PathEscape(repo), params.Encode())

		var packages []cloudsmithPackage
		more, err := r.cloudsmithGet(endpoint, &packages)
		if err != nil {
			return nil, fmt.Errorf("failed to list packages of %s/%s: %w", owner, repo, err)
		}

		complete := stopEarly
		for _, pkg := range packages {
			// The query matches loosely (e.g. a dot matches any character)
			if ok, _ := filepath.Match(pattern, pkg.Name); !ok || pkg.Format != format || !pkg.SyncCompleted {
				continue
			}
			if exact != "" && pkg.Version != exact {
				continue
			}
			known := versions[pkg.Name]
			if !containsString(known, pkg.Version) {
				if limit > 0 && len(known) >= limit {
					continue // an older version than the selected ones
				}
				versions[pkg.Name] = append(known, pkg.Version)
			}
			selected = append(selected, pkg)
		}
		if stopEarly {
			// Done once a version older than the selected ones was listed
			// (all architectures of the selected versions come before it)
			complete = len(packages) > 0 && len(versions[pattern]) >= limit &&
				packages[len(packages)-1].Version != versions[pattern][len(versions[pattern])-1]
		}
		if !more || len(packages) == 0 || complete {
			break
		}
	}
	return selected, nil
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// cloudsmithNameTerm returns the query term for a package name or glob. The
// query language anchors with ^ and $ but has no regular expressions, so a
// glob is queried by its prefix (the part before the first wildcard) and the
// results are matched against the glob afterwards.
func cloudsmithNameTerm(pattern string) string {
	index := strings.IndexAny(pattern, "*?[")
	if index < 0 {
		return "name:^" + pattern + "$"
	}
	if index == 0 {
		return ""
	}
	return "name:^" + pattern[:index]
}

// cloudsmithGet fetches a page of the Cloudsmith API and reports whether
// there are more pages
func (r *SourceResolver) cloudsmithGet(endpoint string, target any) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	slog.Debug("Listing Cloudsmith packages", "url", endpoint)
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("cloudsmith API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return false, fmt.Errorf("invalid response from the cloudsmith API: %w", err)
	}
	page, _ := strconv.Atoi(resp.Header.Get("X-Pagination-Page"))
	total, _ := strconv.Atoi(resp.Header.Get("X-Pagination-Pagetotal"))
	return page > 0 && page < total, nil
}

func (r *SourceResolver) cloudsmithAPI() string {
	if r.CloudsmithAPI != "" {
		return strings.TrimSuffix(r.CloudsmithAPI, "/")
	}
	return "https://api.cloudsmith.io/v1"
}

// cloudsmithFile returns the package as a resolved file: without a binary,
// with its download URL, or with the downloaded file
func (r *SourceResolver) cloudsmithFile(owner, repo string, source *CloudsmithSource, pkg cloudsmithPackage) (ResolvedFile, error) {
	file := ResolvedFile{
		Filename:    pkg.Filename,
		VersionHint: pkg.Version,
		Package: &PackageInfo{
			Name:         pkg.Name,
			Version:      pkg.Version,
			Format:       pkg.Format,
			Architecture: pkg.architecture(),
		},
	}
	switch source.BinaryMode(pkg.Format) {
	case CloudsmithBinaryNone:
		// The package manager installs the version from its repositories
		// and picks the architecture
		file.Package.Architecture = ""
		return file, nil
	case CloudsmithBinaryLink:
		file.URL = pkg.CDNURL
		return file, nil
	}
	// Packages of different architectures can share a filename (e.g. apk)
	dir := filepath.Join(r.WorkDir, "cloudsmith", owner, repo, pkg.SlugPerm)
	file.Path = filepath.Join(dir, pkg.Filename)
	if r.SkipDownload {
		return file, nil
	}
	if err := r.downloadURL(pkg.CDNURL, dir, file.Path); err != nil {
		return ResolvedFile{}, fmt.Errorf("failed to download %s from %s/%s: %w", pkg.Filename, owner, repo, err)
	}
	return file, nil
}

// downloadURL downloads a public URL to a file
func (r *SourceResolver) downloadURL(source, dir, path string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	slog.Info("Downloading package", "url", source)
	resp, err := r.HTTPClient.Get(source)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// cloudsmithSoftwareInfo builds the software info from the package metadata
// (the filenames of some formats, e.g. apk, have no architecture)
func cloudsmithSoftwareInfo(file ResolvedFile, spec SoftwareSpec) (*SoftwareInfo, error) {
	pkg := file.Package
	softwareType := spec.Type
	if softwareType == "" {
		softwareType = cloudsmithFormats[pkg.Format]
	}
	if softwareType == "" {
		// raw packages: the type follows from the filename (e.g. archive)
		parsed, err := ParseSoftwareFromFilename(file.Filename, "", "", spec.TypeMappings()...)
		if err != nil {
			return nil, err
		}
		softwareType = parsed.SoftwareType
	}
	filename := file.Filename
	if file.URL == "" && file.Path == "" {
		filename = "" // no binary: the version has an empty url
	}
	info := &SoftwareInfo{
		FilePath:     file.Path,
		Filename:     filename,
		Name:         spec.NamePrefix + pkg.Name,
		Version:      pkg.Version,
		SoftwareType: softwareType,
		Architecture: pkg.Architecture,
		ExternalURL:  file.URL,
	}
	if spec.Version != "" {
		info.Version = spec.Version
	}
	return info, nil
}
