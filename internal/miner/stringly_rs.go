package miner

// Rust &str match-drift miner (miner:stringly-rs-drift).
//
// # Detection algorithm: Type-A &str match drift
//
// Detects match arms whose literal values do not appear in the file-local pool
// of const NAME: &str = "literal" producers. The precondition is that the
// match scrutinee is a bare identifier whose NEAREST binding is a &str-typed
// function parameter — any intervening binding (let-shadow, closure param,
// for-loop variable, if/while-let destructure, match-arm capture) suppresses
// the lead.
//
// # Producer set (file-local only)
//
// Every top-level `const NAME: &str = "literal"` or `const NAME: &'static str
// = "literal"` contributes its literal value to the file's producer pool. The
// entire pool is one group — no cross-file joins. This is conservative: if the
// real producer lives in another file the scrutinee will be unresolved (no
// typed-param binding in scope) and the match is skipped.
//
// # Type-B (wildcard-enum drift) — DESCOPED
//
// Rationale: detecting "a `_ =>` wildcard arm in a match over a file-local
// enum where a variant is unhandled" requires enumerating all enum variants.
// For a file-local enum this is achievable, but:
//   - The 7.1% HasError rate means some files with enum definitions are
//     skipped; re-checking a match in a sibling file against an enum whose
//     file errored would cause a false "missing variant" lead.
//   - Even with HasError guard, the match expression and the enum_item must
//     both appear in the same file. When they do, rustc's exhaustiveness
//     checker already catches the missing arm as a compile error (E0004) —
//     unless the `_ =>` wildcard suppresses it. The `_ => unreachable!()`
//     case is the one narrow window, but it requires the wildcard body to be
//     provably unreachable, which needs macro-expansion (unreachable!/panic!
//     are macro_invocations whose expansion we cannot see at parse time).
//   - Conclusion: type-B cannot be made precision-safe at file-local scope
//     without a macro-expansion step. Descoped for v1; ship type-A only.
//
// # Serde config-field evaluation — DECISION: DESCOPE
//
// Assessed: #[serde(default)] and #[serde(default = "path")] attributes joined
// against validator functions that reject the default value. The evaluation:
//   1. `#[serde(default)]` uses the field type's Default::default(). For a
//      bool field that's `false`; for a u32 it's `0`. Determining what the
//      default IS requires type resolution (type_identifier → concrete type →
//      Default impl), which is cross-file.
//   2. `#[serde(default = "path")]` names a function by path. The function
//      body is in another file 98% of the time (it's a free fn in a helpers
//      module). File-local co-location is rare by convention.
//   3. Validator functions (validate_*, check_* or #[validate(range(...))]) are
//      typically in separate impl blocks or even separate crates.
//   4. Result: deterministic file-local proof is infeasible. A cross-file join
//      would require full type resolution (not achievable with a tree-sitter
//      miner). DESCOPED for v1 — no false positives possible.
//
// # Scope resolution binding forms
//
// All binding forms that can shadow a &str-typed parameter:
//   - function_item parameter: (parameter (identifier) ...) → typed binding
//   - closure_parameters identifier → untyped sentinel
//   - let_declaration bare identifier (plain let) → untyped sentinel
//   - let_declaration tuple_struct_pattern (let-else destructure) → untyped
//     sentinel; identifiers extracted via collectRSPatternIdents
//   - for_expression left identifier → untyped sentinel
//   - let_condition (if let / while let) bound identifier → untyped sentinel;
//     identifiers extracted via collectRSPatternIdents
//   - match_arm pattern capture (identifier inside tuple_struct_pattern) →
//     untyped sentinel
//
// Any sentinel nearer than the typed parameter causes the match to be skipped.
//
// # Arm↔pool anchor (D1 precision guard)
//
// A file may have const &str items that are semantically unrelated to the
// match being analyzed (e.g., const VERSION = "1.4.2" alongside a subcommand
// dispatch match; const COLOR_RED = "#ff0000" alongside an HTTP verb match).
// Without an anchor, the miner would flag every arm literal not in the pool —
// all false positives.
//
// Fix: before emitting any lead for a match, require that ≥1 arm literal is
// already present in the producer pool. A single overlap confirms the match
// is dispatching over the same domain as the const producers. The threshold
// is 1 (not majority) because "2 correct arms + 1 typo arm" must still fire;
// majority would silence the typo in that case.
//
// # Test-file gating
//
// Files in tests/ directories or named *_test.rs are skipped. The heuristic
// uses path-segment matching (same as isTSTestPath / isPyTestPath siblings)
// plus a filename suffix check. We do NOT parse the file looking for
// #[cfg(test)] — that would require another AST pass for minimal gain;
// tests/ dirs and *_test.rs cover the overwhelming majority.
//
// Leads: PosterLens="miner:stringly-rs-drift", TargetLens="api-contract-misuse".

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	stringlyRsPosterLens = "miner:stringly-rs-drift"
	stringlyRsTargetLens = "api-contract-misuse"

	// maxRsLeads caps leads posted by this pass per Seed run.
	maxRsLeads = 50
)

