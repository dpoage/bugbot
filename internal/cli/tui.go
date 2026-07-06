package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/tui"
)

// newTUICmd launches the agent-first terminal cockpit. It never opens the
// store for writing and never bootstraps role clients: the current
// implementation is Observer-only (read-only), matching internal/tui's
// SnapshotFeed.
func newTUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the full-screen terminal cockpit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			return tui.Run(ctx, cfg)
		},
	}
	return cmd
}
