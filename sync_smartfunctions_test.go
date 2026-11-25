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
	"gopkg.in/yaml.v3"
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

// testInlineSmartFunction is an inline rule as declared in a manifest
func testInlineSmartFunction() SmartFunctionSpec {
	return SmartFunctionSpec{
		Name:        "inline-events",
		Description: "inline test rule",
		Tags:        []string{"test"},
		Input:       &SmartFunctionInputSpec{TopicPattern: "te/device/*///e/*"},
		Code:        testSmartFunctionCode,
		Tests: map[string]SmartFunctionTestSpec{
			"identify": {
				Inputs: []SmartFunctionTestInputSpec{{
					Topic:    "te/device/dev01///e/zephyr_Identify",
					ClientID: "dev01",
					// An object is sent as JSON
					Payload: map[string]any{"text": "Identify"},
					// Normalised to the format the server returns
					Time: "2026-10-06T10:15:00+02:00",
				}},
				ExpectedOutput: []map[string]any{{"cumulocityType": "event"}},
			},
		},
	}
}

func TestBuildInlineSmartFunction(t *testing.T) {
	rule, err := buildInlineSmartFunction(testInlineSmartFunction(), "")
	require.NoError(t, err)
	assert.Equal(t, testSmartFunctionCode, string(rule.SmartFunction))
	assert.Equal(t, "inline test rule", rule.Config.Description)
	assert.Equal(t, dataprep.TransportMQTT, rule.Config.Input.Transport)
	assert.Equal(t, "te/device/*///e/*", rule.Config.Input.TopicPattern)

	input := rule.Tests["identify"].Inputs[0]
	assert.Equal(t, `{"text":"Identify"}`, input.Payload)
	assert.Equal(t, dataprep.PayloadFormatJSON, input.PayloadFormat)
	assert.Equal(t, "2026-10-06T08:15:00.000Z", input.Time)

	// Code from a file, relative to the manifest directory
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "events.js"), []byte(testSmartFunctionCode), 0o644))
	spec := testInlineSmartFunction()
	spec.Code, spec.CodeFile = "", "events.js"
	rule, err = buildInlineSmartFunction(spec, dir)
	require.NoError(t, err)
	assert.Equal(t, testSmartFunctionCode, string(rule.SmartFunction))

	// The package format is validated
	spec = testInlineSmartFunction()
	spec.Input.TopicPattern = "te/#"
	_, err = buildInlineSmartFunction(spec, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MQTT wildcards")
}

func TestTestPayload(t *testing.T) {
	for _, tc := range []struct {
		value       any
		format      string
		wantPayload string
		wantFormat  string
	}{
		{`{"a":1}`, "", `{"a":1}`, dataprep.PayloadFormatJSON},
		{"hello", "", "hello", dataprep.PayloadFormatText},
		{map[string]any{"a": 1}, "", `{"a":1}`, dataprep.PayloadFormatJSON},
		{42, "", "42", dataprep.PayloadFormatJSON},
		{"aGVsbG8=", dataprep.PayloadFormatBase64, "aGVsbG8=", dataprep.PayloadFormatBase64},
		{nil, "", "", dataprep.PayloadFormatText},
	} {
		payload, format, err := testPayload(tc.value, tc.format)
		require.NoError(t, err)
		assert.Equal(t, tc.wantPayload, payload, "%v", tc.value)
		assert.Equal(t, tc.wantFormat, format, "%v", tc.value)
	}
}

func TestSyncInlineSmartFunctionDeploys(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /service/dataprep/v1/rules/inline-events/deployed", http.StatusNotFound, map[string]any{"error": "not found"})
	f.Handle("PUT /service/dataprep/v1/rules/inline-events/deployed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	syncer := newFakeSyncer(t, f)
	spec := testInlineSmartFunction()
	spec.WaitTimeout = "0s"

	require.NoError(t, syncer.SyncSmartFunctions(context.Background(), []SmartFunctionSpec{spec}))

	result := resultFor(t, syncer, SectionSmartFunctions)
	assert.Equal(t, "inline-events", result.Item)
	assert.Equal(t, ActionCreated, result.Action, "err: %v", result.Err)
	assert.Equal(t, "inline", result.Detail)

	uploads := f.Requests(http.MethodPut, "/service/dataprep/v1/rules/inline-events/deployed")
	require.Len(t, uploads, 1)
	pkg, err := dataprep.PackageFromArchive(bytesReader(uploads[0].RawBody))
	require.NoError(t, err)
	assert.Equal(t, []string{"identify"}, pkg.TestNames())
}

func TestValidateInlineSmartFunctions(t *testing.T) {
	errs := validateSmartFunctions([]SmartFunctionSpec{
		testInlineSmartFunction(),
		{},
		{Name: "both", Input: &SmartFunctionInputSpec{TopicPattern: "x/*"}, Code: "c", CodeFile: "f.js"},
		{Name: "no-time", Input: &SmartFunctionInputSpec{TopicPattern: "x/*"}, Code: "c", Tests: map[string]SmartFunctionTestSpec{
			"t": {Inputs: []SmartFunctionTestInputSpec{{Topic: "x/1", ClientID: "c"}}},
		}},
		{Source: Source{Path: "./rules"}, Code: "c"},
	})
	assert.Equal(t, []string{
		"smartFunctions[1]: input.topicPattern is required for an inline rule",
		"smartFunctions[1]: name is required for an inline rule (or set a source)",
		"smartFunctions[1]: one of code or codeFile is required for an inline rule (or set a source)",
		"smartFunctions[2]: code and codeFile are mutually exclusive",
		"smartFunctions[3].tests.t.inputs[0]: time is required",
		"smartFunctions[4]: description, tags, disabled, input, code, codeFile and tests only apply to inline rules (without a source)",
	}, errs)
}

func TestSmartFunctionExpectedOutputForms(t *testing.T) {
	want := SmartFunctionExpectedOutput{{"cumulocityType": "event", "payload": map[string]any{"text": "hi"}}}
	for name, doc := range map[string]string{
		"yaml list":   "expectedOutput:\n  - cumulocityType: event\n    payload: {text: hi}\n",
		"json flow":   "expectedOutput: [\n    {\"cumulocityType\": \"event\", \"payload\": {\"text\": \"hi\"}}\n  ]\n",
		"json string": "expectedOutput: |\n  [\n      {\"cumulocityType\": \"event\",\n  \"payload\": {\"text\": \"hi\"}}\n  ]\n",
		"quoted":      "expectedOutput: '[{\"cumulocityType\": \"event\", \"payload\": {\"text\": \"hi\"}}]'\n",
		"object":      "expectedOutput:\n  cumulocityType: event\n  payload: {text: hi}\n",
		"json object": "expectedOutput: '{\"cumulocityType\": \"event\", \"payload\": {\"text\": \"hi\"}}'\n",
	} {
		t.Run(name, func(t *testing.T) {
			var test SmartFunctionTestSpec
			require.NoError(t, yaml.Unmarshal([]byte(doc), &test))
			assert.Equal(t, want, test.ExpectedOutput)
		})
	}

	var test SmartFunctionTestSpec
	require.NoError(t, yaml.Unmarshal([]byte("expectedOutput: null\n"), &test))
	assert.Nil(t, test.ExpectedOutput)

	err := yaml.Unmarshal([]byte("inputs: []\nexpectedOutput: |\n  [{\"cumulocityType\": }]\n"), &test)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2: expectedOutput: invalid JSON")

	err = yaml.Unmarshal([]byte("expectedOutput: '[1, 2]'\n"), &test)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "item 0 is not an object")
}
