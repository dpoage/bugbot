package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// minerOutput holds everything the artifact miner extracted from a repo's
// CI/build/dev-container files. All fields are optional; callers should treat
// empty strings and nil slices as "not found". This is a pure data struct so
// synthesisSandbox is a pure function of it (unit-testable without a filesystem).
type minerOutput struct {
	// BaseImage is the first FROM <image> seen (Dockerfile) or the image from
	// devcontainer.json / GitHub Actions workflow.
	BaseImage string
	// SystemDeps are package names that appeared in apt-get/apk install calls.
	SystemDeps []string
	// ToolchainVersion is an explicit language runtime version (e.g. "1.21",
	// "3.11") found in a workflow matrix, .tool-versions, or devcontainer.json.
	ToolchainVersion string
	// TestCmdHints are test invocation strings seen in Makefile / tox / noxfile.
	TestCmdHints []string
	// RawArtifacts carries the raw text of every scanned artifact, keyed by a
	// short label ("Dockerfile", ".github/workflows/ci.yml", etc.). This is fed
	// verbatim to the agent tier so it can reason over the original context.
	RawArtifacts map[string]string
}

// mineArtifacts scans the repo at repoDir for CI/build environment artifacts
// and returns the extracted minerOutput. It never executes any code — it only
// reads and parses file content.
func mineArtifacts(repoDir string) minerOutput {
	out := minerOutput{RawArtifacts: make(map[string]string)}

	scanFile := func(label, path string, fn func(content string)) {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		content := string(data)
		out.RawArtifacts[label] = content
		fn(content)
	}

	// ── Dockerfile* ────────────────────────────────────────────────────────────
	dockerfiles, _ := filepath.Glob(filepath.Join(repoDir, "Dockerfile*"))
	for _, df := range dockerfiles {
		label := filepath.Base(df)
		scanFile(label, df, func(content string) {
			if out.BaseImage == "" {
				out.BaseImage = mineDockerfileImage(content)
			}
			for _, pkg := range mineDockerfileAPTPackages(content) {
				out.SystemDeps = appendUniq(out.SystemDeps, pkg)
			}
			for _, pkg := range mineDockerfileAPKPackages(content) {
				out.SystemDeps = appendUniq(out.SystemDeps, pkg)
			}
		})
	}

	// ── .devcontainer/devcontainer.json ────────────────────────────────────────
	scanFile(".devcontainer/devcontainer.json",
		filepath.Join(repoDir, ".devcontainer", "devcontainer.json"),
		func(content string) {
			if img := mineDevcontainerImage(content); img != "" && out.BaseImage == "" {
				out.BaseImage = img
			}
			if ver := mineDevcontainerVersion(content); ver != "" && out.ToolchainVersion == "" {
				out.ToolchainVersion = ver
			}
		})

	// ── .github/workflows/*.yml ────────────────────────────────────────────────
	wfGlob := filepath.Join(repoDir, ".github", "workflows", "*.yml")
	wfFiles, _ := filepath.Glob(wfGlob)
	wfGlob2 := filepath.Join(repoDir, ".github", "workflows", "*.yaml")
	wfFiles2, _ := filepath.Glob(wfGlob2)
	wfFiles = append(wfFiles, wfFiles2...)
	for _, wf := range wfFiles {
		label := filepath.Join(".github/workflows", filepath.Base(wf))
		scanFile(label, wf, func(content string) {
			if ver := mineWorkflowVersion(content); ver != "" && out.ToolchainVersion == "" {
				out.ToolchainVersion = ver
			}
			if img := mineWorkflowContainerImage(content); img != "" && out.BaseImage == "" {
				out.BaseImage = img
			}
			for _, pkg := range mineWorkflowAPTPackages(content) {
				out.SystemDeps = appendUniq(out.SystemDeps, pkg)
			}
		})
	}

	// ── .gitlab-ci.yml ─────────────────────────────────────────────────────────
	scanFile(".gitlab-ci.yml",
		filepath.Join(repoDir, ".gitlab-ci.yml"),
		func(content string) {
			if img := mineGitlabImage(content); img != "" && out.BaseImage == "" {
				out.BaseImage = img
			}
			for _, pkg := range mineWorkflowAPTPackages(content) {
				out.SystemDeps = appendUniq(out.SystemDeps, pkg)
			}
		})

	// ── Makefile ───────────────────────────────────────────────────────────────
	scanFile("Makefile",
		filepath.Join(repoDir, "Makefile"),
		func(content string) {
			for _, hint := range mineTestHints(content) {
				out.TestCmdHints = appendUniq(out.TestCmdHints, hint)
			}
		})

	// ── tox.ini ────────────────────────────────────────────────────────────────
	scanFile("tox.ini",
		filepath.Join(repoDir, "tox.ini"),
		func(content string) {
			for _, hint := range mineToxHints(content) {
				out.TestCmdHints = appendUniq(out.TestCmdHints, hint)
			}
		})

	// ── noxfile.py ─────────────────────────────────────────────────────────────
	scanFile("noxfile.py",
		filepath.Join(repoDir, "noxfile.py"),
		func(content string) {
			out.TestCmdHints = appendUniq(out.TestCmdHints, "nox")
		})

	// ── flake.nix ──────────────────────────────────────────────────────────────
	scanFile("flake.nix",
		filepath.Join(repoDir, "flake.nix"),
		func(content string) {
			// Nix builds imply a nix-based image or devShell; record as a hint.
			out.TestCmdHints = appendUniq(out.TestCmdHints, "nix build")
		})

	return out
}

