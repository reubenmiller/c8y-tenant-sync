package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reportManifest = `tenantOptions:
  - category: configuration
    key: my.setting
    value: "on"
retentionRules:
  - dataType: MEASUREMENT
    maximumAge: 365
`

// runForReport runs the manifest in dry-run mode and returns the run error
func runForReport(t *testing.T, manifest string, args ...string) (error, string) {
	t.Helper()
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")
	t.Setenv(EnvGitHubStepSummary, "")
	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, append([]string{"run", "-f", manifest}, args...)...)
	})
	return runErr, stdout
}

func readReport(t *testing.T, path string) RunReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var report RunReport
	require.NoError(t, json.Unmarshal(raw, &report), string(raw))
	return report
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(raw)
}

func TestParseReportOutputs(t *testing.T) {
	outputs, err := parseReportOutputs([]string{"a.json", "b.md", "c.MARKDOWN", ReportGitHub}, "")
	require.NoError(t, err)
	assert.Equal(t, []reportOutput{
		{Path: "a.json", Format: ReportFormatJSON},
		{Path: "b.md", Format: ReportFormatMarkdown},
		{Path: "c.MARKDOWN", Format: ReportFormatMarkdown},
		{Path: ReportGitHub, Format: ReportFormatMarkdown, Append: true},
	}, outputs)

	// The format flag overrides the extension
	outputs, err = parseReportOutputs([]string{"report.txt", "a.md"}, "JSON")
	require.NoError(t, err)
	assert.Equal(t, ReportFormatJSON, outputs[0].Format)
	assert.Equal(t, ReportFormatJSON, outputs[1].Format)

	_, err = parseReportOutputs([]string{"report.txt"}, "")
	assert.ErrorContains(t, err, `cannot tell the format of report "report.txt"`)
	_, err = parseReportOutputs([]string{"a.json"}, "xml")
	assert.ErrorContains(t, err, `invalid --report-format "xml"`)
	_, err = parseReportOutputs([]string{ReportGitHub}, "json")
	assert.ErrorContains(t, err, "writes Markdown")
}

func TestRunReportJSONAndMarkdownInDryRun(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))
	jsonPath := filepath.Join(dir, "reports", "run.json")
	mdPath := filepath.Join(dir, "reports", "run.md")

	runErr, stdout := runForReport(t, manifest, "--report", jsonPath, "--report", mdPath)
	require.NoError(t, runErr, stdout)

	report := readReport(t, jsonPath)
	assert.Equal(t, version, report.ToolVersion)
	assert.Equal(t, manifest, report.Manifest)
	assert.True(t, report.DryRun)
	assert.False(t, report.StartTime.IsZero())
	assert.GreaterOrEqual(t, report.DurationSeconds, 0.0)
	assert.Empty(t, report.Error)
	assert.Equal(t, ReportSummary{
		Status:  ReportStatusSuccess,
		Total:   2,
		Changes: 2,
		Counts:  map[Action]int{ActionPlanned: 2},
	}, report.Summary)
	require.Len(t, report.Results, 2)
	assert.Equal(t, ReportResult{
		Section: SectionTenantOptions,
		Item:    "configuration/my.setting",
		Action:  ActionPlanned,
		Detail:  "set value",
	}, report.Results[0])
	assert.Equal(t, SectionRetentionRules, report.Results[1].Section)
	assert.Empty(t, report.Results[1].Tenant, "no tenant label for a single-tenant run")

	md := readText(t, mdPath)
	assert.Contains(t, md, "## Tenant sync report: success")
	assert.Contains(t, md, "| Mode | dry-run |")
	assert.Contains(t, md, "| planned | 2 |")
	assert.Contains(t, md, "| **total** | **2** |")
	assert.Contains(t, md, "| tenantOptions | configuration/my.setting | planned | set value |")
	assert.Contains(t, md, "| retentionRules | MEASUREMENT/\\*/\\*/\\* | planned | ensure maximumAge=365 |", "emphasis characters are escaped")
	assert.NotContains(t, md, "### Tenant", "no tenant headings for a single tenant")
}

