package repro

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ecosystem"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// resetPlaybookCache resets the package-level playbookCache for test
// isolation, mirroring resetSmokeCache in verify_sandbox_test.go.
func resetPlaybookCache() {
	playbookCache.mu.Lock()
	playbookCache.m = make(map[string]*playbookEntry)
	playbookCache.mu.Unlock()
}

// ---------------------------------------------------------------------------
// detectedPlaybookEcosystems / classifyPlaybookProbe: unit-level classifiers
// ---------------------------------------------------------------------------

func TestDetectedPlaybookEcosystems(t *testing.T) {
	tests := []struct {
		name    string
		systems []ingest.BuildSystem
		want    []ecosystem.Ecosystem
	}{
		{"empty", nil, nil},
		{"go module", []ingest.BuildSystem{ingest.BuildSystemGoModule}, []ecosystem.Ecosystem{ecosystem.EcosystemGo}},
		{"npm", []ingest.BuildSystem{ingest.BuildSystemNPM}, []ecosystem.Ecosystem{ecosystem.EcosystemJS}},
		{
			"multi, canonical order regardless of input order",
			[]ingest.BuildSystem{ingest.BuildSystemCargo, ingest.BuildSystemBazel, ingest.BuildSystemGoModule},
			[]ecosystem.Ecosystem{ecosystem.EcosystemGo, ecosystem.EcosystemRust, ecosystem.EcosystemBazel},
		},
		{
			"go workspace and go module dedupe to one go entry",
			[]ingest.BuildSystem{ingest.BuildSystemGoWorkspace, ingest.BuildSystemGoModule},
			[]ecosystem.Ecosystem{ecosystem.EcosystemGo},
		},
		{"unmapped build system (make) contributes nothing", []ingest.BuildSystem{ingest.BuildSystemMake}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectedPlaybookEcosystems(tt.systems)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestClassifyPlaybookProbe(t *testing.T) {
	probe := playbookProbe{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Cmd: []string{"npx", "--version"}}

	t.Run("exit 0 is verified", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{ExitCode: 0, Stdout: "10.2.0\n"}, nil, playbookProbeTimeout)
		if !v.Verified || v.Reason != "" {
			t.Errorf("got %+v, want Verified=true with no reason", v)
		}
	})

	t.Run("exit 127 is not found", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{ExitCode: 127, Stderr: "npx: not found"}, nil, playbookProbeTimeout)
		if v.Verified || v.Reason != "not found" {
			t.Errorf("got %+v, want FAILS with reason %q", v, "not found")
		}
	})

	t.Run("command-not-found marker without exit 127 is still not found", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{ExitCode: 1, Stderr: "sh: npx: command not found"}, nil, playbookProbeTimeout)
		if v.Verified || v.Reason != "not found" {
			t.Errorf("got %+v, want FAILS with reason %q", v, "not found")
		}
	})

	t.Run("timed out result is inconclusive, not a confirmed FAILS", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{TimedOut: true}, nil, 15*time.Second)
		if v.Verified || !v.Inconclusive || v.Reason != "timed out after 15s" {
			t.Errorf("got %+v, want Inconclusive=true with a timeout reason", v)
		}
	})

	t.Run("infra error", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{}, context.DeadlineExceeded, playbookProbeTimeout)
		if v.Verified || !strings.Contains(v.Reason, "sandbox exec failed") {
			t.Errorf("got %+v, want FAILS mentioning the exec error", v)
		}
	})

	t.Run("other non-zero exit carries a truncated evidence tail", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{ExitCode: 2, Stderr: "unexpected error: boom"}, nil, playbookProbeTimeout)
		if v.Verified || !strings.Contains(v.Reason, "boom") {
			t.Errorf("got %+v, want FAILS with the evidence tail", v)
		}
	})
}

// ---------------------------------------------------------------------------
// playbookGuidance: prompt-section rendering
// ---------------------------------------------------------------------------

