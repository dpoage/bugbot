// Package miner seeds the leads blackboard with documented-sentinel-vs-validator
// contradictions mined from source files in the target repository.
//
// The motivating miss (bugbot-ig7): codebases routinely document sentinel-value
// semantics in comments — "0 = unlimited", "empty = all", "negative disables" —
// while the corresponding validator rejects exactly the value the doc defines
// as meaningful. These contradictions are lexically self-revealing: two greps
// find them in seconds, but 45M input tokens of LLM finder search missed one.
// The miner recovers that recall by walking every in-scope source file twice —
// first to extract documented sentinels, then to extract validation sites —
// and joining the two sides on the named entity.
//
// Seed is a pure-Go, in-process pre-pass. It needs no sandbox, no container,
// and no LLM call: it reads each file from disk, runs two deterministic regex
// sweeps over the text, and posts the joined contradictions to the leads table
// via store.AddLead. Because it has no runtime requirements it is always-on
// (no config knob in v1) and runs alongside the analyzer seed step in the
// scan command and the daemon loop.
//
// File-system failures (missing file, permission denied, size cap exceeded)
// are skipped best-effort. Store errors are propagated. The Summary is
// always populated, even on error.
//
// The miner is precision-biased: a documented-sentinel-vs-validator lead
// that is wrong is worse than a missed lead, because the leads table feeds
// straight into the verifier stage. The join is therefore deliberately
// tight: entity must be present on BOTH sides, the documented sentinelClass
// must match the value the constraintClass rejects, and generic short
// identifiers are rejected as entities to suppress spurious joins.
//
// Leads are posted with TargetLens="api-contract-misuse" and
// PosterLens="miner:doc-contradiction" so the leads blackboard can
// attribute a lead back to this pass.
package miner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// maxLeads caps the total leads a single Seed run can post. Mirrors the
// analyzer's maxResultsPerAnalyzer.
const maxLeads = 200

// maxFileBytes caps each file the miner reads. A 1 MiB cap is generous for
// source files.
const maxFileBytes = 1 << 20

const (
	posterLens   = "miner:doc-contradiction"
	targetLens   = "api-contract-misuse"
	noteMaxLen   = 500
	minEntityLen = 3
)

var genericEntityStoplist = map[string]bool{
	"err": true, "nil": true, "ok": true, "ctx": true, "tmp": true,
	"val": true, "buf": true, "num": true, "cnt": true, "ret": true,
	"src": true, "dst": true, "max": true, "min": true, "len": true,
	"cur": true, "key": true, "row": true, "col": true, "pos": true,
	"end": true, "sum": true, "line": true, "rows": true,
	"idx": true, "off": true, "str": true, "data": true,
	"i": true, "j": true, "k": true, "n": true, "v": true, "x": true,
	"y": true, "r": true, "w": true, "rw": true, "id": true, "fd": true,
	"in": true, "go": true,
	"the": true, "and": true, "for": true, "but": true, "not": true,
	"may": true, "can": true, "all": true, "any": true, "one": true,
	"two": true, "new": true, "old": true, "now": true, "yet": true,
	"bad": true, "use": true, "via": true, "per": true, "set": true,
	"get": true, "put": true, "run": true, "see": true, "few": true,
	"raw":   true,
	"empty": true, "unlimited": true, "limit": true, "disabled": true,
	"negative": true, "zero": true, "value": true, "human": true,
	"must": true, "files": true, "findings": true, "lenses": true,
	"candidates": true, "observations": true,
}

// sentinelClass classifies a documented sentinel value in a comment.
// The zero value ("") means "not a sentinel" and is used as a negative signal.
// All other valid values are one of the three named constants below.
type sentinelClass string

const (
	sentinelZero     sentinelClass = "zeroMeaningful"
	sentinelNegative sentinelClass = "negativeMeaningful"
	sentinelEmpty    sentinelClass = "emptyMeaningful"
)

// Valid reports whether s is one of the three known sentinel classes.
// The zero value ("") is not valid — it signals "no sentinel found".
func (s sentinelClass) Valid() bool {
	switch s {
	case sentinelZero, sentinelNegative, sentinelEmpty:
		return true
	}
	return false
}

