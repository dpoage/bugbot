package miner

// Python string-union drift miner (miner:stringly-py-drift).
//
// # Design
//
// Mirrors stringly_ts.go's tree-sitter approach with all three oracle-round
// precision lessons applied from birth:
//
//  1. Nearest-binding scope resolution over ALL binding kinds (typed/untyped
//     function params incl. typed_parameter and typed_default_parameter, lambda
//     parameters, for/with/except targets, for_in_clause comprehension vars,
//     annotated assignments). Any untyped binding nearer than the typed one
//     suppresses the switch/match.
//
//  2. Structural whitelist (not blocklist): Literal[...] subscripts must have
//     ALL subscript children be string nodes. Any integer, float, None, bool,
//     or identifier child marks the type as mixed and it is excluded.
//
//  3. Test-file gating: test_*.py, *_test.py, and files under tests/testdata
//     directories are skipped. HasError() tree guard + Summary counter.
//
// # Producer detection
//
//   - Assignment form:  Status = Literal['a', 'b']   (at module or class level)
//   - Annotation form:  type Status = Literal['a', 'b'] (Python 3.12 type alias)
//   - StrEnum subclass: class X(StrEnum): ...         (all str-valued members)
//   - Enum/str mixin:   class X(str, Enum): ...       (all str-valued members)
//   - Pure Enum:        class X(Enum): ...            (all members with string values)
//
// # Consumer detection
//
//   - if/elif == 'lit' chains: if x == 'a': ... elif x == 'b': ...
//   - match statements:        match x: case 'a': ...
//
// # Scope resolution
//
// For each consumer (if-chain or match), the scrutinee identifier is looked
// up against all collected bindings. The NEAREST binding (smallest scope span
// enclosing the consumer) wins. If that binding carries a known producer type,
// the join proceeds; otherwise the consumer is skipped (precision-first).
//
// # Type-A / Type-B
//
//   - Type-A: a case/branch literal not present in the producer's member set.
//   - Type-B: a producer member never handled by any case/branch.
//     Suppressed when the if-chain has an else clause or the match has a
//     wildcard case (`case _:`).
//
// # Known limitations (v1)
//
//   - Only pure-identifier scrutinees are resolved (no attribute access,
//     no subscript, no call). This is conservative-safe.
//   - Literal from imported symbol not tracked across files.
//   - StrEnum members with non-string values (e.g. mixin classes with int
//     fields) are excluded by the structural whitelist.
//
// Leads: PosterLens="miner:stringly-py-drift", TargetLens="api-contract-misuse".

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
	stringlyPyPosterLens = "miner:stringly-py-drift"
	stringlyPyTargetLens = "api-contract-misuse"

	// maxPyLeads caps leads posted by this pass per Seed run.
	maxPyLeads = 50
)

// ─── query S-expressions ─────────────────────────────────────────────────────

// pyLiteralAssignQuery finds module/class-level assignments of the form:
//
//	Status = Literal['a', 'b']
//
// We capture the assignment node (@type.decl) and the left identifier (@type.name).
// Member extraction is done via a separate query to handle multiple members.
const pyLiteralAssignQuery = `
(assignment
  left: (identifier) @type.name
  right: (subscript
    value: (identifier) @lit.id)) @type.decl
`

// pyLiteralMemberQuery extracts all string-node subscript children from a
// Literal[...] subscript. We associate by byte containment in Go code.
const pyLiteralMemberQuery = `
(subscript
  value: (identifier) @lit.id
  subscript: (string) @member.val)
`

// pyEnumClassQuery finds class definitions with a base class (for StrEnum,
// Enum, etc.). We examine bases and members in Go code.
const pyEnumClassQuery = `
(class_definition
  name: (identifier) @class.name
  superclasses: (argument_list)) @class.def
`

// pyEnumMemberQuery extracts string-valued assignments within a class body.
const pyEnumMemberQuery = `
(class_definition
  name: (identifier) @class.name
  body: (block
    (assignment
      left: (identifier) @member.name
      right: (string) @member.val)))
`

// pyTypedParamQuery finds typed function/method parameters: typed_parameter.
// Captures: "param.name", "param.type", "param.func".
const pyTypedParamQuery = `
(function_definition
  parameters: (parameters
    (typed_parameter
      (identifier) @param.name
      type: (type (identifier) @param.type)))) @param.func
`

// pyTypedDefaultParamQuery finds typed parameters with defaults.
const pyTypedDefaultParamQuery = `
(function_definition
  parameters: (parameters
    (typed_default_parameter
      name: (identifier) @param.name
      type: (type (identifier) @param.type)))) @param.func
`

