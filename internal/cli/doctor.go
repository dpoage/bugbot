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
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
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
	// verifySandbox, when non-nil, overrides the real sandbox verifier call.
	// Unit tests inject a scripted function here to avoid needing a container
	// runtime. When nil, the real repro.VerifySandbox is used.
	verifySandbox func(ctx context.Context, repoDir string, cfg config.Config) (repro.SmokeVerdict, error)
	out           io.Writer
}

// newDoctorCmd builds the `bugbot doctor` subcommand. It runs a checklist of
// environment and config probes and exits nonzero if any hard check fails.
func newDoctorCmd() *cobra.Command {
	var verifySandboxFlag bool
	var repairFlag bool
	cmd := &cobra.Command{
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
				configPath:    configPathFromCmd(cmd),
				repoDir:       ".",
				lookupEnv:     os.Getenv,
				lookPath:      exec.LookPath,
				verifySandbox: nil, // nil → enabled only when --verify-sandbox is passed
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
			if repairFlag {
				return runRepair(ctx, env)
			}
			results := runChecks(ctx, env, verifySandboxFlag)
			printResults(env.out, results)
			for _, r := range results {
				if r.hard && r.Status == statusFail {
					return fmt.Errorf("doctor: one or more checks failed")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&verifySandboxFlag, "verify-sandbox", false,
		"run a live sandbox toolchain smoke-test (requires the container runtime and image pull; off by default)")
	cmd.Flags().BoolVar(&repairFlag, "repair", false,
		"back up and rebuild a corrupt state database (salvaging readable rows); refuses while a writer holds the lock")
	return cmd
}

// checkStore probes the state database's integrity with PRAGMA quick_check. A
// corrupt db is a hard failure — it blocks scans and risks further data loss —
// and the detail points the operator at `bugbot doctor --repair`. A missing db
// is fine (bugbot has not run here yet) and reported INFO. The probe opens
// read-only so it never contends with a running writer's lock.
func checkStore(ctx context.Context, cfg config.Config) checkResult {
	path := cfg.Storage.Path
	if _, err := os.Stat(path); err != nil {
		return checkResult{Name: "state db", Status: statusInfo, Detail: "no state database yet (" + path + ")"}
	}
	st, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return checkResult{Name: "state db", Status: statusFail, hard: true,
			Detail: fmt.Sprintf("cannot open %s: %v (stop any writer and run `bugbot doctor --repair`)", path, err)}
	}
	defer func() { _ = st.Close() }()
	if err := st.Check(ctx); err != nil {
		return checkResult{Name: "state db", Status: statusFail, hard: true,
			Detail: fmt.Sprintf("integrity check failed: %v (stop any writer and run `bugbot doctor --repair`)", err)}
	}
	return checkResult{Name: "state db", Status: statusPass, Detail: "quick_check ok (" + path + ")"}
}

// checkStealthAdvisories emits non-fatal WARNs for two stealth-mode
// footguns: (a) a stealth config paired with a leftover ./.bugbot directory
// in the scanned repo — two possible state locations can silently diverge —
// and (b) a resolved daemon control-socket path long enough to risk hitting
// the ~104-108 byte unix domain socket path limit on common platforms.
// Never affects the exit code.
func checkStealthAdvisories(env doctorEnv, cfg config.Config) []checkResult {
	var results []checkResult

	if cfg.Stealth {
		legacyDir := filepath.Join(env.repoDir, ".bugbot")
		if info, err := os.Stat(legacyDir); err == nil && info.IsDir() {
			results = append(results, checkResult{
				Name:   "stealth split-brain",
				Status: statusWarn,
				Detail: fmt.Sprintf("stealth mode is on but %s still exists alongside %s — remove the stale in-repo directory or state may be split across both", legacyDir, filepath.Dir(cfg.Storage.Path)),
			})
		}
	}

	if sockPath := cfg.ControlSocketPath(); len(sockPath) > 100 {
		results = append(results, checkResult{
			Name:   "control socket path",
			Status: statusWarn,
			Detail: fmt.Sprintf("%s is %d bytes; unix domain socket paths are limited to ~104-108 bytes on common platforms and the daemon may fail to bind", sockPath, len(sockPath)),
		})
	}

	return results
}

// runRepair backs up and rebuilds a corrupt state database via store.Recover,
// printing a salvage summary. store.Recover takes the cross-process writer
// lock, so this refuses (with *ErrLocked) when a scan or daemon is running —
// the operator must stop writers first, which is the safe order anyway.
func runRepair(ctx context.Context, env doctorEnv) error {
	cfg, err := config.Load(env.configPath)
	if err != nil {
		return err
	}
	path := cfg.Storage.Path
	if _, statErr := os.Stat(path); statErr != nil {
		_, _ = fmt.Fprintf(env.out, "repair: no state database at %s; nothing to do\n", path)
		return nil
	}
	_, _ = fmt.Fprintf(env.out, "repair: rebuilding %s …\n", path)
	rep, err := store.Recover(ctx, path)
	if err != nil {
		if rep != nil && rep.BackupPath != "" {
			_, _ = fmt.Fprintf(env.out, "repair: corrupt db backed up to %s\n", rep.BackupPath)
		}
		return fmt.Errorf("repair failed: %w", err)
	}
	_, _ = fmt.Fprintf(env.out, "repair: ok — corrupt db backed up to %s\n", rep.BackupPath)
	_, _ = fmt.Fprintf(env.out, "repair: salvaged %d rows across %d tables\n", rep.TotalSalvaged(), len(rep.Salvaged))
	names := make([]string, 0, len(rep.Salvaged))
	for t := range rep.Salvaged {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		_, _ = fmt.Fprintf(env.out, "  %-22s %d rows\n", t, rep.Salvaged[t])
	}
	if len(rep.Partial) > 0 {
		sort.Strings(rep.Partial)
		_, _ = fmt.Fprintf(env.out, "repair: PARTIAL reads (corruption hit mid-table): %s\n", strings.Join(rep.Partial, ", "))
	}
	if rep.SourceOpenErr != "" {
		_, _ = fmt.Fprintf(env.out, "repair: WARNING could not open the corrupt db to salvage (%s); installed a fresh empty database\n", rep.SourceOpenErr)
	}
	return nil
}

// runChecks executes every doctor check in order and returns the full result
// list. Hard failures in early checks cause dependent checks to report SKIP.
func runChecks(ctx context.Context, env doctorEnv, runSandboxVerify bool) []checkResult {
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
	} else if cfg.Sandbox.Backend == "bwrap" {
		results = append(results, checkBwrap(ctx, cfg)...)
	} else {
		results = append(results, checkSandbox(ctx, env, cfg)...)
	}

	// 3b. State db integrity — hard; requires a valid config for the db path.
	if cfgOK {
		results = append(results, checkStore(ctx, cfg))
	}

	// 3c. Stealth-mode advisories — warn-only; requires a valid config.
	if cfgOK {
		results = append(results, checkStealthAdvisories(env, cfg)...)
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

	// Image-vs-language advisories (4e): cross-check the configured sandbox
	// image against the repo's detected languages, and warn if offline bazel
	// repro is configured against a non-custom image. Advisory only — never
	// affects the exit code (mirrors checkLangTier above).
	if cfgOK {
		results = append(results, checkImageToolchain(ctx, env, langs, buildSystems, cfg)...)
	}

	// 6. Live sandbox toolchain smoke-test (--verify-sandbox only).
	// Off by default because it requires the container runtime and may pull an
	// image. The cheap checkImageToolchain name-match warn above still runs
	// regardless.
	if runSandboxVerify && cfgOK {
		results = append(results, checkSandboxVerifier(ctx, env, cfg)...)
	}

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
		Detail: "loaded " + abs + "; state dir " + filepath.Dir(cfg.Storage.Path),
	}, cfg, true
}

