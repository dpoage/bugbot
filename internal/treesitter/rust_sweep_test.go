//go:build integration

package treesitter

// TestRustGrammarSweep measures two rates of the gotreesitter Rust grammar
// over a real Rust corpus: parse-error rate (HasError) and def-capture rate
// (AST-ground-truth definition nodes vs. defQuery captures).
//
// Set BUGBOT_RS_SWEEP_DIR to one or more colon-separated corpus root directories
// before running:
//
//	BUGBOT_RS_SWEEP_DIR=/tmp/ripgrep-corpus:/tmp/serde-corpus \
//	  go test ./internal/treesitter/ -tags integration -run TestRustGrammarSweep -v
//
// The test skips (not fails) when BUGBOT_RS_SWEEP_DIR is unset or empty.
// When the corpus is present, it asserts at least one .rs file was found,
// reports both rates, and asserts the def-capture gate (≥90%).
//
// # Two-tier outcome (recorded 2026-07-11, ripgrep + serde, 308 files)
//
//   - Total files:            308 (100 ripgrep + 208 serde)
//   - Parse failures:          22 (7.1%)
//   - Miner gate:             <5% parse-fail required — MINER TIER: NOT WIRED
//   - HasError def-capture:   96.7% (267/276 AST def nodes captured)
//   - Clean def-capture:      93.9% (5088/5419 AST def nodes captured)
//   - Corpus def-capture:     94.0% (5355/5695 AST def nodes captured)
//   - Nav gate:               ≥90% def-capture required — NAV TIER: WIRED
//   - OUTCOME: .rs is registered in grammarTable for nav (outline/refs/deep_refs).
//     Miners remain gated at the <5% parse-fail bar via this sweep test.
//
// # Oracle caveats on the 94.0% def-capture number
//
// The denominator is AST def-node count, not source-level declaration count.
// Two effects deflate both numerator and denominator in related ways:
//
//  1. Error-cascade deflation: when a file's error recovery produces a flat
//     ERROR tree, def nodes that a correct parse would create are absent from
//     the AST entirely. Example: serde/src/private/ser.rs has 1382 lines of
//     real declarations but collapses to ast=4 after error cascade. Both
//     numerator (captures) and denominator (AST defs) collapse together, so
//     the 94.0% rate is locally inflated relative to true source coverage for
//     those files. True source-level capture is lower than 94.0%.
//
//  2. Trait method signatures: the defQuery captures trait_item (the trait
//     name) but NOT individual trait method signatures (fn declarations inside
//     a trait body without a body block). These are function_declaration nodes,
//     not function_item nodes; excluding them omits ~111 definitions across the
//     corpus. Including them would give ≈92.2% — still above the 90% nav gate.
//
// # Root cause of 7.1% parse failures
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
// worker closures). All 14 failing ripgrep files contain it; the 8 failing
// serde_derive files hit it in generated-code paths.
//
// # Options to reduce parse-fail rate below 5% (to enable miners)
//
//  1. Upgrade gotreesitter — monitor v0.20.3+ for a Rust grammar fix covering
//     this pattern. Re-run this sweep test; if <5%, enable miners.
//
//  2. Grammar patch — use the grammargen pipeline (cmd/grammargen/) to build a
//     fixed Rust grammar from tree-sitter-rust HEAD. Requires the Rust+C
//     toolchain to compile grammar.json.

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

// rustDefKinds is the set of AST node types the defQuery captures as
// top-level or nested definition nodes. Used as ground truth for def-capture
// rate measurement; must stay in sync with rustGrammar.defQuery in grammars.go.
var rustDefKinds = map[string]bool{
	"function_item": true,
	"struct_item":   true,
	"enum_item":     true,
	"trait_item":    true,
	"impl_item":     true,
	"mod_item":      true,
	"const_item":    true,
	"static_item":   true,
}

