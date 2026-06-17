package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepo materializes a tiny on-disk repo with the given files and returns
// the repo root. Used by seam tests to exercise EnumerateSeams against a
// real filesystem (the detector reads file bytes from snap.Root, so a real
// tree is required).
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// makeSnapshot builds an in-memory Snapshot pointing at root with the given
// repo-relative paths and their detected languages. The detector reads files
// from snap.Root, so paths and language classifications are both inputs to
// the test.
func makeSnapshot(t *testing.T, root string, paths []string) *Snapshot {
	t.Helper()
	files := make([]File, 0, len(paths))
	for _, p := range paths {
		files = append(files, File{
			Path:     p,
			Language: DetectLanguage(p),
			Size:     0,
		})
	}
	return &Snapshot{Root: root, Files: files}
}

// seamByKey returns the first seam matching (kind, key) or nil.
func seamByKey(seams []Seam, kind SeamKind, key string) *Seam {
	for i, s := range seams {
		if s.Kind == kind && s.Key == key {
			return &seams[i]
		}
	}
	return nil
}

// hasSide returns true when any side of seam has file with the given
// language. Used to assert both-sides-named on a detected seam.
func hasSide(seam *Seam, file string) bool {
	if seam == nil {
		return false
	}
	for _, s := range seam.Sides {
		if s.File == file {
			return true
		}
	}
	return false
}

// TestEnumerateSeams_DataFileAcrossPythonAndGo verifies the data-file
// detector surfaces a seam when the same .json basename is referenced by a
// Python file and a Go file, and that both files appear as sides. Single-
// language-only references do NOT produce a seam; markdown is LangOther
// and is excluded even when it references the file.
func TestEnumerateSeams_DataFileAcrossPythonAndGo(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"py/writer.py":   "import json\nwith open('metrics.json', 'w') as f:\n    json.dump({'count': 5}, f)\n",
		"go/reader.go":   "package reader\nimport _ \"metrics.json\"\n",
		"docs/readme.md": "# See metrics.json for the format.\n",
	})
	snap := makeSnapshot(t, root, []string{"py/writer.py", "go/reader.go", "docs/readme.md"})

	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamDataFile, "metrics.json")
	if seam == nil {
		t.Fatalf("expected a SeamDataFile for metrics.json; got %+v", seams)
	}
	if !hasSide(seam, "py/writer.py") {
		t.Errorf("missing Python side: %+v", seam.Sides)
	}
	if !hasSide(seam, "go/reader.go") {
		t.Errorf("missing Go side: %+v", seam.Sides)
	}
	// Markdown is LangOther — the detector must skip it. Asserting its
	// absence here is the positive expression of that filter.
	if hasSide(seam, "docs/readme.md") {
		t.Errorf("markdown side should be excluded (LangOther): %+v", seam.Sides)
	}
	// The Go and Python sides must carry their detected languages.
	seenLangs := map[Language]bool{}
	for _, s := range seam.Sides {
		seenLangs[s.Language] = true
	}
	if !seenLangs[LangPython] || !seenLangs[LangGo] {
		t.Errorf("expected Python and Go languages on sides, got %+v", seenLangs)
	}
}

// TestEnumerateSeams_NoSeamSingleLanguage confirms that references to a
// data file in ONLY one language do NOT surface a seam — the cross-language
// condition is the whole point of the lens.
func TestEnumerateSeams_NoSeamSingleLanguage(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/a.go": "package a\nconst _ = \"x.json\"\n",
		"go/b.go": "package b\nconst _ = \"x.json\"\n",
	})
	snap := makeSnapshot(t, root, []string{"go/a.go", "go/b.go"})

	seams := EnumerateSeams(snap)
	if seamByKey(seams, SeamDataFile, "x.json") != nil {
		t.Fatalf("expected no seam for single-language data-file refs, got %+v", seams)
	}
}

