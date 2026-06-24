package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/sarif"
	"github.com/dpoage/bugbot/internal/store"
)

// ExportFormat is the set of interchange formats supported by `bugbot export`.
type ExportFormat string

const (
	// ExportFormatSARIF produces a SARIF 2.1.0 document suitable for
	// GitHub Code Scanning upload.
	ExportFormatSARIF ExportFormat = "sarif"
)

// parseExportFormat validates s and returns the corresponding ExportFormat, or
// an error listing supported values when s is unknown.
func parseExportFormat(s string) (ExportFormat, error) {
	switch ExportFormat(s) {
	case ExportFormatSARIF:
		return ExportFormatSARIF, nil
	default:
		return "", fmt.Errorf("unsupported --format %q (supported: sarif)", s)
	}
}

// newExportCmd implements `bugbot export --format sarif [--output FILE]`.
// It loads all findings from the store and marshals them as SARIF 2.1.0.
func newExportCmd() *cobra.Command {
	var (
		formatStr string
		output    string
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
			format, err := parseExportFormat(formatStr)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			_, st, err := cmdOpenStoreReadOnly(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			findings, err := st.ListFindings(ctx, store.FindingFilter{})
			if err != nil {
				return fmt.Errorf("list findings: %w", err)
			}

			switch format {
			case ExportFormatSARIF:
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
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&formatStr, "format", "sarif", "output format (sarif)")
	cmd.Flags().StringVar(&output, "output", "", "write to FILE instead of stdout")
	return cmd
}
