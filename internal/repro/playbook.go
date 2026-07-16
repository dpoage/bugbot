package repro

// playbook.go implements bugbot-u2v5: a per-repo "verified command
// playbook". Today's preflight layers answer "is the toolchain alive"
// (VerifySandboxOnce) and "what modes does this image support" (probed
// CapabilitySet) but neither answers "which concrete launcher actually
// works in THIS repo, in THIS exact sandbox spec (same mounts/env/
// resolution a real repro run gets)". A plan that reaches for `npx jest`
// dies with "npx: not found" even after prior iteration proved `node
// --test` works; this battery runs once, cheaply, and records the answer
// so both the reproducer prompt and the pre-launch plan gate can steer
// around a launcher already known to fail — see agent.go's playbookGuidance
// hook and repro.go's rejectPlaybookFailedLaunch hook.
//
// Degradation rule (CRITICAL, matches VerifySandboxOnce's contract): a
// battery that cannot run at all (sandbox exec infrastructure failure) or
// times out mid-battery collapses to an EMPTY Playbook, never an error that
// blocks the reproduce stage. Callers treat len(Playbook.Verdicts) == 0 as
// "inactive" — no prompt section, gate never fires — exactly today's
// pre-bugbot-u2v5 behavior.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/util"
)

// playbookProbeTimeout bounds a single launcher probe. Kept short: every
// probe command below is a cheap `--version`/`--help` invocation, not a real
// test run.
const playbookProbeTimeout = 15 * time.Second

// playbookBatteryTimeout is the ceiling for the WHOLE battery (all probes
// for all detected ecosystems). A probe that would start after this ceiling
// is reached is recorded FAILS with a reason instead of run.
const playbookBatteryTimeout = 90 * time.Second

// PlaybookVerdict is the recorded outcome of probing one canonical launcher
// inside the real repro sandbox spec.
type PlaybookVerdict struct {
	// Ecosystem is the launcher's ecosystem (see ecosystem.Ecosystem), used
	// to prefer a same-family alternative when this launcher FAILS.
	Ecosystem ecosystem.Ecosystem
	// Launcher is the canonical binary base name probed, e.g. "python3",
	// "pytest", "node", "npx", "go", "cargo", "bazel".
	Launcher string
	// Verified is true when the probe exited 0 inside the real sandbox spec.
	Verified bool
	// Inconclusive is true when the probe was cut off (per-probe timeout) or
	// never ran evidence-bearing to completion — as opposed to a CONFIRMED
	// non-zero exit. Distinct from a plain FAILS: neither
	// rejectPlaybookFailedLaunch nor playbookGuidance's "Do NOT propose"
	// bullet treat an Inconclusive verdict as evidence the launcher is
	// broken (a slow-but-working launcher must not be gated out).
	Inconclusive bool
	// Reason is set when !Verified: a short, single-line (util.FlattenField'd)
	// explanation ("not found", "timed out after 15s", or a truncated
	// evidence tail) suitable for both the reproducer prompt and plan-gate
	// feedback — flattened so raw sandbox stdout/stderr can never inject
	// newlines that break the one-bullet-per-verdict prompt structure or
	// fake section headers into either surface.
	Reason string
}

// Playbook is the per-repo battery result: which canonical launchers this
// exact repo+sandbox combination can actually run. A zero-value Playbook
// (nil Verdicts) means the battery never ran or degraded — see the file doc.
type Playbook struct {
	Verdicts []PlaybookVerdict
}

// verdictFor returns the recorded verdict for launcher, or ok=false when the
// battery never probed it (an unprobed launcher is neither verified nor
// failed — callers must not gate on it).
func (p Playbook) verdictFor(launcher string) (PlaybookVerdict, bool) {
	for _, v := range p.Verdicts {
		if v.Launcher == launcher {
			return v, true
		}
	}
	return PlaybookVerdict{}, false
}

