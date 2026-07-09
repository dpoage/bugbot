package funnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
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
	fpA := domain.Fingerprint("nil-safety/error-handling", "p.go", l4)
	fpB := domain.Fingerprint("nil-safety/error-handling", "p.go", l6)
	if fpA != fpB {
		t.Errorf("fingerprint must be stable across line drift within a symbol")
	}
}

// TestLocusResolver_SymbolLocusFormatUnchangedByContentAnchor pins the exact
// byte-for-byte shape of the symbol-anchored ("S:") locus (bugbot-ezmx.1's
// format) to prove bugbot-ezmx.5's content anchor is purely an ADDITIONAL
// fallback tier and never touches the primary symbol path.
func TestLocusResolver_SymbolLocusFormatUnchangedByContentAnchor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(locusFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := NewLocusResolver(dir)

	const want = "S:definition.function\x00Foo"
	if got := lr.Resolve("p.go", 4); got != want {
		t.Fatalf("symbol locus format = %q, want %q", got, want)
	}
}

// TestLocusResolver_FallsBackToLine covers the safe-degradation paths: no root
// and a missing file both yield the bare line fallback (there is no source
// text to anchor to). An unsupported extension still HAS a readable file, so
// it degrades one step less: past the symbol anchor to the content anchor,
// not all the way to the line.
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
	got := lr.Resolve("data.txt", 1)
	if !strings.HasPrefix(got, "C:") {
		t.Errorf("unsupported-extension resolve = %q, want a content anchor (C:...)", got)
	}
	if got2 := lr.Resolve("data.txt", 1); got2 != got {
		t.Errorf("content anchor must be deterministic: %q vs %q", got, got2)
	}
}

// TestLocusResolver_ContentAnchorStableUnderLineDrift is the headline fix for
// bugbot-ezmx.5: non-declaration package-level text (here a comment; Go has
// no bare statements at package scope, so a comment is the realistic
// "no enclosing symbol" case) anchors to the implicated line's own
// normalized text, so inserting unrelated lines ABOVE it — which shifts its
// line number but not its content — leaves the locus, and therefore the
// fingerprint, unchanged.
func TestLocusResolver_ContentAnchorStableUnderLineDrift(t *testing.T) {
	dir := t.TempDir()
	const before = "package p\n\n// TODO: wire this up before release\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := NewLocusResolver(dir)
	locusBefore := lr.Resolve("p.go", 3)
	if strings.HasPrefix(locusBefore, "S:") || strings.HasPrefix(locusBefore, "L:") {
		t.Fatalf("expected a content anchor for non-declaration package-level text, got %q", locusBefore)
	}

	// Insert two unrelated lines above the target; its line number shifts
	// from 3 to 5, but its own text is untouched.
	const after = "package p\n\nimport \"fmt\"\n\n// TODO: wire this up before release\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	locusAfter := lr.Resolve("p.go", 5)
	if locusAfter != locusBefore {
		t.Fatalf("content anchor must be stable under drift above the line: before %q, after %q", locusBefore, locusAfter)
	}

	// The fingerprint built from it must also be stable, matching the
	// symbol-anchored case's guarantee.
	fpBefore := domain.Fingerprint("nil-safety/error-handling", "p.go", locusBefore)
	fpAfter := domain.Fingerprint("nil-safety/error-handling", "p.go", locusAfter)
	if fpBefore != fpAfter {
		t.Errorf("fingerprint must be stable across line drift for a content-anchored locus")
	}
}

// TestLocusResolver_ContentAnchorDuplicateLineTieBreak pins the tie-break for
// bugbot-ezmx.5's degenerate case: two lines in the same file that are
// byte-for-byte identical after normalization (e.g. two unrelated `return
// err` statements at package level, modeled here with a repeated comment)
// must NOT collapse onto the same anchor — that would fold two distinct
// defects into one fingerprint. The ordinal (nth occurrence of that exact
// normalized text, counting from the top of the file) disambiguates them.
func TestLocusResolver_ContentAnchorDuplicateLineTieBreak(t *testing.T) {
	dir := t.TempDir()
	const src = "package p\n\n// unreachable\nfunc other() {}\n// unreachable\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := NewLocusResolver(dir)

	first := lr.Resolve("p.go", 3)
	second := lr.Resolve("p.go", 5)
	if first == second {
		t.Fatalf("identical lines at different positions must not collide: both resolved to %q", first)
	}
	if !strings.HasSuffix(first, "#1") {
		t.Errorf("first occurrence ordinal = %q, want suffix #1", first)
	}
	if !strings.HasSuffix(second, "#2") {
		t.Errorf("second occurrence ordinal = %q, want suffix #2", second)
	}
	// Same content up to the "#" must match: both anchor the same hash, only
	// the ordinal differs.
	if first[:strings.LastIndex(first, "#")] != second[:strings.LastIndex(second, "#")] {
		t.Errorf("duplicate lines must share the same content hash: %q vs %q", first, second)
	}
}

// TestLocusResolver_ContentAnchorBlankLineFallsBackToLine covers the other
// degenerate case: a blank or whitespace-only implicated line carries no
// distinguishing content, so hashing it would collide every blank line in
// every file. The resolver falls all the way back to the line number instead
// of minting a content anchor for it.
func TestLocusResolver_ContentAnchorBlankLineFallsBackToLine(t *testing.T) {
	dir := t.TempDir()
	const src = "package p\n\n   \nvar x = 1\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := NewLocusResolver(dir)
	if got := lr.Resolve("p.go", 3); got != "L:3" {
		t.Errorf("blank-line resolve = %q, want L:3", got)
	}
}
