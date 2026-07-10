package ingest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SeamKind classifies a cross-language contract surface. A seam is a "thing one
// language produces and another language consumes" that a single-language
// finder cannot see. v1 owns two kinds: serialized data files shared between
// language runtimes, and environment variables read from multiple language
// runtimes. Intra-language config-field seams (e.g. a Go struct's field used
// by both producer and consumer Go packages) are deferred — that class is
// covered by the doc-contradiction miner, which has the cross-reference
// tooling it needs.
type SeamKind string

const (
	// SeamDataFile is a serialized data file (.json/.yaml/.yml/.proto/.ndjson
	// /.csv/.toml) referenced by source files in at least two distinct
	// non-Other languages. The seam key is the file basename.
	SeamDataFile SeamKind = "data_file"
	// SeamEnvVar is an environment variable read by source files in at least
	// two distinct non-Other languages. The seam key is the variable name.
	SeamEnvVar SeamKind = "env_var"
	// SeamHTTPRoute is an HTTP route path that has a server-side registration
	// (PRODUCER) and/or a client-side URL literal call (CONSUMER). The seam
	// key is the normalized path (leading '/', no host). Emitted when the
	// producer/consumer sets are non-empty and come from distinct files or
	// when only one side is present (contract drift: registered-but-never-
	// called, or called-but-never-registered). Go patterns only in v1; other
	// languages deferred.
	SeamHTTPRoute SeamKind = "http_route"
	// SeamRPCMethod is a protobuf RPC method (declared in a .proto IDL) that
	// may or may not have matching call sites in non-.proto source. The seam
	// key is the method name. PRODUCER = proto rpc declaration and/or
	// server-side handler func. CONSUMER = call site in non-proto source.
	// Emitted when there is a mismatch (declared-but-uncalled, called-but-
	// undeclared) or when both sides are present across >=2 files. Go + .proto
	// only in v1; other languages deferred.
	SeamRPCMethod SeamKind = "rpc_method"
)

// SeamSide describes one producer or consumer at a seam: the file that
// references the contract surface, the language of that file, and the line
// where the reference appears (best-effort: 0 when line could not be
// determined cheaply).
type SeamSide struct {
	// File is the repo-relative, forward-slash-separated path of the file
	// that references the seam.
	File string
	// Language is the file's coarse extension-derived language.
	Language Language
	// Line is the 1-based line of the first matching reference in File, or 0
	// when the detector could not localize the reference (rare — only when
	// the per-name scan in lineForEnvMatch finds no match).
	Line int
}

// Seam is one cross-language contract surface detected in the snapshot. The
// boundary lens's unit of work is one Seam: the agent reads every Side's
// file, then hunts for contract mismatches across the language boundary.
type Seam struct {
	// Kind classifies the contract surface (see SeamKind constants).
	Kind SeamKind
	// Key is the contract identifier: the data-file basename for
	// SeamDataFile, the env-var name for SeamEnvVar.
	Key string
	// Sides lists the files that touch the contract. Capped at seamMaxSides
	// (the detector trims after the cap with a deterministic one-per-
	// language-first, then round-robin policy). Sorted by File on output.
	Sides []SeamSide
}

// seamMaxSides caps the number of Sides recorded per seam. The agent has a
// bounded context; 8 sides covers a producer/consumer pair plus a handful
// of auxiliary readers in realistic polyglot repos. Beyond that the
// investigator is already sampling, not reading.
const seamMaxSides = 8

// seamMaxBytes is the per-file byte cap for the seam detectors. It is
// substantially larger than readHeadBytes in snapshot.go because the
// detectors want to see the whole file: data-file references can be
// anywhere in the source, not just the head.
const seamMaxBytes = 1 << 20 // 1 MiB

// seamMaxTotal caps the total number of seams returned from one snapshot.
// Seams are emitted in (Kind, Key) order, so a bounded list is
// deterministic. 100 covers a wide polyglot repo; bigger lists are a
// signal the detectors over-matched.
const seamMaxTotal = 100

