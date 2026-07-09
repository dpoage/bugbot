package engine

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/control"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/repro"
)

// TestNewShared_DoesNotOpenASecondStore verifies NewShared never touches the
// store package's Open/OpenReadOnly (it only wraps the handle it is given),
// by constructing it over a nil store — a real Open call would panic or
// error, not silently succeed, so this proves construction alone performs
// no store I/O.
func TestNewShared_ConstructsOwnerModeDispatcherWithoutStoreIO(t *testing.T) {
	d := NewShared(config.Config{}, progress.Discard{}, nil)
	if d.mode != Owner {
		t.Errorf("NewShared().mode = %v, want Owner", d.mode)
	}
	if d.store != nil {
		t.Errorf("NewShared().store = %v, want the nil handle passed in", d.store)
	}
	if d.hbCancel != nil {
		t.Error("NewShared() started a heartbeat goroutine; want none (daemon owns liveness)")
	}
}

// TestDispatchVerb_UnknownVerb verifies an unrecognized verb errors instead
// of silently no-op'ing, without ever touching a store/funnel.
func TestDispatchVerb_UnknownVerb(t *testing.T) {
	d := NewShared(config.Config{}, progress.Discard{}, nil)
	_, err := d.DispatchVerb(context.Background(), control.Verb("review"), control.DispatchOpts{})
	if err == nil {
		t.Fatal("DispatchVerb(unknown) error = nil, want error")
	}
}

// TestDispatchSummaryHelpers pins the exact reduction from typed engine
// Results to the wire's DispatchSummary, matching internal/tui/palette.go's
// scanSummary/verifySummary/reproSummary/sweepSummary text derivation so an
// Attach-mode dispatch renders identically to an in-process one.
func TestDispatchSummaryHelpers(t *testing.T) {
	t.Run("scan nil result", func(t *testing.T) {
		got := scanDispatchSummary(&ScanResult{})
		if got.HasResult {
			t.Errorf("scanDispatchSummary(nil Result) = %+v, want HasResult=false", got)
		}
	})
	t.Run("scan with findings", func(t *testing.T) {
		got := scanDispatchSummary(&ScanResult{Result: &funnel.Result{Findings: []domain.Finding{{}, {}}}})
		if !got.HasResult || got.FindingCount != 2 {
			t.Errorf("scanDispatchSummary() = %+v, want HasResult=true FindingCount=2", got)
		}
	})
	t.Run("verify no drain", func(t *testing.T) {
		got := verifyDispatchSummary(&VerifyResult{})
		if got.HasDrain {
			t.Errorf("verifyDispatchSummary(nil Drain) = %+v, want HasDrain=false", got)
		}
	})
	t.Run("verify with drain", func(t *testing.T) {
		got := verifyDispatchSummary(&VerifyResult{Drain: &funnel.Result{Findings: []domain.Finding{{}}}})
		if !got.HasDrain || got.FindingCount != 1 {
			t.Errorf("verifyDispatchSummary() = %+v, want HasDrain=true FindingCount=1", got)
		}
	})
	t.Run("repro skipped", func(t *testing.T) {
		got := reproDispatchSummary(&ReproResult{Skipped: "no sandbox"})
		if got.Skipped != "no sandbox" {
			t.Errorf("reproDispatchSummary() = %+v, want Skipped=\"no sandbox\"", got)
		}
	})
	t.Run("repro empty backlog", func(t *testing.T) {
		got := reproDispatchSummary(&ReproResult{})
		if got.HasSummary {
			t.Errorf("reproDispatchSummary(nil Summary) = %+v, want HasSummary=false", got)
		}
	})
	t.Run("repro attempted", func(t *testing.T) {
		got := reproDispatchSummary(&ReproResult{Summary: &repro.Summary{Attempted: 4}})
		if !got.HasSummary || got.Attempted != 4 {
			t.Errorf("reproDispatchSummary() = %+v, want HasSummary=true Attempted=4", got)
		}
	})
	t.Run("sweep with findings", func(t *testing.T) {
		got := sweepDispatchSummary(&SweepResult{Result: &funnel.Result{Findings: []domain.Finding{{}, {}, {}}}})
		if !got.HasResult || got.FindingCount != 3 {
			t.Errorf("sweepDispatchSummary() = %+v, want HasResult=true FindingCount=3", got)
		}
	})
	t.Run("reconcile nil result", func(t *testing.T) {
		got := reconcileDispatchSummary(&ReconcileResult{})
		if got != (control.DispatchSummary{}) {
			t.Errorf("reconcileDispatchSummary(nil Result) = %+v, want zero value", got)
		}
	})
	t.Run("reconcile with counts", func(t *testing.T) {
		got := reconcileDispatchSummary(&ReconcileResult{Result: &funnel.Result{Stats: funnel.Stats{
			ReconcileNominated:  5,
			ReconcileArbitrated: 4,
			ReconcileMerged:     2,
			ReconcileSkippedCap: 1,
		}}})
		want := control.DispatchSummary{ReconcileNominated: 5, ReconcileArbitrated: 4, ReconcileMerged: 2, ReconcileSkippedCap: 1}
		if got != want {
			t.Errorf("reconcileDispatchSummary() = %+v, want %+v", got, want)
		}
	})
}
