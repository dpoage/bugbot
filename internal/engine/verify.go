package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/dpoage/bugbot/internal/funnel"
)

// VerifyOpts holds the parsed flag values for `bugbot verify`.
type VerifyOpts struct {
	Target    string
	Force     bool
	Suspected bool
	Out       io.Writer
}

// VerifyResult is the outcome of a Dispatcher.Verify call.
type VerifyResult struct {
	Drain     *funnel.Result
	Suspected *funnel.Result // nil unless Suspected was requested
}

// Verify implements `bugbot verify`: a one-shot verify drain that picks up
// pending_candidates left by any interrupted run and verifies them through
// the normal triage+verify pipeline WITHOUT invoking the finder/cartographer.
// This is the same verify-drain logic the daemon runs on its periodic
// verify-drain timer.
//
// Pass Suspected to also re-verify every OPEN Tier-3 suspected finding:
// durable orphans from a hard-budget stop or no-verdict panel that have no
// pending_candidates WAL row. This is a second pass over the same funnel
// pipeline — finder/cartographer remain off — but re-judges the durable T3
// rows against the current code so they can be promoted to Tier 2 or
// dismissed as refuted.
func (d *Dispatcher) Verify(ctx context.Context, opts VerifyOpts) (*VerifyResult, error) {
	// Advisory scan-lock: mirrors `bugbot scan`'s lock so a manual verify does
	// not race the daemon or a concurrent scan.
	if err := d.ensureOwner(ctx, opts.Force); err != nil {
		return nil, err
	}

	cfg := d.cfg
	st := d.store
	out := opts.Out

	// Repo opens before role-client resolution, matching main's order.
	repo, err := d.openRepo(ctx, opts.Target)
	if err != nil {
		return nil, err
	}

	if err := d.ensureRoleClients(ctx); err != nil {
		return nil, err
	}

	// Default `bugbot verify` stays quiet and never writes a status.json
	// snapshot (which would race a running daemon's single-writer snapshot).
	// With Suspected the re-verify pass runs the verifier on every open
	// Tier-3 finding and can take minutes, so the CLI wires a stdout
	// LogRenderer into d.sink for live per-stage / per-finding feedback
	// (nil otherwise) — see internal/cli/verify.go's sink construction.
	funnelOpts, _, sbErr := BuildFunnelOptions(cfg, FunnelOptionOverrides{Progress: d.sink})
	if sbErr != nil {
		return nil, sbErr
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

	res, err := f.VerifyDrain(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify drain: %w", err)
	}

	didDrain := res != nil && (res.Stats.Resumed > 0 || len(res.Findings) > 0)
	if didDrain {
		_, _ = fmt.Fprintf(out,
			"\nVerify drain: %d resumed, %d finding(s) persisted.\n",
			res.Stats.Resumed, len(res.Findings),
		)
	} else {
		_, _ = fmt.Fprintln(out, "Verify drain: no pending candidates.")
	}

	result := &VerifyResult{Drain: res}

	// Suspected: second pass over durable open T3 findings (orphans from a
	// hard-budget stop or no-verdict panel). The finder stays off; the
	// verifier re-judges each durable T3 against current code, promoting
	// survivors to Tier 2 or dismissing refuted ones. Only run when the flag
	// is set so the default behaviour is byte-identical to today.
	if opts.Suspected {
		_, _ = fmt.Fprintln(out,
			"\nRe-verifying open Tier-3 suspected findings (verifier only, no finder; this can take a few minutes)…")
		rres, rerr := f.ReverifySuspected(ctx)
		if rerr != nil {
			return nil, fmt.Errorf("reverify suspected: %w", rerr)
		}
		if rres == nil {
			rres = &funnel.Result{}
		}
		_, _ = fmt.Fprintf(out,
			"Re-verify suspected: %d re-judged, %d verified, %d killed.\n",
			rres.Stats.Resumed, rres.Stats.Verified, rres.Stats.Killed,
		)
		result.Suspected = rres
	}
	return result, nil
}
