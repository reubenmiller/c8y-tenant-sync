package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/dataprep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSmartFunctionCode = "export function onMessage(msg, ctx) { return []; }\n"

// writeRulePackage writes a rule package folder dir/name
func writeRulePackage(t *testing.T, dir, name string) string {
	t.Helper()
	pkgDir := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, dataprep.ConfigFile), []byte(
		"smartFunctionFile: smartFunction.js\ndescription: test rule\ninput:\n  transport: mqtt\n  topicPattern: te/device/*\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "smartFunction.js"), []byte(testSmartFunctionCode), 0o644))
	return pkgDir
}

// writeRuleArchive writes the package of writeRulePackage as dir/name.tar.gz
func writeRuleArchive(t *testing.T, dir, name string) {
	t.Helper()
	pkg, err := dataprep.PackageFromDir(writeRulePackage(t, t.TempDir(), name))
	require.NoError(t, err)
	archive, err := pkg.Archive()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".tar.gz"), archive, 0o644))
}

func TestSmartFunctionPackages(t *testing.T) {
	packages := smartFunctionPackages([]ResolvedFile{
		{Path: "/rules/inlet/data-prep.yaml", Filename: "data-prep.yaml"},
		{Path: "/rules/outlet.tar.gz", Filename: "outlet.tar.gz"},
		{Path: "/rules/events.TGZ", Filename: "events.TGZ"},
	}, "")
	require.Len(t, packages, 3)

	assert.Equal(t, "inlet", packages[0].Name)
	assert.True(t, packages[0].IsDir)
	assert.Equal(t, "/rules/inlet", packages[0].Path)

	assert.Equal(t, "outlet", packages[1].Name)
	assert.False(t, packages[1].IsDir)
	assert.Equal(t, "events", packages[2].Name)

	// The manifest name wins
	packages = smartFunctionPackages([]ResolvedFile{{Path: "/rules/outlet.tar.gz", Filename: "outlet.tar.gz"}}, "renamed")
	assert.Equal(t, "renamed", packages[0].Name)
}

func TestSyncSmartFunctionsDryRunValidatesPackages(t *testing.T) {
	syncer, dir := newTestSyncer(t)
	syncer.DryRun = true
	writeRulePackage(t, dir, "inlet")
	writeRuleArchive(t, dir, "outlet")

	// An invalid package: MQTT wildcards are not supported
	broken := writeRulePackage(t, dir, "broken")
	require.NoError(t, os.WriteFile(filepath.Join(broken, dataprep.ConfigFile), []byte(
		"smartFunctionFile: smartFunction.js\ninput:\n  transport: mqtt\n  topicPattern: te/#\n"), 0o644))

	require.NoError(t, syncer.SyncSmartFunctions(context.Background(), []SmartFunctionSpec{{Source: Source{Path: "."}}}))

	byItem := map[string]ChangeResult{}
	for _, r := range syncer.Results {
		byItem[r.Item] = r
	}
	require.Len(t, byItem, 3, "results: %+v", syncer.Results)
	assert.Equal(t, ActionPlanned, byItem["inlet"].Action)
	assert.Equal(t, ActionPlanned, byItem["outlet"].Action)
	assert.Equal(t, ActionFailed, byItem["broken"].Action)
	assert.Contains(t, byItem["broken"].Err.Error(), "MQTT wildcards")
}

func TestSyncSmartFunctionsNameRequiresSinglePackage(t *testing.T) {
	syncer, dir := newTestSyncer(t)
	writeRulePackage(t, dir, "a")
	writeRulePackage(t, dir, "b")

	require.NoError(t, syncer.SyncSmartFunctions(context.Background(), []SmartFunctionSpec{
		{Name: "single", Source: Source{Path: "."}},
	}))
	result := resultFor(t, syncer, SectionSmartFunctions)
	assert.Equal(t, ActionFailed, result.Action)
	assert.Contains(t, result.Err.Error(), "'name' is set")
}

