package cli

import (
	"bytes"
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
// The label for each finding is computed by anchorAbsentAtRef: a file absent
// at --from, OR an anchored line beyond the file's EOF at --from, is
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
		intro := anchorAbsentAtRef(ctx, repo, fnd.File, fnd.Line, fromRef)
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

// anchorAbsentAtRef reports whether the given file:line anchor did not exist
// at the given git ref. The regress attribution pass labels such anchors
// INTRODUCED (the file was added or the line was added by something in the
// range). The function returns true (i.e. "absent at ref") in three cases:
//
//  1. The file is absent at ref (git exits non-zero on "show <ref>:<path>"
//     for a path that was never tracked at <ref>).
//  2. The anchored line is past the file's EOF at ref (line < 1 or
//     line > number-of-lines at ref).
//  3. Any other transient git error (treat conservatively as "absent" so
//     attribution does not produce spurious PRE-EXISTING labels when the
//     repo is briefly unwritable). The summary still shows the finding in
//     the INTRODUCED bucket alongside genuinely-introduced findings, which
//     matches what an operator wants: "things to investigate first".
//
// The "absent" reading is the load-bearing case for `bugbot regress`:
// findings the funnel produces for a file that did not yet exist at the
// range's base commit are, by definition, regressions introduced within the
// range. A finding anchored to a line that did not yet exist at the base
// commit but whose file did is the same story at finer granularity — code
// past EOF at base is code that was added in the range.
func anchorAbsentAtRef(ctx context.Context, repo *ingest.Repo, file string, line int, ref string) bool {
	if repo == nil || file == "" || ref == "" {
		// Defensive: a nil repo or empty anchor is treated as "absent at ref"
		// so the caller's labelling is conservative (prefer false-positive
		// INTRODUCED over false-positive PRE-EXISTING).
		return true
	}
	content, err := repo.ReadFileAtRef(ctx, ref, file)
	if err != nil {
		// File absent at ref (or any other git error). Either way, label as
		// INTRODUCED — the regress attribution deliberately swallows errors
		// so a transient git problem cannot flip a finding into the
		// PRE-EXISTING bucket by accident.
		return true
	}
	if line < 1 {
		return true
	}
	// Split on newline. The line-count convention used by editors and bug
	// reporters is 1-indexed: line 1 is the first line of the file. We split
	// on '\n' so a file without a trailing newline still has its last line
	// present (e.g. "a\nb" splits to ["a", "b"], len=2, line 2 is valid). A
	// trailing newline adds a spurious empty element to the split; we drop it
	// so "a\nb\n" splits to ["a", "b"] (2 lines), not ["a", "b", ""] (3).
	lines := bytes.Split(content, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	if line > len(lines) {
		return true
	}
	return false
}
