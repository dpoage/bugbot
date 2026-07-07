package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/progress"
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
//
// The full orchestration (resolve PR, verify HEAD, funnel, plan the gh
// comment sync, apply it) lives in engine.Dispatcher.ReviewPR; this command is
// a thin flag-parsing / presentation layer over it, so internal/tui's dispatch
// palette can drive the same orchestration in-process.
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
			errOut := cmd.ErrOrStderr()

			d, err := engine.Open(ctx, cfg, progress.NewLogRenderer(errOut))
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()

			res, err := d.ReviewPR(ctx, engine.ReviewPROpts{
				Target:      target,
				PRNumber:    prNumber,
				Concurrency: concurrency,
				Refuters:    refuters,
				Lenses:      lenses,
				Suspected:   reviewCfg.suspected,
				DryRun:      dryRun,
				GH:          engine.RealGH,
				Out:         out,
				ErrOut:      errOut,
			})
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}

			printReviewSummary(out, res)

			run := reviewRun{result: res.Result, newVerifiedCount: res.NewVerifiedCount}
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

// reviewRun is the outcome engine.ReviewPR reports to the gate logic.
type reviewRun struct {
	result           *funnel.Result
	newVerifiedCount int
}

// reviewGateError computes the CI exit gate. The reliability gate takes
// precedence: an unreliable run (most finders failed to parse) must never be
// read as a clean pass, so it errors regardless of failOn. Otherwise, when
// failOn=="verified", a run that surfaced at least one NEW Tier<=2 finding
// (fingerprint not already present on the PR) errors so CI fails on genuinely
// new bugs; re-posts of already-known findings and failOn=="none" return nil.
func reviewGateError(run reviewRun, failOn string, prNumber int) error {
	if run.result.Stats.MostFindersFailed() {
		return newGateError(fmt.Sprintf("review unreliable: %d of %d finder agents produced no parseable output",
			run.result.Stats.FinderFailures, run.result.Stats.FinderRuns))
	}
	if failOn == "verified" && run.newVerifiedCount > 0 {
		return newGateError(fmt.Sprintf("PR review gate: %d new verified finding(s) on PR #%d", run.newVerifiedCount, prNumber))
	}
	return nil
}

// printReviewSummary writes a human-readable account of what the run did:
// scan outcome and the create/update/skip/resolve tallies.
func printReviewSummary(out io.Writer, res *engine.ReviewPRResult) {
	_, _ = fmt.Fprintf(out, "\nReview complete for PR #%d (head %s)\n", res.PRNumber, util.ShortSHA(res.HeadSHA))
	_, _ = fmt.Fprintf(out, "Findings surfaced: %d (new verified: %d)\n", len(res.Result.Findings), res.NewVerifiedCount)
	verb := "Comments"
	if res.DryRun {
		verb = "Comments (dry-run, nothing posted)"
	}
	_, _ = fmt.Fprintf(out, "%s: created=%d updated=%d unchanged=%d resolved=%d\n", verb, res.Created, res.Updated, res.Skipped, res.Resolved)

	if !res.Result.Stats.FinderReliable() {
		_, _ = fmt.Fprintf(out, "\n%s\n", reliabilityWarning(res.Result.Stats))
	}
}
