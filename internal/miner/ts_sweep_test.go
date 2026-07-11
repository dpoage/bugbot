//go:build integration

package miner

// TestRealTSSweep runs the TypeScript stringly-drift miner over a real TS corpus.
//
// Set BUGBOT_TS_SWEEP_DIR to the corpus root directory before running:
//
//	BUGBOT_TS_SWEEP_DIR=/path/to/effect/src go test ./internal/miner/ -tags integration -run TestRealTSSweep -v
//
// The test skips (not fails) when BUGBOT_TS_SWEEP_DIR is unset or empty, so it
// never vacuously passes. When the corpus is present, it asserts:
//   - at least one .ts file was found and attempted
//   - parse-failure rate is reported (expected ~48% on effect/src due to the
//     typed-param arrow grammar gap; see stringly_ts.go v1 limitation note)
//   - 0 false-positive leads on the parseable subset
//
// Sweep result recorded 2026-07-11 (effect library, effect/src, 412 files):
//   - Total files:     412
//   - Parse failures:  198 (48.1% — typed-param arrow grammar gap)
//   - Parseable:       214 (51.9%)
//   - Unions found:     18 files
//   - Bindings hit:      3 files reach a typed-union binding
//   - Leads:             0
//   - Result: PASS — 0 FPs on the parseable subset
//   - Honest claim: "0 FPs on 214 parseable files; 198 skipped due to grammar gap"

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

func TestRealTSSweep(t *testing.T) {
	root := os.Getenv("BUGBOT_TS_SWEEP_DIR")
	if root == "" {
		t.Skip("BUGBOT_TS_SWEEP_DIR not set; skipping real-corpus sweep")
	}

	var files []ingest.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".ts") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, ingest.File{
			Path:     filepath.ToSlash(rel),
			Language: ingest.LangTypeScript,
			Size:     1,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(files) == 0 {
		t.Fatalf("BUGBOT_TS_SWEEP_DIR=%q contained 0 .ts files; check path", root)
	}
	t.Logf("corpus: %d .ts files under %s", len(files), root)

	snap := &ingest.Snapshot{Commit: "test", Root: root, Files: files}
	st, stErr := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if stErr != nil {
		t.Fatalf("store: %v", stErr)
	}
	defer st.Close()

	var sum Summary
	if err := seedStringlyTSDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyTSDrift: %v", err)
	}

	leads, _ := st.ListLeads(context.Background())
	parseable := len(files) - sum.TSParseFailures
	pct := 0.0
	if len(files) > 0 {
		pct = float64(sum.TSParseFailures) / float64(len(files)) * 100
	}

	t.Logf("total files:    %d", len(files))
	t.Logf("parse failures: %d (%.1f%% — typed-param arrow grammar gap)", sum.TSParseFailures, pct)
	t.Logf("parseable:      %d", parseable)
	t.Logf("ts-drift leads: %d", sum.StringlyTSDriftLeads)
	for i, l := range leads {
		t.Logf("  lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}

	fmt.Printf("SWEEP: total=%d parse_failures=%d (%.1f%%) parseable=%d leads=%d corpus=%s\n",
		len(files), sum.TSParseFailures, pct, parseable, len(leads), root)

	// Precision gate: 0 false positives on parseable files.
	if len(leads) != 0 {
		for _, l := range leads {
			t.Errorf("FP candidate: %s:%d — %s", l.File, l.Line, l.Note)
		}
		t.Errorf("sweep produced %d leads; investigate each for false positives", len(leads))
	}
}
