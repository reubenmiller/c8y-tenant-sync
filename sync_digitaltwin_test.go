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

const testDigitalTwinModelFile = `kind: PropertyDefinition
identifier: maxFlow
contexts: [asset]
jsonSchema: {title: Maximum flow, type: number}
---
kind: AssetDefinition
identifier: Pump
jsonSchema: {title: Pump}
composition:
  allowedProperties: [{identifier: maxFlow}]
`

func TestSyncDigitalTwinDryRun(t *testing.T) {
	syncer, dir := newTestSyncer(t)
	syncer.DryRun = true
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pump.yaml"), []byte(testDigitalTwinModelFile), 0o644))
	prune := true

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		ManagedBy: "tenant-sync",
		Prune:     &prune,
		Sources:   []DigitalTwinSourceSpec{{Source: Source{Path: "."}}},
		PropertyDefinitions: []DigitalTwinDefinitionSpec{
			{Identifier: "serial", Contexts: []string{"asset", "event"}, JSONSchema: map[string]any{"title": "Serial", "type": "string"}},
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

func TestSyncDigitalTwinRejectsAssetsAndDuplicates(t *testing.T) {
	syncer, dir := newTestSyncer(t)
	syncer.DryRun = true
	require.NoError(t, os.WriteFile(filepath.Join(dir, "assets.yaml"), []byte("kind: Asset\nexternalId: pump-07\nasset: {name: Pump 07}\n"), 0o644))

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{Sources: []DigitalTwinSourceSpec{{Source: Source{Path: "assets.yaml"}}}}))
	result := resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "only definitions are supported")

	// The same definition inline and in a model file
	syncer, dir = newTestSyncer(t)
	syncer.DryRun = true
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pump.yaml"), []byte(testDigitalTwinModelFile), 0o644))
	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		Sources:          []DigitalTwinSourceSpec{{Source: Source{Path: "pump.yaml"}}},
		AssetDefinitions: []DigitalTwinDefinitionSpec{{Identifier: "Pump"}},
	}))
	result = resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "AssetDefinition Pump")
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
	require.NoError(t, os.WriteFile(filepath.Join(syncer.Resolver.BaseDir, "pump.yaml"), []byte(testDigitalTwinModelFile), 0o644))

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{Sources: []DigitalTwinSourceSpec{{Source: Source{Path: "pump.yaml"}}}}))

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

func TestDigitalTwinValidate(t *testing.T) {
	prune := true
	errs := (&DigitalTwinSpec{
		Prune:               &prune,
		Sources:             []DigitalTwinSourceSpec{{Source: Source{URL: "https://example.com/model.yaml"}}},
		PropertyDefinitions: []DigitalTwinDefinitionSpec{{Identifier: "a"}, {Identifier: "a"}, {Identifier: "a", Contexts: []string{"asset"}}},
		AssetDefinitions: []DigitalTwinDefinitionSpec{
			{Identifier: "Pump", Contexts: []string{"asset"}},
			{Identifier: "Valve", Composition: &DigitalTwinCompositionSpec{AllowedProperties: []DigitalTwinAllowedItem{{}}}},
		},
	}).validate()
	assert.Equal(t, []string{
		"digitalTwin: prune requires managedBy (the tag marking the definitions the manifest owns)",
		"digitalTwin.propertyDefinitions[1]: duplicate property definition a",
		"digitalTwin.assetDefinitions[0]: contexts only apply to property definitions",
		"digitalTwin.assetDefinitions[1].composition: item 0: identifier is required",
	}, errs)
}

