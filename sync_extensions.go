package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/ui/applicationplugins"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/ui/plugins"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/tidwall/gjson"
)

// SyncExtensions uploads UI extensions (plugin packages) into the tenant
// running the sync. An extension is created when missing and a version is
// only uploaded when the source version is not on the extension yet. The
// extensions are owned by the tenant running the sync, so the work happens
// once per run, not once per target.
func (s *Syncer) SyncExtensions(ctx context.Context, specs []ExtensionSpec) error {
	for _, spec := range specs {
		if s.extensionsDone[spec.Name] {
			continue
		}
		if s.extensionsDone == nil {
			s.extensionsDone = map[string]bool{}
		}
		s.extensionsDone[spec.Name] = true

		if s.DryRun {
			s.record(SectionExtensions, spec.Name, ActionPlanned, "create if missing, upload the source version if it is not on the extension yet", nil)
			continue
		}
		s.ensureExtension(ctx, spec)
	}
	return nil
}

// ensureExtension creates the extension when missing and uploads the source
// as a new version when the version is not on the extension yet
func (s *Syncer) ensureExtension(ctx context.Context, spec ExtensionSpec) {
	owner := s.baseTenant(ctx)
	if owner == "" {
		s.record(SectionExtensions, spec.Name, ActionFailed, "lookup",
			fmt.Errorf("could not read the tenant running the sync (the owner of the extension)"))
		return
	}

	// List the source without downloading to read the version
	probe := *s.Resolver
	probe.SkipDownload = true
	listed, err := probe.Resolve(spec.Source)
	if err != nil {
		s.recordExtensionSourceError(spec, err)
		return
	}
	if len(listed) != 1 {
		s.record(SectionExtensions, spec.Name, ActionFailed, "",
			fmt.Errorf("extension source must resolve to exactly one file, got %d (narrow it down with github.assets or github.tags)", len(listed)))
		return
	}
	version := archiveVersion(listed[0])

	existing, err := s.ownedExtension(ctx, spec.Name, owner)
	if err != nil {
		s.record(SectionExtensions, spec.Name, ActionFailed, "lookup", err)
		return
	}
	if existing != nil && version != "" && hasExtensionVersion(*existing, version) {
		s.recordUploadedExtension(ctx, spec, *existing, version)
		return
	}

	files, ok := s.resolveSource(SectionExtensions, spec.Name, spec.Source)
	if !ok {
		return
	}
	if len(files) != 1 || files[0].Path == "" {
		s.record(SectionExtensions, spec.Name, ActionFailed, "",
			fmt.Errorf("extension source must provide exactly one local file (url/linkOnly sources are not supported for extensions)"))
		return
	}

	manifest := plugins.ManifestFile{}
	if err := plugins.GetManifestContents(files[0].Path, &manifest); err != nil {
		s.record(SectionExtensions, spec.Name, ActionFailed, "read "+files[0].Filename, err)
		return
	}
	if manifest.Name != "" && manifest.Name != spec.Name {
		s.record(SectionExtensions, spec.Name, ActionFailed, "",
			fmt.Errorf("%s contains the extension %q, not %q", files[0].Filename, manifest.Name, spec.Name))
		return
	}
	if manifest.Version != "" {
		// The version in the archive wins over the one in the filename
		version = manifest.Version
	}
	if version == "" {
		s.record(SectionExtensions, spec.Name, ActionFailed, "",
			fmt.Errorf("no version found in %s (neither in the filename nor in its cumulocity.json)", files[0].Filename))
		return
	}
	if existing != nil && hasExtensionVersion(*existing, version) {
		s.recordUploadedExtension(ctx, spec, *existing, version)
		return
	}

	created := false
	appID := ""
	if existing != nil {
		appID = existing.ID()
	} else {
		plugin := plugins.NewPlugin(spec.Name)
		if manifest.Key != "" {
			plugin.Key = manifest.Key
		}
		if manifest.ContextPath != "" {
			plugin.ContextPath = manifest.ContextPath
		}
		if manifest.Package != "" {
			plugin.Manifest.WithPackage(manifest.Package)
		}
		result := s.Client.UIPlugins.Create(ctx, plugin)
		if result.Err != nil {
			s.record(SectionExtensions, spec.Name, ActionFailed, "create extension", result.Err)
			return
		}
		appID = result.Data.ID()
		created = true
	}

	tags := spec.Tags
	if tags == nil {
		tags = []string{applicationplugins.VersionLatest}
	}
	upload := s.Client.ApplicationVersions.CreateFromFile(ctx, appID, files[0].Path, version, tags)
	if upload.Err != nil {
		s.record(SectionExtensions, spec.Name, ActionFailed, "upload version "+version, upload.Err)
		return
	}

	detail := fmt.Sprintf("uploaded version %s (%s)", version, files[0].Filename)
	if created || slices.Contains(tags, applicationplugins.VersionLatest) {
		// Like the extensions uploaded in the Administration application, the
		// latest version is the active one
		if result := s.Client.UIPlugins.Activate(ctx, appID, upload.Data.BinaryID()); result.Err != nil {
			s.record(SectionExtensions, spec.Name, ActionFailed, detail+"; activate version "+version, result.Err)
			return
		}
	}
	if created {
		s.record(SectionExtensions, spec.Name, ActionCreated, "created and "+detail, nil)
	} else {
		s.record(SectionExtensions, spec.Name, ActionUpdated, detail, nil)
	}
}

