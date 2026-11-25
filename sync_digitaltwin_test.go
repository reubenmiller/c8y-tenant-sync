package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dtm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// testPropertyDefinitionFile is a plain definition, as posted to the DTM API
const testPropertyDefinitionFile = `{
	"identifier": "maxFlow",
	"contexts": ["asset"],
	"jsonSchema": {"title": "Maximum flow", "type": "number"}
}`

const testAssetDefinitionFile = `identifier: Pump
jsonSchema: {title: Pump}
composition:
  allowedProperties: [{identifier: maxFlow}]
`

// writeDefinitionFiles writes the test definitions to dir/properties and
// dir/assets
func writeDefinitionFiles(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "properties"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "properties", "maxFlow.json"), []byte(testPropertyDefinitionFile), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets", "pump.yaml"), []byte(testAssetDefinitionFile), 0o644))
}

func TestSyncDigitalTwinDryRun(t *testing.T) {
	syncer, dir := newTestSyncer(t)
	syncer.DryRun = true
	writeDefinitionFiles(t, dir)
	prune := true

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		ManagedBy: "tenant-sync",
		Prune:     &prune,
		PropertyDefinitions: []DigitalTwinDefinitionSpec{
			{Source: &Source{Path: "properties"}},
			{Identifier: "serial", Contexts: []string{"asset", "event"}, JSONSchema: map[string]any{"title": "Serial", "type": "string"}},
		},
		AssetDefinitions: []DigitalTwinDefinitionSpec{
			{Source: &Source{Path: "assets"}},
		},
	}))

	var items []string
	for _, r := range syncer.Results {
		assert.Equal(t, ActionPlanned, r.Action, "%+v", r)
		items = append(items, r.Item)
	}
	assert.Equal(t, []string{
		"PropertyDefinition maxFlow[asset]",
		"PropertyDefinition serial[asset,event]",
		"AssetDefinition Pump",
		"prune",
	}, items)
}

func TestSyncDigitalTwinRejectsWrongKindAndDuplicates(t *testing.T) {
	// A file under propertyDefinitions holding an asset definition
	syncer, dir := newTestSyncer(t)
	syncer.DryRun = true
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pump.yaml"), []byte("kind: AssetDefinition\n"+testAssetDefinitionFile), 0o644))

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		PropertyDefinitions: []DigitalTwinDefinitionSpec{{Source: &Source{Path: "pump.yaml"}}},
	}))
	result := resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "is a AssetDefinition, but the file is listed as PropertyDefinition")

	// The same definition inline and in a file
	syncer, dir = newTestSyncer(t)
	syncer.DryRun = true
	writeDefinitionFiles(t, dir)
	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		AssetDefinitions: []DigitalTwinDefinitionSpec{
			{Source: &Source{Path: "assets"}},
			{Identifier: "Pump"},
		},
	}))
	result = resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "AssetDefinition Pump")
}

func TestLoadDigitalTwinFile(t *testing.T) {
	dir := t.TempDir()

	// Several definitions in one file, with and without a matching kind
	multi := filepath.Join(dir, "multi.yaml")
	require.NoError(t, os.WriteFile(multi, []byte("identifier: a\n---\nkind: PropertyDefinition\nidentifier: b\n---\n- identifier: c\n- identifier: d\n"), 0o644))
	model := &dtm.Model{}
	require.NoError(t, loadDigitalTwinFile(model, multi, dtm.KindPropertyDefinition))
	require.Len(t, model.PropertyDefinitions, 4)
	assert.Equal(t, "d", model.PropertyDefinitions[3].Identifier)

	missing := filepath.Join(dir, "missing.json")
	require.NoError(t, os.WriteFile(missing, []byte(`{"jsonSchema": {}}`), 0o644))
	err := loadDigitalTwinFile(&dtm.Model{}, missing, dtm.KindPropertyDefinition)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identifier is required")
}

