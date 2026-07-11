//go:build integration

package miner

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// TestRealTSSweep runs the TypeScript stringly-drift miner over the effect
// library source (412 real .ts files). This is the empirical precision gate
// required by bead bugbot-93z.20: sweep a real mid-size TS codebase and
// confirm 0 false positives.
//
// Sweep result (recorded 2026-07-11):
//   - Corpus: github.com/Effect-TS/effect — effect/src, 412 .ts files
//   - Leads:  0
//   - Result: PASS (0 FPs on 412 real TS files)
//
// Run with: go test ./internal/miner/ -tags integration -run TestRealTSSweep -v
func TestRealTSSweep(t *testing.T) {
	root := "/home/dustin/code/personal/services-runtime/.opencode/node_modules/effect/src"
	var files []ingest.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
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
		t.Fatalf("walk: %v", err)
	}
	t.Logf("sweeping %d TS files under effect/src", len(files))

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
	t.Logf("sweep result: StringlyTSDriftLeads=%d, leads=%d", sum.StringlyTSDriftLeads, len(leads))
	for i, l := range leads {
		t.Logf("  lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}
	fmt.Printf("SWEEP: %d leads on %d TS files (effect library)\n", len(leads), len(files))

	if len(leads) != 0 {
		for _, l := range leads {
			t.Errorf("FP candidate: %s:%d — %s", l.File, l.Line, l.Note)
		}
		t.Errorf("sweep produced %d leads on clean corpus; investigate each", len(leads))
	}
}
