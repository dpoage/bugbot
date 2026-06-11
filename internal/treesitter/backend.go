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
//
// # Grammar binary cost and subsetting
//
// The gotreesitter `grammars` package go:embeds ~206 compressed grammar blobs
// (~20MB) by default; importing it links them all even though this tier only
// uses go/python/typescript/tsx. The upstream supports build-tag subsetting:
// building with `-tags 'grammar_subset grammar_subset_go grammar_subset_python
// grammar_subset_typescript grammar_subset_tsx'` compiles out the all-grammars
// registry and embeds ONLY the selected blobs, cutting the project's static
// binary by ~21MB (86MB -> 65MB). `make build` sets these tags (see Makefile's
// GRAMMAR_TAGS). A plain `go build ./...` (no tags) still works — it just
// embeds every grammar. If a new language is added to grammarTable, add its
// matching grammar_subset_<lang> tag to GRAMMAR_TAGS.
package treesitter

import (
	"bytes"
	"io/fs"
	"log/slog"
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
// source files with tree-sitter. It is safe for concurrent use across agent
// runners; parsed taggers are cached per language for the backend's lifetime,
// and a per-file tag cache avoids re-parsing unchanged files on repeat queries.
type Backend struct {
	root string

	mu      sync.Mutex
	taggers map[string]*lockedTagger // keyed by "grammar.name/kind"

	cacheMu sync.Mutex
	cache   map[cacheKey]cacheEntry // keyed by grammar+kind+path

	// tagFn is the seam used to invoke a tagger. Production uses tagWithRecover;
	// tests inject a panicking function to exercise the recover wrapper. nil
	// means use the default.
	tagFn func(tg *gts.Tagger, src []byte) []gts.Tag
}

// lockedTagger pairs a compiled *gts.Tagger with the mutex that serializes its
// Tag calls. A gts.Tagger is NOT safe for concurrent use: it reuses an internal
// matchesBuf and its *Parser carries mutable parse state (reuse cursor and
// scratch buffers), so two goroutines calling Tag concurrently is a write/write
// data race (confirmed by the race detector). Tools are shared across
// concurrent agent runners, so we serialize per (language, kind) with this
// mutex. A sync.Pool of taggers was the alternative; the mutex is chosen
// because this is a fallback tier (LSP is the hot path), grammar compilation is
// non-trivial, and per-language serialization is acceptable here — pooling
// would add construction churn for little benefit at this tier's call volume.
type lockedTagger struct {
	mu sync.Mutex
	tg *gts.Tagger
}

// cacheKey identifies a per-file tag set: the same file is tagged differently
// for the definition vs reference query, so kind is part of the key.
type cacheKey struct {
	grammar string
	kind    queryKind
	path    string
}

// cacheEntry is a file's tags plus the stat fields used to revalidate it. If a
// later stat shows the same mtime and size, the cached tags are reused without
// re-reading or re-parsing the file.
type cacheEntry struct {
	mtimeUnixNano int64
	size          int64
	tags          []gts.Tag
}

// New creates a tree-sitter backend rooted at the absolute path root. No
// grammars are loaded until the first query for a language.
func New(root string) *Backend {
	return &Backend{
		root:    root,
		taggers: make(map[string]*lockedTagger),
		cache:   make(map[cacheKey]cacheEntry),
	}
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

// DefinitionBodies is like Definition but returns locations whose Range spans
// the WHOLE declaration node (the full function/method/type/def body), not just
// the symbol's name identifier. It exists so the read_symbol tool can render the
// complete declaration text — a 40-line function instead of a capped whole-file
// read — while Definition keeps returning name ranges, which find_definition's
// "path:line: source" rendering depends on. Ranking and the ambiguous/candidate
// accounting are identical to Definition; only the per-location Range differs.
func (b *Backend) DefinitionBodies(absPath, symbol string) (Result, error) {
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
		Locations:  toBodyLocations(matches),
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
// returns the tags produced by the selected query. Files that fail to parse —
// including ones that make the GLR parser panic on pathological input — are
// skipped (a syntactic tier degrades per-file, never aborts). Unchanged files
// are served from the per-file tag cache rather than re-parsed.
func (b *Backend) collect(g *grammar, kind queryKind) ([]tag, error) {
	lt, err := b.taggerFor(g, kind)
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
		tags := b.tagFile(g, kind, lt, path)
		for _, t := range tags {
			out = append(out, tag{path: path, Tag: t})
		}
		return nil
	})
	return out, nil
}

// tagFile returns the tags for one file, served from the per-file cache when
// the file's mtime+size are unchanged. On a cache miss it reads, parses, and
// caches the result. A parse panic (or read failure) yields no tags and is not
// cached, so the file is retried on the next query.
func (b *Backend) tagFile(g *grammar, kind queryKind, lt *lockedTagger, path string) []gts.Tag {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxFileBytes {
		return nil
	}
	mtime := info.ModTime().UnixNano()
	size := info.Size()
	key := cacheKey{grammar: g.name, kind: kind, path: path}

	b.cacheMu.Lock()
	if e, ok := b.cache[key]; ok && e.mtimeUnixNano == mtime && e.size == size {
		tags := e.tags
		b.cacheMu.Unlock()
		return tags
	}
	b.cacheMu.Unlock()

	src, ok := readSource(path)
	if !ok {
		return nil
	}
	tags, ok := b.tag(lt, src)
	if !ok {
		// Parse panicked; skip the file and do not poison the cache.
		slog.Debug("treesitter: dropped file after parse panic", "path", path)
		return nil
	}

	b.cacheMu.Lock()
	b.cache[key] = cacheEntry{mtimeUnixNano: mtime, size: size, tags: tags}
	b.cacheMu.Unlock()
	return tags
}

// tag runs the (serialized) tagger over src, recovering any panic from the
// GLR parser's safety caps so one pathological file cannot crash the agent. It
// returns ok=false when a panic was recovered.
func (b *Backend) tag(lt *lockedTagger, src []byte) (tags []gts.Tag, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("treesitter: recovered from parse panic", "panic", r)
			tags, ok = nil, false
		}
	}()
	lt.mu.Lock()
	defer lt.mu.Unlock()
	fn := b.tagFn
	if fn == nil {
		fn = func(tg *gts.Tagger, s []byte) []gts.Tag { return tg.Tag(s) }
	}
	return fn(lt.tg, src), true
}

