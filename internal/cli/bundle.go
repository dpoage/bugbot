package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// newBundleCmd returns the `bugbot bundle` parent command. Subcommands treat
// repro artifact bundles (the directories writeArtifacts writes under
// .bugbot/repro/<finding-id>/, each carrying a manifest.json — see
// internal/repro/bundle.go) as executable fixtures rather than write-only
// prose: audit runs the static promotion gate with no container, replay
// re-executes a bundle's plan against the current checkout.
func newBundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Inspect and replay saved repro bundles",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newBundleAuditCmd(), newBundleReplayCmd())
	return cmd
}

// discoverBundleDirs resolves paths into a sorted, deduplicated list of
// bundle directories (each containing manifest.json). Each entry in paths is
// either a bundle directory itself, or a parent directory of one or more
// bundle directories (the .bugbot/repro/ layout: one subdirectory per
// finding ID). Missing paths are a hard error UNLESS optionalMissing is true
// (used for the implicit default path, so a repo with no bundles yet is not
// a usage error).
func discoverBundleDirs(paths []string, optionalMissing bool) ([]string, error) {
	seen := make(map[string]struct{})
	var dirs []string
	add := func(dir string) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		dirs = append(dirs, dir)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			if optionalMissing && os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("bundle path %q: %w", p, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("bundle path %q is not a directory", p)
		}
		if _, merr := os.Stat(filepath.Join(p, repro.ManifestFileName)); merr == nil {
			add(p)
			continue
		}
		entries, rerr := os.ReadDir(p)
		if rerr != nil {
			return nil, fmt.Errorf("read bundle parent dir %q: %w", p, rerr)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			child := filepath.Join(p, e.Name())
			if _, merr := os.Stat(filepath.Join(child, repro.ManifestFileName)); merr == nil {
				add(child)
			}
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// newBundleAuditCmd implements `bugbot bundle audit [dir...]`.
//
// audit loads every bundle under the given directories (each a directory
// containing manifest.json, or a parent directory of such directories — the
// `.bugbot/repro/` layout) and classifies each one with the static,
// sandbox-free target-execution gate (repro.Audit): the same pre-execute
// check the reproduce stage applies before ever spending a container run.
// No sandbox, LLM, or target repo checkout is used or required — this is
// pure static analysis of the bundle's own saved files.
//
// With no arguments, audits repro.DefaultArtifactDir (".bugbot/repro")
// relative to the current directory; a missing default directory is treated
// as "no bundles yet" (exit 0), not an error.
//
// Exit code semantics (see internal/cli/exitcode.go):
//
//	0 (ExitOK)          every discovered bundle passed the static gate.
//	1 (ExitError)        a path could not be read or a manifest failed to
//	                     load/parse — an operational/usage failure.
//	2 (ExitGateFailure)  at least one bundle was flagged; printed with its
//	                     reason and detail. This is what would have caught
//	                     the 4 the_cloud false-T1 bundles at write time.
func newBundleAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit [dir...]",
		Short: "Run the static, sandbox-free promotion gate over saved bundles",
		Long: `audit loads every bundle under the given directories — each a directory
containing manifest.json, or a parent directory of such directories (the
.bugbot/repro/ layout) — and classifies each with the same static
target-execution gate the reproduce stage applies before ever touching a
container. No sandbox, LLM, or target repo checkout is used.

With no arguments, audits the default artifact directory (.bugbot/repro)
relative to the current directory; if it does not exist yet, that is treated
as "no bundles" (exit 0), not an error.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := args
			implicitDefault := len(paths) == 0
			if implicitDefault {
				paths = []string{repro.DefaultArtifactDir}
			}

			dirs, err := discoverBundleDirs(paths, implicitDefault)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if len(dirs) == 0 {
				_, _ = fmt.Fprintln(out, "no bundles found")
				return nil
			}

			flagged := 0
			for _, dir := range dirs {
				b, lerr := repro.LoadBundle(dir)
				if lerr != nil {
					return fmt.Errorf("load bundle %s: %w", dir, lerr)
				}
				res := repro.Audit(b)
				if res.Flagged() {
					flagged++
					_, _ = fmt.Fprintf(out, "FLAGGED %s: %s (%s)\n", dir, res.Reason, res.Detail)
				} else {
					_, _ = fmt.Fprintf(out, "OK      %s\n", dir)
				}
			}
			_, _ = fmt.Fprintf(out, "%d bundle(s) audited, %d flagged\n", len(dirs), flagged)
			if flagged > 0 {
				return newGateError(fmt.Sprintf("bundle audit: %d bundle(s) flagged by the static target-execution gate", flagged))
			}
			return nil
		},
	}
	return cmd
}

// newBundleReplayCmd implements `bugbot bundle replay <dir>`.
//
// replay re-executes a single saved bundle's plan against --target's CURRENT
// checkout (not necessarily the commit it was originally demonstrated
// against — that is the point: has the code changed enough that the bug no
// longer reproduces?), through the exact workspace-reconstruction path the
// official reproduce stage uses (repro.buildSpec, shared with Attempt's
// execute), then the same promotion gate: ClassifyTargetExecution
// pre-execute, interpret() post-execute.
//
// Network is ALWAYS forced to "none" regardless of the bundle's own recorded
// network policy or the operator's sandbox.network config — replaying an
// already-saved, previously-generated command is exactly the untrusted-code
// case the sandbox's hardened default exists to contain.
//
// Requires a container runtime (podman or docker); exits with an error
// (ExitError) when none is found.
//
// Exit code semantics (see internal/cli/exitcode.go):
//
//	0 (ExitOK)          the bundle's plan now exits zero (VerdictReasonExitZero)
//	                     — the bug likely no longer reproduces; a candidate for
//	                     auto-closing the finding. This is the ONLY outcome
//	                     that claims "fixed".
//	1 (ExitError)        an infrastructure failure: no bundle at the given
//	                     path, no container runtime, or a sandbox launch
//	                     failure. The bundle's own status is unknown.
//	2 (ExitGateFailure)  every other outcome — the bug still demonstrably
//	                     reproduces, OR the bundle no longer clears the
//	                     promotion gate at all (target_not_executed,
//	                     build_error, toolchain_error, environment_error,
//	                     timeout, not_demonstrated). None of these let an
//	                     operator safely auto-close the finding; the printed
//	                     reason distinguishes "still broken" from "ambiguous,
//	                     needs a human".
func newBundleReplayCmd() *cobra.Command {
	var (
		target      string
		image       string
		timeoutSecs int
	)

	cmd := &cobra.Command{
		Use:   "replay <bundle-dir>",
		Short: "Re-run a saved bundle's plan against the current checkout",
		Long: `replay re-executes a single saved bundle's plan against --target's current
checkout, through the same workspace-reconstruction path (sandbox exec + the
promotion gate: the static target-execution check, then interpret()) the
official reproduce stage uses. Network is always forced to "none",
regardless of the bundle's recorded network policy or sandbox.network
config.

Use this to check whether a previously-demonstrated bug is still present
(exit 2, "demonstrated") or now fixed (exit 0, "exit_zero" — a candidate to
auto-close the finding).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			bundleDir := args[0]

			b, err := repro.LoadBundle(bundleDir)
			if err != nil {
				return err
			}

			cfg, err := config.Load(configPathFromCmd(cmd))
			if err != nil {
				return err
			}

			runtime, ok := sandbox.Detect()
			if !ok {
				return fmt.Errorf("bundle replay: no container runtime (podman or docker) found on PATH")
			}

			effImage := image
			if effImage == "" {
				effImage = cfg.Sandbox.Image
			}
			if effImage == "" {
				effImage = b.Manifest.Sandbox.Image
			}

			sb, err := sandbox.NewCLI(runtime, effImage, engine.SandboxRunOpts(cfg)...)
			if err != nil {
				return fmt.Errorf("bundle replay: build sandbox: %w", err)
			}
			defer func() { _ = sb.Close() }()

			roMounts, rwMounts := engine.LocalMountsFromConfig(cfg)
			deps, err := sandbox.ResolveDeps(target, sandbox.DepOptions{
				Strategy:      sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
				FetchSandbox:  sb,
				FetchImage:    effImage,
				LocalMounts:   roMounts,
				LocalRWMounts: rwMounts,
			})
			if err != nil {
				return fmt.Errorf("bundle replay: resolve dependencies: %w", err)
			}

			timeout := time.Duration(timeoutSecs) * time.Second
			if timeout <= 0 && cfg.Sandbox.TimeoutSeconds > 0 {
				timeout = time.Duration(cfg.Sandbox.TimeoutSeconds) * time.Second
			}

			res, err := repro.Replay(ctx, sb, target, b, repro.ReplayOptions{
				Image:   effImage,
				Timeout: timeout,
				Deps:    deps,
			})
			if err != nil {
				return fmt.Errorf("bundle replay: %w", err)
			}

			printReplayResult(cmd.OutOrStdout(), bundleDir, res)

			if res.Demonstrated {
				return newGateError(fmt.Sprintf("bundle replay %s: still demonstrated (bug present)", bundleDir))
			}
			if res.Reason == repro.VerdictReasonExitZero {
				return nil
			}
			return newGateError(fmt.Sprintf("bundle replay %s: %s — %s", bundleDir, res.Reason, res.Summary))
		},
	}

	addTargetFlag(cmd, &target)
	cmd.Flags().StringVar(&image, "image", "", "sandbox image override (default: sandbox.image from config, then the bundle's recorded image)")
	cmd.Flags().IntVar(&timeoutSecs, "timeout-seconds", 0, "sandbox execution timeout in seconds (0 = use sandbox.timeout_seconds from config, else repro.DefaultTimeout)")

	return cmd
}

