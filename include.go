package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

// IncludeSpec is an entry of the include list: a path or glob pattern of
// another manifest file, relative to the including file, an http(s) URL,
// or a bundle (an archive with a manifest and its files) from a path, URL
// or GitHub release. In YAML it is a plain string or an object.
type IncludeSpec struct {
	// Path is a file path or glob pattern (*, ?, [...]) relative to the
	// directory of the including file, or an http(s) URL
	Path string `yaml:"path" json:"path,omitempty"`

	// GitHub selects a bundle from a GitHub release asset
	GitHub *IncludeGitHub `yaml:"github" json:"github,omitempty"`

	// Manifest is the entry manifest inside a bundle
	Manifest string `yaml:"manifest" json:"manifest,omitempty"`

	// SHA256 is the expected checksum of a bundle archive
	SHA256 string `yaml:"sha256" json:"sha256,omitempty"`

	// Optional skips the entry instead of failing when the file does not
	// exist or the glob pattern matches nothing
	Optional bool `yaml:"optional" json:"optional,omitempty"`
}

// UnmarshalYAML accepts a plain string or an object
func (i *IncludeSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		i.Path = node.Value
		return nil
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			var err error
			switch key.Value {
			case "path":
				err = value.Decode(&i.Path)
			case "optional":
				err = value.Decode(&i.Optional)
			case "manifest":
				err = value.Decode(&i.Manifest)
			case "sha256":
				err = value.Decode(&i.SHA256)
			case "github":
				i.GitHub = &IncludeGitHub{}
				decoder := yaml.NewDecoder(strings.NewReader(nodeYAML(value)))
				decoder.KnownFields(true)
				if err = decoder.Decode(i.GitHub); err != nil {
					err = fmt.Errorf("line %d: include github: %w", key.Line, err)
				}
			default:
				return fmt.Errorf("line %d: field %s not found in type include (expected path, github, manifest, sha256 or optional)", key.Line, key.Value)
			}
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("line %d: an include entry must be a path or an object with path or github", node.Line)
	}
}

// nodeYAML renders a YAML node, so it can be decoded with known fields
func nodeYAML(node *yaml.Node) string {
	out, err := yaml.Marshal(node)
	if err != nil {
		return ""
	}
	return string(out)
}

// validate checks the fields of an include entry
func (i IncludeSpec) validate() error {
	path := strings.TrimSpace(i.Path)
	switch {
	case path == "" && i.GitHub == nil:
		return errors.New("path or github is required")
	case path != "" && i.GitHub != nil:
		return errors.New("path and github are mutually exclusive")
	case i.GitHub != nil:
		if err := i.GitHub.validate(); err != nil {
			return err
		}
	case (i.Manifest != "" || i.SHA256 != "") && !isBundleArchive(path):
		return errors.New("manifest and sha256 are only supported for bundles (github, or a .tar.gz, .tgz or .zip path)")
	case isBundleArchive(path) && strings.ContainsAny(path, "*?["):
		return errors.New("a bundle path can't be a glob pattern")
	}
	if i.SHA256 != "" {
		value := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(i.SHA256)), "sha256:")
		if len(value) != 64 || strings.Trim(value, "0123456789abcdef") != "" {
			return fmt.Errorf("sha256 %q is not a hex SHA-256 checksum", i.SHA256)
		}
	}
	return nil
}

