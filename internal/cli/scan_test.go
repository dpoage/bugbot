package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// openTestStore opens a fresh on-disk store in t.TempDir().
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

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

// TestCheckScanLock_Refuse verifies that a live scan run belonging to a
// different pid causes checkScanLock to return an error naming the run and pid.
func TestCheckScanLock_Refuse(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Create a scan run and assign a foreign pid.
	id, err := st.BeginScanRun(ctx, store.ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}
	foreignPID := 99999
	if _, err := st.DB().ExecContext(ctx, `UPDATE scan_runs SET pid = ? WHERE id = ?`, foreignPID, id); err != nil {
		t.Fatal(err)
	}

	selfPID := os.Getpid()
	lockErr := checkScanLock(ctx, st, false, selfPID)
	if lockErr == nil {
		t.Fatal("checkScanLock: expected error for live foreign scan, got nil")
	}
	if !strings.Contains(lockErr.Error(), id) {
		t.Errorf("checkScanLock error should name the run id %q: %v", id, lockErr)
	}
	if !strings.Contains(lockErr.Error(), "99999") {
		t.Errorf("checkScanLock error should name the pid 99999: %v", lockErr)
	}
}

// TestCheckScanLock_Force verifies that --force bypasses the lock check even
// when a live foreign scan exists.
func TestCheckScanLock_Force(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	id, err := st.BeginScanRun(ctx, store.ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE scan_runs SET pid = 99999 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	if err := checkScanLock(ctx, st, true, os.Getpid()); err != nil {
		t.Errorf("checkScanLock with force=true: got error %v, want nil", err)
	}
}

// TestCheckScanLock_SamePIDAllowed verifies that a scan run owned by the same
// pid (e.g. re-entrant call in tests) does NOT trigger the lock.
func TestCheckScanLock_SamePIDAllowed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.BeginScanRun(ctx, store.ScanOneshot, "abc"); err != nil {
		t.Fatal(err)
	}
	// selfPID matches the pid written by BeginScanRun.
	if err := checkScanLock(ctx, st, false, os.Getpid()); err != nil {
		t.Errorf("checkScanLock same pid: got error %v, want nil", err)
	}
}

// TestCheckScanLock_StaleHeartbeatAllowed verifies that a run with a stale
// heartbeat (older than 10 min) does not block a new scan.
func TestCheckScanLock_StaleHeartbeatAllowed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	id, err := st.BeginScanRun(ctx, store.ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}
	// Back-date heartbeat and assign a foreign pid.
	stale := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339Nano)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE scan_runs SET heartbeat = ?, pid = 99999 WHERE id = ?`, stale, id,
	); err != nil {
		t.Fatal(err)
	}

	if err := checkScanLock(ctx, st, false, os.Getpid()); err != nil {
		t.Errorf("checkScanLock stale heartbeat: got error %v, want nil", err)
	}
}

// TestCheckScanLock_FinishedRunAllowed verifies that a finished run (even with
// a fresh heartbeat) does not block a new scan.
func TestCheckScanLock_FinishedRunAllowed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	id, err := st.BeginScanRun(ctx, store.ScanOneshot, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE scan_runs SET pid = 99999 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id, "{}"); err != nil {
		t.Fatal(err)
	}

	if err := checkScanLock(ctx, st, false, os.Getpid()); err != nil {
		t.Errorf("checkScanLock finished run: got error %v, want nil", err)
	}
}

// TestCheckScanLock_EmptyStore verifies that checkScanLock returns nil when no
// scan runs exist (first-ever scan).
func TestCheckScanLock_EmptyStore(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := checkScanLock(ctx, st, false, os.Getpid()); err != nil {
		t.Errorf("checkScanLock empty store: got error %v, want nil", err)
	}
}

// TestApplyReranked refreshes matched findings' severity and verdict detail from
// the post-sweep set and leaves unmatched findings untouched. This is the
// oracle #3 fix: with the terminal Stage F removed from run(), a scan Result
// carries PRE-sweep severities, and the oneshot summary must reflect the
// re-ranked store state instead.
func TestApplyReranked(t *testing.T) {
	findings := []domain.Finding{
		{ID: "a", Severity: "high", VerdictDetail: "pre-a"},
		{ID: "b", Severity: "medium", VerdictDetail: "pre-b"},
		{ID: "c", Severity: "low", VerdictDetail: "untouched"},
	}
	reranked := map[string]domain.Finding{
		"a": {ID: "a", Severity: "low", VerdictDetail: "downranked: zero non-test callers"},
		"b": {ID: "b", Severity: "critical", VerdictDetail: "escalated"},
		// "c" intentionally absent: nothing re-ranked it this pass.
		"z": {ID: "z", Severity: "high", VerdictDetail: "not in the scan's result set"},
	}

	applyReranked(findings, reranked)

	if findings[0].Severity != "low" || findings[0].VerdictDetail != "downranked: zero non-test callers" {
		t.Errorf("finding a not refreshed from re-ranked set: %+v", findings[0])
	}
	if findings[1].Severity != "critical" || findings[1].VerdictDetail != "escalated" {
		t.Errorf("finding b not refreshed from re-ranked set: %+v", findings[1])
	}
	if findings[2].Severity != "low" || findings[2].VerdictDetail != "untouched" {
		t.Errorf("finding c (absent from re-ranked set) must be left untouched: %+v", findings[2])
	}
}

// TestSandboxRunOpts_HonorsResourceCaps pins the contract that the configured
// sandbox.cpus, sandbox.memory_mb, and sandbox.pids_limit actually reach the
// container backend. Regression: sandboxRunOpts threaded network/timeout/idle
// but dropped cpus/memory_mb, and pids_limit had no config knob at all — so
// every run kept the backend defaults (2 CPUs / 2048 MB / 256 pids). The 256
// pids cap crashed the Bazel JVM ("unable to create native thread") during
// analysis, so every Bazel-repo reproduction failed as environment_error.
// Distinct-from-default values prove the config — not the hardcoded backend
// default — flows through.
func TestSandboxRunOpts_HonorsResourceCaps(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.CPUs = 4
	cfg.Sandbox.MemoryMB = 8192
	cfg.Sandbox.PidsLimit = 4096

	sb := &sandbox.CLI{}
	for _, o := range sandboxRunOpts(cfg) {
		o(sb)
	}

	cpus, mem, pids := sb.Limits()
	if cpus != 4 {
		t.Errorf("cpus = %v, want 4 (sandbox.cpus dropped before reaching backend)", cpus)
	}
	if mem != 8192 {
		t.Errorf("memoryMB = %d, want 8192 (sandbox.memory_mb dropped before reaching backend)", mem)
	}
	if pids != 4096 {
		t.Errorf("pidsLimit = %d, want 4096 (sandbox.pids_limit dropped before reaching backend)", pids)
	}
}
