package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
)

// newScanCmd runs a single pass of the detection funnel over a target repo.
// Stub: loads config, then reports that the funnel is not yet implemented.
func newScanCmd() *cobra.Command {
	var (
		repoPath    string
		includeT3   bool
		concurrency int
	)

	cmd := &cobra.Command{
		Use:   "scan [flags]",
		Short: "Run the detection funnel once over a target repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"scan: not implemented yet (repo=%q include_t3=%v concurrency=%d storage=%q)\n",
				repoPath, includeT3, concurrency, cfg.Storage.Path)
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", ".", "path to the target repository")
	cmd.Flags().BoolVar(&includeT3, "include-suspected", false, "include T3 (suspected) findings in output")
	cmd.Flags().IntVar(&concurrency, "concurrency", 4, "number of parallel finder agents")

	return cmd
}