// countRustASTDefs recursively counts all AST nodes whose type is in
// rustDefKinds. This is the ground-truth denominator for def-capture rate:
// it counts what the grammar actually produced, not what the source contains.
func countRustASTDefs(n *gts.Node, lang *gts.Language) int {
	total := 0
	if rustDefKinds[n.Type(lang)] {
		total++
	}
	for i := range int(n.ChildCount()) {
		total += countRustASTDefs(n.Child(i), lang)
	}
	return total
}

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

	// Compile the same defQuery used by rustGrammar in grammars.go.
	const defQuery = `
(function_item name: (identifier) @name) @definition.function
(struct_item name: (type_identifier) @name) @definition.type
(enum_item name: (type_identifier) @name) @definition.type
(trait_item name: (type_identifier) @name) @definition.type
(impl_item type: (type_identifier) @name) @definition.impl
(mod_item name: (identifier) @name) @definition.module
(const_item name: (identifier) @name) @definition.constant
(static_item name: (identifier) @name) @definition.variable
`
	q, err := gts.NewQuery(defQuery, lang)
	if err != nil {
		t.Fatalf("defQuery compile: %v", err)
	}

	dirs := strings.Split(sweepEnv, ":")

	var (
		total, errCount      int
		astDefs, capDefs     int // corpus-wide def-capture counters
		hasErrAst, hasErrCap int // same for HasError files only
	)

	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, we error) error {
			if we != nil || d.IsDir() || filepath.Ext(path) != ".rs" {
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
			root := tree.RootNode()
			hasErr := root.HasError()
			if hasErr {
				errCount++
				t.Logf("HAS_ERROR: %s", path)
			}

			// AST-ground-truth def count: walk the tree for rustDefKinds nodes.
			astN := countRustASTDefs(root, lang)

			// Query capture count: unique @name captures (deduplicated by start byte).
			cursor := q.Exec(root, lang, src)
			seen := map[uint32]bool{}
			capN := 0
			for {
				m, ok := cursor.NextMatch()
				if !ok {
					break
				}
				for _, c := range m.Captures {
					if c.Name == "name" {
						sb := c.Node.StartByte()
						if !seen[sb] {
							seen[sb] = true
							capN++
						}
					}
				}
			}

			astDefs += astN
			capDefs += capN
			if hasErr {
				hasErrAst += astN
				hasErrCap += capN
			}

			tree.Release()
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}

	if total == 0 {
		t.Fatalf("BUGBOT_RS_SWEEP_DIR=%q contained 0 .rs files; check path", sweepEnv)
	}

	parsePct := float64(errCount) / float64(total) * 100
	capPct := 0.0
	if astDefs > 0 {
		capPct = float64(capDefs) / float64(astDefs) * 100
	}
	hasErrCapPct := 0.0
	if hasErrAst > 0 {
		hasErrCapPct = float64(hasErrCap) / float64(hasErrAst) * 100
	}

	fmt.Printf("SWEEP: total=%d has_error=%d (%.1f%%) ast_defs=%d cap_defs=%d def_capture=%.1f%% corpus=%s\n",
		total, errCount, parsePct, astDefs, capDefs, capPct, sweepEnv)

	t.Logf("=== parse-error rate ===")
	t.Logf("total .rs files:       %d", total)
	t.Logf("HasError files:        %d (%.1f%%)", errCount, parsePct)
	t.Logf("miner gate:            <5.0%% — %s", minerGateResult(parsePct))

	t.Logf("=== def-capture rate (AST ground truth) ===")
	t.Logf("  clean files:         ast=%d cap=%d rate=%.1f%%",
		astDefs-hasErrAst, capDefs-hasErrCap,
		safePct(capDefs-hasErrCap, astDefs-hasErrAst))
	t.Logf("  HasError files:      ast=%d cap=%d rate=%.1f%%", hasErrAst, hasErrCap, hasErrCapPct)
	t.Logf("  corpus total:        ast=%d cap=%d rate=%.1f%%", astDefs, capDefs, capPct)
	t.Logf("  nav gate:            >=90.0%% — %s", navGateResult(capPct))
	t.Logf("oracle caveats: (1) error-cascade files deflate denominator (e.g.")
	t.Logf("  serde/src/private/ser.rs: 1382 lines but ast=4 after cascade);")
	t.Logf("  true source-level capture < %.1f%%. (2) trait method signatures", capPct)
	t.Logf("  excluded (~111 across corpus); including them gives ~92.2%%.")

	// Gate: def-capture must meet the nav tier threshold.
	// Parse-fail rate is informational only (miners remain gated at <5% regardless).
	const navGate = 90.0
	if capPct < navGate {
		t.Errorf("def-capture rate %.1f%% is below nav gate %.1f%% — "+
			"grammar should NOT be wired until capture improves", capPct, navGate)
	}
}

func minerGateResult(pct float64) string {
	if pct < 5.0 {
		return "PASS — miners may be enabled"
	}
	return fmt.Sprintf("FAIL — %.1f%% exceeds gate; miners remain gated", pct)
}

func navGateResult(pct float64) string {
	if pct >= 90.0 {
		return fmt.Sprintf("PASS — %.1f%% >= 90%%; nav tier wired", pct)
	}
	return fmt.Sprintf("FAIL — %.1f%% < 90%%; nav tier should NOT be wired", pct)
}

func safePct(num, den int) float64 {
	if den == 0 {
		return 100
	}
	return float64(num) / float64(den) * 100
}
