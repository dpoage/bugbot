//go:build integration

package miner

// TestRealRSSweep runs the Rust &str match-drift miner over a real Rust corpus.
//
// Set BUGBOT_RS_SWEEP_DIR to one or more colon-separated corpus roots before
// running (same env var used by the grammar sweep test in
// internal/treesitter/rust_sweep_test.go):
//
//	BUGBOT_RS_SWEEP_DIR=/tmp/ripgrep-corpus:/tmp/serde-corpus \
//	  go test ./internal/miner/ -tags integration -run TestRealRSSweep -v
//
// The test skips (not fails) when BUGBOT_RS_SWEEP_DIR is unset or empty, so
// it never vacuously passes. When the corpus is present, it asserts:
//   - at least one .rs file was found and attempted
//   - parse-failure rate is reported (~7.1% expected on ripgrep + serde)
//   - 0 false-positive leads on the parseable subset
//
// Any lead produced is printed and investigated manually before the test may
// be considered passing; the precision gate requires 0 FPs.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

func TestRealRSSweep(t *testing.T) {
	sweepEnv := os.Getenv("BUGBOT_RS_SWEEP_DIR")
	if sweepEnv == "" {
		t.Skip("BUGBOT_RS_SWEEP_DIR not set; skipping real-corpus sweep")
	}

	roots := strings.Split(sweepEnv, ":")

	var files []ingest.File
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".rs") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			files = append(files, ingest.File{
				Path:     filepath.ToSlash(rel),
				Language: ingest.LangRust,
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(files) == 0 {
		t.Fatalf("BUGBOT_RS_SWEEP_DIR=%q contained 0 .rs files; check path", sweepEnv)
	}
	t.Logf("corpus: %d .rs files under %s", len(files), sweepEnv)

	// Use the first root as the snapshot root. For multi-root corpora, we
	// run separate sweeps per root to keep file paths relative.
	// Simple case: use the first root and only its files.
	firstRoot := strings.Split(sweepEnv, ":")[0]
	var firstFiles []ingest.File
	for _, f := range files {
		abs := filepath.Join(firstRoot, filepath.FromSlash(f.Path))
		if _, err := os.Stat(abs); err == nil {
			firstFiles = append(firstFiles, f)
		}
	}
	if len(firstFiles) == 0 {
		// Fallback: run all files under each root separately.
		firstFiles = files
	}

	// For multi-root: run each root separately and accumulate.
	type sweepResult struct {
		root         string
		total        int
		parseFailure int
		leads        int
	}
	var results []sweepResult
	var allLeads []interface{ GetFile() string }

	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		var rootFiles []ingest.File
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".rs") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			rootFiles = append(rootFiles, ingest.File{
				Path:     filepath.ToSlash(rel),
				Language: ingest.LangRust,
			})
			return nil
		})
		if len(rootFiles) == 0 {
			continue
		}

		snap := &ingest.Snapshot{Commit: "sweep", Root: root, Files: rootFiles}
		st := openStore(t)
		var sum Summary
		if err := seedStringlyRsDrift(context.Background(), snap, st, &sum); err != nil {
			t.Fatalf("seedStringlyRsDrift(%s): %v", root, err)
		}
		leads, _ := st.ListLeads(context.Background())
		results = append(results, sweepResult{
			root:         root,
			total:        len(rootFiles),
			parseFailure: sum.RsParseFailures,
			leads:        len(leads),
		})
		for _, l := range leads {
			t.Logf("LEAD %s:%d: %s", l.File, l.Line, l.Note)
		}
		_ = allLeads // allLeads used for reporting
	}

	// Aggregate and report.
	var totalFiles, totalFail, totalLeads int
	for _, r := range results {
		totalFiles += r.total
		totalFail += r.parseFailure
		totalLeads += r.leads
		parseable := r.total - r.parseFailure
		pct := 0.0
		if r.total > 0 {
			pct = float64(r.parseFailure) / float64(r.total) * 100
		}
		t.Logf("root=%s total=%d parse_failures=%d (%.1f%%) parseable=%d leads=%d",
			r.root, r.total, r.parseFailure, pct, parseable, r.leads)
	}

	parseable := totalFiles - totalFail
	failPct := 0.0
	if totalFiles > 0 {
		failPct = float64(totalFail) / float64(totalFiles) * 100
	}

	fmt.Printf("SWEEP: total=%d parse_failures=%d (%.1f%%) parseable=%d leads=%d corpus=%s\n",
		totalFiles, totalFail, failPct, parseable, totalLeads, sweepEnv)

	if totalFiles == 0 {
		t.Fatal("no .rs files found in any corpus root")
	}

	// Precision gate: 0 false positives.
	// Any lead here is a CANDIDATE FP. Investigate before declaring green.
	if totalLeads != 0 {
		t.Errorf("PRECISION GATE FAIL: %d leads on real corpus (expected 0 FPs); investigate each lead above",
			totalLeads)
	}
}
