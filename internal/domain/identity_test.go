package domain

import "testing"

func TestDefectKindValid(t *testing.T) {
	for _, k := range AllDefectKinds {
		if !k.Valid() {
			t.Errorf("AllDefectKinds member %q reported invalid", k)
		}
	}
	if DefectKind("use-after-free").Valid() {
		t.Error("an out-of-enum kind must not validate")
	}
	if DefectKind("").Valid() {
		t.Error("the empty kind must not validate")
	}
}

func TestNormalizeSubject(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare", "ServeHTTP", "servehttp"},
		{"pointer receiver", "(*Handler).ServeHTTP", "servehttp"},
		{"value receiver", "(Handler).ServeHTTP", "servehttp"},
		{"package qualified", "pkg.Foo", "foo"},
		{"type dot method", "Foo.Bar", "bar"},
		{"whitespace", "  Foo.Bar  ", "bar"},
		{"already lowercase", "foo", "foo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSubject(c.in); got != c.want {
				t.Errorf("NormalizeSubject(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestFingerprintV3_ExcludesLens pins the core design property: two
// candidates from different lenses at the same locus, defect_kind, and
// subject mint the IDENTICAL v3 fingerprint. This is what lets cross-lens
// duplicates converge at triage's ordinary exact-fingerprint dedup step
// without ever consulting description-Jaccard similarity.
func TestFingerprintV3_ExcludesLens(t *testing.T) {
	a := FingerprintV3("pkg/bug.go", "S:function\x00Greeting", DefectNilDeref, "Greeting")
	b := FingerprintV3("pkg/bug.go", "S:function\x00Greeting", DefectNilDeref, "Greeting")
	if a != b {
		t.Fatalf("identical (file, locus, kind, subject) must mint identical fingerprints regardless of lens: %q != %q", a, b)
	}
}

// TestFingerprintV3_KindDisambiguates pins the other core property: two
// distinct defect_kinds at the SAME locus and subject must NOT collide, even
// though everything else about the identity tuple matches.
func TestFingerprintV3_KindDisambiguates(t *testing.T) {
	nilDeref := FingerprintV3("pkg/bug.go", "S:function\x00Greeting", DefectNilDeref, "Greeting")
	leak := FingerprintV3("pkg/bug.go", "S:function\x00Greeting", DefectResourceLeak, "Greeting")
	if nilDeref == leak {
		t.Fatal("two distinct defect_kinds at the same locus/subject must not share a fingerprint")
	}
}

// TestFingerprintV3_SubjectNormalized proves subject normalization is applied
// inside FingerprintV3 itself, not left to callers: differently-phrased
// subjects naming the same symbol converge.
func TestFingerprintV3_SubjectNormalized(t *testing.T) {
	a := FingerprintV3("pkg/bug.go", "S:method\x00ServeHTTP", DefectNilDeref, "(*Handler).ServeHTTP")
	b := FingerprintV3("pkg/bug.go", "S:method\x00ServeHTTP", DefectNilDeref, "ServeHTTP")
	if a != b {
		t.Fatalf("differently-qualified subjects naming the same symbol must converge: %q != %q", a, b)
	}
}

// TestFingerprintV3_FileCaseAndSeparatorInsensitive mirrors the v2 Fingerprint
// contract (same normalization for file paths).
func TestFingerprintV3_FileCaseAndSeparatorInsensitive(t *testing.T) {
	a := FingerprintV3("Pkg/Bug.go", "L:10", DefectLogic, "Foo")
	b := FingerprintV3("pkg\\bug.go", "L:10", DefectLogic, "Foo")
	if a != b {
		t.Fatalf("file path case/separator must not affect identity: %q != %q", a, b)
	}
}