func TestRunReportGroupsByTenant(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))
	jsonPath := filepath.Join(dir, "run.json")
	mdPath := filepath.Join(dir, "run.md")

	runErr, stdout := runForReport(t, manifest, "--target", "t100,t200", "--report", jsonPath, "--report", mdPath)
	require.NoError(t, runErr, stdout)

	report := readReport(t, jsonPath)
	require.Len(t, report.Results, 4)
	tenants := []string{}
	for _, result := range report.Results {
		tenants = append(tenants, result.Tenant)
		assert.False(t, strings.Contains(result.Item, ": "), "the tenant is not folded into the item: %q", result.Item)
	}
	assert.Equal(t, []string{"t100", "t100", "t200", "t200"}, tenants)
	assert.Contains(t, stdout, "[tenantOptions] t100: configuration/my.setting", "the console output keeps the label")

	md := readText(t, mdPath)
	first := strings.Index(md, "### Tenant `t100`")
	second := strings.Index(md, "### Tenant `t200`")
	require.GreaterOrEqual(t, first, 0, md)
	require.Greater(t, second, first, md)
	assert.Equal(t, 2, strings.Count(md[first:second], "| planned |"))
}

func TestReportMarkdownCollapsesUnchangedItems(t *testing.T) {
	results := []ChangeResult{
		{Section: SectionUsers, Item: "a@example.invalid", Action: ActionUnchanged},
		{Section: SectionUsers, Item: "b@example.invalid", Action: ActionCreated, Detail: "in groups: a|b"},
		{Section: SectionUsers, Item: "c@example.invalid", Action: ActionFailed, Detail: "create", Err: errors.New("line 1\nline 2")},
	}
	md := BuildRunReport(results, nil, "tenant.yaml", "https://example.invalid", time.Now(), time.Second, false, nil).Markdown()

	assert.Contains(t, md, "## Tenant sync report: failed")
	assert.Contains(t, md, "| Mode | apply |")
	assert.Contains(t, md, "| users | b@example.invalid | created | in groups: a\\|b |")
	assert.Contains(t, md, "| users | c@example.invalid | failed | create: line 1<br>line 2 |")
	details := strings.Index(md, "<details>")
	require.GreaterOrEqual(t, details, 0)
	assert.Contains(t, md, "<summary>1 unchanged item(s)</summary>")
	assert.Greater(t, strings.Index(md, "a@example.invalid"), details, "unchanged items are inside the details block")
	assert.Less(t, strings.Index(md, "b@example.invalid"), details)
}

func TestReportRedactsSecrets(t *testing.T) {
	redactor := &Redactor{}
	redactor.Add("Top-Secret-Value-1")
	results := []ChangeResult{{
		Section: SectionTenantOptions,
		Item:    "integration/api.secret",
		Action:  ActionFailed,
		Detail:  "value Top-Secret-Value-1",
		Err:     errors.New("rejected Top-Secret-Value-1"),
	}}
	report := BuildRunReport(results, redactor, "tenant.yaml", "", time.Now(), 0, false, errors.New("aborted with Top-Secret-Value-1"))

	raw, err := report.JSON()
	require.NoError(t, err)
	// Markdown escapes the mask as \*\*\*
	for content, mask := range map[string]string{string(raw): "***", report.Markdown(): `\*\*\*`} {
		assert.NotContains(t, content, "Top-Secret-Value-1")
		assert.Contains(t, content, "rejected "+mask)
		assert.Contains(t, content, "value "+mask)
		assert.Contains(t, content, "aborted with "+mask)
	}
}

func TestRunReportWrittenOnFailureWithoutSecrets(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("GET /tenant/currentTenant", http.StatusOK, map[string]any{"name": "t123"})
	f.JSON("POST /tenant/options", http.StatusUnprocessableEntity, map[string]any{
		"error":   "options/validationError",
		"message": "value Env-Secret-Value-42 is not allowed",
	})
	setupRunAgainst(t, f)
	t.Setenv("TS_REPORT_SECRET", "Env-Secret-Value-42")

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`tenantOptions:
  - category: integration
    key: api.secret
    valueFrom:
      env: TS_REPORT_SECRET
`), 0o644))
	jsonPath := filepath.Join(dir, "run.json")
	mdPath := filepath.Join(dir, "run.md")

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--report", jsonPath, "--report", mdPath)
	})
	require.ErrorIs(t, runErr, errExitFailures, stdout)

	report := readReport(t, jsonPath)
	assert.Equal(t, ReportStatusFailed, report.Summary.Status)
	assert.False(t, report.DryRun)
	assert.Equal(t, f.server.URL, report.Host)
	require.Len(t, report.Results, 1)
	assert.Equal(t, ActionFailed, report.Results[0].Action)
	assert.Contains(t, report.Results[0].Error, "***", "the error reaches the report, masked")

	for _, path := range []string{jsonPath, mdPath} {
		assert.NotContains(t, readText(t, path), "Env-Secret-Value-42", path)
	}
	assert.Contains(t, readText(t, mdPath), "## Tenant sync report: failed")
}

