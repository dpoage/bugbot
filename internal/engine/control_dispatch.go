package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/control"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// NewShared builds a Dispatcher in Owner mode over an ALREADY-OPEN,
// already-owned writable store — for a process (the daemon) that already
// holds the writer lock and wants to reuse Dispatcher's existing verb logic
// (Scan/Verify/Repro/Sweep) for control-socket dispatch RPCs, WITHOUT
// opening a second store handle.
//
// A second store.Open on the same path would contend for the writer lock
// even within the same process: the lock is an exclusive flock tied to the
// open file description (internal/store/dblock.go), not the process, so a
// same-process second Open blocks/fails exactly like a foreign process
// would. NewShared sidesteps this entirely by never calling store.Open —
// the caller supplies the handle it already owns.
//
// Lifecycle: the returned Dispatcher's Close must NEVER be called — its
// store is not owned by it (the daemon's own store lifecycle governs it),
// and no heartbeat goroutine is started (mode is Owner unconditionally, but
// initOwner is deliberately skipped: the daemon's own liveness/scheduler
// loop is a stronger, already-existing "I am alive" signal than the
// heartbeat that Open's Owner path starts for a standalone CLI invocation).
func NewShared(cfg config.Config, sink progress.EventSink, st *store.Store) *Dispatcher {
	return &Dispatcher{cfg: cfg, sink: sink, store: st, mode: Owner}
}

// DispatchVerb executes verb via the Dispatcher's existing Scan/Verify/
// Repro/Sweep/Reconcile methods, translating a control-socket wire request
// into the verb's typed Opts and reducing its typed Result into the small
// structured summary the wire protocol carries back (see internal/control's
// package doc: dispatch replies are a summary, not a serialized
// funnel.Result — full result trees are neither meant to cross a process
// boundary nor needed by the palette, which only ever renders a short
// count-based summary line).
//
// Table-driven per bugbot-2p8z.4's scope pin: a future review verb
// (bugbot-2p8z.5, once extracted from package cli into engine) slots in by
// adding one more case here — the transport (internal/control, the daemon's
// dispatch queue) needs no changes.
//
// Out/ErrOut are always io.Discard: dispatch output over the socket is
// summarized, never streamed as raw text (see DispatchOpts's doc comment).
func (d *Dispatcher) DispatchVerb(ctx context.Context, verb control.Verb, opts control.DispatchOpts) (control.DispatchSummary, error) {
	switch verb {
	case control.VerbScan:
		res, err := d.Scan(ctx, ScanOpts{
			Target: opts.Target, Since: opts.Since, Force: opts.Force,
			Out: io.Discard, ErrOut: io.Discard,
		})
		if err != nil {
			return control.DispatchSummary{}, err
		}
		return scanDispatchSummary(res), nil

	case control.VerbVerify:
		res, err := d.Verify(ctx, VerifyOpts{
			Target: opts.Target, Force: opts.Force, Suspected: opts.Suspected,
			Out: io.Discard,
		})
		if err != nil {
			return control.DispatchSummary{}, err
		}
		return verifyDispatchSummary(res), nil

	case control.VerbRepro:
		res, err := d.Repro(ctx, ReproOpts{
			Target: opts.Target, MaxN: opts.MaxN, Out: io.Discard,
		})
		if err != nil {
			return control.DispatchSummary{}, err
		}
		return reproDispatchSummary(res), nil

	case control.VerbSweep:
		res, err := d.Sweep(ctx, SweepOpts{
			Target: opts.Target, Force: opts.Force, Out: io.Discard, ErrOut: io.Discard,
		})
		if err != nil {
			return control.DispatchSummary{}, err
		}
		return sweepDispatchSummary(res), nil

	case control.VerbReconcile:
		res, err := d.Reconcile(ctx, ReconcileOpts{
			Cap: opts.Cap, Out: io.Discard,
		})
		if err != nil {
			return control.DispatchSummary{}, err
		}
		return reconcileDispatchSummary(res), nil

	default:
		return control.DispatchSummary{}, fmt.Errorf("engine: unknown dispatch verb %q", verb)
	}
}

func scanDispatchSummary(res *ScanResult) control.DispatchSummary {
	if res == nil || res.Result == nil {
		return control.DispatchSummary{}
	}
	return control.DispatchSummary{HasResult: true, FindingCount: len(res.Result.Findings)}
}

func verifyDispatchSummary(res *VerifyResult) control.DispatchSummary {
	if res == nil || res.Drain == nil {
		return control.DispatchSummary{}
	}
	return control.DispatchSummary{HasDrain: true, FindingCount: len(res.Drain.Findings)}
}

func reproDispatchSummary(res *ReproResult) control.DispatchSummary {
	if res == nil {
		return control.DispatchSummary{}
	}
	if res.Skipped != "" {
		return control.DispatchSummary{Skipped: res.Skipped}
	}
	if res.Summary == nil {
		return control.DispatchSummary{}
	}
	return control.DispatchSummary{HasSummary: true, Attempted: res.Summary.Attempted}
}

func sweepDispatchSummary(res *SweepResult) control.DispatchSummary {
	if res == nil || res.Result == nil {
		return control.DispatchSummary{}
	}
	return control.DispatchSummary{HasResult: true, FindingCount: len(res.Result.Findings)}
}

func reconcileDispatchSummary(res *ReconcileResult) control.DispatchSummary {
	if res == nil || res.Result == nil {
		return control.DispatchSummary{}
	}
	s := res.Result.Stats
	return control.DispatchSummary{
		ReconcileNominated:  s.ReconcileNominated,
		ReconcileArbitrated: s.ReconcileArbitrated,
		ReconcileMerged:     s.ReconcileMerged,
		ReconcileSkippedCap: s.ReconcileSkippedCap,
	}
}