// pyAnyParamQuery finds ALL scope-introducing identifier bindings:
// untyped function params, typed params (to catch shadow sentinels even
// when the type is unknown), lambda params, for targets, with targets,
// except targets, comprehension vars.
//
// All captures use "any.name" (binding identifier) and "any.scope" (scope node).
const pyAnyParamQuery = `
(function_definition
  parameters: (parameters
    (identifier) @any.name)) @any.scope
(function_definition
  parameters: (parameters
    (typed_parameter
      (identifier) @any.name))) @any.scope
(function_definition
  parameters: (parameters
    (typed_default_parameter
      name: (identifier) @any.name))) @any.scope
(lambda
  parameters: (lambda_parameters (identifier) @any.name)) @any.scope
(for_statement
  left: (identifier) @any.name) @any.scope
(with_statement
  (with_clause
    (with_item
      (as_pattern
        (as_pattern_target (identifier) @any.name))))) @any.scope
(except_clause
  (as_pattern
    (as_pattern_target (identifier) @any.name))) @any.scope
(for_in_clause
  left: (identifier) @any.name) @any.scope
`

// pyAnnotatedVarQuery finds annotated variable assignments (module or function
// level): `x: SomeType` or `x: SomeType = value`. These introduce a typed
// binding that we track for scope resolution.
const pyAnnotatedVarQuery = `
(assignment
  left: (identifier) @var.name
  type: (type (identifier) @var.type))
`

// pyIfChainQuery finds if/elif comparisons of the form `x == 'lit'` or
// `'lit' == x`. We capture the if_statement node and the scrutinee+literal.
const pyIfChainQuery = `
(if_statement
  condition: (comparison_operator
    (identifier) @if.scrutinee
    (string) @if.lit)) @if.stmt
`

// pyIfChainRevQuery handles reversed form: `'lit' == x`.
const pyIfChainRevQuery = `
(if_statement
  condition: (comparison_operator
    (string) @if.lit
    (identifier) @if.scrutinee)) @if.stmt
`

// pyElifQuery finds elif comparisons.
const pyElifQuery = `
(elif_clause
  condition: (comparison_operator
    (identifier) @elif.scrutinee
    (string) @elif.lit)) @elif.clause
`

// pyElifRevQuery handles reversed elif: `'lit' == x`.
const pyElifRevQuery = `
(elif_clause
  condition: (comparison_operator
    (string) @elif.lit
    (identifier) @elif.scrutinee)) @elif.clause
`

// pyElseQuery detects if_statement nodes that have an else_clause.
const pyElseQuery = `
(if_statement (else_clause) @else.clause) @if.stmt
`

// pyMatchQuery finds match statements with string case patterns.
const pyMatchQuery = `
(match_statement
  subject: (identifier) @match.scrutinee
  body: (block
    (case_clause
      (case_pattern (string) @case.lit)))) @match.stmt
`

// pyMatchWildcardQuery finds match statements that have a wildcard (case _:).
const pyMatchWildcardQuery = `
(match_statement
  body: (block
    (case_clause
      (case_pattern (_))))) @match.stmt
`

// ─── data types ──────────────────────────────────────────────────────────────

// pyProducerType is a closed string-literal set found in a Python file.
type pyProducerType struct {
	name      string          // type alias or class name
	members   map[string]bool // set of string values (unquoted)
	line      int             // 1-based line of declaration
	startByte uint32
	endByte   uint32
}

// pyBinding records a scope-introducing binding for scrutinee resolution.
type pyBinding struct {
	name         string
	typeName     string // non-empty only for typed bindings with known producer type
	scopeStart   uint32
	scopeEnd     uint32
	isTypedUnion bool // true iff typeName names a known producer type
}

// pyIfChain records one if/elif chain dispatching on a bare identifier.
type pyIfChain struct {
	scrutinee string
	stmtByte  uint32
	stmtLine  int
	hasElse   bool
	cases     []pyCaseLit
}

// pyMatchStmt records one match statement.
type pyMatchStmt struct {
	scrutinee   string
	stmtByte    uint32
	stmtLine    int
	hasWildcard bool
	cases       []pyCaseLit
}

type pyCaseLit struct {
	value string // unquoted string value
	line  int    // 1-based
}

// ─── language handle cache ────────────────────────────────────────────────────

type pyLangHandle struct {
	lang               *gts.Language
	literalAssignQ     *gts.Query
	literalMemberQ     *gts.Query
	enumClassQ         *gts.Query
	enumMemberQ        *gts.Query
	typedParamQ        *gts.Query
	typedDefaultParamQ *gts.Query
	anyParamQ          *gts.Query
	annotatedVarQ      *gts.Query
	ifChainQ           *gts.Query
	ifChainRevQ        *gts.Query
	elifQ              *gts.Query
	elifRevQ           *gts.Query
	elseQ              *gts.Query
	matchQ             *gts.Query
	matchWildcardQ     *gts.Query
}

var pyLangHandleCache *pyLangHandle

