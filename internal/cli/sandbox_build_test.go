package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderFacts returns a deterministic fact set so the render assertions do not
// depend on the host (recommendImage / runtime detection / $HOME).
func renderFacts() sandboxBuildFacts {
	return sandboxBuildFacts{
		RepoDir:        "/home/u/proj",
		RepoName:       "proj",
		OutDir:         "/home/u/proj/bugbot-sandbox",
		BaseImage:      "gcr.io/bazel-public/bazel:latest",
		BazelVersion:   "8.4.2",
		ImageTag:       "localhost/proj-bugbot-sandbox:latest",
		VendorDir:      "/home/u/.cache/proj-bugbot/vendor",
		RepoCachePath:  "",
		Runtime:        "podman",
		VendorMount:    sandboxVendorMount,
		RepoCacheMount: sandboxRepoCacheMount,
		DiskCacheMount: sandboxDiskCacheMount,
		WarmTest:       strings.Join(sandboxWarmTestArgs, " "),
	}
}

func TestSandboxBuild_DockerfileRendersFacts(t *testing.T) {
	f := renderFacts()
	out, err := renderSandboxTemplate("templates/sandbox_build.Dockerfile.tmpl", f)
	if err != nil {
		t.Fatalf("render Dockerfile: %v", err)
	}

	// Substituted facts.
	mustContain(t, "Dockerfile", out,
		"FROM "+f.BaseImage,
		"ARG BAZEL_VERSION="+f.BazelVersion,
		"--vendor_dir="+f.VendorMount,
		"--repository_cache="+f.RepoCacheMount,
		"--disk_cache="+f.DiskCacheMount,
	)
	// Load-bearing directives.
	mustContain(t, "Dockerfile", out,
		"--lockfile_mode=error",
		"test --build_tests_only",
		"/etc/bazel.bazelrc",
	)
	// The read-only-mount rationale (why vendor is baked, not mounted) must
	// survive generalization.
	mustContain(t, "Dockerfile", out,
		"read-only mount",
		"writable through the container overlay",
		"rm -f "+f.VendorMount+"/bazel-external",
	)
}

func TestSandboxBuild_BuildShRendersFacts(t *testing.T) {
	f := renderFacts()
	out, err := renderSandboxTemplate("templates/sandbox_build.build.sh.tmpl", f)
	if err != nil {
		t.Fatalf("render build.sh: %v", err)
	}

	// Substituted facts.
	mustContain(t, "build.sh", out,
		f.ImageTag,
		f.VendorDir,
		f.BaseImage,
		f.BazelVersion,
		"RUNTIME=\"${RUNTIME:-"+f.Runtime+"}\"",
	)
	// Two-phase warm-cache-as-layer recipe: offline warm test then commit.
	mustContain(t, "build.sh", out,
		"bazel test --build_tests_only --test_output=errors //...",
		"--network=none",
		`"$RUNTIME" commit`,
		"writable through the per-run container overlay",
	)
	// Repository-cache resolution falls back to `bazelisk info` when no path
	// was baked in.
	mustContain(t, "build.sh", out, "bazelisk info repository_cache")
}

// TestSandboxBuild_ScaffoldNoExec proves the scaffold-only path (no --run)
// writes the files and refreshes config WITHOUT ever invoking the orchestration
// seam. The spy fails the test if reached.
func TestSandboxBuild_ScaffoldNoExec(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "MODULE.bazel"), "module(name=\"proj\")\n")

	orig := execOrchestration
	execOrchestration = func(context.Context, io.Writer, sandboxBuildFacts) error {
		t.Fatal("scaffold-only path must NOT shell out, but execOrchestration was called")
		return nil
	}
	t.Cleanup(func() { execOrchestration = orig })

	out, err := runSandboxBuildCmd(t, "--target", repo)
	if err != nil {
		t.Fatalf("sandbox build: %v\noutput:\n%s", err, out)
	}

	for _, name := range []string{"Dockerfile", "build.sh"} {
		p := filepath.Join(repo, "bugbot-sandbox", name)
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("expected scaffold file %s: %v", p, statErr)
		}
	}
	// build.sh must be executable.
	info, statErr := os.Stat(filepath.Join(repo, "bugbot-sandbox", "build.sh"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("build.sh is not executable: mode %v", info.Mode().Perm())
	}
	mustContain(t, "stdout", out, "Next steps:", "bazel vendor --vendor_dir=")
}