func TestPlaybookGuidance_RendersVerifiedThenFailed(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "node", Verified: true},
		{Ecosystem: ecosystem.EcosystemGo, Launcher: "go", Verified: true},
	}}
	g := playbookGuidance(pb)

	if !strings.Contains(g, "Verified commands for this repo") {
		t.Fatalf("guidance missing header:\n%s", g)
	}
	if !strings.Contains(g, "node: VERIFIED-WORKS") || !strings.Contains(g, "go: VERIFIED-WORKS") {
		t.Errorf("guidance must list verified launchers:\n%s", g)
	}
	if !strings.Contains(g, "npx: FAILS (not found)") {
		t.Errorf("guidance must list the failed launcher with its reason:\n%s", g)
	}
	// Verified launchers are listed as a block before failed ones.
	if strings.Index(g, "VERIFIED-WORKS") > strings.Index(g, "FAILS") {
		t.Errorf("verified launchers must be listed before failed ones:\n%s", g)
	}
}

func TestPlaybookGuidance_InconclusiveIsNeutralNotDoNotPropose(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Inconclusive: true, Reason: "timed out after 15s"},
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "node", Verified: true},
	}}
	g := playbookGuidance(pb)
	if !strings.Contains(g, "npx: inconclusive (timed out after 15s)") {
		t.Errorf("guidance must render the inconclusive launcher with its reason:\n%s", g)
	}
	if strings.Contains(g, "npx: FAILS") || strings.Contains(g, "npx") && strings.Contains(g, "Do NOT propose") {
		t.Errorf("an Inconclusive verdict must never render as a confirmed FAILS / \"Do NOT propose\" line:\n%s", g)
	}
}

func TestPlaybookGuidance_EmptyPlaybookYieldsNoSection(t *testing.T) {
	if g := playbookGuidance(Playbook{}); g != "" {
		t.Errorf("empty playbook must yield no prompt section, got %q", g)
	}
}

// ---------------------------------------------------------------------------
// rejectPlaybookFailedLaunch: pre-launch plan gate
// ---------------------------------------------------------------------------

func TestRejectPlaybookFailedLaunch(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "node", Verified: true},
	}}

	t.Run("FAILS launcher rejected naming a verified alternative", func(t *testing.T) {
		msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"npx", "jest"}}, pb)
		if msg == "" {
			t.Fatal("want rejection feedback, got none")
		}
		if !strings.Contains(msg, "npx") || !strings.Contains(msg, "node") {
			t.Errorf("feedback must name both the failed launcher and the verified alternative: %q", msg)
		}
	})

	t.Run("verified launcher proceeds", func(t *testing.T) {
		if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"node", "--test"}}, pb); msg != "" {
			t.Errorf("verified launcher must not be rejected, got %q", msg)
		}
	})

	t.Run("unprobed launcher proceeds (absence of evidence is not FAILS)", func(t *testing.T) {
		if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"go", "test", "./..."}}, pb); msg != "" {
			t.Errorf("unprobed launcher must not be rejected, got %q", msg)
		}
	})

	t.Run("ungated command proceeds", func(t *testing.T) {
		if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"make", "test"}}, pb); msg != "" {
			t.Errorf("a command InferToolFromCmd does not recognize must never be rejected, got %q", msg)
		}
	})

	t.Run("empty playbook is inactive", func(t *testing.T) {
		if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"npx", "jest"}}, Playbook{}); msg != "" {
			t.Errorf("an inactive (empty) playbook must never gate, got %q", msg)
		}
	})

	t.Run("FAILS launcher with no verified alternative names the environment gap", func(t *testing.T) {
		onlyFailed := Playbook{Verdicts: []PlaybookVerdict{
			{Ecosystem: ecosystem.EcosystemRust, Launcher: "cargo", Reason: "not found"},
		}}
		msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"cargo", "test"}}, onlyFailed)
		if msg == "" || !strings.Contains(msg, "cargo") {
			t.Fatalf("want feedback naming cargo and the environment gap, got %q", msg)
		}
		if strings.Contains(msg, "IS verified to work here") {
			t.Errorf("must not fabricate a verified alternative when none exists: %q", msg)
		}
	})
}