// JSONSchema describes an include entry as a string or an object
func (IncludeSpec) JSONSchema() *jsonschema.Schema {
	str := func(description string) *jsonschema.Schema {
		return &jsonschema.Schema{Type: "string", Description: description}
	}
	strList := func(description string) *jsonschema.Schema {
		return &jsonschema.Schema{Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: description}
	}

	tokenFrom := jsonschema.NewProperties()
	tokenFrom.Set("env", str("Name of an environment variable which holds the token"))
	github := jsonschema.NewProperties()
	github.Set("repo", &jsonschema.Schema{Type: "string", Description: "Repository in the form owner/name", Pattern: "^[^/]+/[^/]+$"})
	github.Set("release", str("Release to use: latest (default) or a tag; with tags: the latest release matching the patterns"))
	github.Set("tags", strList("Glob patterns matched against the release tags"))
	github.Set("assets", strList("Glob patterns matched against the release asset names; exactly one asset must match (default: *.tar.gz, *.tgz, *.zip)"))
	github.Set("includePrereleases", &jsonschema.Schema{Type: "boolean", Description: "Include prereleases when release is latest"})
	github.Set("token", str("GitHub API token (falls back to GITHUB_TOKEN / GH_TOKEN env vars; supports ${VAR} expansion)"))
	github.Set("tokenFrom", &jsonschema.Schema{Type: "object", Properties: tokenFrom, Required: []string{"env"}, AdditionalProperties: jsonschema.FalseSchema, Description: "Take the GitHub API token from an environment variable"})

	properties := jsonschema.NewProperties()
	properties.Set("path", str("File path or glob pattern (*, ?, [...]) relative to the directory of the including file, an http(s) URL, or a bundle archive (.tar.gz, .tgz or .zip)"))
	properties.Set("github", &jsonschema.Schema{
		Type:                 "object",
		Properties:           github,
		Required:             []string{"repo"},
		AdditionalProperties: jsonschema.FalseSchema,
		Description:          "Include a bundle from a GitHub release asset",
	})
	properties.Set("manifest", str("Entry manifest inside a bundle (default: from "+BundleMarkerFile+", else tenant.yaml, else the only YAML file at the top level)"))
	properties.Set("sha256", &jsonschema.Schema{Type: "string", Description: "Expected SHA-256 checksum of a bundle archive", Pattern: "^(sha256:)?[0-9a-fA-F]{64}$"})
	properties.Set("optional", &jsonschema.Schema{
		Type:        "boolean",
		Description: "Skip the entry instead of failing when the file does not exist, the glob pattern matches nothing, or no release asset matches",
	})
	return &jsonschema.Schema{
		Description: "Manifest file path or glob pattern relative to the including file, an http(s) URL, or an object with path or github (bundle)",
		OneOf: []*jsonschema.Schema{
			{Type: "string", Examples: []any{"base.yaml", "common/*.yaml", "https://raw.githubusercontent.com/org/repo/main/base.yaml", "vendor/blueprint.tar.gz"}},
			{
				Type:                 "object",
				Properties:           properties,
				AdditionalProperties: jsonschema.FalseSchema,
				OneOf: []*jsonschema.Schema{
					{Required: []string{"path"}},
					{Required: []string{"github"}},
				},
			},
		},
	}
}

// manifestOrigin tells where a manifest came from, which decides what it
// may use
type manifestOrigin struct {
	// remote is set for content fetched from a URL or a GitHub release: its
	// hooks and commands need AllowRemoteCommands
	remote bool
	// bundleRoot is the directory a remote bundle was extracted to: local
	// paths (includes and sources) must stay inside it. Empty for a remote
	// manifest outside a bundle, which can't use local paths at all.
	bundleRoot string
}

// manifestRef is a manifest to load: its key (absolute path or URL) and
// origin
type manifestRef struct {
	key    string
	origin manifestOrigin
}

// manifestLoader loads a manifest with its includes
type manifestLoader struct {
	opts LoadOptions
	// rootDir is the absolute directory of the main (first) manifest, which
	// relative source paths are resolved against at run time
	rootDir string
	// rootFile is the key of the main (first) manifest
	rootFile string
	// loaded holds the keys of the files loaded so far. A file which is
	// reached more than once (e.g. a shared base included by two files) is
	// merged only once, at its first position
	loaded map[string]bool
	// files are the included files in load order (for display)
	files []string
	// localFiles are the absolute paths of the local manifest files and
	// bundle archives outside bundles (for packaging a bundle)
	localFiles []string
	// labelRoot prefixes errors of the manifests given on the command line
	// with their name (when there are several or the first has includes)
	labelRoot bool

	// bundles are the extracted bundles by source
	bundles map[string]*extractedBundle
	// bundleRoots maps the extraction directory of a bundle to its name
	bundleRoots map[string]string
	// bundleDir is where bundles are downloaded and extracted
	bundleDir string
	// tempDirs are removed by Manifest.Cleanup
	tempDirs []string
}