func loadPyLangHandle() (*pyLangHandle, error) {
	if pyLangHandleCache != nil {
		return pyLangHandleCache, nil
	}
	entry := tsregistry.DetectLanguage("x.py")
	if entry == nil {
		return nil, fmt.Errorf("stringly-py: no grammar for x.py")
	}
	lang := entry.Language()
	h := &pyLangHandle{lang: lang}
	var err error
	compile := func(name, q string) *gts.Query {
		if err != nil {
			return nil
		}
		query, e := gts.NewQuery(q, lang)
		if e != nil {
			err = fmt.Errorf("stringly-py: compile %s query: %w", name, e)
			return nil
		}
		return query
	}
	h.literalAssignQ = compile("literal-assign", pyLiteralAssignQuery)
	h.literalMemberQ = compile("literal-member", pyLiteralMemberQuery)
	h.enumClassQ = compile("enum-class", pyEnumClassQuery)
	h.enumMemberQ = compile("enum-member", pyEnumMemberQuery)
	h.typedParamQ = compile("typed-param", pyTypedParamQuery)
	h.typedDefaultParamQ = compile("typed-default-param", pyTypedDefaultParamQuery)
	h.anyParamQ = compile("any-param", pyAnyParamQuery)
	h.annotatedVarQ = compile("annotated-var", pyAnnotatedVarQuery)
	h.ifChainQ = compile("if-chain", pyIfChainQuery)
	h.ifChainRevQ = compile("if-chain-rev", pyIfChainRevQuery)
	h.elifQ = compile("elif", pyElifQuery)
	h.elifRevQ = compile("elif-rev", pyElifRevQuery)
	h.elseQ = compile("else", pyElseQuery)
	h.matchQ = compile("match", pyMatchQuery)
	h.matchWildcardQ = compile("match-wildcard", pyMatchWildcardQuery)
	if err != nil {
		return nil, err
	}
	pyLangHandleCache = h
	return h, nil
}

// ─── pass functions ───────────────────────────────────────────────────────────

func parsePyFile(h *pyLangHandle, src []byte) (*gts.Tree, error) {
	parser := gts.NewParser(h.lang)
	return parser.Parse(src)
}