func TestRejectPlaybookFailedLaunch_InconclusiveNeverGates(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Inconclusive: true, Reason: "timed out after 15s"},
	}}
	if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"npx", "jest"}}, pb); msg != "" {
		t.Errorf("an Inconclusive verdict (per-probe timeout) must never gate a plan, got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// execProbe: per-probe timeout enforcement against a deliberately slow fake
// ---------------------------------------------------------------------------

// slowFakeSandbox is a scripted Sandbox (distinct from sandbox.Mock) that
// respects ctx cancellation exactly like a real backend, but blocks for
// `delay` before responding — used to prove execProbe enforces its own
// timeout instead of relying on the backend to self-limit.
type slowFakeSandbox struct {
	delay time.Duration
}

func (f *slowFakeSandbox) Exec(ctx context.Context, _ sandbox.Spec) (sandbox.Result, error) {
	select {
	case <-time.After(f.delay):
		return sandbox.Result{ExitCode: 0, Stdout: "eventually ok"}, nil
	case <-ctx.Done():
		return sandbox.Result{}, ctx.Err()
	}
}

var _ sandbox.Sandbox = (*slowFakeSandbox)(nil)

func TestExecProbe_EnforcesPerProbeTimeout(t *testing.T) {
	fake := &slowFakeSandbox{delay: 300 * time.Millisecond}
	timeout := 20 * time.Millisecond

	start := time.Now()
	res, err := execProbe(context.Background(), fake, sandbox.Spec{Cmd: []string{"node", "--test", "--help"}}, timeout)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("execProbe returned error %v, want a TimedOut Result instead", err)
	}
	if !res.TimedOut {
		t.Errorf("want Result.TimedOut=true when the sandbox outlives the probe timeout, got %+v", res)
	}
	if elapsed >= fake.delay {
		t.Errorf("execProbe took %s — it must return at ~%s, not wait for the slow sandbox's %s", elapsed, timeout, fake.delay)
	}
}

func TestExecProbe_FastResponseWins(t *testing.T) {
	fake := &slowFakeSandbox{delay: 0}
	res, err := execProbe(context.Background(), fake, sandbox.Spec{Cmd: []string{"go", "version"}}, playbookProbeTimeout)
	if err != nil {
		t.Fatalf("execProbe: %v", err)
	}
	if res.TimedOut || res.ExitCode != 0 {
		t.Errorf("got %+v, want a clean, non-timed-out result", res)
	}
}

// ---------------------------------------------------------------------------
// runPlaybookBattery / PlaybookOnce: end-to-end battery behavior
// ---------------------------------------------------------------------------

func TestRunPlaybookBattery_VerifiedAndFailedLaunchers(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "go.mod", "module example.com/x\ngo 1.21\n")
	mustWriteFile(t, dir, "package.json", `{"name":"x"}`)

	m := sandbox.NewMock(sandbox.MockResponse{})
	m.ResponseFunc = func(_ int, spec sandbox.Spec) (sandbox.Result, error) {
		switch {
		case len(spec.Cmd) > 0 && spec.Cmd[0] == "go":
			return sandbox.Result{ExitCode: 0, Stdout: "go1.21"}, nil
		case len(spec.Cmd) > 0 && spec.Cmd[0] == "node":
			return sandbox.Result{ExitCode: 0}, nil
		case len(spec.Cmd) > 0 && spec.Cmd[0] == "npx":
			return sandbox.Result{ExitCode: 127, Stderr: "npx: not found"}, nil
		}
		return sandbox.Result{ExitCode: 1}, nil
	}

	systems := ingest.DetectBuildSystems(dir)
	pb, ok := runPlaybookBattery(context.Background(), m, dir, sandbox.Spec{}, sandbox.Resolution{}, systems, playbookProbeTimeout, playbookBatteryTimeout)
	if !ok {
		t.Fatal("battery must not abort on a clean Mock")
	}

	goV, ok := pb.verdictFor("go")
	if !ok || !goV.Verified {
		t.Errorf("go verdict = %+v, want Verified=true", goV)
	}
	nodeV, ok := pb.verdictFor("node")
	if !ok || !nodeV.Verified {
		t.Errorf("node verdict = %+v, want Verified=true", nodeV)
	}
	npxV, ok := pb.verdictFor("npx")
	if !ok || npxV.Verified || npxV.Inconclusive || npxV.Reason != "not found" {
		t.Errorf("npx verdict = %+v, want Verified=false reason=\"not found\"", npxV)
	}
}