// displayPath returns the name of a manifest for the output: the URL, the
// bundle name with the path inside the bundle (bundle!path), or the path
// relative to the main manifest directory (or the absolute path when that
// is not possible, e.g. on another volume)
func (l *manifestLoader) displayPath(abs string) string {
	if isRemoteManifest(abs) {
		return abs
	}
	if isStdinManifest(abs) {
		return stdinManifestName
	}
	for root, name := range l.bundleRoots {
		if isWithin(root, abs) {
			rel, _ := filepath.Rel(root, abs)
			return name + "!" + filepath.ToSlash(rel)
		}
	}
	if rel, err := filepath.Rel(l.rootDir, abs); err == nil {
		return rel
	}
	return abs
}

// rootRef returns the manifest of a path given on the command line: a local
// file, a URL, or a bundle archive from a path or URL
func (l *manifestLoader) rootRef(path string) (manifestRef, error) {
	key, err := manifestKey(path)
	if err != nil {
		return manifestRef{}, err
	}
	switch {
	case isRemoteManifest(key) && isBundleArchive(key):
		bundle, err := l.urlBundle(key, IncludeSpec{Path: key})
		if err != nil {
			return manifestRef{}, err
		}
		return manifestRef{key: bundle.entry, origin: manifestOrigin{remote: true, bundleRoot: bundle.root}}, nil
	case isRemoteManifest(key):
		return manifestRef{key: key, origin: manifestOrigin{remote: true}}, nil
	case isBundleArchive(key):
		bundle, err := l.localBundle(key, IncludeSpec{Path: key})
		if err != nil {
			return manifestRef{}, err
		}
		l.localFiles = append(l.localFiles, key)
		return manifestRef{key: bundle.entry}, nil
	default:
		return manifestRef{key: key}, nil
	}
}

// loadAll loads the manifests given on the command line and merges them in
// order, so later files win (like files included in that order). A file
// given more than once, or already included by an earlier file, is merged
// once.
func (l *manifestLoader) loadAll(paths []string) (*Manifest, error) {
	if len(paths) > 1 {
		l.labelRoot = true
	}
	merged := &Manifest{}
	for _, path := range paths {
		ref, err := l.rootRef(path)
		if err != nil {
			return nil, err
		}
		if l.loaded[ref.key] {
			continue
		}
		manifest, err := l.load(ref, nil)
		if err != nil {
			return nil, err
		}
		if len(paths) == 1 {
			return manifest, nil
		}
		merged = mergeManifests(merged, manifest)
	}
	return merged, nil
}