// checkProviders checks that each provider's credential env var is set and
// non-empty. The check is mode-aware: api_key providers check api_key_env;
// oauth-token providers check auth_token_env instead. Results are returned in
// sorted provider name order so output is deterministic.
//
// Providers referenced by at least one role (finder, verifier, reproducer,
// cartographer, arbiter) are checked as hard failures — a missing credential
// means the scan cannot run. Providers not referenced by any role are checked
// as warnings only, because they may be future roles or test providers.
// Secrets are never printed — only the env var name.
func checkProviders(env doctorEnv, cfg config.Config) []checkResult {
	// Collect the set of provider names actually referenced by the role config.
	referenced := make(map[string]bool)
	for _, p := range []string{
		cfg.Roles.Finder.Provider,
		cfg.Roles.Verifier.Provider,
		cfg.Roles.Reproducer.Provider,
		cfg.Roles.Cartographer.Provider,
		cfg.Roles.Arbiter.Provider,
	} {
		if p != "" {
			referenced[p] = true
		}
	}

	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []checkResult
	for _, name := range names {
		p := cfg.Providers[name]
		isReferenced := referenced[name]

		var envName, modeLabel string
		if p.Auth == "oauth-token" {
			envName = p.AuthTokenEnv
			modeLabel = " (oauth-token)"
		} else {
			envName = p.APIKeyEnv
			modeLabel = ""
		}

		val := env.lookupEnv(envName)
		if val == "" {
			if isReferenced {
				out = append(out, checkResult{
					Name:   "provider " + name,
					Status: statusFail,
					// Print only the env var name — NEVER the value (secret).
					Detail: "$" + envName + " is not set" + modeLabel,
					hard:   true,
				})
			} else {
				out = append(out, checkResult{
					Name:   "provider " + name,
					Status: statusWarn,
					Detail: "$" + envName + " is not set" + modeLabel + " (provider not referenced by any role)",
				})
			}
		} else {
			out = append(out, checkResult{
				Name:   "provider " + name,
				Status: statusPass,
				Detail: "$" + envName + " is set" + modeLabel,
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
// plus `git ls-files` for extension-based language detection). Generous because
// ls-files on a large repo legitimately takes time, but finite because an
// informational check must never wedge doctor.
const repoFactsTimeout = 30 * time.Second

// checkBwrap checks the bwrap backend's usability: bwrap on PATH, Linux, and
// working unprivileged user namespaces (DetectBwrap probes all three and
// reports whichever fails with an actionable reason — acceptance criterion
// 1), plus which resource-limit enforcement mechanism (if any) this host
// offers (acceptance criterion 4). Bwrap absent/unusable is a hard failure,
// same severity as a missing container runtime in checkSandbox; a missing
// resource-limit mechanism is only hard when the operator has not opted
// into sandbox.allow_uncapped, since that is a real "the next real run will
// fail" condition rather than an advisory.
func checkBwrap(ctx context.Context, cfg config.Config) []checkResult {
	if ok, reason := sandbox.DetectBwrap(); !ok {
		return []checkResult{
			{Name: "sandbox binary", Status: statusFail, Detail: reason, hard: true},
			{Name: "sandbox resource caps", Status: statusSkip, Detail: "bwrap unavailable"},
		}
	}
	results := []checkResult{{
		Name:   "sandbox binary",
		Status: statusPass,
		Detail: "bwrap found on PATH; unprivileged user namespaces verified",
	}}
	label, enforced := sandbox.DescribeBwrapCapMethod(ctx)
	switch {
	case enforced:
		results = append(results, checkResult{Name: "sandbox resource caps", Status: statusPass, Detail: label})
	case cfg.Sandbox.AllowUncapped:
		results = append(results, checkResult{
			Name:   "sandbox resource caps",
			Status: statusWarn,
			Detail: label + "; sandbox.allow_uncapped is set, so runs proceed with no enforced memory/CPU/pids limits",
		})
	default:
		results = append(results, checkResult{
			Name:   "sandbox resource caps",
			Status: statusFail,
			Detail: label + "; runs will fail — set sandbox.allow_uncapped to proceed uncapped instead",
			hard:   true,
		})
	}
	return results
}

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
		// Production path: list tracked files via `git ls-files` (no content
		// reads, no binary sniffing) and classify by extension only.
		out, err := env.runCommand(ctx, "git", "-C", env.repoDir, "ls-files")
		if err != nil {
			results = append(results, checkResult{
				Name:   "languages",
				Status: statusWarn,
				Detail: "could not list tracked files: " + err.Error(),
			})
		} else {
			var paths []string
			for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				if line != "" {
					paths = append(paths, line)
				}
			}
			langs = ingest.DominantLanguagesFromPaths(paths)
			results = append(results, langInfoResult(langs))
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

// langImageHints maps each language that requires a real toolchain to the
// substrings expected to appear in a sandbox image name. A language is
// considered "covered" when cfg.Sandbox.Image contains ANY one of its hints
// (case-insensitive). Languages absent from this map (LangShell, LangOther,
// LangCSS, LangHTML, LangMarkdown, etc.) need no toolchain in the image and
// never trigger a warning.
var langImageHints = map[ingest.Language][]string{
	ingest.LangGo:         {"golang"},
	ingest.LangPython:     {"python"},
	ingest.LangTypeScript: {"node"},
	ingest.LangJavaScript: {"node"},
	ingest.LangRust:       {"rust"},
	ingest.LangJava:       {"openjdk", "eclipse-temurin", "gradle", "maven"},
	ingest.LangC:          {"gcc", "clang"},
	ingest.LangCPP:        {"gcc", "clang"},
}

// langToolchainBinaries maps each language that requires an explicit toolchain
// to the binaries that must be resolvable via `command -v` inside the image.
// Multiple slices represent "at least one of each group must be present" — here
// each slice is a single logical requirement. We only probe languages where a
// missing binary causes every repro/verify to fail in practice.
var langToolchainBinaries = map[ingest.Language][]string{
	ingest.LangGo:         {"go"},
	ingest.LangPython:     {"pip", "pip3"},
	ingest.LangTypeScript: {"node", "npm"},
	ingest.LangJavaScript: {"node", "npm"},
	ingest.LangRust:       {"cargo", "rustc"},
	ingest.LangJava:       {"java", "javac"},
	ingest.LangC:          {"gcc", "cc", "clang"},
	ingest.LangCPP:        {"g++", "c++", "clang++", "cmake"},
}

// langToolchainSuggestions maps each language to a suggested image that is
// known to carry its toolchain. Shown in WARN messages when binaries are absent.
var langToolchainSuggestions = map[ingest.Language]string{
	ingest.LangGo:         "golang:<ver>-alpine",
	ingest.LangPython:     "python:3-slim",
	ingest.LangTypeScript: "node:22-slim",
	ingest.LangJavaScript: "node:22-slim",
	ingest.LangRust:       "rust:1-slim",
	ingest.LangJava:       "eclipse-temurin:21-jdk-alpine",
	ingest.LangC:          "gcc:13",
	ingest.LangCPP:        "gcc:13",
}

// probeImageBinaries checks whether the given binaries exist inside image by
// running `<rt> run --rm <image> sh -c 'command -v bin1 bin2 ...'` bounded by
// sandboxProbeTimeout. Returns the list of binaries that are absent. If the
// image is not present locally the probe is skipped and (nil, true) is returned
// (callers should emit an INFO skip). On other errors the probe is best-effort:
// we return the error text as a single "absent" entry so the caller can warn.
func probeImageBinaries(ctx context.Context, env doctorEnv, rt, image string, bins []string) (absent []string, imageNotLocal bool) {
	// Check image presence first, same pattern as checkSandbox (image inspect).
	imgCtx, imgCancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer imgCancel()
	if _, err := env.runCommand(imgCtx, rt, "image", "inspect", image); err != nil {
		return nil, true
	}

	// Image is present; probe each binary individually so we can report which
	// ones are missing by name.
	for _, bin := range bins {
		probeCtx, probeCancel := context.WithTimeout(ctx, sandboxProbeTimeout)
		_, err := env.runCommand(probeCtx, rt, "run", "--rm", image, "sh", "-c", "command -v "+bin)
		probeCancel()
		if err != nil {
			absent = append(absent, bin)
		}
	}
	return absent, false
}

// checkImageToolchain cross-checks the configured sandbox image against the
// repo's detected dominant languages and emits one WARN per language whose
// expected toolchain hint is absent from the image name. For languages whose
// image name DOES match a hint, it also runs a binary probe inside the image
// (bounded by sandboxProbeTimeout) and warns if required binaries are missing.
// If the image is not present locally the probe is skipped with an INFO result.
// When a Bazel build runs under network=none it emits one advisory result:
// WARN if the image is a plain public bazel base (almost certainly not
// purpose-built for offline repro), INFO otherwise — offline bazel repro IS
// supported when the sandbox image is purpose-built.
// All results are advisory only — they never affect the exit code (mirrors
// checkLangTier above). An empty image string or empty langs slice produces no
// results.
func checkImageToolchain(ctx context.Context, env doctorEnv, langs []ingest.Language, buildSystems []ingest.BuildSystem, cfg config.Config) []checkResult {
	image := cfg.Sandbox.Image
	imageLower := strings.ToLower(image)
	if image == "" {
		return nil
	}
	rt := cfg.Sandbox.Runtime
	var out []checkResult
	for _, lang := range langs {
		hints, ok := langImageHints[lang]
		if !ok {
			continue
		}
		covered := false
		for _, h := range hints {
			if strings.Contains(imageLower, h) {
				covered = true
				break
			}
		}
		if !covered {
			// Name-match failed: warn without probing (image is clearly wrong family).
			out = append(out, checkResult{
				Name:   "image toolchain " + string(lang),
				Status: statusWarn,
				Detail: string(lang) + " detected but sandbox image " + image + " carries no expected toolchain (expected one of: " + strings.Join(hints, ", ") + ") — repro/verify of its findings will fail in this image",
			})
			continue
		}

		// Name-match succeeded: run a binary probe to confirm the toolchain is
		// actually present (a matching image name is no guarantee).
		bins, ok := langToolchainBinaries[lang]
		if !ok || len(bins) == 0 {
			continue
		}
		absent, notLocal := probeImageBinaries(ctx, env, rt, image, bins)
		if notLocal {
			out = append(out, checkResult{
				Name:   "image toolchain probe " + string(lang),
				Status: statusInfo,
				Detail: "image " + image + " not present locally; skipping binary probe for " + string(lang) + " toolchain (pull the image first to enable the probe)",
			})
			continue
		}
		if len(absent) > 0 {
			suggest := langToolchainSuggestions[lang]
			detail := string(lang) + " toolchain probe: image " + image + " is missing " + strings.Join(absent, ", ") + " — repro/verify will fail; consider using " + suggest
			out = append(out, checkResult{
				Name:   "image toolchain probe " + string(lang),
				Status: statusWarn,
				Detail: detail,
			})
		}
	}
	// Offline (network=none) bazel repro IS supported — but only against a
	// purpose-built offline image that bakes the three ingredients (vendored
	// external deps + a prefetched bazel repository cache + a warm disk-cache
	// layer; build it with `bugbot sandbox build`). We cannot prove the image
	// carries them, so we use a name heuristic: a plain public bazel base
	// (bazel-public, or the recommended default) is almost certainly NOT
	// purpose-built → WARN; any custom/local image gets an advisory INFO. Both
	// are advisory only and never affect the exit code.
	if containsBuildSystemBazel(buildSystems) && strings.EqualFold(cfg.Sandbox.Network, "none") {
		plainBazelBase := image == "" ||
			strings.Contains(imageLower, "bazel-public") ||
			imageLower == strings.ToLower(bazelBaseImage)
		if plainBazelBase {
			out = append(out, checkResult{
				Name:   "image bazel offline",
				Status: statusWarn,
				Detail: "Bazel detected with sandbox.network=none and a plain bazel base image (" + image + "): offline bazel repro needs a purpose-built offline image carrying vendored external deps + a prefetched bazel repository cache + a warm disk-cache layer — build one with `bugbot sandbox build`",
			})
		} else {
			out = append(out, checkResult{
				Name:   "image bazel offline",
				Status: statusInfo,
				Detail: "Bazel detected with sandbox.network=none: offline bazel repro IS supported when the sandbox image is purpose-built — image " + image + " must carry vendored external deps + a prefetched bazel repository cache + a warm disk-cache layer (build it with `bugbot sandbox build`)",
			})
		}
	}
	return out
}

// containsBuildSystemBazel reports whether buildSystems contains Bazel. It
// is a local helper for checkImageToolchain so this file does not have to
// depend on the prime file's containsBuildSystem.
func containsBuildSystemBazel(buildSystems []ingest.BuildSystem) bool {
	for _, b := range buildSystems {
		if b == ingest.BuildSystemBazel {
			return true
		}
	}
	return false
}

// sandboxVerifyTimeout bounds the live smoke-test run. Generous because the
// image may need to be pulled and the smoke command (e.g. go vet ./...) has to
// build index caches on first run.
const sandboxVerifyTimeout = 3 * time.Minute

// checkSandboxVerifier runs the repro.VerifySandbox smoke-test against the
// configured image and emits PASS/FAIL + category. It is only called when
// --verify-sandbox is set. The existing cheap checkImageToolchain name-match
// warn is always emitted; this is the authoritative live check.
func checkSandboxVerifier(ctx context.Context, env doctorEnv, cfg config.Config) []checkResult {
	tctx, cancel := context.WithTimeout(ctx, sandboxVerifyTimeout)
	defer cancel()

	var verdict repro.SmokeVerdict
	var err error
	if env.verifySandbox != nil {
		verdict, err = env.verifySandbox(tctx, env.repoDir, cfg)
	} else {
		verdict, err = repro.RunSandboxVerify(tctx, env.repoDir, cfg)
	}
	if err != nil {
		return []checkResult{{
			Name:   "sandbox verifier",
			Status: statusFail,
			Detail: "smoke-test failed to run: " + err.Error(),
		}}
	}
	if verdict.OK {
		return []checkResult{{
			Name:   "sandbox verifier",
			Status: statusPass,
			Detail: "toolchain smoke PASS (" + string(verdict.Category) + "): " + verdict.Detail,
		}}
	}
	return []checkResult{{
		Name:   "sandbox verifier",
		Status: statusFail,
		Detail: "toolchain smoke FAIL (" + string(verdict.Category) + "): " + verdict.Detail,
	}}
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