// constraintClass classifies what a validation site rejects.
// All valid values are one of the three named constants below.
type constraintClass string

const (
	constraintRejectsZero     constraintClass = "rejectsZero"
	constraintRejectsNegative constraintClass = "rejectsNegative"
	constraintRejectsEmpty    constraintClass = "rejectsEmpty"
)

// Valid reports whether c is one of the three known constraint classes.
func (c constraintClass) Valid() bool {
	switch c {
	case constraintRejectsZero, constraintRejectsNegative, constraintRejectsEmpty:
		return true
	}
	return false
}

type docSite struct {
	entity    string
	sClass    sentinelClass
	docFile   string
	docLine   int
	entities  []string
	docPhrase string
}

type constraintSite struct {
	entity     string
	cClass     constraintClass
	codeFile   string
	codeLine   int
	codePhrase string
	entities   []string
}

type Summary struct {
	DocSites        int
	ConstraintSites int
	LeadsPosted     int
	// EnumDriftLeads counts leads from the enum/const-drift pass
	// (switch cases using raw integer literals instead of named constants).
	EnumDriftLeads int
	// StringlyDriftLeads counts leads from the Go stringly-typed drift pass
	// (type X string; const values vs raw switch case literals — Go only).
	StringlyDriftLeads int
	// StringlyTSDriftLeads counts leads from the TypeScript string-union drift
	// pass (type Alias = 'a' | 'b' | ... vs switch case literals — TS only).
	StringlyTSDriftLeads int
	// TSParseFailures counts TS files skipped because the gotreesitter grammar
	// produced a parse tree with HasError()=true (known gap: typed-param arrow
	// functions). Exposed so callers and tests can audit grammar coverage.
	TSParseFailures int
}

type leadKey struct {
	TargetLens string
	File       string
	Line       int
}

func Seed(ctx context.Context, snap *ingest.Snapshot, st *store.Store) (Summary, error) {
	var sum Summary
	if snap == nil {
		return sum, fmt.Errorf("miner: nil snapshot")
	}
	if st == nil {
		return sum, fmt.Errorf("miner: nil store")
	}

	var docs []docSite
	var cons []constraintSite
	for _, f := range snap.Files {
		if !minerLang(f.Language) {
			continue
		}
		fdocs, fcons, err := mineFile(snap.Root, f)
		if err != nil {
			continue
		}
		docs = append(docs, fdocs...)
		cons = append(cons, fcons...)
	}
	sum.DocSites = len(docs)
	sum.ConstraintSites = len(cons)

	seen := make(map[leadKey]bool)
	type pending struct {
		c constraintSite
		d docSite
	}
	var leads []pending
consLoop:
	for _, c := range cons {
		cEnts := expandEntities(c.entity)
		for _, e := range c.codePhraseEntities() {
			cEnts = appendUnique(cEnts, e)
		}
		for _, d := range docs {
			if !sentinelContradictsDoc(d.sClass, c.cClass) {
				continue
			}
			if !entityOverlap(d.entities, cEnts) {
				continue
			}
			k := leadKey{
				TargetLens: targetLens,
				File:       c.codeFile,
				Line:       c.codeLine,
			}
			if seen[k] {
				continue consLoop
			}
			seen[k] = true
			leads = append(leads, pending{c: c, d: d})
			continue consLoop
		}
	}

	if len(leads) > maxLeads {
		leads = leads[:maxLeads]
	}

	for _, p := range leads {
		note := buildNote(p.d, p.c)
		if err := st.AddLead(ctx, store.Lead{
			PosterLens: posterLens,
			TargetLens: targetLens,
			File:       p.c.codeFile,
			Line:       p.c.codeLine,
			Note:       note,
		}); err != nil {
			return sum, fmt.Errorf("miner: add lead for %s:%d: %w", p.c.codeFile, p.c.codeLine, err)
		}
		sum.LeadsPosted++
	}

	if err := seedEnumDrift(ctx, snap, st, &sum); err != nil {
		return sum, err
	}

	if err := seedConfigFieldContradictions(ctx, snap, st, &sum); err != nil {
		return sum, err
	}

	if err := seedStringlyDrift(ctx, snap, st, &sum); err != nil {
		return sum, err
	}

	if err := seedStringlyTSDrift(ctx, snap, st, &sum); err != nil {
		return sum, err
	}

	return sum, nil
}

