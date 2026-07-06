package engine

import (
	"context"
	"fmt"
	"io"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/util"
)

// ReviewOpts holds the parameters `bugbot review` needs to run its Targeted
// scan over a pull request's changed files. Repo is accepted rather than
// opened internally because the caller (internal/cli/review.go) must already
// have it open to resolve the PR's base/head SHAs via `gh` before it can even
// call Review. Changed files are computed inside Review (from BaseSHA/HeadSHA)
// AFTER role-client resolution, matching main's buildRoleClients-then-diff
// order.
type ReviewOpts struct {
	Repo        *ingest.Repo
	PRNumber    int
	BaseSHA     string
	HeadSHA     string
	Concurrency int
	Refuters    int
	Lenses      []string
	Out         io.Writer
	ErrOut      io.Writer
}

// Review runs the same blast-radius Targeted scan `bugbot scan --since`
// runs, scoped to a pull request's changed files (the funnel expands the
// blast radius and intersects with scan scope internally). It does not touch
// GitHub: resolving the PR, verifying HEAD, diffing for commentable lines,
// and posting/reconciling comments are gh-specific orchestration that stays
// in internal/cli/review.go (ghrunner.go / prcomments.go / prdiff.go), which
// call Review to obtain the funnel.Result they render and sync against gh.
func (d *Dispatcher) Review(ctx context.Context, opts ReviewOpts) (*funnel.Result, error) {
	// main's `bugbot review` had no advisory-lock gate either — runReviewScan
	// opened the store (flock) unconditionally. force=true mirrors that: see
	// Dispatcher.Repro's comment for the full rationale.
	if err := d.ensureOwner(ctx, true); err != nil {
		return nil, err
	}
	if err := d.ensureRoleClients(ctx); err != nil {
		return nil, err
	}

	// PR base..head changed files drive the targeted scan; the funnel expands
	// the blast radius and intersects with scan scope internally. Computed
	// here (after role clients), matching main's runReviewScan order.
	changes, err := opts.Repo.ChangedFiles(ctx, opts.BaseSHA, opts.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("diff %s..%s: %w", util.ShortSHA(opts.BaseSHA), util.ShortSHA(opts.HeadSHA), err)
	}
	changed := ingest.ChangedPaths(changes)

	_, _ = fmt.Fprintf(opts.Out, "Reviewing PR #%d: %d changed file(s) (%s..%s)\n",
		opts.PRNumber, len(changed), util.ShortSHA(opts.BaseSHA), util.ShortSHA(opts.HeadSHA))

	funnelOpts, sbDegraded, sbErr := BuildFunnelOptions(d.cfg, FunnelOptionOverrides{
		Lenses:      opts.Lenses,
		Refuters:    opts.Refuters,
		MaxParallel: opts.Concurrency,
		Progress:    d.sink,
	})
	if sbErr != nil {
		return nil, sbErr
	}
	if sbDegraded {
		PrintSandboxDegradedWarning(opts.ErrOut)
	}
	f, err := funnel.New(funnel.RoleClients{Finder: d.finder, Verifier: d.verifier, Cartographer: d.cartographer, Arbiter: d.arbiter}, d.store, opts.Repo, funnelOpts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return f.Targeted(ctx, changed)
}
