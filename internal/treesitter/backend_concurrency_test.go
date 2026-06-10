package treesitter

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gts "github.com/odvcencio/gotreesitter"
)

// TestConcurrentDefinitionReferencesRace hammers a single Backend with many
// concurrent Definition and References calls. A *gts.Tagger is not safe for
// concurrent use (shared matchesBuf, mutable Parser state), so without the
// per-tagger mutex this reproduces a write/write data race on the Parser. Run
// under `go test -race` to verify the serialization fix.
func TestConcurrentDefinitionReferencesRace(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"a/svc.go":  "package a\n\nfunc New() int { return 1 }\n\nfunc Use() int { return New() }\n",
		"b/here.go": "package b\n\nfunc New() int { return 2 }\n\nfunc Caller() { New(); New() }\n",
		"c/more.go": "package c\n\ntype T struct{}\n\nfunc (t T) New() int { return 3 }\n",
		"app.py":    "class Foo:\n    def bar(self):\n        return 1\n\ndef New():\n    return Foo().bar()\n",
		"app.ts":    "function New(){ return 1 }\nclass C { greet(){ return New() } }\n",
	})
	b := New(root)
	t.Cleanup(func() { _ = b.Close() })

	goDef := filepath.Join(root, "a/svc.go")
	pyDef := filepath.Join(root, "app.py")
	tsDef := filepath.Join(root, "app.ts")

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			switch i % 5 {
			case 0:
				_, _ = b.Definition(goDef, "New")
			case 1:
				_, _ = b.References(goDef, "New")
			case 2:
				_, _ = b.Definition(pyDef, "New")
			case 3:
				_, _ = b.References(tsDef, "New")
			default:
				_, _ = b.Definition(tsDef, "New")
			}
		}(i)
	}
	wg.Wait()

	// Sanity: a definition query still resolves after the concurrent storm.
	res, err := b.Definition(goDef, "New")
	if err != nil {
		t.Fatalf("Definition after concurrency: %v", err)
	}
	if res.Candidates == 0 {
		t.Fatalf("expected at least one New definition, got none")
	}
}

// TestTagRecoversFromPanic verifies the parse path drops a file whose tagger
// panics (the GLR safety caps can panic on pathological input) and keeps
// collecting the rest, rather than crashing the agent. It injects a panicking
// tagFn for one file's content and a real one otherwise.
func TestTagRecoversFromPanic(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"good.go":  "package main\n\nfunc Keep() int { return 1 }\n",
		"boom.go":  "package main\n\nfunc Boom() int { return 2 }\n",
		"other.go": "package main\n\nfunc Also() int { return 3 }\n",
	})
	b := New(root)
	t.Cleanup(func() { _ = b.Close() })

	boom := filepath.Join(root, "boom.go")
	boomSrc, err := os.ReadFile(boom)
	if err != nil {
		t.Fatal(err)
	}

	var dropped atomic.Int32
	b.tagFn = func(tg *gts.Tagger, src []byte) []gts.Tag {
		if string(src) == string(boomSrc) {
			dropped.Add(1)
			panic("simulated GLR safety-cap panic")
		}
		return tg.Tag(src)
	}

	// Definition over the whole repo must not panic; the good files still
	// resolve and boom.go is silently dropped.
	res, err := b.Definition(filepath.Join(root, "good.go"), "Keep")
	if err != nil {
		t.Fatalf("Definition with panicking file: %v", err)
	}
	if got := locLines(t, root, res); !contains(got, "good.go:3") {
		t.Errorf("Keep def = %v, want good.go:3", got)
	}
	// Boom is never reported as a definition.
	res2, err := b.Definition(boom, "Boom")
	if err != nil {
		t.Fatalf("Definition(Boom): %v", err)
	}
	if got := locLines(t, root, res2); contains(got, "boom.go:3") {
		t.Errorf("panicking file must yield no tags, got %v", got)
	}
	if dropped.Load() == 0 {
		t.Errorf("expected the panicking file to be visited at least once")
	}
}

// TestTagFileCacheReuse proves the per-file tag cache serves unchanged files
// without re-tagging on the second query, and re-tags a file whose mtime
// changes. A counting tagFn records how many times each file is actually
// parsed.
func TestTagFileCacheReuse(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"a.go": "package main\n\nfunc Alpha() int { return 1 }\n",
		"b.go": "package main\n\nfunc Beta() int { return Alpha() }\n",
	})
	b := New(root)
	t.Cleanup(func() { _ = b.Close() })

	var mu sync.Mutex
	parses := map[string]int{}
	b.tagFn = func(tg *gts.Tagger, src []byte) []gts.Tag {
		mu.Lock()
		// Identify the file by a stable marker in its source.
		switch {
		case containsSub(string(src), "Alpha() int"):
			parses["a.go"]++
		case containsSub(string(src), "Beta()"):
			parses["b.go"]++
		}
		mu.Unlock()
		return tg.Tag(src)
	}

	// First definition query: both files get parsed once (def query).
	if _, err := b.Definition(filepath.Join(root, "a.go"), "Alpha"); err != nil {
		t.Fatal(err)
	}
	if parses["a.go"] != 1 || parses["b.go"] != 1 {
		t.Fatalf("after first query parses=%v, want each file once", parses)
	}

	// Second identical query: served entirely from cache, no new parses.
	if _, err := b.Definition(filepath.Join(root, "a.go"), "Alpha"); err != nil {
		t.Fatal(err)
	}
	if parses["a.go"] != 1 || parses["b.go"] != 1 {
		t.Fatalf("second query re-parsed files: parses=%v, want unchanged", parses)
	}

	// Touch a.go so its mtime advances; it must be re-tagged, b.go must not.
	aPath := filepath.Join(root, "a.go")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(aPath, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Definition(aPath, "Alpha"); err != nil {
		t.Fatal(err)
	}
	if parses["a.go"] != 2 {
		t.Errorf("touched file a.go re-parses=%d, want 2", parses["a.go"])
	}
	if parses["b.go"] != 1 {
		t.Errorf("untouched file b.go re-parsed: %d, want 1", parses["b.go"])
	}
}

// containsSub is a tiny substring helper kept local to the test to avoid
// importing strings just for this.
func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
