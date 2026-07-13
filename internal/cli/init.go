package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/progress"
)

// newInitCmd writes a commented starter config to the current directory. It
// refuses to overwrite an existing file.
func newInitCmd() *cobra.Command {
	var interactive bool
	var stealth bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter bugbot.yaml to the current directory",
		Long: `init writes a commented starter ` + config.DefaultFileName + ` to the current
directory. It refuses to overwrite an existing file.

With --interactive the wizard probes your environment (provider credentials,
sandbox runtime, repo layout) and asks only what detection cannot answer.
Every prompt has a default; pressing Enter through the wizard yields a config
equivalent to the static starter modulo detected runtime/provider.

With --stealth the config and all repo state (storage db, reports,
transcripts) are written under $HOME/.bugbot/<repo-key>/ instead of the
working tree, leaving zero footprint in the scanned repo.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if interactive {
				// Gate on a real TTY so accidental pipe invocations give a
				// clear error instead of hanging waiting for prompts.
				if !progress.IsTerminal(cmd.InOrStdin()) {
					return fmt.Errorf("--interactive requires an interactive terminal (stdin is not a TTY)")
				}
				return runInitInteractive(cmd, stealth)
			}
			return runInitStatic(cmd, stealth)
		},
	}

	cmd.Flags().BoolVar(&interactive, "interactive", false,
		"run the interactive onboarding wizard instead of writing the static starter config")
	cmd.Flags().BoolVar(&stealth, "stealth", false,
		"write the config and all repo state under $HOME/.bugbot/<repo-key>/ instead of the working tree, leaving no footprint in the scanned repo")

	return cmd
}

// stealthConfigComment is injected near the top of a freshly written starter
// (or wizard-rendered) config when --stealth is set, right after the
// secrets notice and before the first documented section. It turns on
// Config.Stealth so config.Load seeds Storage.Path, Report.Dir, and
// TranscriptDir under the per-repo stealth state directory instead of the
// working tree.
const stealthConfigAnchor = "# process environment at run time.\n\n# ---------------------------------------------------------------------------\n# providers:"

// injectStealthFlag inserts an active `stealth: true` line (with a one-line
// explanatory comment) near the top of a rendered config, right after the
// secrets notice. It is a no-op (returns content unchanged) if the expected
// anchor text is not found, so a future template rewrite fails loudly via
// TestInitCmd_StealthFlagInjected rather than silently dropping the setting.
func injectStealthFlag(content string) string {
	const replacement = "# process environment at run time.\n\n" +
		"# stealth: state (storage db, reports, transcripts) lives under\n" +
		"# $HOME/.bugbot/<repo-key>/, never inside this repo.\n" +
		"stealth: true\n\n" +
		"# ---------------------------------------------------------------------------\n" +
		"# providers:"
	return strings.Replace(content, stealthConfigAnchor, replacement, 1)
}

// stripStealthExplicitPaths comments out (with an explanatory note) the
// three explicit path keys that would otherwise override the stealth-mode
// seeded defaults from config.Load's two-pass path-seeding logic:
// storage.path, report.dir, and a top-level transcript_dir. Explicit YAML
// values always win over the seeded defaults, so leaving them active in a
// config written by --stealth would silently defeat stealth mode on the
// very next `bugbot scan`. Matching is by exact line content, not
// substring, so the commented repro-section examples that happen to mention
// the same paths are left untouched. Applied alongside injectStealthFlag
// whenever --stealth writes a config.
func stripStealthExplicitPaths(content string) string {
	targets := map[string]string{
		"  path: .bugbot/state.db":            "  # path: .bugbot/state.db  # omitted under --stealth; storage.path is seeded under $HOME/.bugbot/<repo-key>/",
		"  dir: .bugbot/reports":              "  # dir: .bugbot/reports  # omitted under --stealth; report.dir is seeded under $HOME/.bugbot/<repo-key>/",
		"transcript_dir: .bugbot/transcripts": "# transcript_dir: .bugbot/transcripts  # omitted under --stealth; transcript_dir is seeded under $HOME/.bugbot/<repo-key>/",
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if repl, ok := targets[line]; ok {
			lines[i] = repl
		}
	}
	return strings.Join(lines, "\n")
}

// stealthInitTarget resolves the stealth-mode config path and its
// containing state directory for the repo enclosing the current directory.
func stealthInitTarget() (path, stateDir string, err error) {
	stateDir, err = config.StealthStateDir(config.RepoToplevel("."))
	if err != nil {
		return "", "", err
	}
	return filepath.Join(stateDir, config.DefaultFileName), stateDir, nil
}

// runInitStatic is the original non-interactive path: write StarterYAML
// atomically via O_EXCL and print next steps. When stealth is set, the
// config (with an injected `stealth: true` line) and a repo marker are
// written under the per-repo stealth state directory instead.
func runInitStatic(cmd *cobra.Command, stealth bool) (err error) {
	path := config.DefaultFileName
	content := config.StarterYAML

	if stealth {
		var stateDir string
		var targetErr error
		path, stateDir, targetErr = stealthInitTarget()
		if targetErr != nil {
			return targetErr
		}
		if mkErr := os.MkdirAll(stateDir, 0o700); mkErr != nil {
			return fmt.Errorf("create %s: %w", stateDir, mkErr)
		}
		if markErr := config.WriteRepoMarker(stateDir, config.RepoToplevel(".")); markErr != nil {
			return fmt.Errorf("write repo marker: %w", markErr)
		}
		content = stripStealthExplicitPaths(injectStealthFlag(content))
	}

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

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
	if stealth {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Stealth mode: no .gitignore entry needed — nothing is written inside the repo.")
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Next: set the api_key_env variables, then run `bugbot scan`.")
	return nil
}

// runInitInteractive drives the wizard, writes the resulting config, runs
// doctor checks, and prints next steps. It assumes stdin is a real TTY (the
// RunE wrapper has already verified this). When stealth is set, the config
// (with an injected `stealth: true` line) and a repo marker are written
// under the per-repo stealth state directory instead of the working tree.
func runInitInteractive(cmd *cobra.Command, stealth bool) (err error) {
	path := config.DefaultFileName
	var stateDir string

	if stealth {
		var targetErr error
		path, stateDir, targetErr = stealthInitTarget()
		if targetErr != nil {
			return targetErr
		}
	}

	// Check for existing file BEFORE we start the wizard so we don't waste
	// the user's time answering questions only to fail at write time.
	if _, statErr := os.Stat(path); statErr == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", path)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Bugbot interactive setup wizard\n")
	_, _ = fmt.Fprintf(out, "================================\n")
	_, _ = fmt.Fprintf(out, "Press Enter to accept [defaults]. Ctrl-C to abort.\n")

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
	if stealth {
		yamlContent = stripStealthExplicitPaths(injectStealthFlag(yamlContent))
		if mkErr := os.MkdirAll(stateDir, 0o700); mkErr != nil {
			return fmt.Errorf("create %s: %w", stateDir, mkErr)
		}
		if markErr := config.WriteRepoMarker(stateDir, config.RepoToplevel(".")); markErr != nil {
			return fmt.Errorf("write repo marker: %w", markErr)
		}
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

	_, _ = fmt.Fprintf(out, "\nWrote %s\n\n", path)
	if stealth {
		_, _ = fmt.Fprintln(out, "Stealth mode: no .gitignore entry needed — nothing is written inside the repo.")
	}

	// Run doctor checks so the user sees any immediate problems.
	_, _ = fmt.Fprintln(out, "Running doctor checks...")
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
	_, _ = fmt.Fprintln(out, "\nNext steps:")
	_, _ = fmt.Fprintf(out, "  1. Set the %s environment variable.\n", wCfg.APIKeyEnv)
	_, _ = fmt.Fprintln(out, "  2. Set sandbox.image to a toolchain image for your repo language.")
	_, _ = fmt.Fprintln(out, "  3. Run `bugbot scan` to start your first analysis.")
	if wCfg.EnablePublish {
		_, _ = fmt.Fprintln(out, "  4. Ensure `gh` CLI is installed and authenticated for publish.")
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
