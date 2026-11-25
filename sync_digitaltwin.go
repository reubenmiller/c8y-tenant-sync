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

// digitalTwinDefaultPatterns select the model files in a directory
var digitalTwinDefaultPatterns = []string{"*.yaml", "*.yml", "*.json"}

// resolvedDigitalTwinSource returns the source with the default patterns
// applied
func resolvedDigitalTwinSource(source Source) Source {
	if source.Path != "" && len(source.Patterns) == 0 {
		source.Patterns = digitalTwinDefaultPatterns
	}
	return source
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

// digitalTwinDefinitionKinds are the kinds of definition documents
var digitalTwinDefinitionKinds = []string{
	dtm.KindPropertyDefinition,
	dtm.KindAssetDefinition,
	dtm.KindMeasurementDefinition,
	dtm.KindEventDefinition,
	dtm.KindAlarmDefinition,
}

// loadDigitalTwinFile adds the definitions of a model file to the model.
// A file holds one or more objects (YAML documents separated by ---, a list,
// or JSON). Objects without a kind field get defaultKind, so plain definition
// files (as returned by the DTM API) can be used as they are.
func loadDigitalTwinFile(model *dtm.Model, path, defaultKind string) error {
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
			if kind, _ := obj["kind"].(string); kind == "" {
				if defaultKind == "" {
					return fmt.Errorf("document %d item %d has no kind field (add one, or set kind on the source for files with plain definitions)", doc, i+1)
				}
				obj["kind"] = defaultKind
			}
			if obj["kind"] == dtm.KindAsset {
				return fmt.Errorf("document %d item %d: only definitions are supported (kind %s is not synced)", doc, i+1, dtm.KindAsset)
			}
			if err := model.Add(obj); err != nil {
				return fmt.Errorf("document %d item %d: %w", doc, i+1, err)
			}
		}
	}
}

// buildDigitalTwinModel loads the model files of the sources and adds the
// inline definitions. ok is false when a source failed (already recorded).
func (s *Syncer) buildDigitalTwinModel(spec *DigitalTwinSpec) (model *dtm.Model, ok bool) {
	model = &dtm.Model{}
	ok = true
	for index, source := range spec.Sources {
		item := fmt.Sprintf("digitalTwin.sources[%d]", index)
		files, resolved := s.resolveSource(SectionDigitalTwin, item, resolvedDigitalTwinSource(source.Source))
		if !resolved {
			if !source.Optional {
				ok = false
			}
			continue
		}
		for _, file := range files {
			if s.notDownloaded(file) {
				// Remote assets are not downloaded in dry-run mode
				s.record(SectionDigitalTwin, file.Filename, ActionPlanned, "apply definitions from "+file.Filename+" (not downloaded in dry-run)", nil)
				continue
			}
			if file.Path == "" {
				s.record(SectionDigitalTwin, file.Filename, ActionFailed, "",
					fmt.Errorf("model source must provide a local file (url/linkOnly sources are not supported)"))
				ok = false
				continue
			}
			if err := loadDigitalTwinFile(model, file.Path, source.Kind); err != nil {
				s.record(SectionDigitalTwin, file.Filename, ActionFailed, "read model file", err)
				ok = false
			}
		}
	}
	for _, d := range spec.PropertyDefinitions {
		model.PropertyDefinitions = append(model.PropertyDefinitions, d.toDefinition())
	}
	for _, d := range spec.AssetDefinitions {
		model.AssetDefinitions = append(model.AssetDefinitions, d.toDefinition())
	}

	if err := checkDuplicateDefinitions(model); err != nil {
		s.record(SectionDigitalTwin, "model", ActionFailed, "", err)
		ok = false
	}
	return model, ok
}

// checkDuplicateDefinitions rejects definitions declared more than once
// (e.g. inline and in a model file), which would be applied twice
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
		{dtm.KindMeasurementDefinition, model.MeasurementDefinitions},
		{dtm.KindEventDefinition, model.EventDefinitions},
		{dtm.KindAlarmDefinition, model.AlarmDefinitions},
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
	for i, source := range spec.Sources {
		label := fmt.Sprintf("digitalTwin.sources[%d]", i)
		if source.Kind != "" && !slices.Contains(digitalTwinDefinitionKinds, source.Kind) {
			errs = append(errs, fmt.Sprintf("%s: unknown kind %q: supported values are %s", label, source.Kind, strings.Join(digitalTwinDefinitionKinds, ", ")))
		}
		if err := source.Validate(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
		}
		if source.URL != "" {
			errs = append(errs, label+": url sources are not supported (the model file must be available locally)")
		}
		if source.Cloudsmith != nil {
			errs = append(errs, label+": cloudsmith sources are not supported (use path or github)")
		}
	}

	seen := map[string]bool{}
	for i, d := range spec.PropertyDefinitions {
		label := fmt.Sprintf("digitalTwin.propertyDefinitions[%d]", i)
		if d.Identifier == "" {
			errs = append(errs, label+": identifier is required")
			continue
		}
		if d.Composition != nil {
			errs = append(errs, label+": composition only applies to asset definitions")
		}
		if seen[d.Key()] {
			errs = append(errs, fmt.Sprintf("%s: duplicate property definition %s", label, d.Key()))
		}
		seen[d.Key()] = true
	}

	seen = map[string]bool{}
	for i, d := range spec.AssetDefinitions {
		label := fmt.Sprintf("digitalTwin.assetDefinitions[%d]", i)
		if d.Identifier == "" {
			errs = append(errs, label+": identifier is required")
			continue
		}
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
		if seen[d.Identifier] {
			errs = append(errs, fmt.Sprintf("%s: duplicate asset definition %s", label, d.Identifier))
		}
		seen[d.Identifier] = true
	}
	return errs
}
