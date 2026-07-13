package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/store"
)

// This file holds the command bootstrap that genuinely belongs in
// internal/cli: configPathFromCmd (reads a cobra flag), closeStore (a tiny
// defer-friendly wrapper), and thin cmdOpenStore(ReadOnly) forwarders kept
// for the CLI commands that open a store directly rather than through an
// engine.Dispatcher. Everything else that used to live here — LLM
// role-client resolution, funnel.Options plumbing, and the sandbox-degraded
// warning — moved to internal/engine (BuildRoleClients, BuildFunnelOptions,
// PrintSandboxDegradedWarning) so cobra-free frontends other than this CLI
// (starting with the Observer TUI) can share the same wiring through an
// engine.Dispatcher.

// configPathFromCmd returns the config file path to use, in priority order:
//  1. an explicit --config flag value that differs from config.DefaultFileName
//     (the user asked for a specific file; honor it as-is);
//  2. config.DefaultFileName ("bugbot.yaml") when it exists in the current
//     directory (today's convention — a local config always wins);
//  3. the stealth-mode config path under $HOME/.bugbot/<repo-key>/, when it
//     exists (config.StealthConfigPath), so `bugbot init --stealth` leaves
//     zero footprint in the repo yet every command still finds its config;
//  4. config.DefaultFileName otherwise, preserving today's "file not found"
//     error message when nothing is configured.
//
// Every RunE closure calls this at the top; the root command registers the
// --config flag with config.DefaultFileName as the default.
func configPathFromCmd(cmd *cobra.Command) string {
	p, err := cmd.Root().PersistentFlags().GetString("config")
	if err != nil || p == "" {
		p = config.DefaultFileName
	}
	if p != config.DefaultFileName {
		return p
	}
	if _, statErr := os.Stat(config.DefaultFileName); statErr == nil {
		return config.DefaultFileName
	}
	if stealthPath, stealthErr := config.StealthConfigPath("."); stealthErr == nil {
		if _, statErr := os.Stat(stealthPath); statErr == nil {
			return stealthPath
		}
	}
	return config.DefaultFileName
}

// cmdOpenStore is a thin forwarder to engine.OpenStore, kept for the CLI
// commands that open a store directly rather than through an
// engine.Dispatcher (cartography, publish, and report's mutating
// subcommand). Commands that dispatch through a Dispatcher
// (scan/verify/repro/sweep/review/daemon) call engine.Open instead.
func cmdOpenStore(ctx context.Context, cfgPath string) (config.Config, *store.Store, error) {
	return engine.OpenStore(ctx, cfgPath)
}

// cmdOpenStoreReadOnly is a thin forwarder to engine.OpenStoreReadOnly, kept
// for the read-only CLI commands (report, leads, metrics, export, status)
// that run fine while a scan or daemon holds the writer lock elsewhere.
func cmdOpenStoreReadOnly(ctx context.Context, cfgPath string) (config.Config, *store.Store, error) {
	return engine.OpenStoreReadOnly(ctx, cfgPath)
}

// closeStore closes a store and discards the error. Its sole purpose is to
// replace the verbatim `defer func() { _ = st.Close() }()` pattern that
// otherwise repeats at every store-using command site. Close failures on a
// process-about-to-exit store are never actionable in CLI context.
func closeStore(st *store.Store) {
	_ = st.Close()
}
