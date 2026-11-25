package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/core"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dataprep"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dtm"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/pagination"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/jsonmodels"
)

// definitionBookkeepingFields are fields of a stored definition which the
// platform maintains; they are not exported
var definitionBookkeepingFields = []string{
	"id", "self", "owner", "creationTime", "lastUpdated",
	"c8y_AvailableActions", "c8y_Origin", "c8y_SharedDefinition",
}

// exportableDefinition reports whether a definition belongs to the tenant:
// library definitions and definitions shared from the enterprise tenant are
// skipped, as they cannot be created or changed by a manifest
func exportableDefinition(def jsonmodels.DigitalTwinDefinition) bool {
	return def.Origin() != "library" && !def.IsShared() && def.CanEdit()
}

// skipUnavailable reports (on stderr) that a service is not available in the
// tenant, so a full export still works for tenants without it
func skipUnavailable(section, service string) {
	fmt.Fprintf(os.Stderr, "⏭ %s: skipped (%s is not available in the tenant)\n", section, service)
}

// digitalTwin exports the property and asset definitions of the tenant
func (e *Exporter) digitalTwin(ctx context.Context) (*DigitalTwinSpec, error) {
	spec := &DigitalTwinSpec{}
	var err error
	spec.PropertyDefinitions, err = e.definitions(ctx, e.Client.DigitalTwin.Definitions.Properties, dtm.KindPropertyDefinition)
	if core.IsNotFound(err) {
		skipUnavailable(SectionDigitalTwin, "the Digital Twin Manager")
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("property definitions: %w", err)
	}
	if spec.AssetDefinitions, err = e.definitions(ctx, e.Client.DigitalTwin.Definitions.Assets, dtm.KindAssetDefinition); err != nil {
		return nil, fmt.Errorf("asset definitions: %w", err)
	}
	if len(spec.PropertyDefinitions) == 0 && len(spec.AssetDefinitions) == 0 {
		return nil, nil
	}
	return spec, nil
}

func (e *Exporter) definitions(ctx context.Context, svc *dtm.DefinitionService, kind string) ([]DigitalTwinDefinitionSpec, error) {
	var specs []DigitalTwinDefinitionSpec
	for def, err := range svc.ListAll(ctx, dtm.ListDefinitionsOptions{
		PaginationOptions: pagination.PaginationOptions{PageSize: 2000},
	}).Items() {
		if err != nil {
			return nil, err
		}
		if !exportableDefinition(def) {
			continue
		}
		spec, err := exportDefinition(def.Bytes(), kind)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", def.Identifier(), err)
		}
		specs = append(specs, spec)
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Key() < specs[j].Key() })
	return specs, nil
}

// exportDefinition turns a stored definition into a manifest definition:
// server generated jsonSchema fields and platform bookkeeping fields are
// left out, and other custom fields become fragments
func exportDefinition(raw []byte, kind string) (DigitalTwinDefinitionSpec, error) {
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return DigitalTwinDefinitionSpec{}, err
	}
	spec := DigitalTwinDefinitionSpec{
		Identifier: stringOf(stored["identifier"]),
		Contexts:   stringsOf(stored["contexts"]),
		Tags:       stringsOf(stored["tags"]),
	}
	slices.Sort(spec.Contexts)
	slices.Sort(spec.Tags)

	if schema, ok := stored["jsonSchema"].(map[string]any); ok {
		spec.JSONSchema = exportSchema(schema, kind)
	}
	if kind == dtm.KindAssetDefinition {
		if composition, ok := stored["composition"]; ok {
			if err := convertJSON(composition, &spec.Composition); err != nil {
				return spec, fmt.Errorf("composition: %w", err)
			}
			if c := spec.Composition; c != nil {
				// Empty lists are left out, like in a manifest
				if len(c.AllowedProperties) == 0 {
					c.AllowedProperties = nil
				}
				if len(c.AllowedSubAssets) == 0 {
					c.AllowedSubAssets = nil
				}
			}
		}
	}

	for key, value := range stored {
		switch key {
		case "identifier", "contexts", "tags", "jsonSchema", "composition":
			continue
		}
		if slices.Contains(definitionBookkeepingFields, key) {
			continue
		}
		if spec.Fragments == nil {
			spec.Fragments = map[string]any{}
		}
		spec.Fragments[key] = value
	}
	return spec, nil
}

