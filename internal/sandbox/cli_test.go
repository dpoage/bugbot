package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewCLIRequiresImage(t *testing.T) {
	if _, err := NewCLI("podman", ""); err == nil {
		t.Fatal("expected error when image is empty")
	}
}

func TestNewCLIUnknownRuntime(t *testing.T) {
	if _, err := NewCLI("definitely-not-a-real-runtime-xyz", "img"); err == nil {
		t.Fatal("expected error for runtime not on PATH")
	}
}

func TestResolveParamsAppliesDefaultsAndOverrides(t *testing.T) {
	s := &CLI{
		runtime:        "podman",
		defaultImage:   "default-img",
		defaultCPUs:    2,
		defaultMemory:  2048,
		defaultNetwork: "none",
		pidsLimit:      256,
		maxOutputBytes: DefaultMaxOutputBytes,
	}

	// Empty spec -> backend defaults.
	p := s.resolveParams(Spec{Cmd: []string{"true"}})
	if p.image != "default-img" || p.cpus != 2 || p.memoryMB != 2048 || p.network != "none" || p.pidsLimit != 256 {
		t.Fatalf("defaults not applied: %+v", p)
	}

	// Spec overrides.
	p = s.resolveParams(Spec{
		Cmd:      []string{"true"},
		Image:    "custom",
		CPUs:     0.5,
		MemoryMB: 256,
		Network:  "host",
		Env:      []string{"A=b"},
	})
	if p.image != "custom" || p.cpus != 0.5 || p.memoryMB != 256 || p.network != "host" {
		t.Fatalf("overrides not applied: %+v", p)
	}
	if len(p.env) != 1 || p.env[0] != "A=b" {
		t.Fatalf("env not propagated: %+v", p.env)
	}
}

func TestExecRejectsEmptyCmd(t *testing.T) {
	s := &CLI{runtime: "podman", defaultImage: "img", maxOutputBytes: DefaultMaxOutputBytes}
	if _, err := s.Exec(context.Background(), Spec{RepoDir: t.TempDir()}); err == nil {
		t.Fatal("expected error for empty Cmd")
	}
}

func TestRandTokenUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		tok := randToken()
		if len(tok) != 32 {
			t.Fatalf("token length = %d, want 32", len(tok))
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestOptionsConfigureCLI(t *testing.T) {
	s := &CLI{}
	for _, o := range []Option{
		WithCPUs(4), WithMemoryMB(1024), WithTimeout(5 * time.Second),
		WithNetwork("bridge"), WithPidsLimit(64), WithMaxOutputBytes(2048),
		WithScratchSizeMB(256), WithWorkspaceGrowthCeilingMB(1024),
		WithWorkspaceFileCountCeiling(50_000),
	} {
		o(s)
	}
	if s.defaultCPUs != 4 || s.defaultMemory != 1024 || s.defaultTimeout != 5*time.Second ||
		s.defaultNetwork != "bridge" || s.pidsLimit != 64 || s.maxOutputBytes != 2048 {
		t.Fatalf("options not applied: %+v", s)
	}
	if s.defaultScratchSizeMB != 256 {
		t.Errorf("defaultScratchSizeMB = %d, want 256", s.defaultScratchSizeMB)
	}
	if want := int64(1024) * 1024 * 1024; s.defaultGrowthCeilingBytes != want {
		t.Errorf("defaultGrowthCeilingBytes = %d, want %d (1024 MB in bytes)", s.defaultGrowthCeilingBytes, want)
	}
	if s.defaultFileCountCeiling != 50_000 {
		t.Errorf("defaultFileCountCeiling = %d, want 50000 (a plain file count, no unit conversion)", s.defaultFileCountCeiling)
	}
}

// TestFileCountCeilingAccessor pins FileCountCeiling()'s explicit-zero-
// disables contract (bugbot-gb3o), mirroring ScratchAndGrowthCeiling's own
// coverage in internal/engine/scan_helpers_test.go: an operator-configured
// 0 must read back as a truly disabled 0, not silently fall back to
// NewCLI's non-zero built-in default.
func TestFileCountCeilingAccessor(t *testing.T) {
	s, err := NewCLI("podman", "img", WithWorkspaceFileCountCeiling(0))
	if err != nil {
		t.Fatalf("NewCLI: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.FileCountCeiling(); got != 0 {
		t.Errorf("FileCountCeiling() = %d, want 0 (explicit disable did not override the pre-existing non-zero default)", got)
	}
}

// TestResultInfraKilledAndKillReason_FileCount pins the file-count kill
// reason's DISTINCTNESS from the byte-size quota reason (bugbot-gb3o) — a
// mutation collapsing WorkspaceFileCountExceeded's branch into
// WorkspaceQuotaExceeded's (or vice versa) in KillReason's switch must fail
// this test.
func TestResultInfraKilledAndKillReason_FileCount(t *testing.T) {
	res := Result{ExitCode: -1, WorkspaceFileCountExceeded: true}
	if !res.InfraKilled() {
		t.Error("InfraKilled() = false, want true for a WorkspaceFileCountExceeded result")
	}
	reason := res.KillReason()
	if reason == "" {
		t.Fatal("KillReason() = \"\", want a non-empty reason naming file count")
	}
	if !strings.Contains(reason, "file count") {
		t.Errorf("KillReason() = %q, want it to name file count", reason)
	}
	sizeReason := Result{ExitCode: -1, WorkspaceQuotaExceeded: true}.KillReason()
	if reason == sizeReason {
		t.Errorf("file-count KillReason() must be distinct from the size-quota KillReason(); both were %q", reason)
	}
	if strings.Contains(reason, "workspace_growth_ceiling_mb") {
		t.Errorf("KillReason() = %q, must not reference the size knob's config key", reason)
	}
}

// TestResultKillReason_EmptyWhenNotInfraKilled: a plain success/failure
// Result must never surface a stale kill reason.
func TestResultKillReason_EmptyWhenNotInfraKilled(t *testing.T) {
	if got := (Result{ExitCode: 1}).KillReason(); got != "" {
		t.Errorf("KillReason() = %q, want \"\" for a non-infra-killed result", got)
	}
}