func TestSyncDigitalTwinApply(t *testing.T) {
	f := newFakeC8y(t)
	// maxFlow does not exist yet
	f.JSON("GET /service/dtm/definitions/properties", http.StatusOK, map[string]any{"definitions": []any{}})
	f.Handle("POST /service/dtm/definitions/properties", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})
	// Pump exists and already matches
	f.JSON("GET /service/dtm/definitions/assets/Pump", http.StatusOK, map[string]any{
		"identifier":           "Pump",
		"jsonSchema":           map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "title": "Pump", "type": "object"},
		"composition":          map[string]any{"allowedProperties": []any{map[string]any{"identifier": "maxFlow", "minOccurs": "0", "maxOccurs": "1"}}},
		"c8y_AvailableActions": map[string]any{"editAllowed": true, "deleteAllowed": true},
	})
	syncer := newFakeSyncer(t, f)
	writeDefinitionFiles(t, syncer.Resolver.BaseDir)

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		// Asset definitions are applied after the property definitions,
		// whatever the order in the manifest
		AssetDefinitions:    []DigitalTwinDefinitionSpec{{Source: &Source{Path: "assets"}}},
		PropertyDefinitions: []DigitalTwinDefinitionSpec{{Source: &Source{Path: "properties"}}},
	}))

	require.Len(t, syncer.Results, 2, "results: %+v", syncer.Results)
	assert.Equal(t, "PropertyDefinition maxFlow[asset]", syncer.Results[0].Item)
	assert.Equal(t, ActionCreated, syncer.Results[0].Action, "err: %v", syncer.Results[0].Err)
	assert.Equal(t, "AssetDefinition Pump", syncer.Results[1].Item)
	assert.Equal(t, ActionUnchanged, syncer.Results[1].Action, "err: %v", syncer.Results[1].Err)

	creates := f.Requests(http.MethodPost, "/service/dtm/definitions/properties")
	require.Len(t, creates, 1)
	assert.Equal(t, "maxFlow", creates[0].Body["identifier"])
	assert.Empty(t, f.Requests(http.MethodPut, "/service/dtm"))
}

func TestSyncDigitalTwinFromURL(t *testing.T) {
	f := newFakeC8y(t)
	// The definition file is served by the same test server, like a raw
	// GitHub URL
	f.Handle("GET /raw/main/cumulocity/dtm/tiny_edge_remote_access.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"identifier": "tiny_edge_remote_access", "contexts": ["asset"], "jsonSchema": {"title": "remote access", "type": "object"}}`))
	})
	f.JSON("GET /service/dtm/definitions/properties", http.StatusOK, map[string]any{"definitions": []any{}})
	f.Handle("POST /service/dtm/definitions/properties", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})
	syncer := newFakeSyncer(t, f)

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		PropertyDefinitions: []DigitalTwinDefinitionSpec{
			{Source: &Source{URL: f.server.URL + "/raw/main/cumulocity/dtm/tiny_edge_remote_access.json?token=x"}},
		},
	}))

	result := resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, "PropertyDefinition tiny_edge_remote_access[asset]", result.Item)
	assert.Equal(t, ActionCreated, result.Action, "err: %v", result.Err)
}

func TestSyncDigitalTwinDownloadFailureSkipsApply(t *testing.T) {
	f := newFakeC8y(t)
	syncer := newFakeSyncer(t, f)
	prune := true

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		ManagedBy:           "tenant-sync",
		Prune:               &prune,
		PropertyDefinitions: []DigitalTwinDefinitionSpec{{Source: &Source{URL: f.server.URL + "/missing.json"}}},
	}))

	result := resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "failed to download")
	// Nothing is applied (and in particular nothing pruned) without the file
	assert.Empty(t, f.Requests(http.MethodGet, "/service/dtm"))
	assert.Empty(t, f.Requests(http.MethodDelete, "/service/dtm"))
}

