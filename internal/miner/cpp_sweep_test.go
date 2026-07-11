//go:build integration

package miner

// TestRealCppSweep runs the C/C++ enum-drift miner over a real C/C++ corpus.
//
// Set BUGBOT_CPP_SWEEP_DIR to the corpus root directory before running:
//
//	BUGBOT_CPP_SWEEP_DIR=/path/to/redis go test ./internal/miner/ -tags integration -run TestRealCppSweep -v
//
// The test skips (not fails) when BUGBOT_CPP_SWEEP_DIR is unset or empty, so it
// never vacuously passes. When the corpus is present, it asserts:
//   - at least one C or C++ file was found and attempted
//   - parse-failure rate is reported (expected high due to enum grammar gap)
//   - 0 false-positive leads (precision gate)
//
// Sweep result recorded 2026-07-11 (redis/src, BUGBOT_CPP_SWEEP_DIR=/tmp/redis-sweep):
//   - Total files:     see t.Logf output
//   - Parse failures:  ~high (enum declarations in .c files cause HasError)
//   - Leads:           0 (0 FPs on the clean-parsing subset)

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

func TestRealCppSweep(t *testing.T) {
	root := os.Getenv("BUGBOT_CPP_SWEEP_DIR")
	if root == "" {
		t.Skip("BUGBOT_CPP_SWEEP_DIR not set; skipping real-corpus C/C++ sweep")
	}

	var files []ingest.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		var lang ingest.Language
		switch ext {
		case ".c", ".h":
			lang = ingest.LangC
		case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
			lang = ingest.LangCPP
		default:
			return nil
		}
		files = append(files, ingest.File{Path: filepath.ToSlash(rel), Language: lang})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(files) == 0 {
		t.Fatalf("BUGBOT_CPP_SWEEP_DIR=%q contained 0 C/C++ files; check path", root)
	}
	t.Logf("corpus: %d C/C++ files under %s", len(files), root)

	snap := &ingest.Snapshot{Commit: "test", Root: root, Files: files}
	st := openStore(t)

	var sum Summary
	if err := seedCppEnumDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedCppEnumDrift: %v", err)
	}

	leads, _ := st.ListLeads(context.Background())
	parseable := len(files) - sum.CppParseFailures
	pct := 0.0
	if len(files) > 0 {
		pct = float64(sum.CppParseFailures) / float64(len(files)) * 100
	}

	t.Logf("total files:    %d", len(files))
	t.Logf("parse failures: %d (%.1f%% — enum grammar gap in gotreesitter v0.20.2)", sum.CppParseFailures, pct)
	t.Logf("parseable:      %d", parseable)
	t.Logf("cpp-drift leads: %d", sum.CppDriftLeads)
	for i, l := range leads {
		t.Logf("  lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}

	fmt.Printf("SWEEP: total=%d parse_failures=%d (%.1f%%) parseable=%d leads=%d corpus=%s\n",
		len(files), sum.CppParseFailures, pct, parseable, len(leads), root)

	// Precision gate: 0 false positives on parseable files.
	if len(leads) != 0 {
		t.Errorf("PRECISION GATE FAILED: %d leads on corpus (expected 0 FPs)", len(leads))
		for i, l := range leads {
			t.Logf("  FP[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
		}
	}
}
