package repro

// bundle.go defines the machine-readable contract for a repro artifact
// bundle (see writeArtifacts in artifacts.go) and the Bundle type that reads
// one back off disk. A bundle written by writeArtifacts is no longer
// write-only prose: manifest.json is the canonical, parseable identity that
// `bugbot bundle audit`/`replay` (internal/cli/bundle.go) and the static
// corpus suite (internal/repro/testdata/corpus, corpus_test.go) consume
// instead of scraping README.md.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// ManifestFileName is the manifest's fixed basename within a bundle
// directory, sibling to README.md and run.sh.
const ManifestFileName = "manifest.json"

// ManifestFinding is the finding identity captured in manifest.json — enough
// to attribute a bundle to its origin without needing store access.
type ManifestFinding struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	CommitSHA   string `json:"commit_sha"`
}

// ManifestPlan is the repro plan captured in manifest.json: the exact argv
// the sandbox executed and the repro-relative paths of the files it ran
// against. File CONTENTS are not duplicated into the manifest — they are the
// real files written alongside it in the bundle directory (see
// writePlanFiles); Files here is the index LoadBundle uses to read them back.
type ManifestPlan struct {
	Cmd   []string `json:"cmd"`
	Files []string `json:"files"`
}

// ManifestSandbox is the sandbox execution policy captured in manifest.json.
type ManifestSandbox struct {
	Image     string            `json:"image"`
	Ecosystem sandbox.Ecosystem `json:"ecosystem"`
	Network   string            `json:"network"`
}

// ManifestResult is the sandboxed run's outcome captured in manifest.json.
type ManifestResult struct {
	ExitCode     int  `json:"exit_code"`
	SentinelSeen bool `json:"sentinel_seen"`
}

// Manifest is the JSON contract written to manifest.json by writeArtifacts
// and read back by LoadBundle. It is the bundle's machine-readable identity:
// everything `bugbot bundle audit`/`replay` and the corpus suite need to
// re-execute or classify the bundle without re-deriving it from README.md
// prose.
type Manifest struct {
	Finding       ManifestFinding `json:"finding"`
	Plan          ManifestPlan    `json:"plan"`
	Sandbox       ManifestSandbox `json:"sandbox"`
	Result        ManifestResult  `json:"result"`
	BugbotVersion string          `json:"bugbot_version"`
}

// Bundle is a repro bundle loaded back from disk: the manifest plus the
// actual repro file contents, keyed the same way Plan.Files is, so a loaded
// Bundle round-trips into a *Plan for replay/audit without re-parsing
// README.md prose.
type Bundle struct {
	// Dir is the bundle's directory on disk (the directory manifest.json,
	// run.sh, README.md and the repro files live in).
	Dir string
	// Manifest is the parsed manifest.json contents.
	Manifest Manifest
	// Files holds each manifest.Plan.Files entry's content, keyed by the
	// same repro-relative path.
	Files map[string]string
}

// LoadBundle reads a bundle directory written by writeArtifacts: manifest.json
// plus every file manifest.Plan.Files names. It returns an error if the
// manifest is missing/malformed, or any listed file is missing or escapes the
// bundle directory — a bundle that cannot fully round-trip is not safe to
// audit or replay.
func LoadBundle(dir string) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return nil, fmt.Errorf("repro: load bundle %s: %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("repro: parse manifest %s: %w", dir, err)
	}
	if len(m.Plan.Cmd) == 0 {
		return nil, fmt.Errorf("repro: bundle %s: manifest has empty plan.cmd", dir)
	}

	files := make(map[string]string, len(m.Plan.Files))
	for _, rel := range m.Plan.Files {
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("repro: bundle %s: unsafe manifest file path %q", dir, rel)
		}
		content, rerr := os.ReadFile(filepath.Join(dir, clean))
		if rerr != nil {
			return nil, fmt.Errorf("repro: bundle %s: read repro file %q: %w", dir, rel, rerr)
		}
		files[rel] = string(content)
	}
	return &Bundle{Dir: dir, Manifest: m, Files: files}, nil
}

// Plan reconstructs the *Plan this bundle demonstrated: the exact files and
// command manifest.json recorded, ready for execute() (replay) or the static
// target-execution gate (audit) — the same shape either path consumes,
// exercising the workspace-reconstruction contract that the official run
// itself relies on (see repro.go's Attempt/execute).
func (b *Bundle) Plan() *Plan {
	return &Plan{Files: b.Files, Cmd: b.Manifest.Plan.Cmd}
}
