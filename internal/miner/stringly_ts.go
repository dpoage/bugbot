package miner

// TypeScript string-union drift miner.
//
// # Design decision: tree-sitter queries, not regex lexers
//
// The Go stringly miner (stringly.go) encodes Go lexical structure in pure
// regex and required ~15 oracle fix rounds to handle rune literals, backtick
// raw strings, single-line block comments, and := shadow scoping. Porting the
// same regex approach to TypeScript would inherit the same fragility — TS adds
// template literals, optional chaining, JSX quasi-quotations, and type
// annotations that require AST-level understanding.
//
// Tree-sitter is the right tool here:
//   - It parses at the AST level, so string literals in comments, template
//     expressions, or JSX attributes never pollute case-arm or type-member
//     extraction.
//   - The gotreesitter library (already used by internal/treesitter) exposes
//     NewQuery + Query.Execute(tree) for arbitrary per-file queries.
//   - Adding a new language is a new query table entry with no new
//     parse-state machinery.
//
// The miner imports gotreesitter directly (the same dependency that
// internal/treesitter already imports) to run per-file queries without
// triggering the Backend's full-repository walk.
//
// # Detection algorithm (file-local, type-anchored)
//
//  1. passTS_UnionTypes: use two tree-sitter queries.
//     (a) Find all type_alias_declaration nodes with their name and byte range.
//     (b) Find all literal_type(string) nodes; associate each with the
//         innermost enclosing type_alias_declaration by containment.
//     (c) Find all literal_type with non-string children (number, boolean, etc.)
//         and mark those type names as mixed unions — excluded from analysis.
//     Only pure-string unions with ≥2 members are kept.
//
//  2. passTS_FunctionParams: find all function/method/arrow-function parameters
//     with explicit type annotations matching one of the union types found in
//     step 1. Returns slice of {funcStart, funcEnd, paramName, typeName}.
//
//  3. passTS_SwitchCases: find switch statements whose scrutinee is a bare
//     identifier and whose case arms use raw string literals. Group by switch
//     start byte. Also detect which switches have a default clause.
//
//  4. Join: for each switch, find the innermost enclosing function parameter
//     binding that resolves the scrutinee to a union type. If found:
//     • Type-A: each case literal NOT in the union member set (typo/stale).
//     • Type-B: each union member NOT covered by any case literal (missing arm).
//       Suppressed when hasDefault is true (explicit-subset idiom is valid).
//     When no typed binding can be established, emit nothing (precision-first).
//
// Scope: TypeScript and TSX (.ts, .tsx, .mts, .cts) only — gated via
// ingest.LangTypeScript. JavaScript is excluded; JS string unions via JSDoc
// exist but are not reliably structurally checkable without a type checker.
//
// Leads: PosterLens="miner:stringly-ts-drift", TargetLens="api-contract-misuse".

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	stringlyTSPosterLens = "miner:stringly-ts-drift"
	stringlyTSTargetLens = "api-contract-misuse"

	// maxTSLeads caps leads posted by this pass per Seed run.
	maxTSLeads = 50
)

// ─── query S-expressions ─────────────────────────────────────────────────────
//
// IMPORTANT: tree-sitter TypeScript grammar parses binary union types
// left-recursively, so `'a' | 'b' | 'c'` produces:
//
//   (union_type
//     (union_type
//       (literal_type (string ...))
//       (literal_type (string ...)))
//     (literal_type (string ...)))
//
// We therefore do NOT query for literal_type inside union_type with a field
// path; instead we run two independent queries and associate by byte containment.

// tsDeclQuery finds all type_alias_declaration nodes with their name and
// full byte range (via the @decl capture on the outer node).
const tsDeclQuery = `
(type_alias_declaration
  name: (type_identifier) @type.name) @type.decl
`

// tsStringMemberQuery finds all literal_type nodes whose child is a string.
// Each match is a string-literal union member; we associate it with the
// enclosing type_alias_declaration via byte containment.
const tsStringMemberQuery = `
(literal_type (string) @member)
`

// tsNonStringMemberQuery finds literal_type nodes with non-string content.
// Any type_alias_declaration that contains such a node is a mixed union and
// must be excluded from analysis.
const tsNonStringMemberQuery = `
(literal_type
  [(number) (true) (false) (null) (undefined)] @nonstr)
`

