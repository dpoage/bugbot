package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// newReviewCmd runs the detection funnel over a pull request's changed code and
// posts adversarially-verified findings as inline PR review comments, with a
// summary comment for everything that cannot be anchored inline.
//
// It resolves the PR base/head via the `gh` CLI, requires the local checkout's
// HEAD to equal the PR head SHA (CI checks out the head; that is the target
// environment), runs the same blast-radius Targeted scan as `bugbot scan
// --since`, and reconciles its comments against the PR on every run: new
// findings are created, changed findings updated, unchanged findings skipped,
// and findings no longer reported are marked resolved (never deleted).
//
// Exit code mirrors scan.go: nonzero when most finders failed to parse
// (precedence), and — when review.fail_on=verified — nonzero when the run
// surfaces a NEW Tier<=2 finding not already present on the PR, so CI fails on
// genuinely new bugs but not on re-posts of already-known ones.
func newReviewCmd() *cobra.Command {
	var (
		target      string
		prNumber    int
		concurrency int
		refuters    int
		lenses      []string
		failOn      string
		suspected   string
		dryRun      bool
	)

	cmd := &cobra.Command{
		Use:   "review --pr N [flags]",
		Short: "Review a pull request: scan its changed code and post verified findings as PR comments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if prNumber <= 0 {
				return fmt.Errorf("--pr is required and must be a positive PR number")
			}

			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}

			// Flags override config; config defaults are normalized by Default().
			reviewCfg := resolveReviewConfig(cfg.Review, failOn, suspected)
			if err := validateReviewFlags(reviewCfg); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			gh := realGH

			run, err := executeReview(ctx, reviewParams{
				cfg:         cfg,
				reviewCfg:   reviewCfg,
				target:      target,
				prNumber:    prNumber,
				concurrency: concurrency,
				refuters:    refuters,
				lenses:      lenses,
				dryRun:      dryRun,
				gh:          gh,
				out:         out,
			})
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}

			if gateErr := reviewGateError(run, reviewCfg.failOn, prNumber); gateErr != nil {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
				return gateErr
			}
			return nil
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().IntVar(&prNumber, "pr", 0, "pull request number to review (required)")
	cmd.Flags().IntVar(&concurrency, "concurrency", funnel.DefaultMaxParallel, "number of parallel agents")
	cmd.Flags().IntVar(&refuters, "refuters", funnel.DefaultRefuters, "number of adversarial refuter agents per candidate")
	cmd.Flags().StringSliceVar(&lenses, "lens", nil, "restrict finder lenses (repeatable); default is all built-in lenses")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "CI exit gate: verified|none (overrides config review.fail_on)")
	cmd.Flags().StringVar(&suspected, "suspected", "", "how to surface T3 suspected findings: summary|withhold (overrides config review.suspected)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "scan and render everything but make no gh write calls (gh reads still run)")

	return cmd
}

// reviewConfig is the resolved (flag-over-config) review behavior.
type reviewConfig struct {
	failOn    string
	suspected string
}

// resolveReviewConfig overlays CLI flags onto config, defaulting empties.
func resolveReviewConfig(cfg config.Review, failOnFlag, suspectedFlag string) reviewConfig {
	rc := reviewConfig{failOn: cfg.FailOn, suspected: cfg.Suspected}
	if failOnFlag != "" {
		rc.failOn = failOnFlag
	}
	if suspectedFlag != "" {
		rc.suspected = suspectedFlag
	}
	if rc.failOn == "" {
		rc.failOn = "verified"
	}
	if rc.suspected == "" {
		rc.suspected = "summary"
	}
	return rc
}

// validateReviewFlags rejects unknown flag/config values up front.
func validateReviewFlags(rc reviewConfig) error {
	switch rc.failOn {
	case "verified", "none":
	default:
		return fmt.Errorf("--fail-on %q invalid (want verified or none)", rc.failOn)
	}
	switch rc.suspected {
	case "summary", "withhold":
	default:
		return fmt.Errorf("--suspected %q invalid (want summary or withhold)", rc.suspected)
	}
	return nil
}

// reviewParams bundles everything executeReview needs, so the RunE closure stays
// thin and the orchestration is testable with a fake gh.
type reviewParams struct {
	cfg         config.Config
	reviewCfg   reviewConfig
	target      string
	prNumber    int
	concurrency int
	refuters    int
	lenses      []string
	dryRun      bool
	gh          ghRunner
	out         io.Writer
}

// reviewRun is the outcome executeReview reports to the gate logic.
type reviewRun struct {
	result           *funnel.Result
	newVerifiedCount int
}