// recordExtensionSourceError records a failed source resolution (skipped for
// optional sources with nothing to do)
func (s *Syncer) recordExtensionSourceError(spec ExtensionSpec, err error) {
	if spec.Source.Optional && errors.Is(err, ErrNothingToDo) {
		s.record(SectionExtensions, spec.Name, ActionSkipped, err.Error(), nil)
		return
	}
	s.record(SectionExtensions, spec.Name, ActionFailed, "resolve source", err)
}

// ownedExtension returns the extension of the given name owned by the owner
// tenant (nil if none)
func (s *Syncer) ownedExtension(ctx context.Context, name, owner string) (*jsonmodels.UIPlugin, error) {
	result := s.Client.UIPlugins.List(ctx, plugins.ListOptions{
		Name:              name,
		PaginationOptions: pagination.PaginationOptions{PageSize: 100},
	})
	for plugin, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		if plugin.Name() == name && plugin.Get("owner.tenant.id").String() == owner {
			return &plugin, nil
		}
	}
	return nil, nil
}

// visibleExtension returns the extension of the given name (or context path)
// which the tenant can install, preferring one owned by the owner tenant
func (s *Syncer) visibleExtension(ctx context.Context, name, owner string) (*jsonmodels.UIPlugin, error) {
	result := s.Client.UIPlugins.List(ctx, plugins.ListOptions{
		Name:              name,
		PaginationOptions: pagination.PaginationOptions{PageSize: 100},
	})
	var found *jsonmodels.UIPlugin
	for plugin, err := range result.Items() {
		if err != nil {
			return nil, err
		}
		if plugin.Name() != name && plugin.ContextPath() != name {
			continue
		}
		if plugin.Get("owner.tenant.id").String() == owner {
			return &plugin, nil
		}
		if found == nil {
			found = &plugin
		}
	}
	return found, nil
}

// recordUploadedExtension records an extension whose version is already
// uploaded. An extension without an active version (uploaded by earlier
// versions of the tool) gets the version with the latest tag activated.
func (s *Syncer) recordUploadedExtension(ctx context.Context, spec ExtensionSpec, plugin jsonmodels.UIPlugin, version string) {
	detail := "version " + version + " already uploaded"
	if plugin.ActiveVersionID() != "" {
		s.record(SectionExtensions, spec.Name, ActionUnchanged, detail, nil)
		return
	}
	active := resolveExtensionVersion(plugin, applicationplugins.VersionLatest)
	if active == "" {
		active = version
	}
	binaryID := ""
	for _, v := range plugin.Get("applicationVersions").Array() {
		if v.Get("version").String() == active {
			binaryID = v.Get("binaryId").String()
		}
	}
	if binaryID == "" {
		s.record(SectionExtensions, spec.Name, ActionFailed, detail, fmt.Errorf("no binary found for version %s to activate", active))
		return
	}
	if result := s.Client.UIPlugins.Activate(ctx, plugin.ID(), binaryID); result.Err != nil {
		s.record(SectionExtensions, spec.Name, ActionFailed, detail+"; activate version "+active, result.Err)
		return
	}
	s.record(SectionExtensions, spec.Name, ActionUpdated, detail+"; activated version "+active, nil)
}

