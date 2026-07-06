package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/funnel"
)

// TestPrintResult_ReliableNoFindings confirms a clean run (finders ran, none
// failed, nothing found) prints the plain "No findings." and NO reliability
// warning.
func TestPrintResult_ReliableNoFindings(t *testing.T) {
	var buf bytes.Buffer
	res := &funnel.Result{
		Commit: "abc123",
		Stats:  funnel.Stats{FinderRuns: 6, FinderFailures: 0},
	}
	printResult(&buf, res)
	out := buf.String()

	if !strings.Contains(out, "No findings.") {
		t.Errorf("reliable empty run should print 'No findings.':\n%s", out)
	}
	if strings.Contains(out, "RELIABILITY WARNING") {
		t.Errorf("reliable run must NOT print a reliability warning:\n%s", out)
	}
}

// TestPrintResult_FinderFailureNeverBareNoFindings is the trust-bug regression:
// when a finder failed to parse, an empty result must NOT read as "No findings."
// and a prominent warning must appear.
func TestPrintResult_FinderFailureNeverBareNoFindings(t *testing.T) {
	var buf bytes.Buffer
	res := &funnel.Result{
		Commit: "abc123",
		Stats:  funnel.Stats{FinderRuns: 6, FinderFailures: 2},
	}
	printResult(&buf, res)
	out := buf.String()

	if strings.Contains(out, "\nNo findings.\n") {
		t.Errorf("a scan with finder failures must NOT print a bare 'No findings.':\n%s", out)
	}
	if !strings.Contains(out, "RELIABILITY WARNING") {
		t.Errorf("finder-failure run must print a prominent reliability warning:\n%s", out)
	}
	if !strings.Contains(out, "NOT a clean bill of health") {
		t.Errorf("empty+unreliable run should say it is not a clean bill of health:\n%s", out)
	}
	if !strings.Contains(out, "Agent failures:") {
		t.Errorf("failure counts should be reported:\n%s", out)
	}
}

// TestPrintResult_NoFindersRanWarns confirms a run where no finder agent ran at
// all (e.g. empty scope) is flagged as telling us nothing, not as clean.
func TestPrintResult_NoFindersRanWarns(t *testing.T) {
	var buf bytes.Buffer
	res := &funnel.Result{Commit: "abc123", Stats: funnel.Stats{FinderRuns: 0}}
	printResult(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "RELIABILITY WARNING") {
		t.Errorf("no-finders run must warn:\n%s", out)
	}
	if !strings.Contains(out, "No finder agents ran") {
		t.Errorf("warning should explain no finders ran:\n%s", out)
	}
}

// TestPrintResult_ToolHealthWarning confirms harness tool-health issues are
// surfaced prominently in the scan summary, and absent on a clean run.
func TestPrintResult_ToolHealthWarning(t *testing.T) {
	var withIssues bytes.Buffer
	res := &funnel.Result{
		Commit: "abc123",
		Stats: funnel.Stats{
			FinderRuns: 6, FinderFailures: 0,
			ToolIssues: []funnel.ToolIssue{{Source: "infra", Tool: "sandbox_exec", Severity: "high", Count: 2}},
		},
	}
	printResult(&withIssues, res)
	if out := withIssues.String(); !strings.Contains(out, "Tool health:") ||
		!strings.Contains(out, "sandbox_exec") || !strings.Contains(out, "HIGH") {
		t.Errorf("tool-health issue should be surfaced in the summary:\n%s", out)
	}

	var clean bytes.Buffer
	printResult(&clean, &funnel.Result{Commit: "abc123", Stats: funnel.Stats{FinderRuns: 6}})
	if strings.Contains(clean.String(), "Tool health:") {
		t.Errorf("clean run must not print a tool-health line:\n%s", clean.String())
	}
}

// TestPrintResult_MergeRootCauseOnly is the bugbot-g13 regression: a run whose
// only merges came from the same-root-cause pass (within=0, cross=0, root=N)
// must still report the merge to the operator on the "Location merges" line.
func TestPrintResult_MergeRootCauseOnly(t *testing.T) {
	var buf bytes.Buffer
	res := &funnel.Result{
		Commit: "abc123",
		Stats:  funnel.Stats{FinderRuns: 6, MergedRootCause: 3},
	}
	printResult(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "Location merges:") {
		t.Errorf("a root-cause-only merge run must print the Location merges line:\n%s", out)
	}
	if !strings.Contains(out, "root_cause=3") {
		t.Errorf("merge line must surface root_cause=3:\n%s", out)
	}
}

// TestStats_ReliabilityHelpers covers the boundary conditions of the helpers the
// CLI and exit code rely on.
func TestStats_ReliabilityHelpers(t *testing.T) {
	cases := []struct {
		name           string
		runs, failures int
		reliable, most bool
	}{
		{"clean", 6, 0, true, false},
		{"one of six failed", 6, 1, false, false},
		{"half failed not majority", 6, 3, false, false},
		{"majority failed", 6, 4, false, true},
		{"all failed", 6, 6, false, true},
		{"no finders ran", 0, 0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := funnel.Stats{FinderRuns: c.runs, FinderFailures: c.failures}
			if got := s.FinderReliable(); got != c.reliable {
				t.Errorf("FinderReliable() = %v, want %v", got, c.reliable)
			}
			if got := s.MostFindersFailed(); got != c.most {
				t.Errorf("MostFindersFailed() = %v, want %v", got, c.most)
			}
		})
	}
}
