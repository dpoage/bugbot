package ingest

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// bazelQueryTimeout is the maximum wall time allowed for a `bazel query`
// invocation. Bazel startup can be slow on cold caches; 30 s is a generous
// but bounded ceiling.
const bazelQueryTimeout = 30 * time.Second

// BlastRadius computes the set of files an investigation should consider given
// a set of changed files: the changed files themselves plus their direct
// dependents. The result is the union of the changed set and the dependents,
// sorted and de-duplicated.
//
// "Direct dependents" are resolved with up to three strategies, applied in
// order, with graceful fallback on any error:
//
//  1. Bazel-native reverse-dep query (when the repo is a Bazel workspace AND
//     `bazel` is on PATH). Changed files are mapped to Bazel labels; then
//     `bazel query "rdeps(//..., <labels>)" --output=package` enumerates
//     reverse-dependent packages; packages are mapped back to snapshot files.
//     A bounded 30 s timeout is enforced. On ANY error (tool absent, query
//     fails, timeout) the Bazel path is skipped and the Go+textual fallbacks
//     run instead.
//
//  2. Go-aware import graph. All .go files in the snapshot are parsed for
//     their package import paths. A changed .go file is mapped to the local Go
//     package it declares; every .go file whose package imports that package is
//     a dependent. Local packages are keyed by their import-path SUFFIX (the
//     repo-relative directory), so we match imports without knowing the
//     module's root import path. This handles both single-module repos (go.mod)
//     and go.work multi-module workspaces: the suffix-matching approach already
//     resolves cross-module imports correctly as long as the directory paths
//     appear as import-path suffixes, which is the standard Go convention. No
//     additional exec is needed for go.work.
//
//  3. Textual fallback for non-Go (and as a backstop). For each changed file
//     we search the snapshot for word-boundary references to the file's
//     basename without extension (e.g. a change to `auth/login.py` looks for
//     the token `login`). Case-sensitive, whole-word matches only. This is
//     necessarily imprecise: a common basename like `index` or `utils` will
//     over-match, and dynamic/reflective references are missed entirely. It is
//     a recall aid for scoping, not a precise dependency analysis.
//
// PRECISION LIMITS: the Bazel path is one query level (rdeps to packages, not
// individual files); the Go graph is one hop and ignores build tags, cgo, and
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

	// --- Bazel-native reverse-dep query ---------------------------------
	// Attempt when the repo has a Bazel workspace marker. Two cases:
	//
	//   nil queryRunner (production): run exec.LookPath("bazel") first; skip
	//   the native path entirely when bazel is absent to avoid spawning a
	//   doomed process. When bazel is present, use execQueryRunner.
	//
	//   non-nil queryRunner (test injection): bypass LookPath and call the
	//   injected runner directly — the runner controls all output and errors.
	//
	// Any failure (tool absent, query error, timeout) is silently swallowed;
	// we fall through to the Go+textual path.
	systems := DetectBuildSystems(r.root)
	isBazel := false
	for _, s := range systems {
		if s == BuildSystemBazel {
			isBazel = true
			break
		}
	}
	if isBazel {
		var qr queryRunner
		if r.queryRunner != nil {
			// Injected test runner: bypass LookPath, always call it.
			qr = r.queryRunner
		} else {
			// Production: pre-check that bazel is on PATH before spawning.
			if _, lookErr := exec.LookPath("bazel"); lookErr == nil {
				qr = execQueryRunner
			}
			// If bazel is absent, qr stays nil and we skip the native path.
		}
		if qr != nil {
			bazelDeps, berr := r.bazelDependents(ctx, snap, changedSet, qr)
			if berr == nil {
				for _, p := range bazelDeps {
					result.add(p)
				}
			}
			// On error: fall through to Go+textual path (no return).
		}
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

// bazelDependents queries Bazel for the reverse dependencies of the changed
// files. It maps each changed file to a Bazel file-label
// ("//path/to:file.ext"), then runs:
//
//	bazel query "rdeps(//..., set('//a:f.go' '//b:g.go' ...))" --output=package
//
// Labels are single-quoted inside the set() expression so that spaces, parens,
// and other Bazel query metacharacters in file paths do not corrupt the query.
// Labels that themselves contain a single quote are skipped (they would break
// the quoting and cannot be safely escaped in this context).
//
// The query runs with a bounded timeout (bazelQueryTimeout). Any error causes
// an immediate return so the caller can fall back to the Go+textual path.
func (r *Repo) bazelDependents(ctx context.Context, snap *Snapshot, changed *stringSet, qr queryRunner) ([]string, error) {
	// Map changed files to Bazel file labels: //dir:basename
	var labels []string
	for p := range changed.m {
		dir := path.Dir(p)
		base := path.Base(p)
		var label string
		if dir == "." {
			label = "//:" + base
		} else {
			label = "//" + dir + ":" + base
		}
		// Skip labels that contain a single quote: they cannot be safely
		// single-quoted inside the set() expression.
		if strings.ContainsRune(label, '\'') {
			continue
		}
		labels = append(labels, "'"+label+"'")
	}
	if len(labels) == 0 {
		return nil, nil
	}

	// Build the rdeps query expression with single-quoted labels.
	setExpr := "set(" + strings.Join(labels, " ") + ")"
	queryExpr := "rdeps(//..., " + setExpr + ")"

	// Apply a bounded timeout on top of any existing deadline.
	qctx, cancel := context.WithTimeout(ctx, bazelQueryTimeout)
	defer cancel()

	out, err := qr(qctx, r.root, "bazel", "query", queryExpr, "--output=package")
	if err != nil {
		return nil, err
	}

	// Parse the package list and collect snapshot files whose directory is a
	// Bazel package returned by the query.
	//
	// Bazel --output=package emits "//pkg/path" or just "pkg/path" per line;
	// we normalise by stripping the leading "//".
	pkgSet := newStringSet()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pkg := strings.TrimPrefix(line, "//")
		pkgSet.add(pkg)
	}

	var deps []string
	for _, f := range snap.Files {
		dir := path.Dir(f.Path)
		if dir == "." {
			dir = ""
		}
		// --output=package emits package directories, not individual file paths,
		// so we match files by their containing directory only.
		if pkgSet.has(dir) {
			deps = append(deps, f.Path)
		}
	}
	return deps, nil
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

// PackageImporters returns, per local Go package directory, the set of OTHER
// local package directories that directly import it (its direct dependents),
// computed over the WHOLE snapshot. It is the unscoped convenience wrapper
// around PackageImportersScoped; see that method for the algorithm and the
// reuse rationale.
func (r *Repo) PackageImporters(ctx context.Context, snap *Snapshot) (map[string][]string, error) {
	return r.PackageImportersScoped(ctx, snap, nil)
}

// PackageImportersScoped is PackageImporters restricted to a set of in-scope
// files. When inScope is non-nil, only files whose repo-relative path is a key
// in inScope are parsed, and only those files' package directories form the
// "universe" of keys and suffix-match candidates — so every recorded edge has
// BOTH endpoints inside inScope. When inScope is nil the whole snapshot is used
// (the wrapper's behaviour).
//
// This is the large-repo lever for the cartographer: on a blast-radius scan the
// only importer edges contextFor can ever inject are those between packages that
// have summaries (i.e. inside the blast radius), so parsing the whole repo's
// imports is wasted work. Scoping the parse to the blast set drops the cost from
// O(snapshot) to O(in-scope) while producing byte-identical injection.
//
// Non-Go files contribute no edges; unparseable Go files are skipped. Keys and
// values are repo-relative directory paths (goPackageDir). Values are sorted and
// de-duplicated; self-edges (a package importing itself) are omitted. It reuses
// parseGoImports, goPackageDir, and the importMatchesLocalDir suffix logic from
// goDependents rather than reimplementing the graph (the same forward scan, with
// edges inverted).
//
// ctx is honored at every file boundary so a cancellation between parse calls
// aborts the pass promptly rather than scanning the full set.
func (r *Repo) PackageImportersScoped(ctx context.Context, snap *Snapshot, inScope map[string]bool) (map[string][]string, error) {
	if snap == nil {
		return nil, nil
	}
	// Pass 1: collect local package directories and per-file (dir, imports)
	// pairs. localDirs is the "universe" of possible keys in the result map
	// and the set suffix-matching tests imports against.
	localDirs := newStringSet()
	type goFile struct {
		dir     string
		imports []string
	}
	var files []goFile
	for _, f := range snap.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f.Language != LangGo {
			continue
		}
		if inScope != nil && !inScope[f.Path] {
			continue
		}
		imports, err := parseGoImports(filepath.Join(r.root, filepath.FromSlash(f.Path)))
		if err != nil {
			// Unparseable Go (syntax error, generated stub): skip its
			// imports rather than fail the whole computation. Mirrors
			// goDependents' posture — the graph is best-effort, not strict.
			imports = nil
		}
		dir := goPackageDir(f.Path)
		localDirs.add(dir)
		files = append(files, goFile{dir: dir, imports: imports})
	}
	if localDirs.len() == 0 {
		return map[string][]string{}, nil
	}

	// Pass 2: invert the edges. For each file's import, ask
	// importMatchesLocalDir whether it suffix-matches any local dir. If so,
	// identify the specific dir it matches and record the file's own dir as
	// an importer of that dir. Self-edges (file's dir == matched dir) are
	// omitted — a package is not a dependent of itself.
	importers := make(map[string]map[string]struct{})
	for _, gf := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, imp := range gf.imports {
			matchedDir := matchLocalDir(imp, localDirs)
			if matchedDir == "" || matchedDir == gf.dir {
				continue
			}
			if importers[matchedDir] == nil {
				importers[matchedDir] = make(map[string]struct{})
			}
			importers[matchedDir][gf.dir] = struct{}{}
		}
	}

	// Flatten to sorted slices for deterministic output.
	result := make(map[string][]string, len(importers))
	for d, set := range importers {
		out := make([]string, 0, len(set))
		for im := range set {
			out = append(out, im)
		}
		sort.Strings(out)
		result[d] = out
	}
	return result, nil
}

// matchLocalDir returns the specific local directory d in dirs that
// import path imp suffix-matches, or "" if none (or the match is the
// empty dir — an import equal to the module path cannot be uniquely
// attributed). It is the per-import version of importMatchesLocalDir that
// PackageImporters uses to invert the edge.
//
// Like importMatchesLocalDir, the empty dir is excluded: root-package
// dependents are not identifiable by suffix alone and would otherwise be
// silently misattributed to whichever dir's suffix happened to match.
func matchLocalDir(imp string, dirs *stringSet) string {
	imp = strings.Trim(imp, "/")
	for d := range dirs.m {
		if d == "" {
			continue
		}
		if imp == d || strings.HasSuffix(imp, "/"+d) {
			return d
		}
	}
	return ""
}
