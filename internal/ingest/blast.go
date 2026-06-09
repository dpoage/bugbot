package ingest

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// BlastRadius computes the set of files an investigation should consider given
// a set of changed files: the changed files themselves plus their direct
// dependents. The result is the union of the changed set and the dependents,
// sorted and de-duplicated.
//
// "Direct dependents" are resolved with two strategies, layered:
//
//   - Go-aware import graph. All .go files in the snapshot are parsed for their
//     package import paths. A changed .go file is mapped to the local Go
//     package it declares; every .go file whose package imports that package is
//     a dependent. Local packages are keyed by their import-path SUFFIX (the
//     repo-relative directory), so we match imports without knowing the
//     module's root import path. This is direct (one hop) by design — it scopes
//     work, it is not a full transitive closure.
//
//   - Textual fallback for non-Go (and as a backstop). For each changed file we
//     search the snapshot for word-boundary references to the file's basename
//     without extension (e.g. a change to `auth/login.py` looks for the token
//     `login`). Case-sensitive, whole-word matches only. This is necessarily
//     imprecise: a common basename like `index` or `utils` will over-match, and
//     dynamic/reflective references are missed entirely. It is a recall aid for
//     scoping, not a precise dependency analysis.
//
// PRECISION LIMITS: the Go graph is one hop and ignores build tags, cgo, and
// dot/blank imports' transitive effects; the textual pass trades precision for
// recall. Downstream stages must treat the radius as "files worth looking at,"
// not "files definitely affected."
func (r *Repo) BlastRadius(ctx context.Context, snap *Snapshot, changed []string) ([]string, error) {
	result := newStringSet()
	for _, c := range changed {
		result.add(c)
	}
	if snap == nil || len(changed) == 0 {
		return result.sorted(), nil
	}

	changedSet := newStringSet()
	for _, c := range changed {
		changedSet.add(c)
	}

	// --- Go import-graph dependents -------------------------------------
	goDeps, err := r.goDependents(ctx, snap, changedSet)
	if err != nil {
		return nil, err
	}
	for _, p := range goDeps {
		result.add(p)
	}

	// --- Textual fallback for non-Go changed files ----------------------
	// Only run the (more expensive, lower-precision) text search for changed
	// files we did NOT resolve via the Go graph, to avoid double work and to
	// keep Go precision high.
	for _, c := range changed {
		if DetectLanguage(c) == LangGo {
			continue
		}
		deps, err := r.textualDependents(ctx, snap, c, changedSet)
		if err != nil {
			return nil, err
		}
		for _, p := range deps {
			result.add(p)
		}
	}

	return result.sorted(), nil
}

// goPackageDir returns the repo-relative directory of a path, which serves as
// the local package key (Go packages are one-per-directory).
func goPackageDir(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

// goDependents builds the Go import graph over the snapshot and returns the
// .go files that directly import any package containing a changed .go file.
func (r *Repo) goDependents(ctx context.Context, snap *Snapshot, changed *stringSet) ([]string, error) {
	// Collect .go files and parse imports once.
	type goFile struct {
		path    string
		dir     string   // local package key (repo-relative dir)
		imports []string // import paths
	}
	var files []goFile
	// changedDirs: local package dirs touched by a changed .go file.
	changedDirs := newStringSet()

	for _, f := range snap.Files {
		if f.Language != LangGo {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		imports, err := parseGoImports(filepath.Join(r.root, filepath.FromSlash(f.Path)))
		if err != nil {
			// Unparseable Go (syntax error, generated stub): skip its imports
			// rather than fail the whole computation.
			imports = nil
		}
		dir := goPackageDir(f.Path)
		files = append(files, goFile{path: f.Path, dir: dir, imports: imports})
		if changed.has(f.Path) {
			changedDirs.add(dir)
		}
	}

	if changedDirs.len() == 0 {
		return nil, nil
	}

	var deps []string
	for _, gf := range files {
		// A file in the changed package is already covered by the changed set;
		// here we want OTHER files that import the changed package.
		for _, imp := range gf.imports {
			if importMatchesLocalDir(imp, changedDirs) {
				deps = append(deps, gf.path)
				break
			}
		}
	}
	return deps, nil
}

// importMatchesLocalDir reports whether import path imp refers to one of the
// changed local package directories. Because we do not know the module's root
// import path, we match by suffix: an import "github.com/x/y/auth/session"
// matches local dir "auth/session". The empty dir (repo root package) matches
// an import equal to the module path itself, which we cannot identify by
// suffix; root-package dependents are therefore only caught when some file in
// the importing set is itself in the root (handled by the changed set).
func importMatchesLocalDir(imp string, dirs *stringSet) bool {
	imp = strings.Trim(imp, "/")
	for d := range dirs.m {
		if d == "" {
			continue
		}
		if imp == d || strings.HasSuffix(imp, "/"+d) {
			return true
		}
	}
	return false
}

// parseGoImports parses just the import section of a Go file and returns the
// imported package paths (unquoted).
func parseGoImports(abs string) ([]string, error) {
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, abs, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	imports := make([]string, 0, len(af.Imports))
	for _, spec := range af.Imports {
		if spec.Path == nil {
			continue
		}
		p := strings.Trim(spec.Path.Value, `"`)
		if p != "" {
			imports = append(imports, p)
		}
	}
	return imports, nil
}

// textualDependents searches the snapshot for word-boundary references to the
// changed file's basename-without-extension. The changed file itself and other
// already-changed files are excluded from the returned dependents.
func (r *Repo) textualDependents(ctx context.Context, snap *Snapshot, changedPath string, changed *stringSet) ([]string, error) {
	base := basenameNoExt(changedPath)
	// Skip tokens that are too short or too generic to be a useful signal;
	// matching on "" or single characters would flag the whole repo.
	if len(base) < 3 {
		return nil, nil
	}

	var deps []string
	for _, f := range snap.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f.Path == changedPath || changed.has(f.Path) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(f.Path)))
		if err != nil {
			// Raced deletion or unreadable file: skip.
			continue
		}
		if containsWord(content, base) {
			deps = append(deps, f.Path)
		}
	}
	return deps, nil
}

// basenameNoExt returns the final path element with its extension stripped.
func basenameNoExt(p string) string {
	b := path.Base(p)
	if ext := path.Ext(b); ext != "" {
		b = b[:len(b)-len(ext)]
	}
	return b
}

// containsWord reports whether word appears in content as a whole word: bounded
// on both sides by a non-identifier character (or start/end of input).
// Matching is case-sensitive. "Identifier character" is [A-Za-z0-9_], which
// covers the common cross-language case of importing/referencing a module by
// its file basename.
func containsWord(content []byte, word string) bool {
	if word == "" {
		return false
	}
	s := string(content)
	from := 0
	for {
		idx := strings.Index(s[from:], word)
		if idx < 0 {
			return false
		}
		start := from + idx
		end := start + len(word)
		beforeOK := start == 0 || !isIdentByte(s[start-1])
		afterOK := end == len(s) || !isIdentByte(s[end])
		if beforeOK && afterOK {
			return true
		}
		from = start + 1
		if from >= len(s) {
			return false
		}
	}
}

// isIdentByte reports whether b is an identifier byte [A-Za-z0-9_].
func isIdentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_':
		return true
	default:
		return false
	}
}
