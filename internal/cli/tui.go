package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/tui"
)

// newTUICmd launches the agent-first terminal cockpit. internal/tui.Run
// auto-selects Owner mode (dispatch-capable, live in-process events) when
// the store's writer lock is free, and falls back to the pre-existing
// read-only Observer mode (SnapshotFeed) whenever another process — or an
// idle Owner cockpit — already holds it.
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
