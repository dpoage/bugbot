// Package treesitter is a syntactic code-navigation fallback used when no
// language server can answer (the server binary is missing, crashed, or does
// not support the query). It parses source directly with per-language
// tree-sitter grammars and tags-style definition/reference queries — the same
// mechanism behind GitHub's basic ("search-based") code navigation.
//
// The honest limit is that this tier is syntactic, not semantic: it has no
// cross-file type resolution, so it cannot disambiguate two same-named symbols
// the way an LSP server can. find_definition therefore returns *ranked
// candidates* for an ambiguous name (exact-name match among definitions, ranked
// same-file-first then by path proximity), and the caller surfaces a one-line
// caveat saying the match is syntactic. find_references is more reliable than
// grep because it matches AST nodes: a name appearing only in a comment or
// string literal is never reported as a reference.
//
// The runtime is github.com/odvcencio/gotreesitter, a pure-Go tree-sitter
// implementation with no cgo and no C toolchain, so it builds under
// CGO_ENABLED=0 (the project's static-binary constraint that rules out the
// mainstream cgo bindings).
package treesitter

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	gts "github.com/odvcencio/gotreesitter"

	"github.com/dpoage/bugbot/internal/lsp"
)

const (
	// maxFileBytes skips files larger than this when scanning the repository
	// (likely data/binaries); matches the grep and code-nav ceilings.
	maxFileBytes = 5 * 1024 * 1024
	// maxCandidates caps how many definition candidates a single ambiguous-name
	// query reports, so a name like "New" cannot flood the result.
	maxCandidates = 50
	// maxReferences caps reference results for one query.
	maxReferences = 200
)

// Backend answers code-navigation queries for one repository root by parsing
// source files with tree-sitter. It is safe for concurrent use; parsed taggers
// are cached per language for the backend's lifetime.
type Backend struct {
	root string

	mu      sync.Mutex
	taggers map[string]*langTaggers // keyed by grammar name
}

// langTaggers holds the compiled definition and reference taggers for one
// language. Compilation (which loads the grammar) is done once per language.
type langTaggers struct {
	def *gts.Tagger
	ref *gts.Tagger
}

// New creates a tree-sitter backend rooted at the absolute path root. No
// grammars are loaded until the first query for a language.
func New(root string) *Backend {
	return &Backend{root: root, taggers: make(map[string]*langTaggers)}
}

// Close releases any resources. The pure-Go runtime holds no OS handles, so
// this is a no-op kept for symmetry with the LSP manager's lifecycle.
func (b *Backend) Close() error { return nil }

// Supports reports whether this tier has a grammar for the file's extension.
// Callers use it to give a precise "no grammar for .X files" degradation
// message instead of a generic miss.
func (b *Backend) Supports(path string) bool {
	return grammarForExt(strings.ToLower(filepath.Ext(path))) != nil
}

// Result is one tier query's answer: the located positions plus the count of
// distinct candidates considered (so the caller can render a "N candidates"
// caveat). Ambiguous reports whether more than one definition matched the name.
type Result struct {
	Locations  []lsp.Location
	Candidates int
	Ambiguous  bool
}

// tag is one extracted symbol occurrence with its absolute file path.
type tag struct {
	path string
	gts.Tag
}

// Definition returns ranked definition candidates for the symbol named symbol,
// whose query occurrence is in absPath. Because the tier is syntactic, it
// matches every definition of that exact name across the language's files and
// ranks them: same file first, then by path proximity to absPath.
func (b *Backend) Definition(absPath, symbol string) (Result, error) {
	g := grammarForExt(strings.ToLower(filepath.Ext(absPath)))
	if g == nil {
		return Result{}, nil
	}
	defs, err := b.collect(g, queryDef)
	if err != nil {
		return Result{}, err
	}

	var matches []tag
	for _, d := range defs {
		if d.Name == symbol {
			matches = append(matches, d)
		}
	}
	rankByProximity(matches, absPath)

	if len(matches) > maxCandidates {
		matches = matches[:maxCandidates]
	}
	return Result{
		Locations:  toLocations(matches),
		Candidates: len(matches),
		Ambiguous:  len(matches) > 1,
	}, nil
}

// References returns every call site / member-access reference to symbol across
// the language's files. Unlike grep, comment and string-literal mentions are
// never reported, because the query matches AST call nodes.
func (b *Backend) References(absPath, symbol string) (Result, error) {
	g := grammarForExt(strings.ToLower(filepath.Ext(absPath)))
	if g == nil {
		return Result{}, nil
	}
	refs, err := b.collect(g, queryRef)
	if err != nil {
		return Result{}, err
	}

	var matches []tag
	for _, r := range refs {
		if r.Name == symbol {
			matches = append(matches, r)
		}
	}
	rankByProximity(matches, absPath)
	if len(matches) > maxReferences {
		matches = matches[:maxReferences]
	}
	return Result{Locations: toLocations(matches), Candidates: len(matches)}, nil
}

// queryKind selects which tags query (definitions or references) collect runs.
type queryKind int

const (
	queryDef queryKind = iota
	queryRef
)

// collect parses every file of the grammar's language under the root and
// returns the tags produced by the selected query. Files that fail to parse
// are skipped (a syntactic tier degrades per-file, never aborts).
func (b *Backend) collect(g *grammar, kind queryKind) ([]tag, error) {
	tg, err := b.taggerFor(g, kind)
	if err != nil {
		return nil, err
	}
	exts := extsForGrammar(g)

	var out []tag
	_ = filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Never follow symlinks out of the root, matching the grep/code-nav
		// sandbox contract.
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		src, ok := readSource(path)
		if !ok {
			return nil
		}
		for _, t := range tg.Tag(src) {
			out = append(out, tag{path: path, Tag: t})
		}
		return nil
	})
	return out, nil
}

// taggerFor compiles (once) and returns the tagger for the grammar and query
// kind. Grammar loading happens on first use of a language.
func (b *Backend) taggerFor(g *grammar, kind queryKind) (*gts.Tagger, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	lt := b.taggers[g.name]
	if lt == nil {
		lt = &langTaggers{}
		b.taggers[g.name] = lt
	}
	switch kind {
	case queryDef:
		if lt.def == nil {
			t, err := newTagger(g, g.defQuery)
			if err != nil {
				return nil, err
			}
			lt.def = t
		}
		return lt.def, nil
	default:
		if lt.ref == nil {
			t, err := newTagger(g, g.refQuery)
			if err != nil {
				return nil, err
			}
			lt.ref = t
		}
		return lt.ref, nil
	}
}

// readSource loads a file for parsing, skipping oversized and binary content
// (a NUL byte in the head marks it binary).
func readSource(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxFileBytes {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, false
	}
	return data, true
}

// skipDir reports whether a directory should be pruned from the walk. We skip
// dot-directories and conventional vendored/dependency trees so the tier does
// not surface candidates from third-party code.
func skipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", "testdata":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

// toLocations converts ranked tags to LSP locations pointing at each symbol's
// name range.
func toLocations(tags []tag) []lsp.Location {
	out := make([]lsp.Location, 0, len(tags))
	for _, t := range tags {
		out = append(out, lsp.Location{
			URI: lsp.URIFromPath(t.path),
			Range: lsp.Range{
				Start: lsp.Position{Line: int(t.NameRange.StartPoint.Row), Character: int(t.NameRange.StartPoint.Column)},
				End:   lsp.Position{Line: int(t.NameRange.EndPoint.Row), Character: int(t.NameRange.EndPoint.Column)},
			},
		})
	}
	return out
}