// dataFileSuffixes lists the extensions EnumerateSeams treats as serialized
// data-file keys. Adding a new format here is a deliberate change: the
// detector will pick up cross-language references to the new format
// without any other code change (the boundary lens reads both sides
// regardless of format).
var dataFileSuffixes = []string{
	".json", ".yaml", ".yml", ".proto",
	".ndjson", ".csv", ".toml",
}

// dataFileSuffixSet is the lookup form for dataFileSuffixes. Built once at
// init so the per-reference check is a set lookup, not a slice scan.
var dataFileSuffixSet = func() map[string]bool {
	m := make(map[string]bool, len(dataFileSuffixes))
	for _, s := range dataFileSuffixes {
		m[s] = true
	}
	return m
}()

// quotedIdent is the canonical pattern for matching a string literal that
// names a contract surface. It accepts double-quoted ("…"), single-quoted
// ('…'), and back-tick (`…`) forms. The captured group is the literal
// contents (quotes stripped).
//
// PRECISION NOTE: the patterns are deliberately lenient on the inside —
// they accept anything that isn't the matching quote and that isn't a
// control character. They are a contract-surface grep, not a parser. A
// docstring like "see config.yaml" still counts as a data-file reference;
// we err on the side of surfacing more candidates because the lens
// itself (and triage) is the precision gate.
var quotedIdent = regexp.MustCompile(`"([^"\x00-\x1f]+)"|'([^'\x00-\x1f]+)'|` + "`([^`\x00-\x1f]+)`")