// ─── query S-expressions ─────────────────────────────────────────────────────

// rsConstStrQuery finds top-level const_item nodes whose type is &str or
// &'static str and whose value is a string_literal or raw_string_literal.
// Captures: "const.name" = identifier, "const.val" = the string node.
//
// The type path: reference_type → primitive_type "str". We match both
// `&str` (no lifetime) and `&'static str` (with lifetime) by matching the
// reference_type whose child primitive_type is "str".
const rsConstStrQuery = `
(const_item
  name: (identifier) @const.name
  type: (reference_type
    (primitive_type))
  value: (string_literal) @const.val)
`

// rsConstRawStrQuery finds top-level const_item nodes with raw_string_literal.
const rsConstRawStrQuery = `
(const_item
  name: (identifier) @const.name
  type: (reference_type
    (primitive_type))
  value: (raw_string_literal) @const.val)
`

// rsTypedStrParamQuery finds function parameters typed as &str or &'static str.
// Captures: "param.name" = the identifier, "param.func" = the function_item.
//
// NOTE: We match reference_type → primitive_type here. The grammar encodes
// `&str` as reference_type(primitive_type("str")). We do not attempt to match
// `String` (type_identifier) because String-typed params don't directly match
// string literals in match arms without .as_str() — a non-bare-identifier
// scrutinee that our match query already excludes.
const rsTypedStrParamQuery = `
(function_item
  parameters: (parameters
    (parameter
      pattern: (identifier) @param.name
      type: (reference_type
        (primitive_type))))) @param.func
`

// rsAnyBindingQuery finds ALL scope-introducing identifier bindings that are
// NOT covered by the typed-param query. These are shadow sentinels: if any
// of these is the NEAREST enclosing binding for the match scrutinee, the
// outer typed param is shadowed and the match is skipped.
//
// Covered forms and their rationale:
//   - closure_parameters identifier: |x| or |x: T| → all closure params are
//     sentinels because we cannot determine their type without type inference.
//   - let_declaration identifier (plain let, no type annotation): let x = ...
//     Rust shadowing is pervasive; `let x = x.trim()` rebinds with unknown type.
//   - for_expression left identifier: for item in items → loop variable.
//   - let_condition tuple_struct_pattern inner identifier: if let Some(x) = ...
//     and while let Some(x) = ... — x is bound from destructure, type unknown.
//   - let_declaration tuple_struct_pattern inner identifier: let Some(y) = opt
//     else { ... } (let-else) — y is bound from destructure, type unknown.
//
// All captures use "any.name" (binding identifier) and "any.scope" (scope node).
//
// IMPORTANT: let_declaration with an explicit &str type annotation IS a typed
// binding. We handle this separately in passRS_Bindings by checking for a
// type_annotation child. A plain `let x = ...` without annotation is a sentinel.
const rsClosureParamQuery = `
(closure_expression
  parameters: (closure_parameters
    (identifier) @any.name)) @any.scope
`

// rsForLoopQuery finds for-loop variable bindings.
const rsForLoopQuery = `
(for_expression
  pattern: (identifier) @any.name) @any.scope
`

// rsLetDeclarationQuery finds plain let bindings (no type annotation).
// We capture the full let_declaration as scope node; the pattern we look for
// is a bare identifier on the left (not a tuple/struct destructure).
// The let_declaration's parent block provides the scope span.
const rsLetDeclarationQuery = `
(let_declaration
  pattern: (identifier) @any.name) @any.scope
`

// rsLetDeclarationPatQuery finds let-else bindings with destructuring patterns
// (tuple_struct_pattern, struct_pattern). These are shadow sentinels: the
// bound identifiers shadow any outer &str-typed parameter.
//
// Example: `let Some(cmd) = opt else { return; };` — `cmd` is bound via
// tuple_struct_pattern and must be treated as an untyped sentinel.
// We capture the let_declaration node as scope; the parent block is resolved
// in Go code via enclosingRSBlock.
const rsLetDeclarationPatQuery = `
(let_declaration
  pattern: (tuple_struct_pattern) @let.pattern) @let.decl
`