func minerLang(l ingest.Language) bool {
	switch l {
	case ingest.LangGo, ingest.LangPython, ingest.LangJavaScript, ingest.LangTypeScript,
		ingest.LangCPP, ingest.LangC, ingest.LangRust, ingest.LangJava, ingest.LangRuby:
		return true
	}
	return false
}

func mineFile(root string, f ingest.File) ([]docSite, []constraintSite, error) {
	abs := filepath.Join(root, filepath.FromSlash(f.Path))
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}
	if fi.Size() > maxFileBytes {
		return nil, nil, fmt.Errorf("file too large: %d bytes", fi.Size())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, err
	}
	docs := passDocumented(f.Path, string(data))
	cons := passEnforced(f.Path, string(data))
	return docs, cons, nil
}

func passDocumented(path, content string) []docSite {
	var out []docSite
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), maxFileBytes)
	lines := scanAllLines(scanner)
	for i, line := range lines {
		comment := stripComment(line)
		if comment == "" {
			continue
		}
		cls := docSentinelClass(comment)
		if cls == "" {
			continue
		}
		entity, entLine := entityForComment(lines, i, comment)
		if entity == "" {
			continue
		}
		if !isPlausibleEntity(entity) {
			continue
		}
		ents := expandEntities(entity)
		for _, k := range extractSnakeKeys(comment) {
			ents = appendUnique(ents, k)
		}
		out = append(out, docSite{
			entity:    entity,
			sClass:    cls,
			docFile:   path,
			docLine:   entLine,
			entities:  ents,
			docPhrase: truncate(comment, 80),
		})
	}
	return out
}

// docSentinelClass inspects a comment line and reports which sentinelClass
// the line documents, if any. The check is order-dependent: a single
// comment may match several phrases (e.g. "Zero means unlimited, no
// limit"); the first class that fits wins. Order matters: empty is
// checked first because "empty = all" is a common phrase that should
// not be misclassified as a zeroMeaningful doc.
func docSentinelClass(comment string) sentinelClass {
	lc := strings.ToLower(comment)
	switch {
	case strings.Contains(lc, "empty") && (strings.Contains(lc, "all") || strings.Contains(lc, "= all")):
		return sentinelEmpty
	case strings.Contains(lc, "negative") || strings.HasPrefix(strings.TrimSpace(lc), "-1"):
		return sentinelNegative
	case strings.Contains(lc, "zero") || strings.HasPrefix(strings.TrimSpace(lc), "0") ||
		regexp.MustCompile(`\b0\s*=\s*unlimited`).MatchString(lc):
		return sentinelZero
	}
	return ""
}

// entityForComment returns the identifier a comment line "attaches to".
// Trailing comments use the ident on the same line; leading comments use
// the ident on the next non-blank line. There is no fallback to comment
// text: a doc-only ident ("Negative disables" with no surrounding code)
// is rejected so it cannot match unrelated validators.
func entityForComment(lines []string, commentIdx int, comment string) (string, int) {
	if ent, ok := trailingCommentIdent(lines[commentIdx]); ok && isPlausibleEntity(ent) {
		return ent, commentIdx + 1
	}
	for j := commentIdx + 1; j < len(lines) && j <= commentIdx+5; j++ {
		next := strings.TrimSpace(lines[j])
		if next == "" {
			continue
		}
		if ent := firstIdent(next); ent != "" && isPlausibleEntity(ent) {
			return ent, j + 1
		}
		break
	}
	return "", 0
}