// alternativeTo returns a verified launcher in the SAME ecosystem family as
// launcher (e.g. "node" for a failed "npx" — both js), or ok=false when no
// such alternative exists. Deliberately narrow: a launcher from a DIFFERENT
// ecosystem (e.g. "go" for a failed "cargo") is never offered as an
// "alternative" — that would steer the agent toward substituting a
// non-behavioral test in the wrong language, which rejectPlaybookFailedLaunch
// already explicitly warns against for the sibling capability gate.
func (p Playbook) alternativeTo(launcher string) (alt string, ok bool) {
	var failedEco ecosystem.Ecosystem
	if v, found := p.verdictFor(launcher); found {
		failedEco = v.Ecosystem
	}
	for _, v := range p.Verdicts {
		if !v.Verified || v.Launcher == launcher {
			continue
		}
		if v.Ecosystem == failedEco {
			return v.Launcher, true
		}
	}
	return "", false
}

// String renders a stable, human-readable dump of the playbook (verified
// launchers first, then failed ones with their reason). Exposed for callers
// (e.g. `bugbot doctor`) that want a clean text summary without reaching
// into Verdicts themselves.
func (p Playbook) String() string {
	if len(p.Verdicts) == 0 {
		return "verified-command playbook: no data (battery did not run)"
	}
	var b strings.Builder
	b.WriteString("verified-command playbook:\n")
	for _, v := range p.Verdicts {
		if v.Verified {
			fmt.Fprintf(&b, "  %-10s VERIFIED-WORKS\n", v.Launcher)
		}
	}
	for _, v := range p.Verdicts {
		if !v.Verified {
			fmt.Fprintf(&b, "  %-10s FAILS (%s)\n", v.Launcher, v.Reason)
		}
	}
	return b.String()
}

// playbookProbe is one battery entry: the canonical launcher probed for one
// ecosystem, and the cheap argv that proves it works.
type playbookProbe struct {
	Ecosystem ecosystem.Ecosystem
	Launcher  string
	Cmd       []string
}

// playbookProbesByEcosystem is the bounded battery contents (bugbot-u2v5
// acceptance): one canonical-launcher probe per gated ecosystem, plus npx as
// a second JS probe since jest/vitest suites are commonly launched through
// it. Ordered so Playbook.Verdicts (and thus the rendered prompt section)
// has a stable, deterministic ordering across runs.
var playbookProbesByEcosystem = map[ecosystem.Ecosystem][]playbookProbe{
	ecosystem.EcosystemGo: {
		{ecosystem.EcosystemGo, "go", []string{"go", "version"}},
	},
	ecosystem.EcosystemPython: {
		// "python3" probes bare interpreter liveness only (`--version`),
		// deliberately SEPARATE from pytest importability: InferToolFromCmd
		// matches the FIRST recognized token in a plan's argv, so a plan
		// like `python3 -m unittest ...` also infers tool="python3" (the
		// "-m pytest"/"-m unittest" suffix never gets inspected) — if this
		// verdict instead measured pytest importability, a pytest-absent
		// image would record "python3: FAILS" and rejectPlaybookFailedLaunch
		// would then wrongly reject a perfectly valid `python3 -m unittest`
		// plan for a launcher that never actually failed.
		{ecosystem.EcosystemPython, "python3", []string{"python3", "--version"}},
		// "pytest" is a distinct, same-ecosystem probe for pytest
		// importability specifically. It only gates a plan whose FIRST argv
		// token is literally `pytest` (ecosystem.cmdEcosystem's "pytest"
		// entry) — a `python3 -m pytest` plan still infers tool="python3"
		// per the note above, so this verdict is reachable by the gate only
		// for bare `pytest ...` invocations, and by alternativeTo as a
		// same-ecosystem partner for python3.
		{ecosystem.EcosystemPython, "pytest", []string{"python3", "-m", "pytest", "--version"}},
	},
	ecosystem.EcosystemJS: {
		{ecosystem.EcosystemJS, "node", []string{"node", "--test", "--help"}},
		// npx is probed unconditionally whenever the JS ecosystem is
		// detected (reusing the existing ingest.DetectBuildSystems /
		// ecosystem detection rather than writing new npx-specific
		// detection, per bugbot-u2v5's scope): npx-wrapped jest/vitest
		// invocations are a common enough pattern in JS repos that the
		// cheap extra probe is worth it, and a FAILS verdict here is
		// exactly the the_cloud incident (bugbot-f36r) this bead exists to
		// catch pre-launch.
		{ecosystem.EcosystemJS, "npx", []string{"npx", "--version"}},
	},
	ecosystem.EcosystemRust: {
		{ecosystem.EcosystemRust, "cargo", []string{"cargo", "--version"}},
	},
	ecosystem.EcosystemBazel: {
		{ecosystem.EcosystemBazel, "bazel", []string{"bazel", "--version"}},
	},
}

