//go:build integration

package miner

// TestRealPySweep runs the Python stringly-drift miner over a real Python corpus.
//
// Set BUGBOT_PY_SWEEP_DIR to the corpus root directory before running:
//
//	BUGBOT_PY_SWEEP_DIR=/path/to/requests/src go test ./internal/miner/ -tags integration -run TestRealPySweep -v
//
// The test skips (not fails) when BUGBOT_PY_SWEEP_DIR is unset or empty, so it
// never vacuously passes. When the corpus is present, it asserts:
//   - at least one .py file was found and attempted
//   - parse-failure rate is reported
//   - 0 false-positive leads on the parseable subset

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

func TestRealPySweep(t *testing.T) {
	root := os.Getenv("BUGBOT_PY_SWEEP_DIR")
	if root == "" {
		t.Skip("BUGBOT_PY_SWEEP_DIR not set; skipping real-corpus sweep")
	}

	var files []ingest.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".py" && ext != ".pyi" {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		files = append(files, ingest.File{
			Path:     filepath.ToSlash(rel),
			Language: ingest.LangPython,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(files) == 0 {
		t.Fatalf("BUGBOT_PY_SWEEP_DIR=%q contained 0 .py files; check path", root)
	}
	t.Logf("corpus: %d .py files under %s", len(files), root)

	snap := &ingest.Snapshot{Commit: "test", Root: root, Files: files}
	st := openStore(t)

	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}

	leads, _ := st.ListLeads(context.Background())
	parseable := len(files) - sum.PyParseFailures
	pct := 0.0
	if len(files) > 0 {
		pct = float64(sum.PyParseFailures) / float64(len(files)) * 100
	}

	t.Logf("total files:    %d", len(files))
	t.Logf("parse failures: %d (%.1f%%)", sum.PyParseFailures, pct)
	t.Logf("parseable:      %d", parseable)
	t.Logf("py-drift leads: %d", sum.StringlyPyDriftLeads)
	for i, l := range leads {
		t.Logf("  lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}

	fmt.Printf("SWEEP: total=%d parse_failures=%d (%.1f%%) parseable=%d leads=%d corpus=%s\n",
		len(files), sum.PyParseFailures, pct, parseable, len(leads), root)

	// Precision gate: 0 false positives on parseable files.
	if len(leads) != 0 {
		t.Errorf("precision gate FAIL: %d leads on real corpus — investigate each before declaring a genuine find", len(leads))
		for _, l := range leads {
			t.Logf("  INVESTIGATE: %s:%d: %s", l.File, l.Line, l.Note)
		}
	}
}