func scanAllLines(scanner *bufio.Scanner) []string {
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

var constraintMsgPatterns = []struct {
	re  *regexp.Regexp
	cls constraintClass
}{
	{regexp.MustCompile(`(?i)\bmust\s+be\s+>\s*0\b`), constraintRejectsZero},
	{regexp.MustCompile(`(?i)\bmust\s+be\s+positive\b`), constraintRejectsZero},
	{regexp.MustCompile(`(?i)\bmust\s+be\s+>=\s*1\b`), constraintRejectsZero},
	{regexp.MustCompile(`(?i)\bmust\s+not\s+be\s+empty\b`), constraintRejectsEmpty},
	{regexp.MustCompile(`(?i)\bmust\s+be\s+non-?empty\b`), constraintRejectsEmpty},
	{regexp.MustCompile(`(?i)\bcannot\s+be\s+empty\b`), constraintRejectsEmpty},
	{regexp.MustCompile(`(?i)\bmust\s+be\s+>=\s*0\b`), constraintRejectsNegative},
}

var guardPatterns = []struct {
	re             *regexp.Regexp
	cls            constraintClass
	requiresReturn bool
}{
	{regexp.MustCompile(`\bif\s+([A-Za-z_][A-Za-z0-9_]*)\s*<=\s*0\s*\{`), constraintRejectsZero, true},
	{regexp.MustCompile(`\bif\s+([A-Za-z_][A-Za-z0-9_]*)\s*==\s*0\s*\{`), constraintRejectsZero, true},
	{regexp.MustCompile(`\bif\s+([A-Za-z_][A-Za-z0-9_]*)\s*<\s*1\s*\{`), constraintRejectsZero, true},
	{regexp.MustCompile(`\bif\s+len\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)\s*==\s*0\s*\{`), constraintRejectsEmpty, true},
	{regexp.MustCompile(`\bif\s+([A-Za-z_][A-Za-z0-9_]*)\s*==\s*""\s*\{`), constraintRejectsEmpty, true},
	{regexp.MustCompile(`\bif\s+([A-Za-z_][A-Za-z0-9_]*)\s*<\s*0\s*\{`), constraintRejectsNegative, true},
}

func passEnforced(path, content string) []constraintSite {
	var out []constraintSite
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), maxFileBytes)
	lines := scanAllLines(scanner)
	for i, line := range lines {
		for _, p := range constraintMsgPatterns {
			if loc := p.re.FindStringIndex(line); loc != nil {
				_ = loc
				entity := extractEntityFromErrorString(line)
				if !isPlausibleEntity(entity) {
					continue
				}
				ents := expandEntities(entity)
				out = append(out, constraintSite{
					entity:     entity,
					cClass:     p.cls,
					codeFile:   path,
					codeLine:   i + 1,
					codePhrase: truncate(line, 80),
					entities:   ents,
				})
			}
		}
		for _, g := range guardPatterns {
			m := g.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			entity := m[1]
			if !isPlausibleEntity(entity) {
				continue
			}
			if g.requiresReturn && !hasEarlyReturn(lines, i) {
				continue
			}
			ents := expandEntities(entity)
			out = append(out, constraintSite{
				entity:     entity,
				cClass:     g.cls,
				codeFile:   path,
				codeLine:   i + 1,
				codePhrase: truncate(line, 80),
				entities:   ents,
			})
		}
	}
	return out
}

func hasEarlyReturn(lines []string, from int) bool {
	end := from + 6
	if end > len(lines) {
		end = len(lines)
	}
	for _, l := range lines[from:end] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "return ") || t == "return" ||
			strings.HasPrefix(t, "return(errors") || strings.Contains(t, "return fmt.Errorf") ||
			strings.HasPrefix(t, "return err") ||
			strings.HasPrefix(t, "panic(") ||
			strings.HasPrefix(t, "os.Exit(") {
			return true
		}
	}
	return false
}

func extractEntityFromErrorString(snippet string) string {
	if m := snakeKeyRe.FindString(snippet); m != "" {
		return m
	}
	return firstIdent(snippet)
}

var snakeKeyRe = regexp.MustCompile(`\b([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\b`)