// passGo_PyProducers extracts all pure-string producer types from a Python file.
//
// Two forms:
//  1. Assignment: `Status = Literal['a', 'b']` — structural whitelist: ALL
//     subscript children must be string nodes; any integer/float/identifier
//     marks the type as mixed and excludes it.
//  2. Enum classes: StrEnum subclass, str+Enum mixin, or Enum class whose
//     ALL assignments in the class body have string RHS values.
func passPy_Producers(h *pyLangHandle, tree *gts.Tree, src []byte) []pyProducerType {
	var out []pyProducerType

	// ── Step 1: Literal[...] assignment producers ────────────────────────────

	type literalDecl struct {
		name      string
		line      int
		startByte uint32
		endByte   uint32
	}
	var literalDecls []literalDecl

	for _, m := range h.literalAssignQ.Execute(tree) {
		var name, litID string
		var declNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "type.name":
				name = c.Node.Text(src)
			case "lit.id":
				litID = c.Node.Text(src)
			case "type.decl":
				declNode = c.Node
			}
		}
		if name == "" || litID != "Literal" || declNode == nil {
			continue
		}
		literalDecls = append(literalDecls, literalDecl{
			name:      name,
			line:      int(declNode.StartPoint().Row) + 1,
			startByte: declNode.StartByte(),
			endByte:   declNode.EndByte(),
		})
	}

	if len(literalDecls) > 0 {
		// Collect all string members by containment, and detect mixed types.
		// membersByDecl: name → set of unquoted string values
		membersByDecl := make(map[string]map[string]bool, len(literalDecls))
		// mixedDecls: name → true if any non-string subscript child found
		mixedDecls := make(map[string]bool)

		for _, m := range h.literalMemberQ.Execute(tree) {
			var litID, memberVal string
			var memberNode *gts.Node
			for _, c := range m.Captures {
				switch c.Name {
				case "lit.id":
					litID = c.Node.Text(src)
				case "member.val":
					memberNode = c.Node
					memberVal = c.Node.Text(src)
				}
			}
			if litID != "Literal" || memberNode == nil {
				continue
			}
			mb := memberNode.StartByte()
			// Find best (narrowest) containing literalDecl.
			var best *literalDecl
			for i := range literalDecls {
				d := &literalDecls[i]
				if d.startByte <= mb && mb < d.endByte {
					if best == nil || (d.endByte-d.startByte) < (best.endByte-best.startByte) {
						best = d
					}
				}
			}
			if best == nil {
				continue
			}
			unquoted := unquotePyString(memberVal)
			if unquoted == "" {
				continue
			}
			if membersByDecl[best.name] == nil {
				membersByDecl[best.name] = make(map[string]bool)
			}
			membersByDecl[best.name][unquoted] = true
		}

		// Structural whitelist: walk each declaration's subscript node and
		// verify that every named child (after the identifier) is a string.
		// We find the RHS subscript node by walking the tree.
		for i := range literalDecls {
			d := &literalDecls[i]
			if mixedDecls[d.name] {
				continue
			}
			// Find the subscript node within d's byte range using the tree.
			if !isPurePyStringLiteral(tree.RootNode(), h.lang, src, d.startByte, d.endByte) {
				mixedDecls[d.name] = true
			}
		}

		seen := make(map[string]bool)
		for _, d := range literalDecls {
			if seen[d.name] || mixedDecls[d.name] {
				continue
			}
			seen[d.name] = true
			members := membersByDecl[d.name]
			if len(members) < 2 {
				continue // trivial, not enum-style
			}
			out = append(out, pyProducerType{
				name:      d.name,
				members:   members,
				line:      d.line,
				startByte: d.startByte,
				endByte:   d.endByte,
			})
		}
	}

	// ── Step 2: Enum/StrEnum class producers ─────────────────────────────────

	// Collect class bases to determine if a class is an enum-like producer.
	// classBases: className → set of base names
	classBases := make(map[string][]string)
	// classExtents: className → (startByte, endByte, line)
	type classExtent struct {
		startByte, endByte uint32
		line               int
	}
	classExtents := make(map[string]classExtent)

	for _, m := range h.enumClassQ.Execute(tree) {
		var className string
		var classDef *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "class.name":
				className = c.Node.Text(src)
			case "class.def":
				classDef = c.Node
			}
		}
		if className == "" || classDef == nil {
			continue
		}
		// Extract base names from argument_list children.
		argList := findChildByType(classDef, h.lang, "argument_list")
		if argList == nil {
			continue
		}
		var bases []string
		for i := 0; i < argList.ChildCount(); i++ {
			ch := argList.Child(i)
			if ch == nil || !ch.IsNamed() {
				continue
			}
			if ch.Type(h.lang) == "identifier" {
				bases = append(bases, ch.Text(src))
			}
		}
		classBases[className] = bases
		classExtents[className] = classExtent{
			startByte: classDef.StartByte(),
			endByte:   classDef.EndByte(),
			line:      int(classDef.StartPoint().Row) + 1,
		}
	}

	// Collect string-valued member assignments per class.
	classMembersStr := make(map[string]map[string]bool)
	classHasNonStr := make(map[string]bool)

	for _, m := range h.enumMemberQ.Execute(tree) {
		var className, memberName, memberVal string
		for _, c := range m.Captures {
			switch c.Name {
			case "class.name":
				className = c.Node.Text(src)
			case "member.name":
				memberName = c.Node.Text(src)
			case "member.val":
				memberVal = c.Node.Text(src)
			}
		}
		if className == "" || memberName == "" || memberVal == "" {
			continue
		}
		unquoted := unquotePyString(memberVal)
		if unquoted == "" {
			continue
		}
		if classMembersStr[className] == nil {
			classMembersStr[className] = make(map[string]bool)
		}
		classMembersStr[className][unquoted] = true
	}

	// Also detect classes that have NON-string assignment members.
	// We do this by walking each class body and checking for assignments
	// whose RHS is not a string. This is the structural whitelist for enums.
	for className, ext := range classExtents {
		// Find the class_definition node in the tree.
		classNode := findNodeAtByte(tree.RootNode(), h.lang, "class_definition", ext.startByte)
		if classNode == nil {
			continue
		}
		body := findChildByType(classNode, h.lang, "block")
		if body == nil {
			continue
		}
		for i := 0; i < body.ChildCount(); i++ {
			ch := body.Child(i)
			if ch == nil || !ch.IsNamed() {
				continue
			}
			if ch.Type(h.lang) != "assignment" {
				continue
			}
			// Check RHS: must be a string.
			rhs := ch.ChildByFieldName("right", h.lang)
			if rhs == nil {
				continue
			}
			if rhs.Type(h.lang) != "string" {
				classHasNonStr[className] = true
				break
			}
		}
	}

	for className, bases := range classBases {
		if !isPyEnumBase(bases) {
			continue
		}
		// If ALL members are strings and there are ≥2, it's a producer.
		if classHasNonStr[className] {
			continue
		}
		members := classMembersStr[className]
		if len(members) < 2 {
			continue
		}
		ext := classExtents[className]
		out = append(out, pyProducerType{
			name:      className,
			members:   members,
			line:      ext.line,
			startByte: ext.startByte,
			endByte:   ext.endByte,
		})
	}

	return out
}

// isPyEnumBase reports whether a list of base class names qualifies as an
// enum-like producer: StrEnum, or str+Enum mixin, or plain Enum.
func isPyEnumBase(bases []string) bool {
	baseSet := make(map[string]bool, len(bases))
	for _, b := range bases {
		baseSet[b] = true
	}
	if baseSet["StrEnum"] {
		return true
	}
	if baseSet["str"] && baseSet["Enum"] {
		return true
	}
	if baseSet["Enum"] {
		return true
	}
	return false
}

