package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
)

// maxToolIssueSummaryLen bounds the summary field. Reports are stored and
// surfaced via progress events; longer prose defeats the side-channel purpose
// and pollutes downstream consumers.
const maxToolIssueSummaryLen = 500

// ReportToolIssueTool lets an agent flag a HARNESS tool that is broken,
// failing, or unreliable — distinct from post_lead (cross-lens suspicion) and
// from the candidate/output path (real security findings). It is a
// low-frequency side channel, gated behind Scan.ToolComplaints: do NOT use it
// to excuse a thin or empty result.
//
// The onReport callback is where the funnel injects the progress event
// (KindToolUnhealthy) and Stats aggregation, keeping the agent package free
// of progress imports. The error returned from onReport is surfaced as a
// tool error so the model can see the failure.
//
// Each instance is per-runner: a fresh tool holds the callback supplied by
// the funnel for that pipeline role.
type ReportToolIssueTool struct {
	onReport func(tool string, sev domain.Severity, summary string) error
}

// NewReportToolIssueTool builds a report_tool_issue tool instance. onReport
// is invoked once per valid invocation; the funnel supplies the
// implementation (progress event + stats increment).
func NewReportToolIssueTool(onReport func(tool string, sev domain.Severity, summary string) error) *ReportToolIssueTool {
	return &ReportToolIssueTool{onReport: onReport}
}

// reportToolIssueArgs is the JSON schema for the tool arguments.
type reportToolIssueArgs struct {
	Tool     string `json:"tool"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// Def implements agent.Tool.
func (t *ReportToolIssueTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "report_tool_issue",
		Description: "Flag a HARNESS tool that is broken, failing, or unreliable. Use this when a" +
			" tool you depend on is misbehaving — for example, a code-nav tool that never" +
			" returns results, or a sandbox that will not launch. It is NOT for reporting" +
			" bugs or findings about the target repository (use the normal candidate output" +
			" or post_lead). It is a low-frequency side channel: do NOT use it to excuse" +
			" a thin or empty result, nor to justify an inconclusive or abandoned verdict." +
			" tool is the name of the broken tool; severity is" +
			" critical/high/medium/low; summary is a short single-line description.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "tool": {
      "type": "string",
      "description": "The name of the broken tool (e.g. \"sandbox_exec\", \"read_symbol\")."
    },
    "severity": {
      "type": "string",
      "enum": ["critical", "high", "medium", "low"],
      "description": "Impact of the failure: critical blocks the run, high degrades it materially, medium is annoying, low is cosmetic."
    },
    "summary": {
      "type": "string",
      "description": "Short single-line description of the failure (max ~500 characters)."
    }
  },
  "required": ["tool", "severity", "summary"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It validates the arguments, normalizes severity
// via domain.ParseSeverity, then calls onReport. A validation error is
// returned as a tool error so the model can retry with corrected arguments.
func (t *ReportToolIssueTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args reportToolIssueArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}

	// tool must be non-empty and short — tool names are bounded identifiers.
	if args.Tool == "" {
		return "", fmt.Errorf("tool is required")
	}
	if len(args.Tool) > 64 {
		return "", fmt.Errorf("tool must be at most 64 bytes, got %d", len(args.Tool))
	}

	// severity must be one of the four domain.Severity values. ParseSeverity
	// is case-insensitive and trims whitespace; we report the canonical set in
	// the error so the model can self-correct.
	sev, ok := domain.ParseSeverity(args.Severity)
	if !ok {
		return "", fmt.Errorf("severity %q is invalid; valid values are: critical, high, medium, low", args.Severity)
	}

	// summary must be non-empty. Collapse internal whitespace to a single line
	// (mirroring tools_status_note.go) and bound the length; overlong input is
	// truncated to the cap with an ellipsis so the model still gets a
	// meaningful truncated summary rather than a hard error.
	summary := strings.Join(strings.Fields(args.Summary), " ")
	if summary == "" {
		return "", fmt.Errorf("summary must be non-empty")
	}
	if len(summary) > maxToolIssueSummaryLen {
		// Reserve 3 bytes for the ellipsis ("…"), then back off to a UTF-8 rune
		// boundary so a multibyte rune is never split (which would store invalid
		// UTF-8). The result stays within maxToolIssueSummaryLen bytes.
		cut := maxToolIssueSummaryLen - len("…")
		for cut > 0 && !utf8.RuneStart(summary[cut]) {
			cut--
		}
		summary = summary[:cut] + "…"
	}

	if err := t.onReport(args.Tool, sev, summary); err != nil {
		return "", fmt.Errorf("report tool issue: %w", err)
	}

	return fmt.Sprintf("reported unhealthy tool %q (%s): %s", args.Tool, sev, summary), nil
}
