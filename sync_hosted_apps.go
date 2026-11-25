package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/applications"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
)

// isHostedWithSource reports whether the application is a web application
// (HOSTED) whose binary comes from a source. Such applications are owned by
// the tenant running the sync and kept at the version of the source.
func (a ApplicationSpec) isHostedWithSource() bool {
	return a.Source != nil && strings.EqualFold(a.Type, "HOSTED")
}

// archiveVersionPattern matches the version in a web application archive
// name, e.g. devicemanagement-1025.15.3.zip
var archiveVersionPattern = regexp.MustCompile(`(\d+(?:\.\d+)+(?:-[0-9A-Za-z.]+)?)\.zip$`)

// archiveVersion returns the version of a resolved web application archive:
// the version in the filename or, when the filename has none, the version of
// the source (e.g. the GitHub release tag)
func archiveVersion(file ResolvedFile) string {
	if match := archiveVersionPattern.FindStringSubmatch(file.Filename); match != nil {
		return match[1]
	}
	return strings.TrimPrefix(file.VersionHint, "v")
}

// ensureHostedApplication keeps a tenant-owned web application at the version
// of its source. Only an application owned by the tenant running the sync
// counts as existing: a built-in application of the same name owned by
// another tenant (e.g. devicemanagement owned by "management") is left alone
// and a tenant-owned copy with the same name and context path is created,
// which then takes the place of the subscribed one for the owner tenant.
//
// The source is listed first without downloading. The archive is only
// downloaded and uploaded when the application is new, its manifest.version
// differs from the source version, or with --force. When no version can be
// read from the source, an existing application is only re-uploaded with
// --force.
func (s *Syncer) ensureHostedApplication(ctx context.Context, spec ApplicationSpec) (string, bool) {
	if selfLink, ok := s.appSelfLinks[spec.Name]; ok {
		return selfLink, true
	}

	owner := s.baseTenant(ctx)
	if owner == "" {
		s.record(SectionApplications, spec.Name, ActionFailed, "lookup",
			fmt.Errorf("could not read the tenant running the sync (the owner of the web application)"))
		return "", false
	}

	own, foreign, err := s.hostedApplicationsByName(ctx, spec.Name, owner)
	if err != nil {
		s.record(SectionApplications, spec.Name, ActionFailed, "lookup", err)
		return "", false
	}
	if s.appOwners == nil {
		s.appOwners = map[string]string{}
	}
	s.appOwners[spec.Name] = owner

	// List the source without downloading to read the version
	probe := *s.Resolver
	probe.SkipDownload = true
	listed, err := probe.Resolve(*spec.Source)
	if err != nil {
		s.recordSourceError(spec, err)
		return "", false
	}
	if len(listed) != 1 {
		s.record(SectionApplications, spec.Name, ActionFailed, "",
			fmt.Errorf("application source must resolve to exactly one file, got %d (narrow it down with github.assets or github.tags)", len(listed)))
		return "", false
	}
	desiredVersion := archiveVersion(listed[0])

	var app jsonmodels.Application
	created := false
	currentVersion := ""
	if own == nil {
		contextPath := spec.ContextPath
		if contextPath == "" {
			contextPath = spec.Name
		}
		key := spec.Key
		if key == "" {
			key = spec.Name + "-application-key"
			if foreign != nil {
				// The key of the built-in application is taken
				key = owner + "-" + key
			}
		}
		result := s.Client.Applications.Create(ctx, map[string]any{
			"name":        spec.Name,
			"key":         key,
			"type":        "HOSTED",
			"contextPath": contextPath,
		})
		if result.Err != nil {
			s.record(SectionApplications, spec.Name, ActionFailed, "create application", result.Err)
			return "", false
		}
		app = result.Data
		created = true
	} else {
		app = *own
		currentVersion = app.Get("manifest.version").String()
		upToDate := desiredVersion != "" && desiredVersion == currentVersion
		unknown := desiredVersion == ""
		if !s.Force && (upToDate || unknown) {
			detail := "version " + currentVersion + " already active"
			if unknown {
				detail = "binary upload skipped (no version in the source; use --force to re-upload)"
			}
			s.record(SectionApplications, spec.Name, ActionUnchanged, detail, nil)
			return s.cacheAppSelfLink(spec.Name, app.Self()), true
		}
	}

	files, ok := s.resolveSource(SectionApplications, spec.Name, *spec.Source)
	if !ok {
		return "", false
	}
	if len(files) != 1 || files[0].Path == "" {
		s.record(SectionApplications, spec.Name, ActionFailed, "",
			fmt.Errorf("application source must provide exactly one local file (url/linkOnly sources are not supported for application binaries)"))
		return "", false
	}

	upload := s.Client.Applications.Upload(ctx, app.ID(), applications.UploadFileOptions{
		FilePath:    files[0].Path,
		Name:        files[0].Filename,
		ContentType: "application/zip",
	})
	if upload.Err != nil {
		s.record(SectionApplications, spec.Name, ActionFailed, "upload binary", upload.Err)
		return "", false
	}

	switch {
	case created && foreign != nil:
		s.record(SectionApplications, spec.Name, ActionCreated,
			fmt.Sprintf("created tenant-owned copy (in place of the application of tenant %s) and uploaded %s", foreign.Get("owner.tenant.id").String(), files[0].Filename), nil)
	case created:
		s.record(SectionApplications, spec.Name, ActionCreated, "created and uploaded "+files[0].Filename, nil)
	case desiredVersion != "" && desiredVersion != currentVersion:
		from := currentVersion
		if from == "" {
			from = "unknown"
		}
		s.record(SectionApplications, spec.Name, ActionUpdated,
			fmt.Sprintf("version %s -> %s (uploaded %s)", from, desiredVersion, files[0].Filename), nil)
	default:
		s.record(SectionApplications, spec.Name, ActionUpdated, "re-uploaded "+files[0].Filename, nil)
	}
	return s.cacheAppSelfLink(spec.Name, app.Self()), true
}

// recordSourceError records a failed source resolution (skipped for optional
// sources with nothing to do)
func (s *Syncer) recordSourceError(spec ApplicationSpec, err error) {
	if spec.Source.Optional && errors.Is(err, ErrNothingToDo) {
		s.record(SectionApplications, spec.Name, ActionSkipped, err.Error(), nil)
		return
	}
	s.record(SectionApplications, spec.Name, ActionFailed, "resolve source", err)
}

// hostedApplicationsByName returns the HOSTED application of the given name
// owned by the owner tenant (nil if none) and any other HOSTED application of
// that name visible to the tenant (e.g. a built-in one owned by "management")
func (s *Syncer) hostedApplicationsByName(ctx context.Context, name, owner string) (own, foreign *jsonmodels.Application, err error) {
	result := s.Client.Applications.List(ctx, applications.ListOptions{
		Name:              name,
		Type:              "HOSTED",
		PaginationOptions: pagination.PaginationOptions{PageSize: 100},
	})
	for app, err := range result.Items() {
		if err != nil {
			return nil, nil, err
		}
		if app.Name() != name {
			continue
		}
		app := app
		if app.Get("owner.tenant.id").String() == owner {
			if own == nil {
				own = &app
			}
		} else if foreign == nil {
			foreign = &app
		}
	}
	return own, foreign, nil
}