// isPurePyStringLiteral walks the subtree rooted at the module node looking
// for the first subscript node within [startByte, endByte), then verifies
// that every named child (except the leading identifier and syntax tokens)
// is a string node. Returns false if any non-string named child is found.
func isPurePyStringLiteral(node *gts.Node, lang *gts.Language, src []byte, startByte, endByte uint32) bool {
	// Find the assignment node at startByte, then descend to subscript.
	assignNode := findNodeAtByte(node, lang, "assignment", startByte)
	if assignNode == nil {
		return false
	}
	// RHS is the subscript.
	rhs := assignNode.ChildByFieldName("right", lang)
	if rhs == nil || rhs.Type(lang) != "subscript" {
		return false
	}
	// Named children of subscript: first is the identifier (Literal), rest are members.
	// We skip the first named child (the function identifier).
	first := true
	for i := 0; i < rhs.ChildCount(); i++ {
		ch := rhs.Child(i)
		if ch == nil || !ch.IsNamed() {
			continue
		}
		if first {
			first = false
			continue // skip identifier node
		}
		if ch.Type(lang) != "string" {
			return false // non-string member → mixed
		}
	}
	return true
}

// findChildByType returns the first direct child of node whose type matches.
func findChildByType(node *gts.Node, lang *gts.Language, typ string) *gts.Node {
	if node == nil {
		return nil
	}
	for i := 0; i < node.ChildCount(); i++ {
		ch := node.Child(i)
		if ch != nil && ch.Type(lang) == typ {
			return ch
		}
	}
	return nil
}

// findNodeAtByte finds the first named node of the given type within the tree
// whose start byte equals startByte (or is closest to it). Used for precise
// structural access when we know the exact location from a query result.
func findNodeAtByte(root *gts.Node, lang *gts.Language, typ string, startByte uint32) *gts.Node {
	if root == nil {
		return nil
	}
	if root.IsNamed() && root.Type(lang) == typ && root.StartByte() == startByte {
		return root
	}
	for i := 0; i < root.ChildCount(); i++ {
		ch := root.Child(i)
		if ch == nil {
			continue
		}
		// Only descend into nodes that contain startByte.
		if ch.StartByte() <= startByte && ch.EndByte() > startByte {
			if result := findNodeAtByte(ch, lang, typ, startByte); result != nil {
				return result
			}
		}
	}
	return nil
}

// passPy_Bindings collects ALL scope-introducing bindings.
//
// Strategy mirrors passTS_Bindings: typed union params get isTypedUnion=true;
// all others are shadow sentinels with isTypedUnion=false.
func passPy_Bindings(h *pyLangHandle, tree *gts.Tree, src []byte, knownTypes map[string]bool) []pyBinding {
	if len(knownTypes) == 0 {
		return nil
	}
	var out []pyBinding

	// ── Typed params (typed_parameter and typed_default_parameter) ────────────

	typedKey := make(map[string]bool) // "name:scopeStart" → already typed
	collectTyped := func(q *gts.Query) {
		for _, m := range q.Execute(tree) {
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
				return
			}
			if !knownTypes[paramType] {
				return
			}
			k := fmt.Sprintf("%s:%d", paramName, funcStart)
			if typedKey[k] {
				return
			}
			typedKey[k] = true
			out = append(out, pyBinding{
				name:         paramName,
				typeName:     paramType,
				scopeStart:   funcStart,
				scopeEnd:     funcEnd,
				isTypedUnion: true,
			})
		}
	}
	collectTyped(h.typedParamQ)
	collectTyped(h.typedDefaultParamQ)

	// ── Annotated variable assignments: `x: KnownType` ───────────────────────
	for _, m := range h.annotatedVarQ.Execute(tree) {
		var varName, varType string
		var varNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "var.name":
				varName = c.Node.Text(src)
				varNode = c.Node
			case "var.type":
				varType = c.Node.Text(src)
			}
		}
		if varName == "" || varType == "" || varNode == nil {
			continue
		}
		if !knownTypes[varType] {
			continue
		}
		// Scope: the parent function or module.
		scope := enclosingPyScope(varNode, h.lang)
		k := fmt.Sprintf("%s:%d", varName, scope.StartByte())
		if typedKey[k] {
			continue
		}
		typedKey[k] = true
		out = append(out, pyBinding{
			name:         varName,
			typeName:     varType,
			scopeStart:   scope.StartByte(),
			scopeEnd:     scope.EndByte(),
			isTypedUnion: true,
		})
	}

	// ── All other bindings (shadow sentinels) ─────────────────────────────────
	for _, m := range h.anyParamQ.Execute(tree) {
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
			continue // already recorded as typed union param
		}
		out = append(out, pyBinding{
			name:         bindName,
			scopeStart:   scopeStart,
			scopeEnd:     scopeEnd,
			isTypedUnion: false,
		})
	}

	return out
}

