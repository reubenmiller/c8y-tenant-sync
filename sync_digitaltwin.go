package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/core"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dtm"
	"gopkg.in/yaml.v3"
)

// digitalTwinDefaultPatterns select the definition files in a directory
var digitalTwinDefaultPatterns = []string{"*.json", "*.yaml", "*.yml"}

// resolvedDigitalTwinSource returns the source with the default patterns
// applied
func resolvedDigitalTwinSource(source Source) Source {
	if source.Path != "" && len(source.Patterns) == 0 {
		source.Patterns = digitalTwinDefaultPatterns
	}
	return source
}

// digitalTwinKey describes one definition list of the digitalTwin section
type digitalTwinKey struct {
	// name is the manifest key, e.g. propertyDefinitions
	name string
	// kind is the SDK model kind of the definitions
	kind  string
	specs []DigitalTwinDefinitionSpec
}

// keys lists the definition lists of the section in apply order
func (spec *DigitalTwinSpec) keys() []digitalTwinKey {
	return []digitalTwinKey{
		{"propertyDefinitions", dtm.KindPropertyDefinition, spec.PropertyDefinitions},
		{"assetDefinitions", dtm.KindAssetDefinition, spec.AssetDefinitions},
	}
}

// toDefinition converts the manifest definition into the SDK type
func (d DigitalTwinDefinitionSpec) toDefinition() dtm.Definition {
	def := dtm.Definition{
		Identifier: d.Identifier,
		Contexts:   d.Contexts,
		Tags:       d.Tags,
		JSONSchema: d.JSONSchema,
		Extra:      d.Fragments,
	}
	if c := d.Composition; c != nil {
		def.Composition = &dtm.Composition{
			AllowedProperties:    toAllowedItems(c.AllowedProperties),
			AllowedSubAssets:     toAllowedItems(c.AllowedSubAssets),
			AdditionalProperties: c.AdditionalProperties,
			AdditionalSubAssets:  c.AdditionalSubAssets,
		}
	}
	return def
}

func toAllowedItems(items []DigitalTwinAllowedItem) []dtm.AllowedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]dtm.AllowedItem, 0, len(items))
	for _, item := range items {
		out = append(out, dtm.AllowedItem{Identifier: item.Identifier, MinOccurs: item.MinOccurs, MaxOccurs: item.MaxOccurs})
	}
	return out
}

// definitionKey identifies a definition within its kind, as reported by the
// SDK: the identifier and, when set, the sorted contexts
func definitionKey(d dtm.Definition) string {
	return DigitalTwinDefinitionSpec{Identifier: d.Identifier, Contexts: d.Contexts}.Key()
}

// loadDigitalTwinFile adds the definitions of a file to the model, as the
// given kind. A file holds one or more definitions (JSON, YAML documents
// separated by ---, or a list) in the format of the DTM API. A kind field is
// optional, but must match the manifest key the file is listed under.
func loadDigitalTwinFile(model *dtm.Model, path, kind string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	for doc := 1; ; doc++ {
		var value any
		if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		items, isList := value.([]any)
		if !isList {
			items = []any{value}
		}
		for i, item := range items {
			if item == nil {
				continue
			}
			obj, isObject := item.(map[string]any)
			if !isObject {
				return fmt.Errorf("document %d item %d is not an object", doc, i+1)
			}
			if declared, _ := obj["kind"].(string); declared != "" && declared != kind {
				return fmt.Errorf("document %d item %d is a %s, but the file is listed as %s (move it to the matching key)", doc, i+1, declared, kind)
			}
			obj["kind"] = kind
			if err := model.Add(obj); err != nil {
				return fmt.Errorf("document %d item %d: %w", doc, i+1, err)
			}
		}
	}
}