func TestRunReportFormatOverride(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))
	path := filepath.Join(dir, "report.txt")

	runErr, stdout := runForReport(t, manifest, "--report", path, "--report-format", "json")
	require.NoError(t, runErr, stdout)
	assert.Equal(t, 2, readReport(t, path).Summary.Total)

	runErr, stdout = runForReport(t, manifest, "--report", path, "--report-format", "markdown")
	require.NoError(t, runErr, stdout)
	assert.True(t, strings.HasPrefix(readText(t, path), "## Tenant sync report"), "the file is replaced")
}

func TestRunReportBadFormat(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))

	runErr, _ := runForReport(t, manifest, "--report", filepath.Join(dir, "a.json"), "--report-format", "xml")
	assert.ErrorContains(t, runErr, `invalid --report-format "xml"`)
	runErr, _ = runForReport(t, manifest, "--report", filepath.Join(dir, "a.txt"))
	assert.ErrorContains(t, runErr, "cannot tell the format")
	assert.NoFileExists(t, filepath.Join(dir, "a.json"))
}

func TestRunReportGitHubStepSummary(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))
	summary := filepath.Join(dir, "step-summary.md")
	require.NoError(t, os.WriteFile(summary, []byte("# Earlier step\n\n"), 0o644))

	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")
	t.Setenv(EnvGitHubStepSummary, summary)
	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--report", ReportGitHub)
	})
	require.NoError(t, runErr, stdout)

	content := readText(t, summary)
	assert.True(t, strings.HasPrefix(content, "# Earlier step\n\n## Tenant sync report: success"), "appended: %s", content)

	// Without GITHUB_STEP_SUMMARY the report is skipped
	runErr, stdout = runForReport(t, manifest, "--report", ReportGitHub)
	assert.NoError(t, runErr, stdout)
}

func TestRunReportWriteErrorDoesNotHideFailure(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))
	blocker := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	badPath := filepath.Join(blocker, "run.json") // parent is a file

	// A successful sync reports the write error
	runErr, _ := runForReport(t, manifest, "--report", badPath)
	assert.ErrorContains(t, runErr, "failed to write report")

	// A failed sync keeps its own error
	f := newFakeC8y(t)
	f.JSON("POST /tenant/options", http.StatusUnprocessableEntity, map[string]any{"error": "x/y", "message": "rejected"})
	setupRunAgainst(t, f)
	t.Setenv(EnvGitHubStepSummary, "")
	var failErr error
	captureStdio(t, func() {
		failErr = runRoot(t, "run", "-f", manifest, "--only", SectionTenantOptions, "--report", badPath)
	})
	assert.ErrorIs(t, failErr, errExitFailures)
}

func TestRunReportWrittenWhenAborted(t *testing.T) {
	f := newFakeC8y(t)
	setupRunAgainst(t, f)
	t.Setenv(EnvGitHubStepSummary, "")

	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`hooks:
  pre:
    - name: precheck
      run: exit 3
`+reportManifest), 0o644))
	jsonPath := filepath.Join(dir, "run.json")

	var runErr error
	stdout, _ := captureStdio(t, func() {
		runErr = runRoot(t, "run", "-f", manifest, "--report", jsonPath)
	})
	require.Error(t, runErr, stdout)

	report := readReport(t, jsonPath)
	assert.Equal(t, ReportStatusFailed, report.Summary.Status)
	assert.NotEmpty(t, report.Error)
	assert.Equal(t, 1, report.Summary.Counts[ActionFailed], "the failed hook is recorded")
}

func TestRunFailOnChange(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "tenant.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(reportManifest), 0o644))
	jsonPath := filepath.Join(dir, "run.json")

	runErr, _ := runForReport(t, manifest, "--fail-on-change", "--report", jsonPath)
	require.ErrorIs(t, runErr, errChangesDetected)
	assert.ErrorContains(t, runErr, "changes detected: 2 item(s) created, updated or planned")
	assert.Equal(t, 2, readReport(t, jsonPath).Summary.Changes, "the report is written too")

	// No changes: success
	empty := filepath.Join(dir, "empty.yaml")
	require.NoError(t, os.WriteFile(empty, []byte("tenantOptions: []\n"), 0o644))
	runErr, _ = runForReport(t, empty, "--fail-on-change")
	assert.NoError(t, runErr)

	// Unchanged items do not count
	f := newFakeC8y(t)
	f.JSON("GET /tenant/options/configuration/my.setting", http.StatusOK, map[string]any{"category": "configuration", "key": "my.setting", "value": "on"})
	setupRunAgainst(t, f)
	var applyErr error
	captureStdio(t, func() {
		applyErr = runRoot(t, "run", "-f", manifest, "--only", SectionTenantOptions, "--fail-on-change")
	})
	assert.NoError(t, applyErr)
}