// ── per-artifact parsers (pure functions, unit-testable) ──────────────────────

var reDockerfileFrom = regexp.MustCompile(`(?mi)^FROM\s+(\S+)`)

// mineDockerfileImage returns the base image from the first FROM directive.
func mineDockerfileImage(content string) string {
	m := reDockerfileFrom.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	img := m[1]
	// Strip build-arg interpolation; if it references a variable we can't
	// resolve, return empty rather than emit a garbled image name.
	if strings.HasPrefix(img, "$") || strings.Contains(img, "${") {
		return ""
	}
	// Strip "as <alias>" that sometimes follows on the same line.
	if idx := strings.Index(strings.ToLower(img), " as "); idx >= 0 {
		img = img[:idx]
	}
	return img
}

var reAPTInstall = regexp.MustCompile(`(?i)apt(?:-get)?\s+install\b([^&|;\n]*)`)
var reAPKAdd = regexp.MustCompile(`(?i)apk\s+add\b([^&|;\n]*)`)

// mineDockerfileAPTPackages extracts apt-get install package names.
func mineDockerfileAPTPackages(content string) []string {
	return extractPackages(reAPTInstall, joinContinuations(content))
}

// mineDockerfileAPKPackages extracts apk add package names.
func mineDockerfileAPKPackages(content string) []string {
	return extractPackages(reAPKAdd, joinContinuations(content))
}

// joinContinuations replaces backslash-newline sequences with a space so that
// multi-line shell commands are treated as a single logical line.
func joinContinuations(content string) string {
	return strings.ReplaceAll(content, "\\\n", " ")
}

// extractPackages pulls individual package names from a pkg-manager install match.
func extractPackages(re *regexp.Regexp, content string) []string {
	var pkgs []string
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		if len(m) < 2 {
			continue
		}
		for _, tok := range strings.Fields(m[1]) {
			tok = strings.TrimRight(tok, `\`)
			// Skip flags (start with -), build-arg references, and empty tokens.
			if tok == "" || strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "$") {
				continue
			}
			pkgs = append(pkgs, tok)
		}
	}
	return pkgs
}

// mineWorkflowAPTPackages extracts apt packages from any workflow/CI YAML
// using the same regex used for Dockerfiles (matches RUN lines too).
func mineWorkflowAPTPackages(content string) []string {
	return mineDockerfileAPTPackages(content)
}

// mineWorkflowVersion extracts language versions from GitHub Actions matrix or
// setup-* step inputs.
var reWorkflowGoVer = regexp.MustCompile(`go-version['":\s]+([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
var reWorkflowPyVer = regexp.MustCompile(`python-version['":\s]+['"]?([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
var reWorkflowNodeVer = regexp.MustCompile(`node-version['":\s]+['"]?([0-9]+(?:\.[0-9]+)?)`)

func mineWorkflowVersion(content string) string {
	for _, re := range []*regexp.Regexp{reWorkflowGoVer, reWorkflowPyVer, reWorkflowNodeVer} {
		if m := re.FindStringSubmatch(content); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

// mineWorkflowContainerImage extracts a `container: image: <img>` or
// `image: <img>` value from a GitHub Actions workflow.
var reWorkflowContainer = regexp.MustCompile(`(?m)^\s+image:\s+(\S+)`)

func mineWorkflowContainerImage(content string) string {
	m := reWorkflowContainer.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// mineGitlabImage extracts the top-level `image:` from a GitLab CI file.
var reGitlabImage = regexp.MustCompile(`(?m)^image:\s+(\S+)`)

func mineGitlabImage(content string) string {
	m := reGitlabImage.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// mineDevcontainerImage extracts "image" from a devcontainer.json (simple
// regex; full JSON parse would be overkill given the variety of devcontainer
// formats).
var reDevcontainerImage = regexp.MustCompile(`"image"\s*:\s*"([^"]+)"`)

func mineDevcontainerImage(content string) string {
	m := reDevcontainerImage.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// mineDevcontainerVersion extracts a toolchain version from devcontainer.json.
var reDevcontainerVer = regexp.MustCompile(`"version"\s*:\s*"([0-9]+\.[0-9]+(?:\.[0-9]+)?)"`)

func mineDevcontainerVersion(content string) string {
	m := reDevcontainerVer.FindStringSubmatch(content)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// mineTestHints extracts test-related make targets from a Makefile.
var reTestTarget = regexp.MustCompile(`(?m)^(test[\w-]*|check|verify)\s*:`)

func mineTestHints(content string) []string {
	var hints []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	inTestTarget := false
	for scanner.Scan() {
		line := scanner.Text()
		if reTestTarget.MatchString(line) {
			inTestTarget = true
			continue
		}
		if inTestTarget {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				inTestTarget = false
				continue
			}
			if strings.HasPrefix(line, "\t") {
				hints = appendUniq(hints, strings.TrimPrefix(trimmed, "@"))
			} else {
				inTestTarget = false
			}
		}
	}
	return hints
}

// mineToxHints extracts commands from tox.ini [testenv] commands.
var reToxCommands = regexp.MustCompile(`(?m)^commands\s*=\s*(.+)`)

func mineToxHints(content string) []string {
	m := reToxCommands.FindStringSubmatch(content)
	if len(m) < 2 {
		return nil
	}
	return []string{strings.TrimSpace(m[1])}
}

// appendUniq appends s to slice only when it is not already present.
func appendUniq(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}
