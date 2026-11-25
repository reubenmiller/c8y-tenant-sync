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
// another manifest file, relative to the including file. In YAML it is a
// plain string or an object with path and optional.
type IncludeSpec struct {
	// Path is a file path or glob pattern (*, ?, [...]) relative to the
	// directory of the including file
	Path string `yaml:"path" json:"path"`

	// Optional skips the entry instead of failing when the file does not
	// exist or the glob pattern matches nothing
	Optional bool `yaml:"optional" json:"optional,omitempty"`
}

// UnmarshalYAML accepts a plain string or an object with path and optional
func (i *IncludeSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		i.Path = node.Value
		return nil
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			switch key.Value {
			case "path":
				if err := value.Decode(&i.Path); err != nil {
					return err
				}
			case "optional":
				if err := value.Decode(&i.Optional); err != nil {
					return err
				}
			default:
				return fmt.Errorf("line %d: field %s not found in type include (expected path or optional)", key.Line, key.Value)
			}
		}
		return nil
	default:
		return fmt.Errorf("line %d: an include entry must be a path or an object with path and optional", node.Line)
	}
}

// JSONSchema describes an include entry as a string or an object
func (IncludeSpec) JSONSchema() *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	properties.Set("path", &jsonschema.Schema{
		Type:        "string",
		Description: "File path or glob pattern (*, ?, [...]) relative to the directory of the including file",
	})
	properties.Set("optional", &jsonschema.Schema{
		Type:        "boolean",
		Description: "Skip the entry instead of failing when the file does not exist or the glob pattern matches nothing",
	})
	return &jsonschema.Schema{
		Description: "Manifest file path or glob pattern relative to the including file, or an object with path and optional",
		OneOf: []*jsonschema.Schema{
			{Type: "string", Examples: []any{"base.yaml", "common/*.yaml"}},
			{
				Type:                 "object",
				Properties:           properties,
				Required:             []string{"path"},
				AdditionalProperties: jsonschema.FalseSchema,
			},
		},
	}
}

// manifestLoader loads a manifest with its includes
type manifestLoader struct {
	opts LoadOptions
	// rootDir is the absolute directory of the main (first) manifest, which
	// relative source paths are resolved against at run time
	rootDir string
	// rootFile is the absolute path of the main (first) manifest
	rootFile string
	// loaded holds the absolute paths of the files loaded so far. A file
	// which is reached more than once (e.g. a shared base included by two
	// files) is merged only once, at its first position
	loaded map[string]bool
	// files are the included files in load order (for display)
	files []string
	// labelRoot prefixes errors of the manifests given on the command line
	// with their name (when there are several or the first has includes)
	labelRoot bool
}

// displayPath returns the path relative to the main manifest directory (or
// the absolute path when that is not possible, e.g. on another volume)
func (l *manifestLoader) displayPath(abs string) string {
	if rel, err := filepath.Rel(l.rootDir, abs); err == nil {
		return rel
	}
	return abs
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
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if l.loaded[abs] {
			continue
		}
		manifest, err := l.load(path, nil)
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
// the absolute paths of the including files, for cycle detection; it is
// empty for the files given on the command line.
func (l *manifestLoader) load(path string, chain []string) (*Manifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
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

	label := func(err error) error {
		if isRoot && !l.labelRoot {
			return err
		}
		return fmt.Errorf("%s: %w", l.displayPath(abs), err)
	}

	raw, err := readManifestFile(path)
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

	dir := filepath.Dir(abs)
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
		files, err := l.resolveInclude(dir, abs, include)
		if err != nil {
			return nil, label(fmt.Errorf("include[%d]: %w", index, err))
		}
		for _, file := range files {
			if l.loaded[file] && !slices.Contains(chain, file) && file != abs {
				continue
			}
			included, err := l.load(file, append(slices.Clone(chain), abs))
			if err != nil {
				return nil, err
			}
			merged = mergeManifests(merged, included)
		}
	}
	return mergeManifests(merged, own), nil
}

// resolveInclude returns the absolute paths of the files of an include
// entry. Glob matches are sorted by name; the including file itself is
// skipped when a pattern matches it.
func (l *manifestLoader) resolveInclude(dir, self string, include IncludeSpec) ([]string, error) {
	pattern := strings.TrimSpace(include.Path)
	if pattern == "" {
		return nil, errors.New("path is required")
	}
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
	}
	if m.DigitalTwin != nil {
		for i := range m.DigitalTwin.Sources {
			rebasePath(&m.DigitalTwin.Sources[i])
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
//   - targets and certificateAuthority from over replace those of base
//   - hooks pre/post lists are concatenated, section hooks per section
func mergeManifests(base, over *Manifest) *Manifest {
	out := &Manifest{
		Targets:              base.Targets,
		CertificateAuthority: base.CertificateAuthority,
	}
	if over.Targets != nil {
		out.Targets = over.Targets
	}
	if over.CertificateAuthority != nil {
		out.CertificateAuthority = over.CertificateAuthority
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
	out.Applications = mergeByKey(base.Applications, over.Applications, func(a ApplicationSpec) string { return a.Name })
	out.DeviceGroups = mergeByKey(base.DeviceGroups, over.DeviceGroups, func(g DeviceGroupSpec) string { return g.Name })
	out.UserGroups = mergeByKey(base.UserGroups, over.UserGroups, func(g UserGroupSpec) string { return g.Name })
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

// mergeDigitalTwin merges the digitalTwin sections: sources are
// concatenated, definitions with the same key are replaced, and managedBy
// and prune of over win when set
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
		Sources:   concat(base.Sources, over.Sources),
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