// executeReview is the full orchestration: resolve PR, verify HEAD, scan the
// blast radius, compute commentable lines, plan the comment sync, and apply it
// (unless dry-run). It is separated from the cobra wiring so tests can drive it
// with a fake gh and a real local repo.
func executeReview(ctx context.Context, p reviewParams) (reviewRun, error) {
	repo, err := ingest.Open(ctx, p.target)
	if err != nil {
		return reviewRun{}, fmt.Errorf("open target: %w", err)
	}

	pr, err := resolvePR(ctx, p.gh, p.prNumber)
	if err != nil {
		return reviewRun{}, err
	}

	head, err := repo.HeadCommit(ctx)
	if err != nil {
		return reviewRun{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	if head != pr.HeadSHA {
		return reviewRun{}, fmt.Errorf(
			"local HEAD %s does not match PR #%d head %s; check out the PR head first:\n  git fetch origin pull/%d/head && git checkout %s",
			util.ShortSHA(head), p.prNumber, util.ShortSHA(pr.HeadSHA), p.prNumber, pr.HeadSHA)
	}

	res, err := runReviewScan(ctx, repo, p, pr)
	if err != nil {
		return reviewRun{}, err
	}

	// Commentable RIGHT-side lines, computed locally from the same diff GitHub
	// anchors against.
	diff, err := repo.UnifiedDiff(ctx, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return reviewRun{}, fmt.Errorf("compute PR diff: %w", err)
	}
	commentable := parseUnifiedDiff(diff)

	existing, err := loadExisting(ctx, p.gh, p.prNumber)
	if err != nil {
		return reviewRun{}, err
	}

	plan := planSync(res, commentable, existing, pr.HeadSHA, p.reviewCfg.suspected)

	if err := applyPlan(ctx, p.gh, p.prNumber, pr.HeadSHA, plan, p.dryRun, p.out); err != nil {
		return reviewRun{}, err
	}

	printReviewSummary(p.out, res, plan, pr, p.dryRun)

	return reviewRun{result: res, newVerifiedCount: len(plan.newGateFingerprints)}, nil
}

// reviewGateError computes the CI exit gate. The reliability gate takes
// precedence: an unreliable run (most finders failed to parse) must never be
// read as a clean pass, so it errors regardless of failOn. Otherwise, when
// failOn=="verified", a run that surfaced at least one NEW Tier<=2 finding
// (fingerprint not already present on the PR) errors so CI fails on genuinely
// new bugs; re-posts of already-known findings and failOn=="none" return nil.
func reviewGateError(run reviewRun, failOn string, prNumber int) error {
	if run.result.Stats.MostFindersFailed() {
		return fmt.Errorf("review unreliable: %d of %d finder agents produced no parseable output",
			run.result.Stats.FinderFailures, run.result.Stats.FinderRuns)
	}
	if failOn == "verified" && run.newVerifiedCount > 0 {
		return fmt.Errorf("PR review gate: %d new verified finding(s) on PR #%d", run.newVerifiedCount, prNumber)
	}
	return nil
}

// runReviewScan wires the funnel exactly like scan.go and runs a Targeted scan
// over the PR's changed files (blast radius applied internally by the funnel).
func runReviewScan(ctx context.Context, repo *ingest.Repo, p reviewParams, pr prInfo) (*funnel.Result, error) {
	st, err := store.Open(ctx, p.cfg.Storage.Path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer closeStore(st)

	finder, verifier, cartographer, err := buildRoleClients(ctx, &p.cfg)
	if err != nil {
		return nil, err
	}

	// PR base..head changed files drive the targeted scan; the funnel expands the
	// blast radius and intersects with scan scope internally.
	changes, err := repo.ChangedFiles(ctx, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("diff %s..%s: %w", util.ShortSHA(pr.BaseSHA), util.ShortSHA(pr.HeadSHA), err)
	}
	changed := ingest.ChangedPaths(changes)
	_, _ = fmt.Fprintf(p.out, "Reviewing PR #%d: %d changed file(s) (%s..%s)\n",
		p.prNumber, len(changed), util.ShortSHA(pr.BaseSHA), util.ShortSHA(pr.HeadSHA))

	opts, sbDegraded, sbErr := buildFunnelOptions(p.cfg, FunnelOptionOverrides{
		Lenses:      p.lenses,
		Refuters:    p.refuters,
		MaxParallel: p.concurrency,
		Progress:    progress.NewLogRenderer(p.out),
	})
	if sbErr != nil {
		return nil, sbErr
	}
	if sbDegraded {
		printSandboxDegradedWarning(p.out)
	}
	f, err := funnel.New(funnel.RoleClients{Finder: finder, Verifier: verifier, Cartographer: cartographer}, st, repo, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return f.Targeted(ctx, changed)
}

// applyPlan executes the decided sync actions against gh, unless dryRun. Skips
// are no-ops (already counted). Order is stable for deterministic output.
func applyPlan(ctx context.Context, gh ghRunner, pr int, headSHA string, plan planResult, dryRun bool, out io.Writer) error {
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
func applyAction(ctx context.Context, gh ghRunner, pr int, headSHA string, a syncAction) error {
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

// printReviewSummary writes a human-readable account of what the run did: scan
// outcome and the create/update/skip/resolve tallies.
func printReviewSummary(out io.Writer, res *funnel.Result, plan planResult, pr prInfo, dryRun bool) {
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

	_, _ = fmt.Fprintf(out, "\nReview complete for PR #%d (head %s)\n", pr.Number, util.ShortSHA(pr.HeadSHA))
	_, _ = fmt.Fprintf(out, "Findings surfaced: %d (new verified: %d)\n", len(res.Findings), len(plan.newGateFingerprints))
	verb := "Comments"
	if dryRun {
		verb = "Comments (dry-run, nothing posted)"
	}
	_, _ = fmt.Fprintf(out, "%s: created=%d updated=%d unchanged=%d resolved=%d\n", verb, created, updated, skipped, resolved)

	if !res.Stats.FinderReliable() {
		_, _ = fmt.Fprintf(out, "\n%s\n", reliabilityWarning(res.Stats))
	}
}
