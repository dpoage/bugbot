package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// newRegressCmd implements `bugbot regress --from A [--to B]`: it scopes a
// targeted scan to the blast radius of the A..B commit range and then labels
// each finding INTRODUCED (anchor absent at A) vs PRE-EXISTING (anchor present
// at A). This is PART-A of the bugbot-ovt bead: the one-shot CLI surface and
// its attribution pass. The DAEMON-side "new since last green" digest is
// PART-B and lives in internal/daemon (deferred; out of scope here).
//
// The run IS a blast-radius scan over A..B (exactly like `scan --since` but
// with a two-ended range) followed by an attribution pass; we reuse
// runScanCmd's body rather than duplicating it. Reusing runScanCmd also means
// the regress command automatically inherits every future bug-fix / change to
// the scan pipeline (analyzer seeding, doc-contradiction seeding, fixpoint
// drain, reliability gating, repro catch-up, etc.).
//
// The command surface mirrors newScanCmd so a user who knows `bugbot scan`
// already knows `bugbot regress`: same scan-tuning flags (--concurrency,
// --refuters, --lens, --repro), same `--target`. The single new requirement
// is --from; --to defaults to HEAD (empty string in flags is normalized to
// HEAD inside runScanCmd's scope resolver).
func newRegressCmd() *cobra.Command {
	var flags ScanFlags

	cmd := &cobra.Command{
		Use:   "regress [flags]",
		Short: "Find bugs introduced within a commit range (A..B)",
		Long: `regress runs a targeted scan over the blast radius of a commit range
and labels each finding INTRODUCED vs PRE-EXISTING.

INTRODUCED: the finding's anchored file:line did not exist at --from (the
file was absent, or the line was beyond the file's EOF at --from). The bug
was almost certainly introduced by something in --from..--to.

PRE-EXISTING: the finding's anchored file:line existed at --from. The bug is
older than the range and was surfaced incidentally by the blast-radius
re-investigation; treat it as background context, not a regression of the
range.

--from is required; --to defaults to HEAD when omitted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.From == "" {
				return fmt.Errorf("regress requires --from <commit>")
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Same SIGINT/SIGTERM wiring as newScanCmd: surface cancellation
			// to the scan run so it can seal the run row as interrupted
			// rather than leaving it dangling.
			ctx, stopSignal := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stopSignal()
			return runScanCmd(ctx, cmd, flags)
		},
	}

	addTargetFlag(cmd, &flags.Target)
	cmd.Flags().StringVar(&flags.From, "from", "", "inclusive lower bound of the commit range (required)")
	cmd.Flags().StringVar(&flags.To, "to", "", "upper bound of the commit range; defaults to HEAD when empty")
	cmd.Flags().IntVar(&flags.Concurrency, "concurrency", funnel.DefaultMaxParallel, "number of parallel agents")
	cmd.Flags().IntVar(&flags.Refuters, "refuters", funnel.DefaultRefuters, "number of adversarial refuter agents per candidate")
	cmd.Flags().StringSliceVar(&flags.Lenses, "lens", nil, "restrict finder lenses (repeatable); default is all built-in lenses")
	cmd.Flags().BoolVar(&flags.DoRepro, "repro", false, "run the Reproduce stage: generate sandboxed failing tests and promote demonstrated findings to Tier-1")
	cmd.Flags().BoolVar(&flags.Force, "force", false, "bypass the advisory single-scan lock and proceed even if another scan appears active")

	return cmd
}

// printRegressAttribution emits a human-readable INTRODUCED / PRE-EXISTING
// attribution block following the regular scan summary, plus a summary
// count line. The block is empty (no section header) when there are no
// findings, so a clean regress run stays terse.
//
// The label for each finding is computed by repo.AnchorAbsentAtRef: a file
// absent at --from, OR an anchored line beyond the file's EOF at --from, is
// INTRODUCED. Otherwise it is PRE-EXISTING.
//
// Errors while probing individual anchors are swallowed (a transient repo
// issue cannot abort the summary): such findings are conservatively labeled
// INTRODUCED because "we cannot prove it is pre-existing" is not the same
// evidence as "we proved it was present at --from" — putting them in the
// same bucket as genuinely-introduced findings matches what an operator
// wants: "things to investigate first".
func printRegressAttribution(ctx context.Context, out io.Writer, repo *ingest.Repo, findings []store.Finding, fromRef string) {
	if len(findings) == 0 {
		return
	}
	type labeled struct {
		f     store.Finding
		intro bool
	}
	labeledFindings := make([]labeled, 0, len(findings))
	introCount := 0
	for _, fnd := range findings {
		intro := repo.AnchorAbsentAtRef(ctx, fromRef, fnd.File, fnd.Line)
		if intro {
			introCount++
		}
		labeledFindings = append(labeledFindings, labeled{f: fnd, intro: intro})
	}
	// INTRODUCED first so a reader scanning top-to-bottom sees the new bugs
	// before pre-existing background. Stable, so input order is preserved
	// within each label group.
	sort.SliceStable(labeledFindings, func(i, j int) bool {
		return labeledFindings[i].intro && !labeledFindings[j].intro
	})

	_, _ = fmt.Fprintln(out, "\nRegress attribution (vs "+fromRef+"):")
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, lf := range labeledFindings {
		label := "PRE-EXISTING"
		if lf.intro {
			label = "INTRODUCED "
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s:%d\tT%d %s\t%s\n",
			label, lf.f.File, lf.f.Line, lf.f.Tier, lf.f.Severity, lf.f.Title)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintf(out, "\n%d introduced, %d pre-existing since %s\n",
		introCount, len(findings)-introCount, fromRef)
}