// buildDigitalTwinModel loads the definition files and adds the inline
// definitions, keeping the order of each list. ok is false when a source
// failed (already recorded).
func (s *Syncer) buildDigitalTwinModel(spec *DigitalTwinSpec) (model *dtm.Model, ok bool) {
	model = &dtm.Model{}
	ok = true
	for _, key := range spec.keys() {
		for index, entry := range key.specs {
			if entry.Source == nil {
				def := entry.toDefinition()
				if key.kind == dtm.KindAssetDefinition {
					model.AssetDefinitions = append(model.AssetDefinitions, def)
				} else {
					model.PropertyDefinitions = append(model.PropertyDefinitions, def)
				}
				continue
			}
			if !s.loadDigitalTwinSource(model, key, index, *entry.Source) {
				ok = false
			}
		}
	}

	if err := checkDuplicateDefinitions(model); err != nil {
		s.record(SectionDigitalTwin, "model", ActionFailed, "", err)
		ok = false
	}
	return model, ok
}

// loadDigitalTwinSource adds the definitions of one source entry to the
// model. It returns false when the source failed (already recorded); an
// optional source without files is not a failure.
func (s *Syncer) loadDigitalTwinSource(model *dtm.Model, key digitalTwinKey, index int, source Source) bool {
	label := fmt.Sprintf("digitalTwin.%s[%d]", key.name, index)
	files, resolved := s.resolveSource(SectionDigitalTwin, label, resolvedDigitalTwinSource(source))
	if !resolved {
		return source.Optional
	}
	files, ok := s.fetchFiles(SectionDigitalTwin, files)
	for _, file := range files {
		if s.notDownloaded(file) {
			// Remote files are not downloaded in dry-run mode
			s.record(SectionDigitalTwin, file.Filename, ActionPlanned, "apply "+key.kind+"s from "+file.Filename+" (not downloaded in dry-run)", nil)
			continue
		}
		if file.Path == "" {
			s.record(SectionDigitalTwin, file.Filename, ActionFailed, "", fmt.Errorf("definition source did not provide a file"))
			ok = false
			continue
		}
		if err := loadDigitalTwinFile(model, file.Path, key.kind); err != nil {
			s.record(SectionDigitalTwin, file.Filename, ActionFailed, "read definition file", err)
			ok = false
		}
	}
	return ok
}

// checkDuplicateDefinitions rejects definitions declared more than once
// (e.g. inline and in a file), which would be applied twice
func checkDuplicateDefinitions(model *dtm.Model) error {
	var duplicates []string
	for _, group := range digitalTwinGroups(model) {
		seen := map[string]bool{}
		for _, d := range group.definitions {
			key := definitionKey(d)
			if seen[key] {
				duplicates = append(duplicates, group.kind+" "+key)
			}
			seen[key] = true
		}
	}
	if len(duplicates) > 0 {
		return fmt.Errorf("definitions declared more than once: %s", strings.Join(duplicates, ", "))
	}
	return nil
}

type digitalTwinGroup struct {
	kind        string
	definitions []dtm.Definition
}

// digitalTwinGroups lists the definitions of the model in apply order
func digitalTwinGroups(model *dtm.Model) []digitalTwinGroup {
	return []digitalTwinGroup{
		{dtm.KindPropertyDefinition, model.PropertyDefinitions},
		{dtm.KindAssetDefinition, model.AssetDefinitions},
	}
}

// SyncDigitalTwin creates or updates the Digital Twin Manager definitions of
// the manifest, in dependency order (property definitions before the asset
// definitions using them). Definitions which already match are not written.
// With prune, definitions tagged managedBy which are no longer declared are
// deleted.
func (s *Syncer) SyncDigitalTwin(ctx context.Context, spec *DigitalTwinSpec) error {
	if spec == nil {
		return nil
	}

	model, ok := s.buildDigitalTwinModel(spec)
	if !ok {
		// Never apply (and in particular never prune) a partial model
		return nil
	}

	if s.DryRun {
		for _, group := range digitalTwinGroups(model) {
			for _, d := range group.definitions {
				s.record(SectionDigitalTwin, group.kind+" "+definitionKey(d), ActionPlanned, "create or update", nil)
			}
		}
		if spec.PruneEnabled() {
			s.record(SectionDigitalTwin, "prune", ActionPlanned,
				fmt.Sprintf("delete definitions tagged %q which are not declared", spec.ManagedBy), nil)
		}
		return nil
	}

	result := s.Client.DigitalTwin.Apply(ctx, model, dtm.ApplyOptions{
		ManagedBy:       spec.ManagedBy,
		Prune:           spec.PruneEnabled(),
		ContinueOnError: true,
	})
	if result.Data == nil {
		// The model was rejected before anything was applied (e.g. a cycle
		// in the asset definition compositions)
		s.record(SectionDigitalTwin, "model", ActionFailed, "", digitalTwinErrorHint(result.Err))
		return nil
	}

	for _, item := range result.Data.Items {
		detail := ""
		if len(item.Changed) > 0 {
			detail = "changed: " + strings.Join(item.Changed, ", ")
		}
		s.record(SectionDigitalTwin, item.Kind+" "+item.Key, digitalTwinAction(item.Action), detail, digitalTwinErrorHint(item.Err))
	}
	return nil
}

