package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dataprep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDigitalTwinAndDataPrep registers the DTM and Data Preparation
// endpoints read by export, with values as returned by a real tenant
func fakeDigitalTwinAndDataPrep(f *fakeC8y) {
	editable := map[string]any{"deleteAllowed": true, "editAllowed": true, "editMetadataAllowed": true}
	library := map[string]any{"deleteAllowed": false, "editAllowed": false, "editMetadataAllowed": true}
	f.JSON("GET /service/dtm/definitions/properties", http.StatusOK, map[string]any{
		"definitions": []any{
			map[string]any{
				"identifier": "maxFlow", "contexts": []any{"asset"}, "tags": []any{"pump"},
				"jsonSchema": map[string]any{
					"$schema": "http://json-schema.org/draft-07/schema#", "additionalProperties": false,
					"title": "Maximum flow", "type": "number", "minimum": 0,
				},
				"creationTime": "2026-11-25T00:44:34.683Z", "lastUpdated": "2026-11-25T00:45:23.179Z",
				"c8y_AvailableActions": editable,
			},
			map[string]any{
				"identifier": "c8y_Position", "contexts": []any{"asset"}, "c8y_Origin": "library",
				"jsonSchema":           map[string]any{"title": "Location", "type": "object"},
				"c8y_AvailableActions": library,
			},
		},
		"statistics": stats(),
	})
	f.JSON("GET /service/dtm/definitions/assets", http.StatusOK, map[string]any{
		"definitions": []any{
			map[string]any{
				"identifier": "Pump",
				"icon":       map[string]any{"category": "devicesAndSensors", "name": "pump"},
				"jsonSchema": map[string]any{
					"$schema": "http://json-schema.org/draft-07/schema#", "additionalProperties": false,
					"definitions": map[string]any{"maxFlow": map[string]any{"type": "number"}},
					"properties":  map[string]any{"maxFlow": map[string]any{"$ref": "#/definitions/maxFlow"}},
					"title":       "Pump", "description": "A pump", "type": "object",
				},
				"composition": map[string]any{
					"additionalProperties": false, "additionalSubAssets": false,
					"allowedProperties": []any{map[string]any{"identifier": "maxFlow", "minOccurs": "1"}},
					"allowedSubAssets":  []any{},
				},
				"c8y_AvailableActions": editable,
			},
			map[string]any{
				"identifier": "c8y_GenericAsset", "c8y_Origin": "library",
				"jsonSchema": map[string]any{"title": "Generic Asset"}, "c8y_AvailableActions": library,
			},
		},
		"statistics": stats(),
	})
	f.JSON("GET /service/dataprep/v1/rules", http.StatusOK, map[string]any{
		"twin": map[string]any{"config": map[string]any{"description": "twin"}},
	})
	f.JSON("GET /service/dataprep/v1/rules/twin/deployed", http.StatusOK, map[string]any{
		"config": map[string]any{
			"description": "twin messages", "disabled": true, "tags": []any{"twin"},
			"source": map[string]any{"transport": "mqtt", "topicPattern": "te/device/*///twin/*"},
		},
		"deploymentStatus": map[string]any{"deploymentInProgress": false, "error": ""},
		"jsCode":           "export function onMessage(message, context) {\n  return [{ text: `${message.topic}` }];\n}\n",
		"tests": map[string]any{
			"remote-access": map[string]any{
				"inputs": []any{map[string]any{
					"clientID": "dev01", "payload": `{"sessions":[]}`, "payloadFormat": "json",
					"time": "2026-09-22T06:00:00.000Z", "topic": "te/device/dev01///twin/tedge_RemoteAccess",
				}},
				"expectedOutput": []any{map[string]any{
					"cumulocityType": "managedObject",
					"payload":        map[string]any{"remoteAccess": map[string]any{"sessions": []any{}, "policy": map[string]any{}}},
				}},
			},
			"text": map[string]any{
				"inputs": []any{map[string]any{
					"clientID": "dev01", "payload": `{"not": "a json payload"}`, "payloadFormat": "text",
					"time": "2026-09-22T06:00:00.000Z", "topic": "te/device/dev01///twin/x",
				}},
			},
		},
	})
}

