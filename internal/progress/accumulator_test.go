package progress

import (
	"testing"
	"time"
)

// TestStatusAccumulator_StampsPIDAndStartedAt verifies NewStatusAccumulator
// stamps the same identity fields NewSnapshotSink used to stamp directly, so
// a consumer (e.g. tui.LiveFeed) building a Frame from Snapshot() can run
// the same staleness/liveness checks a status.json reader would.
func TestStatusAccumulator_StampsPIDAndStartedAt(t *testing.T) {
	acc := NewStatusAccumulator()
	snap := acc.Snapshot()

	if snap.PID <= 0 {
		t.Errorf("PID not stamped: %d", snap.PID)
	}
	if snap.StartedAt.IsZero() {
		t.Errorf("StartedAt not stamped")
	}
}

// TestStatusAccumulator_ApplyFoldsAndSnapshotReflects verifies Apply folds
// events identically to what SnapshotSink.Handle used to do inline, and that
// Snapshot() materializes ActiveAgents/UnhealthyTools from the live maps —
// the extraction this test guards against regressing.
func TestStatusAccumulator_ApplyFoldsAndSnapshotReflects(t *testing.T) {
	acc := NewStatusAccumulator()
	now := time.Unix(1234, 0)

	acc.Apply(Event{Kind: KindScanStarted, ScanKind: "sweep", Commit: "deadbeef", Time: now})
	acc.Apply(Event{Kind: KindAgentStarted, Role: RoleFinder, Label: "lensA", Time: now})
	acc.Apply(Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lensA", Activity: "reading main.go", Time: now})
	acc.Apply(Event{Kind: KindToolUnhealthy, Role: RoleFinder, Label: "lensA", Tool: "sandbox", Severity: "high", Message: "timeout", Time: now})

	snap := acc.Snapshot()

	if snap.ScanKind != "sweep" || snap.Commit != "deadbeef" {
		t.Errorf("scan identity not folded: %+v", snap)
	}
	if len(snap.ActiveAgents) != 1 || snap.ActiveAgents[0].Activity != "reading main.go" {
		t.Fatalf("ActiveAgents = %+v, want one entry with the folded activity", snap.ActiveAgents)
	}
	if len(snap.UnhealthyTools) != 1 || snap.UnhealthyTools[0].Tool != "sandbox" || snap.UnhealthyTools[0].Severity != "high" {
		t.Fatalf("UnhealthyTools = %+v, want one sandbox/high entry", snap.UnhealthyTools)
	}
}

// TestStatusAccumulator_TerminalReporting verifies Apply reports terminal
// (scan/cycle finished) events accurately, since SnapshotSink.Handle relies
// on this return value to force an immediate write.
func TestStatusAccumulator_TerminalReporting(t *testing.T) {
	acc := NewStatusAccumulator()

	if terminal := acc.Apply(Event{Kind: KindStageStarted, Stage: StageVerify}); terminal {
		t.Error("KindStageStarted reported terminal=true, want false")
	}
	if terminal := acc.Apply(Event{Kind: KindScanFinished, ScanKind: "sweep"}); !terminal {
		t.Error("KindScanFinished reported terminal=false, want true")
	}
}
