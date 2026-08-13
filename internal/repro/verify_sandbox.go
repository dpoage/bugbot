package repro

import (
	"context"
	"fmt"
	"path/filepath"
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
	// ExitCode is the smoke command's process exit code (sandbox.Result's
	// ExitCode, carried through unchanged). sandbox.Result already reports
	// -1 on a watchdog/idle-timeout kill (see bwrap.go/cli.go/hostexec.go),
	// so classifySmoke does not need a separate synthetic sentinel for the
	// timeout branch — it just forwards res.ExitCode like every other
	// branch (bugbot-6835 design decision).
	ExitCode int
	// Detail is a short, roughly-one-line, exit-code-prefixed summary
	// ("exit 127: <head+tail excerpt>"). Kept deliberately compact (see
	// smokeDetailBudget) so the existing single-line BlocksRepro
	// diagnostics that interpolate it directly — cli/daemon.go:141,
	// engine/repro.go:182,513 — keep reading well without needing an edit,
	// while still carrying the exit code and a head+tail (not head-only)
	// excerpt (bugbot-6835).
	Detail string
	// FullOutput is the head+tail-preserved combined stdout+stderr
	// (headTailExcerpt at smokeFullOutputBudget, >=2000 chars). This is the
	// fuller record bugbot-cbm5 needed: bazel/entrypoint root causes print
	// LAST, after image-pull/setup noise, and a 300-char head-only cap hid
	// them across three live runs. Populated by every classifySmoke branch;
	// left empty by verdicts that short-circuit before any smoke command
	// ran (unprobeable, sandbox-exec infrastructure failure) since there is
	// no captured output to show. doctor's checkSandboxVerifier renders
	// this (falling back to Detail when empty); the terser BlocksRepro
	// diagnostics above intentionally keep using Detail, not this field
	// (bugbot-6835).
	FullOutput string
	// Launcher is the base name of the toolchain binary the smoke command
	// probed ("go", "python", "bazel", ...), from smokeCmd's suite
	// detection. BlocksRepro keys on it: build-driver launchers must not
	// gate the whole stage (see below).
	Launcher string
}

// BlocksRepro reports whether this verdict should gate the repro stage off
// entirely: the sandbox demonstrably cannot run the target ecosystem, so
// every per-finding attempt would burn budget on environment_error
// (bugbot-u6td). Timeout, dep_missing, and unprobeable do NOT block — the
// first two mean the toolchain responded; unprobeable means we simply had no
// probe to run, which is no evidence the sandbox is broken.
//
// Build-driver launchers (bazel/bazelisk) NEVER block, whatever the category
// (bugbot-4z7m): on a multi-language repo, detectSuiteCmd picks the build
// driver as THE canonical launcher, so a sandbox without bazel — the normal
// state under the bwrap backend — would disable the whole stage even though
// the probed language capabilities (python, node, go) are fully usable. The
// per-finding claim gate (bugbot-14g0 blocked_toolchain) and the pre-launch
// plan gate (bugbot-rj3z) now provide the budget protection u6td wanted, at
// per-finding granularity, with per-finding visibility instead of a silent
// stage skip.
func (v SmokeVerdict) BlocksRepro() bool {
	if v.Launcher == "bazel" || v.Launcher == "bazelisk" {
		return false
	}
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
	cmd, launcher := smokeCmd(repoDir)
	if len(cmd) == 0 {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryUnprobeable,
			ExitCode: -1, // no command ever ran — -1 avoids reading as exit-0 success (bugbot-6835).
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
		RWMounts:    append(append([]sandbox.ROMount(nil), spec.RWMounts...), res.RWMounts...),
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
			ExitCode: -1, // sb.Exec itself errored before any exit code existed (bugbot-6835).
			Detail:   "sandbox exec failed: " + err.Error(),
			Launcher: launcher,
		}, err
	}

	verdict := classifySmoke(result, cmd)
	verdict.Launcher = launcher
	return verdict, nil
}

