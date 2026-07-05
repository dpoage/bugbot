package repro

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// SmokeCategory is the classification of a toolchain smoke-test result.
// Typed so callers switch on it exhaustively instead of comparing bare strings.
type SmokeCategory string

const (
	// SmokeCategoryOK: the toolchain responded — either a clean exit or a
	// genuine test/compile failure. A genuine failure still means "the
	// toolchain ran", which is what we probe for.
	SmokeCategoryOK SmokeCategory = "ok"
	// SmokeCategoryTimeout: the smoke command exceeded its deadline.
	SmokeCategoryTimeout SmokeCategory = "timeout"
	// SmokeCategoryEnvError: the sandbox environment failed (read-only
	// filesystem, disk full, cache init error, sandbox exec failure, etc.).
	SmokeCategoryEnvError SmokeCategory = "env_error"
	// SmokeCategoryToolchainMissing: the toolchain binary is absent or the
	// container image lacks the required runtime (exit 125/126/127, or
	// "command not found" / "no such file" in output).
	SmokeCategoryToolchainMissing SmokeCategory = "toolchain_missing"
	// SmokeCategoryDepMissing: the toolchain is present but required
	// dependencies are missing (missing module, missing package, etc.).
	SmokeCategoryDepMissing SmokeCategory = "dep_missing"
	// SmokeCategoryUnprobeable: no toolchain smoke command could be derived
	// for this repo (unknown ecosystem — no suite command, no version probe).
	// Absence of evidence, not evidence of a broken sandbox: the repro stage
	// is NOT gated on it (BlocksRepro reports false).
	SmokeCategoryUnprobeable SmokeCategory = "unprobeable"
)

// SmokeVerdict is the result of a toolchain smoke-test inside a sandbox.
// It uses a vocabulary that mirrors interpret.go's verdict categories so that
// the designer, the sandbox verifier, and doctor all speak the same language.
type SmokeVerdict struct {
	// OK is true when the toolchain responded — either a clean exit or a
	// genuine test/compile failure.  A genuine failure still means "the
	// toolchain ran", which is what we are probing for.
	OK bool
	// Category classifies the outcome.
	Category SmokeCategory
	// Detail is a short human-readable explanation (truncated output).
	Detail string
}

// BlocksRepro reports whether this verdict should gate the repro stage off
// entirely: the image demonstrably cannot run the target ecosystem, so every
// per-finding attempt would burn budget on environment_error (bugbot-u6td).
// Timeout, dep_missing, and unprobeable do NOT block — the first two mean the
// toolchain responded; unprobeable means we simply had no probe to run, which
// is no evidence the sandbox is broken.
func (v SmokeVerdict) BlocksRepro() bool {
	return v.Category == SmokeCategoryToolchainMissing || v.Category == SmokeCategoryEnvError
}

// smokeTimeout is the ceiling for a single smoke-test run.  We keep it
// shorter than the full repro timeout (90 s) because the smoke command is
// chosen specifically to be cheap.
const smokeTimeout = 45 * time.Second

// VerifySandbox runs a cheap toolchain smoke-test inside the sandbox described
// by spec and res, under network=none, and returns a structured SmokeVerdict.
//
// The smoke command is derived from repoDir using detectSuiteCmd (same-package
// call into patch.go; same logic the reproducer uses) and then mapped to a
// cheaper liveness probe per ecosystem.  When detectSuiteCmd returns nil
// (unknown toolchain) a bare version probe ("go version" / "python --version"
// etc.) is attempted; if we cannot derive any probe the verdict is
// unprobeable with a helpful message.
//
// Classification re-uses the same defaultEnvMarkers / hasAnyMarker /
// exit-125-126-127 logic from ecosystem.go that interpret() uses, so the
// verifier and the reproducer pipeline share a single classification layer.
//
// Important: a genuine test failure (toolchain ran, tests failed) is
// classified as "ok" because we only care whether the toolchain IS PRESENT and
// FUNCTIONAL — not whether the project's tests pass.
func VerifySandbox(ctx context.Context, sb sandbox.Sandbox, repoDir string, spec sandbox.Spec, res sandbox.Resolution) (SmokeVerdict, error) {
	cmd := smokeCmd(repoDir)
	if len(cmd) == 0 {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryUnprobeable,
			Detail:   "could not derive a toolchain smoke command for this repo",
		}, nil
	}

	// Build the spec from the resolved deps (mounts/env/setup) so we behave
	// exactly as the real run would.
	runSpec := sandbox.Spec{
		RepoDir:     repoDir,
		Cmd:         cmd,
		Image:       spec.Image,
		Env:         append(append([]string(nil), spec.Env...), res.Env...),
		ROMounts:    append(append([]sandbox.ROMount(nil), spec.ROMounts...), res.ROMounts...),
		SetupCmds:   res.SetupCmds,
		Network:     "none",
		Timeout:     smokeTimeout,
		IdleTimeout: spec.IdleTimeout,
		CPUs:        spec.CPUs,
		MemoryMB:    spec.MemoryMB,
	}

	result, err := sb.Exec(ctx, runSpec)
	if err != nil {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   "sandbox exec failed: " + err.Error(),
		}, err
	}

	return classifySmoke(result, cmd), nil
}