// tsFuncParamQuery finds function parameters with explicit type annotations.
// We capture the parameter name and the type identifier so we can check
// whether the parameter type is a known union type.
const tsFuncParamQuery = `
(function_declaration
  parameters: (formal_parameters
    (required_parameter
      pattern: (identifier) @param.name
      type: (type_annotation (type_identifier) @param.type)))) @param.func
(function_declaration
  parameters: (formal_parameters
    (optional_parameter
      pattern: (identifier) @param.name
      type: (type_annotation (type_identifier) @param.type)))) @param.func
(method_definition
  parameters: (formal_parameters
    (required_parameter
      pattern: (identifier) @param.name
      type: (type_annotation (type_identifier) @param.type)))) @param.func
(method_definition
  parameters: (formal_parameters
    (optional_parameter
      pattern: (identifier) @param.name
      type: (type_annotation (type_identifier) @param.type)))) @param.func
(arrow_function
  parameters: (formal_parameters
    (required_parameter
      pattern: (identifier) @param.name
      type: (type_annotation (type_identifier) @param.type)))) @param.func
(arrow_function
  parameters: (formal_parameters
    (optional_parameter
      pattern: (identifier) @param.name
      type: (type_annotation (type_identifier) @param.type)))) @param.func
`

// tsSwitchCaseQuery finds switch_case nodes whose case value is a string literal.
// We capture the switch's scrutinee identifier and the case string.
// NOTE: gotreesitter AST has switch_case with the string as a direct child
// (not via a 'value:' field), confirmed from SExpr output.
const tsSwitchCaseQuery = `
(switch_statement
  value: (parenthesized_expression (identifier) @switch.scrutinee)
  body: (switch_body
    (switch_case (string) @case.value)))
`

// tsSwitchDefaultQuery finds switch_statement nodes that have a default clause.
const tsSwitchDefaultQuery = `
(switch_statement
  value: (parenthesized_expression (identifier) @switch.scrutinee)
  body: (switch_body (switch_default) @has.default))
`

// ─── data types ──────────────────────────────────────────────────────────────

// tsUnionType is a closed string-literal union type found in a TS file.
type tsUnionType struct {
	name      string          // type alias name
	members   map[string]bool // set of string literal values (unquoted)
	line      int             // 1-based line of the type_alias_declaration
	startByte uint32
	endByte   uint32
}

// tsFuncParam records a parameter with an explicit union-type annotation.
type tsFuncParam struct {
	paramName string
	typeName  string
	funcStart uint32
	funcEnd   uint32
}

// tsSwitchInfo records one switch statement.
type tsSwitchInfo struct {
	scrutinee  string
	switchByte uint32
	switchLine int
	hasDefault bool
	cases      []tsCaseLit
}

type tsCaseLit struct {
	value string // unquoted string literal value
	line  int    // 1-based
}

// ─── language handle cache ────────────────────────────────────────────────────

// tsLangHandle caches the compiled language and queries for one grammar.
type tsLangHandle struct {
	lang             *gts.Language
	declQ            *gts.Query
	stringMemberQ    *gts.Query
	nonStringMemberQ *gts.Query
	paramQ           *gts.Query
	switchCaseQ      *gts.Query
	switchDefaultQ   *gts.Query
}

// loadTSLangHandle loads and compiles all queries for the grammar identified
// by sample (e.g. "x.ts" or "x.tsx").
func loadTSLangHandle(sample string) (*tsLangHandle, error) {
	entry := tsregistry.DetectLanguage(sample)
	if entry == nil {
		return nil, fmt.Errorf("stringly-ts: no grammar for %s", sample)
	}
	lang := entry.Language()
	var err error
	h := &tsLangHandle{lang: lang}
	if h.declQ, err = gts.NewQuery(tsDeclQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile decl query: %w", err)
	}
	if h.stringMemberQ, err = gts.NewQuery(tsStringMemberQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile string-member query: %w", err)
	}
	if h.nonStringMemberQ, err = gts.NewQuery(tsNonStringMemberQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile non-string-member query: %w", err)
	}
	if h.paramQ, err = gts.NewQuery(tsFuncParamQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile param query: %w", err)
	}
	if h.switchCaseQ, err = gts.NewQuery(tsSwitchCaseQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile switch-case query: %w", err)
	}
	if h.switchDefaultQ, err = gts.NewQuery(tsSwitchDefaultQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile switch-default query: %w", err)
	}
	return h, nil
}

