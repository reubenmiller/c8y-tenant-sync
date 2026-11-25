package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/core"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dataprep"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
)

// smartFunctionDefaultPatterns select the rule packages in a directory:
// data-prep.yaml marks a package folder, archives are exported packages
var smartFunctionDefaultPatterns = []string{dataprep.ConfigFile, "*.tar.gz", "*.tgz"}

// resolvedSource returns the spec source with the default patterns applied
func (spec SmartFunctionSpec) resolvedSource() Source {
	source := spec.Source
	if source.Path != "" && len(source.Patterns) == 0 {
		source.Patterns = smartFunctionDefaultPatterns
	}
	return source
}

// smartFunctionPackage is a rule package found in a resolved source
type smartFunctionPackage struct {
	// Name is the rule name
	Name string
	// Origin describes where the package comes from (shown as the detail)
	Origin string
	// Path is the package folder or archive file (empty when the source
	// only references a URL)
	Path  string
	IsDir bool
}

// smartFunctionPackages turns resolved files into packages: a data-prep.yaml
// file stands for the folder containing it, any other file is an archive
func smartFunctionPackages(files []ResolvedFile, nameOverride string) []smartFunctionPackage {
	packages := make([]smartFunctionPackage, 0, len(files))
	for _, file := range files {
		pkg := smartFunctionPackage{Origin: file.Filename, Path: file.Path}
		if file.Filename == dataprep.ConfigFile && file.Path != "" {
			pkg.Path = filepath.Dir(file.Path)
			pkg.IsDir = true
			pkg.Name = filepath.Base(pkg.Path)
			pkg.Origin = pkg.Name + "/"
		} else {
			pkg.Name = archiveRuleName(file.Filename)
		}
		if nameOverride != "" {
			pkg.Name = nameOverride
		}
		packages = append(packages, pkg)
	}
	return packages
}

// archiveRuleName derives the rule name from an archive filename
func archiveRuleName(filename string) string {
	for _, suffix := range []string{".tar.gz", ".tgz"} {
		if strings.HasSuffix(strings.ToLower(filename), suffix) {
			return filename[:len(filename)-len(suffix)]
		}
	}
	return filename
}

// load reads and validates the package
func (p smartFunctionPackage) load() (*dataprep.RulePackage, error) {
	var (
		pkg *dataprep.RulePackage
		err error
	)
	if p.IsDir {
		pkg, err = dataprep.PackageFromDir(p.Path)
	} else {
		file, openErr := os.Open(p.Path)
		if openErr != nil {
			return nil, openErr
		}
		defer file.Close()
		pkg, err = dataprep.PackageFromArchive(file)
	}
	if err != nil {
		return nil, err
	}
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	return pkg, nil
}

// SyncSmartFunctions deploys Data Preparation smart functions (rules) from
// rule packages. A rule is only deployed when its code, configuration or
// tests differ from the deployed rule, or when its last deployment failed
// (--force always deploys).
func (s *Syncer) SyncSmartFunctions(ctx context.Context, specs []SmartFunctionSpec) error {
	for index, spec := range specs {
		files, ok := s.resolveSource(SectionSmartFunctions, fmt.Sprintf("smartFunctions[%d]", index), spec.resolvedSource())
		if !ok {
			continue
		}

		packages := smartFunctionPackages(files, spec.Name)
		if spec.Name != "" && len(packages) > 1 {
			s.record(SectionSmartFunctions, spec.Name, ActionFailed, "",
				fmt.Errorf("source resolved to %d packages but 'name' is set; use patterns to select a single package", len(packages)))
			continue
		}

		for _, pkg := range packages {
			s.syncSmartFunction(ctx, spec, pkg)
		}
	}
	return nil
}

func (s *Syncer) syncSmartFunction(ctx context.Context, spec SmartFunctionSpec, pkg smartFunctionPackage) {
	if pkg.Path == "" {
		if s.DryRun {
			// Remote assets are not downloaded in dry-run mode
			s.record(SectionSmartFunctions, pkg.Name, ActionPlanned, "deploy rule from "+pkg.Origin, nil)
			return
		}
		s.record(SectionSmartFunctions, pkg.Name, ActionFailed, pkg.Origin,
			fmt.Errorf("rule package source must provide a local file (url/linkOnly sources are not supported)"))
		return
	}

	rule, err := pkg.load()
	if err != nil {
		s.record(SectionSmartFunctions, pkg.Name, ActionFailed, "read package "+pkg.Origin, err)
		return
	}

	if s.DryRun {
		// The package was read and validated locally, without API calls
		s.record(SectionSmartFunctions, pkg.Name, ActionPlanned, "deploy rule from "+pkg.Origin, nil)
		return
	}

	wait := dataprep.WaitOptions{Timeout: spec.waitTimeout()}
	var result op.Result[jsonmodels.DataPrepRule]
	if s.Force {
		result = s.Client.DataPreparation.Rules.Deploy(ctx, pkg.Name, dataprep.DeployOptions{Package: rule, Wait: wait})
	} else {
		result = s.Client.DataPreparation.Rules.Apply(ctx, pkg.Name, dataprep.ApplyOptions{Package: rule, Wait: wait})
	}

	detail := pkg.Origin
	if changed, ok := result.Meta["changed"].([]string); ok && len(changed) > 0 {
		detail += " (changed: " + strings.Join(changed, ", ") + ")"
	}
	s.record(SectionSmartFunctions, pkg.Name, actionFromResult(result.Status, nil), detail, smartFunctionErrorHint(result.Err))
}

// smartFunctionErrorHint explains common deployment failures
func smartFunctionErrorHint(err error) error {
	var deployErr *dataprep.DeploymentError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &deployErr):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w (increase waitTimeout, or set it to 0 to not wait)", err)
	case core.IsNotFound(err):
		return fmt.Errorf("%w (is the Data Preparation service subscribed to the tenant?)", err)
	}
	return err
}

// validateSmartFunctions checks the smartFunctions section
func validateSmartFunctions(specs []SmartFunctionSpec) []string {
	var errs []string
	for i, spec := range specs {
		label := fmt.Sprintf("smartFunctions[%d]", i)
		if err := spec.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
		}
		if spec.Source.URL != "" {
			errs = append(errs, label+": url sources are not supported (the rule package must be available locally)")
		}
		if spec.Source.Cloudsmith != nil {
			errs = append(errs, label+": cloudsmith sources are not supported (use path or github)")
		}
		if spec.WaitTimeout != "" {
			if d, err := time.ParseDuration(spec.WaitTimeout); err != nil || d < 0 {
				errs = append(errs, fmt.Sprintf("%s: invalid waitTimeout %q (use a Go duration such as 90s or 2m)", label, spec.WaitTimeout))
			}
		}
	}
	return errs
}
