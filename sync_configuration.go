package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/model"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/repository/configuration"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// SyncConfiguration uploads configuration files to the configuration repository.
// Each resolved file becomes a configuration item; when the spec sets a name,
// the source must resolve to a single file.
func (s *Syncer) SyncConfiguration(ctx context.Context, specs []ConfigurationSpec) error {
	for index, spec := range specs {
		if spec.IsExplicit() {
			s.syncExplicitConfiguration(ctx, spec)
			continue
		}

		files, ok := s.resolveSource(SectionConfiguration, fmt.Sprintf("configuration[%d]", index), spec.Source)
		if !ok {
			continue
		}

		if spec.Name != "" && len(files) > 1 {
			s.record(SectionConfiguration, spec.Name, ActionFailed, "",
				fmt.Errorf("source resolved to %d files but 'name' is set; use patterns to select a single file", len(files)))
			continue
		}

		for _, file := range files {
			name := spec.Name
			if name == "" {
				// Derive the name from the filename without extension
				name = strings.TrimSuffix(file.Filename, filepath.Ext(file.Filename))
			}
			item := fmt.Sprintf("%s (%s)", name, spec.ConfigurationType)
			detail := file.Filename
			if file.URL != "" {
				detail = "link → " + file.URL
			}

			if s.DryRun {
				s.record(SectionConfiguration, item, ActionPlanned, detail, nil)
				continue
			}

			createOpts := s.configurationCreateOptions(spec, name)
			if file.URL != "" {
				createOpts.URL = file.URL
			} else {
				createOpts.File = configuration.UploadFileOptions{
					Name:        file.Filename,
					ContentType: detectContentType(file.Filename),
					FilePath:    file.Path,
				}
			}
			s.upsertConfiguration(ctx, spec, item, detail, createOpts)
		}
	}
	return nil
}

// configurationCreateOptions returns the create options of a configuration
// item without its file or url
func (s *Syncer) configurationCreateOptions(spec ConfigurationSpec, name string) configuration.CreateOptions {
	return configuration.CreateOptions{
		Name:              name,
		ConfigurationType: spec.ConfigurationType,
		Description:       spec.Description,
		DeviceType:        spec.DeviceType,
		// Annotation: written alongside real changes, but never a reason to
		// update by itself
		Annotations: []model.Fragment{
			model.Frag(SyncToolFragment, syncMeta()),
		},
	}
}

// upsertConfiguration creates the configuration item if it does not exist
// (or replaces it with --force or the entry's force) and records the outcome
func (s *Syncer) upsertConfiguration(ctx context.Context, spec ConfigurationSpec, item, detail string, createOpts configuration.CreateOptions) {
	var result op.Result[jsonmodels.Configuration]
	if s.Force || spec.Force {
		result = s.Client.Repository.Configuration.UpsertByName(ctx, createOpts)
	} else {
		result = s.Client.Repository.Configuration.GetOrCreate(ctx, createOpts)
	}
	if result.Err != nil {
		s.record(SectionConfiguration, item, ActionFailed, detail, result.Err)
		return
	}
	s.record(SectionConfiguration, item, actionFromResult(result.Status, result.Meta), detail, nil)
}

// syncExplicitConfiguration syncs a configuration declared without a source:
// with a url, with inline content (uploaded as a file), or with neither (an
// empty url)
func (s *Syncer) syncExplicitConfiguration(ctx context.Context, spec ConfigurationSpec) {
	item := fmt.Sprintf("%s (%s)", spec.Name, spec.ConfigurationType)
	detail := "no file (no url)"
	switch {
	case spec.URL != "":
		detail = "link → " + spec.URL
	case spec.Content != "":
		detail = fmt.Sprintf("inline content as %s (%d bytes)", spec.UploadFilename(), len(spec.Content))
	}

	if s.DryRun {
		s.record(SectionConfiguration, item, ActionPlanned, detail, nil)
		return
	}

	createOpts := s.configurationCreateOptions(spec, spec.Name)
	switch {
	case spec.URL != "":
		createOpts.URL = spec.URL
	case spec.Content != "":
		path, cleanup, err := writeInlineFile(s.Resolver.WorkDir, spec.UploadFilename(), spec.Content)
		if err != nil {
			s.record(SectionConfiguration, item, ActionFailed, "write inline content", err)
			return
		}
		defer cleanup()
		createOpts.File = configuration.UploadFileOptions{
			Name:        spec.UploadFilename(),
			ContentType: detectContentType(spec.UploadFilename()),
			FilePath:    path,
		}
	}
	s.upsertConfiguration(ctx, spec, item, detail, createOpts)
}

// writeInlineFile writes inline content to a file with the given name in a
// new temporary directory (inside workDir when set), so it can be uploaded
// with its own filename
func writeInlineFile(workDir, filename, content string) (string, func(), error) {
	dir, err := os.MkdirTemp(workDir, "inline-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, filepath.Base(filename))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}