// rsIfLetQuery finds if-let and while-let bindings. The let_condition node
// contains the pattern; we extract identifiers from it.
// We use a broad capture of the let_condition and traverse it in Go code to
// extract bound identifiers (tuple_struct_pattern, struct_pattern, identifier).
const rsIfLetQuery = `
(let_condition
  pattern: (_) @let.pattern) @let.cond
`

// rsMatchArmCaptureQuery finds match arm patterns that bind identifiers via
// tuple_struct_pattern (e.g., Foo::Bar(x)) or struct_pattern. We extract
// the innermost identifiers via Go traversal.
// We capture the match_arm body as scope.
const rsMatchArmCaptureQuery = `
(match_arm
  pattern: (match_pattern
    (tuple_struct_pattern
      (identifier) @any.name))) @any.scope
`

// rsMatchExprQuery finds match expressions whose scrutinee is a bare identifier.
// Captures: "match.scrutinee" = identifier, "match.expr" = match_expression.
const rsMatchExprQuery = `
(match_expression
  value: (identifier) @match.scrutinee) @match.expr
`

// rsMatchArmStrQuery finds match arms whose pattern is a string_literal.
// Captures: "arm.val" = string_literal, "arm.arm" = match_arm node.
const rsMatchArmStrQuery = `
(match_arm
  pattern: (match_pattern
    (string_literal) @arm.val)) @arm.arm
`

// rsMatchArmRawStrQuery finds match arms whose pattern is a raw_string_literal.
const rsMatchArmRawStrQuery = `
(match_arm
  pattern: (match_pattern
    (raw_string_literal) @arm.val)) @arm.arm
`

// ─── data types ──────────────────────────────────────────────────────────────

// rsConstStr is a file-level const &str = "literal" producer.
type rsConstStr struct {
	name  string
	value string // unquoted literal
	line  int
}

// rsBinding records a scope-introducing binding. isTypedStr is true only for
// &str-typed function parameters; false for all sentinel bindings.
type rsBinding struct {
	name       string
	scopeStart uint32
	scopeEnd   uint32
	isTypedStr bool
}

// rsMatchInfo records one match expression with string literal arms.
// Wildcard arms (_ =>) are intentionally NOT tracked: every &str match in
// Rust requires _ to compile (exhaustiveness), so hasWild would be true for
// every match. There is no useful signal in it for type-A detection.
type rsMatchInfo struct {
	scrutinee string
	exprStart uint32
	exprEnd   uint32
	exprLine  int
	arms      []rsArmLit
}

type rsArmLit struct {
	value string // unquoted
	line  int
}

// ─── language handle ──────────────────────────────────────────────────────────

// rsLangHandle caches the compiled language and queries for the Rust grammar.
type rsLangHandle struct {
	lang          *gts.Language
	constStrQ     *gts.Query
	constRawStrQ  *gts.Query
	typedParamQ   *gts.Query
	closureParamQ *gts.Query
	forLoopQ      *gts.Query
	letDeclQ      *gts.Query
	letDeclPatQ   *gts.Query
	ifLetQ        *gts.Query
	matchArmCapQ  *gts.Query
	matchExprQ    *gts.Query
	matchArmStrQ  *gts.Query
	matchArmRawQ  *gts.Query
}

func loadRSLangHandle() (*rsLangHandle, error) {
	entry := tsregistry.DetectLanguage("x.rs")
	if entry == nil {
		return nil, fmt.Errorf("stringly-rs: no grammar for .rs")
	}
	lang := entry.Language()
	h := &rsLangHandle{lang: lang}

	var err error
	if h.constStrQ, err = gts.NewQuery(rsConstStrQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile const-str query: %w", err)
	}
	if h.constRawStrQ, err = gts.NewQuery(rsConstRawStrQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile const-raw-str query: %w", err)
	}
	if h.typedParamQ, err = gts.NewQuery(rsTypedStrParamQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile typed-param query: %w", err)
	}
	if h.closureParamQ, err = gts.NewQuery(rsClosureParamQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile closure-param query: %w", err)
	}
	if h.forLoopQ, err = gts.NewQuery(rsForLoopQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile for-loop query: %w", err)
	}
	if h.letDeclQ, err = gts.NewQuery(rsLetDeclarationQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile let-decl query: %w", err)
	}
	if h.letDeclPatQ, err = gts.NewQuery(rsLetDeclarationPatQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile let-decl-pat query: %w", err)
	}
	if h.ifLetQ, err = gts.NewQuery(rsIfLetQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile if-let query: %w", err)
	}
	if h.matchArmCapQ, err = gts.NewQuery(rsMatchArmCaptureQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile match-arm-cap query: %w", err)
	}
	if h.matchExprQ, err = gts.NewQuery(rsMatchExprQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile match-expr query: %w", err)
	}
	if h.matchArmStrQ, err = gts.NewQuery(rsMatchArmStrQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile match-arm-str query: %w", err)
	}
	if h.matchArmRawQ, err = gts.NewQuery(rsMatchArmRawStrQuery, lang); err != nil {
		return nil, fmt.Errorf("stringly-rs: compile match-arm-raw query: %w", err)
	}
	return h, nil
}