// extensionModules returns the modules of a version of an extension, which
// are installed into a web application: the exports of the cumulocity.json of
// the version (as the Administration application does), or its remotes for
// extensions without exports
func (s *Syncer) extensionModules(ctx context.Context, contextPath, version string) ([]string, error) {
	resp, err := s.Client.Resty().R().
		SetContext(ctx).
		SetHeader("Accept", "application/json").
		Get("/apps/" + url.PathEscape(contextPath+"@"+version) + "/cumulocity.json")
	if err != nil {
		return nil, fmt.Errorf("read the cumulocity.json of %s@%s: %w", contextPath, version, err)
	}
	if resp.StatusCode() >= 300 {
		return nil, fmt.Errorf("read the cumulocity.json of %s@%s: %s", contextPath, version, resp.Status())
	}
	manifest := gjson.ParseBytes(resp.Bytes())
	var modules []string
	for _, export := range manifest.Get("exports").Array() {
		if name := export.Get("module").String(); name != "" && !slices.Contains(modules, name) {
			modules = append(modules, name)
		}
	}
	if len(modules) == 0 {
		manifest.Get("remotes").ForEach(func(_, list gjson.Result) bool {
			for _, m := range list.Array() {
				if name := m.String(); name != "" && !slices.Contains(modules, name) {
					modules = append(modules, name)
				}
			}
			return true
		})
	}
	slices.Sort(modules)
	return modules, nil
}

// hasExtensionVersion reports whether the extension has the version
func hasExtensionVersion(plugin jsonmodels.UIPlugin, version string) bool {
	return slices.ContainsFunc(plugin.Get("applicationVersions").Array(), func(v gjson.Result) bool {
		return v.Get("version").String() == version
	})
}

// resolveExtensionVersion returns the version of the extension which the
// version or tag selects ("" if none)
func resolveExtensionVersion(plugin jsonmodels.UIPlugin, versionOrTag string) string {
	for _, v := range plugin.Get("applicationVersions").Array() {
		version := v.Get("version").String()
		if version == versionOrTag || slices.ContainsFunc(v.Get("tags").Array(), func(t gjson.Result) bool { return t.String() == versionOrTag }) {
			return version
		}
	}
	return ""
}

// pendingExtension is an extension change of a web application which is
// recorded once the application is updated
type pendingExtension struct {
	item   string
	detail string
}