// printReplayResult writes a short human-readable account of a replay
// outcome: whether the sandbox ran at all, the exit code when it did, and
// the classification (including bugbot-qb4r layer b's witnessOnly flag,
// when set — the bug is still demonstrated but the ecosystem could not
// attribute it to the target file via coverage evidence).
func printReplayResult(out io.Writer, dir string, res repro.ReplayResult) {
	switch {
	case res.Demonstrated && res.WitnessOnly:
		_, _ = fmt.Fprintf(out, "%s: DEMONSTRATED, WITNESS-ONLY (exit %d, ecosystem=%s) — bug still present, but %s has no coverage-based witness to attribute it to the target file\n", dir, res.ExitCode, res.Ecosystem, res.Ecosystem)
	case res.Demonstrated:
		_, _ = fmt.Fprintf(out, "%s: DEMONSTRATED (exit %d, ecosystem=%s) — bug still present\n", dir, res.ExitCode, res.Ecosystem)
	case !res.SandboxRan:
		_, _ = fmt.Fprintf(out, "%s: REJECTED %s (%s) — static gate, no sandbox run spent\n", dir, res.Reason, res.Summary)
	default:
		_, _ = fmt.Fprintf(out, "%s: %s (exit %d, ecosystem=%s) — %s\n", dir, res.Reason, res.ExitCode, res.Ecosystem, res.Summary)
	}
}