// smokeCmd picks the cheapest toolchain liveness probe for repoDir and names
// the launcher binary it probes (SmokeVerdict.Launcher).
// It first calls detectSuiteCmd (same package, patch.go) to get the canonical
// suite launcher, then maps it to a cheaper offline probe where available.
// Falls back to a bare version probe when no suite cmd is detected, and
// returns (nil, "") when even that cannot be inferred.
func smokeCmd(repoDir string) ([]string, string) {
	suite := detectSuiteCmd(repoDir)
	if len(suite) == 0 {
		return nil, ""
	}

	// Map the canonical suite command's head binary to a cheap liveness probe.
	// We walk through any "bash -c ..." wrapper automatically because
	// detectSuiteCmd always returns a real argv, not a shell string.
	switch suite[0] {
	case "go":
		// go vet is faster than go test and still exercises the build toolchain.
		return []string{"go", "vet", "./..."}, "go"
	case "cargo":
		// cargo metadata is essentially instantaneous (no compilation).
		return []string{"cargo", "metadata", "--no-deps", "--format-version=1"}, "cargo"
	case "python", "python3":
		// pytest --collect-only exercises the import machinery without running tests.
		return []string{"python", "-m", "pytest", "--collect-only", "-q"}, "python"
	case "npm":
		return []string{"node", "--version"}, "node"
	case "pnpm":
		return []string{"node", "--version"}, "node"
	case "bazel", "bazelisk":
		// bazel version is the only thing that works offline without a
		// workspace. Probe through EITHER launcher name: bazelisk is the
		// bazel launcher and is commonly installed under its own name only
		// (bugbot-4z7m); a bare `bazel version` argv would misreport such a
		// host as toolchain_missing. exec preserves the launcher's own exit
		// code; exit 127 when neither name resolves keeps classifySmoke's
		// toolchain_missing semantics.
		return []string{"/bin/sh", "-c",
			"command -v bazel >/dev/null 2>&1 && exec bazel version; " +
				"command -v bazelisk >/dev/null 2>&1 && exec bazelisk version; " +
				"exit 127"}, "bazel"
	case "bash":
		// C/C++ suites are compound `bash -c "cmake ... && ctest ..."` strings.
		// Probe the toolchain version instead of running the full
		// configure+build+test, which would defeat the smoke test's purpose.
		if len(suite) >= 3 {
			switch {
			case strings.HasPrefix(suite[2], "cmake"):
				return []string{"cmake", "--version"}, "cmake"
			case strings.HasPrefix(suite[2], "meson"):
				return []string{"meson", "--version"}, "meson"
			}
		}
	}

	// Fallback: return the suite command as-is; better than nothing.
	return suite, filepath.Base(suite[0])
}

// smokeDetailBudget bounds SmokeVerdict.Detail: kept close to the historical
// 300-char size so the single-line BlocksRepro diagnostics that interpolate
// Detail directly (cli/daemon.go:141, engine/repro.go:182,513) do not grow
// into multi-KB messages — that is what FullOutput/smokeFullOutputBudget is
// for (bugbot-6835).
const smokeDetailBudget = 300

// smokeFullOutputBudget bounds SmokeVerdict.FullOutput. >=2000 chars per
// bugbot-6835 (a 300-char head-only cap hid the bugbot-cbm5 ENTRYPOINT
// argv-mangling root cause across three live runs, which printed only in
// the tail); sized generously above the floor so a real bazel/toolchain
// failure dump — image-pull noise, then the actual diagnostic — fits both
// ends without elision in the common case.
const smokeFullOutputBudget = 4096

// headTailExcerpt keeps a head prefix AND a tail suffix of s (split evenly
// across budget, rune-safe cut points so no UTF-8 sequence is split), with
// an elision marker recording how many bytes were dropped in between.
// Unlike trunc (head-only) and tailExcerpt (tail-only, both in
// interpret.go), this preserves both ends: sandbox failures often print
// environment/setup noise FIRST and the actual diagnostic (compile error,
// "not found", entrypoint mangling) LAST, and either end can carry the
// signal depending on the failure mode (bugbot-6835 / bugbot-cbm5).
func headTailExcerpt(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	headLen := budget / 2
	tailLen := budget - headLen

	// Walk the head boundary back to the start of a rune (never split a
	// UTF-8 continuation byte, mirrors tailExcerpt's boundary walk below).
	for headLen > 0 && s[headLen]&0xC0 == 0x80 {
		headLen--
	}
	// Walk the tail boundary forward to the start of a rune.
	tailStart := len(s) - tailLen
	for tailStart < len(s) && s[tailStart]&0xC0 == 0x80 {
		tailStart++
	}

	elided := tailStart - headLen
	return fmt.Sprintf("%s\n... [%d bytes elided] ...\n%s", s[:headLen], elided, s[tailStart:])
}

// smokeDetail formats SmokeVerdict.Detail: an "exit N: " prefix — so the
// BlocksRepro diagnostics that interpolate Detail directly surface the exit
// code without needing an edit — followed by an optional note and a
// head+tail excerpt bounded at smokeDetailBudget. note is inserted plain
// (e.g. "smoke command timed out"); pass "" when the exit code and excerpt
// alone are enough (bugbot-6835).
func smokeDetail(res sandbox.Result, out, note string) string {
	excerpt := headTailExcerpt(out, smokeDetailBudget)
	if note == "" {
		return fmt.Sprintf("exit %d: %s", res.ExitCode, excerpt)
	}
	return fmt.Sprintf("exit %d: %s: %s", res.ExitCode, note, excerpt)
}