// syncApplicationExtensions installs, upgrades or removes the extensions of
// a web application (its config.remotes). Installed extensions which are not
// listed are kept. Only an application owned by the tenant running the sync
// can be changed, so the work happens once per run.
func (s *Syncer) syncApplicationExtensions(ctx context.Context, spec ApplicationSpec) {
	if s.appExtensionsDone[spec.Name] {
		return
	}
	if s.appExtensionsDone == nil {
		s.appExtensionsDone = map[string]bool{}
	}
	s.appExtensionsDone[spec.Name] = true

	item := func(ext ApplicationExtensionSpec) string { return spec.Name + "/" + ext.Name }

	if s.DryRun {
		for _, ext := range spec.Extensions {
			detail := "install " + ext.VersionOrLatest()
			if !ext.IsInstalled() {
				detail = "remove"
			}
			s.record(SectionApplications, item(ext), ActionPlanned, detail, nil)
		}
		return
	}

	failAll := func(detail string, err error) {
		for _, ext := range spec.Extensions {
			s.record(SectionApplications, item(ext), ActionFailed, detail, err)
		}
	}

	owner := s.baseTenant(ctx)
	if owner == "" {
		failAll("lookup", fmt.Errorf("could not read the tenant running the sync"))
		return
	}
	own, _, err := s.hostedApplicationsByName(ctx, spec.Name, owner)
	if err != nil {
		failAll("lookup", err)
		return
	}
	if own == nil {
		if spec.Source != nil {
			// The deployment failed and was already reported
			for _, ext := range spec.Extensions {
				s.record(SectionApplications, item(ext), ActionSkipped, "the web application was not deployed", nil)
			}
			return
		}
		failAll("", fmt.Errorf("no web application %q owned by tenant %s: extensions can only be installed into a tenant-owned application (add a source to replace a built-in application with a tenant-owned copy)", spec.Name, owner))
		return
	}

	current := s.Client.UIApplicationPlugins.List(ctx, own.ID())
	if current.Err != nil {
		failAll("list installed extensions", current.Err)
		return
	}
	installed := map[string]applicationplugins.InstalledPlugin{}
	for _, p := range current.Data.Plugins() {
		installed[p.ContextPath] = p
	}

	install := map[string]any{}
	var remove []string
	var pending []pendingExtension
	for _, ext := range spec.Extensions {
		plugin, err := s.visibleExtension(ctx, ext.Name, owner)
		if err != nil {
			s.record(SectionApplications, item(ext), ActionFailed, "lookup", err)
			continue
		}
		contextPath := ext.Name
		if plugin != nil {
			contextPath = plugin.ContextPath()
		}
		have, isInstalled := installed[contextPath]

		if !ext.IsInstalled() {
			if !isInstalled {
				s.record(SectionApplications, item(ext), ActionUnchanged, "not installed", nil)
				continue
			}
			remove = append(remove, contextPath)
			pending = append(pending, pendingExtension{item(ext), "removed version " + have.Version})
			continue
		}

		if plugin == nil {
			s.record(SectionApplications, item(ext), ActionFailed, "",
				fmt.Errorf("extension %q not found (upload it with the extensions section)", ext.Name))
			continue
		}
		version := resolveExtensionVersion(*plugin, ext.VersionOrLatest())
		if version == "" {
			s.record(SectionApplications, item(ext), ActionFailed, "",
				fmt.Errorf("extension %q has no version or tag %q", ext.Name, ext.VersionOrLatest()))
			continue
		}
		modules, err := s.extensionModules(ctx, plugin.ContextPath(), version)
		if err != nil {
			s.record(SectionApplications, item(ext), ActionFailed, "", err)
			continue
		}
		installedModules := slices.Sorted(slices.Values(have.Modules))
		modulesMatch := slices.Equal(modules, installedModules)
		if isInstalled && have.Version == version && modulesMatch {
			s.record(SectionApplications, item(ext), ActionUnchanged, "version "+version+" installed", nil)
			continue
		}
		// The remotes entry is written as the Administration application does;
		// an installed version is replaced
		if isInstalled {
			remove = append(remove, contextPath)
		}
		install[contextPath+"@"+version] = modules
		detail := "installed version " + version
		switch {
		case isInstalled && have.Version != version:
			detail = fmt.Sprintf("version %s -> %s", have.Version, version)
		case isInstalled:
			detail = fmt.Sprintf("version %s: modules updated (%s)", version, strings.Join(modules, ", "))
		}
		pending = append(pending, pendingExtension{item(ext), detail})
	}
	if len(pending) == 0 {
		return
	}

	result := s.Client.UIApplicationPlugins.Update(ctx, own.ID(), applicationplugins.UpdateOptions{
		Remove: remove,
		Body:   map[string]any{"config": map[string]any{"remotes": install}},
	})
	for _, change := range pending {
		if result.Err != nil {
			s.record(SectionApplications, change.item, ActionFailed, strings.Replace(change.detail, "installed", "install", 1), result.Err)
			continue
		}
		s.record(SectionApplications, change.item, ActionUpdated, change.detail, nil)
	}
}