// parseRSFile parses src with the Rust grammar.
func parseRSFile(h *rsLangHandle, src []byte) (*gts.Tree, error) {
	parser := gts.NewParser(h.lang)
	return parser.Parse(src)
}

// ─── pass functions ───────────────────────────────────────────────────────────

// passRS_Consts extracts all top-level const NAME: &str = "literal" items.
// Only const_item nodes directly under source_file are producers (top-level).
func passRS_Consts(h *rsLangHandle, tree *gts.Tree, src []byte) []rsConstStr {
	var out []rsConstStr

	for _, m := range h.constStrQ.Execute(tree) {
		var name string
		var valNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "const.name":
				name = c.Node.Text(src)
			case "const.val":
				valNode = c.Node
			}
		}
		if name == "" || valNode == nil {
			continue
		}
		// Only top-level consts: parent must be source_file.
		// Walk up: const_item → source_file.
		constNode := valNode.Parent()
		if constNode == nil {
			continue
		}
		parent := constNode.Parent()
		if parent == nil || parent.Type(h.lang) != "source_file" {
			continue
		}
		val := unquoteRSString(valNode, h.lang, src)
		if val == "" {
			continue
		}
		line := int(valNode.StartPoint().Row) + 1
		out = append(out, rsConstStr{name: name, value: val, line: line})
	}

	// Also raw string literals.
	for _, m := range h.constRawStrQ.Execute(tree) {
		var name string
		var valNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "const.name":
				name = c.Node.Text(src)
			case "const.val":
				valNode = c.Node
			}
		}
		if name == "" || valNode == nil {
			continue
		}
		constNode := valNode.Parent()
		if constNode == nil {
			continue
		}
		parent := constNode.Parent()
		if parent == nil || parent.Type(h.lang) != "source_file" {
			continue
		}
		val := unquoteRSRawString(valNode, h.lang, src)
		if val == "" {
			continue
		}
		line := int(valNode.StartPoint().Row) + 1
		out = append(out, rsConstStr{name: name, value: val, line: line})
	}

	return out
}