// EnumerateSeams scans the snapshot for cross-language contract surfaces
// and returns a deterministic, bounded list of seams. It is a pure
// function over the snapshot: it reads file bytes from disk through
// snap.Root, and never mutates the snapshot or the filesystem.
//
// Two detectors run independently:
//
//   - SeamDataFile: for every tracked source file (any non-Other
//     language), extract string literals whose value is a basename ending
//     in one of dataFileSuffixes; group by basename; emit a seam when the
//     SAME basename is referenced by files of >= 2 distinct non-Other
//     languages.
//
//   - SeamEnvVar: for every tracked source file in Go, Python,
//     JavaScript, or TypeScript, extract environment-variable references
//     (os.Getenv/os.LookupEnv in Go, os.environ[]/os.getenv in Python,
//     process.env.X / process.env["X"] in JS/TS); group by variable
//     name; emit a seam when the SAME variable is read by files of >= 2
//     distinct languages.
//
// Files that fail to read (deleted between Snapshot and EnumerateSeams,
// permission errors, oversize) are silently skipped — the result is
// best-effort, not exhaustive. The detectors over-match on purpose; the
// boundary lens and triage are the precision gates that follow.
//
// Output order is (Kind, Key): all SeamDataFile rows in lexicographic Key
// order, then all SeamEnvVar rows. Sides within a seam are sorted by File.
func EnumerateSeams(snap *Snapshot) []Seam {
	if snap == nil {
		return nil
	}
	// fileRef is the per-(file, language) row kept in the index maps.
	type fileRef struct {
		file string
		line int
	}
	// dataFileRefs: dataFileKey -> language -> []fileRef.
	dataFileRefs := make(map[string]map[Language][]fileRef)
	// envVarRefs: envVarName -> language -> []fileRef.
	envVarRefs := make(map[string]map[Language][]fileRef)

	// httpRouteProducers: normalizedPath -> []fileRef (Go server route registrations).
	httpRouteProducers := make(map[string][]fileRef)
	// httpRouteConsumers: normalizedPath -> []fileRef (Go client URL-literal call sites).
	httpRouteConsumers := make(map[string][]fileRef)

	// rpcProducers: methodName -> []fileRef (.proto declarations or Go server handlers).
	rpcProducers := make(map[string][]fileRef)
	// rpcConsumers: methodName -> []fileRef (call sites in non-.proto source).
	rpcConsumers := make(map[string][]fileRef)

	for _, f := range snap.Files {
		// .proto files are LangOther but are scanned for RPC declarations.
		isProto := strings.HasSuffix(f.Path, ".proto")
		if f.Language == LangOther && !isProto {
			continue
		}
		content, ok := readCapped(filepath.Join(snap.Root, f.Path), seamMaxBytes)
		if !ok {
			continue
		}
		// Data-file and env-var detectors apply only to known source languages,
		// not to .proto IDL files (which are processed solely by rpcProducers).
		if !isProto {
		// Data-file references: any string literal whose value ends in
		// a known data-file suffix. We accept every language's
		// quoted-string forms because the contract surface is the file
		// name, not the language.
		for _, idx := range quotedIdent.FindAllIndex(content, -1) {
			s, e := idx[0], idx[1]
			inner := string(content[s+1 : e-1])
			if !dataFileSuffixSet[strings.ToLower(filepath.Ext(inner))] {
				continue
			}
			base := filepath.Base(inner)
			if base == "." || base == "/" || base == "" {
				continue
			}
			line := lineForOffset(content, s)
			byLang, ok := dataFileRefs[base]
			if !ok {
				byLang = make(map[Language][]fileRef)
				dataFileRefs[base] = byLang
			}
			byLang[f.Language] = append(byLang[f.Language], fileRef{file: f.Path, line: line})
		}
		// Env-var references are language-specific; route to the per-language
		// detector. Each name found gets its own per-(name, language) row so the
		// reduction step can group by name across languages.
		envNames := extractEnvVarNames(f.Language, content)
		for _, name := range envNames {
			lref, ok := envVarRefs[name]
			if !ok {
				lref = make(map[Language][]fileRef)
				envVarRefs[name] = lref
			}
			line := lineForEnvMatch(f.Language, content, name)
			lref[f.Language] = append(lref[f.Language], fileRef{file: f.Path, line: line})
		}
		} // end if !isProto

		// HTTP route detection (Go only in v1; other languages deferred).
		// PRODUCER: server route registrations.
		// CONSUMER: client URL-literal call sites.
		if f.Language == LangGo {
			for _, m := range httpServerRouteRe.FindAllSubmatchIndex(content, -1) {
				// Two capture groups: [2:3] for HandleFunc/Handle, [4:5] for method forms.
				var raw string
				if m[2] >= 0 {
					raw = string(content[m[2]:m[3]])
				} else if m[4] >= 0 {
					raw = string(content[m[4]:m[5]])
				}
				path := normalizeHTTPPath(raw)
				if path == "" {
					continue
				}
				line := lineForOffset(content, m[0])
				httpRouteProducers[path] = append(httpRouteProducers[path], fileRef{file: f.Path, line: line})
			}
			for _, m := range httpClientCallRe.FindAllSubmatchIndex(content, -1) {
				// Two capture groups: [2:3] for NewRequest, [4:5] for method calls.
				var raw string
				if m[2] >= 0 {
					raw = string(content[m[2]:m[3]])
				} else if m[4] >= 0 {
					raw = string(content[m[4]:m[5]])
				}
				path := normalizeHTTPPath(raw)
				if path == "" {
					continue
				}
				line := lineForOffset(content, m[0])
				httpRouteConsumers[path] = append(httpRouteConsumers[path], fileRef{file: f.Path, line: line})
			}
		}

		// RPC method detection (Go + .proto in v1; other languages deferred).
		// .proto files: PRODUCER declarations.
		// Go files: server handler PRODUCER funcs + call-site CONSUMER refs.
		if isProto {
			for _, m := range protoRPCDeclRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcProducers[name] = append(rpcProducers[name], fileRef{file: f.Path, line: line})
			}
		} else if f.Language == LangGo {
			// Go server handlers: func (r *FooServer) MethodName(ctx ...
			for _, m := range goRPCHandlerRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcProducers[name] = append(rpcProducers[name], fileRef{file: f.Path, line: line})
			}
			// Call sites: .MethodName( or ClientName.MethodName( in Go
			for _, m := range goRPCCallRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcConsumers[name] = append(rpcConsumers[name], fileRef{file: f.Path, line: line})
			}
		}
	}

	// reduceSeams turns a per-language fileRef map into a Seam if the
	// >=2-distinct-languages condition holds, else returns nil.
	// Output sides are sorted by File; the per-language list is
	// sorted by (line, file) before side selection so the same
	// (file, line) wins on ties across runs.
	reduceSeams := func(kind SeamKind, key string, byLang map[Language][]fileRef) *Seam {
		if len(byLang) < 2 {
			return nil
		}
		// One side per language, then round-robin extras.
		seen := make(map[string]bool, seamMaxSides)
		var sides []SeamSide
		// Sort languages for deterministic first-side selection.
		langs := make([]Language, 0, len(byLang))
		for l := range byLang {
			langs = append(langs, l)
		}
		sort.Slice(langs, func(i, j int) bool { return langs[i] < langs[j] })
		// Per-language sorted refs (by line, then file).
		refsByLang := make([][]fileRef, len(langs))
		for i, l := range langs {
			refs := append([]fileRef(nil), byLang[l]...)
			sort.Slice(refs, func(a, b int) bool {
				if refs[a].line != refs[b].line {
					return refs[a].line < refs[b].line
				}
				return refs[a].file < refs[b].file
			})
			refsByLang[i] = refs
		}
		// First-side pass: one file per language.
		for i, refs := range refsByLang {
			if len(sides) >= seamMaxSides || len(refs) == 0 {
				break
			}
			fr := refs[0]
			if seen[fr.file] {
				continue
			}
			seen[fr.file] = true
			sides = append(sides, SeamSide{
				File:     fr.file,
				Language: langs[i],
				Line:     fr.line,
			})
		}
		// Round-robin extras.
		cursors := make([]int, len(refsByLang))
		for len(sides) < seamMaxSides {
			progress := false
			for i, refs := range refsByLang {
				if len(sides) >= seamMaxSides {
					break
				}
				if cursors[i] >= len(refs) {
					continue
				}
				cursors[i]++
				fr := refs[cursors[i]-1]
				if seen[fr.file] {
					continue
				}
				seen[fr.file] = true
				sides = append(sides, SeamSide{
					File:     fr.file,
					Language: langs[i],
					Line:     fr.line,
				})
				progress = true
			}
			if !progress {
				break
			}
		}
		sort.Slice(sides, func(i, j int) bool { return sides[i].File < sides[j].File })
		return &Seam{Kind: kind, Key: key, Sides: sides}
	}

	var out []Seam
	// Deterministic emission: data files first, sorted by basename.
	dataKeys := make([]string, 0, len(dataFileRefs))
	for k := range dataFileRefs {
		dataKeys = append(dataKeys, k)
	}
	sort.Strings(dataKeys)
	for _, k := range dataKeys {
		if s := reduceSeams(SeamDataFile, k, dataFileRefs[k]); s != nil {
			out = append(out, *s)
			if len(out) >= seamMaxTotal {
				return out
			}
		}
	}
	// Then env vars, sorted by name.
	envKeys := make([]string, 0, len(envVarRefs))
	for k := range envVarRefs {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		if s := reduceSeams(SeamEnvVar, k, envVarRefs[k]); s != nil {
			out = append(out, *s)
			if len(out) >= seamMaxTotal {
				return out
			}
		}
	}

	// reduceProducerConsumer builds a Seam from separate producer and consumer
	// fileRef slices. The gate is: at least one producer AND at least one
	// consumer across >=2 distinct files (a self-reference in one file must
	// not flood). If only one side is present (declared-but-uncalled or
	// called-but-undeclared) the seam is still emitted — that IS the contract
	// drift. If both sides are present but all refs live in a single file,
	// the seam is suppressed (not a cross-process boundary). Sides are sorted
	// by File for determinism.
	reduceProducerConsumer := func(kind SeamKind, key string, lang Language, producers, consumers []fileRef) *Seam {
		// Collect distinct files across both sides.
		filesSet := make(map[string]bool, len(producers)+len(consumers))
		for _, r := range producers {
			filesSet[r.file] = true
		}
		for _, r := range consumers {
			filesSet[r.file] = true
		}
		// Suppress pure self-references (only one distinct file, both sides in it).
		if len(filesSet) < 2 && len(producers) > 0 && len(consumers) > 0 {
			return nil
		}
		// Must have at least one side non-empty.
		if len(producers) == 0 && len(consumers) == 0 {
			return nil
		}
		var sides []SeamSide
		seen := make(map[string]bool, seamMaxSides)
		// Sort each side by (line, file) for deterministic first-pick.
		sortRefs := func(refs []fileRef) []fileRef {
			out := append([]fileRef(nil), refs...)
			sort.Slice(out, func(a, b int) bool {
				if out[a].line != out[b].line {
					return out[a].line < out[b].line
				}
				return out[a].file < out[b].file
			})
			return out
		}
		prods := sortRefs(producers)
		cons := sortRefs(consumers)
		// Interleave: one producer, one consumer, repeat until seamMaxSides.
		pi, ci := 0, 0
		for len(sides) < seamMaxSides {
			advanced := false
			for pi < len(prods) && len(sides) < seamMaxSides {
				r := prods[pi]
				pi++
				if seen[r.file] {
					continue
				}
				seen[r.file] = true
				sides = append(sides, SeamSide{File: r.file, Language: lang, Line: r.line})
				advanced = true
				break
			}
			for ci < len(cons) && len(sides) < seamMaxSides {
				r := cons[ci]
				ci++
				if seen[r.file] {
					continue
				}
				seen[r.file] = true
				sides = append(sides, SeamSide{File: r.file, Language: lang, Line: r.line})
				advanced = true
				break
			}
			if !advanced {
				break
			}
		}
		if len(sides) == 0 {
			return nil
		}
		sort.Slice(sides, func(i, j int) bool { return sides[i].File < sides[j].File })
		return &Seam{Kind: kind, Key: key, Sides: sides}
	}

	// HTTP routes: emit after env vars, sorted by path.
	httpKeys := make([]string, 0, len(httpRouteProducers)+len(httpRouteConsumers))
	httpKeySet := make(map[string]bool)
	for k := range httpRouteProducers {
		if !httpKeySet[k] {
			httpKeys = append(httpKeys, k)
			httpKeySet[k] = true
		}
	}
	for k := range httpRouteConsumers {
		if !httpKeySet[k] {
			httpKeys = append(httpKeys, k)
			httpKeySet[k] = true
		}
	}
	sort.Strings(httpKeys)
	for _, k := range httpKeys {
		if s := reduceProducerConsumer(SeamHTTPRoute, k, LangGo, httpRouteProducers[k], httpRouteConsumers[k]); s != nil {
			out = append(out, *s)
			if len(out) >= seamMaxTotal {
				return out
			}
		}
	}

	// RPC methods: emit after HTTP routes, sorted by method name.
	rpcKeys := make([]string, 0, len(rpcProducers)+len(rpcConsumers))
	rpcKeySet := make(map[string]bool)
	for k := range rpcProducers {
		if !rpcKeySet[k] {
			rpcKeys = append(rpcKeys, k)
			rpcKeySet[k] = true
		}
	}
	for k := range rpcConsumers {
		if !rpcKeySet[k] {
			rpcKeys = append(rpcKeys, k)
			rpcKeySet[k] = true
		}
	}
	sort.Strings(rpcKeys)
	for _, k := range rpcKeys {
		// For RPC, producers may be .proto (LangOther after DetectLanguage, but
		// we include them explicitly) or Go handlers; consumers are Go call sites.
		// Pass LangGo as the representative language; the lens reads the file.
		if s := reduceProducerConsumer(SeamRPCMethod, k, LangGo, rpcProducers[k], rpcConsumers[k]); s != nil {
			out = append(out, *s)
			if len(out) >= seamMaxTotal {
				return out
			}
		}
	}

	return out
}