// classifySmoke turns a sandbox.Result from a smoke run into a SmokeVerdict.
// The classification mirrors interpret() in interpret.go:
//   - TimedOut                          → timeout
//   - ExitCode 125/126/127              → toolchain_missing (env-level failure)
//   - Non-zero + defaultEnvMarkers      → env_error
//   - Non-zero + toolchain absent hints → toolchain_missing
//   - Non-zero + "dep" / "module" hints → dep_missing
//   - Zero OR genuine run output        → ok  (toolchain responded)
//
// Every branch carries res.ExitCode (already -1 on timeout, per
// sandbox.Result's own contract) and a FullOutput head+tail excerpt at
// smokeFullOutputBudget, alongside the terser exit-code-prefixed Detail
// (bugbot-6835).
func classifySmoke(res sandbox.Result, cmd []string) SmokeVerdict {
	out := res.Stdout + "\n" + res.Stderr
	full := headTailExcerpt(out, smokeFullOutputBudget)

	if res.TimedOut {
		return SmokeVerdict{
			OK:         false,
			Category:   SmokeCategoryTimeout,
			ExitCode:   res.ExitCode,
			Detail:     smokeDetail(res, out, "smoke command timed out"),
			FullOutput: full,
		}
	}

	if res.ExitCode == 0 {
		return SmokeVerdict{OK: true, Category: SmokeCategoryOK, ExitCode: res.ExitCode, Detail: smokeDetail(res, out, ""), FullOutput: full}
	}

	// Exit 125/126/127 mean the container runtime or shell failed before
	// the command ran — the toolchain is missing or the image is wrong.
	if res.ExitCode == 125 || res.ExitCode == 126 || res.ExitCode == 127 {
		return SmokeVerdict{
			OK:         false,
			Category:   SmokeCategoryToolchainMissing,
			ExitCode:   res.ExitCode,
			Detail:     smokeDetail(res, out, ""),
			FullOutput: full,
		}
	}

	lowOut := strings.ToLower(out)

	// Environment-level failures (read-only fs, disk full, cache init) — same
	// markers as interpret.go defaultEnvMarkers.
	if hasAnyMarker(lowOut, defaultEnvMarkers) {
		return SmokeVerdict{
			OK:         false,
			Category:   SmokeCategoryEnvError,
			ExitCode:   res.ExitCode,
			Detail:     smokeDetail(res, out, ""),
			FullOutput: full,
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
			OK:         false,
			Category:   SmokeCategoryToolchainMissing,
			ExitCode:   res.ExitCode,
			Detail:     smokeDetail(res, out, ""),
			FullOutput: full,
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
			OK:         false,
			Category:   SmokeCategoryDepMissing,
			ExitCode:   res.ExitCode,
			Detail:     smokeDetail(res, out, ""),
			FullOutput: full,
		}
	}

	// Any other non-zero exit means the toolchain RAN but something went
	// wrong (compile error, test failure, etc.).  That is "ok" for our
	// purposes: we only care that the toolchain is present and functional.
	return SmokeVerdict{OK: true, Category: SmokeCategoryOK, ExitCode: res.ExitCode, Detail: smokeDetail(res, out, ""), FullOutput: full}
}

// localMountsFromConfig converts cfg.Sandbox.LocalMounts into read-only
// sandbox.ROMounts for RunSandboxVerify's dep resolution (duplicated here
// rather than imported: engine depends on repro, so importing
// engine.LocalMountsFromConfig from here would cycle — see
// newVerifySandbox's doc for the same constraint). Split by
// config.LocalMount.Writable into a read-only slice (ro) and a writable
// slice (rw) exactly like engine.localMountsFromConfig (bugbot-wjc2), so
// the smoke run mounts a writable vendor/disk-cache dir the same way a
// real repro run does. Shared=true on both halves: host-owned dirs that
// must not be SELinux :Z relabeled.
func localMountsFromConfig(cfg config.Config) (ro, rw []sandbox.ROMount) {
	for _, m := range cfg.Sandbox.LocalMounts {
		mount := sandbox.ROMount{HostPath: m.Host, ContainerPath: m.Container, Shared: true}
		if m.Writable {
			rw = append(rw, mount)
		} else {
			ro = append(ro, mount)
		}
	}
	return ro, rw
}