// passRS_Bindings collects all scope-introducing bindings.
//
// isTypedStr=true: function parameter typed as &str.
// isTypedStr=false: closure param, for-loop var, let-shadow, if/while-let
//
//	destructure, match-arm capture — all are shadow sentinels.
func passRS_Bindings(h *rsLangHandle, tree *gts.Tree, src []byte) []rsBinding {
	var out []rsBinding
	typedKey := make(map[string]bool) // "name:scopeStart" → true for typed bindings

	// 1. &str-typed function parameters.
	for _, m := range h.typedParamQ.Execute(tree) {
		var paramName string
		var funcNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "param.name":
				paramName = c.Node.Text(src)
			case "param.func":
				funcNode = c.Node
			}
		}
		if paramName == "" || funcNode == nil {
			continue
		}
		scopeStart := funcNode.StartByte()
		scopeEnd := funcNode.EndByte()
		k := fmt.Sprintf("%s:%d", paramName, scopeStart)
		typedKey[k] = true
		out = append(out, rsBinding{
			name:       paramName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
			isTypedStr: true,
		})
	}

	// 2. Closure parameter identifiers — shadow sentinels.
	for _, m := range h.closureParamQ.Execute(tree) {
		var bindName string
		var scopeNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "any.name":
				bindName = c.Node.Text(src)
			case "any.scope":
				scopeNode = c.Node
			}
		}
		if bindName == "" || scopeNode == nil {
			continue
		}
		scopeStart := scopeNode.StartByte()
		scopeEnd := scopeNode.EndByte()
		k := fmt.Sprintf("%s:%d", bindName, scopeStart)
		if typedKey[k] {
			continue
		}
		out = append(out, rsBinding{
			name:       bindName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
			isTypedStr: false,
		})
	}

	// 3. For-loop variable — shadow sentinel.
	for _, m := range h.forLoopQ.Execute(tree) {
		var bindName string
		var scopeNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "any.name":
				bindName = c.Node.Text(src)
			case "any.scope":
				scopeNode = c.Node
			}
		}
		if bindName == "" || scopeNode == nil {
			continue
		}
		scopeStart := scopeNode.StartByte()
		scopeEnd := scopeNode.EndByte()
		k := fmt.Sprintf("%s:%d", bindName, scopeStart)
		if typedKey[k] {
			continue
		}
		out = append(out, rsBinding{
			name:       bindName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
			isTypedStr: false,
		})
	}

	// 4. Plain let_declaration (bare identifier pattern) — shadow sentinel.
	// We skip let declarations that have an explicit type annotation of &str;
	// those are typed bindings equivalent to parameters. In practice this is
	// rare in Rust (explicit let x: &str = "..." is more common in tests).
	// Conservative: treat all let_declaration as sentinels (the typed case
	// can only AVOID a false positive by making us emit nothing).
	for _, m := range h.letDeclQ.Execute(tree) {
		var bindName string
		var scopeNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "any.name":
				bindName = c.Node.Text(src)
			case "any.scope":
				scopeNode = c.Node
			}
		}
		if bindName == "" || scopeNode == nil {
			continue
		}
		// Scope for let_declaration is the enclosing block (parent of let_declaration).
		// Use the parent block as the scope span.
		scopeNode = enclosingRSBlock(scopeNode, h.lang)
		if scopeNode == nil {
			continue
		}
		scopeStart := scopeNode.StartByte()
		scopeEnd := scopeNode.EndByte()
		k := fmt.Sprintf("%s:%d", bindName, scopeStart)
		if typedKey[k] {
			continue
		}
		out = append(out, rsBinding{
			name:       bindName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
			isTypedStr: false,
		})
	}

	// 4b. let-else destructuring patterns: `let Some(cmd) = ... else { ... }`.
	// The bare-identifier rsLetDeclarationQuery (section 4) only matches
	// `let identifier = ...` patterns. `let Some(x) = ... else { ... }` has
	// a tuple_struct_pattern on the left, so `x` is invisible to section 4.
	// Fix: match let_declaration with tuple_struct_pattern and extract all
	// identifiers via collectRSPatternIdents (same traversal as if-let).
	// Scope: enclosing block of the let_declaration (same as section 4).
	for _, m := range h.letDeclPatQ.Execute(tree) {
		var patNode *gts.Node
		var declNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "let.pattern":
				patNode = c.Node
			case "let.decl":
				declNode = c.Node
			}
		}
		if patNode == nil || declNode == nil {
			continue
		}
		blockNode := enclosingRSBlock(declNode, h.lang)
		if blockNode == nil {
			continue
		}
		collectRSPatternIdents(patNode, h.lang, src, func(ident string) {
			scopeStart := blockNode.StartByte()
			scopeEnd := blockNode.EndByte()
			k := fmt.Sprintf("%s:%d", ident, scopeStart)
			if typedKey[k] {
				return
			}
			out = append(out, rsBinding{
				name:       ident,
				scopeStart: scopeStart,
				scopeEnd:   scopeEnd,
				isTypedStr: false,
			})
		})
	}

	// 5. if-let / while-let patterns: extract identifiers from the pattern.
	for _, m := range h.ifLetQ.Execute(tree) {
		var patNode *gts.Node
		var condNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "let.pattern":
				patNode = c.Node
			case "let.cond":
				condNode = c.Node
			}
		}
		if patNode == nil || condNode == nil {
			continue
		}
		// Scope: the block that is the body of the if/while expression.
		// Parent chain: let_condition → if_expression/while_expression → block.
		scopeNode := condNode.Parent()
		if scopeNode == nil {
			continue
		}
		// Find the block child of the if/while expression.
		var bodyBlock *gts.Node
		for i := range int(scopeNode.ChildCount()) {
			child := scopeNode.Child(i)
			if child.Type(h.lang) == "block" {
				bodyBlock = child
				break
			}
		}
		if bodyBlock == nil {
			continue
		}
		// Extract all identifiers from the pattern recursively.
		collectRSPatternIdents(patNode, h.lang, src, func(ident string) {
			scopeStart := bodyBlock.StartByte()
			scopeEnd := bodyBlock.EndByte()
			k := fmt.Sprintf("%s:%d", ident, scopeStart)
			if typedKey[k] {
				return
			}
			out = append(out, rsBinding{
				name:       ident,
				scopeStart: scopeStart,
				scopeEnd:   scopeEnd,
				isTypedStr: false,
			})
		})
	}

	// 6. Match-arm capture patterns (e.g. Foo::Bar(x) → x is bound).
	for _, m := range h.matchArmCapQ.Execute(tree) {
		var bindName string
		var armNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "any.name":
				bindName = c.Node.Text(src)
			case "any.scope":
				armNode = c.Node
			}
		}
		if bindName == "" || armNode == nil {
			continue
		}
		// Scope: the match_arm's body block.
		var bodyBlock *gts.Node
		for i := range int(armNode.ChildCount()) {
			child := armNode.Child(i)
			t := child.Type(h.lang)
			if t == "block" {
				bodyBlock = child
				break
			}
		}
		if bodyBlock == nil {
			// Arm body may be an expression without braces; use arm span.
			bodyBlock = armNode
		}
		scopeStart := bodyBlock.StartByte()
		scopeEnd := bodyBlock.EndByte()
		k := fmt.Sprintf("%s:%d", bindName, scopeStart)
		if typedKey[k] {
			continue
		}
		out = append(out, rsBinding{
			name:       bindName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
			isTypedStr: false,
		})
	}

	return out
}