// digitalTwinAction maps an SDK apply action to a sync action
func digitalTwinAction(action dtm.Action) Action {
	switch action {
	case dtm.ActionCreate:
		return ActionCreated
	case dtm.ActionUpdate:
		return ActionUpdated
	case dtm.ActionDelete:
		return ActionDeleted
	}
	return ActionUnchanged
}

// digitalTwinErrorHint explains common failures
func digitalTwinErrorHint(err error) error {
	if err != nil && core.IsNotFound(err) && strings.Contains(err.Error(), "/service/dtm") {
		return fmt.Errorf("%w (is the Digital Twin Manager subscribed to the tenant?)", err)
	}
	return err
}

// validate checks the digitalTwin section
func (spec *DigitalTwinSpec) validate() []string {
	if spec == nil {
		return nil
	}
	var errs []string
	if spec.PruneEnabled() && strings.TrimSpace(spec.ManagedBy) == "" {
		errs = append(errs, "digitalTwin: prune requires managedBy (the tag marking the definitions the manifest owns)")
	}
	for _, key := range spec.keys() {
		seen := map[string]bool{}
		for i, d := range key.specs {
			label := fmt.Sprintf("digitalTwin.%s[%d]", key.name, i)
			if d.Source != nil {
				errs = append(errs, validateDigitalTwinSource(label, d)...)
				continue
			}
			if d.Identifier == "" {
				errs = append(errs, label+": identifier (or source) is required")
				continue
			}
			if key.kind == dtm.KindPropertyDefinition && d.Composition != nil {
				errs = append(errs, label+": composition only applies to asset definitions")
			}
			if key.kind == dtm.KindAssetDefinition {
				if len(d.Contexts) > 0 {
					errs = append(errs, label+": contexts only apply to property definitions")
				}
				if d.Composition != nil {
					for j, item := range slices.Concat(d.Composition.AllowedProperties, d.Composition.AllowedSubAssets) {
						if item.Identifier == "" {
							errs = append(errs, fmt.Sprintf("%s.composition: item %d: identifier is required", label, j))
						}
					}
				}
			}
			if seen[d.Key()] {
				errs = append(errs, fmt.Sprintf("%s: duplicate definition %s", label, d.Key()))
			}
			seen[d.Key()] = true
		}
	}
	return errs
}

// validateDigitalTwinSource checks a source entry: it has no inline fields
func validateDigitalTwinSource(label string, d DigitalTwinDefinitionSpec) []string {
	var errs []string
	if d.Source.Cloudsmith != nil {
		errs = append(errs, label+".source: cloudsmith sources are not supported (use path, url or github)")
	} else if err := d.Source.Validate(); err != nil {
		errs = append(errs, fmt.Sprintf("%s.source: %v", label, err))
	}
	inline := d
	inline.Source = nil
	if !inline.isEmpty() {
		errs = append(errs, label+": source and inline definition fields (identifier, jsonSchema, ...) are mutually exclusive")
	}
	return errs
}

// isEmpty reports whether no field is set
func (d DigitalTwinDefinitionSpec) isEmpty() bool {
	return d.Source == nil && d.Identifier == "" && len(d.Contexts) == 0 && len(d.Tags) == 0 &&
		len(d.JSONSchema) == 0 && d.Composition == nil && len(d.Fragments) == 0
}
