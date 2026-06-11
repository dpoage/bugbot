package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/lsp"
	"github.com/dpoage/bugbot/internal/repro"
)

// checkStatus classifies the outcome of a single doctor check.
type checkStatus string

const (
	statusPass checkStatus = "PASS"
	statusFail checkStatus = "FAIL"
	statusWarn checkStatus = "WARN"
	statusSkip checkStatus = "SKIP"
	statusInfo checkStatus = "INFO"
)

// checkResult is the outcome of one doctor check. Hard checks (config,
// providers, sandbox binary, sandbox responding) cause the command to exit
// nonzero when they fail. Repo-fact and informational checks never affect the
// exit code.
type checkResult struct {
	Name   string
	Status checkStatus
	Detail string
	// hard marks results that should count as failures for the exit code.
	hard bool
}

// doctorEnv holds the injectable seams for all external probes so unit tests
// can run doctor without a real container runtime, network, or API keys.
type doctorEnv struct {
	// configPath is the YAML file to load; absolute after construction.
	configPath string
	// repoDir is the working directory for git and build-system checks.
	repoDir string
	// lookupEnv returns the value of the named environment variable.
	lookupEnv func(string) string
	// lookPath resolves a binary name to an absolute path, as exec.LookPath.
	lookPath func(string) (string, error)
	// runCommand runs name with args and returns combined stdout+stderr. Used
	// for sandbox probes; callers must pass a context with a deadline.
	runCommand func(ctx context.Context, name string, args ...string) (string, error)
	// snapshot returns the dominant languages for the current repo. Exists as a
	// seam so unit tests don't need a real git repository.
	snapshot func(ctx context.Context) ([]ingest.Language, error)
	out      io.Writer
}