// load reads, expands, parses and validates one manifest file, then merges
// its includes (first, in order) with its own content (last). chain holds
// the keys of the including files, for cycle detection; it is empty for
// the files given on the command line.
func (l *manifestLoader) load(ref manifestRef, chain []string) (*Manifest, error) {
	abs := ref.key
	isURL := isRemoteManifest(abs)
	// A manifest from a URL or stdin is not a local file: its local paths
	// are based on the main manifest directory (the current directory when
	// the main manifest is not a local file either)
	isFile := !isURL && !isStdinManifest(abs)
	isRoot := len(chain) == 0
	if slices.Contains(chain, abs) {
		names := make([]string, 0, len(chain)+1)
		for _, item := range append(chain, abs) {
			names = append(names, l.displayPath(item))
		}
		return nil, fmt.Errorf("include cycle: %s", strings.Join(names, " -> "))
	}
	l.loaded[abs] = true
	if !isRoot {
		l.files = append(l.files, l.displayPath(abs))
	}
	if isFile && !l.inBundle(abs) {
		l.localFiles = append(l.localFiles, abs)
	}

	label := func(err error) error {
		if isRoot && !l.labelRoot {
			return err
		}
		return fmt.Errorf("%s: %w", l.displayPath(abs), err)
	}

	raw, err := readManifestFile(abs)
	if err != nil {
		if isRoot {
			return nil, err
		}
		return nil, fmt.Errorf("%s (included by %s): %w", l.displayPath(abs), l.displayPath(chain[len(chain)-1]), err)
	}
	expanded, err := expandManifest(string(raw), l.opts.Secrets)
	if err != nil {
		return nil, label(err)
	}

	own := &Manifest{}
	decoder := yaml.NewDecoder(strings.NewReader(expanded))
	// Strict parsing: unknown fields are errors so typos are caught early
	decoder.KnownFields(true)
	if err := decoder.Decode(own); err != nil && !errors.Is(err, io.EOF) {
		return nil, label(fmt.Errorf("failed to parse manifest: %w", err))
	}
	if isRoot && len(own.Include) > 0 {
		l.labelRoot = true
	}

	own.applyDefaults()
	if err := own.Validate(); err != nil {
		return nil, label(err)
	}

	// Hooks and commands of a remote manifest run from the main manifest
	// directory (the current directory when it is remote too); those of a
	// bundle run from the directory of the file in the extracted bundle
	dir := l.rootDir
	if isFile {
		dir = filepath.Dir(abs)
	}
	if ref.origin.remote {
		if err := own.checkRemoteManifest(ref.origin, dir, l.opts.AllowRemoteCommands); err != nil {
			return nil, label(err)
		}
	}
	if abs != l.rootFile {
		own.rebase(dir, l.rootDir)
	}

	includes := own.Include
	own.Include = nil
	if len(includes) == 0 {
		return own, nil
	}

	merged := &Manifest{}
	for index, include := range includes {
		refs, err := l.resolveInclude(ref, dir, include)
		if err != nil {
			return nil, label(fmt.Errorf("include[%d]: %w", index, err))
		}
		for _, included := range refs {
			file := included.key
			if l.loaded[file] && !slices.Contains(chain, file) && file != abs {
				continue
			}
			manifest, err := l.load(included, append(slices.Clone(chain), abs))
			if err != nil {
				if include.Optional && isRemoteNotFound(err, file) {
					l.unload(file)
					continue
				}
				return nil, err
			}
			merged = mergeManifests(merged, manifest)
		}
	}
	return mergeManifests(merged, own), nil
}

// inBundle reports whether a file is inside an extracted bundle
func (l *manifestLoader) inBundle(abs string) bool {
	for root := range l.bundleRoots {
		if isWithin(root, abs) {
			return true
		}
	}
	return false
}

// unload forgets a file which was skipped after it was marked as loaded
// (an optional remote include which does not exist)
func (l *manifestLoader) unload(file string) {
	delete(l.loaded, file)
	if name := l.displayPath(file); len(l.files) > 0 && l.files[len(l.files)-1] == name {
		l.files = l.files[:len(l.files)-1]
	}
}

