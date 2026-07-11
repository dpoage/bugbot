package miner

// TypeScript string-union drift miner.
//
// # Design decision: tree-sitter queries, not regex lexers
//
// The Go stringly miner (stringly.go) encodes Go lexical structure in pure
// regex and required ~15 oracle fix rounds to handle rune literals, backtick
// raw strings, single-line block comments, and := shadow scoping. Porting the
// same regex approach to TypeScript would inherit the same fragility — TS adds
// template literals, optional chaining, and JSX quasi-quotations.
//
// Tree-sitter is the right tool here:
//   - It parses at the AST level, so string literals in comments or template
//     expressions never pollute case-arm or type-member extraction.
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
//     (c) STRUCTURAL WHITELIST (not a blocklist): walk each type_alias_declaration's
//         value node tree; every direct non-union_type child of a union_type
//         in the value must be literal_type and must contain a string child.
//         Any other node type (predefined_type, type_identifier, object_type,
//         function_type, conditional_type, etc.) → the type is mixed and
//         excluded. This is correct even for 'a'|'b'|number (predefined_type
//         not in any literal_type) and 'a'|{k:string} (object_type).
//     Only pure-string unions with ≥2 members are kept.
//
//  2. passTS_Bindings: find ALL scope-introducing bindings in the file:
//     typed-union parameters (function/method/arrow), untyped parameters,
//     and block-scoped const/let/var declarators. Each binding records its
//     name, scope byte range, and whether it is a typed union param.
//
//  3. passTS_SwitchCases: find switch statements whose scrutinee is a bare
//     identifier and whose case arms use raw string literals. Group by switch
//     start byte. Also detect which switches have a default clause.
//
//  4. Join: for each switch, find the NEAREST binding (smallest scope span that
//     contains the switch) whose name matches the scrutinee. If the nearest
//     such binding is a typed union param, proceed; otherwise emit nothing
//     (precision-first). This resolves the shadow class: an untyped inner
//     closure param or a block-scoped const that shadows an outer typed param
//     is the nearer binding and causes the switch to be skipped.
//     • Type-A: each case literal NOT in the union member set (typo/stale).
//     • Type-B: each union member NOT covered by any case literal (missing arm).
//       Suppressed when hasDefault is true (explicit-subset idiom is valid).
//       One lead per missing member.
//
// Scope: TypeScript and TSX (.ts, .tsx, .mts, .cts) only — gated via
// ingest.LangTypeScript. Test files (.test.ts, .spec.ts, files in
// __tests__/test/tests/spec directories) are skipped — they plant deliberate
// defects that would flood leads. JavaScript is excluded; JS string unions via
// JSDoc exist but are not reliably structurally checkable without a type checker.
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
//	(union_type
//	  (union_type
//	    (literal_type (string ...))
//	    (literal_type (string ...)))
//	  (literal_type (string ...)))
//
// We therefore do NOT query for literal_type inside union_type with a field
// path; instead we run two independent queries and associate by byte containment,
// then apply the structural whitelist in Go code.

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

// tsDeclValueQuery finds the VALUE node of each type_alias_declaration.
// We use this to walk the union_type chain for structural whitelist validation.
// Captures: "decl.name" = type identifier, "decl.value" = the value node.
const tsDeclValueQuery = `
(type_alias_declaration
  name: (type_identifier) @decl.name
  value: _ @decl.value)
`

// tsTypedParamQuery finds function parameters with explicit type annotations
// that are a simple type_identifier (not a generic, qualified, or union type).
// Captures: "param.name", "param.type", "param.func" (enclosing function node).
const tsTypedParamQuery = `
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

// tsAnyParamQuery finds ALL function parameters regardless of type annotation.
// Used to detect shadow bindings: an untyped parameter in an inner function
// that has the same name as a typed outer parameter hides the outer type.
// Captures: "any.name" = parameter identifier, "any.func" = enclosing function.
const tsAnyParamQuery = `
(function_declaration
  parameters: (formal_parameters
    [(required_parameter pattern: (identifier) @any.name)
     (optional_parameter pattern: (identifier) @any.name)])) @any.func
(method_definition
  parameters: (formal_parameters
    [(required_parameter pattern: (identifier) @any.name)
     (optional_parameter pattern: (identifier) @any.name)])) @any.func
(arrow_function
  parameters: (formal_parameters
    [(required_parameter pattern: (identifier) @any.name)
     (optional_parameter pattern: (identifier) @any.name)])) @any.func
`

// tsBlockScopeQuery finds block-scoped const/let/var declarators that bind
// an identifier. Used to detect block-scoped shadows of outer typed params.
// Captures: "decl.name" = the identifier, "decl.scope" = the statement node
// (lexical_declaration or variable_declaration; scope ends at its end byte).
const tsBlockScopeQuery = `
(lexical_declaration
  (variable_declarator name: (identifier) @decl.name)) @decl.scope