// playbookEcosystemOrder fixes the iteration order over
// playbookProbesByEcosystem so the battery (and its resulting prompt
// section) is deterministic regardless of ingest.DetectBuildSystems' own
// slice ordering.
var playbookEcosystemOrder = []ecosystem.Ecosystem{
	ecosystem.EcosystemGo,
	ecosystem.EcosystemPython,
	ecosystem.EcosystemJS,
	ecosystem.EcosystemRust,
	ecosystem.EcosystemBazel,
}

// buildSystemEcosystem maps a detected ingest.BuildSystem to the ecosystem
// key the battery probes for it. Build systems with no playbook probe (make,
// ninja, cmake, dotnet, meson, ...) are intentionally absent.
var buildSystemEcosystem = map[ingest.BuildSystem]ecosystem.Ecosystem{
	ingest.BuildSystemBazel:       ecosystem.EcosystemBazel,
	ingest.BuildSystemGoWorkspace: ecosystem.EcosystemGo,
	ingest.BuildSystemGoModule:    ecosystem.EcosystemGo,
	ingest.BuildSystemJSWorkspace: ecosystem.EcosystemJS,
	ingest.BuildSystemNPM:         ecosystem.EcosystemJS,
	ingest.BuildSystemCargo:       ecosystem.EcosystemRust,
	ingest.BuildSystemPython:      ecosystem.EcosystemPython,
}

// detectedPlaybookEcosystems maps systems (the SAME ingest.DetectBuildSystems
// result already resolved once in New and threaded as r.buildSystems — no
// new repo detection is performed here) to the set of ecosystems the battery
// should probe, in playbookEcosystemOrder.
func detectedPlaybookEcosystems(systems []ingest.BuildSystem) []ecosystem.Ecosystem {
	present := make(map[ecosystem.Ecosystem]bool, len(systems))
	for _, bs := range systems {
		if eco, ok := buildSystemEcosystem[bs]; ok {
			present[eco] = true
		}
	}
	var out []ecosystem.Ecosystem
	for _, eco := range playbookEcosystemOrder {
		if present[eco] {
			out = append(out, eco)
		}
	}
	return out
}

// execProbe runs spec (with Timeout set to timeout) against sb, enforcing
// timeout itself via ctx cancellation rather than trusting the backend alone
// — VerifySandbox trusts the backend to honor Spec.Timeout, but the playbook
// battery additionally derives its own bounded context so a backend that
// ignores Spec.Timeout (e.g. a scripted test fake) still can't blow the
// per-probe budget. timeout is a parameter (rather than always reading the
// playbookProbeTimeout const) so tests can exercise real timeout enforcement
// against a deliberately slow fake sandbox without waiting the full 15s.
func execProbe(ctx context.Context, sb sandbox.Sandbox, spec sandbox.Spec, timeout time.Duration) (sandbox.Result, error) {
	spec.Timeout = timeout
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type outcome struct {
		res sandbox.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := sb.Exec(probeCtx, spec)
		ch <- outcome{res, err}
	}()

	select {
	case <-probeCtx.Done():
		return sandbox.Result{TimedOut: true}, nil
	case o := <-ch:
		return o.res, o.err
	}
}