func extractSnakeKeys(s string) []string {
	matches := snakeKeyRe.FindAllString(s, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]bool)
	for _, m := range matches {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func sentinelContradictsDoc(d sentinelClass, c constraintClass) bool {
	// Guard: both classes must be valid. The zero value of sentinelClass ("")
	// means "no sentinel found" and should never reach this function; similarly
	// an unknown constraintClass would be a programming error.
	if !d.Valid() || !c.Valid() {
		return false
	}
	switch d {
	case sentinelZero:
		return c == constraintRejectsZero
	case sentinelNegative:
		return c == constraintRejectsNegative
	case sentinelEmpty:
		return c == constraintRejectsEmpty
	}
	return false
}

func entityOverlap(docs, cons []string) bool {
	if len(docs) == 0 || len(cons) == 0 {
		return false
	}
	consNorm := make(map[string]bool, len(cons))
	for _, c := range cons {
		for _, k := range normalizeEntityKey(c) {
			consNorm[k] = true
		}
	}
	for _, d := range docs {
		for _, k := range normalizeEntityKey(d) {
			if consNorm[k] {
				return true
			}
		}
	}
	return false
}

func normalizeEntityKey(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := []string{s}
	out = appendUnique(out, camelToSnake(s))
	out = appendUnique(out, snakeToCamel(s))
	return out
}

func camelToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := rune(s[i-1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func snakeToCamel(s string) string {
	if s == "" || !strings.Contains(s, "_") {
		return s
	}
	parts := strings.Split(s, "_")
	var b strings.Builder
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			b.WriteString(strings.ToLower(p))
			continue
		}
		runes := []rune(p)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	return b.String()
}

func expandEntities(entity string) []string {
	return normalizeEntityKey(entity)
}

func (c constraintSite) codePhraseEntities() []string {
	return extractSnakeKeys(c.codePhrase)
}

func appendUnique(out []string, s string) []string {
	for _, e := range out {
		if e == s {
			return out
		}
	}
	return append(out, s)
}

func buildNote(d docSite, c constraintSite) string {
	note := fmt.Sprintf("doc at %s:%d says %s %s=unlimited, but validator at %s:%d rejects %s",
		d.docFile, d.docLine, d.entity, sentinelVerb(d.sClass),
		c.codeFile, c.codeLine, c.entity)
	return truncate(note, noteMaxLen)
}

func sentinelVerb(c sentinelClass) string {
	switch c {
	case sentinelZero:
		return "0"
	case sentinelNegative:
		return "negative"
	case sentinelEmpty:
		return "empty"
	}
	return "?"
}

func stripComment(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "//") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
	}
	if strings.HasPrefix(trimmed, "#") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
	}
	return ""
}

func trailingCommentIdent(line string) (string, bool) {
	idx := -1
	for _, marker := range []string{"//", "#"} {
		if i := strings.Index(line, marker); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		return "", false
	}
	code := strings.TrimSpace(line[:idx])
	if code == "" {
		return "", false
	}
	ent := firstIdent(code)
	if ent == "" {
		return "", false
	}
	return ent, true
}

func firstIdent(s string) string {
	for i, r := range s {
		if !unicode.IsLetter(r) && r != '_' {
			continue
		}
		for j := i; j < len(s); j++ {
			rj := rune(s[j])
			if !unicode.IsLetter(rj) && !unicode.IsDigit(rj) && rj != '_' {
				return s[i:j]
			}
		}
		return s[i:]
	}
	return ""
}

