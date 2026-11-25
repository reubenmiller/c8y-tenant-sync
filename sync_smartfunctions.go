package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/core"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dataprep"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/op"
	"gopkg.in/yaml.v3"
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
		if spec.IsInline() {
			s.syncInlineSmartFunction(ctx, spec)
			continue
		}
		files, ok := s.resolveSource(SectionSmartFunctions, fmt.Sprintf("smartFunctions[%d]", index), spec.resolvedSource())
		if !ok {
			continue
		}

		files, _ = s.fetchFiles(SectionSmartFunctions, files)
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
	if s.notDownloaded(ResolvedFile{Path: pkg.Path}) {
		// Remote assets are not downloaded in dry-run mode, so the package
		// cannot be validated
		s.record(SectionSmartFunctions, pkg.Name, ActionPlanned, "deploy rule from "+pkg.Origin+" (not downloaded in dry-run)", nil)
		return
	}
	if pkg.Path == "" {
		s.record(SectionSmartFunctions, pkg.Name, ActionFailed, pkg.Origin,
			fmt.Errorf("rule package source did not provide a file"))
		return
	}

	rule, err := pkg.load()
	if err != nil {
		s.record(SectionSmartFunctions, pkg.Name, ActionFailed, "read package "+pkg.Origin, err)
		return
	}

	s.deploySmartFunction(ctx, spec, pkg.Name, pkg.Origin, rule)
}

// syncInlineSmartFunction deploys a rule declared in the manifest
func (s *Syncer) syncInlineSmartFunction(ctx context.Context, spec SmartFunctionSpec) {
	origin := "inline"
	if spec.CodeFile != "" {
		origin = spec.CodeFile
	}
	rule, err := buildInlineSmartFunction(spec, s.Resolver.BaseDir)
	if err != nil {
		s.record(SectionSmartFunctions, spec.Name, ActionFailed, origin, err)
		return
	}
	s.deploySmartFunction(ctx, spec, spec.Name, origin, rule)
}

// deploySmartFunction deploys a validated rule package when it differs from
// the deployed rule (always with --force)
func (s *Syncer) deploySmartFunction(ctx context.Context, spec SmartFunctionSpec, name, origin string, rule *dataprep.RulePackage) {
	if s.DryRun {
		// The package was built and validated locally, without API calls
		s.record(SectionSmartFunctions, name, ActionPlanned, "deploy rule from "+origin, nil)
		return
	}

	wait := dataprep.WaitOptions{Timeout: spec.waitTimeout()}
	var result op.Result[jsonmodels.DataPrepRule]
	if s.Force {
		result = s.Client.DataPreparation.Rules.Deploy(ctx, name, dataprep.DeployOptions{Package: rule, Wait: wait})
	} else {
		result = s.Client.DataPreparation.Rules.Apply(ctx, name, dataprep.ApplyOptions{Package: rule, Wait: wait})
	}

	detail := origin
	if changed, ok := result.Meta["changed"].([]string); ok && len(changed) > 0 {
		detail += " (changed: " + strings.Join(changed, ", ") + ")"
	}
	s.record(SectionSmartFunctions, name, actionFromResult(result.Status, nil), detail, smartFunctionErrorHint(result.Err))
}

// SmartFunctionExpectedOutput is the expected output of a test: the
// Cumulocity objects of all inputs in order. In the manifest it is a list,
// a single object, or a string holding a JSON array or object (e.g. pasted
// from a test run).
type SmartFunctionExpectedOutput []map[string]any

// UnmarshalYAML accepts a list, an object or a JSON string
func (o *SmartFunctionExpectedOutput) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		var items []map[string]any
		if err := node.Decode(&items); err != nil {
			return err
		}
		*o = items
		return nil
	case yaml.MappingNode:
		var item map[string]any
		if err := node.Decode(&item); err != nil {
			return err
		}
		*o = SmartFunctionExpectedOutput{item}
		return nil
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			*o = nil
			return nil
		}
		items, err := parseExpectedOutputJSON(node.Value)
		if err != nil {
			return fmt.Errorf("line %d: expectedOutput: %w", node.Line, err)
		}
		*o = items
		return nil
	}
	return fmt.Errorf("line %d: expectedOutput must be a list, an object or a JSON string", node.Line)
}

// parseExpectedOutputJSON parses a JSON array of objects, or a single object.
// Text which is not strict JSON is read as YAML, which also accepts what is
// common when copying from JavaScript: trailing commas, unquoted keys and
// single quotes.
func parseExpectedOutputJSON(text string) (SmartFunctionExpectedOutput, error) {
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		jsonErr := err
		if err := yaml.Unmarshal([]byte(text), &value); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", jsonErr)
		}
		// Normalise the YAML values like JSON values
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", jsonErr)
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", jsonErr)
		}
	}
	switch v := value.(type) {
	case map[string]any:
		return SmartFunctionExpectedOutput{v}, nil
	case []any:
		items := make(SmartFunctionExpectedOutput, 0, len(v))
		for i, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("item %d is not an object", i)
			}
			items = append(items, obj)
		}
		return items, nil
	}
	return nil, fmt.Errorf("the JSON must be an array of objects or an object")
}

// JSONSchema describes the expected output as a list, an object or a string
func (SmartFunctionExpectedOutput) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Description: "Expected Cumulocity objects of all inputs in order (stored with the rule): a list, a single object, or a string holding a JSON array or object",
		OneOf: []*jsonschema.Schema{
			{Type: "array", Items: &jsonschema.Schema{Type: "object"}},
			{Type: "object"},
			{Type: "string", Description: "JSON array or object, e.g. pasted from a test run"},
		},
	}
}