// enclosingPyScope finds the nearest function_definition or module ancestor
// of node, to use as the scope for annotated variable bindings.
func enclosingPyScope(node *gts.Node, lang *gts.Language) *gts.Node {
	cur := node.Parent()
	for cur != nil {
		switch cur.Type(lang) {
		case "function_definition", "module":
			return cur
		}
		cur = cur.Parent()
	}
	return node // fallback: use node itself
}

// passPy_IfChains extracts if/elif chains that compare a bare identifier
// to string literals.
func passPy_IfChains(h *pyLangHandle, tree *gts.Tree, src []byte) []pyIfChain {
	// Map: if_statement startByte → *pyIfChain
	byStmt := make(map[uint32]*pyIfChain)
	var order []uint32

	addCase := func(stmtKey uint32, stmtLine int, scrutinee string, litNode *gts.Node, litVal string) {
		if _, ok := byStmt[stmtKey]; !ok {
			byStmt[stmtKey] = &pyIfChain{
				scrutinee: scrutinee,
				stmtByte:  stmtKey,
				stmtLine:  stmtLine,
			}
			order = append(order, stmtKey)
		}
		entry := byStmt[stmtKey]
		if entry.scrutinee != scrutinee {
			// Mixed scrutinees in the chain — not a single-variable dispatch.
			entry.scrutinee = ""
			return
		}
		unquoted := unquotePyString(litVal)
		if unquoted == "" {
			return
		}
		entry.cases = append(entry.cases, pyCaseLit{
			value: unquoted,
			line:  int(litNode.StartPoint().Row) + 1,
		})
	}

	// Process if conditions.
	for _, m := range h.ifChainQ.Execute(tree) {
		var scrutinee, litVal string
		var stmtNode, litNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "if.scrutinee":
				scrutinee = c.Node.Text(src)
			case "if.lit":
				litNode = c.Node
				litVal = c.Node.Text(src)
			case "if.stmt":
				stmtNode = c.Node
			}
		}
		if stmtNode == nil || scrutinee == "" || litNode == nil {
			continue
		}
		addCase(stmtNode.StartByte(), int(stmtNode.StartPoint().Row)+1, scrutinee, litNode, litVal)
	}
	for _, m := range h.ifChainRevQ.Execute(tree) {
		var scrutinee, litVal string
		var stmtNode, litNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "if.scrutinee":
				scrutinee = c.Node.Text(src)
			case "if.lit":
				litNode = c.Node
				litVal = c.Node.Text(src)
			case "if.stmt":
				stmtNode = c.Node
			}
		}
		if stmtNode == nil || scrutinee == "" || litNode == nil {
			continue
		}
		addCase(stmtNode.StartByte(), int(stmtNode.StartPoint().Row)+1, scrutinee, litNode, litVal)
	}

	// Process elif conditions.
	for _, m := range h.elifQ.Execute(tree) {
		var scrutinee, litVal string
		var elifNode, litNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "elif.scrutinee":
				scrutinee = c.Node.Text(src)
			case "elif.lit":
				litNode = c.Node
				litVal = c.Node.Text(src)
			case "elif.clause":
				elifNode = c.Node
			}
		}
		if elifNode == nil || scrutinee == "" || litNode == nil {
			continue
		}
		// Parent of elif_clause is the if_statement.
		stmtNode := elifNode.Parent()
		if stmtNode == nil {
			continue
		}
		addCase(stmtNode.StartByte(), int(stmtNode.StartPoint().Row)+1, scrutinee, litNode, litVal)
	}
	for _, m := range h.elifRevQ.Execute(tree) {
		var scrutinee, litVal string
		var elifNode, litNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "elif.scrutinee":
				scrutinee = c.Node.Text(src)
			case "elif.lit":
				litNode = c.Node
				litVal = c.Node.Text(src)
			case "elif.clause":
				elifNode = c.Node
			}
		}
		if elifNode == nil || scrutinee == "" || litNode == nil {
			continue
		}
		stmtNode := elifNode.Parent()
		if stmtNode == nil {
			continue
		}
		addCase(stmtNode.StartByte(), int(stmtNode.StartPoint().Row)+1, scrutinee, litNode, litVal)
	}

	// Detect else clauses.
	elseKeys := make(map[uint32]bool)
	for _, m := range h.elseQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name == "if.stmt" && c.Node != nil {
				elseKeys[c.Node.StartByte()] = true
			}
		}
	}

	out := make([]pyIfChain, 0, len(order))
	for _, key := range order {
		e := byStmt[key]
		if e.scrutinee == "" || len(e.cases) == 0 {
			continue
		}
		e.hasElse = elseKeys[key]
		out = append(out, *e)
	}
	return out
}

