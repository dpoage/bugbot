package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/funnel"
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
