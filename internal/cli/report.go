package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
)

// newReportCmd groups the report subcommands (list, show, dismiss).
func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Inspect and manage findings",
		Args:  cobra.NoArgs,
		// With no subcommand, print help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		newReportListCmd(),
		newReportShowCmd(),
		newReportDismissCmd(),
	)

	return cmd
}

// newReportListCmd lists stored findings. Stub.
func newReportListCmd() *cobra.Command {
	var (
		tier    string
		showAll bool
	)
	cmd := &cobra.Command{
		Use:   "list [flags]",
		Short: "List findings (T3/suspected suppressed unless --all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"report list: not implemented yet (tier=%q all=%v storage=%q)\n",
				tier, showAll, cfg.Storage.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "", "filter by tier: t1, t2, or t3")
	cmd.Flags().BoolVar(&showAll, "all", false, "include suppressed (T3) findings")
	return cmd
}

// newReportShowCmd shows a single finding by ID. Stub.
func newReportShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <finding-id>",
		Short: "Show a single finding in detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"report show: not implemented yet (id=%q storage=%q)\n",
				args[0], cfg.Storage.Path)
			return nil
		},
	}
	return cmd
}

// newReportDismissCmd dismisses a finding, recording a persistent suppression.
// Stub.
func newReportDismissCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "dismiss <finding-id>",
		Short: "Dismiss a finding and record a persistent suppression",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"report dismiss: not implemented yet (id=%q reason=%q storage=%q)\n",
				args[0], reason, cfg.Storage.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why this finding is being dismissed")
	return cmd
}