// readCapped reads up to limit bytes from path. Returns (content, true)
// on success, (nil, false) on any error or oversize. The error is
// swallowed on purpose: seam enumeration is best-effort and over-matches
// on intent; the boundary lens is the precision gate.
func readCapped(path string, limit int64) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > limit {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return b, true
}

// lineForOffset returns the 1-based line number of byte offset off in
// content. off MUST be a valid index into content. 1 is returned for
// off==0 (start of file). The implementation counts newlines up to off
// without allocating — content is typically <1 MiB, so the O(n) scan is
// acceptable in a non-hot path.
func lineForOffset(content []byte, off int) int {
	if off <= 0 {
		return 1
	}
	if off > len(content) {
		off = len(content)
	}
	line := 1
	for i := 0; i < off; i++ {
		if content[i] == '\n' {
			line++
		}
	}
	return line
}

// envGoGetenv matches Go's os.Getenv("X") and os.LookupEnv("X") call
// forms. Lookup is fine-grained enough to keep the literal table
// reasonable: a single regex would match across many languages.
var envGoGetenv = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*"([A-Za-z_][A-Za-z0-9_]*)"`)

// envPyGetenv matches Python's os.environ["X"] and os.environ.get("X")
// and os.getenv("X") call forms.
var envPyGetenv = regexp.MustCompile(`os\.(?:environ\[\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\]|environ\.get\(\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\)|getenv\(\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\))`)

// envJsProcessEnv matches JS/TS process.env.X (member access) and
// process.env["X"] (computed access) forms.
var envJsProcessEnv = regexp.MustCompile(`process\.env\.([A-Za-z_][A-Za-z0-9_]*)|process\.env\[\s*["']([A-Za-z_][A-Za-z0-9_]*)["']\s*\]`)

// extractEnvVarNames returns the env-var names referenced in content,
// dispatching on language. The matchers are deliberately narrow (no
// fuzzy matching); a refactor that introduces a different env-var API
// needs an explicit detector addition. Unknown languages yield an empty
// slice.
func extractEnvVarNames(lang Language, content []byte) []string {
	switch lang {
	case LangGo:
		out := envGoGetenv.FindAllSubmatch(content, -1)
		if len(out) == 0 {
			return nil
		}
		names := make([]string, 0, len(out))
		for _, m := range out {
			names = append(names, string(m[1]))
		}
		return names
	case LangPython:
		out := envPyGetenv.FindAllSubmatch(content, -1)
		if len(out) == 0 {
			return nil
		}
		names := make([]string, 0, len(out))
		for _, m := range out {
			for i := 1; i < len(m); i++ {
				if len(m[i]) > 0 {
					names = append(names, string(m[i]))
					break
				}
			}
		}
		return names
	case LangJavaScript, LangTypeScript:
		out := envJsProcessEnv.FindAllSubmatch(content, -1)
		if len(out) == 0 {
			return nil
		}
		names := make([]string, 0, len(out))
		for _, m := range out {
			for i := 1; i < len(m); i++ {
				if len(m[i]) > 0 {
					names = append(names, string(m[i]))
					break
				}
			}
		}
		return names
	default:
		return nil
	}
}

// lineForEnvMatch returns the 1-based line of the first env-var
// reference to name in content. The detector runs the same per-language
// regex as extractEnvVarNames, but returns the line of the first match
// for the given variable. 0 when the variable is not referenced (which
// should be impossible because extractEnvVarNames already saw it — but
// the defensive return keeps the function total).
func lineForEnvMatch(lang Language, content []byte, name string) int {
	switch lang {
	case LangGo:
		re := regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*"` + regexp.QuoteMeta(name) + `"`)
		loc := re.FindIndex(content)
		if loc == nil {
			return 0
		}
		return lineForOffset(content, loc[0])
	case LangPython:
		patterns := []string{
			`os\.environ\[\s*['"]` + regexp.QuoteMeta(name) + `['"]\s*\]`,
			`os\.environ\.get\(\s*['"]` + regexp.QuoteMeta(name) + `['"]`,
			`os\.getenv\(\s*['"]` + regexp.QuoteMeta(name) + `['"]`,
		}
		for _, p := range patterns {
			re := regexp.MustCompile(p)
			loc := re.FindIndex(content)
			if loc != nil {
				return lineForOffset(content, loc[0])
			}
		}
		return 0
	case LangJavaScript, LangTypeScript:
		patterns := []string{
			`process\.env\.` + regexp.QuoteMeta(name) + `\b`,
			`process\.env\[\s*['"]` + regexp.QuoteMeta(name) + `['"]\s*\]`,
		}
		for _, p := range patterns {
			re := regexp.MustCompile(p)
			loc := re.FindIndex(content)
			if loc != nil {
				return lineForOffset(content, loc[0])
			}
		}
		return 0
	default:
		return 0
	}
}

// httpServerRouteRe matches Go HTTP server route registrations. Two pattern
// groups are combined:
//  1. Standard-library mux: .HandleFunc("/path", ...) / http.Handle("/path", ...)
//     — the HandleFunc/Handle keyword is unique to server-side registration.
//  2. Router-framework shorthand: .Get("/path", handler) / .Post("/path", handler)
//     — distinguished from the client http.Get("/path") by requiring a COMMA
//     after the path literal (a handler arg always follows in router frameworks).
//
// Captured groups: group 1 for HandleFunc/Handle forms, group 2 for method forms.
// Go only in v1; other language server frameworks are deferred.
var httpServerRouteRe = regexp.MustCompile(`(?:(?:\.HandleFunc|\.Handle|http\.Handle|http\.HandleFunc)\s*\(\s*"(/[^"\x00-\x1f]*)"|(?:\.Get|\.Post|\.Put|\.Delete|\.Patch)\s*\(\s*"(/[^"\x00-\x1f]*)"\s*,)`)

// httpClientCallRe matches Go HTTP client URL-literal call sites. Captured
// group 1 is the URL/path literal. Patterns:
//   - http.NewRequest(method, "url", ...)
//   - client.Get("url") / .Get("url")
//   - resp.Get("url") — conservative: any .Get/.Post/.Put/.Delete with a
//     string literal starting with '/' or "http"
//
// Go only in v1; other client libraries are deferred.
var httpClientCallRe = regexp.MustCompile(`(?:http\.NewRequest\s*\([^,]+,\s*"((?:https?://[^"]*|/[^"]*))"|(?:\.Get|\.Post|\.Put|\.Delete|\.Patch)\s*\(\s*"((?:https?://[^"]*|/[^"]*))"\s*[,)])`)

// protoRPCDeclRe matches protobuf RPC declarations in .proto files.
// Captured group 1 is the method name.
var protoRPCDeclRe = regexp.MustCompile(`\brpc\s+([A-Z][A-Za-z0-9_]*)\s*\(`)

// goRPCHandlerRe matches Go gRPC server handler method declarations of the
// form:  func (r *SomethingServer) MethodName(ctx
// Captured group 1 is the method name. These are recognizable as RPC
// implementations by the receiver type ending in "Server" and the first
// parameter being ctx.
var goRPCHandlerRe = regexp.MustCompile(`func\s*\([^)]*Server[^)]*\)\s+([A-Z][A-Za-z0-9_]*)\s*\(\s*ctx\b`)

// goRPCCallRe matches Go gRPC client call sites of the form:
//   someClient.MethodName(ctx, ...)
// where MethodName begins with an uppercase letter (exported, matching proto
// conventions). Captured group 1 is the method name. We require the receiver
// to be a lowercase-starting identifier (variable, not type assertion) to
// reduce false positives from struct literals and interface declarations.
var goRPCCallRe = regexp.MustCompile(`\b[a-z][A-Za-z0-9_]*\.([A-Z][A-Za-z0-9_]*)\s*\(\s*ctx\b`)

// normalizeHTTPPath strips the scheme+host from a URL literal and returns the
// path component. Returns "" for any literal that doesn't look like a routable
// path (e.g. bare filenames, empty strings, patterns with only a query string).
// Examples:
//
//	"/widgets"              → "/widgets"
//	"http://api.example.com/widgets" → "/widgets"
//	"https://svc/v1/users"  → "/v1/users"
//	""                      → ""
//	"widgets"               → "" (no leading slash — not a routable path literal)
func normalizeHTTPPath(raw string) string {
	if raw == "" {
		return ""
	}
	// Strip scheme+host if present.
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "" // bare host, no path
		}
		raw = rest[slash:]
	}
	// Path must start with '/'.
	if raw == "" || raw[0] != '/' {
		return ""
	}
	// Strip query and fragment for grouping.
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "/" || raw == "" {
		return "" // root-only path is too generic
	}
	return raw
}