// resolveInclude returns the manifests of an include entry of parent:
//   - github: the entry manifest of the bundle from the release asset
//   - an http(s) URL, or a relative path in a remote manifest (resolved
//     against its URL): that URL, or the entry manifest of the bundle when
//     it names an archive. A URL which does not exist is detected when it
//     is loaded.
//   - a local path or glob pattern relative to dir: the matching files
//     (sorted by name, without parent itself), with bundle archives replaced
//     by their entry manifests. Inside a remote bundle, the files must be in
//     the bundle.
func (l *manifestLoader) resolveInclude(parent manifestRef, dir string, include IncludeSpec) ([]manifestRef, error) {
	if err := include.validate(); err != nil {
		return nil, err
	}
	if include.GitHub != nil {
		bundle, err := l.githubBundle(include)
		if err != nil || bundle == nil {
			return nil, err
		}
		return []manifestRef{{key: bundle.entry, origin: manifestOrigin{remote: true, bundleRoot: bundle.root}}}, nil
	}

	pattern := strings.TrimSpace(include.Path)
	var location string
	switch {
	case isRemoteManifest(pattern):
		key, err := manifestKey(pattern)
		if err != nil {
			return nil, err
		}
		location = key
	case isRemoteManifest(parent.key):
		key, err := resolveRemoteInclude(parent.key, include)
		if err != nil {
			return nil, err
		}
		location = key
	}
	if location != "" {
		if !isBundleArchive(location) {
			return []manifestRef{{key: location, origin: manifestOrigin{remote: true}}}, nil
		}
		bundle, err := l.urlBundle(location, include)
		if err != nil {
			if include.Optional && isRemoteNotFound(err, location) {
				return nil, nil
			}
			return nil, err
		}
		return []manifestRef{{key: bundle.entry, origin: manifestOrigin{remote: true, bundleRoot: bundle.root}}}, nil
	}

	bundleRoot := parent.origin.bundleRoot
	if bundleRoot != "" && (filepath.IsAbs(pattern) || !isWithin(bundleRoot, filepath.Join(dir, pattern))) {
		return nil, fmt.Errorf("%s: a bundle can only include paths inside the bundle", include.Path)
	}
	files, err := l.matchLocalInclude(dir, parent.key, include)
	if err != nil {
		return nil, err
	}
	refs := make([]manifestRef, 0, len(files))
	for _, file := range files {
		if bundleRoot != "" && !isWithin(bundleRoot, file) {
			return nil, fmt.Errorf("%s: a bundle can only include paths inside the bundle", include.Path)
		}
		if !isBundleArchive(file) {
			refs = append(refs, manifestRef{key: file, origin: parent.origin})
			continue
		}
		if !l.inBundle(file) {
			l.localFiles = append(l.localFiles, file)
		}
		bundle, err := l.localBundle(file, include)
		if err != nil {
			return nil, err
		}
		origin := manifestOrigin{}
		if parent.origin.remote {
			origin = manifestOrigin{remote: true, bundleRoot: bundle.root}
		}
		refs = append(refs, manifestRef{key: bundle.entry, origin: origin})
	}
	return refs, nil
}

// matchLocalInclude returns the absolute paths of the local files of an
// include entry. Glob matches are sorted by name; the including file itself
// is skipped when a pattern matches it.
func (l *manifestLoader) matchLocalInclude(dir, self string, include IncludeSpec) ([]string, error) {
	pattern := strings.TrimSpace(include.Path)
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(dir, pattern)
	}

	if !strings.ContainsAny(include.Path, "*?[") {
		info, err := os.Stat(pattern)
		switch {
		case err == nil && info.IsDir():
			return nil, fmt.Errorf("%s is a directory (use a glob pattern such as %s)", include.Path, filepath.ToSlash(filepath.Join(include.Path, "*.yaml")))
		case err == nil:
			return []string{pattern}, nil
		case errors.Is(err, os.ErrNotExist) && include.Optional:
			return nil, nil
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("included file %s not found (set optional: true to skip missing files)", include.Path)
		default:
			return nil, err
		}
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", include.Path, err)
	}
	var files []string
	for _, match := range matches {
		if match == self {
			continue
		}
		if info, err := os.Stat(match); err == nil && !info.IsDir() {
			files = append(files, match)
		}
	}
	if len(files) == 0 && !include.Optional {
		return nil, fmt.Errorf("glob pattern %s matches no files (set optional: true to allow no matches)", include.Path)
	}
	return files, nil
}