// ─── pass functions ───────────────────────────────────────────────────────────

// parseTSFile parses src with the given language handle.
func parseTSFile(h *tsLangHandle, src []byte) (*gts.Tree, error) {
	parser := gts.NewParser(h.lang)
	return parser.Parse(src)
}

// passTS_UnionTypes extracts all pure-string-literal union type aliases.
func passTS_UnionTypes(h *tsLangHandle, tree *gts.Tree, src []byte) []tsUnionType {
	// Step 1: collect all type_alias_declaration extents and names.
	type declInfo struct {
		name      string
		line      int
		startByte uint32
		endByte   uint32
	}
	var decls []declInfo
	for _, m := range h.declQ.Execute(tree) {
		var nameStr string
		var declStart, declEnd uint32
		var nameLine int
		for _, c := range m.Captures {
			switch c.Name {
			case "type.name":
				nameStr = c.Node.Text(src)
				nameLine = int(c.Node.StartPoint().Row) + 1
			case "type.decl":
				declStart = c.Node.StartByte()
				declEnd = c.Node.EndByte()
			}
		}
		if nameStr == "" {
			continue
		}
		decls = append(decls, declInfo{
			name:      nameStr,
			line:      nameLine,
			startByte: declStart,
			endByte:   declEnd,
		})
	}
	if len(decls) == 0 {
		return nil
	}

	// Step 2: find all string members; associate with enclosing decl.
	membersByDecl := make(map[string]map[string]bool, len(decls))
	for _, m := range h.stringMemberQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name != "member" {
				continue
			}
			mb := c.Node.StartByte()
			// Find the innermost enclosing type_alias_declaration.
			var best *declInfo
			for i := range decls {
				d := &decls[i]
				if d.startByte <= mb && mb < d.endByte {
					if best == nil || (d.endByte-d.startByte) < (best.endByte-best.startByte) {
						best = d
					}
				}
			}
			if best == nil {
				continue
			}
			unquoted := unquoteTSString(c.Node.Text(src))
			if unquoted == "" {
				continue
			}
			if membersByDecl[best.name] == nil {
				membersByDecl[best.name] = make(map[string]bool)
			}
			membersByDecl[best.name][unquoted] = true
		}
	}

	// Step 3: mark mixed decls (non-string literal members).
	mixed := make(map[string]bool)
	for _, m := range h.nonStringMemberQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name != "nonstr" {
				continue
			}
			mb := c.Node.StartByte()
			for i := range decls {
				d := &decls[i]
				if d.startByte <= mb && mb < d.endByte {
					mixed[d.name] = true
				}
			}
		}
	}

	// Step 4: build output — pure string unions with ≥2 members, preserving
	// declaration order.
	var out []tsUnionType
	seen := make(map[string]bool)
	for _, d := range decls {
		if seen[d.name] {
			continue // first declaration wins
		}
		seen[d.name] = true
		if mixed[d.name] {
			continue
		}
		members := membersByDecl[d.name]
		if len(members) < 2 {
			continue // not an enum-style union
		}
		out = append(out, tsUnionType{
			name:      d.name,
			members:   members,
			line:      d.line,
			startByte: d.startByte,
			endByte:   d.endByte,
		})
	}
	return out
}

// passTS_FunctionParams extracts typed parameter bindings.
func passTS_FunctionParams(h *tsLangHandle, tree *gts.Tree, src []byte, knownTypes map[string]bool) []tsFuncParam {
	if len(knownTypes) == 0 {
		return nil
	}
	var out []tsFuncParam
	for _, m := range h.paramQ.Execute(tree) {
		var paramName, paramType string
		var funcStart, funcEnd uint32
		hasFuncCapture := false
		for _, c := range m.Captures {
			switch c.Name {
			case "param.name":
				paramName = c.Node.Text(src)
			case "param.type":
				paramType = c.Node.Text(src)
			case "param.func":
				funcStart = c.Node.StartByte()
				funcEnd = c.Node.EndByte()
				hasFuncCapture = true
			}
		}
		if paramName == "" || paramType == "" || !hasFuncCapture {
			continue
		}
		if !knownTypes[paramType] {
			continue
		}
		out = append(out, tsFuncParam{
			paramName: paramName,
			typeName:  paramType,
			funcStart: funcStart,
			funcEnd:   funcEnd,
		})
	}
	return out
}

