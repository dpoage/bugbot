package daemon

import (
	"context"
	"errors"

	"github.com/dpoage/bugbot/internal/control"
)

// dispatchQueueSize bounds how many control-socket dispatch RPCs may be
// queued awaiting a cycle boundary before SubmitDispatch's send blocks the
// CALLING (socket) goroutine — never the scheduler goroutine, which only
// ever RECEIVES from this channel. A small buffer is enough: dispatch verbs
// are operator-paced (not a hot path), and a full queue simply means
// SubmitDispatch's caller waits, exactly like a human waiting for `bugbot
// scan` to start.
const dispatchQueueSize = 4

// Dispatcher executes one control-socket dispatch verb to completion and
// reduces it to a wire-ready summary. The CLI wires a concrete
// implementation backed by *engine.Dispatcher sharing the daemon's
// already-open store (see engine.NewShared + engine.Dispatcher.DispatchVerb)
// — package daemon deliberately does not import engine, keeping the
// scheduler loop's dependency surface small and verb semantics centralized
// where Scan/Verify/Repro/Sweep/Reconcile already live. Tests inject a fake.
//
// nil disables dispatch entirely: SubmitDispatch returns an error without
// ever touching the scheduler loop, and Daemon.Run never selects on the
// dispatch queue in the first place.
type Dispatcher interface {
	Dispatch(ctx context.Context, verb control.Verb, opts control.DispatchOpts) (control.DispatchSummary, error)
}

// dispatchJob is one queued control-socket RPC awaiting the scheduler's
// attention. reply is 1-buffered so execDispatch's send never blocks even
// if the caller has already given up (ctx cancelled).
type dispatchJob struct {
	ctx   context.Context
	verb  control.Verb
	opts  control.DispatchOpts
	reply chan dispatchResult
}

type dispatchResult struct {
	summary control.DispatchSummary
	err     error
}

// errDispatchDisabled is returned by SubmitDispatch when the Daemon was
// built without a Dispatcher (Deps.Dispatch nil) — the control socket may
// still be listening (e.g. for event streaming only), but no verb executor
// was wired.
var errDispatchDisabled = errors.New("daemon: dispatch not enabled")

// SubmitDispatch enqueues verb/opts for execution at the next cycle
// boundary and blocks until it completes (or ctx is cancelled, or the
// Daemon has no Dispatcher wired). This is the ONLY entry point a
// control-socket server goroutine should call — it never touches the
// scheduler's internals directly, preserving the invariant that dispatched
// work only ever runs on the scheduler's own goroutine, serialized with
// (never concurrent with) an in-flight cycle.
//
// Reply semantics: SubmitDispatch returns once the verb has RUN TO
// COMPLETION, not merely been accepted — matching how the in-process
// dispatch palette already behaves (a Scan/Verify/Repro/Sweep/Reconcile
// call blocks its caller until the verb returns). See bugbot-2p8z.4's
// design notes.
func (d *Daemon) SubmitDispatch(ctx context.Context, verb control.Verb, opts control.DispatchOpts) (control.DispatchSummary, error) {
	if d.dispatch == nil {
		return control.DispatchSummary{}, errDispatchDisabled
	}

	job := dispatchJob{ctx: ctx, verb: verb, opts: opts, reply: make(chan dispatchResult, 1)}
	select {
	case d.dispatchCh <- job:
	case <-ctx.Done():
		return control.DispatchSummary{}, ctx.Err()
	}

	select {
	case res := <-job.reply:
		return res.summary, res.err
	case <-ctx.Done():
		return control.DispatchSummary{}, ctx.Err()
	}
}

// execDispatch runs job to completion on the scheduler goroutine (called
// only from Run's select loop, never concurrently with a cycle) and posts
// the result. The daemon's own ctx (not job.ctx) drives execution: once a
// job is accepted off the queue it runs to completion regardless of the
// original caller giving up — SubmitDispatch's second select simply stops
// waiting on job.reply, but the verb itself (e.g. a Scan already touching
// the store) is never interrupted mid-write.
func (d *Daemon) execDispatch(ctx context.Context, job dispatchJob) {
	summary, err := d.dispatch.Dispatch(ctx, job.verb, job.opts)
	job.reply <- dispatchResult{summary: summary, err: err}
}
