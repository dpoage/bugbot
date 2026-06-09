package sandbox

import (
	"context"
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
	} {
		o(s)
	}
	if s.defaultCPUs != 4 || s.defaultMemory != 1024 || s.defaultTimeout != 5*time.Second ||
		s.defaultNetwork != "bridge" || s.pidsLimit != 64 || s.maxOutputBytes != 2048 {
		t.Fatalf("options not applied: %+v", s)
	}
}