// passTS_SwitchCases extracts switch statements dispatching on a bare identifier
// with string-literal case arms.
func passTS_SwitchCases(h *tsLangHandle, tree *gts.Tree, src []byte) []tsSwitchInfo {
	type switchEntry struct {
		scrutinee  string
		switchLine int
		cases      []tsCaseLit
	}

	bySwitch := make(map[uint32]*switchEntry) // key: switch start byte
	var order []uint32

	for _, m := range h.switchCaseQ.Execute(tree) {
		var scrutinee, caseVal string
		var scrutineeNode, caseNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "switch.scrutinee":
				scrutineeNode = c.Node
				scrutinee = c.Node.Text(src)
			case "case.value":
				caseNode = c.Node
				caseVal = c.Node.Text(src)
			}
		}
		if scrutinee == "" || caseVal == "" || scrutineeNode == nil || caseNode == nil {
			continue
		}
		unquoted := unquoteTSString(caseVal)
		if unquoted == "" {
			continue
		}

		// Walk up from the scrutinee to find the switch_statement node's start byte.
		// scrutineeNode is inside parenthesized_expression which is the first child
		// of switch_statement.
		swNode := parentN(scrutineeNode, 2) // identifier → paren_expr → switch_stmt
		if swNode == nil {
			continue
		}
		key := swNode.StartByte()

		entry, exists := bySwitch[key]
		if !exists {
			entry = &switchEntry{
				scrutinee:  scrutinee,
				switchLine: int(swNode.StartPoint().Row) + 1,
			}
			bySwitch[key] = entry
			order = append(order, key)
		}
		caseLine := int(caseNode.StartPoint().Row) + 1
		entry.cases = append(entry.cases, tsCaseLit{value: unquoted, line: caseLine})
	}

	// Collect default-having switches.
	defaultKeys := make(map[uint32]bool)
	for _, m := range h.switchDefaultQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name != "switch.scrutinee" || c.Node == nil {
				continue
			}
			swNode := parentN(c.Node, 2)
			if swNode != nil {
				defaultKeys[swNode.StartByte()] = true
			}
		}
	}

	out := make([]tsSwitchInfo, 0, len(bySwitch))
	for _, key := range order {
		e := bySwitch[key]
		out = append(out, tsSwitchInfo{
			scrutinee:  e.scrutinee,
			switchByte: key,
			switchLine: e.switchLine,
			hasDefault: defaultKeys[key],
			cases:      e.cases,
		})
	}
	return out
}

// parentN walks n up `steps` levels via Parent() and returns the ancestor.
// Returns nil if not enough parents.
func parentN(n *gts.Node, steps int) *gts.Node {
	cur := n
	for i := 0; i < steps; i++ {
		if cur == nil {
			return nil
		}
		cur = cur.Parent()
	}
	return cur
}

// unquoteTSString strips surrounding single or double quotes from a TS string
// literal node text (e.g. `"active"` → `active`, `'pending'` → `pending`).
// Returns "" when the input does not look like a simple quoted string.
func unquoteTSString(s string) string {
	if len(s) < 2 {
		return ""
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return s[1 : len(s)-1]
	}
	return ""
}

// ─── join pass ────────────────────────────────────────────────────────────────

