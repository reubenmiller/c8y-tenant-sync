package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Report formats
const (
	ReportFormatJSON     = "json"
	ReportFormatMarkdown = "markdown"
)

// ReportFormats lists the values accepted by --report-format
var ReportFormats = []string{ReportFormatJSON, ReportFormatMarkdown}

// ReportGitHub is the --report value which appends the Markdown report to the
// GitHub Actions step summary (the file named by GITHUB_STEP_SUMMARY)
const ReportGitHub = "github"

// EnvGitHubStepSummary is set by GitHub Actions to the step summary file
const EnvGitHubStepSummary = "GITHUB_STEP_SUMMARY"

// Report status values
const (
	ReportStatusSuccess = "success"
	ReportStatusFailed  = "failed"
)

// RunReport is the machine-readable record of a run
type RunReport struct {
	ToolVersion     string         `json:"toolVersion"`
	Manifest        string         `json:"manifest"`
	Host            string         `json:"host,omitempty"`
	StartTime       time.Time      `json:"startTime"`
	DurationSeconds float64        `json:"durationSeconds"`
	DryRun          bool           `json:"dryRun"`
	Error           string         `json:"error,omitempty"`
	Summary         ReportSummary  `json:"summary"`
	Results         []ReportResult `json:"results"`
}

// ReportSummary aggregates the results of a run
type ReportSummary struct {
	// Status is success when no item failed and the run was not aborted
	Status string `json:"status"`
	Total  int    `json:"total"`
	// Changes counts the created, updated and planned items
	Changes int            `json:"changes"`
	Counts  map[Action]int `json:"counts"`
}

