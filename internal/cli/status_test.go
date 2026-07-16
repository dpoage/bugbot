package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// mustMarshalIndent marshals v or fails the test.
func mustMarshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// deadPID returns a PID that is (almost certainly) not a live process: it spawns
// a trivial process, waits for it to exit, and returns its now-reaped PID. A
// reaped PID is not signalable, so processAlive reports false for it.
func deadPID(t *testing.T) int {
	t.Helper()
	// A PID that cannot exist on Linux (max is configurable but well under this).
	// Using a fixed sentinel avoids racing PID reuse from a spawned process.
	const sentinel = 0x7FFFFFF0
	return sentinel
}

// writeStatus marshals a Status to the status.json beside the config's DB path
// (the same location SnapshotSink writes and `bugbot status` reads).
func writeStatus(t *testing.T, cfgPath string, st progress.Status) {
	t.Helper()
	// The DB path is <dir>/state.db per setup(); status.json is its sibling.
	dir := filepath.Dir(filepath.Join(filepath.Dir(cfgPath), "state.db"))
	path := progress.StatusPath(dir)
	// Reuse the package's atomic writer via a direct marshal to keep the test
	// independent of write timing.
	data := mustMarshalIndent(t, st)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatus_MissingFile(t *testing.T) {
	cfgPath, _, _ := setup(t)
	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored on missing file (should exit 0): %v", err)
	}
	if !strings.Contains(out, "no bugbot activity recorded") {
		t.Errorf("missing-file message wrong:\n%s", out)
	}
}

func TestStatus_Fresh(t *testing.T) {
	cfgPath, _, _ := setup(t)
	writeStatus(t, cfgPath, progress.Status{
		PID:         os.Getpid(), // a live process -> not stale by pid
		StartedAt:   time.Now().Add(-time.Minute),
		LastUpdated: time.Now(),
		ScanKind:    "sweep",
		Commit:      "abcdef1234567890",
		Stage:       "verify",
		Counts:      progress.Counts{Hypothesized: 5, Triaged: 3, Verified: 1, Killed: 2},
		SpendInput:  100,
		SpendOutput: 50,
		ActiveAgents: []progress.AgentStatus{
			{Role: "finder", Label: "nil-safety/error-handling"},
		},
		LastEvent: "stage: verify",
	})

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	for _, want := range []string{
		"bugbot is active",
		"kind=sweep",
		"abcdef123456",
		"stage:        verify",
		"hypothesized=5 triaged=3 verified=1 killed=2",
		"total=150 tokens",
		"nil-safety/error-handling",
		"World state:",
		"open: T2=1", // setup() seeds one open T2 finding
		"blackboard:   empty",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fresh status missing %q\n---\n%s", want, out)
		}
	}
}

// TestStatus_LiveReproBlockedRendered is the live-snapshot half of
// bugbot-14g0 acceptance 2's consumer requirement: Status.ReproBlocked
// (populated by a KindReproBlocked event) must be rendered by `bugbot
// status`, naming the actual missing binary — "node" for "js", and "go" (the
// binary), never Go's probe-mode token "present" (bugbot-813i).
func TestStatus_LiveReproBlockedRendered(t *testing.T) {
	cfgPath, _, _ := setup(t)
	writeStatus(t, cfgPath, progress.Status{
		PID:          os.Getpid(),
		StartedAt:    time.Now().Add(-time.Minute),
		LastUpdated:  time.Now(),
		ReproBlocked: map[string]int{"js": 38, "python": 2, "go": 1},
	})

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	for _, want := range []string{
		"38 finding(s) — image lacks node",
		"2 finding(s) — image lacks python",
		"1 finding(s) — image lacks go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q\n---\n%s", want, out)
		}
	}
}

func TestStatus_StaleByTime(t *testing.T) {
	cfgPath, _, _ := setup(t)
	writeStatus(t, cfgPath, progress.Status{
		PID:         os.Getpid(), // alive, but...
		StartedAt:   time.Now().Add(-time.Hour),
		LastUpdated: time.Now().Add(-time.Hour), // ...far older than staleAfter
		ScanKind:    "sweep",
		LastEvent:   "stage: verify",
	})

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if !strings.Contains(out, "stale or crashed") {
		t.Errorf("expected stale message:\n%s", out)
	}
	// Last-known state is still shown.
	if !strings.Contains(out, "kind=sweep") {
		t.Errorf("stale status omitted last-known state:\n%s", out)
	}
}