func joinTSDrift(
	path string,
	unions []tsUnionType,
	params []tsFuncParam,
	switches []tsSwitchInfo,
) (typeA []store.Lead, typeB []store.Lead) {
	if len(unions) == 0 || len(params) == 0 || len(switches) == 0 {
		return nil, nil
	}

	unionByName := make(map[string]*tsUnionType, len(unions))
	for i := range unions {
		unionByName[unions[i].name] = &unions[i]
	}

	seen := make(map[leadKey]bool)

	for i := range switches {
		sw := &switches[i]

		// Find the innermost function parameter that types the scrutinee.
		var resolvedType *tsUnionType
		var bestSpan uint32 = ^uint32(0)
		for _, p := range params {
			if p.paramName != sw.scrutinee {
				continue
			}
			if p.funcStart > sw.switchByte || p.funcEnd < sw.switchByte {
				continue
			}
			span := p.funcEnd - p.funcStart
			if span < bestSpan {
				bestSpan = span
				if ut, ok := unionByName[p.typeName]; ok {
					resolvedType = ut
				}
			}
		}
		if resolvedType == nil {
			continue // unprovable — emit nothing (precision-first)
		}

		covered := make(map[string]bool, len(sw.cases))
		for _, c := range sw.cases {
			covered[c.value] = true
		}

		// Type-A: case literal not in union.
		for _, c := range sw.cases {
			if resolvedType.members[c.value] {
				continue
			}
			k := leadKey{TargetLens: stringlyTSTargetLens, File: path, Line: c.line}
			if seen[k] {
				continue
			}
			seen[k] = true
			note := fmt.Sprintf(
				"stringly-ts-drift: case literal %q at %s:%d does not match "+
					"any member of union type %s; likely a typo or stale branch",
				c.value, path, c.line, resolvedType.name,
			)
			typeA = append(typeA, store.Lead{
				PosterLens: stringlyTSPosterLens,
				TargetLens: stringlyTSTargetLens,
				File:       path,
				Line:       c.line,
				Note:       truncate(note, noteMaxLen),
			})
		}

		// Type-B: union member not covered. Suppressed when default exists.
		if sw.hasDefault {
			continue
		}
		var uncovered []string
		for member := range resolvedType.members {
			if !covered[member] {
				uncovered = append(uncovered, member)
			}
		}
		sort.Strings(uncovered)
		for _, val := range uncovered {
			k := leadKey{TargetLens: stringlyTSTargetLens, File: path, Line: sw.switchLine}
			if seen[k] {
				continue
			}
			seen[k] = true
			note := fmt.Sprintf(
				"stringly-ts-drift: switch at %s:%d handles type %s but "+
					"missing case for union member %q",
				path, sw.switchLine, resolvedType.name, val,
			)
			typeB = append(typeB, store.Lead{
				PosterLens: stringlyTSPosterLens,
				TargetLens: stringlyTSTargetLens,
				File:       path,
				Line:       sw.switchLine,
				Note:       truncate(note, noteMaxLen),
			})
		}
	}
	return typeA, typeB
}

// ─── seed entrypoint ──────────────────────────────────────────────────────────

// seedStringlyTSDrift runs the TypeScript string-union drift miner.
func seedStringlyTSDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	tsHandle, err := loadTSLangHandle("x.ts")
	if err != nil {
		// Grammar unavailable — degrade silently.
		return nil
	}
	tsxHandle, err := loadTSLangHandle("x.tsx")
	if err != nil {
		tsxHandle = tsHandle
	}

	leadsPosted := 0

	for _, f := range snap.Files {
		if f.Language != ingest.LangTypeScript {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Path))
		var h *tsLangHandle
		if ext == ".tsx" {
			h = tsxHandle
		} else {
			h = tsHandle
		}

		abs := filepath.Join(snap.Root, filepath.FromSlash(f.Path))
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if fi.Size() > maxFileBytes {
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}

		tree, err := parseTSFile(h, src)
		if err != nil || tree == nil {
			continue
		}

		unions := passTS_UnionTypes(h, tree, src)
		if len(unions) == 0 {
			tree.Release()
			continue
		}

		knownTypes := make(map[string]bool, len(unions))
		for _, u := range unions {
			knownTypes[u.name] = true
		}

		params := passTS_FunctionParams(h, tree, src, knownTypes)
		switches := passTS_SwitchCases(h, tree, src)
		tree.Release()

		typeA, typeB := joinTSDrift(f.Path, unions, params, switches)
		for _, lead := range append(typeA, typeB...) {
			if err := st.AddLead(ctx, lead); err != nil {
				return fmt.Errorf("miner: stringly-ts lead %s: %w", lead.File, err)
			}
			sum.StringlyDriftLeads++
			sum.LeadsPosted++
			leadsPosted++
			if leadsPosted >= maxTSLeads {
				return nil
			}
		}
	}
	return nil
}
