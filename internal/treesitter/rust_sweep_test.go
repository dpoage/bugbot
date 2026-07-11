//go:build integration

package treesitter

// TestRustGrammarSweep measures the parse-error rate of the gotreesitter Rust
// grammar over a real Rust corpus.
//
// Set BUGBOT_RS_SWEEP_DIR to one or more colon-separated corpus root directories
// before running:
//
//	BUGBOT_RS_SWEEP_DIR=/tmp/ripgrep-corpus:/tmp/serde-corpus \
//	  go test ./internal/treesitter/ -tags integration -run TestRustGrammarSweep -v
//
// The test skips (not fails) when BUGBOT_RS_SWEEP_DIR is unset or empty.
// When the corpus is present, it asserts at least one .rs file was found and
// reports the ERROR-tree rate. It does NOT assert a pass-rate gate — that gate
// is documented below and evaluated manually; re-open bead bugbot-tdq5.1 when
// the grammar improves.
//
// # Sweep result recorded 2026-07-11 (ripgrep + serde, 308 files)
//
//   - Total files:     308 (100 ripgrep + 208 serde)
//   - Parse failures:   22 (7.1%)
//   - Pass gate:        <5% required to wire grammar in grammarTable
//   - OUTCOME:         STOP — rate exceeds gate; grammar NOT wired
//
// # Root cause
//
// gotreesitter v0.20.2's Rust grammar misparses a specific combination:
//
//   - a `return` statement inside a match arm
//   - inside a `move |...| { ... }` closure
//   - inside a `Box::new(...)` expression
//   - in a function that also uses `?` operators
//
// Minimal reproducer (HasError = true):
//
//	fn foo() -> Result<bool> {
//	    let x = a()?;
//	    run(|| {
//	        let y = &y;
//	        Box::new(move |z| {
//	            match thing() {
//	                Ok(v) => v,
//	                Err(_) => return early,  // ← triggers parse error
//	            }
//	        })
//	    });
//	    Ok(true)
//	}
//
// This pattern is pervasive in async/parallel Rust (rayon, tokio, crossbeam
// worker closures). The 14 failing ripgrep files all contain it. The 8 failing
// serde_derive files also hit it in generated-code paths.
//
// # Options for reaching <5%
//
//  1. Upgrade gotreesitter — wait for v0.20.3+ which may fix this grammar bug.
//     Check https://github.com/odvcencio/gotreesitter/releases for a rust parity
//     test that covers this pattern.
//
//  2. Partial skip — do not skip the whole file on HasError; instead only skip
//     ERROR nodes in query matches. gotreesitter's query engine already skips
//     error nodes in structural queries, so defs/refs in non-errored subtrees
//     may still be captured even when tree.RootNode().HasError() is true.
//     Validate: measure definition-capture rate on the 22 failing files.
//
//  3. Grammar patch — generate a fixed Rust grammar from tree-sitter-rust HEAD
//     using gotreesitter's grammargen pipeline (see cmd/grammargen/). This
//     requires the Rust C toolchain to generate grammar.json from grammar.js.
//
// # Honest claim
//
// "Rust grammar validated against 308 real .rs files (ripgrep + serde);
//  22 files (7.1%) produce parse errors in gotreesitter v0.20.2, exceeding
//  the <5% gate. Grammar NOT wired. Re-open when rate improves."

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"
)

func TestRustGrammarSweep(t *testing.T) {
	sweepEnv := os.Getenv("BUGBOT_RS_SWEEP_DIR")
	if sweepEnv == "" {
		t.Skip("BUGBOT_RS_SWEEP_DIR not set; skipping Rust grammar sweep")
	}

	entry := tsregistry.DetectLanguage("x.rs")
	if entry == nil {
		t.Fatal("no Rust grammar registered in gotreesitter; cannot sweep")
	}
	lang := entry.Language()
	parser := gts.NewParser(lang)

	dirs := strings.Split(sweepEnv, ":")

	var total, errCount int
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || filepath.Ext(path) != ".rs" {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			tree, parseErr := parser.Parse(src)
			if parseErr != nil || tree == nil {
				errCount++
				total++
				t.Logf("PARSE_ERR: %s", path)
				return nil
			}
			total++
			if tree.RootNode().HasError() {
				errCount++
				t.Logf("HAS_ERROR: %s", path)
			}
			tree.Release()
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if total == 0 {
		t.Fatalf("BUGBOT_RS_SWEEP_DIR=%q contained 0 .rs files; check path", sweepEnv)
	}

	pct := float64(errCount) / float64(total) * 100
	fmt.Printf("SWEEP: total=%d errors=%d (%.1f%%) corpus=%s\n",
		total, errCount, pct, sweepEnv)

	t.Logf("total .rs files:  %d", total)
	t.Logf("parse errors:     %d (%.1f%%)", errCount, pct)
	t.Logf("pass gate:        <5.0%%")
	if pct < 5.0 {
		t.Logf("RESULT: PASS — rate %.1f%% is under gate; grammar may be wired", pct)
	} else {
		t.Logf("RESULT: STOP — rate %.1f%% exceeds gate; do NOT wire grammar", pct)
	}

	// The test itself does not fail on rate — it is a measurement tool.
	// The gate decision is documented above and in bead bugbot-tdq5.1.
}
