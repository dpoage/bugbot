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
	// The lang field carries the actual file language for HTTP/RPC detectors
	// so that multi-language seam sides carry their true language rather than
	// a hardcoded representative. Zero value ("") means "use default lang".
	type fileRef struct {
		file string
		line int
		lang Language // optional: set by HTTP/RPC detectors; "" → use caller default
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

		// HTTP route detection: Go, JS/TS, and Python producer/consumer patterns.
		// PRODUCER: server route registrations.
		// CONSUMER: client URL-literal call sites.
		switch f.Language {
		case LangGo:
			for _, m := range httpServerRouteRe.FindAllSubmatchIndex(content, -1) {
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
				httpRouteProducers[path] = append(httpRouteProducers[path], fileRef{file: f.Path, line: line, lang: LangGo})
			}
			for _, m := range httpClientCallRe.FindAllSubmatchIndex(content, -1) {
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
				httpRouteConsumers[path] = append(httpRouteConsumers[path], fileRef{file: f.Path, line: line, lang: LangGo})
			}
		case LangJavaScript, LangTypeScript:
			// PRODUCER: Express/Koa/Fastify style app.get('/path', handler) /
			// router.post('/path', handler). Requires a comma after the path
			// literal (handler arg) to distinguish from consumer fetch calls.
			for _, m := range httpJsRouteProducerRe.FindAllSubmatchIndex(content, -1) {
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
				httpRouteProducers[path] = append(httpRouteProducers[path], fileRef{file: f.Path, line: line, lang: f.Language})
			}
			// CONSUMER: fetch('/path') and axios/http client calls with a
			// leading-slash or full URL. Non-routable strings (cache keys,
			// map keys) are rejected by normalizeHTTPPath's leading-slash gate.
			// httpJsClientCallRe has 4 capture groups (fetch-sq, fetch-dq,
			// axios-sq, axios-dq); scan all pairs for the first non-empty one.
			for _, m := range httpJsClientCallRe.FindAllSubmatchIndex(content, -1) {
				raw := firstSubmatch(content, m)
				path := normalizeHTTPPath(raw)
				if path == "" {
					continue
				}
				line := lineForOffset(content, m[0])
				httpRouteConsumers[path] = append(httpRouteConsumers[path], fileRef{file: f.Path, line: line, lang: f.Language})
			}
		case LangPython:
			// PRODUCER: Flask @app.route('/path'), FastAPI @app.get('/path').
			// Both decorator forms require a leading-slash literal.
			for _, m := range httpPyRouteDecoratorRe.FindAllSubmatchIndex(content, -1) {
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
				httpRouteProducers[path] = append(httpRouteProducers[path], fileRef{file: f.Path, line: line, lang: LangPython})
			}
			// PRODUCER: Django path('accounts/login/', ...) — no leading slash
			// in source; normalizePyDjangoPath adds one for cross-language join.
			for _, m := range httpPyDjangoPathRe.FindAllSubmatchIndex(content, -1) {
				raw := string(content[m[2]:m[3]])
				path := normalizePyDjangoPath(raw)
				if path == "" {
					continue
				}
				line := lineForOffset(content, m[0])
				httpRouteProducers[path] = append(httpRouteProducers[path], fileRef{file: f.Path, line: line, lang: LangPython})
			}
			// CONSUMER: requests.get(url)/httpx.get(url) with a leading-slash
			// or full-URL literal. Mirrors Go's httpClientCallRe precision gate.
			for _, m := range httpPyClientCallRe.FindAllSubmatchIndex(content, -1) {
				raw := string(content[m[2]:m[3]])
				path := normalizeHTTPPath(raw)
				if path == "" {
					continue
				}
				line := lineForOffset(content, m[0])
				httpRouteConsumers[path] = append(httpRouteConsumers[path], fileRef{file: f.Path, line: line, lang: LangPython})
			}
		}

		// RPC method detection: Go + .proto + Python gRPC.
		// .proto files: PRODUCER declarations.
		// Go files: server handler PRODUCER funcs + call-site CONSUMER refs.
		// Python files: gRPC servicer PRODUCER declarations + stub CONSUMER calls.
		if isProto {
			for _, m := range protoRPCDeclRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcProducers[name] = append(rpcProducers[name], fileRef{file: f.Path, line: line, lang: LangOther})
			}
		} else if f.Language == LangGo {
			for _, m := range goRPCHandlerRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcProducers[name] = append(rpcProducers[name], fileRef{file: f.Path, line: line, lang: LangGo})
			}
			for _, m := range goRPCCallRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcConsumers[name] = append(rpcConsumers[name], fileRef{file: f.Path, line: line, lang: LangGo})
			}
		} else if f.Language == LangPython {
			// Python gRPC servicer method PRODUCER: tight signature
			// def MethodName(self, request, context) — exactly those three
			// parameters in order. This is the canonical gRPC-Python servicer
			// shape and is distinctive enough to be a precision anchor.
			// The producer-anchor gate at emission means a bare def matching
			// this pattern but with no .proto or Go handler pair is suppressed.
			for _, m := range pyRPCHandlerRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcProducers[name] = append(rpcProducers[name], fileRef{file: f.Path, line: line, lang: LangPython})
			}
			// Python gRPC stub CONSUMER: stub.MethodName(request).
			// Producer-anchor gate (enforced at emission) prevents flooding:
			// any lowercase_var.UpperMethod() matches this pattern, and without
			// a real producer it is noise, not a seam.
			for _, m := range pyRPCCallRe.FindAllSubmatchIndex(content, -1) {
				name := string(content[m[2]:m[3]])
				line := lineForOffset(content, m[0])
				rpcConsumers[name] = append(rpcConsumers[name], fileRef{file: f.Path, line: line, lang: LangPython})
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
	// fileRef slices. The gate for HTTP is: at least one side present and >=2
	// distinct files when both sides are non-empty (self-references suppressed).
	// For RPC the caller enforces an additional pre-condition: producers must be
	// non-empty (a real contract — .proto rpc declaration or gRPC server handler
	// — must exist before consumer call sites are treated as seam evidence).
	// Sides are sorted by File for determinism.
	reduceProducerConsumer := func(kind SeamKind, key string, defaultLang Language, producers, consumers []fileRef) *Seam {
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
				sideL := defaultLang
				if r.lang != "" {
					sideL = r.lang
				}
				sides = append(sides, SeamSide{File: r.file, Language: sideL, Line: r.line})
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
				sideL := defaultLang
				if r.lang != "" {
					sideL = r.lang
				}
				sides = append(sides, SeamSide{File: r.file, Language: sideL, Line: r.line})
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
		// Producer-anchor gate: skip methods with no real contract declaration.
		// A call site matching goRPCCallRe without a .proto rpc declaration or
		// genuine gRPC handler is indistinguishable from an ordinary Go method
		// call (db.ExecContext, repo.UpsertFinding, …). With no producer the
		// consumer evidence is noise, not a seam.
		if len(rpcProducers[k]) == 0 {
			continue
		}
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

// goRPCHandlerRe matches Go gRPC unary server handler method declarations.
// A genuine gRPC unary handler has: receiver ending in "Server", an
// uppercase method name, ctx as the first parameter, a second pointer
// request parameter, and a (pointer response, error) return list.
// Methods with only one parameter (e.g. Serve(ctx context.Context) error)
// do NOT match, preventing control-plane methods from registering as RPC
// producers. Captured group 1 is the method name.
var goRPCHandlerRe = regexp.MustCompile(`func\s*\([^)]*Server[^)]*\)\s+([A-Z][A-Za-z0-9_]*)\s*\(\s*ctx\b[^)]*,\s*\*[A-Za-z][A-Za-z0-9_]*\s*\)\s*\(\s*\*[A-Za-z]`)

// goRPCCallRe matches Go gRPC client call sites of the form:
//
//	someClient.MethodName(ctx, ...)
//
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

// firstSubmatch returns the string content of the first non-empty captured
// group in a FindAllSubmatchIndex result row m (which pairs [start, end] for
// the whole match at m[0:2] and each group at m[2i:2i+1]). Returns "" when
// no group captured anything. Used when a regex has multiple alternation
// branches each with one capture group (the active branch wins).
func firstSubmatch(content []byte, m []int) string {
	for i := 2; i+1 < len(m); i += 2 {
		if m[i] >= 0 {
			return string(content[m[i]:m[i+1]])
		}
	}
	return ""
}

// httpJsRouteProducerRe matches JS/TS server-side HTTP route registrations for
// Express, Koa, and Fastify. The pattern requires:
//  1. A method receiver (any identifier) followed by .get/.post/.put/.delete/.patch
//  2. A string literal (single- or double-quoted) with a leading slash as the first arg
//  3. A COMMA immediately after the path literal, proving a handler arg follows
//     (this is the critical distinguisher from consumer fetch/axios calls).
//
// Captured groups: group 1 for single-quoted paths, group 2 for double-quoted paths.
var httpJsRouteProducerRe = regexp.MustCompile(`(?:\.get|\.post|\.put|\.delete|\.patch)\s*\(\s*(?:'(/[^'\x00-\x1f]*)'|"(/[^"\x00-\x1f]*)")\s*,`)

// httpJsClientCallRe matches JS/TS client HTTP calls. Patterns:
//  1. fetch('/path') or fetch("https://host/path") — the built-in fetch API
//  2. axios.get('/path') / axios.post('/path') etc — axios client
//
// A leading slash (or full URL whose path normalizeHTTPPath extracts) is
// required; non-routable strings (cache keys, map keys) are rejected by
// normalizeHTTPPath's leading-slash gate, mirroring the Go detector's gate.
//
// Captured groups: group 1 (single-quote fetch), group 2 (double-quote fetch),
// group 3 (single-quote axios), group 4 (double-quote axios).
var httpJsClientCallRe = regexp.MustCompile(`(?:(?:\bfetch\s*\(\s*(?:'((?:https?://[^']*|/[^']*))'|"((?:https?://[^"]*|/[^"]*))")\s*[,)])|(?:\baxios\.(?:get|post|put|delete|patch)\s*\(\s*(?:'((?:https?://[^']*|/[^']*))'|"((?:https?://[^"]*|/[^"]*))")\s*[,)]))`)

// httpPyRouteDecoratorRe matches Python Flask and FastAPI HTTP route
// decorator/call forms:
//  1. Flask:   @app.route('/path') or @bp.route('/path')
//  2. FastAPI: @app.get('/path') / @app.post('/path') etc.
//
// The decorator must include a leading-slash path literal (single- or
// double-quoted). Captured groups: group 1 for single-quoted, group 2 for
// double-quoted.
var httpPyRouteDecoratorRe = regexp.MustCompile(`@\w+\.(?:route|get|post|put|delete|patch)\s*\(\s*(?:'(/[^'\x00-\x1f]*)'|"(/[^"\x00-\x1f]*)")`)

// httpPyDjangoPathRe matches Django urlpatterns path() calls:
//
//	path('accounts/login/', ...)
//	path("api/v1/users/", ...)
//
// Django paths do NOT have a leading slash; normalizePyDjangoPath prepends
// one for cross-language join consistency. The pattern requires a comma
// after the path literal (view arg follows) to avoid matching ordinary
// string arguments in other call sites. Captured group 1 is the raw path.
var httpPyDjangoPathRe = regexp.MustCompile(`\bpath\(\s*(?:'([^'\x00-\x1f]*)'|"([^"\x00-\x1f]*)")\s*,`)

// httpPyClientCallRe matches Python HTTP client URL-literal call sites.
// Libraries: requests, httpx, aiohttp (via session.get). The first arg
// must be a string literal with a leading slash or full URL; non-routable
// strings are rejected by normalizeHTTPPath's leading-slash gate.
// Captured group 1 (single-quote) or group 2 (double-quote) is the URL/path.
var httpPyClientCallRe = regexp.MustCompile(`\b(?:requests|httpx|session)\.(?:get|post|put|delete|patch)\s*\(\s*(?:'((?:https?://[^']*|/[^']*))'|"((?:https?://[^"]*|/[^"]*))"\s*)`)

// pyRPCHandlerRe matches Python gRPC servicer method declarations. The
// canonical gRPC-Python servicer method has exactly three parameters:
// self, request, and context — in that order. This tight signature
// prevents ordinary instance methods (self, arg) from matching.
// Captured group 1 is the method name (must be uppercase, matching proto
// conventions).
var pyRPCHandlerRe = regexp.MustCompile(`def\s+([A-Z][A-Za-z0-9_]*)\s*\(\s*self\s*,\s*request\s*,\s*context\s*\)`)

// pyRPCCallRe matches Python gRPC stub call sites of the form:
//
//	stub.MethodName(request)
//
// where MethodName begins with an uppercase letter (matching proto/gRPC
// conventions). The receiver must be a lowercase-starting identifier.
// This pattern is intentionally broad; the producer-anchor gate in the
// emission step is the precision control (consumer-only evidence is never
// emitted as a seam, mirroring the Go RPC flood lesson).
var pyRPCCallRe = regexp.MustCompile(`\b[a-z][A-Za-z0-9_]*\.([A-Z][A-Za-z0-9_]*)\s*\(`)

// normalizePyDjangoPath normalizes a Django path() string to a routable
// path with a leading slash, consistent with normalizeHTTPPath's output
// format. Django path patterns have no leading slash in source (e.g.
// "accounts/login/"); this function prepends one so that a Django producer
// and a fetch("/accounts/login/") consumer join on the same key.
//
// Returns "" for the empty string or a string that is already a full URL
// (which normalizeHTTPPath should handle instead). The "/" root path is
// rejected as too generic, matching normalizeHTTPPath's gate.
func normalizePyDjangoPath(raw string) string {
	if raw == "" {
		return ""
	}
	// If it already has a scheme, delegate to the standard normalizer.
	if strings.Contains(raw, "://") {
		return normalizeHTTPPath(raw)
	}
	// Strip query and fragment.
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return ""
	}
	// Prepend leading slash if absent.
	if raw[0] != '/' {
		raw = "/" + raw
	}
	// Strip trailing slash for join consistency (Django paths often end with /).
	// Exception: don't strip if raw is exactly "/".
	if len(raw) > 1 && raw[len(raw)-1] == '/' {
		raw = raw[:len(raw)-1]
	}
	if raw == "/" || raw == "" {
		return ""
	}
	return raw
}
