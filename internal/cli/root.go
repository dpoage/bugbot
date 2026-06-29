// Package cli wires up Bugbot's cobra command tree. Most subcommands are stubs
// for now; `bugbot init` is fully implemented.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
)

// NewRootCmd builds the root command and attaches all subcommands.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bugbot",
		Short: "Bugbot: an autonomous, continuously-running bug-finding harness",
		Long: `Bugbot finds and reports bugs in a target codebase using LLMs.

It runs a precision-first pipeline (ingest, hypothesize, triage, verify,
reproduce, report) and tracks findings across confidence tiers:
  T1 Reproduced, T2 Verified, T3 Suspected (suppressed by default).`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var configPath string
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultFileName,
		"path to the Bugbot config file")

	root.AddCommand(
		newDoctorCmd(),
		newInitCmd(),
		newPrimeCmd(),
		newScanCmd(),
		newRegressCmd(),
		newReproCmd(),
		newVerifyCmd(),
		newSweepCmd(),
		newReviewCmd(),
		newDaemonCmd(),
		newStatusCmd(),
		newReportCmd(),
		newMetricsCmd(),
		newCartographyCmd(),
		newLeadsCmd(),
		newEvalCmd(),
		newPublishCmd(),
		newDesignSandboxCmd(),
		newSandboxCmd(),
		newExportCmd(),
	)

	return root
}

// Execute runs the root command. The caller (main) reports any error.
func Execute() error {
	return NewRootCmd().Execute()
}