func TestMergeDigitalTwin(t *testing.T) {
	prune := true
	base := &DigitalTwinSpec{
		ManagedBy:           "base",
		Sources:             []DigitalTwinSourceSpec{{Source: Source{Path: "a"}}},
		PropertyDefinitions: []DigitalTwinDefinitionSpec{{Identifier: "maxFlow", Tags: []string{"old"}}},
	}
	over := &DigitalTwinSpec{
		Prune:               &prune,
		Sources:             []DigitalTwinSourceSpec{{Source: Source{Path: "b"}}},
		PropertyDefinitions: []DigitalTwinDefinitionSpec{{Identifier: "maxFlow", Tags: []string{"new"}}, {Identifier: "minFlow"}},
	}
	merged := mergeDigitalTwin(base, over)
	assert.Equal(t, "base", merged.ManagedBy)
	assert.True(t, merged.PruneEnabled())
	assert.Len(t, merged.Sources, 2)
	require.Len(t, merged.PropertyDefinitions, 2)
	assert.Equal(t, []string{"new"}, merged.PropertyDefinitions[0].Tags)

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
  sources:
    - path: ./dtm
  propertyDefinitions:
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
	require.Len(t, manifest.DigitalTwin.AssetDefinitions, 1)

	def := manifest.DigitalTwin.AssetDefinitions[0].toDefinition()
	assert.Equal(t, "1", def.Composition.AllowedProperties[0].MinOccurs)
	assert.Contains(t, def.Extra, "icon")
}

func TestLoadDigitalTwinFileWithSourceKind(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "tiny_edge.json")
	require.NoError(t, os.WriteFile(plain, []byte(`{
		"identifier": "tiny_edge",
		"jsonSchema": {"type": "object", "title": "tiny-edge client"},
		"contexts": ["asset", "event", "operation"],
		"tags": ["tiny-edge"]
	}`), 0o644))

	model := &dtm.Model{}
	err := loadDigitalTwinFile(model, plain, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set kind on the source")

	require.NoError(t, loadDigitalTwinFile(model, plain, dtm.KindPropertyDefinition))
	require.Len(t, model.PropertyDefinitions, 1)
	assert.Equal(t, "tiny_edge", model.PropertyDefinitions[0].Identifier)

	// A kind in the document wins over the source kind
	model = &dtm.Model{}
	mixed := filepath.Join(dir, "mixed.yaml")
	require.NoError(t, os.WriteFile(mixed, []byte(testDigitalTwinModelFile), 0o644))
	require.NoError(t, loadDigitalTwinFile(model, mixed, dtm.KindPropertyDefinition))
	assert.Len(t, model.PropertyDefinitions, 1)
	assert.Len(t, model.AssetDefinitions, 1)

	errs := (&DigitalTwinSpec{Sources: []DigitalTwinSourceSpec{{Source: Source{Path: "."}, Kind: "Property"}}}).validate()
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], `unknown kind "Property"`)
}

func TestSyncDigitalTwinFromURL(t *testing.T) {
	f := newFakeC8y(t)
	// The definition file is served by the same test server, like a raw
	// GitHub URL with a plain definition (no kind field)
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
		Sources: []DigitalTwinSourceSpec{{
			Source: Source{URL: f.server.URL + "/raw/main/cumulocity/dtm/tiny_edge_remote_access.json?token=x"},
			Kind:   dtm.KindPropertyDefinition,
		}},
	}))

	result := resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, "PropertyDefinition tiny_edge_remote_access[asset]", result.Item)
	assert.Equal(t, ActionCreated, result.Action, "err: %v", result.Err)
}

func TestSyncDigitalTwinFromURLDownloadFailureSkipsApply(t *testing.T) {
	f := newFakeC8y(t)
	syncer := newFakeSyncer(t, f)
	prune := true

	require.NoError(t, syncer.SyncDigitalTwin(context.Background(), &DigitalTwinSpec{
		ManagedBy: "tenant-sync",
		Prune:     &prune,
		Sources:   []DigitalTwinSourceSpec{{Source: Source{URL: f.server.URL + "/missing.json"}, Kind: dtm.KindPropertyDefinition}},
	}))

	result := resultFor(t, syncer, SectionDigitalTwin)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "failed to download")
	// Nothing is applied (and in particular nothing pruned) without the file
	assert.Empty(t, f.Requests(http.MethodGet, "/service/dtm"))
	assert.Empty(t, f.Requests(http.MethodDelete, "/service/dtm"))
}