// newVerifySandbox constructs the sandbox backend RunSandboxVerify probes
// against, selected by cfg.Sandbox.Backend exactly like
// engine.newConfiguredSandbox (duplicated here rather than imported: engine
// depends on repro, so importing engine from here would cycle). Toolchain
// binds/PATH/fingerprint are wired for bwrap exactly as the real repro path
// wires them, so the smoke probe sees the same environment a real run would
// — a toolchain that only exists via sandbox.host_toolchains must not read
// as "missing" here.
func newVerifySandbox(cfg config.Config) (sandbox.Sandbox, error) {
	if cfg.Sandbox.Backend == "bwrap" {
		var opts []sandbox.BwrapOption
		if cfg.Sandbox.PidsLimit > 0 {
			opts = append(opts, sandbox.WithBwrapPidsLimit(cfg.Sandbox.PidsLimit))
		}
		if cfg.Sandbox.CPUs > 0 {
			opts = append(opts, sandbox.WithBwrapCPUs(float64(cfg.Sandbox.CPUs)))
		}
		if cfg.Sandbox.MemoryMB > 0 {
			opts = append(opts, sandbox.WithBwrapMemoryMB(cfg.Sandbox.MemoryMB))
		}
		if cfg.Sandbox.AllowUncapped {
			opts = append(opts, sandbox.WithBwrapAllowUncapped(true))
		}
		if len(cfg.Sandbox.HostToolchains) > 0 {
			res, err := sandbox.ResolveHostToolchains(cfg.Sandbox.HostToolchains)
			if err != nil {
				return nil, fmt.Errorf("resolve host toolchains: %w", err)
			}
			opts = append(opts,
				sandbox.WithBwrapToolchainBinds(res.Mounts),
				sandbox.WithBwrapToolchainPath(res.PathPrepend),
			)
		}
		return sandbox.NewBwrap(opts...)
	}
	var sbOpts []sandbox.Option
	if cfg.Sandbox.PidsLimit > 0 {
		// Honor the configured pids cap for the smoke run too, so doctor uses the
		// same process/thread budget as real repro/verify runs (the backend
		// default of 256 is too low for heavy toolchains like the Bazel JVM).
		sbOpts = append(sbOpts, sandbox.WithPidsLimit(cfg.Sandbox.PidsLimit))
	}
	return sandbox.NewCLI(cfg.Sandbox.Runtime, cfg.Sandbox.Image, sbOpts...)
}

// RunSandboxVerify is the convenience entry-point called by doctor.
// It constructs the configured sandbox backend (container CLI or bwrap),
// resolves the configured dep strategy, and delegates to VerifySandbox. The
// repoDir is the working directory used for both dep resolution and
// smoke-command detection.
func RunSandboxVerify(ctx context.Context, repoDir string, cfg config.Config) (SmokeVerdict, error) {
	// sandbox.image is meaningless for bwrap (see Bwrap's doc comment) so this
	// hard requirement applies only to the container backend.
	if cfg.Sandbox.Backend != "bwrap" && cfg.Sandbox.Image == "" {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   "sandbox.image is not configured",
		}, nil
	}

	sb, err := newVerifySandbox(cfg)
	if err != nil {
		return SmokeVerdict{
			OK:       false,
			Category: SmokeCategoryEnvError,
			Detail:   "could not create sandbox: " + err.Error(),
		}, err
	}

	localRO, localRW := localMountsFromConfig(cfg)
	opts := sandbox.DepOptions{
		Strategy: sandbox.DepStrategy(cfg.Sandbox.DepStrategy),
		// FetchSandbox/FetchImage/LocalMounts/HostToolchains, threaded so
		// doctor sees the SAME dependency provisioning a real repro run
		// would have (bugbot-48ya acceptance 3): without FetchSandbox,
		// dep_strategy: fetch unconditionally errored here ("requires a
		// fetch sandbox") even when the real repro path resolves it fine;
		// without LocalMounts/HostToolchains, the smoke run never saw an
		// operator's local_mounts/host_toolchains entries. ResolveDeps'
		// FETCH branch only builds a Prefetch closure — it is not invoked
		// below, so this has no network side effect.
		FetchSandbox:   sb,
		FetchImage:     cfg.Sandbox.Image,
		LocalMounts:    localRO,
		LocalRWMounts:  localRW,
		HostToolchains: cfg.Sandbox.HostToolchains,
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
		Image:    cfg.Sandbox.Image,
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
	// Keying purely on cfg.Sandbox.Image would collapse every bwrap config
	// (Image is always "" there) onto one cache entry regardless of which
	// host toolchains are configured; fold Backend + HostToolchains in too so
	// a distinct bwrap configuration gets its own probe.
	key := repoDir + "\x00" + cfg.Sandbox.Backend + "\x00" + cfg.Sandbox.Image + "\x00" + strings.Join(cfg.Sandbox.HostToolchains, ",")
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
