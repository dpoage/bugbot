package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
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
		"open findings: 1", // setup() seeds one open finding
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fresh status missing %q\n---\n%s", want, out)
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
