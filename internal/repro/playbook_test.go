package repro

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	t.Run("timed out result", func(t *testing.T) {
		v := classifyPlaybookProbe(probe, sandbox.Result{TimedOut: true}, nil, 15*time.Second)
		if v.Verified || v.Reason != "timed out after 15s" {
			t.Errorf("got %+v, want FAILS with a timeout reason", v)
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
	pb, ok := runPlaybookBattery(context.Background(), m, dir, sandbox.Spec{}, sandbox.Resolution{}, systems)
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
	if !ok || npxV.Verified || npxV.Reason != "not found" {
		t.Errorf("npx verdict = %+v, want Verified=false reason=\"not found\"", npxV)
	}
}

func TestRunPlaybookBattery_AbortsOnInfraError(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, dir, "go.mod", "module example.com/x\ngo 1.21\n")

	m := sandbox.NewMock(sandbox.MockResponse{Err: context.DeadlineExceeded})
	systems := ingest.DetectBuildSystems(dir)

	pb, ok := runPlaybookBattery(context.Background(), m, dir, sandbox.Spec{}, sandbox.Resolution{}, systems)
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

func TestPlaybookAlternativeTo_FallsBackAcrossEcosystems(t *testing.T) {
	pb := Playbook{Verdicts: []PlaybookVerdict{
		{Ecosystem: ecosystem.EcosystemRust, Launcher: "cargo", Reason: "not found"},
		{Ecosystem: ecosystem.EcosystemGo, Launcher: "go", Verified: true},
	}}
	alt, ok := pb.alternativeTo("cargo")
	if !ok || alt != "go" {
		t.Errorf("alternativeTo(cargo) = (%q, %v), want (\"go\", true) — cross-ecosystem fallback", alt, ok)
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
