package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
)

// newDaemonCmd runs Bugbot continuously, polling for new commits and sweeping
// periodically under token budgets. Stub for now.
func newDaemonCmd() *cobra.Command {
	var repoPath string

	cmd := &cobra.Command{
		Use:   "daemon [flags]",
		Short: "Run Bugbot continuously with polling, sweeps, and idle backoff",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"daemon: not implemented yet (repo=%q poll=%s sweep=%s idle_backoff=%s)\n",
				repoPath, cfg.Daemon.PollInterval, cfg.Daemon.SweepInterval, cfg.Daemon.IdleBackoff)
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", ".", "path to the target repository")

	return cmd
}