// TestRunPlaybookBattery_PythonAndPytestAreSeparateLaunchers proves the
// python3-vs-pytest probe split (non-blocking review item (a)): a pytest-
// absent-but-python3-present sandbox must record "python3" as VERIFIED
// (bare interpreter liveness) even though the separate "pytest" launcher
// verdict is FAILS. Before the split, a single conflated "python3" verdict
// would have recorded FAILS here and (once the gate is live) wrongly
// rejected a valid `python3 -m unittest` plan.
func TestRunPlaybookBattery_PythonAndPytestAreSeparateLaunchers(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "pyproject.toml", "[project]\nname = \"x\"\n")

	m := sandbox.NewMock(sandbox.MockResponse{})
	m.ResponseFunc = func(_ int, spec sandbox.Spec) (sandbox.Result, error) {
		switch {
		case len(spec.Cmd) == 2 && spec.Cmd[0] == "python3" && spec.Cmd[1] == "--version":
			return sandbox.Result{ExitCode: 0, Stdout: "Python 3.11.4"}, nil
		case len(spec.Cmd) >= 3 && spec.Cmd[0] == "python3" && spec.Cmd[2] == "pytest":
			return sandbox.Result{ExitCode: 1, Stderr: "No module named pytest"}, nil
		}
		return sandbox.Result{ExitCode: 1}, nil
	}

	systems := ingest.DetectBuildSystems(dir)
	pb, ok := runPlaybookBattery(context.Background(), m, dir, sandbox.Spec{}, sandbox.Resolution{}, systems, playbookProbeTimeout, playbookBatteryTimeout)
	if !ok {
		t.Fatal("battery must not abort on a clean Mock")
	}

	pyV, ok := pb.verdictFor("python3")
	if !ok || !pyV.Verified {
		t.Errorf("python3 verdict = %+v, want Verified=true (bare interpreter liveness, independent of pytest)", pyV)
	}
	pytestV, ok := pb.verdictFor("pytest")
	if !ok || pytestV.Verified {
		t.Errorf("pytest verdict = %+v, want Verified=false (pytest module absent)", pytestV)
	}

	// Gate proof: a plan running `python3 -m unittest` infers tool="python3"
	// (the "-m unittest" suffix is never inspected by InferToolFromCmd), so
	// it must proceed even though pytest specifically FAILS.
	if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"python3", "-m", "unittest"}}, pb); msg != "" {
		t.Errorf("python3 -m unittest must not be rejected by a pytest-specific FAILS verdict, got %q", msg)
	}
	// A bare `pytest ...` plan DOES infer tool="pytest" and must be
	// rejected, naming python3 (same ecosystem) as the verified alternative.
	msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"pytest", "-q"}}, pb)
	if msg == "" || !strings.Contains(msg, "python3") {
		t.Errorf("bare pytest invocation must be rejected naming python3 as the alternative, got %q", msg)
	}
}

func TestRunPlaybookBattery_AbortsOnInfraError(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "go.mod", "module example.com/x\ngo 1.21\n")

	m := sandbox.NewMock(sandbox.MockResponse{Err: context.DeadlineExceeded})
	systems := ingest.DetectBuildSystems(dir)

	pb, ok := runPlaybookBattery(context.Background(), m, dir, sandbox.Spec{}, sandbox.Resolution{}, systems, playbookProbeTimeout, playbookBatteryTimeout)
	if ok {
		t.Fatal("battery must abort (ok=false) on an infra-level Exec error")
	}
	if len(pb.Verdicts) != 0 {
		t.Errorf("aborted battery must discard partial verdicts, got %+v", pb.Verdicts)
	}
	// The degradation rule end-to-end: an aborted battery must leave both
	// consumers inactive.
	if g := playbookGuidance(pb); g != "" {
		t.Errorf("aborted battery must yield no prompt section, got %q", g)
	}
	if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"go", "test"}}, pb); msg != "" {
		t.Errorf("aborted battery must leave the gate inactive, got %q", msg)
	}
}