func isPlausibleEntity(s string) bool {
	if len(s) < minEntityLen {
		return false
	}
	if genericEntityStoplist[strings.ToLower(s)] {
		return false
	}
	if isGoKeyword(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func isGoKeyword(s string) bool {
	switch s {
	case "if", "else", "for", "range", "return", "func", "var", "const",
		"type", "struct", "interface", "package", "import", "nil", "true",
		"false", "switch", "case", "default", "break", "continue", "goto",
		"defer", "select", "map", "chan":
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// ============================================================================
// Enum / const-drift pass
// ============================================================================
//
// Motivation: Go code frequently defines a set of named integer constants
// (either an iota block or explicit assignments) and later uses raw integer
// literals in switch cases instead of the named constants. This "magic
// literal" pattern creates a maintenance hazard: if constants are reordered
// the switch silently handles the wrong case. It is also an api-contract-
// misuse: the contract is "use the named constant", and the call site
// violates it by embedding the raw numeric value.
//
// Detection algorithm (per file, single-pass join):
//  1. passConstDecls: regex-scan for `const NAME = <int>` lines and
//     `iota` const blocks. Build a map of integer value → constant name.
//     Only non-generic names pass isPlausibleEntity.
//  2. passSwitchCaseLiterals: regex-scan for `case <int>:` lines.
//     Record the integer value and source location.
//  3. Join: if a case literal matches a declared constant's value AND the
//     constant name is plausible, emit a drift lead.
//
// Precision guards:
//   - Constant names must pass isPlausibleEntity (len ≥ 3, not in stoplist,
//     no Go keywords).
//   - Literal value must be non-negative and ≤ 255 (above 255 the chance of
//     accidental collision with a real protocol constant rises sharply).
//   - Only one lead per (file, line) pair (seen map dedup).
//   - Files with no const declarations matching the pattern are skipped fast.

const (
	enumDriftPosterLens = "miner:enum-const-drift"
	enumDriftTargetLens = "api-contract-misuse"

	// maxDriftLiteral caps the integer range we consider. Values above this
	// threshold are likely protocol constants or offsets, not enum indices,
	// reducing false positives.
	maxDriftLiteral = 255
)

// constDecl holds one named integer constant extracted from source.
type constDecl struct {
	name  string
	value int64
	file  string
	line  int
}

// caseLiteral holds one raw integer case literal found in a switch block.
type caseLiteral struct {
	value int64
	file  string
	line  int
}

// constDeclRe matches `<name> = <int>` or `<name> <type> = <int>` const
// declarations. Also matches iota lines with an explicit value comment
// convention. We do NOT match iota itself — we match explicit integer RHS only.
//
// Examples matched:
//
//	StatusOK    = 0
//	StatusError = 2
//	ModeRead    HTTPMethod = 3
var constDeclRe = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9_]*)(?:\s+[A-Za-z][A-Za-z0-9_]*)?\s*=\s*([0-9]+)\s*(?://.*)?$`)

// iotaDeclRe matches iota lines in a const block. Captures the name so we
// can assign the sequential iota value (starting at 0 within a block).
// We only match iota-without-offset (bare `= iota`); shift/mask iota
// expressions are excluded to avoid precision errors.
var iotaDeclRe = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9_]*)\s*(?:[A-Za-z][A-Za-z0-9_]*)?\s*=\s*iota\b`)

// iotaContinuationRe matches continuation names in an iota block (subsequent
// lines after `= iota` that implicitly increment). Only uppercase leading char
// (exported constants).
var iotaContinuationRe = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9_]*)\s*(?:[A-Za-z][A-Za-z0-9_]*)?\s*$`)

// caseLiteralRe matches `case <int>:` lines in switch blocks.
var caseLiteralRe = regexp.MustCompile(`\bcase\s+([0-9]+)\s*:`)

// passConstDecls extracts named integer constants from Go source.
func passConstDecls(path, content string) []constDecl {
	var out []constDecl
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), maxFileBytes)
	lines := scanAllLines(scanner)

	inConst := false // inside a const ( ) block
	inIota := false  // inside an iota sub-block
	iotaVal := int64(0)

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect const block entry/exit.
		if strings.HasPrefix(trimmed, "const (") || trimmed == "const (" {
			inConst = true
			inIota = false
			iotaVal = 0
			continue
		}
		if inConst && trimmed == ")" {
			inConst = false
			inIota = false
			iotaVal = 0
			continue
		}

		// Single-line const (outside block): `const NAME = <int>`
		// Strip the leading `const ` keyword so constDeclRe (which expects
		// `NAME = value`) can match correctly.
		if !inConst {
			stripped := strings.TrimPrefix(strings.TrimSpace(line), "const ")
			if m := constDeclRe.FindStringSubmatch(stripped); m != nil {
				name := m[1]
				val, err := strconv.ParseInt(m[2], 10, 64)
				if err == nil && val >= 0 && val <= maxDriftLiteral && isPlausibleEntity(name) {
					out = append(out, constDecl{name: name, value: val, file: path, line: i + 1})
				}
			}
			continue
		}

		// Inside a const block: detect iota start.
		if m := iotaDeclRe.FindStringSubmatch(line); m != nil {
			inIota = true
			iotaVal = 0
			name := m[1]
			if isPlausibleEntity(name) {
				out = append(out, constDecl{name: name, value: iotaVal, file: path, line: i + 1})
			}
			iotaVal++
			continue
		}

		if inIota {
			// Blank or comment line resets iota tracking (conservative).
			if trimmed == "" || strings.HasPrefix(trimmed, "//") {
				continue
			}
			// Check if it's a continuation (no explicit = sign).
			if !strings.Contains(trimmed, "=") {
				if m := iotaContinuationRe.FindStringSubmatch(line); m != nil {
					name := m[1]
					if isPlausibleEntity(name) && iotaVal <= maxDriftLiteral {
						out = append(out, constDecl{name: name, value: iotaVal, file: path, line: i + 1})
					}
					iotaVal++
					continue
				}
				// Not a continuation — stop iota tracking.
				inIota = false
			} else {
				// Explicit = inside a const block: check for plain integer.
				if m := constDeclRe.FindStringSubmatch(line); m != nil {
					name := m[1]
					val, err := strconv.ParseInt(m[2], 10, 64)
					if err == nil && val >= 0 && val <= maxDriftLiteral && isPlausibleEntity(name) {
						out = append(out, constDecl{name: name, value: val, file: path, line: i + 1})
					}
				}
				inIota = false // explicit = breaks iota sequence
			}
			continue
		}

		// Inside const block, not in iota: look for explicit assignments.
		if m := constDeclRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			val, err := strconv.ParseInt(m[2], 10, 64)
			if err == nil && val >= 0 && val <= maxDriftLiteral && isPlausibleEntity(name) {
				out = append(out, constDecl{name: name, value: val, file: path, line: i + 1})
			}
		}
	}
	return out
}

