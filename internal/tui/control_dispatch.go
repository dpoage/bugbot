package tui

import (
	"context"
	"errors"

	"github.com/dpoage/bugbot/internal/control"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/repro"
)

// controlDispatch is the Attach-mode transport backing the dispatch
// palette (bugbot-2p8z.4): it satisfies the SAME `dispatcher` interface
// Owner mode's *engine.Dispatcher does, but executes each verb as a
// control-socket RPC (see internal/control) instead of in-process.
//
// The wire reply carries a reduced control.DispatchSummary (counts), not a
// serialized funnel.Result — reconstructing a typed *engine.XResult here is
// therefore a DELIBERATE, minimal adapter: the returned Result's Findings
// slice (when populated) exists ONLY so palette.go's existing
// scanSummary/verifySummary/sweepSummary helpers — which only ever call
// len(...) on it — render the identical count-based text Owner mode shows.
// Nothing else may read element fields from these reconstructed slices:
// they are placeholder-length only, not real Finding records.
//
// ReviewPR is NOT wired over the socket in this wave (bugbot-2p8z.4's verb
// table — control.Verbs — deliberately excludes review; see
// internal/control's package doc): it returns errReviewPRNotSupportedOverAttach
// instead of proxying the RPC. review's opts/result shapes (GH client,
// dry-run comment-sync deltas) are a poor fit for the socket's
// count-only DispatchSummary and gh-authenticated network calls are not
// something a remote daemon should perform on a TUI operator's behalf
// sight-unseen; wiring it is left to a future bead if Attach-mode review
// dispatch is wanted.
type controlDispatch struct {
	client *control.Client
}

// errReviewPRNotSupportedOverAttach is returned by controlDispatch.ReviewPR:
// review is not one of the four socket-dispatchable verbs (bugbot-2p8z.4's
// control.Verbs table), so it cannot run against a separately-attached
// daemon in this wave.
var errReviewPRNotSupportedOverAttach = errors.New("tui: review is not supported over a control-socket Attach connection; run `bugbot review` locally or use Owner mode")
var _ dispatcher = (*controlDispatch)(nil)

func newControlDispatch(client *control.Client) *controlDispatch {
	return &controlDispatch{client: client}
}

// Mode implements dispatcher. Attach mode enables dispatch exactly like
// Owner (the palette's gating only checks m.disp == nil), so this reports
// engine.Owner rather than adding a parallel engine.Mode value purely for
// display purposes.
func (c *controlDispatch) Mode() engine.Mode { return engine.Owner }

// ReviewPR implements dispatcher: always fails with
// errReviewPRNotSupportedOverAttach — see controlDispatch's doc comment for
// why review is deliberately not wired over the control socket in this
// wave. ctx and opts are unused (accepted only to satisfy the interface).
func (c *controlDispatch) ReviewPR(_ context.Context, _ engine.ReviewPROpts) (*engine.ReviewPRResult, error) {
	return nil, errReviewPRNotSupportedOverAttach
}

func (c *controlDispatch) Scan(ctx context.Context, opts engine.ScanOpts) (*engine.ScanResult, error) {
	sum, err := c.client.Dispatch(ctx, control.VerbScan, control.DispatchOpts{
		Target: opts.Target, Since: opts.Since, Force: opts.Force,
	})
	if err != nil {
		return nil, err
	}
	if !sum.HasResult {
		return &engine.ScanResult{}, nil
	}
	return &engine.ScanResult{Result: placeholderResult(sum.FindingCount)}, nil
}

func (c *controlDispatch) Verify(ctx context.Context, opts engine.VerifyOpts) (*engine.VerifyResult, error) {
	sum, err := c.client.Dispatch(ctx, control.VerbVerify, control.DispatchOpts{
		Target: opts.Target, Force: opts.Force, Suspected: opts.Suspected,
	})
	if err != nil {
		return nil, err
	}
	if !sum.HasDrain {
		return &engine.VerifyResult{}, nil
	}
	return &engine.VerifyResult{Drain: placeholderResult(sum.FindingCount)}, nil
}

func (c *controlDispatch) Repro(ctx context.Context, opts engine.ReproOpts) (*engine.ReproResult, error) {
	sum, err := c.client.Dispatch(ctx, control.VerbRepro, control.DispatchOpts{
		Target: opts.Target, MaxN: opts.MaxN,
	})
	if err != nil {
		return nil, err
	}
	res := &engine.ReproResult{Skipped: sum.Skipped}
	if sum.HasSummary {
		res.Summary = &repro.Summary{Attempted: sum.Attempted}
	}
	return res, nil
}

func (c *controlDispatch) Sweep(ctx context.Context, opts engine.SweepOpts) (*engine.SweepResult, error) {
	sum, err := c.client.Dispatch(ctx, control.VerbSweep, control.DispatchOpts{
		Target: opts.Target, Force: opts.Force,
	})
	if err != nil {
		return nil, err
	}
	if !sum.HasResult {
		return &engine.SweepResult{}, nil
	}
	return &engine.SweepResult{Result: placeholderResult(sum.FindingCount)}, nil
}

// placeholderResult builds a funnel.Result whose Findings slice has length n
// and nothing else — see controlDispatch's doc comment for why this is safe
// (only len() is ever read from it).
func placeholderResult(n int) *funnel.Result {
	return &funnel.Result{Findings: make([]domain.Finding, n)}
}