// TestRunPlaybookBattery_CeilingExceededLeavesRemainingProbesUnprobed proves
// the ceiling-skip path leaves a probe unprobed (no verdict appended) rather
// than recording a gate-eligible FAILS — the defect the oracle review
// flagged as latent (6 probes x 15s happened to equal the 90s ceiling
// exactly, masking it). Uses a real, ctx-respecting slow fake (not
// sandbox.Mock, which answers instantly) so wall-clock time genuinely
// elapses between the JS ecosystem's two probes (node, then npx).
//
// Timing: the slow fake's delay (60ms) exceeds batteryTimeout (20ms), so
// execProbe's derived context — WithTimeout(batteryCtx, probeTimeout) —
// inherits the tighter battery deadline and node's OWN probe is cut off
// there too (Inconclusive, not a confirmed FAILS — see
// TestClassifyPlaybookProbe's timeout case). By the time that returns, the
// battery ceiling has fully elapsed, so the pre-probe check for npx (the
// SECOND JS probe) finds batteryCtx already expired and skips it
// entirely — no verdict appended at all. probeTimeout is set far above both
// so it never independently truncates anything; only the battery ceiling
// does here.
func TestRunPlaybookBattery_CeilingExceededLeavesRemainingProbesUnprobed(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "package.json", `{"name":"x"}`)
	systems := ingest.DetectBuildSystems(dir)

	fake := &slowFakeSandbox{delay: 60 * time.Millisecond}
	pb, ok := runPlaybookBattery(context.Background(), fake, dir, sandbox.Spec{}, sandbox.Resolution{}, systems, 500*time.Millisecond, 20*time.Millisecond)
	if !ok {
		t.Fatal("a ceiling timeout is not an infra error; battery must stay active (ok=true)")
	}
	if len(pb.Verdicts) != 1 {
		t.Fatalf("want exactly 1 verdict (node, cut off by the ceiling; npx never started), got %+v", pb.Verdicts)
	}
	nodeV, ok := pb.verdictFor("node")
	if !ok || nodeV.Verified || !nodeV.Inconclusive {
		t.Errorf("node verdict = %+v, want Inconclusive=true (cut off by the battery ceiling, not a confirmed FAILS)", nodeV)
	}
	if _, ok := pb.verdictFor("npx"); ok {
		t.Errorf("npx must be left UNPROBED (no verdict at all) once the ceiling is exceeded, got %+v", pb.Verdicts)
	}
	// End-to-end: neither consumer treats this as a confirmed failure.
	if strings.Contains(playbookGuidance(pb), "node: FAILS") {
		t.Errorf("a ceiling-truncated probe must never render as a confirmed FAILS:\n%s", playbookGuidance(pb))
	}
	if msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"node", "--test"}}, pb); msg != "" {
		t.Errorf("a ceiling-truncated (Inconclusive) verdict must never gate a plan, got %q", msg)
	}
}

func TestPlaybookOnce_CachesResult(t *testing.T) {
	resetPlaybookCache()
	t.Cleanup(resetPlaybookCache)

	dir := t.TempDir()
	mustWriteFile(t, dir, "go.mod", "module example.com/x\ngo 1.21\n")

	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0, Stdout: "go1.21"}})
	systems := ingest.DetectBuildSystems(dir)
	spec := sandbox.Spec{Image: "debian:slim"}
	res := sandbox.Resolution{}
	ctx := context.Background()

	pb1 := PlaybookOnce(ctx, m, dir, spec, res, systems)
	pb2 := PlaybookOnce(ctx, m, dir, spec, res, systems)

	if m.CallCount() != 1 {
		t.Errorf("PlaybookOnce: want exactly 1 sandbox call across two calls with the same key, got %d", m.CallCount())
	}
	if len(pb1.Verdicts) == 0 || len(pb2.Verdicts) == 0 {
		t.Fatalf("both calls must return a populated playbook: pb1=%+v pb2=%+v", pb1, pb2)
	}
	goV1, _ := pb1.verdictFor("go")
	goV2, _ := pb2.verdictFor("go")
	if goV1 != goV2 {
		t.Errorf("cached calls must return identical verdicts: %+v vs %+v", goV1, goV2)
	}
}