// TestSandboxBuild_NonBazelError covers the rejection path for non-Bazel repos.
func TestSandboxBuild_NonBazelError(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), "module x\n\ngo 1.25\n")

	out, err := runSandboxBuildCmd(t, "--target", repo)
	if err == nil {
		t.Fatalf("expected error for non-Bazel repo, got nil\noutput:\n%s", out)
	}
	msg := err.Error()
	mustContain(t, "error", msg, "supports Bazel repos", "dep_strategy off/host/fetch")
	// Detected ecosystem is named so the user knows why.
	mustContain(t, "error", msg, "go_module")

	// And nothing was scaffolded.
	if _, statErr := os.Stat(filepath.Join(repo, "bugbot-sandbox")); !os.IsNotExist(statErr) {
		t.Errorf("non-Bazel repo must not be scaffolded (stat err: %v)", statErr)
	}
}

func TestSandboxBuild_RegisteredAndHelp(t *testing.T) {
	root := NewRootCmd()

	// Registration: the `sandbox build` path resolves.
	c, _, err := root.Find([]string{"sandbox", "build"})
	if err != nil {
		t.Fatalf("Find sandbox build: %v", err)
	}
	if c.Name() != "build" {
		t.Fatalf("resolved command = %q, want build", c.Name())
	}

	// `sandbox build --help` works and documents the load-bearing flags.
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"sandbox", "build", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sandbox build --help: %v", err)
	}
	mustContain(t, "help", buf.String(), "--run", "--image", "--bazel-version", "--vendor-dir")

	// `sandbox` lists the build subcommand.
	buf.Reset()
	root.SetArgs([]string{"sandbox", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sandbox --help: %v", err)
	}
	mustContain(t, "sandbox help", buf.String(), "build")
}

func TestRefreshSandboxImage_PreservesSiblings(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "bugbot.yaml")
	writeFile(t, path, "sandbox:\n  image: old:tag\n  cpus: 4\n  network: none\nreport:\n  dir: out\n")

	var buf bytes.Buffer
	if err := refreshSandboxImage(&buf, repo, "localhost/proj-bugbot-sandbox:latest"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got := readFile(t, path)
	mustContain(t, "bugbot.yaml", got,
		"image: localhost/proj-bugbot-sandbox:latest",
		"cpus: 4",
		"network: none",
		"report:",
	)
	if strings.Contains(got, "old:tag") {
		t.Errorf("old image tag survived refresh:\n%s", got)
	}
}

func TestRefreshSandboxImage_AddsBlockWhenAbsent(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "bugbot.yaml")
	writeFile(t, path, "report:\n  dir: out\n")

	var buf bytes.Buffer
	if err := refreshSandboxImage(&buf, repo, "new:tag"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got := readFile(t, path)
	mustContain(t, "bugbot.yaml", got, "sandbox:", "image: new:tag", "report:")
}

func TestRefreshSandboxImage_MissingFileSkips(t *testing.T) {
	repo := t.TempDir() // no bugbot.yaml

	var buf bytes.Buffer
	if err := refreshSandboxImage(&buf, repo, "new:tag"); err != nil {
		t.Fatalf("refresh on missing file must not error: %v", err)
	}
	mustContain(t, "skip note", buf.String(), "not found", "new:tag")
}

func TestDetectBazelVersion(t *testing.T) {
	withVer := t.TempDir()
	writeFile(t, filepath.Join(withVer, ".bazelversion"), "9.1.0\n")
	if got := detectBazelVersion(withVer); got != "9.1.0" {
		t.Errorf("detectBazelVersion(.bazelversion=9.1.0) = %q", got)
	}

	empty := t.TempDir()
	if got := detectBazelVersion(empty); got != defaultBazelVersion {
		t.Errorf("detectBazelVersion(absent) = %q, want %q", got, defaultBazelVersion)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func runSandboxBuildCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newSandboxBuildCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func mustContain(t *testing.T, label, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("%s: missing %q\n--- content ---\n%s", label, n, haystack)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
