package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/sarif"
	"github.com/dpoage/bugbot/internal/store"
)

// newExportCmd implements `bugbot export --format sarif [--output FILE]`.
// It loads all findings from the store and marshals them as SARIF 2.1.0.
func newExportCmd() *cobra.Command {
	var (
		format string
		output string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export findings to an interchange format",
		Long: `Export findings from the store to an interchange format.

Currently supported formats:
  sarif  SARIF 2.1.0 (suitable for GitHub Code Scanning upload)

By default the SARIF document is written to stdout; use --output to write to a
file instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "sarif" {
				return fmt.Errorf("unsupported --format %q (supported: sarif)", format)
			}

			ctx := cmd.Context()
			_, st, err := cmdOpenStore(ctx)
			if err != nil {
				return err
			}
			defer closeStore(st)

			findings, err := st.ListFindings(ctx, store.FindingFilter{})
			if err != nil {
				return fmt.Errorf("list findings: %w", err)
			}

			doc := sarif.FromFindings(findings)
			data, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal sarif: %w", err)
			}
			data = append(data, '\n')

			if output == "" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(output, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", output, err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&format, "format", "sarif", "output format (sarif)")
	cmd.Flags().StringVar(&output, "output", "", "write to FILE instead of stdout")
	return cmd
}