// newDoctorCmd builds the `bugbot doctor` subcommand. It runs a checklist of
// environment and config probes and exits nonzero if any hard check fails.
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check environment and config for common setup problems",
		Long: `Doctor runs a checklist of environment and configuration probes.

Each item is printed with a status tag (PASS/FAIL/WARN/SKIP/INFO) and a short
detail. The command exits nonzero when any hard check fails:
  - config invalid or unreadable
  - a provider API key env var is unset
  - the sandbox runtime is missing or not responding

Repo-fact and informational items (git, languages, build systems, LSP servers)
never affect the exit code.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			env := doctorEnv{
				configPath: configPath,
				repoDir:    ".",
				lookupEnv:  os.Getenv,
				lookPath:   exec.LookPath,
				runCommand: func(ctx context.Context, name string, args ...string) (string, error) {
					c := exec.CommandContext(ctx, name, args...)
					// A killed child can leave a grandchild holding the output
					// pipes, blocking CombinedOutput past ctx cancellation;
					// WaitDelay bounds that wait so probes cannot wedge doctor.
					c.WaitDelay = 2 * time.Second
					out, err := c.CombinedOutput()
					return string(out), err
				},
				snapshot: nil, // nil triggers the real git path in runChecks
				out:      cmd.OutOrStdout(),
			}
			results := runChecks(ctx, env)
			printResults(env.out, results)
			for _, r := range results {
				if r.hard && r.Status == statusFail {
					return fmt.Errorf("doctor: one or more checks failed")
				}
			}
			return nil
		},
	}
}

// runChecks executes every doctor check in order and returns the full result
// list. Hard failures in early checks cause dependent checks to report SKIP.
func runChecks(ctx context.Context, env doctorEnv) []checkResult {
	var results []checkResult

	// 1. Config load — hard. Downstream checks that need a valid config are
	//    skipped on failure.
	cfgResult, cfg, cfgOK := checkConfig(env)
	results = append(results, cfgResult)

	// 2. Providers — hard; requires a valid config.
	if !cfgOK {
		results = append(results, checkResult{
			Name:   "providers",
			Status: statusSkip,
			Detail: "config did not load",
		})
	} else {
		results = append(results, checkProviders(env, cfg)...)
	}

	// 3. Sandbox — hard; requires a valid config.
	if !cfgOK {
		results = append(results, checkResult{
			Name:   "sandbox binary",
			Status: statusSkip,
			Detail: "config did not load",
		}, checkResult{
			Name:   "sandbox responding",
			Status: statusSkip,
			Detail: "config did not load",
		}, checkResult{
			Name:   "sandbox image",
			Status: statusSkip,
			Detail: "config did not load",
		})
	} else {
		results = append(results, checkSandbox(ctx, env, cfg)...)
	}

	// 4. Repo facts — informational, never hard.
	repoResults, langs, buildSystems := checkRepo(ctx, env, cfg, cfgOK)
	results = append(results, repoResults...)

	// 5. Informational checks.
	results = append(results, checkGHCLI(env))
	results = append(results, checkLSP(env, langs)...)

	// Per-language tier warnings (4d): warn when a dominant language lacks
	// specific repro guidance or a matching detected build system. These are
	// appended after the LSP block so they appear near the language info item.
	results = append(results, checkLangTier(langs, buildSystems)...)

	return results
}

// checkConfig loads and validates the config file. Returns the result, the
// loaded config (zero on failure), and whether the load succeeded.
func checkConfig(env doctorEnv) (checkResult, config.Config, bool) {
	// Abs can only fail if the working directory is gone; fall back to the
	// raw path in the detail line rather than failing the check over it.
	abs, absErr := filepath.Abs(env.configPath)
	if absErr != nil {
		abs = env.configPath
	}
	cfg, err := config.Load(env.configPath)
	if err != nil {
		return checkResult{
			Name:   "config",
			Status: statusFail,
			Detail: err.Error(),
			hard:   true,
		}, config.Config{}, false
	}
	return checkResult{
		Name:   "config",
		Status: statusPass,
		Detail: "loaded " + abs,
	}, cfg, true
}

// checkProviders checks that each provider's API key env var is set and
// non-empty. Results are returned in sorted provider name order so output is
// deterministic. Each missing key is a hard failure.
func checkProviders(env doctorEnv, cfg config.Config) []checkResult {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []checkResult
	for _, name := range names {
		p := cfg.Providers[name]
		envName := p.APIKeyEnv
		val := env.lookupEnv(envName)
		if val == "" {
			out = append(out, checkResult{
				Name:   "provider " + name,
				Status: statusFail,
				// Print only the env var name — NEVER the value (secret).
				Detail: "$" + envName + " is not set",
				hard:   true,
			})
		} else {
			out = append(out, checkResult{
				Name:   "provider " + name,
				Status: statusPass,
				Detail: "$" + envName + " is set",
			})
		}
	}
	return out
}

// sandboxProbeTimeout bounds how long we wait for each runtime probe
// (`<runtime> info`, `<runtime> image inspect`). A wedged runtime must not
// hang doctor.
const sandboxProbeTimeout = 5 * time.Second

// repoFactsTimeout bounds the informational repo-facts group (git rev-parse
// plus the ingest snapshot, which walks every tracked file). Generous because
// snapshotting a large repo legitimately takes time, but finite because an
// informational check must never wedge doctor.
const repoFactsTimeout = 30 * time.Second

// checkSandbox checks the sandbox runtime binary, its responsiveness, and
// whether the configured image is present locally. Binary absent and runtime
// not responding are hard failures; image absent is a warning only.
func checkSandbox(ctx context.Context, env doctorEnv, cfg config.Config) []checkResult {
	rt := cfg.Sandbox.Runtime

	// 3a. Binary present?
	_, err := env.lookPath(rt)
	if err != nil {
		return []checkResult{
			{
				Name:   "sandbox binary",
				Status: statusFail,
				Detail: rt + " not found on PATH",
				hard:   true,
			},
			{
				Name:   "sandbox responding",
				Status: statusSkip,
				Detail: "binary absent",
			},
			{
				Name:   "sandbox image",
				Status: statusSkip,
				Detail: "binary absent",
			},
		}
	}

	results := []checkResult{{
		Name:   "sandbox binary",
		Status: statusPass,
		Detail: rt + " found",
	}}

	// 3b. Responding? Use a tight timeout so a wedged daemon doesn't hang doctor.
	probeCtx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()

	_, probeErr := env.runCommand(probeCtx, rt, "info")
	if probeErr != nil {
		detail := rt + " not responding"
		if probeCtx.Err() != nil {
			detail = rt + " timed out after " + sandboxProbeTimeout.String()
		}
		results = append(results, checkResult{
			Name:   "sandbox responding",
			Status: statusFail,
			Detail: detail,
			hard:   true,
		}, checkResult{
			Name:   "sandbox image",
			Status: statusSkip,
			Detail: "runtime not responding",
		})
		return results
	}
	results = append(results, checkResult{
		Name:   "sandbox responding",
		Status: statusPass,
		Detail: rt + " info succeeded",
	})

	// 3c. Image present locally? Non-zero exit from image inspect means absent.
	// We discard the output (can be large JSON); only the exit status matters.
	// Bounded like the info probe: a runtime that answers `info` but wedges on
	// `image inspect` must not hang doctor either.
	image := cfg.Sandbox.Image
	imgCtx, imgCancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer imgCancel()
	_, imgErr := env.runCommand(imgCtx, rt, "image", "inspect", image)
	if imgErr != nil {
		results = append(results, checkResult{
			Name:   "sandbox image",
			Status: statusWarn,
			Detail: image + " not present locally (pull needed)",
		})
	} else {
		results = append(results, checkResult{
			Name:   "sandbox image",
			Status: statusPass,
			Detail: image + " present locally",
		})
	}
	return results
}

// checkRepo performs the git, languages, and build-system checks (group 4).
// None of these are hard failures. Returns the results, the dominant languages
// (for downstream checks), and the detected build systems.
func checkRepo(ctx context.Context, env doctorEnv, cfg config.Config, cfgOK bool) ([]checkResult, []ingest.Language, []ingest.BuildSystem) {
	var results []checkResult

	// The cobra context carries no deadline, and these checks are purely
	// informational — a hung git or filesystem must not wedge doctor, so the
	// whole group (rev-parse + snapshot) shares one bounded context.
	ctx, cancel := context.WithTimeout(ctx, repoFactsTimeout)
	defer cancel()

	// 4a. Is repoDir a git repo?
	_, gitErr := env.runCommand(ctx, "git", "-C", env.repoDir, "rev-parse", "--git-dir")
	if gitErr != nil {
		results = append(results, checkResult{
			Name:   "git repo",
			Status: statusWarn,
			Detail: env.repoDir + " is not a git repository (4b/4c skipped)",
		})
		return results, nil, nil
	}
	results = append(results, checkResult{
		Name:   "git repo",
		Status: statusPass,
		Detail: env.repoDir,
	})

	// 4b. Dominant languages.
	var langs []ingest.Language
	if env.snapshot != nil {
		// Injected seam for tests.
		var snapErr error
		langs, snapErr = env.snapshot(ctx)
		if snapErr != nil {
			results = append(results, checkResult{
				Name:   "languages",
				Status: statusWarn,
				Detail: "could not determine languages: " + snapErr.Error(),
			})
		} else {
			results = append(results, langInfoResult(langs))
		}
	} else {
		// Production path: open the repo and snapshot it.
		var filter ingest.ScanFilter
		if cfgOK {
			filter = ingest.ScanFilter{Include: cfg.Scan.Include, Exclude: cfg.Scan.Exclude}
		}
		repo, err := ingest.Open(ctx, env.repoDir)
		if err != nil {
			results = append(results, checkResult{
				Name:   "languages",
				Status: statusWarn,
				Detail: "could not open repo: " + err.Error(),
			})
		} else {
			snap, err := repo.Snapshot(ctx, filter)
			if err != nil {
				results = append(results, checkResult{
					Name:   "languages",
					Status: statusWarn,
					Detail: "could not snapshot repo: " + err.Error(),
				})
			} else {
				langs = ingest.DominantLanguages(snap)
				results = append(results, langInfoResult(langs))
			}
		}
	}

	// 4c. Build systems — pure filesystem stat, no seam needed.
	systems := ingest.DetectBuildSystems(env.repoDir)
	if len(systems) == 0 {
		results = append(results, checkResult{
			Name:   "build systems",
			Status: statusInfo,
			Detail: "none detected",
		})
	} else {
		names := make([]string, len(systems))
		for i, s := range systems {
			names[i] = string(s)
		}
		results = append(results, checkResult{
			Name:   "build systems",
			Status: statusInfo,
			Detail: strings.Join(names, ", "),
		})
	}

	return results, langs, systems
}

// langInfoResult formats the INFO result for the detected languages list.
func langInfoResult(langs []ingest.Language) checkResult {
	if len(langs) == 0 {
		return checkResult{
			Name:   "languages",
			Status: statusInfo,
			Detail: "none detected",
		}
	}
	names := make([]string, len(langs))
	for i, l := range langs {
		names[i] = string(l)
	}
	return checkResult{
		Name:   "languages",
		Status: statusInfo,
		Detail: strings.Join(names, ", "),
	}
}

// checkGHCLI checks for the gh CLI used by `bugbot publish`. Absent is
// informational only — it does not affect the exit code.
func checkGHCLI(env doctorEnv) checkResult {
	_, err := env.lookPath("gh")
	if err != nil {
		return checkResult{
			Name:   "gh cli",
			Status: statusInfo,
			Detail: "not found (publish unavailable)",
		}
	}
	return checkResult{
		Name:   "gh cli",
		Status: statusPass,
		Detail: "gh found",
	}
}

// langBuildSystems maps each language to the set of build systems that can
// compile/test it. This is doctor-specific policy: a dominant language without
// one of these build-system markers may lack build infrastructure.
var langBuildSystems = map[ingest.Language][]ingest.BuildSystem{
	ingest.LangGo:         {ingest.BuildSystemGoModule, ingest.BuildSystemGoWorkspace, ingest.BuildSystemBazel},
	ingest.LangRust:       {ingest.BuildSystemCargo},
	ingest.LangJavaScript: {ingest.BuildSystemNPM, ingest.BuildSystemJSWorkspace, ingest.BuildSystemBazel},
	ingest.LangTypeScript: {ingest.BuildSystemNPM, ingest.BuildSystemJSWorkspace, ingest.BuildSystemBazel},
	ingest.LangPython:     {ingest.BuildSystemPython, ingest.BuildSystemBazel},
}

// checkLangTier emits WARN for each dominant language that lacks (i) specific
// repro guidance or (ii) a matching detected build system. These are append-
// only advisory items; they never affect the exit code.
func checkLangTier(langs []ingest.Language, buildSystems []ingest.BuildSystem) []checkResult {
	if len(langs) == 0 {
		return nil
	}
	bsSet := make(map[ingest.BuildSystem]bool, len(buildSystems))
	for _, bs := range buildSystems {
		bsSet[bs] = true
	}

	var out []checkResult
	for _, lang := range langs {
		if !repro.HasGuidance(lang, buildSystems...) {
			out = append(out, checkResult{
				Name:   "lang tier " + string(lang),
				Status: statusWarn,
				Detail: string(lang) + " has no specific repro guidance (generic fallback used)",
			})
		}
		if expected, ok := langBuildSystems[lang]; ok {
			found := false
			for _, bs := range expected {
				if bsSet[bs] {
					found = true
					break
				}
			}
			if !found {
				names := make([]string, len(expected))
				for i, bs := range expected {
					names[i] = string(bs)
				}
				out = append(out, checkResult{
					Name:   "lang build " + string(lang),
					Status: statusWarn,
					Detail: string(lang) + " detected but no matching build system (" + strings.Join(names, "/") + ")",
				})
			}
		}
	}
	return out
}

// checkLSP checks whether an LSP server is installed for each dominant
// language. Results are informational — not finding a server is not a hard
// failure. DefaultServers() is consumed dynamically so additions to the server
// registry compose without touching this file.
func checkLSP(env doctorEnv, langs []ingest.Language) []checkResult {
	if len(langs) == 0 {
		return nil
	}
	servers := lsp.DefaultServers()
	var out []checkResult

	for _, lang := range langs {
		exts := ingest.ExtensionsForLanguage(lang)
		if len(exts) == 0 {
			continue
		}

		// Collect configs that serve any extension for this language.
		var candidates []lsp.ServerConfig
		for _, sc := range servers {
			for _, ext := range exts {
				if _, ok := sc.LanguageIDs[ext]; ok {
					candidates = append(candidates, sc)
					break
				}
			}
		}
		if len(candidates) == 0 {
			continue
		}

		// Report the first installed server as pass; if none installed, warn.
		var triedNames []string
		installed := false
		for _, sc := range candidates {
			triedNames = append(triedNames, sc.Cmd)
			if _, err := env.lookPath(sc.Cmd); err == nil {
				out = append(out, checkResult{
					Name:   "lsp " + string(lang),
					Status: statusPass,
					Detail: sc.Cmd + " installed",
				})
				installed = true
				break
			}
		}
		if !installed {
			out = append(out, checkResult{
				Name:   "lsp " + string(lang),
				Status: statusInfo,
				Detail: "no server installed (tried " + strings.Join(triedNames, ", ") + ")",
			})
		}
	}
	return out
}

// statusWidth is the fixed column width for the status tag in output lines.
const statusWidth = 6

// printResults writes one aligned line per check result then a summary line.
func printResults(w io.Writer, results []checkResult) {
	// Measure the longest name for alignment.
	maxName := 0
	for _, r := range results {
		if len(r.Name) > maxName {
			maxName = len(r.Name)
		}
	}

	var failed, warned int
	for _, r := range results {
		tag := fmt.Sprintf("%-*s", statusWidth, string(r.Status))
		name := fmt.Sprintf("%-*s", maxName, r.Name)
		_, _ = fmt.Fprintf(w, "%s  %s  %s\n", tag, name, r.Detail)
		switch r.Status {
		case statusFail:
			failed++
		case statusWarn:
			warned++
		}
	}

	// Summary line.
	_, _ = fmt.Fprintln(w)
	switch {
	case failed > 0 && warned > 0:
		_, _ = fmt.Fprintf(w, "doctor: %d failed, %d warning(s)\n", failed, warned)
	case failed > 0:
		_, _ = fmt.Fprintf(w, "doctor: %d failed\n", failed)
	case warned > 0:
		_, _ = fmt.Fprintf(w, "doctor: %d warning(s)\n", warned)
	default:
		_, _ = fmt.Fprintln(w, "doctor: all checks passed")
	}
}