func TestPlaybookOnce_DistinctResolutionGetsDistinctCacheEntry(t *testing.T) {
	resetPlaybookCache()
	t.Cleanup(resetPlaybookCache)

	dir := t.TempDir()
	mustWriteFile(t, dir, "go.mod", "module example.com/x\ngo 1.21\n")

	m := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 0}})
	systems := ingest.DetectBuildSystems(dir)
	ctx := context.Background()

	PlaybookOnce(ctx, m, dir, sandbox.Spec{}, sandbox.Resolution{}, systems)
	PlaybookOnce(ctx, m, dir, sandbox.Spec{}, sandbox.Resolution{Env: []string{"GOFLAGS=-mod=mod"}}, systems)

	if m.CallCount() != 2 {
		t.Errorf("a distinct dependency resolution must run its own battery, want 2 sandbox calls, got %d", m.CallCount())
	}
}

// ---------------------------------------------------------------------------
// Playbook.alternativeTo / String
// ---------------------------------------------------------------------------

func TestPlaybookAlternativeTo_PrefersSameEcosystem(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "node", Verified: true},
		{Ecosystem: ecosystem.EcosystemGo, Launcher: "go", Verified: true},
	}}
	alt, ok := pb.alternativeTo("npx")
	if !ok || alt != "node" {
		t.Errorf("alternativeTo(npx) = (%q, %v), want (\"node\", true) — same-ecosystem preference", alt, ok)
	}
}

// TestPlaybookAlternativeTo_BazeliskForFailedBazel pins bugbot-wjc2: a
// bazelisk-only sandbox (bazel FAILS, bazelisk verified) must offer
// bazelisk as the same-ecosystem alternative for a failed "bazel" launcher,
// and rejectPlaybookFailedLaunch must name it in its rejection feedback.
func TestPlaybookAlternativeTo_BazeliskForFailedBazel(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemBazel, Launcher: "bazel", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemBazel, Launcher: "bazelisk", Verified: true},
	}}
	alt, ok := pb.alternativeTo("bazel")
	if !ok || alt != "bazelisk" {
		t.Errorf("alternativeTo(bazel) = (%q, %v), want (\"bazelisk\", true) — same-ecosystem preference", alt, ok)
	}

	msg := rejectPlaybookFailedLaunch(&Plan{Cmd: []string{"bazel", "test", "//..."}}, pb)
	if msg == "" {
		t.Fatal("want rejection feedback, got none")
	}
	if !strings.Contains(msg, "bazel") || !strings.Contains(msg, "bazelisk") {
		t.Errorf("feedback must name both the failed launcher and the verified bazelisk alternative: %q", msg)
	}
}

func TestPlaybookAlternativeTo_NoCrossEcosystemFallback(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemRust, Launcher: "cargo", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemGo, Launcher: "go", Verified: true},
	}}
	// "go" is a DIFFERENT ecosystem from the failed "cargo" — must never be
	// offered as a substitute (cross-ecosystem guidance is misleading).
	if alt, ok := pb.alternativeTo("cargo"); ok {
		t.Errorf("alternativeTo(cargo) = (%q, true), want ok=false — no same-ecosystem verified launcher exists", alt)
	}
}

func TestPlaybookAlternativeTo_NoneWhenNothingVerified(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemRust, Launcher: "cargo", Reason: "not found"},
	}}
	if _, ok := pb.alternativeTo("cargo"); ok {
		t.Error("alternativeTo must report ok=false when nothing verified")
	}
}

func TestPlaybookString(t *testing.T) {
	if s := (Playbook{}).String(); !strings.Contains(s, "no data") {
		t.Errorf("empty Playbook.String() = %q, want a no-data message", s)
	}
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Launcher: "go", Verified: true},
		{Launcher: "npx", Reason: "not found"},
	}}
	s := pb.String()
	if !strings.Contains(s, "go") || !strings.Contains(s, "VERIFIED-WORKS") {
		t.Errorf("String() missing verified launcher: %q", s)
	}
	if !strings.Contains(s, "npx") || !strings.Contains(s, "FAILS (not found)") {
		t.Errorf("String() missing failed launcher: %q", s)
	}
}