// classifyPlaybookProbe turns a probe's sandbox.Result/error into a
// PlaybookVerdict. Exit 0 is VERIFIED-WORKS. A confirmed non-zero exit (the
// toolchain ran and refused, or exec itself failed with a not-found-style
// exit code) is FAILS — the playbook makes no "toolchain ran but the actual
// repro would still fail" allowance the way smoke verdicts do, because a
// wrong launcher choice is exactly what this feature exists to catch. A
// TimedOut result is Inconclusive, NOT a confirmed FAILS: the probe was cut
// off, not refused, so neither the gate nor the prompt's "Do NOT propose"
// bullet may treat it as evidence the launcher is broken (see
// PlaybookVerdict.Inconclusive). Every Reason is util.FlattenField'd before
// being stored: this is the one place raw sandbox stdout/stderr enters a
// PlaybookVerdict, and every consumer (playbookGuidance, gate feedback)
// embeds Reason inline in a single prompt/feedback line — an unflattened
// multi-line reason would break that structure and, per the codebase's
// mandatory sandbox-output-fencing convention (see util.FenceBlock's
// callers in interpret.go/agent.go/patch.go), must never reach the prompt
// unflattened.
func classifyPlaybookProbe(probe playbookProbe, res sandbox.Result, err error, timeout time.Duration) PlaybookVerdict {
	if err != nil {
		return PlaybookVerdict{
			Ecosystem: probe.Ecosystem, Launcher: probe.Launcher,
			Reason: util.FlattenField("sandbox exec failed: " + trunc(err.Error(), 120)),
		}
	}
	if res.TimedOut {
		return PlaybookVerdict{
			Ecosystem: probe.Ecosystem, Launcher: probe.Launcher,
			Inconclusive: true,
			Reason:       fmt.Sprintf("timed out after %s", timeout),
		}
	}
	if res.ExitCode == 0 {
		return PlaybookVerdict{Ecosystem: probe.Ecosystem, Launcher: probe.Launcher, Verified: true}
	}
	out := strings.ToLower(strings.TrimSpace(res.Stdout + " " + res.Stderr))
	if res.ExitCode == 126 || res.ExitCode == 127 || hasAnyMarker(out, []string{
		"command not found", "executable file not found", "no such file or directory", "not found",
	}) {
		return PlaybookVerdict{Ecosystem: probe.Ecosystem, Launcher: probe.Launcher, Reason: "not found"}
	}
	return PlaybookVerdict{
		Ecosystem: probe.Ecosystem, Launcher: probe.Launcher,
		Reason: util.FlattenField(trunc(strings.TrimSpace(res.Stdout+" "+res.Stderr), 120)),
	}
}

// runPlaybookBattery runs the bounded probe battery for every ecosystem
// detected in systems, against the SAME sandbox spec a real repro run gets
// (spec's Image/CPUs/MemoryMB/IdleTimeout plus res's ROMounts/Env/SetupCmds
// — mirrors VerifySandbox's spec assembly). probeTimeout/batteryTimeout are
// parameters (production callers pass playbookProbeTimeout/
// playbookBatteryTimeout) so tests can exercise real ceiling/timeout
// enforcement without waiting the full budget.
//
// Returns ok=false when an infra-level Exec error aborts the battery
// outright (sandbox down): callers must discard any partial verdicts and
// treat the playbook as inactive (degradation rule).
//
// A probe that would start after batteryTimeout has elapsed is left
// UNPROBED — no verdict is appended for it at all — rather than recorded as
// a gate-eligible FAILS: the battery ceiling reaching zero is absence of
// evidence, not a confirmed failure, and verdictFor(launcher) correctly
// returns ok=false for it (see rejectPlaybookFailedLaunch's contract). A
// per-probe timeout DOES still produce a verdict, but an Inconclusive one
// (see classifyPlaybookProbe) — also never gate-eligible. Either way the
// (possibly partial) Playbook is still returned active; only an infra error
// aborts the whole battery.
func runPlaybookBattery(ctx context.Context, sb sandbox.Sandbox, repoDir string, spec sandbox.Spec, res sandbox.Resolution, systems []ingest.BuildSystem, probeTimeout, batteryTimeout time.Duration) (Playbook, bool) {
	batteryCtx, cancel := context.WithTimeout(ctx, batteryTimeout)
	defer cancel()

	baseSpec := sandbox.Spec{
		RepoDir:     repoDir,
		Image:       spec.Image,
		Env:         append(append([]string(nil), spec.Env...), res.Env...),
		ROMounts:    append(append([]sandbox.ROMount(nil), spec.ROMounts...), res.ROMounts...),
		SetupCmds:   res.SetupCmds,
		Network:     "none",
		IdleTimeout: spec.IdleTimeout,
		CPUs:        spec.CPUs,
		MemoryMB:    spec.MemoryMB,
	}

	var verdicts []PlaybookVerdict
	for _, eco := range detectedPlaybookEcosystems(systems) {
		for _, probe := range playbookProbesByEcosystem[eco] {
			if batteryCtx.Err() != nil {
				// Ceiling exceeded: leave this (and every remaining) probe
				// unprobed rather than appending a verdict — see doc above.
				continue
			}
			runSpec := baseSpec
			runSpec.Cmd = probe.Cmd
			result, err := execProbe(batteryCtx, sb, runSpec, probeTimeout)
			if err != nil {
				// An infra-level Exec error (not a TimedOut Result, which
				// execProbe/classifyPlaybookProbe treat as a normal
				// per-probe Inconclusive) means the sandbox itself is
				// unusable — abort the whole battery per the degradation
				// rule rather than report a misleading partial playbook.
				return Playbook{}, false
			}
			verdicts = append(verdicts, classifyPlaybookProbe(probe, result, nil, probeTimeout))
		}
	}
	return Playbook{Verdicts: verdicts}, true
}

