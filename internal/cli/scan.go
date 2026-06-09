package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// newScanCmd runs a single pass of the detection funnel over a target repo. It
// loads config, opens the state store and the target repository, builds the
// finder/verifier LLM clients from the role mappings, runs the funnel (a whole-
// snapshot Sweep, or a blast-radius-scoped Targeted scan when --since is given),
// and prints a human summary of the findings, per-stage counts, and spend.
//
// Exit code is always 0 for now: findings are surfaced in the output (report
// sinks come in a later stage). The findings count is printed so callers can
// detect "found something" by parsing the summary.
func newScanCmd() *cobra.Command {
	var (
		target      string
		since       string
		includeT3   bool
		concurrency int
		refuters    int
		lenses      []string
	)

	cmd := &cobra.Command{
		Use:   "scan [flags]",
		Short: "Run the detection funnel once over a target repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			st, err := store.Open(ctx, cfg.Storage.Path)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer func() { _ = st.Close() }()

			repo, err := ingest.Open(ctx, target)
			if err != nil {
				return fmt.Errorf("open target: %w", err)
			}

			finder, err := llm.ResolveRole(ctx, &cfg, "finder", llm.Options{})
			if err != nil {
				return fmt.Errorf("build finder client: %w", err)
			}
			verifier, err := llm.ResolveRole(ctx, &cfg, "verifier", llm.Options{})
			if err != nil {
				return fmt.Errorf("build verifier client: %w", err)
			}

			opts := funnel.Options{
				Lenses:      lenses,
				Refuters:    refuters,
				MaxParallel: concurrency,
				TokenBudget: cfg.Budgets.PerCycleTokens,
			}
			f, err := funnel.New(funnel.RoleClients{Finder: finder, Verifier: verifier}, st, repo, opts)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			var res *funnel.Result
			if since != "" {
				head, herr := repo.HeadCommit(ctx)
				if herr != nil {
					return fmt.Errorf("resolve HEAD: %w", herr)
				}
				changes, cerr := repo.ChangedFiles(ctx, since, head)
				if cerr != nil {
					return fmt.Errorf("diff %s..HEAD: %w", since, cerr)
				}
				changed := ingest.ChangedPaths(changes)
				fmt.Fprintf(out, "Targeted scan: %d changed file(s) since %s\n", len(changed), since)
				res, err = f.Targeted(ctx, changed)
			} else {
				res, err = f.Sweep(ctx)
			}
			if err != nil {
				return err
			}

			_ = includeT3 // reserved: this stage emits T2 only; T3 filtering arrives with the report stage
			printResult(out, res)
			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", ".", "path to the target repository")
	cmd.Flags().StringVar(&since, "since", "", "scan only the blast radius of changes since this commit (targeted scan)")
	cmd.Flags().BoolVar(&includeT3, "include-suspected", false, "include T3 (suspected) findings in output (reserved; this stage emits T2)")
	cmd.Flags().IntVar(&concurrency, "concurrency", funnel.DefaultMaxParallel, "number of parallel agents")
	cmd.Flags().IntVar(&refuters, "refuters", funnel.DefaultRefuters, "number of adversarial refuter agents per candidate")
	cmd.Flags().StringSliceVar(&lenses, "lens", nil, "restrict finder lenses (repeatable); default is all built-in lenses")

	return cmd
}

// printResult writes a human-readable summary of a funnel run: a findings table
// (tier, severity, file:line, title), per-stage counts, token spend, and any
// degradation/skip notes.
func printResult(out io.Writer, res *funnel.Result) {
	fmt.Fprintf(out, "\nScan complete (commit %s)\n", shortSHA(res.Commit))

	if len(res.Findings) == 0 {
		fmt.Fprintln(out, "\nNo findings.")
	} else {
		fmt.Fprintf(out, "\n%d finding(s):\n\n", len(res.Findings))
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TIER\tSEVERITY\tLOCATION\tTITLE")
		for _, fnd := range res.Findings {
			fmt.Fprintf(tw, "T%d\t%s\t%s:%d\t%s\n",
				fnd.Tier, fnd.Severity, fnd.File, fnd.Line, fnd.Title)
		}
		_ = tw.Flush()
	}

	s := res.Stats
	fmt.Fprintf(out, "\nStages: hypothesized=%d triaged=%d verified=%d killed=%d\n",
		s.Hypothesized, s.Triaged, s.Verified, s.Killed)
	fmt.Fprintf(out, "Triage drops: low_confidence=%d duplicate=%d suppressed=%d out_of_scope=%d\n",
		s.DroppedLowConfidence, s.DroppedDuplicate, s.DroppedSuppressed, s.DroppedOutOfScope)
	fmt.Fprintf(out, "Spend: input=%d output=%d total=%d tokens\n",
		s.InputTokens, s.OutputTokens, s.InputTokens+s.OutputTokens)

	if res.Degraded || res.Stopped {
		fmt.Fprintf(out, "Budget: degraded=%v stopped=%v\n", res.Degraded, res.Stopped)
	}
	for _, note := range res.Skipped {
		fmt.Fprintf(out, "  skipped: %s\n", note)
	}
}

// shortSHA abbreviates a commit SHA for display, leaving short/empty values
// untouched.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