// smokeCmd picks the cheapest toolchain liveness probe for repoDir.
// It first calls detectSuiteCmd (same package, patch.go) to get the canonical
// suite launcher, then maps it to a cheaper offline probe where available.
// Falls back to a bare version probe when no suite cmd is detected, and
// returns nil when even that cannot be inferred.
func smokeCmd(repoDir string) []string {
	suite := detectSuiteCmd(repoDir)
	if len(suite) == 0 {
		return nil
	}

	// Map the canonical suite command's head binary to a cheap liveness probe.
	// We walk through any "bash -c ..." wrapper automatically because
	// detectSuiteCmd always returns a real argv, not a shell string.
	switch suite[0] {
	case "go":
		// go vet is faster than go test and still exercises the build toolchain.
		return []string{"go", "vet", "./..."}
	case "cargo":
		// cargo metadata is essentially instantaneous (no compilation).
		return []string{"cargo", "metadata", "--no-deps", "--format-version=1"}
	case "python", "python3":
		// pytest --collect-only exercises the import machinery without running tests.
		return []string{"python", "-m", "pytest", "--collect-only", "-q"}
	case "npm":
		return []string{"node", "--version"}
	case "pnpm":
		return []string{"node", "--version"}
	case "bazel":
		// bazel version is the only thing that works offline without a workspace.
		return []string{"bazel", "version"}
	case "bash":
		// C/C++ suites are compound `bash -c "cmake ... && ctest ..."` strings.
		// Probe the toolchain version instead of running the full
		// configure+build+test, which would defeat the smoke test's purpose.
		if len(suite) >= 3 {
			switch {
			case strings.HasPrefix(suite[2], "cmake"):
				return []string{"cmake", "--version"}
			case strings.HasPrefix(suite[2], "meson"):
				return []string{"meson", "--version"}
			}
		}
	}

	// Fallback: return the suite command as-is; better than nothing.
	return suite
}

// classifySmoke turns a sandbox.Result from a smoke run into a SmokeVerdict.
// The classification mirrors interpret() in interpret.go:
//   - TimedOut                          → timeout
//   - ExitCode 125/126/127              → toolchain_missing (env-level failure)
//   - Non-zero + defaultEnvMarkers      → env_error
//   - Non-zero + toolchain absent hints → toolchain_missing
//   - Non-zero + "dep" / "module" hints → dep_missing
//   - Zero OR genuine run output        → ok  (toolchain responded)
func classifySmoke(res sandbox.Result, cmd []string) SmokeVerdict {
	out := res.Stdout + "\n" + res.Stderr

	if res.TimedOut {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryTimeout,
			Detail:   "smoke command timed out: " + trunc(out, 300),
		}
	}

	if res.ExitCode == 0 {
		return SmokeVerdict{OK: true, Category: SmokeCategoryOK, Detail: trunc(out, 300)}
	}

	// Exit 125/126/127 mean the container runtime or shell failed before
	// the command ran — the toolchain is missing or the image is wrong.
	if res.ExitCode == 125 || res.ExitCode == 126 || res.ExitCode == 127 {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryToolchainMissing,
			Detail:   trunc(out, 300),
		}
	}

	lowOut := strings.ToLower(out)

	// Environment-level failures (read-only fs, disk full, cache init) — same
	// markers as interpret.go defaultEnvMarkers.
	if hasAnyMarker(lowOut, defaultEnvMarkers) {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   trunc(out, 300),
		}
	}

	// Command-not-found or similar toolchain-absence signals.
	toolchainAbsentMarkers := []string{
		"command not found",
		"executable file not found",
		"no such file or directory",
		"not found",
	}
	if hasAnyMarker(lowOut, toolchainAbsentMarkers) {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryToolchainMissing,
			Detail:   trunc(out, 300),
		}
	}

	// Dependency resolution failures (missing module, missing package).
	depMarkers := []string{
		"no required module provides",
		"cannot find module",
		"missing go.sum entry",
		"modulenotfounderror",
		"no module named",
		"cannot find package",
		"unresolved import",
		"package not found",
	}
	if hasAnyMarker(lowOut, depMarkers) {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryDepMissing,
			Detail:   trunc(out, 300),
		}
	}

	// Any other non-zero exit means the toolchain RAN but something went
	// wrong (compile error, test failure, etc.).  That is "ok" for our
	// purposes: we only care that the toolchain is present and functional.
	return SmokeVerdict{OK: true, Category: SmokeCategoryOK, Detail: trunc(out, 300)}
}