// collectRSPatternIdents walks a pattern node and calls f for every bound
// identifier. Conservative: treats every bare identifier in the pattern as a
// potential binding. For `Some(x)` this yields "Some" and "x"; calling code
// uses this as a sentinel list so over-counting is precision-safe (we just
// suppress more matches, never emit false leads).
func collectRSPatternIdents(n *gts.Node, lang *gts.Language, src []byte, f func(string)) {
	if n == nil {
		return
	}
	if n.Type(lang) == "identifier" {
		f(n.Text(src))
		return
	}
	for i := range int(n.ChildCount()) {
		collectRSPatternIdents(n.Child(i), lang, src, f)
	}
}

// passRS_MatchExprs extracts match expressions with bare-identifier scrutinees
// and string literal arms.
func passRS_MatchExprs(h *rsLangHandle, tree *gts.Tree, src []byte) []rsMatchInfo {
	// Phase 1: collect match expressions with bare-identifier scrutinees.
	type matchKey struct {
		start uint32
	}
	byMatch := make(map[uint32]*rsMatchInfo)
	var order []uint32

	for _, m := range h.matchExprQ.Execute(tree) {
		var scrutinee string
		var exprNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "match.scrutinee":
				scrutinee = c.Node.Text(src)
			case "match.expr":
				exprNode = c.Node
			}
		}
		if scrutinee == "" || exprNode == nil {
			continue
		}
		// Exclude match expressions inside macro_definition (token_tree bodies).
		// Walk ancestors: if any ancestor is a macro_definition, skip.
		if insideRSMacroDef(exprNode, h.lang) {
			continue
		}
		key := exprNode.StartByte()
		if _, ok := byMatch[key]; !ok {
			byMatch[key] = &rsMatchInfo{
				scrutinee: scrutinee,
				exprStart: exprNode.StartByte(),
				exprEnd:   exprNode.EndByte(),
				exprLine:  int(exprNode.StartPoint().Row) + 1,
			}
			order = append(order, key)
		}
	}

	// Phase 2: collect string literal arm values.
	for _, m := range h.matchArmStrQ.Execute(tree) {
		var valNode *gts.Node
		var armNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "arm.val":
				valNode = c.Node
			case "arm.arm":
				armNode = c.Node
			}
		}
		if valNode == nil || armNode == nil {
			continue
		}
		// Find the enclosing match_expression.
		matchExpr := enclosingRSMatchExpr(armNode, h.lang)
		if matchExpr == nil {
			continue
		}
		key := matchExpr.StartByte()
		mi, ok := byMatch[key]
		if !ok {
			continue
		}
		val := unquoteRSString(valNode, h.lang, src)
		if val == "" {
			continue
		}
		line := int(valNode.StartPoint().Row) + 1
		mi.arms = append(mi.arms, rsArmLit{value: val, line: line})
	}

	// Phase 2b: raw string arms.
	for _, m := range h.matchArmRawQ.Execute(tree) {
		var valNode *gts.Node
		var armNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "arm.val":
				valNode = c.Node
			case "arm.arm":
				armNode = c.Node
			}
		}
		if valNode == nil || armNode == nil {
			continue
		}
		matchExpr := enclosingRSMatchExpr(armNode, h.lang)
		if matchExpr == nil {
			continue
		}
		key := matchExpr.StartByte()
		mi, ok := byMatch[key]
		if !ok {
			continue
		}
		val := unquoteRSRawString(valNode, h.lang, src)
		if val == "" {
			continue
		}
		line := int(valNode.StartPoint().Row) + 1
		mi.arms = append(mi.arms, rsArmLit{value: val, line: line})
	}

	// (Phase 3 removed: wildcard arm tracking was dead code. Every &str match
	// in Rust must have _ => ... to compile; hasWild would be true for every
	// match and carries no useful signal for type-A detection.)

	out := make([]rsMatchInfo, 0, len(order))
	for _, key := range order {
		mi := byMatch[key]
		if len(mi.arms) == 0 {
			continue
		}
		out = append(out, *mi)
	}
	return out
}

