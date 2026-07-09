package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// TestActionableLockError_WrapsErrLockedWithGuidance covers the store-locked
// acceptance case: a *store.ErrLocked (the daemon holding the writer flock)
// must come back with guidance pointing at the TUI palette / control-socket
// dispatch path instead of a bare "database is locked" message, and must
// still satisfy errors.As(*store.ErrLocked) so callers/tests can keep
// matching on the underlying type.
func TestActionableLockError_WrapsErrLockedWithGuidance(t *testing.T) {
	locked := &store.ErrLocked{Path: "/tmp/state.db", HolderPID: 4242}
	err := actionableLockError(fmt.Errorf("open store: %w", locked))
	if err == nil {
		t.Fatal("actionableLockError(ErrLocked) = nil, want a wrapped error")
	}
	msg := err.Error()
	for _, want := range []string{"TUI command palette", "control socket", "reconcile_interval"} {
		if !strings.Contains(msg, want) {
			t.Errorf("actionableLockError message = %q, want it to mention %q", msg, want)
		}
	}
	var got *store.ErrLocked
	if !errors.As(err, &got) {
		t.Fatalf("errors.As(actionableLockError(...), *store.ErrLocked) = false, want true")
	}
	if got.HolderPID != 4242 {
		t.Errorf("unwrapped ErrLocked.HolderPID = %d, want 4242 (original preserved)", got.HolderPID)
	}
}

// TestActionableLockError_PassesThroughOtherErrors confirms a non-ErrLocked
// error is returned unchanged: the friendly rewrite is specific to the
// store-locked case, not a catch-all error wrapper.
func TestActionableLockError_PassesThroughOtherErrors(t *testing.T) {
	plain := errors.New("config: invalid storage.path")
	if got := actionableLockError(plain); got != plain {
		t.Errorf("actionableLockError(plain) = %v, want the original error unchanged", got)
	}
}

// TestPrintReconcileResult_FailuresAndSpend confirms the CLI's supplemental
// line covers exactly what Dispatcher.Reconcile does not self-print:
// arbiter-failure count and token spend. Spend uses DedupArbiterTokens (the
// field funnel.ReconcileDedup actually populates) -- NOT InputTokens/
// OutputTokens, which stay structurally 0 for a reconcile pass.
func TestPrintReconcileResult_FailuresAndSpend(t *testing.T) {
	var buf bytes.Buffer
	res := &engine.ReconcileResult{Result: &funnel.Result{
		Stats: funnel.Stats{
			ReconcileFailures:  2,
			DedupArbiterTokens: 150,
		},
	}}
	printReconcileResult(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "2 arbiter call(s)") {
		t.Errorf("output = %q, want the arbiter-failure count reported", out)
	}
	if !strings.Contains(out, "150 tokens") {
		t.Errorf("output = %q, want the dedup-arbiter spend line", out)
	}
}

// TestPrintReconcileResult_QuietOnCleanRun confirms a run with no failures
// and no spend prints nothing extra -- the CLI must not manufacture noise on
// top of Dispatcher.Reconcile's own self-printed summary line.
func TestPrintReconcileResult_QuietOnCleanRun(t *testing.T) {
	var buf bytes.Buffer
	printReconcileResult(&buf, &engine.ReconcileResult{Result: &funnel.Result{}})
	if buf.Len() != 0 {
		t.Errorf("output = %q, want no supplemental output for a zero-spend, zero-failure result", buf.String())
	}
}