// testTimeLayout is how the server returns test input times; manifest times
// are written the same way so stored tests compare equal on the next run
const testTimeLayout = "2006-01-02T15:04:05.000Z"

// buildInlineSmartFunction builds and validates the rule package of an
// inline rule. Relative code files are resolved against baseDir.
func buildInlineSmartFunction(spec SmartFunctionSpec, baseDir string) (*dataprep.RulePackage, error) {
	code := spec.Code
	if spec.CodeFile != "" {
		path := spec.CodeFile
		if !filepath.IsAbs(path) && baseDir != "" {
			path = filepath.Join(baseDir, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read codeFile: %w", err)
		}
		code = string(raw)
	}

	input := dataprep.Input{Transport: dataprep.TransportMQTT}
	if spec.Input != nil {
		input.TopicPattern = spec.Input.TopicPattern
		input.ClientIDPattern = spec.Input.ClientIDPattern
		input.TransportID = spec.Input.TransportID
		if spec.Input.Transport != "" {
			input.Transport = spec.Input.Transport
		}
	}
	rule := dataprep.NewPackage(dataprep.RuleConfig{
		SmartFunctionFile: dataprep.ExportedSmartFunctionFile,
		Description:       spec.Description,
		Tags:              spec.Tags,
		Disabled:          spec.Disabled,
		Input:             input,
	}).WithSmartFunction(code)

	for name, test := range spec.Tests {
		tc := dataprep.TestCase{ExpectedOutput: test.ExpectedOutput}
		for i, in := range test.Inputs {
			payload, format, err := testPayload(in.Payload, in.PayloadFormat)
			if err != nil {
				return nil, fmt.Errorf("test %q input %d: %w", name, i, err)
			}
			tc.Inputs = append(tc.Inputs, dataprep.TestInput{
				Topic:         in.Topic,
				ClientID:      in.ClientID,
				Payload:       payload,
				PayloadFormat: format,
				Time:          normaliseTestTime(in.Time),
			})
		}
		rule.WithTest(name, tc)
	}

	if err := rule.Validate(); err != nil {
		return nil, err
	}
	return rule, nil
}

// testPayload turns a manifest payload into the package payload: strings are
// sent as they are (json when they hold JSON, text otherwise), other values
// as JSON
func testPayload(value any, format string) (string, string, error) {
	switch v := value.(type) {
	case nil:
		if format == "" {
			format = dataprep.PayloadFormatText
		}
		return "", format, nil
	case string:
		if format == "" {
			format = dataprep.PayloadFormatText
			if json.Valid([]byte(v)) {
				format = dataprep.PayloadFormatJSON
			}
		}
		return v, format, nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "", "", fmt.Errorf("payload: %w", err)
		}
		if format == "" {
			format = dataprep.PayloadFormatJSON
		}
		return string(raw), format, nil
	}
}

// normaliseTestTime writes an RFC 3339 time like the server returns it (UTC
// with milliseconds); other values are kept for validation to report
func normaliseTestTime(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return t.UTC().Format(testTimeLayout)
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
		if spec.WaitTimeout != "" {
			if d, err := time.ParseDuration(spec.WaitTimeout); err != nil || d < 0 {
				errs = append(errs, fmt.Sprintf("%s: invalid waitTimeout %q (use a Go duration such as 90s or 2m)", label, spec.WaitTimeout))
			}
		}
		if spec.IsInline() {
			errs = append(errs, validateInlineSmartFunction(label, spec)...)
			continue
		}
		if spec.Description != "" || len(spec.Tags) > 0 || spec.Disabled || spec.Input != nil ||
			spec.Code != "" || spec.CodeFile != "" || len(spec.Tests) > 0 {
			errs = append(errs, label+": description, tags, disabled, input, code, codeFile and tests only apply to inline rules (without a source)")
		}
		if err := spec.Source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
		}
		if spec.Source.Cloudsmith != nil {
			errs = append(errs, label+": cloudsmith sources are not supported (use path or github)")
		}
	}
	return errs
}

// validateInlineSmartFunction checks an inline rule (the package itself is
// validated when it is built, as the code may come from a file)
func validateInlineSmartFunction(label string, spec SmartFunctionSpec) []string {
	var errs []string
	if spec.Name == "" {
		errs = append(errs, label+": name is required for an inline rule (or set a source)")
	}
	if spec.Input == nil || spec.Input.TopicPattern == "" {
		errs = append(errs, label+": input.topicPattern is required for an inline rule")
	}
	switch {
	case spec.Code == "" && spec.CodeFile == "":
		errs = append(errs, label+": one of code or codeFile is required for an inline rule (or set a source)")
	case spec.Code != "" && spec.CodeFile != "":
		errs = append(errs, label+": code and codeFile are mutually exclusive")
	}
	for name, test := range spec.Tests {
		if len(test.Inputs) == 0 {
			errs = append(errs, fmt.Sprintf("%s.tests.%s: needs at least one input", label, name))
		}
		for j, in := range test.Inputs {
			if in.Time == "" {
				errs = append(errs, fmt.Sprintf("%s.tests.%s.inputs[%d]: time is required", label, name, j))
			} else if _, err := time.Parse(time.RFC3339, in.Time); err != nil {
				errs = append(errs, fmt.Sprintf("%s.tests.%s.inputs[%d]: time %q is not an ISO 8601 timestamp with a time zone", label, name, j, in.Time))
			}
		}
	}
	slices.Sort(errs)
	return errs
}