// mustWriteFile writes content to name under dir, failing the test on error.
func mustWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := writeFileBytes(filepath.Join(dir, name), []byte(content)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// Attempt end-to-end: a Reproducer constructed with Options.Playbook wired
// (as a production caller like buildReproducerWithSandbox now does — see
// engine/repro.go) proves both consumers are live: the reproducer's system
// prompt carries the "Verified commands for this repo" section, and a plan
// invoking a launcher the playbook confirmed FAILS is rejected pre-launch
// without ever reaching the sandbox.
// ---------------------------------------------------------------------------

func TestAttempt_PlaybookWired_PromptAndGateLive(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "package.json", `{"name":"x"}`)

	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "npx", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemJS, Launcher: "node", Verified: true},
	}}

	// Round 1: the agent proposes npx (playbook-FAILS); the gate must reject
	// it before any sandbox launch. Round 2: it revises to node (verified).
	failedPlan := Plan{
		Files:  map[string]string{"repro_test.js": "test('x', () => {})"},
		Cmd:    []string{"npx", "jest"},
		Expect: "npx jest demonstrates the bug",
	}
	goodJSPlan := Plan{
		Files:  map[string]string{"repro_test.js": "test('x', () => {})"},
		Cmd:    []string{"node", "--test"},
		Expect: "node --test demonstrates the bug",
	}
	client := newScriptedClient(planBody(t, failedPlan), planBody(t, goodJSPlan))
	sb := sandbox.NewMock(sandbox.MockResponse{Result: sandbox.Result{ExitCode: 1, Stdout: "not ok 1 - x"}})

	r, err := New(client, sb, dir, Options{ArtifactDir: t.TempDir(), Playbook: pb, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// File left empty: ClassifyTargetExecution (bugbot-qb4r layer a) is a
	// no-op with no target path, so this test isolates the playbook gate
	// (rejectPlaybookFailedLaunch) from the unrelated static-reachability
	// gate.
	finding := domain.Finding{
		ID:        "f1",
		Title:     "x breaks",
		Severity:  "high",
		Tier:      2,
		Lens:      "logic",
		CommitSHA: "abc123",
	}

	att, err := r.Attempt(context.Background(), finding)
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}

	// Gate proof: round 1's npx plan never reached the sandbox — only round
	// 2's node plan did (twice: official run + bugbot-c49s determinism
	// confirmation, both served the same demonstrating mock response).
	if sb.CallCount() != 2 {
		t.Fatalf("sandbox CallCount = %d, want 2 (round 1's playbook-FAILS plan must never launch; round 2 = official + confirmation)", sb.CallCount())
	}
	for i, call := range sb.Calls() {
		if len(call.Spec.Cmd) == 0 || call.Spec.Cmd[0] != "node" {
			t.Errorf("sandbox call %d must be round 2's node plan, got cmd=%v", i, call.Spec.Cmd)
		}
	}
	if att.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (round 1 rejected pre-launch, round 2 executed)", att.Attempts)
	}

	// Prompt proof: the system prompt (fixed for the whole Attempt, built
	// once in newRunner) carries the playbook section for every round.
	reqs := client.allRequests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	if !strings.Contains(reqs[0].System, "Verified commands for this repo") {
		t.Errorf("system prompt missing the playbook section:\n%s", reqs[0].System)
	}
	if !strings.Contains(reqs[0].System, "node: VERIFIED-WORKS") {
		t.Errorf("system prompt must list node as verified:\n%s", reqs[0].System)
	}
	if !strings.Contains(reqs[0].System, "npx: FAILS (not found)") {
		t.Errorf("system prompt must list npx as failed with its reason:\n%s", reqs[0].System)
	}

	// Round 1's revision feedback (fed as the round-2 user task) must name
	// the verified alternative.
	if !strings.Contains(client.taskText(1), "node") {
		t.Errorf("round 2's task must carry gate feedback naming node as the alternative:\n%s", client.taskText(1))
	}
}