// rebase makes the local paths of an included manifest independent of its
// location: relative source paths are rewritten relative to the main
// manifest directory (rootDir), and hooks and commands run from the
// directory of the included file (dir)
func (m *Manifest) rebase(dir, rootDir string) {
	rebasePath := func(source *Source) {
		if source == nil || source.Path == "" || filepath.IsAbs(source.Path) {
			return
		}
		target := filepath.Join(dir, source.Path)
		if rel, err := filepath.Rel(rootDir, target); err == nil {
			source.Path = rel
		} else {
			source.Path = target
		}
	}
	for i := range m.TrustedCertificates {
		rebasePath(&m.TrustedCertificates[i].Source)
	}
	for i := range m.CertificateRevocationLists {
		rebasePath(&m.CertificateRevocationLists[i].Source)
	}
	for i := range m.Microservices {
		rebasePath(m.Microservices[i].Source)
	}
	for i := range m.Extensions {
		rebasePath(&m.Extensions[i].Source)
	}
	for i := range m.Applications {
		rebasePath(m.Applications[i].Source)
	}
	for i := range m.Software {
		rebasePath(&m.Software[i].Source)
	}
	for i := range m.Firmware {
		rebasePath(&m.Firmware[i].Source)
	}
	for i := range m.Configuration {
		rebasePath(&m.Configuration[i].Source)
	}
	for i := range m.SmartRestTemplates {
		rebasePath(&m.SmartRestTemplates[i].Source)
	}
	for i := range m.SmartFunctions {
		rebasePath(&m.SmartFunctions[i].Source)
		if file := m.SmartFunctions[i].CodeFile; file != "" && !filepath.IsAbs(file) {
			// Same as a source path: relative to the main manifest directory
			code := Source{Path: file}
			rebasePath(&code)
			m.SmartFunctions[i].CodeFile = code.Path
		}
	}
	if m.DigitalTwin != nil {
		for _, defs := range [][]DigitalTwinDefinitionSpec{m.DigitalTwin.PropertyDefinitions, m.DigitalTwin.AssetDefinitions} {
			for i := range defs {
				if defs[i].Source != nil {
					rebasePath(defs[i].Source)
				}
			}
		}
	}

	setDir := func(hooks []HookSpec) {
		for i := range hooks {
			if hooks[i].Dir == "" {
				hooks[i].Dir = dir
			}
		}
	}
	setDir(m.Hooks.Pre)
	setDir(m.Hooks.Post)
	for _, section := range m.Hooks.Sections {
		setDir(section.Pre)
		setDir(section.Post)
	}
	for i := range m.Commands {
		if m.Commands[i].Dir == "" {
			m.Commands[i].Dir = dir
		}
	}
}

// mergeManifests merges over into base and returns the result:
//   - list sections are concatenated (base first)
//   - entries with an identity (child tenant domain, tenant option
//     category+key, feature key, retention rule selector, application name,
//     root device group name (the whole tree is replaced), user group name,
//     inventory role name, user name, device profile name, command group
//     name) replace an entry with the same identity in base, at its
//     position (the later definition wins)
//   - entries without an identity (trusted certificates, revocation lists,
//     software, firmware, configuration, SmartREST templates) are all kept
//     and applied in order
//   - targets, certificateAuthority and loginOptions from over replace
//     those of base
//   - hooks pre/post lists are concatenated, section hooks per section
func mergeManifests(base, over *Manifest) *Manifest {
	out := &Manifest{
		Targets:              base.Targets,
		CertificateAuthority: base.CertificateAuthority,
		LoginOptions:         base.LoginOptions,
	}
	if over.Targets != nil {
		out.Targets = over.Targets
	}
	if over.CertificateAuthority != nil {
		out.CertificateAuthority = over.CertificateAuthority
	}
	if over.LoginOptions != nil {
		out.LoginOptions = over.LoginOptions
	}

	out.Tenants = mergeByKey(base.Tenants, over.Tenants, func(t ChildTenantSpec) string { return strings.ToLower(t.Domain) })
	out.TenantOptions = mergeByKey(base.TenantOptions, over.TenantOptions, func(o TenantOptionSpec) string {
		if o.Category == "" || o.Key == "" {
			return ""
		}
		return o.Category + "\x00" + o.Key
	})
	out.Features = mergeByKey(base.Features, over.Features, func(f FeatureSpec) string { return f.Key })
	out.RetentionRules = mergeByKey(base.RetentionRules, over.RetentionRules, RetentionRuleSpec.Selector)
	out.Microservices = mergeByKey(base.Microservices, over.Microservices, func(a MicroserviceSpec) string { return a.Name })
	out.Extensions = mergeByKey(base.Extensions, over.Extensions, func(e ExtensionSpec) string { return e.Name })
	out.Applications = mergeByKey(base.Applications, over.Applications, func(a ApplicationSpec) string { return a.Name })
	out.DeviceGroups = mergeByKey(base.DeviceGroups, over.DeviceGroups, func(g DeviceGroupSpec) string { return g.Name })
	out.GlobalRoles = mergeByKey(base.GlobalRoles, over.GlobalRoles, func(g GlobalRoleSpec) string { return g.Name })
	out.InventoryRoles = mergeByKey(base.InventoryRoles, over.InventoryRoles, func(r InventoryRoleSpec) string { return r.Name })
	out.Users = mergeByKey(base.Users, over.Users, func(u UserSpec) string { return u.Username })
	out.DeviceProfiles = mergeByKey(base.DeviceProfiles, over.DeviceProfiles, func(p DeviceProfileSpec) string { return p.Name })
	out.Commands = mergeByKey(base.Commands, over.Commands, func(c CommandGroupSpec) string { return c.Name })

	out.TrustedCertificates = concat(base.TrustedCertificates, over.TrustedCertificates)
	out.CertificateRevocationLists = concat(base.CertificateRevocationLists, over.CertificateRevocationLists)
	out.Software = concat(base.Software, over.Software)
	out.Firmware = concat(base.Firmware, over.Firmware)
	out.Configuration = concat(base.Configuration, over.Configuration)
	out.SmartRestTemplates = concat(base.SmartRestTemplates, over.SmartRestTemplates)
	out.SmartFunctions = concat(base.SmartFunctions, over.SmartFunctions)
	out.DigitalTwin = mergeDigitalTwin(base.DigitalTwin, over.DigitalTwin)

	out.Hooks.Pre = concat(base.Hooks.Pre, over.Hooks.Pre)
	out.Hooks.Post = concat(base.Hooks.Post, over.Hooks.Post)
	if len(base.Hooks.Sections) > 0 || len(over.Hooks.Sections) > 0 {
		out.Hooks.Sections = make(map[string]SectionHooksSpec, len(base.Hooks.Sections)+len(over.Hooks.Sections))
		for name, hooks := range base.Hooks.Sections {
			out.Hooks.Sections[name] = hooks
		}
		for name, hooks := range over.Hooks.Sections {
			existing := out.Hooks.Sections[name]
			out.Hooks.Sections[name] = SectionHooksSpec{
				Pre:  concat(existing.Pre, hooks.Pre),
				Post: concat(existing.Post, hooks.Post),
			}
		}
	}
	return out
}

