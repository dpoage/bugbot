package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/domain"
)

// --- helpers ----------------------------------------------------------------

func runReportToolIssueTool(t *testing.T, tool *ReportToolIssueTool, args interface{}) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), raw)
}

func newTestReportToolIssueTool(reported *[]reportToolIssueCapture) *ReportToolIssueTool {
	onReport := func(tool string, sev domain.Severity, summary string) error {
		*reported = append(*reported, reportToolIssueCapture{tool, sev, summary})
		return nil
	}
	return NewReportToolIssueTool(onReport)
}

type reportToolIssueCapture struct {
	tool     string
	severity domain.Severity
	summary  string
}

// --- Tests ------------------------------------------------------------------

func TestReportToolIssueTool_Def(t *testing.T) {
	tool := NewReportToolIssueTool(func(_ string, _ domain.Severity, _ string) error { return nil })
	def := tool.Def()
	if def.Name != "report_tool_issue" {
		t.Errorf("name = %q, want report_tool_issue", def.Name)
	}
	if def.Description == "" {
		t.Error("description is empty")
	}
	// Schema must be valid JSON with additionalProperties:false.
	var schema map[string]interface{}
	if err := json.Unmarshal(def.Parameters, &schema); err != nil {
		t.Fatalf("parameters schema is not valid JSON: %v", err)
	}
	if ap, ok := schema["additionalProperties"]; !ok || ap != false {
		t.Error("additionalProperties must be false")
	}
	// All three fields are required.
	req, _ := schema["required"].([]interface{})
	reqSet := map[string]bool{}
	for _, r := range req {
		reqSet[r.(string)] = true
	}
	for _, field := range []string{"tool", "severity", "summary"} {
		if !reqSet[field] {
			t.Errorf("field %q not in required list", field)
		}
	}
	// severity must be an enum with the four canonical values.
	props, _ := schema["properties"].(map[string]interface{})
	sev, ok := props["severity"].(map[string]interface{})
	if !ok {
		t.Fatal("severity property missing")
	}
	enum, _ := sev["enum"].([]interface{})
	got := map[string]bool{}
	for _, v := range enum {
		got[v.(string)] = true
	}
	for _, want := range []string{"critical", "high", "medium", "low"} {
		if !got[want] {
			t.Errorf("severity enum missing %q", want)
		}
	}
}

// TestReportToolIssueTool_UnknownSeverity_ListsValidValues verifies that an
// invalid severity is rejected and the error enumerates the valid values, and
// that onReport is NOT called.
func TestReportToolIssueTool_UnknownSeverity_ListsValidValues(t *testing.T) {
	var captured []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&captured)

	_, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "sandbox_exec",
		"severity": "catastrophic",
		"summary":  "the sandbox is on fire",
	})
	if err == nil {
		t.Fatal("unknown severity must return an error")
	}
	for _, v := range []string{"critical", "high", "medium", "low"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error message missing valid severity %q: %v", v, err)
		}
	}
	if len(captured) != 0 {
		t.Errorf("onReport must not fire on invalid severity, fired %d times", len(captured))
	}
}

// TestReportToolIssueTool_ValidSeverity_Normalized verifies that the onReport
// callback receives a normalized lowercase Severity even when the model
// supplied it in mixed/upper case (ParseSeverity is case-insensitive).
func TestReportToolIssueTool_ValidSeverity_Normalized(t *testing.T) {
	var captured []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&captured)

	_, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "read_symbol",
		"severity": "HIGH",
		"summary":  "returns empty body for known symbols",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("want 1 capture, got %d", len(captured))
	}
	if captured[0].severity != domain.SeverityHigh {
		t.Errorf("severity = %q, want high", captured[0].severity)
	}
	if captured[0].tool != "read_symbol" {
		t.Errorf("tool = %q", captured[0].tool)
	}
	if captured[0].summary != "returns empty body for known symbols" {
		t.Errorf("summary = %q", captured[0].summary)
	}
}

func TestReportToolIssueTool_EmptySummary_IsError(t *testing.T) {
	var captured []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&captured)

	_, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "sandbox_exec",
		"severity": "low",
		"summary":  "",
	})
	if err == nil {
		t.Fatal("empty summary must return an error")
	}
	if !strings.Contains(err.Error(), "summary") {
		t.Errorf("error should mention summary: %v", err)
	}
	if len(captured) != 0 {
		t.Errorf("onReport must not fire on empty summary")
	}
}

