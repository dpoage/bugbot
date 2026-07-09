package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/dpoage/bugbot/internal/funnel"
)

// ReconcileOpts holds the parsed flag values for `bugbot reconcile` /
// the reconcile dispatch verb. Unlike Scan/Verify/Sweep/Repro's Opts, there
// is deliberately no Target or Force field: ReconcileDedup operates
// entirely over the store's OPEN findings (openRepo("") always resolves to
// cwd, matching every sibling verb's TUI-dispatch default — see Repro's
// opts.Target doc comment), and Reconcile always escalates to Owner the
// same unconditional way Repro does (see Reconcile's body) since a
// dedup merge is itself a write with nothing sensible to refuse.
type ReconcileOpts struct {
	// Cap bounds the number of dedup-arbiter invocations this pass may
	// spend; Cap <= 0 means funnel.DefaultReconcileCap (mirrors
	// funnel.ReconcileDedup's own cap<=0 default so a zero-value
	// ReconcileOpts behaves identically to the timer path).
	Cap int
	Out io.Writer
}

// ReconcileResult is the outcome of a Dispatcher.Reconcile call.
type ReconcileResult struct {
	Result *funnel.Result
}

// Reconcile implements the backlog-reconcile dedup pass (bugbot-ezmx.4) as a
// one-shot, manually-dispatchable verb: it nominates duplicate-candidate
// pairs among currently-OPEN findings and merges every confident
// dedup-arbiter "yes" — see funnel.ReconcileDedup. This is the same backlog
// logic the daemon runs on its periodic reconcile timer
// (daemon.runReconcile), exposed for manual operation via the control
// socket / TUI palette / one-shot CLI command.
//
// Gating, relative to the timer path: daemon.runReconcile applies both the
// day-token-budget gate (via its caller, Run) and a storeHealthy integrity
// check before it runs, because an unattended timer firing on a possibly
// corrupt store is exactly the case that gate exists for. Reconcile does
// NEITHER of those: it applies no storeHealthy gate itself, matching every
// other Dispatcher verb (Scan/Verify/Repro/Sweep never call storeHealthy —
// that check is a daemon-timer-only concept, not part of engine's verb
// contract) and no day-budget gate, matching every other DISPATCHED verb
// (the day-budget gate in daemon.Run only wraps the timer-driven calls, not
// manual dispatch — an operator who explicitly asks for a reconcile pass
// gets one). The per-call token budget still applies via
// BuildFunnelOptions, identical to Scan/Verify/Sweep. A human explicitly
// asking for a reconcile pass is trusted the same way an explicit `bugbot
// scan` is: run it now, do not silently no-op on a heuristic the operator
// cannot see.
func (d *Dispatcher) Reconcile(ctx context.Context, opts ReconcileOpts) (*ReconcileResult, error) {
	// main's dispatch verbs that lack a Force flag (Repro) escalate
	// unconditionally: checkScanLock's heuristic never refuses a fresh
	// Observer, and a genuinely live writer still refuses via
	// escalateToOwner's store.Open ErrLocked. Reconcile follows the same
	// shape since ReconcileOpts has no Force field to thread through.
	if err := d.ensureOwner(ctx, true); err != nil {
		return nil, err
	}
	if err := d.ensureRoleClients(ctx); err != nil {
		return nil, err
	}

	cfg := d.cfg
	st := d.store

	// Reconcile has no Target concept (see ReconcileOpts's doc comment):
	// openRepo("") resolves to cwd, matching every sibling verb's TUI
	// dispatch default and the daemon's own single-target repo.
	repo, err := d.openRepo(ctx, "")
	if err != nil {
		return nil, err
	}

	funnelOpts, sandboxDegraded, err := BuildFunnelOptions(cfg, FunnelOptionOverrides{
		Progress: d.sink,
	})
	if err != nil {
		return nil, err
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	if sandboxDegraded {
		PrintSandboxDegradedWarning(out)
	}

	f, err := funnel.New(funnel.RoleClients{
		Finder:       d.finder,
		Verifier:     d.verifier,
		Cartographer: d.cartographer,
		Arbiter:      d.arbiter,
	}, st, repo, funnelOpts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	result, err := f.ReconcileDedup(ctx, opts.Cap)
	if err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}

	if result.Stats.ReconcileNominated == 0 {
		_, _ = fmt.Fprintln(out, "Backlog reconcile: no duplicate candidates nominated.")
		return &ReconcileResult{Result: result}, nil
	}
	_, _ = fmt.Fprintf(out, "Backlog reconcile: %d nominated, %d arbitrated, %d merged, %d skipped (cap).\n",
		result.Stats.ReconcileNominated, result.Stats.ReconcileArbitrated,
		result.Stats.ReconcileMerged, result.Stats.ReconcileSkippedCap)
	return &ReconcileResult{Result: result}, nil
}