// passSwitchCaseLiterals extracts raw integer case literals from switch blocks.
func passSwitchCaseLiterals(path, content string) []caseLiteral {
	var out []caseLiteral
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), maxFileBytes)
	lines := scanAllLines(scanner)
	for i, line := range lines {
		m := caseLiteralRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || val < 0 || val > maxDriftLiteral {
			continue
		}
		out = append(out, caseLiteral{value: val, file: path, line: i + 1})
	}
	return out
}

// seedEnumDrift runs the enum/const-drift pass over the snapshot and posts
// leads. It is called from Seed after the doc-contradiction pass. Store errors
// are returned; file errors are skipped best-effort (matching mineFile).
func seedEnumDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	seen := make(map[leadKey]bool)

	for _, f := range snap.Files {
		if !minerLang(f.Language) {
			continue
		}
		abs := filepath.Join(snap.Root, filepath.FromSlash(f.Path))
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if fi.Size() > maxFileBytes {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		content := string(data)

		decls := passConstDecls(f.Path, content)
		if len(decls) == 0 {
			continue // fast path: no matching consts in this file
		}
		cases := passSwitchCaseLiterals(f.Path, content)
		if len(cases) == 0 {
			continue
		}

		// Build a lookup: value → const name (first-declared wins for dedup).
		valToConst := make(map[int64]constDecl, len(decls))
		for _, d := range decls {
			if _, exists := valToConst[d.value]; !exists {
				valToConst[d.value] = d
			}
		}

		for _, c := range cases {
			cd, ok := valToConst[c.value]
			if !ok {
				continue
			}
			k := leadKey{TargetLens: enumDriftTargetLens, File: c.file, Line: c.line}
			if seen[k] {
				continue
			}
			seen[k] = true

			note := fmt.Sprintf(
				"enum-drift: switch case uses raw literal %d (file %s, line %d) "+
					"where named constant %s (declared at %s:%d) should be used; "+
					"reordering constants silently breaks this case",
				c.value, c.file, c.line, cd.name, cd.file, cd.line,
			)
			note = truncate(note, noteMaxLen)

			if err := st.AddLead(ctx, store.Lead{
				PosterLens: enumDriftPosterLens,
				TargetLens: enumDriftTargetLens,
				File:       c.file,
				Line:       c.line,
				Note:       note,
			}); err != nil {
				return fmt.Errorf("miner: enum-drift lead %s:%d: %w", c.file, c.line, err)
			}
			sum.EnumDriftLeads++
			sum.LeadsPosted++
			if sum.LeadsPosted >= maxLeads {
				return nil
			}
		}
	}
	return nil
}