func TestSyncSmartFunctionsDeploysNewRule(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /service/dataprep/v1/rules/inlet/deployed", http.StatusNotFound, map[string]any{"error": "not found"})
	f.Handle("PUT /service/dataprep/v1/rules/inlet/deployed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	syncer := newFakeSyncer(t, f)
	pkgDir := writeRulePackage(t, t.TempDir(), "inlet")

	require.NoError(t, syncer.SyncSmartFunctions(context.Background(), []SmartFunctionSpec{
		{Source: Source{Path: pkgDir}, WaitTimeout: "0s"},
	}))

	result := resultFor(t, syncer, SectionSmartFunctions)
	assert.Equal(t, ActionCreated, result.Action, "err: %v", result.Err)
	assert.Equal(t, "inlet", result.Item)

	uploads := f.Requests(http.MethodPut, "/service/dataprep/v1/rules/inlet/deployed")
	require.Len(t, uploads, 1)
	pkg, err := dataprep.PackageFromArchive(bytesReader(uploads[0].RawBody))
	require.NoError(t, err)
	assert.Equal(t, testSmartFunctionCode, string(pkg.SmartFunction))
}

func TestSyncSmartFunctionsUnchangedRuleIsNotDeployed(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /service/dataprep/v1/rules/inlet/deployed", http.StatusOK, map[string]any{
		"config": map[string]any{
			"description": "test rule",
			"disabled":    false,
			"source":      map[string]any{"transport": "mqtt", "topicPattern": "te/device/*"},
		},
		"deploymentStatus": map[string]any{"deploymentInProgress": false, "error": ""},
		"jsCode":           testSmartFunctionCode,
		"tests":            map[string]any{},
	})
	syncer := newFakeSyncer(t, f)
	pkgDir := writeRulePackage(t, t.TempDir(), "inlet")

	require.NoError(t, syncer.SyncSmartFunctions(context.Background(), []SmartFunctionSpec{{Source: Source{Path: pkgDir}}}))

	result := resultFor(t, syncer, SectionSmartFunctions)
	assert.Equal(t, ActionUnchanged, result.Action, "err: %v", result.Err)
	assert.Empty(t, f.Requests(http.MethodPut, "/service/dataprep"))
}

func TestValidateSmartFunctions(t *testing.T) {
	errs := validateSmartFunctions([]SmartFunctionSpec{
		{Source: Source{Path: "./rules"}},
		{Source: Source{URL: "https://example.com/rule.tar.gz"}},
		{Source: Source{Path: "./rules"}, WaitTimeout: "soon"},
	})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "smartFunctions[2]: invalid waitTimeout")

	assert.Equal(t, DefaultSmartFunctionWaitTimeout, SmartFunctionSpec{}.waitTimeout())
	assert.Zero(t, SmartFunctionSpec{WaitTimeout: "0"}.waitTimeout())
}

func TestSyncSmartFunctionDryRunWithoutDownload(t *testing.T) {
	// Dry-run lists GitHub assets with the path they would be downloaded to
	syncer, dir := newTestSyncer(t)
	syncer.DryRun = true
	syncer.syncSmartFunction(context.Background(), SmartFunctionSpec{}, smartFunctionPackage{
		Name:   "tiny-edge-events",
		Origin: "tiny-edge-events.tar.gz",
		Path:   filepath.Join(dir, "not-downloaded", "tiny-edge-events.tar.gz"),
	})
	result := resultFor(t, syncer, SectionSmartFunctions)
	assert.Equal(t, ActionPlanned, result.Action, "err: %v", result.Err)
	assert.Contains(t, result.Detail, "not downloaded in dry-run")
}

func TestSyncSmartFunctionsFromURL(t *testing.T) {
	f := newFakeC8y(t)
	archiveDir := t.TempDir()
	writeRuleArchive(t, archiveDir, "inlet")
	archive, err := os.ReadFile(filepath.Join(archiveDir, "inlet.tar.gz"))
	require.NoError(t, err)
	f.Handle("GET /releases/download/v1/inlet.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	f.JSON("GET /service/dataprep/v1/rules/inlet/deployed", http.StatusNotFound, map[string]any{"error": "not found"})
	f.Handle("PUT /service/dataprep/v1/rules/inlet/deployed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	syncer := newFakeSyncer(t, f)

	require.NoError(t, syncer.SyncSmartFunctions(context.Background(), []SmartFunctionSpec{
		{Source: Source{URL: f.server.URL + "/releases/download/v1/inlet.tar.gz"}, WaitTimeout: "0s"},
	}))

	result := resultFor(t, syncer, SectionSmartFunctions)
	assert.Equal(t, "inlet", result.Item)
	assert.Equal(t, ActionCreated, result.Action, "err: %v", result.Err)
	assert.Len(t, f.Requests(http.MethodPut, "/service/dataprep/v1/rules/inlet/deployed"), 1)
}