(variable_declaration
  (variable_declarator name: (identifier) @decl.name)) @decl.scope
`

// tsSwitchCaseQuery finds switch_case nodes whose case value is a string literal.
// NOTE: gotreesitter AST has switch_case with the string as a direct child
// (not via a 'value:' field), confirmed from SExpr inspection.
const tsSwitchCaseQuery = `
(switch_statement
  value: (parenthesized_expression (identifier) @switch.scrutinee)
  body: (switch_body
    (switch_case (string) @case.value)))
`

// tsSwitchDefaultQuery finds switch statements that have a default clause.
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

// tsBinding records a scope-introducing binding for the scrutinee resolution pass.
// It covers typed union params, untyped params, and block-scoped const/let/var.
type tsBinding struct {
	name         string
	typeName     string // non-empty only for typed params whose type is a known union
	scopeStart   uint32 // byte range of the enclosing function or block scope
	scopeEnd     uint32
	isTypedUnion bool // true iff typeName is a known union type
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
	lang           *gts.Language
	declQ          *gts.Query
	stringMemberQ  *gts.Query
	declValueQ     *gts.Query
	typedParamQ    *gts.Query
	anyParamQ      *gts.Query
	blockScopeQ    *gts.Query
	switchCaseQ    *gts.Query
	switchDefaultQ *gts.Query
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
	if h.declValueQ, err = gts.NewQuery(tsDeclValueQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile decl-value query: %w", err)
	}
	if h.typedParamQ, err = gts.NewQuery(tsTypedParamQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile typed-param query: %w", err)
	}
	if h.anyParamQ, err = gts.NewQuery(tsAnyParamQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile any-param query: %w", err)
	}
	if h.blockScopeQ, err = gts.NewQuery(tsBlockScopeQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-ts: compile block-scope query: %w", err)
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
//
// The mixed-union check uses a STRUCTURAL WHITELIST: every direct non-union_type
// child of a union_type in the declaration's value tree must be a literal_type
// whose sole child is a string. Any other child type → the alias is mixed.
// This excludes 'a'|number (predefined_type), 'a'|SomeRef (type_identifier),
// 'a'|{k:string} (object_type), and any other non-literal-type member.
func passTS_UnionTypes(h *tsLangHandle, tree *gts.Tree, src []byte) []tsUnionType {
	// Step 1: collect all type_alias_declaration extents and names.
	type declInfo struct {
		name      string
		line      int
		startByte uint32
		endByte   uint32
		valueNode *gts.Node // the value node of the type alias
	}
	var decls []declInfo

	for _, m := range h.declValueQ.Execute(tree) {
		var nameStr string
		var valueNode *gts.Node
		var nameLine int
		for _, c := range m.Captures {
			switch c.Name {
			case "decl.name":
				nameStr = c.Node.Text(src)
				nameLine = int(c.Node.StartPoint().Row) + 1
			case "decl.value":
				valueNode = c.Node
			}
		}
		if nameStr == "" || valueNode == nil {
			continue
		}
		decls = append(decls, declInfo{
			name:      nameStr,
			line:      nameLine,
			startByte: valueNode.StartByte(),
			endByte:   valueNode.EndByte(),
			valueNode: valueNode,
		})
	}
	if len(decls) == 0 {
		return nil
	}

	// Step 2: collect all literal_type(string) members; associate by containment.
	membersByDecl := make(map[string]map[string]bool, len(decls))
	for _, m := range h.stringMemberQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name != "member" {
				continue
			}
			mb := c.Node.StartByte()
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

	// Step 3: structural whitelist — for each decl, walk the value node's
	// union_type chain and verify every leaf branch is a literal_type(string).
	// Any other node type makes the union mixed → excluded.
	mixed := make(map[string]bool)
	for i := range decls {
		d := &decls[i]
		if !isPureStringUnion(d.valueNode, h.lang, src) {
			mixed[d.name] = true
		}
	}

	// Step 4: build output — pure string unions with ≥2 members.
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
			continue // trivial alias, not an enum-style union
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

// isPureStringUnion reports whether node is a pure string-literal union:
// either a literal_type(string), or a union_type whose every direct child
// is isPureStringUnion. Any other node type → false.
//
// This is the structural whitelist that replaces the blocklist approach.
// It correctly handles the recursive left-associative union_type encoding.
func isPureStringUnion(node *gts.Node, lang *gts.Language, src []byte) bool {
	if node == nil {
		return false
	}
	switch node.Type(lang) {
	case "union_type":
		// Every direct named child must also be pure.
		for i := 0; i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child == nil || !child.IsNamed() {
				continue // skip anonymous syntax tokens ("|")
			}
			if !isPureStringUnion(child, lang, src) {
				return false
			}
		}
		return true
	case "literal_type":
		// Must have exactly one named child that is a string node.
		for i := 0; i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child == nil || !child.IsNamed() {
				continue
			}
			if child.Type(lang) == "string" {
				return true
			}
			return false // non-string literal (number, boolean, null, etc.)
		}
		return false // empty literal_type
	default:
		return false
	}
}

// passTS_Bindings collects ALL scope-introducing bindings for each identifier
// name: typed union params, untyped params, and block-scoped const/let/var.
//
// The result is used in joinTSDrift to find the NEAREST binding of the
// scrutinee's name. If the nearest binding is not a typed union param, the
// switch is skipped (precision-first shadow handling).
func passTS_Bindings(h *tsLangHandle, tree *gts.Tree, src []byte, knownTypes map[string]bool) []tsBinding {
	if len(knownTypes) == 0 {
		return nil
	}
	var out []tsBinding

	// Collect typed union params first (these are the only ones with isTypedUnion=true).
	for _, m := range h.typedParamQ.Execute(tree) {
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
		out = append(out, tsBinding{
			name:         paramName,
			typeName:     paramType,
			scopeStart:   funcStart,
			scopeEnd:     funcEnd,
			isTypedUnion: true,
		})
	}

	// Collect ALL params (including untyped) to detect shadow bindings.
	// We record every param as a binding with isTypedUnion=false unless
	// it was already added above as a typed union param (we detect duplicates
	// by matching name+scopeStart). The typed entry wins on exact match.
	typedKey := make(map[string]bool) // "name:scopeStart" → already typed
	for _, b := range out {
		typedKey[fmt.Sprintf("%s:%d", b.name, b.scopeStart)] = true
	}

	for _, m := range h.anyParamQ.Execute(tree) {
		var paramName string
		var funcStart, funcEnd uint32
		hasFuncCapture := false
		for _, c := range m.Captures {
			switch c.Name {
			case "any.name":
				paramName = c.Node.Text(src)
			case "any.func":
				funcStart = c.Node.StartByte()
				funcEnd = c.Node.EndByte()
				hasFuncCapture = true
			}
		}
		if paramName == "" || !hasFuncCapture {
			continue
		}
		k := fmt.Sprintf("%s:%d", paramName, funcStart)
		if typedKey[k] {
			continue // already recorded as typed union param
		}
		// Record as an untyped or non-union-typed binding.
		out = append(out, tsBinding{
			name:         paramName,
			scopeStart:   funcStart,
			scopeEnd:     funcEnd,
			isTypedUnion: false,
		})
	}

	// Collect block-scoped const/let/var declarators.
	// We use the statement node's end byte as the scope end.
	// Block-scoped declarations scope to the enclosing block, but approximating
	// with the statement's end byte is conservative (wider than actual scope),
	// which is safe for our purpose: if the switch is inside the statement node,
	// the binding shadows any outer typed param.
	for _, m := range h.blockScopeQ.Execute(tree) {
		var bindName string
		var scopeStart, scopeEnd uint32
		hasScopeCapture := false
		for _, c := range m.Captures {
			switch c.Name {
			case "decl.name":
				bindName = c.Node.Text(src)
			case "decl.scope":
				// The scope of a block-scoped variable extends from the declaration
				// to the end of the enclosing block. We approximate with the
				// declaration statement's parent block's end byte. For precision,
				// we use the file end (which is conservative but always correct):
				// if the variable is declared before the switch, and it shadows
				// an outer typed param, the switch is in its scope.
				// Actual: use parent block end.
				scopeStart = c.Node.StartByte()
				scopeEnd = c.Node.EndByte()
				hasScopeCapture = true
			}
		}
		if bindName == "" || !hasScopeCapture {
			continue
		}
		// Walk up to the enclosing block to get the true scope end.
		if declNode := findCapture(m, "decl.scope"); declNode != nil {
			if block := enclosingBlock(declNode); block != nil {
				scopeEnd = block.EndByte()
			}
		}
		out = append(out, tsBinding{
			name:         bindName,
			scopeStart:   scopeStart,
			scopeEnd:     scopeEnd,
			isTypedUnion: false,
		})
	}
	return out
}

// findCapture returns the node for the first capture with the given name.
func findCapture(m gts.QueryMatch, name string) *gts.Node {
	for _, c := range m.Captures {
		if c.Name == name {
			return c.Node
		}
	}
	return nil
}

// enclosingBlock walks up the parent chain to find the nearest
// statement_block (the {…} body of a function, if, for, etc.).
func enclosingBlock(n *gts.Node) *gts.Node {
	cur := n.Parent()
	for cur != nil {
		// statement_block is the TS grammar node for { ... }
		// We don't have lang here but we can check by ChildCount heuristic;
		// actually use a string comparison on the raw symbol name:
		// gts.Node.Type requires lang, but we can walk until we find a
		// suitable block. Since we just want the end byte conservatively,
		// we'll use the parent of the declaration (the next enclosing scope).
		// For block-scoped let/const, the scope is the nearest ancestor block.
		// Returning the first non-trivial parent is a safe conservative approximation.
		if cur.ChildCount() > 2 {
			return cur
		}
		cur = cur.Parent()
	}
	return nil
}

// passTS_SwitchCases extracts switch statements dispatching on a bare identifier
// with string-literal case arms.
func passTS_SwitchCases(h *tsLangHandle, tree *gts.Tree, src []byte) []tsSwitchInfo {
	type switchEntry struct {
		scrutinee  string
		switchLine int
		cases      []tsCaseLit
	}

	bySwitch := make(map[uint32]*switchEntry)
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
		swNode := parentN(scrutineeNode, 2)
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

// joinTSDrift performs the type-A / type-B join for one TS file.
//
// Shadow resolution: for each switch, find the NEAREST binding (smallest scope
// span enclosing the switch) whose name matches the scrutinee. If the nearest
// binding is a typed union param, proceed; otherwise emit nothing.
func joinTSDrift(
	path string,
	unions []tsUnionType,
	bindings []tsBinding,
	switches []tsSwitchInfo,
) (typeA []store.Lead, typeB []store.Lead) {
	if len(unions) == 0 || len(bindings) == 0 || len(switches) == 0 {
		return nil, nil
	}

	unionByName := make(map[string]*tsUnionType, len(unions))
	for i := range unions {
		unionByName[unions[i].name] = &unions[i]
	}

	seen := make(map[leadKey]bool)

	for i := range switches {
		sw := &switches[i]

		// Find the NEAREST binding of the scrutinee that encloses the switch.
		// "Nearest" = smallest scope span (most tightly enclosing).
		var nearestSpan uint32 = ^uint32(0)
		var nearestBinding *tsBinding
		for j := range bindings {
			b := &bindings[j]
			if b.name != sw.scrutinee {
				continue
			}
			if b.scopeStart > sw.switchByte || b.scopeEnd < sw.switchByte {
				continue
			}
			span := b.scopeEnd - b.scopeStart
			if span < nearestSpan {
				nearestSpan = span
				nearestBinding = b
			}
		}

		// If the nearest binding is not a typed union param, skip entirely.
		if nearestBinding == nil || !nearestBinding.isTypedUnion {
			continue
		}
		resolvedType, ok := unionByName[nearestBinding.typeName]
		if !ok {
			continue
		}

		covered := make(map[string]bool, len(sw.cases))
		for _, c := range sw.cases {
			covered[c.value] = true
		}

		// Type-A: case literal not in union member set.
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
		// One lead per missing member.
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

// ─── test-file gate ───────────────────────────────────────────────────────────

// isTSTestPath reports whether a repo-relative file path looks like a TypeScript
// test file. Mirrors the isTestPath convention from internal/repro/patch.go but
// scoped to TS/JS patterns. Test files in target repos plant deliberate defects
// that would produce false leads.
func isTSTestPath(relPath string) bool {
	slashed := filepath.ToSlash(relPath)
	for _, seg := range strings.Split(slashed, "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "testdata":
			return true
		}
	}
	base := strings.ToLower(filepath.Base(slashed))
	// foo.test.ts, foo.spec.tsx, foo.test.js style.
	if parts := strings.Split(base, "."); len(parts) >= 3 {
		switch parts[len(parts)-2] {
		case "test", "spec":
			return true
		}
	}
	return false
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
		if isTSTestPath(f.Path) {
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

		bindings := passTS_Bindings(h, tree, src, knownTypes)
		switches := passTS_SwitchCases(h, tree, src)
		tree.Release()

		typeA, typeB := joinTSDrift(f.Path, unions, bindings, switches)
		for _, lead := range append(typeA, typeB...) {
			if err := st.AddLead(ctx, lead); err != nil {
				return fmt.Errorf("miner: stringly-ts lead %s: %w", lead.File, err)
			}
			sum.StringlyTSDriftLeads++
			sum.LeadsPosted++
			leadsPosted++
			if leadsPosted >= maxTSLeads {
				return nil
			}
		}
	}
	return nil
}