func TestStatus_StaleByDeadPID(t *testing.T) {
	cfgPath, _, _ := setup(t)
	writeStatus(t, cfgPath, progress.Status{
		PID:         deadPID(t),
		StartedAt:   time.Now().Add(-time.Minute),
		LastUpdated: time.Now(), // recent, so staleness must come from the dead pid
		ScanKind:    "targeted",
	})

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if !strings.Contains(out, "stale or crashed") {
		t.Errorf("expected stale message for dead pid:\n%s", out)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("expected dead-pid annotation:\n%s", out)
	}
}

// TestStatus_IdleStillShowsWorldState pins the pane-of-glass behavior: with no
// status.json at all (nothing running), status still renders the accumulated
// world state from the store.
func TestStatus_IdleStillShowsWorldState(t *testing.T) {
	cfgPath, _, _ := setup(t) // seeds one open T2 finding; writes NO status.json

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	for _, want := range []string{
		"no bugbot activity recorded",
		"World state:",
		"open: T2=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("idle status missing %q\n---\n%s", want, out)
		}
	}
}

// TestStatus_WorldStateReproBlockedRendered is the persisted-store half of
// bugbot-14g0 acceptance 2's consumer requirement: a blocked_toolchain
// repro_attempts row (store.BlockedToolchainCounts) must show up in the
// world-state block even with NO live status.json at all — this is the
// unattended-daemon-restarted-since case a live-only snapshot cannot cover.
func TestStatus_WorldStateReproBlockedRendered(t *testing.T) {
	cfgPath, st, _ := setup(t) // seeds one open T2 finding; writes NO status.json

	// setup() closes its own store handle before returning (see its doc) so
	// the CLI command can open a fresh one; reopen against the same DB path
	// to seed the blocked row.
	ctx := context.Background()
	reopened, err := store.Open(ctx, st.Path())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := reopened.EnqueueRepro(ctx, "fp-blocked-status-test"); err != nil {
		t.Fatalf("EnqueueRepro: %v", err)
	}
	if _, err := reopened.BlockReproAttemptOnToolchain(ctx, "fp-blocked-status-test", "js"); err != nil {
		t.Fatalf("BlockReproAttemptOnToolchain: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if !strings.Contains(out, "World state:") {
		t.Fatalf("missing World state section:\n%s", out)
	}
	if !strings.Contains(out, "1 finding(s) — image lacks node") {
		t.Errorf("missing persisted blocked-toolchain line:\n%s", out)
	}
}

// TestStatus_LiveCandidatesRendering asserts that a mid-hypothesize snapshot
// (LiveCandidates > 0, Counts.Hypothesized == 0) renders the live note and
// that a post-stage snapshot (LiveCandidates == 0) renders the plain final
// format without any live annotation.
func TestStatus_LiveCandidatesRendering(t *testing.T) {
	cfgPath, _, _ := setup(t)

	// Mid-hypothesize: live candidates accumulating.
	writeStatus(t, cfgPath, progress.Status{
		PID:            os.Getpid(),
		StartedAt:      time.Now().Add(-time.Minute),
		LastUpdated:    time.Now(),
		ScanKind:       "sweep",
		Stage:          "hypothesize",
		LiveCandidates: 8,
		Counts:         progress.Counts{}, // zeros — stage not yet finished
	})
	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if !strings.Contains(out, "candidates so far: 8") {
		t.Errorf("mid-hypothesize status should show live candidate note:\n%s", out)
	}
	// hypothesized= should still show 0 (stage not finished).
	if !strings.Contains(out, "hypothesized=0") {
		t.Errorf("mid-hypothesize status should show hypothesized=0:\n%s", out)
	}

	// Post-stage: live counters zero, final counts populated.
	writeStatus(t, cfgPath, progress.Status{
		PID:            os.Getpid(),
		StartedAt:      time.Now().Add(-time.Minute),
		LastUpdated:    time.Now(),
		ScanKind:       "sweep",
		Stage:          "triage",
		LiveCandidates: 0, // reset after stage-finish
		Counts:         progress.Counts{Hypothesized: 8},
	})
	out2, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if strings.Contains(out2, "candidates so far") {
		t.Errorf("post-stage status must not show live candidate note:\n%s", out2)
	}
	if !strings.Contains(out2, "hypothesized=8") {
		t.Errorf("post-stage status should show final hypothesized=8:\n%s", out2)
	}
}

// TestStatus_LiveVerifyKillRendering asserts that a mid-verify snapshot renders
// the live verified/killed notes, and that a post-verify snapshot shows the
// plain final format.
func TestStatus_LiveVerifyKillRendering(t *testing.T) {
	cfgPath, _, _ := setup(t)

	// Mid-verify: live counters accumulating.
	writeStatus(t, cfgPath, progress.Status{
		PID:          os.Getpid(),
		StartedAt:    time.Now().Add(-2 * time.Minute),
		LastUpdated:  time.Now(),
		ScanKind:     "sweep",
		Stage:        "verify",
		LiveVerified: 2,
		LiveKilled:   1,
		Counts:       progress.Counts{Hypothesized: 5, Triaged: 3}, // verify not done
	})
	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if !strings.Contains(out, "so far: 2") {
		t.Errorf("mid-verify status should show live verified note:\n%s", out)
	}
	if !strings.Contains(out, "so far: 1") {
		t.Errorf("mid-verify status should show live killed note:\n%s", out)
	}

	// Post-verify: live counters zeroed, final Counts populated.
	writeStatus(t, cfgPath, progress.Status{
		PID:          os.Getpid(),
		StartedAt:    time.Now().Add(-2 * time.Minute),
		LastUpdated:  time.Now(),
		ScanKind:     "sweep",
		Stage:        "persist",
		LiveVerified: 0,
		LiveKilled:   0,
		Counts:       progress.Counts{Hypothesized: 5, Triaged: 3, Verified: 2, Killed: 1},
	})
	out2, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if strings.Contains(out2, "so far:") {
		t.Errorf("post-verify status must not show any live notes:\n%s", out2)
	}
	if !strings.Contains(out2, "hypothesized=5 triaged=3 verified=2 killed=1") {
		t.Errorf("post-verify status should show final counts:\n%s", out2)
	}
}

// TestStatus_NoStoreNoSideEffect pins that status against a config whose store
// has never been created neither errors nor creates the DB as a side effect.
func TestStatus_NoStoreNoSideEffect(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nonexistent", "state.db")
	cfgYAML := strings.NewReplacer("%DBPATH%", dbPath, "%REPORTDIR%", filepath.Join(dir, "r")).Replace(minimalConfig)
	cfgPath := filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if strings.Contains(out, "World state:") {
		t.Errorf("no-store status must not render a world state:\n%s", out)
	}
	if _, serr := os.Stat(dbPath); !os.IsNotExist(serr) {
		t.Error("status must not create the store as a side effect")
	}
}

// TestStatus_ActivityRendered verifies that an agent with a non-empty Activity
// field displays it in the `bugbot status` output, bracketed next to the agent
// role and label.
func TestStatus_ActivityRendered(t *testing.T) {
	cfgPath, _, _ := setup(t)
	writeStatus(t, cfgPath, progress.Status{
		PID:         os.Getpid(),
		StartedAt:   time.Now().Add(-time.Minute),
		LastUpdated: time.Now(),
		ScanKind:    "sweep",
		Stage:       "hypothesize",
		ActiveAgents: []progress.AgentStatus{
			{Role: "finder", Label: "nil-safety", Activity: "reading main.go"},
			{Role: "finder", Label: "sql-injection"}, // no activity
		},
	})

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if !strings.Contains(out, "reading main.go") {
		t.Errorf("activity note not rendered:\n%s", out)
	}
	// The agent with no activity should appear without extra brackets.
	if !strings.Contains(out, "sql-injection") {
		t.Errorf("second agent (no activity) not shown:\n%s", out)
	}
	// Verify the activity is bracketed.
	if !strings.Contains(out, "[reading main.go]") {
		t.Errorf("activity note not bracketed:\n%s", out)
	}
}

// TestStatus_NoActivityNoBracket verifies that an agent without an activity
// string does NOT show brackets in the output.
func TestStatus_NoActivityNoBracket(t *testing.T) {
	cfgPath, _, _ := setup(t)
	writeStatus(t, cfgPath, progress.Status{
		PID:         os.Getpid(),
		LastUpdated: time.Now(),
		ScanKind:    "sweep",
		ActiveAgents: []progress.AgentStatus{
			{Role: "verifier", Label: "some bug"},
		},
	})

	out, err := run(t, cfgPath, "status")
	if err != nil {
		t.Fatalf("status errored: %v", err)
	}
	if strings.Contains(out, "[") || strings.Contains(out, "]") {
		t.Errorf("no activity should produce no brackets:\n%s", out)
	}
}