func TestReportToolIssueTool_WhitespaceOnlySummary_IsError(t *testing.T) {
	var captured []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&captured)

	_, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "sandbox_exec",
		"severity": "low",
		"summary":  "   \t  ",
	})
	if err == nil {
		t.Fatal("whitespace-only summary must return an error")
	}
	if len(captured) != 0 {
		t.Errorf("onReport must not fire on whitespace-only summary")
	}
}

// TestReportToolIssueTool_OverlongSummary_Bounded verifies that an over-cap
// summary is truncated to the cap (with an ellipsis) rather than rejected,
// so the model still gets a meaningful truncated complaint in.
func TestReportToolIssueTool_OverlongSummary_Bounded(t *testing.T) {
	var captured []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&captured)

	overlong := strings.Repeat("x", maxToolIssueSummaryLen+500)
	out, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "grep",
		"severity": "medium",
		"summary":  overlong,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("want 1 capture, got %d", len(captured))
	}
	// Stored summary must be at most maxToolIssueSummaryLen bytes and end in
	// the ellipsis rune.
	s := captured[0].summary
	if len(s) != maxToolIssueSummaryLen {
		t.Errorf("stored summary length = %d, want %d", len(s), maxToolIssueSummaryLen)
	}
	if !strings.HasSuffix(s, "…") {
		t.Errorf("stored summary should end with ellipsis, got %q", s)
	}
	// Confirmation string should mention the tool name and the severity.
	if !strings.Contains(out, "grep") {
		t.Errorf("confirmation missing tool name: %q", out)
	}
	if !strings.Contains(out, "medium") {
		t.Errorf("confirmation missing severity: %q", out)
	}
}

func TestReportToolIssueTool_EmptyTool_IsError(t *testing.T) {
	var captured []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&captured)

	_, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "",
		"severity": "low",
		"summary":  "some complaint",
	})
	if err == nil {
		t.Fatal("empty tool must return an error")
	}
	if !strings.Contains(err.Error(), "tool") {
		t.Errorf("error should mention tool: %v", err)
	}
	if len(captured) != 0 {
		t.Errorf("onReport must not fire on empty tool")
	}
}

// TestReportToolIssueTool_OnReportError_SurfacedAsToolError verifies that an
// error from the onReport callback is wrapped and surfaced as the tool's
// return error.
func TestReportToolIssueTool_OnReportError_SurfacedAsToolError(t *testing.T) {
	sentinel := errors.New("store write failed")
	tool := NewReportToolIssueTool(func(_ string, _ domain.Severity, _ string) error { return sentinel })

	_, err := runReportToolIssueTool(t, tool, map[string]interface{}{
		"tool":     "sandbox_exec",
		"severity": "critical",
		"summary":  "container runtime missing",
	})
	if err == nil {
		t.Fatal("onReport error must surface as a tool error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error does not wrap sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "report tool issue") {
		t.Errorf("error should mention the operation: %v", err)
	}
}

func TestReportToolIssueTool_InvalidJSON_IsError(t *testing.T) {
	tool := NewReportToolIssueTool(func(_ string, _ domain.Severity, _ string) error { return nil })
	_, err := tool.Run(context.Background(), []byte(`not json`))
	if err == nil {
		t.Fatal("invalid JSON must return an error")
	}
}

// TestReportToolIssueTool_OverlongMultibyteSummary_StaysValidUTF8 verifies that
// truncating an overlong summary never splits a multibyte rune: the stored
// summary stays valid UTF-8 and within the byte cap.
func TestReportToolIssueTool_OverlongMultibyteSummary_StaysValidUTF8(t *testing.T) {
	var reported []reportToolIssueCapture
	tool := newTestReportToolIssueTool(&reported)
	// 400 three-byte runes = 1200 bytes, well over the 500-byte cap, whose rune
	// boundaries do not align with the byte cut point.
	long := strings.Repeat("☃", 400)
	if _, err := runReportToolIssueTool(t, tool, map[string]string{
		"tool": "codenav", "severity": "high", "summary": long,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(reported) != 1 {
		t.Fatalf("reported %d, want 1", len(reported))
	}
	got := reported[0].summary
	if !utf8.ValidString(got) {
		t.Errorf("truncated summary is not valid UTF-8: %q", got)
	}
	if len(got) > maxToolIssueSummaryLen {
		t.Errorf("summary len = %d bytes, want <= %d", len(got), maxToolIssueSummaryLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated summary should end with an ellipsis: %q", got)
	}
}
