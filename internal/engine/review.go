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

// ReviewPROpts holds the parameters `bugbot review`'s full orchestration
// needs: resolve the PR via gh, verify the local checkout's HEAD matches the
// PR head, run the funnel-backed Targeted scan, plan the gh comment sync
// against the PR's existing bugbot comments, and apply it (unless DryRun).
//
// This is the cobra-free entrypoint internal/cli/review.go delegates to and
// internal/tui's dispatch palette calls directly (via GH: RealGH) for its
// review verb — see bugbot-2p8z.5.
type ReviewPROpts struct {
	Target      string
	PRNumber    int
	Concurrency int
	Refuters    int
	Lenses      []string
	// Suspected controls how T3 suspected findings are surfaced in the
	// summary comment: "summary" (default) or "withhold".
	Suspected string
	// DryRun scans and plans everything but makes no gh write calls (gh
	// reads still run).
	DryRun bool
	// GH is the gh CLI seam. Production callers pass RealGH; tests inject a
	// fake that routes on args and returns canned JSON.
	GH GHRunner
	// Out is the primary human-readable output stream; ErrOut carries
	// warnings (e.g. the sandbox-degraded notice). Dispatch progress and
	// dry-run notices are written here directly — callers (cli, tui) MUST
	// route these through a destination that is safe during their own
	// render loop (never os.Stdout/Stderr from within the TUI's alt-screen).
	Out    io.Writer
	ErrOut io.Writer
}

// ReviewPRResult is the outcome of a full ReviewPR run: the funnel result
// plus everything internal/cli's printReviewSummary needs to render the
// human summary and reviewGateError needs to compute the CI exit gate.
type ReviewPRResult struct {
	Result   *funnel.Result
	PRNumber int
	HeadSHA  string
	DryRun   bool
	// Created/Updated/Skipped/Resolved are the gh comment-sync action tallies.
	Created, Updated, Skipped, Resolved int
	// NewVerifiedCount is the number of tier<=2 findings whose fingerprint was
	// NOT already present on the PR before this run — the CI gate's input.
	NewVerifiedCount int
}

// ReviewPR is the full `bugbot review` orchestration: resolve the PR via gh,
// verify the local checkout's HEAD equals the PR head (CI checks out the
// head; that is the target environment — a mismatch is returned as a plain
// error, never a crash, so a caller like the TUI palette can surface it on
// its status line), run the funnel over the PR's changed files, plan the gh
// comment sync against the PR's existing bugbot comments, and apply it
// (unless DryRun).
func (d *Dispatcher) ReviewPR(ctx context.Context, opts ReviewPROpts) (*ReviewPRResult, error) {
	repo, err := d.openRepo(ctx, opts.Target)
	if err != nil {
		return nil, err
	}

	pr, err := resolvePR(ctx, opts.GH, opts.PRNumber)
	if err != nil {
		return nil, err
	}

	head, err := repo.HeadCommit(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve HEAD: %w", err)
	}
	if head != pr.HeadSHA {
		return nil, fmt.Errorf(
			"local HEAD %s does not match PR #%d head %s; check out the PR head first:\n  git fetch origin pull/%d/head && git checkout %s",
			util.ShortSHA(head), opts.PRNumber, util.ShortSHA(pr.HeadSHA), opts.PRNumber, pr.HeadSHA)
	}

	res, err := d.Review(ctx, ReviewOpts{
		Repo:        repo,
		PRNumber:    opts.PRNumber,
		BaseSHA:     pr.BaseSHA,
		HeadSHA:     pr.HeadSHA,
		Concurrency: opts.Concurrency,
		Refuters:    opts.Refuters,
		Lenses:      opts.Lenses,
		Out:         opts.Out,
		ErrOut:      opts.ErrOut,
	})
	if err != nil {
		return nil, err
	}

	// Commentable RIGHT-side lines, computed locally from the same diff GitHub
	// anchors against.
	diff, err := repo.UnifiedDiff(ctx, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("compute PR diff: %w", err)
	}
	commentable := parseUnifiedDiff(diff)

	existing, err := loadExisting(ctx, opts.GH, opts.PRNumber)
	if err != nil {
		return nil, err
	}

	plan := planSync(res, commentable, existing, pr.HeadSHA, opts.Suspected)

	if err := applyPlan(ctx, opts.GH, opts.PRNumber, pr.HeadSHA, plan, opts.DryRun, opts.Out); err != nil {
		return nil, err
	}

	var created, updated, skipped, resolved int
	for _, a := range plan.actions {
		switch a.op {
		case opCreate:
			created++
		case opUpdate:
			updated++
		case opSkip:
			skipped++
		case opResolve:
			resolved++
		}
	}

	return &ReviewPRResult{
		Result:           res,
		PRNumber:         pr.Number,
		HeadSHA:          pr.HeadSHA,
		DryRun:           opts.DryRun,
		Created:          created,
		Updated:          updated,
		Skipped:          skipped,
		Resolved:         resolved,
		NewVerifiedCount: len(plan.newGateFingerprints),
	}, nil
}

// applyPlan executes the decided sync actions against gh, unless dryRun. Skips
// are no-ops (already counted). Order is stable for deterministic output.
func applyPlan(ctx context.Context, gh GHRunner, pr int, headSHA string, plan planResult, dryRun bool, out io.Writer) error {
	for _, a := range plan.actions {
		if a.op == opSkip {
			continue
		}
		if dryRun {
			_, _ = fmt.Fprintf(out, "[dry-run] would %s %s comment", a.op, kindName(a.kind))
			if a.path != "" {
				_, _ = fmt.Fprintf(out, " at %s:%d", a.path, a.line)
			}
			_, _ = fmt.Fprintln(out)
			continue
		}
		if err := applyAction(ctx, gh, pr, headSHA, a); err != nil {
			return fmt.Errorf("%s %s comment: %w", a.op, kindName(a.kind), err)
		}
	}
	return nil
}

// applyAction performs one gh write for a non-skip action.
func applyAction(ctx context.Context, gh GHRunner, pr int, headSHA string, a syncAction) error {
	switch a.op {
	case opCreate:
		switch a.kind {
		case kindReview:
			_, err := gh(ctx, "api", "-X", "POST",
				fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", pr),
				"-f", "commit_id="+headSHA,
				"-f", "path="+a.path,
				"-F", fmt.Sprintf("line=%d", a.line),
				"-f", "side=RIGHT",
				"-f", "body="+a.body,
			)
			return err
		case kindIssue:
			_, err := gh(ctx, "api", "-X", "POST",
				fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", pr),
				"-f", "body="+a.body,
			)
			return err
		}
	case opUpdate, opResolve:
		endpoint := ""
		switch a.kind {
		case kindReview:
			endpoint = fmt.Sprintf("repos/{owner}/{repo}/pulls/comments/%d", a.id)
		case kindIssue:
			endpoint = fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d", a.id)
		}
		_, err := gh(ctx, "api", "-X", "PATCH", endpoint, "-f", "body="+a.body)
		return err
	}
	return fmt.Errorf("unhandled action op=%v kind=%v", a.op, a.kind)
}

func kindName(k commentKind) string {
	if k == kindReview {
		return "inline"
	}
	return "summary"
}
