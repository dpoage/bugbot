package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/dpoage/bugbot/internal/funnel"
)

// SweepOpts holds the parsed flag values for `bugbot sweep`.
type SweepOpts struct {
	Target string
	Force  bool
	Out    io.Writer
	ErrOut io.Writer
}

// SweepResult is the outcome of a Dispatcher.Sweep call.
type SweepResult struct {
	Result *funnel.Result
}

// Sweep implements `bugbot sweep`: a one-shot impact-sweep drain that
// re-ranks unswept open findings by reachability. This is the same
// impact-sweep logic the daemon runs after every postCycle, exposed as a
// one-shot verb for manual operation or scripted workflows. It is
// idempotent: a second run over already-swept findings is a verified no-op
// (UnsweptOpenFindings returns empty, no LLM calls are made).
func (d *Dispatcher) Sweep(ctx context.Context, opts SweepOpts) (*SweepResult, error) {
	// Advisory scan lock: mirrors `bugbot scan`'s behaviour so concurrent
	// drains and scans are detected and reported gracefully.
	if err := d.ensureOwner(ctx, opts.Force); err != nil {
		return nil, err
	}
	if err := d.ensureRoleClients(ctx); err != nil {
		return nil, err
	}

	cfg := d.cfg
	st := d.store

	repo, err := d.openRepo(ctx, opts.Target)
	if err != nil {
		return nil, err
	}

	// nil progress sink (d.sink is nil for the one-shot sweep command): it
	// prints its summary to stdout and does not own a status.json snapshot
	// (the daemon/scan do).
	funnelOpts, sandboxDegraded, err := BuildFunnelOptions(cfg, FunnelOptionOverrides{
		Progress: d.sink,
	})
	if err != nil {
		return nil, err
	}
	if sandboxDegraded {
		PrintSandboxDegradedWarning(opts.ErrOut)
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

	result, err := f.SweepDrain(ctx)
	if err != nil {
		return nil, fmt.Errorf("sweep drain: %w", err)
	}

	out := opts.Out
	if len(result.Findings) == 0 {
		_, _ = fmt.Fprintln(out, "Impact sweep: no unswept open findings.")
		return &SweepResult{Result: result}, nil
	}
	_, _ = fmt.Fprintf(out, "Impact sweep: %d finding(s) swept (scan_run_id=%s).\n",
		len(result.Findings), result.ScanRunID)
	return &SweepResult{Result: result}, nil
}