// concat returns a new slice with the items of a followed by those of b
// (nil when both are empty)
func concat[T any](a, b []T) []T {
	if len(a)+len(b) == 0 {
		return nil
	}
	return append(slices.Clone(a), b...)
}

// mergeByKey concatenates the lists, except that an item of over whose key
// is already in base replaces that item in place. Items with an empty key
// are always appended.
func mergeByKey[T any](base, over []T, key func(T) string) []T {
	out := concat(base, nil)
	positions := make(map[string]int, len(out))
	for i, item := range out {
		if k := key(item); k != "" {
			positions[k] = i
		}
	}
	for _, item := range over {
		k := key(item)
		if position, ok := positions[k]; ok && k != "" {
			out[position] = item
			continue
		}
		if k != "" {
			positions[k] = len(out)
		}
		out = append(out, item)
	}
	return out
}

// mergeDigitalTwin merges the digitalTwin sections: inline definitions with
// the same key are replaced, source entries are all kept, and managedBy and
// prune of over win when set
func mergeDigitalTwin(base, over *DigitalTwinSpec) *DigitalTwinSpec {
	if base == nil {
		return over
	}
	if over == nil {
		return base
	}
	out := &DigitalTwinSpec{
		ManagedBy: base.ManagedBy,
		Prune:     base.Prune,
	}
	if over.ManagedBy != "" {
		out.ManagedBy = over.ManagedBy
	}
	if over.Prune != nil {
		out.Prune = over.Prune
	}
	out.PropertyDefinitions = mergeByKey(base.PropertyDefinitions, over.PropertyDefinitions, DigitalTwinDefinitionSpec.Key)
	out.AssetDefinitions = mergeByKey(base.AssetDefinitions, over.AssetDefinitions, DigitalTwinDefinitionSpec.Key)
	return out
}
