package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMeasureReadSymbolSavings is the offline, deterministic token-savings
// ESTIMATE for read_symbol (bugbot-6pu). For every function/method/type in a
// representative Go corpus it compares two costs, both using the same bytes/4
// token heuristic the Runner's history accounting uses:
//
//   - whole-file: what a finder pays today when read_file pulls the entire file
//     to inspect one declaration (bounded by the finder read cap of maxReadLines
//     lines; large files are clamped to that window, mirroring production);
//   - read_symbol: what the new tool returns instead — only that declaration's
//     numbered body.
//
// It prints the corpus-wide averages and the percentage saved, and asserts only
// a weak, honest floor (read_symbol is not LARGER on average): the headline
// number is an estimate, not a guarantee, in keeping with the bugbot-3nf
// measurement-honesty lesson. Real savings depend on how often a finder reads a
// whole file solely to see one declaration; this isolates the per-lookup delta.
func TestMeasureReadSymbolSavings(t *testing.T) {
	// A small corpus spanning typical declaration sizes in this repo: a tiny
	// helper, a medium method, and a large function, each inside files that also
	// hold other code (so a whole-file read overshoots).
	files := map[string]string{
		"small.go": "package corpus\n\n" +
			"func add(a, b int) int { return a + b }\n\n" +
			padFunc("filler1", 30),
		"medium.go": "package corpus\n\n" +
			"type Server struct {\n\taddr string\n\tport int\n}\n\n" +
			"func (s *Server) Handle(req int) (int, error) {\n" +
			"\tif req < 0 {\n\t\treturn 0, fmt.Errorf(\"bad\")\n\t}\n" +
			"\tn := req * 2\n\tn += s.port\n\treturn n, nil\n}\n\n" +
			padFunc("filler2", 80),
		"large.go": "package corpus\n\n" +
			"func Process(items []int) int {\n" +
			strings.Repeat("\t_ = 1\n", 60) +
			"\treturn len(items)\n}\n\n" +
			padFunc("filler3", 150),
	}
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c, err := NewCodeNav(dir)
	if err != nil {
		t.Fatalf("NewCodeNav: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	tool := toolByName(t, c, "read_symbol")

	// The finder's whole-file read cap (production tightens read_file for finders;
	// we use the package default line cap as the clamp, which is the upper bound
	// on what a single read_file result costs).
	finderCaps := ReadCaps{}.resolve()

	type probe struct {
		file, symbol string
		line         int
	}
	probes := []probe{
		{"small.go", "add", 3},
		{"medium.go", "Handle", 8},
		{"medium.go", "Server", 3},
		{"large.go", "Process", 3},
	}

	var totalWhole, totalSymbol int64
	for _, p := range probes {
		whole := wholeFileReadTokens(t, filepath.Join(dir, p.file), finderCaps)
		out, err := runTool(t, tool, codeNavArgs{File: p.file, Line: p.line, Symbol: p.symbol})
		if err != nil {
			t.Fatalf("read_symbol(%s): %v", p.symbol, err)
		}
		sym := int64(len(out)) / 4 // same bytes/4 heuristic as estimateTokens.
		t.Logf("ESTIMATE %-18s whole-file=%4d tok  read_symbol=%4d tok", p.file+":"+p.symbol, whole, sym)
		totalWhole += whole
		totalSymbol += sym
	}

	avgWhole := float64(totalWhole) / float64(len(probes))
	avgSym := float64(totalSymbol) / float64(len(probes))
	pct := 0.0
	if avgWhole > 0 {
		pct = (1 - avgSym/avgWhole) * 100
	}
	t.Logf("ESTIMATE corpus avg: whole-file=%.0f tok  read_symbol=%.0f tok  saved=%.0f%% (estimate; per-lookup, not run-wide)",
		avgWhole, avgSym, pct)

	// Honest floor only: read_symbol must not be LARGER on average than reading
	// the whole file for a single declaration. The headline % is informational.
	if avgSym > avgWhole {
		t.Errorf("read_symbol avg %.0f exceeds whole-file avg %.0f — the tool should never cost MORE per lookup", avgSym, avgWhole)
	}
}

// wholeFileReadTokens returns the bytes/4 token cost of a read_file result over
// the whole file under the given caps — what a finder pays today to inspect one
// declaration by reading the file.
func wholeFileReadTokens(t *testing.T, abs string, caps ReadCaps) int64 {
	t.Helper()
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderNumbered(string(data), 0, 0, false, caps)
	return int64(len(rendered)) / 4
}

// padFunc returns a Go function of roughly n body lines, used to make corpus
// files larger than the single declaration under test so a whole-file read
// overshoots.
func padFunc(name string, n int) string {
	return fmt.Sprintf("func %s() {\n%s}\n", name, strings.Repeat("\t_ = 1\n", n))
}
