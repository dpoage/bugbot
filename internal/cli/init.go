package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/progress"
)

// newInitCmd writes a commented starter config to the current directory. It
// refuses to overwrite an existing file.
func newInitCmd() *cobra.Command {
	var interactive bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter bugbot.yaml to the current directory",
		Long: `init writes a commented starter ` + config.DefaultFileName + ` to the current
directory. It refuses to overwrite an existing file.

With --interactive the wizard probes your environment (provider credentials,
sandbox runtime, repo layout) and asks only what detection cannot answer.
Every prompt has a default; pressing Enter through the wizard yields a config
equivalent to the static starter modulo detected runtime/provider.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if interactive {
				// Gate on a real TTY so accidental pipe invocations give a
				// clear error instead of hanging waiting for prompts.
				if !progress.IsTerminal(cmd.InOrStdin()) {
					return fmt.Errorf("--interactive requires an interactive terminal (stdin is not a TTY)")
				}
				return runInitInteractive(cmd)
			}
			return runInitStatic(cmd)
		},
	}

	cmd.Flags().BoolVar(&interactive, "interactive", false,
		"run the interactive onboarding wizard instead of writing the static starter config")

	return cmd
}

// runInitStatic is the original non-interactive path: write StarterYAML
// atomically via O_EXCL and print next steps.
func runInitStatic(cmd *cobra.Command) (err error) {
	path := config.DefaultFileName

	// Refuse to clobber an existing file. Use O_EXCL so the
	// check-and-create is atomic.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	// Closing a written file can surface a deferred write/flush failure, so
	// propagate it unless an earlier error already took precedence.
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()

	if _, err := f.WriteString(config.StarterYAML); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Next: set the api_key_env variables, then run `bugbot scan`.")
	return nil
}

// runInitInteractive drives the wizard, writes the resulting config, runs
// doctor checks, and prints next steps. It assumes stdin is a real TTY (the
// RunE wrapper has already verified this).
func runInitInteractive(cmd *cobra.Command) (err error) {
	path := config.DefaultFileName

	// Check for existing file BEFORE we start the wizard so we don't waste
	// the user's time answering questions only to fail at write time.
	if _, statErr := os.Stat(path); statErr == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", path)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Bugbot interactive setup wizard\n")
	fmt.Fprintf(out, "================================\n")
	fmt.Fprintf(out, "Press Enter to accept [defaults]. Ctrl-C to abort.\n")

	wCfg, wizErr := runInteractive(
		cmd.InOrStdin(),
		out,
		".",
		os.Getenv,
		exec.LookPath,
	)
	if wizErr != nil {
		return fmt.Errorf("wizard: %w", wizErr)
	}

	// Render the config from the wizard answers.
	yamlContent, renderErr := renderConfig(wCfg)
	if renderErr != nil {
		return fmt.Errorf("render config: %w", renderErr)
	}

	// Write atomically via O_EXCL.
	f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if openErr != nil {
		if errors.Is(openErr, fs.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		return fmt.Errorf("create %s: %w", path, openErr)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", path, cerr)
		}
	}()

	if _, writeErr := f.WriteString(yamlContent); writeErr != nil {
		return fmt.Errorf("write %s: %w", path, writeErr)
	}

	fmt.Fprintf(out, "\nWrote %s\n\n", path)

	// Run doctor checks so the user sees any immediate problems.
	fmt.Fprintln(out, "Running doctor checks...")
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	env := doctorEnv{
		configPath: path,
		repoDir:    ".",
		lookupEnv:  os.Getenv,
		lookPath:   exec.LookPath,
		runCommand: func(ctx context.Context, name string, args ...string) (string, error) {
			c := exec.CommandContext(ctx, name, args...)
			c.WaitDelay = 2 * time.Second
			b, cmdErr := c.CombinedOutput()
			return string(b), cmdErr
		},
		snapshot: nil, // nil triggers the real git path
		out:      out,
	}
	results := runChecks(ctx, env, false)
	printResults(out, results)

	// Print next steps regardless of doctor outcome.
	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintf(out, "  1. Set the %s environment variable.\n", wCfg.APIKeyEnv)
	fmt.Fprintln(out, "  2. Set sandbox.image to a toolchain image for your repo language.")
	fmt.Fprintln(out, "  3. Run `bugbot scan` to start your first analysis.")
	if wCfg.EnablePublish {
		fmt.Fprintln(out, "  4. Ensure `gh` CLI is installed and authenticated for publish.")
	}

	// Return an error if doctor found hard failures, so the exit code reflects
	// the health of the new config.
	for _, r := range results {
		if r.hard && r.Status == statusFail {
			return fmt.Errorf("doctor: one or more checks failed — review output above")
		}
	}
	return nil
}