func TestDigitalTwinValidate(t *testing.T) {
	prune := true
	errs := (&DigitalTwinSpec{
		Prune: &prune,
		PropertyDefinitions: []DigitalTwinDefinitionSpec{
			{Identifier: "a"},
			{Identifier: "a"},
			{Identifier: "a", Contexts: []string{"asset"}},
			{Source: &Source{Path: "./dtm"}, Identifier: "b"},
			{Source: &Source{Cloudsmith: &CloudsmithSource{Repo: "thinedge/tedge-release"}}},
			{Tags: []string{"no-identifier"}},
		},
		AssetDefinitions: []DigitalTwinDefinitionSpec{
			{Identifier: "Pump", Contexts: []string{"asset"}},
			{Identifier: "Valve", Composition: &DigitalTwinCompositionSpec{AllowedProperties: []DigitalTwinAllowedItem{{}}}},
		},
	}).validate()
	assert.Equal(t, []string{
		"digitalTwin: prune requires managedBy (the tag marking the definitions the manifest owns)",
		"digitalTwin.propertyDefinitions[1]: duplicate definition a",
		"digitalTwin.propertyDefinitions[3]: source and inline definition fields (identifier, jsonSchema, ...) are mutually exclusive",
		"digitalTwin.propertyDefinitions[4].source: cloudsmith sources are not supported (use path, url or github)",
		"digitalTwin.propertyDefinitions[5]: identifier (or source) is required",
		"digitalTwin.assetDefinitions[0]: contexts only apply to property definitions",
		"digitalTwin.assetDefinitions[1].composition: item 0: identifier is required",
	}, errs)
}

func TestMergeDigitalTwin(t *testing.T) {
	prune := true
	base := &DigitalTwinSpec{
		ManagedBy: "base",
		PropertyDefinitions: []DigitalTwinDefinitionSpec{
			{Source: &Source{Path: "a"}},
			{Identifier: "maxFlow", Tags: []string{"old"}},
		},
	}
	over := &DigitalTwinSpec{
		Prune: &prune,
		PropertyDefinitions: []DigitalTwinDefinitionSpec{
			{Source: &Source{Path: "b"}},
			{Identifier: "maxFlow", Tags: []string{"new"}},
			{Identifier: "minFlow"},
		},
	}
	merged := mergeDigitalTwin(base, over)
	assert.Equal(t, "base", merged.ManagedBy)
	assert.True(t, merged.PruneEnabled())
	// Both source entries are kept, maxFlow is replaced in place
	require.Len(t, merged.PropertyDefinitions, 4)
	assert.Equal(t, "a", merged.PropertyDefinitions[0].Source.Path)
	assert.Equal(t, []string{"new"}, merged.PropertyDefinitions[1].Tags)
	assert.Equal(t, "b", merged.PropertyDefinitions[2].Source.Path)

	assert.Same(t, over, mergeDigitalTwin(nil, over))
	assert.Same(t, base, mergeDigitalTwin(base, nil))
}

func TestManifestParsesSmartFunctionsAndDigitalTwin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
smartFunctions:
  - source: {path: ./rules}
    waitTimeout: 2m
digitalTwin:
  managedBy: tenant-sync
  propertyDefinitions:
    - source:
        url: https://raw.githubusercontent.com/thin-edge/tiny-edge/refs/heads/main/cumulocity/dtm/tiny_edge.json
    - source: {path: ./dtm/properties}
    - identifier: maxFlow
      contexts: [asset]
      jsonSchema: {title: Maximum flow, type: number, minimum: 0}
  assetDefinitions:
    - identifier: Pump
      jsonSchema: {title: Pump}
      composition:
        allowedProperties:
          - identifier: maxFlow
            minOccurs: "1"
      fragments:
        icon: {category: devicesAndSensors, name: pump}
`), 0o644))

	manifest, err := LoadManifest(path)
	require.NoError(t, err)
	require.Len(t, manifest.SmartFunctions, 1)
	assert.Equal(t, "2m", manifest.SmartFunctions[0].WaitTimeout)
	require.NotNil(t, manifest.DigitalTwin)
	require.Len(t, manifest.DigitalTwin.PropertyDefinitions, 3)
	assert.Contains(t, manifest.DigitalTwin.PropertyDefinitions[0].Source.URL, "tiny_edge.json")
	assert.Equal(t, "./dtm/properties", manifest.DigitalTwin.PropertyDefinitions[1].Source.Path)
	require.Len(t, manifest.DigitalTwin.AssetDefinitions, 1)

	def := manifest.DigitalTwin.AssetDefinitions[0].toDefinition()
	assert.Equal(t, "1", def.Composition.AllowedProperties[0].MinOccurs)
	assert.Contains(t, def.Extra, "icon")
}
