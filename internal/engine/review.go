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
// compute Changed; PRNumber/BaseSHA/HeadSHA are used only for the progress
// announcement line.
type ReviewOpts struct {
	Repo        *ingest.Repo
	PRNumber    int
	BaseSHA     string
	HeadSHA     string
	Changed     []string
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
	if err := d.ensureOwner(ctx, false); err != nil {
		return nil, err
	}
	if err := d.ensureRoleClients(ctx); err != nil {
		return nil, err
	}

	_, _ = fmt.Fprintf(opts.Out, "Reviewing PR #%d: %d changed file(s) (%s..%s)\n",
		opts.PRNumber, len(opts.Changed), util.ShortSHA(opts.BaseSHA), util.ShortSHA(opts.HeadSHA))

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

	return f.Targeted(ctx, opts.Changed)
}