// passPy_MatchStmts extracts match statements with string case patterns.
func passPy_MatchStmts(h *pyLangHandle, tree *gts.Tree, src []byte) []pyMatchStmt {
	byStmt := make(map[uint32]*pyMatchStmt)
	var order []uint32

	for _, m := range h.matchQ.Execute(tree) {
		var scrutinee, litVal string
		var stmtNode, litNode *gts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "match.scrutinee":
				scrutinee = c.Node.Text(src)
			case "case.lit":
				litNode = c.Node
				litVal = c.Node.Text(src)
			case "match.stmt":
				stmtNode = c.Node
			}
		}
		if stmtNode == nil || scrutinee == "" || litNode == nil {
			continue
		}
		key := stmtNode.StartByte()
		if _, ok := byStmt[key]; !ok {
			byStmt[key] = &pyMatchStmt{
				scrutinee: scrutinee,
				stmtByte:  key,
				stmtLine:  int(stmtNode.StartPoint().Row) + 1,
			}
			order = append(order, key)
		}
		entry := byStmt[key]
		unquoted := unquotePyString(litVal)
		if unquoted == "" {
			continue
		}
		entry.cases = append(entry.cases, pyCaseLit{
			value: unquoted,
			line:  int(litNode.StartPoint().Row) + 1,
		})
	}

	// Detect wildcard (case _:) — the match_wildcard query fires for every
	// case_clause whose case_pattern has any child; the wildcard pattern
	// has an anonymous `_` child. We detect it by checking if there's a
	// case_clause with no string in its pattern.
	wildcardKeys := make(map[uint32]bool)
	for _, m := range h.matchWildcardQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name == "match.stmt" && c.Node != nil {
				// Check if any case_clause is a wildcard (no string child in pattern).
				matchNode := c.Node
				body := findChildByType(matchNode, h.lang, "block")
				if body == nil {
					continue
				}
				for i := 0; i < body.ChildCount(); i++ {
					caseClause := body.Child(i)
					if caseClause == nil || caseClause.Type(h.lang) != "case_clause" {
						continue
					}
					pat := findChildByType(caseClause, h.lang, "case_pattern")
					if pat == nil {
						continue
					}
					// Wildcard: case_pattern has no named string child.
					isWild := true
					for j := 0; j < pat.ChildCount(); j++ {
						ch := pat.Child(j)
						if ch == nil {
							continue
						}
						if ch.IsNamed() && ch.Type(h.lang) == "string" {
							isWild = false
							break
						}
					}
					if isWild {
						wildcardKeys[matchNode.StartByte()] = true
						break
					}
				}
			}
		}
	}

	out := make([]pyMatchStmt, 0, len(order))
	for _, key := range order {
		e := byStmt[key]
		if len(e.cases) == 0 {
			continue
		}
		e.hasWildcard = wildcardKeys[key]
		out = append(out, *e)
	}
	return out
}

// ─── join pass ────────────────────────────────────────────────────────────────

// joinPyDrift performs the type-A / type-B join for one Python file.
//
// Nearest-binding resolution: for each consumer, find the binding of the
// scrutinee with the smallest scope span enclosing the consumer. If that
// binding is not a typed union param, skip entirely (precision-first).
func joinPyDrift(
	path string,
	producers []pyProducerType,
	bindings []pyBinding,
	ifChains []pyIfChain,
	matchStmts []pyMatchStmt,
) (typeA []store.Lead, typeB []store.Lead) {
	if len(producers) == 0 || len(bindings) == 0 {
		return nil, nil
	}
	if len(ifChains) == 0 && len(matchStmts) == 0 {
		return nil, nil
	}

	producerByName := make(map[string]*pyProducerType, len(producers))
	for i := range producers {
		producerByName[producers[i].name] = &producers[i]
	}

	seen := make(map[leadKey]bool)

	processConsumer := func(scrutinee string, consumerByte uint32, consumerLine int, hasDefault bool, cases []pyCaseLit) {
		// Find nearest binding of scrutinee enclosing the consumer.
		nearestSpan := ^uint32(0)
		var nearestBinding *pyBinding
		for j := range bindings {
			b := &bindings[j]
			if b.name != scrutinee {
				continue
			}
			if b.scopeStart > consumerByte || b.scopeEnd < consumerByte {
				continue
			}
			span := b.scopeEnd - b.scopeStart
			if span < nearestSpan {
				nearestSpan = span
				nearestBinding = b
			}
		}
		if nearestBinding == nil || !nearestBinding.isTypedUnion {
			return
		}
		producer, ok := producerByName[nearestBinding.typeName]
		if !ok {
			return
		}

		covered := make(map[string]bool, len(cases))
		for _, c := range cases {
			covered[c.value] = true
		}

		// Type-A: case literal not in producer member set.
		for _, c := range cases {
			if producer.members[c.value] {
				continue
			}
			k := leadKey{TargetLens: stringlyPyTargetLens, File: path, Line: c.line}
			if seen[k] {
				continue
			}
			seen[k] = true
			note := fmt.Sprintf(
				"stringly-py-drift: branch literal %q at %s:%d does not match "+
					"any member of type %s; likely a typo or stale branch",
				c.value, path, c.line, producer.name,
			)
			typeA = append(typeA, store.Lead{
				PosterLens: stringlyPyPosterLens,
				TargetLens: stringlyPyTargetLens,
				File:       path,
				Line:       c.line,
				Note:       truncate(note, noteMaxLen),
			})
		}

		// Type-B: producer member not covered. Suppressed by else/wildcard.
		if hasDefault {
			return
		}
		var uncovered []string
		for member := range producer.members {
			if !covered[member] {
				uncovered = append(uncovered, member)
			}
		}
		sort.Strings(uncovered)
		for _, val := range uncovered {
			k := leadKey{TargetLens: stringlyPyTargetLens, File: path, Line: consumerLine}
			if seen[k] {
				continue
			}
			seen[k] = true
			note := fmt.Sprintf(
				"stringly-py-drift: dispatch at %s:%d handles type %s but "+
					"missing branch for member %q",
				path, consumerLine, producer.name, val,
			)
			typeB = append(typeB, store.Lead{
				PosterLens: stringlyPyPosterLens,
				TargetLens: stringlyPyTargetLens,
				File:       path,
				Line:       consumerLine,
				Note:       truncate(note, noteMaxLen),
			})
		}
	}

	for i := range ifChains {
		ch := &ifChains[i]
		processConsumer(ch.scrutinee, ch.stmtByte, ch.stmtLine, ch.hasElse, ch.cases)
	}
	for i := range matchStmts {
		ms := &matchStmts[i]
		processConsumer(ms.scrutinee, ms.stmtByte, ms.stmtLine, ms.hasWildcard, ms.cases)
	}

	return typeA, typeB
}