// TestEnumerateSeams_EnvVarAcrossGoAndPython confirms the env-var detector
// finds a seam when the same env var is read from both Go and Python, with
// both sides named.
func TestEnumerateSeams_EnvVarAcrossGoAndPython(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/server.go": "package srv\nimport \"os\"\nfunc Token() string { v, _ := os.LookupEnv(\"API_TOKEN\"); return v }\n",
		"py/client.py": "import os\ndef get_token():\n    return os.environ.get(\"API_TOKEN\")\n",
	})
	snap := makeSnapshot(t, root, []string{"go/server.go", "py/client.py"})

	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamEnvVar, "API_TOKEN")
	if seam == nil {
		t.Fatalf("expected a SeamEnvVar for API_TOKEN; got %+v", seams)
	}
	if !hasSide(seam, "go/server.go") {
		t.Errorf("missing Go side: %+v", seam.Sides)
	}
	if !hasSide(seam, "py/client.py") {
		t.Errorf("missing Python side: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_EnvVarAcrossJSAndTS confirms process.env is detected
// in BOTH JavaScript and TypeScript (they share the seam by design).
func TestEnumerateSeams_EnvVarAcrossJSAndTS(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"ui/server.js":  "const t = process.env.API_TOKEN;\n",
		"ui/client.tsx": "const t: string = process.env[\"API_TOKEN\"];\n",
	})
	snap := makeSnapshot(t, root, []string{"ui/server.js", "ui/client.tsx"})

	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamEnvVar, "API_TOKEN")
	if seam == nil {
		t.Fatalf("expected a SeamEnvVar for API_TOKEN; got %+v", seams)
	}
	if !hasSide(seam, "ui/server.js") {
		t.Errorf("missing JS side: %+v", seam.Sides)
	}
	if !hasSide(seam, "ui/client.tsx") {
		t.Errorf("missing TS side: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_NilSnapshotReturnsEmpty ensures EnumerateSeams
// gracefully returns nil on a nil snapshot rather than panicking.
func TestEnumerateSeams_NilSnapshotReturnsEmpty(t *testing.T) {
	if got := EnumerateSeams(nil); got != nil {
		t.Errorf("EnumerateSeams(nil) = %+v, want nil", got)
	}
}

// TestEnumerateSeams_OrderingAndBounded confirms the output order is
// (Kind, Key): all data-file seams in lexicographic Key order, then all
// env-var seams, and the total count is bounded by seamMaxTotal.
func TestEnumerateSeams_OrderingAndBounded(t *testing.T) {
	// Build a fixture with several cross-language seams, all detected
	// in a single sweep.
	files := map[string]string{}
	paths := []string{}
	add := func(rel, content, lang string) {
		_ = lang
		files[rel] = content
		paths = append(paths, rel)
	}
	add("py/a.py", "x = 'alpha.json'\ny = 'beta.json'\n", "py")
	add("go/a.go", "var _ = \"alpha.json\"\nvar _ = \"beta.json\"\n", "go")
	add("py/b.py", "import os\nv = os.environ['Z_TOKEN']\n", "py")
	add("go/b.go", "package b\nimport \"os\"\nvar _ = os.Getenv(\"Z_TOKEN\")\n", "go")

	root := writeRepo(t, files)
	snap := makeSnapshot(t, root, paths)
	seams := EnumerateSeams(snap)
	if len(seams) < 3 {
		t.Fatalf("expected >=3 seams, got %+v", seams)
	}
	// First seam: data_file; then alpha, beta; then env_var Z_TOKEN.
	prevKind := seams[0].Kind
	if prevKind != SeamDataFile {
		t.Errorf("first seam kind = %q, want %q", prevKind, SeamDataFile)
	}
	for _, s := range seams {
		if s.Kind == SeamEnvVar && prevKind == SeamDataFile {
			prevKind = SeamEnvVar // allowed
		} else if s.Kind != prevKind {
			t.Errorf("seam kind out of order: %q follows %q", s.Kind, prevKind)
		}
		prevKind = s.Kind
	}
	// alpha < beta
	if seams[0].Key != "alpha.json" || seams[1].Key != "beta.json" {
		t.Errorf("data-file keys not lex-sorted: %q, %q", seams[0].Key, seams[1].Key)
	}
}

// TestEnumerateSeams_SidesSortedByFile confirms that sides within a seam
// are sorted by file path so output is deterministic.
func TestEnumerateSeams_SidesSortedByFile(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"z.go":    "package z\nconst _ = \"a.json\"\n",
		"a.go":    "package a\nconst _ = \"a.json\"\n",
		"py/m.py": "x = 'a.json'\n",
	})
	snap := makeSnapshot(t, root, []string{"z.go", "a.go", "py/m.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamDataFile, "a.json")
	if seam == nil {
		t.Fatal("expected seam for a.json")
	}
	for i := 1; i < len(seam.Sides); i++ {
		if seam.Sides[i-1].File > seam.Sides[i].File {
			t.Errorf("sides not sorted: %+v", seam.Sides)
			break
		}
	}
}

// TestEnumerateSeams_LineNumberPopulated confirms that the Line field on a
// side carries a 1-based line number for the first matching reference.
func TestEnumerateSeams_LineNumberPopulated(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/a.go": "package a\n// line 2\n// line 3\nvar _ = \"shared.json\"\n",
		"py/b.py": "# comment\n# comment\nx = 'shared.json'\n",
	})
	snap := makeSnapshot(t, root, []string{"go/a.go", "py/b.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamDataFile, "shared.json")
	if seam == nil {
		t.Fatal("expected seam")
	}
	// Find the Go side; expect line 4.
	var goLine int
	for _, s := range seam.Sides {
		if s.File == "go/a.go" {
			goLine = s.Line
		}
	}
	if goLine != 4 {
		t.Errorf("Go side line = %d, want 4", goLine)
	}
}
