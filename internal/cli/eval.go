package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/eval"
)

// newEvalCmd runs Bugbot's built-in offline detection benchmark suite in
// scripted mode and enforces the precision gate.
//
// Scripted mode embeds its finder/verifier behavior in the cases themselves
// (see eval.BuiltinCases), so this command needs NO config file, NO API keys,
// and makes NO LLM calls. It is the same suite as the TestBenchmarkSuite
// regression test and shares the exact same gate (eval.Gate), so the CLI and CI
// never disagree on what "passing" means.
//
// Exit code is non-zero when the gate fails: any clean-code case reports a false
// positive, or aggregate precision drops below 1.0.
func newEvalCmd() *cobra.Command {
	var (
		asJSON    bool
		recorded  bool
		corpusDir string
		dupPairs  bool
	)

	cmd := &cobra.Command{
		Use:   "eval [flags]",
		Short: "Run the offline detection benchmark suite (scripted mode)",
		Long: `Run Bugbot's built-in detection benchmark suite in scripted mode.

Scripted mode drives the real funnel with per-case scripted clients embedded in
the cases, so this command needs no config file, no API keys, and makes no LLM
calls. It prints a per-case table plus an aggregate line.

The command exits non-zero when the precision gate fails:
  - any clean-code case reports a false positive, or
  - aggregate precision drops below 1.0.

This is the same suite and gate as the TestBenchmarkSuite regression test.

With --recorded, the command instead REPLAYS the committed real-model transcript
corpus (internal/eval/testdata/recorded) through the current pipeline in recorded
mode and prints the resulting table. This makes NO LLM calls — it replays saved
sessions. The recorded numbers are a MEASUREMENT of a real model, not an
invariant, so --recorded does NOT apply the precision gate: it exits non-zero
only on a replay/divergence error (a transcript that no longer drives the
pipeline, or a malformed corpus), never on a "low" precision. When no corpus
exists it prints a clear message and exits zero (the corpus is optional and is
captured out-of-band via 'go test -tags record').

With --dup-pairs, the command instead runs the labeled duplicate-pair corpus
(internal/eval.BuiltinDupPairs, covering the paraphrase, cross-lens,
caller/callee, and rename duplicate channels) through the CURRENT identity
layer's cross-scan similarity decision (funnel.SimilarFinding) and prints a
per-channel precision/recall table. This makes NO LLM calls and applies NO
gate — the labels are ground truth, not what current code does, so a low
recall here is an expected baseline measurement (bugbot-ezmx.8), not a
failure. --dup-pairs takes precedence over --recorded when both are set.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			out := cmd.OutOrStdout()

			if dupPairs {
				return runDupPairEval(out, asJSON)
			}

			if recorded {
				return runRecordedEval(ctx, out, corpusDir, asJSON)
			}

			res, err := eval.RunSuite(ctx, eval.BuiltinCases(), eval.ModeScripted)
			if err != nil {
				return fmt.Errorf("run eval suite: %w", err)
			}

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return fmt.Errorf("encode eval result: %w", err)
				}
			} else {
				_, _ = fmt.Fprintln(out, res.String())
			}

			// Enforce the shared precision gate; a violation exits non-zero.
			return eval.Gate(res)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the suite result as machine-readable JSON instead of a table")
	cmd.Flags().BoolVar(&recorded, "recorded", false, "replay the committed real-model transcript corpus (recorded mode) instead of scripted mode; prints the table without the precision gate")
	cmd.Flags().StringVar(&corpusDir, "corpus", eval.DefaultRecordedDir, "directory holding the recorded-mode transcript corpus (used with --recorded)")
	cmd.Flags().BoolVar(&dupPairs, "dup-pairs", false, "run the labeled duplicate-pair corpus through the identity layer (offline, no LLM calls) and report precision/recall of the duplicate decision, instead of the detection suite")

	return cmd
}

// runRecordedEval replays the committed real-model corpus in recorded mode and
// prints the table WITHOUT the precision gate. A real model's precision is a
// measurement, not an invariant, so the only failures here are replay/divergence
// errors. A missing corpus is reported and treated as success (the corpus is
// optional).
func runRecordedEval(ctx context.Context, out io.Writer, corpusDir string, asJSON bool) error {
	cases, err := eval.LoadRecordedCases(corpusDir)
	if err != nil {
		return fmt.Errorf("load recorded corpus %q: %w", corpusDir, err)
	}
	if len(cases) == 0 {
		_, _ = fmt.Fprintf(out, "no recorded corpus at %q; capture one with `go test -tags record ./internal/eval/ -run TestRecordCorpus` and the LLM_LIVE_* environment variables.\n", corpusDir)
		return nil
	}

	res, err := eval.RunSuite(ctx, cases, eval.ModeRecorded)
	if err != nil {
		return fmt.Errorf("replay recorded suite: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("encode eval result: %w", err)
		}
		return nil
	}
	_, _ = fmt.Fprintln(out, res.String())
	_, _ = fmt.Fprintln(out, "(recorded mode: scores are a measurement of a real model, not a gated invariant)")
	return nil
}

// runDupPairEval scores internal/eval.BuiltinDupPairs against the current
// identity layer's cross-scan duplicate decision (eval.RunDupEval, backed by
// funnel.SimilarFinding) and prints the per-channel precision/recall table.
// Pure function, no I/O beyond the corpus in memory, so this can never fail —
// error return is kept for symmetry with the other eval entrypoints and future
// JSON-encode failures.
func runDupPairEval(out io.Writer, asJSON bool) error {
	res := eval.RunDupEval(eval.BuiltinDupPairs())
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return fmt.Errorf("encode dup-pair eval result: %w", err)
		}
		return nil
	}
	_, _ = fmt.Fprintln(out, res.String())
	_, _ = fmt.Fprintln(out, "(dup-pairs mode: measures the current identity layer against labeled ground truth; not a gated invariant)")
	return nil
}