// ─── helper functions ─────────────────────────────────────────────────────────

// enclosingRSBlock walks up from n to find the nearest ancestor block node.
func enclosingRSBlock(n *gts.Node, lang *gts.Language) *gts.Node {
	cur := n.Parent()
	for cur != nil {
		if cur.Type(lang) == "block" {
			return cur
		}
		cur = cur.Parent()
	}
	return nil
}

// enclosingRSMatchExpr walks up from n to find the nearest ancestor
// match_expression node.
func enclosingRSMatchExpr(n *gts.Node, lang *gts.Language) *gts.Node {
	cur := n.Parent()
	for cur != nil {
		if cur.Type(lang) == "match_expression" {
			return cur
		}
		cur = cur.Parent()
	}
	return nil
}

// insideRSMacroDef reports whether node n is inside a macro_definition body.
// macro_rules! bodies parse their contents as token_tree, not as real AST
// nodes. We check ancestors for macro_definition.
func insideRSMacroDef(n *gts.Node, lang *gts.Language) bool {
	cur := n.Parent()
	for cur != nil {
		if cur.Type(lang) == "macro_definition" {
			return true
		}
		cur = cur.Parent()
	}
	return false
}

// unquoteRSString extracts the content from a string_literal node.
// The string_literal node has children: `"`, string_content?, `"`.
// We find the string_content child.
func unquoteRSString(n *gts.Node, lang *gts.Language, src []byte) string {
	for i := range int(n.ChildCount()) {
		child := n.Child(i)
		if child.Type(lang) == "string_content" {
			return child.Text(src)
		}
	}
	// Fallback: strip surrounding quotes from the full text.
	t := n.Text(src)
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		return t[1 : len(t)-1]
	}
	return ""
}

// unquoteRSRawString extracts content from a raw_string_literal node.
// Raw strings like r"content" or r#"content"# have a string_content child.
func unquoteRSRawString(n *gts.Node, lang *gts.Language, src []byte) string {
	for i := range int(n.ChildCount()) {
		child := n.Child(i)
		if child.Type(lang) == "string_content" {
			return child.Text(src)
		}
	}
	return ""
}

// ─── join pass ────────────────────────────────────────────────────────────────