// ReportResult is a single item of a run
type ReportResult struct {
	Tenant  string `json:"tenant,omitempty"`
	Section string `json:"section"`
	Item    string `json:"item"`
	Action  Action `json:"action"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
}

// reportOutput is a resolved --report destination
type reportOutput struct {
	// Path is the file to write, or ReportGitHub
	Path   string
	Format string
	// Append adds to an existing file instead of replacing it (step summary)
	Append bool
	// Automatic marks the step summary added without --report: a failure to
	// write it is only a warning
	Automatic bool
}

// EnvGitHubActions is set to "true" by GitHub Actions
const EnvGitHubActions = "GITHUB_ACTIONS"

// withGitHubSummary adds the GitHub step summary to the outputs when the run
// is inside GitHub Actions (GITHUB_ACTIONS=true and GITHUB_STEP_SUMMARY set),
// unless the summary is disabled or already requested, so it is written once
func withGitHubSummary(outputs []reportOutput, enabled bool) []reportOutput {
	summary := os.Getenv(EnvGitHubStepSummary)
	if !enabled || os.Getenv(EnvGitHubActions) != "true" || summary == "" {
		return outputs
	}
	for _, output := range outputs {
		if output.Path == ReportGitHub || samePath(output.Path, summary) {
			return outputs
		}
	}
	return append(outputs, reportOutput{Path: ReportGitHub, Format: ReportFormatMarkdown, Append: true, Automatic: true})
}

// samePath reports whether two paths name the same file
func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

// isChange reports whether an action changes (or would change) the tenant
func isChange(action Action) bool {
	return action == ActionCreated || action == ActionUpdated || action == ActionDeleted || action == ActionPlanned
}

// countChanges returns the number of created, updated and planned results
func countChanges(results []ChangeResult) int {
	total := 0
	for _, result := range results {
		if isChange(result.Action) {
			total++
		}
	}
	return total
}

// parseReportOutputs validates the --report and --report-format flags. The
// format is taken from the file extension unless format is given.
func parseReportOutputs(paths []string, format string) ([]reportOutput, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "", ReportFormatJSON, ReportFormatMarkdown:
	case "md":
		format = ReportFormatMarkdown
	default:
		return nil, fmt.Errorf("invalid --report-format %q: use %s", format, strings.Join(ReportFormats, " or "))
	}

	var outputs []reportOutput
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("--report needs a file name")
		}
		if path == ReportGitHub {
			if format == ReportFormatJSON {
				return nil, fmt.Errorf("--report %s writes Markdown and cannot be combined with --report-format %s", ReportGitHub, format)
			}
			outputs = append(outputs, reportOutput{Path: ReportGitHub, Format: ReportFormatMarkdown, Append: true})
			continue
		}
		outputFormat := format
		if outputFormat == "" {
			switch strings.ToLower(filepath.Ext(path)) {
			case ".json":
				outputFormat = ReportFormatJSON
			case ".md", ".markdown":
				outputFormat = ReportFormatMarkdown
			default:
				return nil, fmt.Errorf("cannot tell the format of report %q from its extension: use .json or .md, or set --report-format", path)
			}
		}
		outputs = append(outputs, reportOutput{Path: path, Format: outputFormat})
	}
	return outputs, nil
}

// BuildRunReport creates the report of a run. All text passes through the
// redactor, so secrets never reach the report.
func BuildRunReport(results []ChangeResult, redactor *Redactor, manifest, host string, start time.Time, elapsed time.Duration, dryRun bool, runErr error) *RunReport {
	redact := func(text string) string {
		if redactor == nil {
			return text
		}
		return redactor.Redact(text)
	}

	report := &RunReport{
		ToolVersion:     version,
		Manifest:        redact(manifest),
		Host:            redact(host),
		StartTime:       start.UTC().Truncate(time.Millisecond),
		DurationSeconds: float64(elapsed.Milliseconds()) / 1000,
		DryRun:          dryRun,
		Summary:         ReportSummary{Counts: map[Action]int{}},
		Results:         make([]ReportResult, 0, len(results)),
	}
	if runErr != nil {
		report.Error = redact(runErr.Error())
	}

	failed := runErr != nil
	for _, result := range results {
		entry := ReportResult{
			Tenant:  redact(result.Tenant),
			Section: result.Section,
			Item:    redact(result.Item),
			Action:  result.Action,
			Detail:  redact(result.Detail),
		}
		if result.Err != nil {
			entry.Error = redact(result.Err.Error())
		}
		if result.Action == ActionFailed {
			failed = true
		}
		if isChange(result.Action) {
			report.Summary.Changes++
		}
		report.Summary.Counts[result.Action]++
		report.Summary.Total++
		report.Results = append(report.Results, entry)
	}
	report.Summary.Status = ReportStatusSuccess
	if failed {
		report.Summary.Status = ReportStatusFailed
	}
	return report
}

// JSON renders the report as indented JSON
func (r *RunReport) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Markdown renders the report as GitHub flavoured Markdown: the run details,
// a table of counts, and per tenant a table of the changed and failed items,
// with the unchanged items collapsed in a details block
func (r *RunReport) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Tenant sync report: %s\n\n", r.Summary.Status)

	mode := "apply"
	if r.DryRun {
		mode = "dry-run"
	}
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Manifest | %s |\n", mdCode(r.Manifest))
	if r.Host != "" {
		fmt.Fprintf(&b, "| Host | %s |\n", mdCode(r.Host))
	}
	fmt.Fprintf(&b, "| Mode | %s |\n", mode)
	fmt.Fprintf(&b, "| Started | %s |\n", r.StartTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "| Duration | %.1fs |\n", r.DurationSeconds)
	fmt.Fprintf(&b, "| Version | %s |\n", mdCell(r.ToolVersion))
	b.WriteString("\n")

	if r.Error != "" {
		fmt.Fprintf(&b, "> **Run aborted:** %s\n\n", mdCell(r.Error))
	}

	b.WriteString("| Action | Count |\n|---|---:|\n")
	actions := make([]string, 0, len(r.Summary.Counts))
	for action := range r.Summary.Counts {
		actions = append(actions, string(action))
	}
	sort.Strings(actions)
	for _, action := range actions {
		fmt.Fprintf(&b, "| %s | %d |\n", action, r.Summary.Counts[Action(action)])
	}
	fmt.Fprintf(&b, "| **total** | **%d** |\n\n", r.Summary.Total)

	// Group by tenant in the order the tenants were applied
	var tenants []string
	groups := map[string][]ReportResult{}
	for _, result := range r.Results {
		if _, seen := groups[result.Tenant]; !seen {
			tenants = append(tenants, result.Tenant)
		}
		groups[result.Tenant] = append(groups[result.Tenant], result)
	}
	grouped := len(tenants) > 1 || (len(tenants) == 1 && tenants[0] != "")

	for _, tenant := range tenants {
		if grouped {
			fmt.Fprintf(&b, "### Tenant %s\n\n", mdCode(tenant))
		}
		var changed, unchanged []ReportResult
		for _, result := range groups[tenant] {
			if result.Action == ActionUnchanged {
				unchanged = append(unchanged, result)
			} else {
				changed = append(changed, result)
			}
		}
		if len(changed) > 0 {
			writeMarkdownTable(&b, changed)
		} else {
			b.WriteString("No changes.\n\n")
		}
		if len(unchanged) > 0 {
			fmt.Fprintf(&b, "<details>\n<summary>%d unchanged item(s)</summary>\n\n", len(unchanged))
			writeMarkdownTable(&b, unchanged)
			b.WriteString("</details>\n\n")
		}
	}
	return b.String()
}

func writeMarkdownTable(b *strings.Builder, results []ReportResult) {
	b.WriteString("| Section | Item | Action | Detail |\n|---|---|---|---|\n")
	for _, result := range results {
		detail := result.Detail
		if result.Error != "" {
			if detail != "" {
				detail += ": "
			}
			detail += result.Error
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", result.Section, mdCell(result.Item), result.Action, mdCell(detail))
	}
	b.WriteString("\n")
}

// mdCell escapes text for a Markdown table cell
func mdCell(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")
	for _, special := range []string{"\\", "|", "*", "_", "`"} {
		text = strings.ReplaceAll(text, special, "\\"+special)
	}
	text = strings.ReplaceAll(text, "\r\n", "<br>")
	text = strings.ReplaceAll(text, "\n", "<br>")
	return text
}