// exportSchema keeps the declared part of a stored jsonSchema: the fields
// the server generates are left out (they are kept by the sync anyway)
func exportSchema(schema map[string]any, kind string) map[string]any {
	out := map[string]any{}
	if kind == dtm.KindAssetDefinition {
		// The server builds everything but title and description from the
		// composition
		for _, key := range assetSchemaFields {
			if value, ok := schema[key]; ok {
				out[key] = value
			}
		}
		return out
	}
	for key, value := range schema {
		switch key {
		case "$schema":
			continue
		case "additionalProperties":
			// The server sets false when a definition does not declare it
			if value == false {
				continue
			}
		}
		out[key] = value
	}
	return out
}

// smartFunctions exports the deployed Data Preparation rules as inline rules
func (e *Exporter) smartFunctions(ctx context.Context) ([]SmartFunctionSpec, error) {
	list := e.Client.DataPreparation.Rules.List(ctx, dataprep.ListOptions{})
	if core.IsNotFound(list.Err) {
		skipUnavailable(SectionSmartFunctions, "Data Preparation")
		return nil, nil
	}
	if list.Err != nil {
		return nil, list.Err
	}
	var specs []SmartFunctionSpec
	for rule := range list.Seq() {
		exported := e.Client.DataPreparation.Rules.Export(ctx, rule.Name(), dataprep.ExportOptions{})
		if exported.Err != nil {
			return nil, fmt.Errorf("%s: %w", rule.Name(), exported.Err)
		}
		specs = append(specs, exportSmartFunction(rule.Name(), exported.Data))
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

// exportSmartFunction turns a rule package into an inline rule. Defaults
// (the mqtt transport, payload formats which follow from the payload) are
// left out.
func exportSmartFunction(name string, pkg *dataprep.RulePackage) SmartFunctionSpec {
	cfg := pkg.Config
	spec := SmartFunctionSpec{
		Name:        name,
		Description: cfg.Description,
		Tags:        cfg.Tags,
		Disabled:    cfg.Disabled,
		Input: &SmartFunctionInputSpec{
			TopicPattern:    cfg.Input.TopicPattern,
			ClientIDPattern: cfg.Input.ClientIDPattern,
		},
		Code: string(pkg.SmartFunction),
	}
	if cfg.Input.Transport != dataprep.TransportMQTT {
		spec.Input.Transport = cfg.Input.Transport
	}
	if cfg.Input.TransportID != "" && cfg.Input.TransportID != cfg.Input.Transport {
		spec.Input.TransportID = cfg.Input.TransportID
	}

	for testName, tc := range pkg.Tests {
		if spec.Tests == nil {
			spec.Tests = map[string]SmartFunctionTestSpec{}
		}
		test := SmartFunctionTestSpec{ExpectedOutput: tc.ExpectedOutput}
		for _, in := range tc.Inputs {
			input := SmartFunctionTestInputSpec{
				Topic:         in.Topic,
				ClientID:      in.ClientID,
				Payload:       in.Payload,
				PayloadFormat: in.PayloadFormat,
				Time:          in.Time,
			}
			if _, inferred, err := testPayload(in.Payload, ""); err == nil && inferred == in.PayloadFormat {
				input.PayloadFormat = ""
			}
			test.Inputs = append(test.Inputs, input)
		}
		spec.Tests[testName] = test
	}
	return spec
}

func stringOf(value any) string {
	s, _ := value.(string)
	return s
}

func stringsOf(value any) []string {
	items, _ := value.([]any)
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// convertJSON converts a value into a typed value through JSON
func convertJSON(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