// ─── test-file gate ───────────────────────────────────────────────────────────

// isPyTestPath reports whether a repo-relative file path looks like a Python
// test file. Mirrors the isTestPath convention from internal/repro/patch.go.
func isPyTestPath(relPath string) bool {
	slashed := filepath.ToSlash(relPath)
	for _, seg := range strings.Split(slashed, "/") {
		switch seg {
		case "tests", "test", "testdata", "testing":
			return true
		}
	}
	base := filepath.Base(slashed)
	if strings.HasPrefix(base, "test_") {
		return true
	}
	name := strings.TrimSuffix(base, ".py")
	name = strings.TrimSuffix(name, ".pyi")
	if strings.HasSuffix(name, "_test") {
		return true
	}
	return false
}

// ─── string unquoting ─────────────────────────────────────────────────────────

// unquotePyString strips surrounding single or double quotes from a Python
// string literal node text (e.g. `"active"` → `active`, `'pending'` → `pending`).
// Returns "" when the input doesn't look like a simple quoted string.
func unquotePyString(s string) string {
	if len(s) < 2 {
		return ""
	}
	q := s[0]
	if q != '\'' && q != '"' {
		return ""
	}
	if s[len(s)-1] != q {
		return ""
	}
	inner := s[1 : len(s)-1]
	// Reject multiline or empty.
	if inner == "" || strings.ContainsAny(inner, "\n\r") {
		return ""
	}
	return inner
}

// ─── seed entrypoint ──────────────────────────────────────────────────────────

// seedStringlyPyDrift runs the Python string-union drift miner.
func seedStringlyPyDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	h, err := loadPyLangHandle()
	if err != nil {
		// Grammar unavailable — degrade silently.
		return nil
	}

	leadsPosted := 0

	for _, f := range snap.Files {
		if f.Language != ingest.LangPython {
			continue
		}
		if isPyTestPath(f.Path) {
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

		tree, err := parsePyFile(h, src)
		if err != nil || tree == nil {
			continue
		}
		if tree.RootNode().HasError() {
			sum.PyParseFailures++
			tree.Release()
			continue
		}

		producers := passPy_Producers(h, tree, src)
		if len(producers) == 0 {
			tree.Release()
			continue
		}

		knownTypes := make(map[string]bool, len(producers))
		for _, p := range producers {
			knownTypes[p.name] = true
		}

		bindings := passPy_Bindings(h, tree, src, knownTypes)
		ifChains := passPy_IfChains(h, tree, src)
		matchStmts := passPy_MatchStmts(h, tree, src)
		tree.Release()

		typeA, typeB := joinPyDrift(f.Path, producers, bindings, ifChains, matchStmts)
		for _, lead := range append(typeA, typeB...) {
			if err := st.AddLead(ctx, lead); err != nil {
				return fmt.Errorf("miner: stringly-py lead %s: %w", lead.File, err)
			}
			sum.StringlyPyDriftLeads++
			sum.LeadsPosted++
			leadsPosted++
			if leadsPosted >= maxPyLeads {
				return nil
			}
		}
	}
	return nil
}