// taggerFor compiles (once) and returns the locked tagger for the grammar and
// query kind. Grammar loading happens on first use of a language; construction
// is wrapped in a recover so a panic while loading a grammar surfaces as an
// error rather than crashing the agent.
func (b *Backend) taggerFor(g *grammar, kind queryKind) (lt *lockedTagger, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var query string
	switch kind {
	case queryDef:
		query = g.defQuery
	default:
		query = g.refQuery
	}
	cacheK := g.name + "/" + query

	if existing := b.taggers[cacheK]; existing != nil {
		return existing, nil
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Debug("treesitter: recovered from tagger construction panic", "grammar", g.name, "panic", r)
			lt, err = nil, &unsupportedError{lang: g.name}
		}
	}()

	tg, err := newTagger(g, query)
	if err != nil {
		return nil, err
	}
	lt = &lockedTagger{tg: tg}
	b.taggers[cacheK] = lt
	return lt, nil
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

// toBodyLocations converts ranked tags to LSP locations spanning each symbol's
// WHOLE declaration node (gts.Tag.Range, the @definition.X capture), so a caller
// can render the full body. This is the only difference from toLocations, which
// uses the name range.
//
// It also extends each location's start row upward to include any contiguous
// decorator lines that precede the captured node. The tree-sitter @definition
// capture for Python def/class nodes starts at the "def"/"class" keyword —
// the decorated_definition parent node (which owns the @decorator children) is
// not captured. TypeScript method decorators are also excluded from the
// method_definition capture. Since gts.Tag exposes only the byte/row/column
// Range of the captured node (no parent-node handle), we use a line-based scan:
// starting from the line immediately above the captured start, we scan backward
// over contiguous lines whose first non-whitespace character is '@', extending
// the range to include all of them (bounded to decoratorMaxLookback lines).
func toBodyLocations(tags []tag) []lsp.Location {
	out := make([]lsp.Location, 0, len(tags))
	for _, t := range tags {
		startRow := decoratorAdjustedStart(t.path, t.Range.StartPoint.Row)
		out = append(out, lsp.Location{
			URI: lsp.URIFromPath(t.path),
			Range: lsp.Range{
				Start: lsp.Position{Line: int(startRow), Character: 0},
				End:   lsp.Position{Line: int(t.Range.EndPoint.Row), Character: int(t.Range.EndPoint.Column)},
			},
		})
	}
	return out
}

// decoratorMaxLookback is the maximum number of lines we scan backward from a
// definition's start looking for decorator lines. 16 covers any realistic
// decorator stack in Python or TypeScript.
const decoratorMaxLookback = 16

// decoratorAdjustedStart returns the 0-based row at which the declaration
// actually begins, extended upward over any contiguous decorator lines that
// immediately precede bodyStartRow. A decorator line is one whose first
// non-whitespace character is '@'. If the file cannot be read (already
// parsed successfully above, so this is an unexpected edge case) or no
// decorators are found, bodyStartRow is returned unchanged.
func decoratorAdjustedStart(path string, bodyStartRow uint32) uint32 {
	if bodyStartRow == 0 {
		return bodyStartRow
	}
	src, ok := readSource(path)
	if !ok {
		return bodyStartRow
	}
	lines := bytes.Split(src, []byte("\n"))
	// bodyStartRow is 0-based. Scan from row bodyStartRow-1 upward.
	firstRow := bodyStartRow
	for lookback := 1; lookback <= decoratorMaxLookback; lookback++ {
		if bodyStartRow < uint32(lookback) {
			break
		}
		row := bodyStartRow - uint32(lookback)
		if row >= uint32(len(lines)) {
			break
		}
		trimmed := bytes.TrimLeft(lines[row], " \t")
		if len(trimmed) == 0 || trimmed[0] != '@' {
			break
		}
		firstRow = row
	}
	return firstRow
}