// RunSandboxVerify is the convenience entry-point called by doctor.
// It constructs a sandbox.CLI from cfg, resolves the configured dep strategy,
// and delegates to VerifySandbox.  The repoDir is the working directory used
// for both dep resolution and smoke-command detection.
func RunSandboxVerify(ctx context.Context, repoDir string, cfg config.Config) (SmokeVerdict, error) {
	rt := cfg.Sandbox.Runtime
	image := cfg.Sandbox.Image
	if image == "" {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   "sandbox.image is not configured",
		}, nil
	}

	var sbOpts []sandbox.Option
	if cfg.Sandbox.PidsLimit > 0 {
		// Honor the configured pids cap for the smoke run too, so doctor uses the
		// same process/thread budget as real repro/verify runs (the backend
		// default of 256 is too low for heavy toolchains like the Bazel JVM).
		sbOpts = append(sbOpts, sandbox.WithPidsLimit(cfg.Sandbox.PidsLimit))
	}
	sb, err := sandbox.NewCLI(rt, image, sbOpts...)
	if err != nil {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   "could not create sandbox CLI: " + err.Error(),
		}, err
	}

	opts := sandbox.DepOptions{
		Strategy: sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
	}
	res, err := sandbox.ResolveDeps(repoDir, opts)
	if err != nil {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   "could not resolve dependencies: " + err.Error(),
		}, err
	}

	spec := sandbox.Spec{
		Image:    image,
		CPUs:     float64(cfg.Sandbox.CPUs),
		MemoryMB: cfg.Sandbox.MemoryMB,
	}
	if cfg.Sandbox.IdleTimeoutSeconds > 0 {
		spec.IdleTimeout = time.Duration(cfg.Sandbox.IdleTimeoutSeconds) * time.Second
	}

	return VerifySandbox(ctx, sb, repoDir, spec, res)
}

// smokeCache holds per-(repoDir, image) cached smoke verdicts so each probe
// runs at most once per process invocation even when multiple callers race at
// startup. Keyed rather than global: a process probing a second repo or a
// reconfigured image must not inherit the first probe's verdict.
var smokeCache = struct {
	mu sync.Mutex
	m  map[string]*smokeEntry
}{m: make(map[string]*smokeEntry)}

type smokeEntry struct {
	once    sync.Once
	verdict SmokeVerdict
	err     error
}

// VerifySandboxOnce runs the sandbox toolchain smoke probe exactly once per
// (repoDir, sandbox image) pair per process lifetime. Subsequent calls with
// the same pair return the cached result without re-running the probe. Safe
// for concurrent callers.
//
// Callers should check verdict.BlocksRepro():
//   - true (toolchain_missing / env_error) → skip the repro stage.
//   - false (ok / dep_missing / timeout) → proceed (deps may be absent but
//     the toolchain ran, so repro attempts are meaningful).
func VerifySandboxOnce(ctx context.Context, repoDir string, cfg config.Config) (SmokeVerdict, error) {
	key := repoDir + "\x00" + cfg.Sandbox.Image
	smokeCache.mu.Lock()
	e, ok := smokeCache.m[key]
	if !ok {
		e = &smokeEntry{}
		smokeCache.m[key] = e
	}
	smokeCache.mu.Unlock()
	e.once.Do(func() {
		e.verdict, e.err = RunSandboxVerify(ctx, repoDir, cfg)
	})
	return e.verdict, e.err
}
