package ecosystem_test

import (
	"testing"

	"github.com/dpoage/bugbot/internal/ecosystem"
)

func TestInferFromExtension(t *testing.T) {
	cases := []struct {
		file string
		want ecosystem.Ecosystem
	}{
		{"src/app.ts", ecosystem.EcosystemJS},
		{"src/app.tsx", ecosystem.EcosystemJS},
		{"src/app.js", ecosystem.EcosystemJS},
		{"lib/mod.mjs", ecosystem.EcosystemJS},
		{"pkg/main.py", ecosystem.EcosystemPython},
		{"pkg/main.pyi", ecosystem.EcosystemPython},
		{"src/lib.rs", ecosystem.EcosystemRust},
		{"main.go", ""},   // Go: ungated (base toolchain assumed present)
		{"main.cpp", ""},  // C++: ungated (only sanitizer modes are probed)
		{"README.md", ""}, // no probe at all
		{"", ""},
	}
	for _, c := range cases {
		if got := ecosystem.InferFromExtension(c.file); got != c.want {
			t.Errorf("InferFromExtension(%q) = %q, want %q", c.file, got, c.want)
		}
	}
}

func TestInferFromCmd(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want ecosystem.Ecosystem
	}{
		{"npx jest", []string{"npx", "jest", "--runInBand"}, ecosystem.EcosystemJS},
		{"bare node", []string{"node", "--test", "foo.test.js"}, ecosystem.EcosystemJS},
		{"pytest", []string{"python3", "-m", "pytest", "-x"}, ecosystem.EcosystemPython},
		{"cargo test", []string{"cargo", "test", "--", "--nocapture"}, ecosystem.EcosystemRust},
		{"bazel test", []string{"bazel", "test", "//molecules/..."}, ecosystem.EcosystemBazel},
		{"bazelisk", []string{"bazelisk", "test", "//..."}, ecosystem.EcosystemBazel},
		{"sh -c wrapped bazel", []string{"sh", "-c", "bazel test //robot-control:all"}, ecosystem.EcosystemBazel},
		{"go test ungated", []string{"go", "test", "./..."}, ""},
		{"bash -c wrapper unwraps", []string{"bash", "-c", "cd sub && npx vitest run"}, ecosystem.EcosystemJS},
		{"sh -c wrapper unwraps", []string{"sh", "-c", "pytest -x tests/"}, ecosystem.EcosystemPython},
		{"unrecognized", []string{"make", "test"}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ecosystem.InferFromCmd(c.cmd); got != c.want {
				t.Errorf("InferFromCmd(%v) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}

// TestInferToolFromCmd pins the matched-binary-name half of the inference
// (bugbot-4z7m): launcher families (bazel/bazelisk) gate per binary NAME, so
// the gate needs to know which one the plan actually invokes.
func TestInferToolFromCmd(t *testing.T) {
	cases := []struct {
		cmd      []string
		wantEco  ecosystem.Ecosystem
		wantTool string
	}{
		{[]string{"bazel", "test", "//..."}, ecosystem.EcosystemBazel, "bazel"},
		{[]string{"bazelisk", "test", "//..."}, ecosystem.EcosystemBazel, "bazelisk"},
		{[]string{"sh", "-c", "bazelisk build //x"}, ecosystem.EcosystemBazel, "bazelisk"},
		{[]string{"npx", "vitest"}, ecosystem.EcosystemJS, "npx"},
		{[]string{"make", "test"}, "", ""},
	}
	for _, c := range cases {
		eco, tool := ecosystem.InferToolFromCmd(c.cmd)
		if eco != c.wantEco || tool != c.wantTool {
			t.Errorf("InferToolFromCmd(%v) = (%q, %q), want (%q, %q)", c.cmd, eco, tool, c.wantEco, c.wantTool)
		}
	}
}

func TestBaseMode(t *testing.T) {
	cases := []struct {
		eco  ecosystem.Ecosystem
		want string
	}{
		{ecosystem.EcosystemJS, "node"},
		{ecosystem.EcosystemPython, "python"},
		{ecosystem.EcosystemRust, "cargo"},
		{ecosystem.EcosystemBazel, "bazel"},
		{ecosystem.EcosystemGo, ""},
		{ecosystem.EcosystemCpp, ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ecosystem.BaseMode(c.eco); got != c.want {
			t.Errorf("BaseMode(%q) = %q, want %q", c.eco, got, c.want)
		}
	}
}

// TestBaseMode_MatchesRealProbeEntries guards against BaseMode naming a mode
// that the actual probe table never produces — a silent typo here would make
// the gate permanently report every js/python/rust finding as blocked.
func TestBaseMode_MatchesRealProbeEntries(t *testing.T) {
	for _, eco := range []ecosystem.Ecosystem{ecosystem.EcosystemJS, ecosystem.EcosystemPython, ecosystem.EcosystemRust, ecosystem.EcosystemBazel} {
		mode := ecosystem.BaseMode(eco)
		if mode == "" {
			t.Fatalf("BaseMode(%q) = \"\", want a real mode name", eco)
		}
		var entry *ecosystem.ProbeEntry
		for i := range ecosystem.ProbeEntries {
			if ecosystem.ProbeEntries[i].Name == eco {
				entry = &ecosystem.ProbeEntries[i]
				break
			}
		}
		if entry == nil {
			t.Fatalf("no ProbeEntry named %q", eco)
		}
		modes := entry.Interpret(ecosystem.ProbeResult{ExitCode: 1})
		if _, ok := modes[mode]; !ok {
			t.Errorf("ecosystem %q: BaseMode %q is not a key produced by its ProbeEntry.Interpret (keys: %v)", eco, mode, modes)
		}
	}
}
