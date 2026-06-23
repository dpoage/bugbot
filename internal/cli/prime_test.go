package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

func TestRecommendImage(t *testing.T) {
	cases := []struct {
		name      string
		bs        []ingest.BuildSystem
		goVersion string
		want      string
		noteHas   string
	}{
		{"go versioned", []ingest.BuildSystem{ingest.BuildSystemGoModule}, "1.26", "docker.io/library/golang:1.26-alpine", "cgo"},
		{"go unknown version", []ingest.BuildSystem{ingest.BuildSystemGoModule}, "", "docker.io/library/golang:latest-alpine", "cgo"},
		{"python", []ingest.BuildSystem{ingest.BuildSystemPython}, "", "docker.io/library/python:3-slim", "pip"},
		{"rust", []ingest.BuildSystem{ingest.BuildSystemCargo}, "", "docker.io/library/rust:1-slim", "cargo"},
		{"node", []ingest.BuildSystem{ingest.BuildSystemNPM}, "", "docker.io/library/node:22-slim", "npm"},
		{"cmake", []ingest.BuildSystem{ingest.BuildSystemCMake}, "", "docker.io/library/gcc:14", "cmake"},
		// Language-specific system is preferred over the bazel/make meta entries
		// even though DetectBuildSystems lists those first.
		{"bazel wrapping go", []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule}, "1.26", "gcr.io/bazel-public/bazel:latest", "bugbot sandbox build"},
		{"go with convenience make", []ingest.BuildSystem{ingest.BuildSystemGoModule, ingest.BuildSystemMake}, "1.26", "docker.io/library/golang:1.26-alpine", "cgo"},
		// Bazel with no other language toolchain: still pick the bazel base
		// image (we run `bazel test --build_tests_only //...` for Bazel repos);
		// the note must point at a purpose-built offline image.
		{"bazel only", []ingest.BuildSystem{ingest.BuildSystemBazel}, "", "gcr.io/bazel-public/bazel:latest", "purpose-built offline image"},
		{"make only", []ingest.BuildSystem{ingest.BuildSystemMake}, "", "docker.io/library/gcc:14", "make"},
		{"nothing", nil, "", "docker.io/library/debian:stable-slim", "NO compiler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, note := recommendImage(tc.bs, tc.goVersion)
			if img != tc.want {
				t.Errorf("image = %q, want %q", img, tc.want)
			}
			if !strings.Contains(note, tc.noteHas) {
				t.Errorf("note %q should contain %q", note, tc.noteHas)
			}
		})
	}
}

func TestRecommendDepStrategy(t *testing.T) {
	cases := []struct {
		name     string
		bs       []ingest.BuildSystem
		vendored bool
		hasReqs  bool
		want     string
		noteHas  string // optional substring the note must contain
	}{
		{"go vendored", []ingest.BuildSystem{ingest.BuildSystemGoModule}, true, false, "off", ""},
		{"go not vendored", []ingest.BuildSystem{ingest.BuildSystemGoModule}, false, false, "host", ""},
		{"go workspace not vendored", []ingest.BuildSystem{ingest.BuildSystemGoWorkspace}, false, false, "host", ""},
		{"python with reqs", []ingest.BuildSystem{ingest.BuildSystemPython}, false, true, "fetch", ""},
		{"python no reqs", []ingest.BuildSystem{ingest.BuildSystemPython}, false, false, "off", ""},
		{"reqs only no pyproject", nil, false, true, "fetch", ""},
		{"rust", []ingest.BuildSystem{ingest.BuildSystemCargo}, false, false, "off", ""},
		{"none", nil, false, false, "off", ""},
		// Bazel: dep_strategy stays "off" because deps are baked into the
		// offline image; the note must point at `bugbot sandbox build`.
		{"bazel only", []ingest.BuildSystem{ingest.BuildSystemBazel}, false, false, "off", "bugbot sandbox build"},
		{"bazel with go", []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule}, false, false, "off", "baked into the offline sandbox image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, note := recommendDepStrategy(tc.bs, tc.vendored, tc.hasReqs)
			if got != tc.want {
				t.Errorf("strategy = %q, want %q", got, tc.want)
			}
			if note == "" {
				t.Error("strategy note must not be empty")
			}
			if tc.noteHas != "" && !strings.Contains(note, tc.noteHas) {
				t.Errorf("note %q should contain %q", note, tc.noteHas)
			}
		})
	}
}

func TestGoModVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" means do not write go.mod
		want    string
	}{
		{"patch version reduced", "module x\n\ngo 1.26.4\n", "1.26"},
		{"minor only", "module x\ngo 1.21\n", "1.21"},
		{"toolchain line ignored", "module x\ngo 1.22.0\ntoolchain go1.23.1\n", "1.22"},
		{"no directive", "module x\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.content != "" {
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := goModVersion(dir); got != tc.want {
				t.Errorf("goModVersion = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("absent go.mod", func(t *testing.T) {
		if got := goModVersion(t.TempDir()); got != "" {
			t.Errorf("goModVersion on dir without go.mod = %q, want empty", got)
		}
	})
}

func TestGatherPrimeFactsGoVendored(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module x\ngo 1.26.4\n")
	mustWrite(t, filepath.Join(dir, "vendor", "modules.txt"), "# x\n")

	f := gatherPrimeFacts(dir, "podman", true)
	if f.Image != "docker.io/library/golang:1.26-alpine" {
		t.Errorf("image = %q", f.Image)
	}
	if f.DepStrategy != "off" {
		t.Errorf("dep_strategy = %q, want off (vendored)", f.DepStrategy)
	}
	if !f.Vendored {
		t.Error("Vendored should be true")
	}
	if f.GoVersion != "1.26" {
		t.Errorf("GoVersion = %q, want 1.26", f.GoVersion)
	}
}

func TestRenderPrimeContent(t *testing.T) {
	f := primeFacts{
		Target:       "/repo",
		Runtime:      "podman",
		RuntimeFound: true,
		BuildSystems: []ingest.BuildSystem{ingest.BuildSystemGoModule},
		GoVersion:    "1.26",
		Vendored:     true,
		Image:        "docker.io/library/golang:1.26-alpine",
		ImageNote:    "go note",
		DepStrategy:  "off",
		StrategyNote: "strategy note",
	}
	out := renderPrime(f)
	for _, want := range []string{
		"docker.io/library/golang:1.26-alpine", // recommendation echoed
		"dep_strategy: off",
		"environment_error", // the failure mode is explained
		"NEVER put API keys",
		"bugbot doctor",
		"bugbot init",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prime missing %q", want)
		}
	}
}

func TestRenderPrimeNoRuntimeWarns(t *testing.T) {
	out := renderPrime(primeFacts{Target: "/r", RuntimeFound: false, Image: "x", DepStrategy: "off"})
	if !strings.Contains(out, "NONE FOUND") {
		t.Error("missing-runtime warning should appear when RuntimeFound is false")
	}
}

// TestRenderPrimeBazelCaveat confirms the bazel guidance (image AND
// dep_strategy) renders the SUPPORTED offline framing for a Bazel repo:
// the recommended base image surfaces and the output points at
// `bugbot sandbox build` for a purpose-built offline image. The old FALSE
// "unsupported / disable repro" wording must be gone.
func TestRenderPrimeBazelCaveat(t *testing.T) {
	bs := []ingest.BuildSystem{ingest.BuildSystemBazel, ingest.BuildSystemGoModule}
	img, imgNote := recommendImage(bs, "")
	dep, depNote := recommendDepStrategy(bs, false, false)
	f := primeFacts{
		Target:       "/repo",
		Runtime:      "podman",
		RuntimeFound: true,
		BuildSystems: bs,
		Image:        img,
		ImageNote:    imgNote,
		DepStrategy:  dep,
		StrategyNote: depNote,
	}
	out := renderPrime(f)
	for _, want := range []string{
		"gcr.io/bazel-public/bazel:latest", // recommended base image surfaces
		"Bazel",                            // image note surfaces
		"bugbot sandbox build",             // supported offline framing surfaces
		"purpose-built offline image",      // offline image caveat surfaces
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prime missing %q", want)
		}
	}
	// The FALSE "unsupported / disable repro" framing must be gone.
	for _, bad := range []string{"disable repro", "NO bazel dep", "unsupported"} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered prime still contains stale wording %q", bad)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
