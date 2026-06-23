package funnel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

const locusFixture = `package p

func Foo() {
	x := 1
	_ = x
	y := x + 1
	_ = y
}

func Bar() {
	z := 2
	_ = z
}
`

// TestLocusResolver_SymbolAnchoredAndLineStable is the core of the duplicate fix:
// two different lines inside the same function resolve to the same locus, so the
// finding fingerprint is stable when code above the bug shifts its line. A line
// in a different function resolves to a different locus.
func TestLocusResolver_SymbolAnchoredAndLineStable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(locusFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := NewLocusResolver(dir)

	// Foo spans lines 3-8; lines 4 and 6 are both inside it.
	l4 := lr.Resolve("p.go", 4)
	l6 := lr.Resolve("p.go", 6)
	if l4 != l6 {
		t.Fatalf("lines in the same function must share a locus: %q vs %q", l4, l6)
	}
	if l4 == "L:4" || l4 == "L:6" {
		t.Fatalf("expected a symbol locus for a line inside Foo, got line fallback %q", l4)
	}

	// Bar spans lines 10-13; line 11 is inside it and must be a distinct locus.
	if lBar := lr.Resolve("p.go", 11); lBar == l4 {
		t.Fatalf("distinct functions must have distinct loci: both %q", l4)
	}

	// The fingerprint is stable across the line drift (the headline fix); the
	// model-authored title plays no part in identity at all.
	fpA := store.Fingerprint("nil-safety/error-handling", "p.go", l4)
	fpB := store.Fingerprint("nil-safety/error-handling", "p.go", l6)
	if fpA != fpB {
		t.Errorf("fingerprint must be stable across line drift within a symbol")
	}
}

// TestLocusResolver_FallsBackToLine covers the safe-degradation paths: no root,
// a missing file, and an unsupported extension all yield the line fallback.
func TestLocusResolver_FallsBackToLine(t *testing.T) {
	if got := NewLocusResolver("").Resolve("p.go", 10); got != "L:10" {
		t.Errorf("empty-root resolve = %q, want L:10", got)
	}

	dir := t.TempDir()
	lr := NewLocusResolver(dir)
	if got := lr.Resolve("does_not_exist.go", 5); got != "L:5" {
		t.Errorf("missing-file resolve = %q, want L:5", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lr.Resolve("data.txt", 1); got != "L:1" {
		t.Errorf("unsupported-extension resolve = %q, want L:1", got)
	}
}