// mdCode formats text as inline code in a table cell
func mdCode(text string) string {
	if text == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(strings.ReplaceAll(text, "`", "'"), "|", "\\|") + "`"
}

// WriteReports writes the report to every output and prints each failure. It
// returns the first error of an explicitly requested output; failures of the
// automatic GitHub step summary are only warnings.
func WriteReports(report *RunReport, outputs []reportOutput) error {
	var firstErr error
	for _, output := range outputs {
		err := writeReport(report, output)
		if err == nil {
			continue
		}
		if output.Automatic {
			fmt.Fprintf(os.Stderr, "\n⚠ %v (GitHub step summary, ignored)\n", err)
			continue
		}
		fmt.Fprintf(os.Stderr, "\n⚠ %v\n", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func writeReport(report *RunReport, output reportOutput) error {
	path := output.Path
	if path == ReportGitHub {
		path = os.Getenv(EnvGitHubStepSummary)
		if path == "" {
			fmt.Fprintf(os.Stderr, "ℹ %s is not set, skipping the GitHub step summary report\n", EnvGitHubStepSummary)
			return nil
		}
	}

	var content []byte
	switch output.Format {
	case ReportFormatJSON:
		var err error
		if content, err = report.JSON(); err != nil {
			return fmt.Errorf("failed to write report %s: %w", output.Path, err)
		}
	default:
		content = []byte(report.Markdown())
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to write report %s: %w", path, err)
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if output.Append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return fmt.Errorf("failed to write report %s: %w", path, err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("failed to write report %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to write report %s: %w", path, err)
	}
	return nil
}