// inGitHubActions simulates a GitHub Actions runner with an existing step
// summary and returns its path
func inGitHubActions(t *testing.T) string {
	t.Helper()
	summary := filepath.Join(t.TempDir(), "step-summary.md")
	require.NoError(t, os.WriteFile(summary, []byte("# Earlier step\n\n"), 0o644))
	t.Setenv("C8Y_SETTINGS_DEFAULTS_DRY", "true")
	t.Setenv(EnvGitHubActions, "true")
	t.Setenv(EnvGitHubStepSummary, summary)
	return summary
}

func runCaptured(t *testing.T, args ...string) (error, string, string) {
	t.Helper()
	var runErr error
	stdout, stderr := captureStdio(t, func() {
		runErr = runRoot(t, args...)
	})
	return runErr, stdout, stderr
}

func TestRunGitHubSummaryAutomatic(t *testing.T) {
	summary := inGitHubActions(t)
	manifest := writeManifest(t, reportManifest)

	runErr, stdout, _ := runCaptured(t, "run", "-f", manifest)
	require.NoError(t, runErr, stdout)
	content := readText(t, summary)
	assert.True(t, strings.HasPrefix(content, "# Earlier step\n\n## Tenant sync report: success"), "appended: %s", content)
	assert.Equal(t, 1, strings.Count(content, "## Tenant sync report"))
}

func TestRunGitHubSummaryOptOut(t *testing.T) {
	summary := inGitHubActions(t)
	manifest := writeManifest(t, reportManifest)

	runErr, stdout, _ := runCaptured(t, "run", "-f", manifest, "--github-summary=false")
	require.NoError(t, runErr, stdout)
	assert.Equal(t, "# Earlier step\n\n", readText(t, summary))

	// An explicit --report github still writes it
	runErr, stdout, _ = runCaptured(t, "run", "-f", manifest, "--github-summary=false", "--report", ReportGitHub)
	require.NoError(t, runErr, stdout)
	assert.Equal(t, 1, strings.Count(readText(t, summary), "## Tenant sync report"))
}

func TestRunGitHubSummaryNotInActions(t *testing.T) {
	summary := inGitHubActions(t)
	t.Setenv(EnvGitHubActions, "")
	manifest := writeManifest(t, reportManifest)

	runErr, stdout, _ := runCaptured(t, "run", "-f", manifest)
	require.NoError(t, runErr, stdout)
	assert.Equal(t, "# Earlier step\n\n", readText(t, summary), "nothing is written outside GitHub Actions")
}

func TestRunGitHubSummaryWrittenOnce(t *testing.T) {
	summary := inGitHubActions(t)
	manifest := writeManifest(t, reportManifest)

	runErr, stdout, _ := runCaptured(t, "run", "-f", manifest, "--report", ReportGitHub)
	require.NoError(t, runErr, stdout)
	assert.Equal(t, 1, strings.Count(readText(t, summary), "## Tenant sync report"))

	// Also when the summary file is given as a --report path
	runErr, stdout, _ = runCaptured(t, "run", "-f", manifest, "--report-format", "markdown", "--report", summary)
	require.NoError(t, runErr, stdout)
	assert.Equal(t, 1, strings.Count(readText(t, summary), "## Tenant sync report"))
}

func TestRunGitHubSummaryWriteFailureIsAWarning(t *testing.T) {
	inGitHubActions(t)
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	t.Setenv(EnvGitHubStepSummary, filepath.Join(blocker, "summary.md")) // parent is a file
	manifest := writeManifest(t, reportManifest)

	runErr, stdout, stderr := runCaptured(t, "run", "-f", manifest)
	assert.NoError(t, runErr, stdout)
	assert.Contains(t, stderr, "failed to write report")
	assert.Contains(t, stderr, "GitHub step summary, ignored")

	// An explicit request fails the run
	runErr, _, _ = runCaptured(t, "run", "-f", manifest, "--report", ReportGitHub)
	assert.ErrorContains(t, runErr, "failed to write report")
}

func TestRunGitHubSummaryOnFailure(t *testing.T) {
	f := newFakeC8y(t)
	f.JSON("POST /tenant/options", http.StatusUnprocessableEntity, map[string]any{"error": "x/y", "message": "rejected"})
	setupRunAgainst(t, f)
	summary := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv(EnvGitHubActions, "true")
	t.Setenv(EnvGitHubStepSummary, summary)
	manifest := writeManifest(t, reportManifest)

	runErr, _, _ := runCaptured(t, "run", "-f", manifest, "--only", SectionTenantOptions)
	assert.ErrorIs(t, runErr, errExitFailures)
	assert.Contains(t, readText(t, summary), "## Tenant sync report: failed")
}