// joinRSDrift performs the type-A join for one Rust file.
//
// Algorithm:
//  1. Build the producer set: all file-level const &str literal values.
//  2. For each match expression:
//     a. Find the NEAREST binding (smallest scope span containing the match)
//     whose name matches the scrutinee.
//     b. If nearest binding is isTypedStr=true: proceed. Otherwise: skip.
//     c. ANCHOR CHECK: at least one arm literal must already be in the
//     producer pool. If zero arms overlap the pool, the match is NOT a
//     dispatch over the const-defined domain (e.g. subcommand dispatch
//     against VERSION = "1.4.2", HTTP verb dispatch against COLOR_RED =
//     "#ff0000"). Require ≥1 overlap; a single hit is sufficient because
//     it confirms the match is dispatching over the same domain as the
//     producers, and the remaining non-matching arms are candidate typos.
//     Requiring majority would silence "2 correct + 1 typo" matches.
//     d. For each string arm literal NOT in the producer set: emit a type-A
//     lead. (Wildcard arms are not string literals and are naturally skipped.)
func joinRSDrift(
	path string,
	consts []rsConstStr,
	bindings []rsBinding,
	matches []rsMatchInfo,
) []store.Lead {
	if len(consts) == 0 || len(bindings) == 0 || len(matches) == 0 {
		return nil
	}

	// Build producer value set.
	producerValues := make(map[string]bool, len(consts))
	for _, c := range consts {
		producerValues[c.value] = true
	}

	var leads []store.Lead

	for _, mi := range matches {
		// Find nearest binding for the scrutinee.
		nearest := nearestRSBinding(mi.scrutinee, mi.exprStart, mi.exprEnd, bindings)
		if nearest == nil || !nearest.isTypedStr {
			// No typed binding resolves this scrutinee — skip.
			continue
		}

		// Anchor check: require ≥1 arm literal to be in the producer pool.
		// A match whose arms share zero values with the pool is not dispatching
		// over the const-defined domain — it is a coincidental &str parameter
		// being matched against an unrelated set of literals. Without this
		// anchor, const VERSION="1.4.2" + subcommand dispatch = 3 FPs.
		var overlap int
		for _, arm := range mi.arms {
			if producerValues[arm.value] {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}

		// Type-A: arm literals not in producer set.
		for _, arm := range mi.arms {
			if producerValues[arm.value] {
				continue
			}
			note := fmt.Sprintf(
				"match arm %q on &str scrutinee %q not in file-local const &str producer set (%d values); possible typo or stale literal",
				arm.value, mi.scrutinee, len(producerValues),
			)
			leads = append(leads, store.Lead{
				PosterLens: stringlyRsPosterLens,
				TargetLens: stringlyRsTargetLens,
				File:       path,
				Line:       arm.line,
				Note:       note,
			})
		}
	}

	return leads
}

// nearestRSBinding finds the binding with the given name whose scope
// [scopeStart, scopeEnd) contains [exprStart, exprEnd) and has the smallest
// span (scopeEnd - scopeStart). Returns nil if no such binding exists.
func nearestRSBinding(name string, exprStart, exprEnd uint32, bindings []rsBinding) *rsBinding {
	var best *rsBinding
	bestSpan := uint32(0)
	for i := range bindings {
		b := &bindings[i]
		if b.name != name {
			continue
		}
		if b.scopeStart > exprStart || b.scopeEnd < exprEnd {
			continue
		}
		span := b.scopeEnd - b.scopeStart
		if best == nil || span < bestSpan {
			best = b
			bestSpan = span
		}
	}
	return best
}

// ─── test-file gate ───────────────────────────────────────────────────────────

// isRSTestPath reports whether a repo-relative file path looks like a Rust
// test file. Rust test files follow three conventions:
//  1. Files in a tests/ directory (integration tests).
//  2. Files named *_test.rs (rare but exists in some projects).
//  3. Files in any path segment named "tests" or "testdata".
//
// We do NOT look for #[cfg(test)] inline modules — that requires an AST pass
// and the inline test modules are inside otherwise-normal source files, so
// they do not constitute "test files" in the same sense. The miner will
// process those files but the match expressions inside #[cfg(test)] blocks
// are typically not over file-level const producers, so they generate no leads
// in practice.
func isRSTestPath(relPath string) bool {
	slashed := filepath.ToSlash(relPath)
	for _, seg := range strings.Split(slashed, "/") {
		switch seg {
		case "tests", "testdata", "benches":
			return true
		}
	}
	base := strings.ToLower(filepath.Base(slashed))
	return strings.HasSuffix(base, "_test.rs")
}

// ─── seed entrypoint ──────────────────────────────────────────────────────────

// seedStringlyRsDrift runs the Rust &str match-drift miner.
func seedStringlyRsDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	h, err := loadRSLangHandle()
	if err != nil {
		// Grammar unavailable — degrade silently.
		return nil
	}

	leadsPosted := 0

	for _, f := range snap.Files {
		if f.Language != ingest.LangRust {
			continue
		}
		if isRSTestPath(f.Path) {
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
		src, err := os.ReadFile(abs)
		if err != nil {
			continue
		}

		tree, err := parseRSFile(h, src)
		if err != nil || tree == nil {
			continue
		}
		// Skip files whose parse tree contains errors.
		// The Rust grammar fails on ~7.1% of real files (see rust_sweep_test.go).
		if tree.RootNode().HasError() {
			sum.RsParseFailures++
			tree.Release()
			continue
		}

		consts := passRS_Consts(h, tree, src)
		if len(consts) == 0 {
			tree.Release()
			continue
		}

		bindings := passRS_Bindings(h, tree, src)
		matches := passRS_MatchExprs(h, tree, src)
		tree.Release()

		leads := joinRSDrift(f.Path, consts, bindings, matches)
		for _, lead := range leads {
			if err := st.AddLead(ctx, lead); err != nil {
				return fmt.Errorf("miner: stringly-rs lead %s: %w", lead.File, err)
			}
			sum.StringlyRsDriftLeads++
			sum.LeadsPosted++
			leadsPosted++
			if leadsPosted >= maxRsLeads {
				return nil
			}
		}
	}
	return nil
}
