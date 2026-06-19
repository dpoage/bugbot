package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// ── per-artifact pure-function tests ─────────────────────────────────────────

func TestMineDockerfileImage(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"simple", "FROM ubuntu:22.04\nRUN apt-get install -y curl", "ubuntu:22.04"},
		{"multi-stage first", "FROM golang:1.21 AS build\nFROM ubuntu:22.04", "golang:1.21"},
		{"arg-expanded image ignored", "FROM $BASE_IMAGE", ""},
		{"empty", "", ""},
		{"scratch", "FROM scratch", "scratch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mineDockerfileImage(tc.content)
			if got != tc.want {
				t.Errorf("mineDockerfileImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMineDockerfileAPTPackages(t *testing.T) {
	content := `FROM ubuntu:22.04
RUN apt-get update && apt-get install -y --no-install-recommends \
    libssl-dev \
    pkg-config \
    curl
RUN apt install -y git`
	pkgs := mineDockerfileAPTPackages(content)
	want := map[string]bool{"libssl-dev": true, "pkg-config": true, "curl": true, "git": true}
	for _, p := range pkgs {
		if !want[p] {
			t.Errorf("unexpected package %q", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("missing expected package %q", p)
	}
}

func TestMineDockerfileAPKPackages(t *testing.T) {
	content := `FROM alpine:3.18
RUN apk add --no-cache gcc musl-dev`
	pkgs := mineDockerfileAPKPackages(content)
	want := map[string]bool{"gcc": true, "musl-dev": true}
	for _, p := range pkgs {
		if !want[p] {
			t.Errorf("unexpected apk package %q", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("missing expected apk package %q", p)
	}
}

func TestMineWorkflowVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			"go-version",
			`- uses: actions/setup-go@v4
  with:
    go-version: '1.21'`,
			"1.21",
		},
		{
			"python-version",
			`- uses: actions/setup-python@v4
  with:
    python-version: "3.11"`,
			"3.11",
		},
		{
			"node-version",
			`- uses: actions/setup-node@v4
  with:
    node-version: "20"`,
			"20",
		},
		{"none", "just some yaml content", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mineWorkflowVersion(tc.content)
			if got != tc.want {
				t.Errorf("mineWorkflowVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMineWorkflowContainerImage(t *testing.T) {
	content := `jobs:
  build:
    container:
      image: golang:1.21-alpine`
	got := mineWorkflowContainerImage(content)
	if got != "golang:1.21-alpine" {
		t.Errorf("mineWorkflowContainerImage() = %q, want %q", got, "golang:1.21-alpine")
	}
}

func TestMineGitlabImage(t *testing.T) {
	content := `image: python:3.11-slim

stages:
  - test`
	got := mineGitlabImage(content)
	if got != "python:3.11-slim" {
		t.Errorf("mineGitlabImage() = %q, want %q", got, "python:3.11-slim")
	}
}

func TestMineDevcontainerImage(t *testing.T) {
	content := `{
  "name": "Go Dev",
  "image": "mcr.microsoft.com/devcontainers/go:1.21",
  "features": {}
}`
	got := mineDevcontainerImage(content)
	if got != "mcr.microsoft.com/devcontainers/go:1.21" {
		t.Errorf("mineDevcontainerImage() = %q", got)
	}
}

func TestMineTestHints(t *testing.T) {
	content := "test:\n\tgo test ./...\n\ncheck:\n\tgolangci-lint run\n"
	hints := mineTestHints(content)
	found := false
	for _, h := range hints {
		if h == "go test ./..." {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'go test ./...' in hints %v", hints)
	}
}

func TestMineToxHints(t *testing.T) {
	content := `[testenv]
commands = pytest tests/ -v
`
	hints := mineToxHints(content)
	if len(hints) == 0 || hints[0] != "pytest tests/ -v" {
		t.Errorf("mineToxHints() = %v, want [pytest tests/ -v]", hints)
	}
}

func TestMineFlakeNix(t *testing.T) {
	// flake.nix causes "nix build" to be added to TestCmdHints.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "flake.nix"), []byte(`{ outputs = {}; }`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mineArtifacts(dir)
	found := false
	for _, h := range out.TestCmdHints {
		if h == "nix build" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'nix build' hint for flake.nix repo, got %v", out.TestCmdHints)
	}
}

// TestMineArtifactsIntegration exercises mineArtifacts against a synthetic repo tree.
func TestMineArtifactsIntegration(t *testing.T) {
	dir := t.TempDir()

	// Dockerfile
	mustWrite(t, filepath.Join(dir, "Dockerfile"), `FROM golang:1.21-alpine
RUN apk add --no-cache git gcc`)

	// tox.ini
	mustWrite(t, filepath.Join(dir, "tox.ini"), "[testenv]\ncommands = pytest -q\n")

	out := mineArtifacts(dir)

	if out.BaseImage != "golang:1.21-alpine" {
		t.Errorf("BaseImage = %q, want golang:1.21-alpine", out.BaseImage)
	}
	hasPkg := func(want string) bool {
		for _, p := range out.SystemDeps {
			if p == want {
				return true
			}
		}
		return false
	}
	if !hasPkg("git") {
		t.Errorf("expected 'git' in SystemDeps %v", out.SystemDeps)
	}
	if !hasPkg("gcc") {
		t.Errorf("expected 'gcc' in SystemDeps %v", out.SystemDeps)
	}
	if len(out.RawArtifacts) == 0 {
		t.Error("expected RawArtifacts to be populated")
	}
}