func TestExportDigitalTwinAndSmartFunctions(t *testing.T) {
	f := fakeExportTenant(t)
	fakeDigitalTwinAndDataPrep(f)
	exporter := &Exporter{Client: f.Client(), Sections: []string{SectionDigitalTwin, SectionSmartFunctions}}

	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)

	// Definitions: library definitions are skipped, server generated and
	// bookkeeping fields are left out, custom fragments are kept
	require.NotNil(t, manifest.DigitalTwin)
	assert.Equal(t, []DigitalTwinDefinitionSpec{{
		Identifier: "maxFlow",
		Contexts:   []string{"asset"},
		Tags:       []string{"pump"},
		JSONSchema: map[string]any{"title": "Maximum flow", "type": "number", "minimum": float64(0)},
	}}, manifest.DigitalTwin.PropertyDefinitions)
	require.Len(t, manifest.DigitalTwin.AssetDefinitions, 1)
	pump := manifest.DigitalTwin.AssetDefinitions[0]
	assert.Equal(t, map[string]any{"title": "Pump", "description": "A pump"}, pump.JSONSchema)
	assert.Equal(t, map[string]any{"icon": map[string]any{"category": "devicesAndSensors", "name": "pump"}}, pump.Fragments)
	require.NotNil(t, pump.Composition)
	assert.Equal(t, []DigitalTwinAllowedItem{{Identifier: "maxFlow", MinOccurs: "1"}}, pump.Composition.AllowedProperties)

	// Smart functions: inline rules, defaults left out
	require.Len(t, manifest.SmartFunctions, 1)
	rule := manifest.SmartFunctions[0]
	assert.Equal(t, "twin", rule.Name)
	assert.True(t, rule.Disabled)
	assert.Equal(t, &SmartFunctionInputSpec{TopicPattern: "te/device/*///twin/*"}, rule.Input)
	assert.Contains(t, rule.Code, "${message.topic}")
	assert.Empty(t, rule.Tests["remote-access"].Inputs[0].PayloadFormat, "json is inferred from the payload")
	assert.Equal(t, dataprep.PayloadFormatText, rule.Tests["text"].Inputs[0].PayloadFormat, "text must stay explicit for a JSON payload")
}

func TestExportedDigitalTwinAndSmartFunctionsRoundTrip(t *testing.T) {
	f := fakeExportTenant(t)
	fakeDigitalTwinAndDataPrep(f)
	exporter := &Exporter{Client: f.Client(), Sections: []string{SectionDigitalTwin, SectionSmartFunctions}}
	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)

	content, err := MarshalManifest(manifest, "test")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "exported.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	loaded, err := LoadManifest(path)
	require.NoError(t, err, "%s", content)
	// YAML numbers load as int, JSON numbers as float: compare as JSON
	assert.JSONEq(t, mustMarshalJSON(t, manifest.DigitalTwin), mustMarshalJSON(t, loaded.DigitalTwin))

	// The code survives the $ escaping, and empty values in the expected
	// output are kept (they are compared with the deployed rule)
	rule := loaded.SmartFunctions[0]
	assert.Equal(t, manifest.SmartFunctions[0].Code, rule.Code)
	assert.Equal(t, map[string]any{"sessions": []any{}, "policy": map[string]any{}},
		rule.Tests["remote-access"].ExpectedOutput[0]["payload"].(map[string]any)["remoteAccess"])

	// The loaded rule builds the same package as the deployed one
	pkg, err := buildInlineSmartFunction(rule, "")
	require.NoError(t, err)
	deployed := f.Client().DataPreparation.Rules.Get(context.Background(), "twin")
	require.NoError(t, deployed.Err)
	assert.Empty(t, dataprep.Diff(pkg, deployed.Data))
}

func TestExportSkipsUnavailableServices(t *testing.T) {
	// The fake tenant has neither the DTM nor Data Preparation
	f := fakeExportTenant(t)
	exporter := &Exporter{Client: f.Client(), Sections: []string{SectionDigitalTwin, SectionSmartFunctions}}

	manifest, _, err := exporter.Export(context.Background())
	require.NoError(t, err)
	assert.Nil(t, manifest.DigitalTwin)
	assert.Empty(t, manifest.SmartFunctions)
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}