// playbookCache holds per-(repoDir, HEAD sha, dep-resolution fingerprint)
// cached Playbook results so the battery runs at most once per key per
// process lifetime, even when multiple findings' Attempt calls race —
// mirrors verify_sandbox.go's smokeCache/smokeEntry pattern exactly (see
// VerifySandboxOnce's doc for why keying is per-combination, not global).
var playbookCache = struct {
	mu sync.Mutex
	m  map[string]*playbookEntry
}{m: make(map[string]*playbookEntry)}

type playbookEntry struct {
	once sync.Once
	pb   Playbook
}

// repoHeadSHA resolves repoDir's current git HEAD commit, or "" when repoDir
// is not a git work tree (or git is unavailable). An empty value still
// yields a usable (if coarser) cache key — repoDir alone — rather than
// failing the lookup; this mirrors config.RepoToplevel's tolerant fallback.
func repoHeadSHA(repoDir string) string {
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolutionFingerprint hashes the parts of a sandbox.Resolution that change
// what a probe command can see inside the sandbox — mounts, env, and setup
// commands — into a short deterministic key. A distinct dependency
// resolution (e.g. dep_strategy: fetch re-warming with different pinned
// versions) gets its own playbook cache entry instead of silently reusing
// another resolution's verdicts.
func resolutionFingerprint(res sandbox.Resolution) string {
	h := sha256.New()
	// hash.Hash writes never fail; blank-assign to satisfy errcheck.
	_, _ = fmt.Fprintf(h, "strategy\x00%s\x00", res.Strategy)
	for _, m := range res.ROMounts {
		_, _ = fmt.Fprintf(h, "mount\x00%s\x00%s\x00%v\x00", m.HostPath, m.ContainerPath, m.Shared)
	}
	for _, e := range res.Env {
		_, _ = fmt.Fprintf(h, "env\x00%s\x00", e)
	}
	for _, c := range res.SetupCmds {
		_, _ = fmt.Fprintf(h, "setup\x00%s\x00", strings.Join(c, "\x1f"))
	}
	for _, tf := range res.Fingerprints {
		_, _ = fmt.Fprintf(h, "toolchain\x00%s\x00%s\x00", tf.Name, tf.Version)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PlaybookOnce runs the verified-command battery exactly once per (repoDir,
// git HEAD sha, dep-resolution fingerprint) tuple per process lifetime,
// against the real spec/res a repro run would use, and caches the result.
// Subsequent calls with the same key return the cached Playbook without
// re-running the battery. Safe for concurrent callers.
//
// A battery that could not run at all (sandbox exec infra failure) caches
// and returns an empty Playbook (nil Verdicts) — callers must treat that as
// "inactive": no prompt section, gate never fires. This is the degradation
// rule bugbot-u2v5 requires: a playbook failure NEVER blocks the reproduce
// stage the way it behaved before this feature existed.
func PlaybookOnce(ctx context.Context, sb sandbox.Sandbox, repoDir string, spec sandbox.Spec, res sandbox.Resolution, systems []ingest.BuildSystem) Playbook {
	key := repoDir + "\x00" + repoHeadSHA(repoDir) + "\x00" + resolutionFingerprint(res)
	playbookCache.mu.Lock()
	e, ok := playbookCache.m[key]
	if !ok {
		e = &playbookEntry{}
		playbookCache.m[key] = e
	}
	playbookCache.mu.Unlock()
	e.once.Do(func() {
		if pb, ok := runPlaybookBattery(ctx, sb, repoDir, spec, res, systems, playbookProbeTimeout, playbookBatteryTimeout); ok {
			e.pb = pb
		}
		// !ok (battery aborted) leaves e.pb at its zero value (empty
		// Playbook) — the degradation rule.
	})
	return e.pb
}

// playbookGuidance renders the "Verified commands for this repo" system-
// prompt section (agent.go's reproducer prompt-assembly hook): verified
// launchers first ("you MAY use this directly"), then CONFIRMED-failed ones
// with their one-line reason ("Do NOT propose this launcher"), then any
// Inconclusive ones as a neutral, non-directive note — an Inconclusive
// verdict (per-probe timeout) is not evidence the launcher is broken, so it
// must never read as a "Do NOT propose" instruction. Mirrors
// capabilityGuidance's structure. An empty Playbook (battery never ran or
// degraded) yields "" so the prompt is byte-identical to pre-bugbot-u2v5
// behavior. Every v.Reason embedded below is already util.FlattenField'd by
// classifyPlaybookProbe, so no raw sandbox output reaches the prompt here.
func playbookGuidance(pb Playbook) string {
	if len(pb.Verdicts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nVerified commands for this repo (probed once against this exact sandbox spec):\n")
	for _, v := range pb.Verdicts {
		if v.Verified {
			fmt.Fprintf(&b, "- %s: VERIFIED-WORKS. You MAY use this launcher directly.\n", v.Launcher)
		}
	}
	for _, v := range pb.Verdicts {
		if !v.Verified && !v.Inconclusive {
			fmt.Fprintf(&b, "- %s: FAILS (%s). Do NOT propose this launcher.\n", v.Launcher, v.Reason)
		}
	}
	for _, v := range pb.Verdicts {
		if v.Inconclusive {
			fmt.Fprintf(&b, "- %s: inconclusive (%s). No confirmed evidence either way.\n", v.Launcher, v.Reason)
		}
	}
	return b.String()
}

// rejectPlaybookFailedLaunch is the pre-launch plan-gate hook (called
// adjacent to rejectUnavailableEcosystemPlan in repro.go's Attempt loop,
// without restructuring that function): it rejects a plan whose cmd invokes
// a launcher the playbook battery confirmed FAILS, naming a verified
// alternative when one exists.
//
// Returns "" (proceed to sandbox launch) when: the playbook is inactive (no
// battery ran — degradation rule), the plan's cmd names no recognized
// launcher, the launcher was never probed (no evidence either way), the
// launcher verified, or the launcher's only verdict is Inconclusive (a
// per-probe timeout is not a confirmed failure — see
// PlaybookVerdict.Inconclusive). This is deliberately narrower than
// rejectUnavailableEcosystemPlan: it only fires on a CONFIRMED FAILS
// verdict, never on absence (or ambiguity) of evidence.
func rejectPlaybookFailedLaunch(p *Plan, pb Playbook) string {
	if len(pb.Verdicts) == 0 {
		return ""
	}
	_, tool := ecosystem.InferToolFromCmd(p.Cmd)
	if tool == "" {
		return ""
	}
	v, ok := pb.verdictFor(tool)
	if !ok || v.Verified || v.Inconclusive {
		return ""
	}
	if alt, hasAlt := pb.alternativeTo(tool); hasAlt {
		return fmt.Sprintf(
			"Your plan invokes %s, which the verified-command playbook confirmed FAILS in this exact sandbox (%s). "+
				"%s IS verified to work here — revise cmd to use %s instead.",
			tool, v.Reason, alt, alt,
		)
	}
	return fmt.Sprintf(
		"Your plan invokes %s, which the verified-command playbook confirmed FAILS in this exact sandbox (%s), "+
			"and no verified alternative launcher was found for this repo. Revise cmd to use a command this "+
			"sandbox can actually run, or report the environment gap in expect.",
		tool, v.Reason,
	)
}
