package cli

import (
	"context"
	"encoding/json"
	"fmt"

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
	var asJSON bool

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

Recorded-mode evaluation (replaying captured real-model transcripts as
regression fixtures) is driven separately via the recording workflow documented
in internal/eval/README.md; it is not exposed by this command.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			res, err := eval.RunSuite(ctx, eval.BuiltinCases(), eval.ModeScripted)
			if err != nil {
				return fmt.Errorf("run eval suite: %w", err)
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return fmt.Errorf("encode eval result: %w", err)
				}
			} else {
				fmt.Fprintln(out, res.String())
			}

			// Enforce the shared precision gate; a violation exits non-zero.
			return eval.Gate(res)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the suite result as machine-readable JSON instead of a table")

	return cmd
}
