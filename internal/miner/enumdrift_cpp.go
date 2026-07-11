package miner

// C/C++ enum/switch drift miner (miner:enum-cpp-drift).
//
// # Detection algorithm
//
// The miner operates in two passes over the snapshot, then joins:
//
//  1. passC_EnumDecls: scan every C and C++ file and run tree-sitter
//     enumerator queries to extract enum member names (and explicit integer
//     values where present). Files whose parse tree has HasError()=true are
//     ALLOWED here: the gotreesitter v0.20.2 C/C++ grammar cannot parse enum
//     declarations (they produce an ERROR subtree), but the `enumerator` node
//     queries still work correctly on the partial error tree. This grammar
//     limitation is the "known gap" documented below.
//
//  2. passC_Switches: scan every C and C++ file that has HasError()=false and
//     run tree-sitter queries for:
//       – switch_statement condition (scrutinee identifier)
//       – parameter_declaration type bindings (Color c → c:Color)
//       – local variable declarations (Color x = ...; → x:Color)
//       – case_statement values (identifier | number_literal)
//       – presence of a default clause in the switch
//
//  3. join (joinCppDrift): for each switch:
//       – Find the NEAREST binding (smallest enclosing scope) of the scrutinee
//         whose type name matches a known enum. Nearest-binding-of-any-kind
//         resolves shadowing: an int variable shadowing an outer Color param
//         prevents the outer param's type from being used.
//       – Type-A: case arm is an integer literal whose value collides with a
//         known enumerator value AND the enum has all-explicit values (every
//         member has an explicit integer assignment). The collision guard
//         mirrors the Go enum-drift ≤255 sentinel: only fires when the integer
//         is within the enum's explicit value set.
//       – Type-B: enumerator member not covered by any case arm, ONLY for
//         switches WITHOUT a default clause. With a default clause the
//         non-exhaustive subset is an explicit programming pattern, invisible
//         to -Wswitch. Without a default clause in C, the compiler gives no
//         warning (C has weaker switch exhaustion checks than C++ compilers);
//         in mixed C/C++ codebases a default-less non-exhaustive switch is the
//         case where neither the compiler (-Wswitch requires C++ and the right
//         enum type in the scrutinee expression) nor the code review catches
//         the gap. Scoping type-B to no-default switches is therefore the
//         highest-signal configuration.
//
// # Grammar limitation (known gap — v1)
//
// The gotreesitter v0.20.2 C and C++ grammars produce HasError()=true for
// any file containing an enum declaration (enum_specifier node is not parsed
// correctly). This means:
//
//   - Files containing BOTH enum declarations AND switch statements cannot be
//     analysed end-to-end: the enumerator query works on the error tree, but
//     switch_statement queries return 0 results on a HasError()=true tree.
//
//   - The miner therefore relies on the common C/C++ idiom where enum types
//     are declared in header files (.h/.hpp) and switch statements are in
//     implementation files (.c/.cc/.cpp). In that pattern the header has
//     HasError()=true (enum extraction still works) and the implementation
//     file parses cleanly (switch extraction works).
//
//   - Single-file test fixtures must reflect this split too: enum declarations
//     go in separate fixture files from the switch-containing files.
//
//   - A follow-up bead should upgrade to a grammar that correctly parses enum
//     declarations when available in gotreesitter; at that point the cross-file
//     join can be collapsed to per-file analysis.
//
// # Out of scope (v1)
//
// strcmp-chain string dispatch: a common C pattern for string-to-enum dispatch
// via chained if/else if (strcmp(s, "name") == 0). Deferred because: (1) it
// requires flow-sensitive analysis beyond what a single-pass query can provide;
// (2) the grammar already has trouble with complex function bodies; (3) the
// false-positive risk is high (strcmp chains that are NOT enum dispatches are
// common). Record for a v2 bead.
//
// # Config-field evaluation (no v1 implementation)
//
// C/C++ has no standard convention for default-vs-validator joins analogous to
// Go struct field comments with sentinel values. Common patterns (struct field
// with comment, macro-defined defaults, constexpr defaults) are:
//   – Not structurally distinct from ordinary assignments/declarations.
//   – Highly library- and codebase-specific (e.g. Qt's Q_PROPERTY, protobuf
//     generated code, Linux kernel module_param, etc.).
//   – Require cross-file and cross-TU analysis (the default lives in one place,
//     the validator in another, with no machine-readable link).
// Decision: no config-field miner for C/C++ in v1. The join cannot be made
// deterministic without codebase-specific conventions that the general miner
// cannot assume. Record on the bead for future per-ecosystem specialization.
//
// Leads: PosterLens="miner:enum-cpp-drift", TargetLens="api-contract-misuse".

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	cppDriftPosterLens = "miner:enum-cpp-drift"
	cppDriftTargetLens = "api-contract-misuse"

	// maxCppDriftLeads caps leads per Seed run.
	maxCppDriftLeads = 50

	// maxCppDriftLiteral mirrors the Go enum-drift guard: integer literals
	// above this value are unlikely to be accidental collisions with enum
	// indices and are more likely to be protocol constants or sentinel values.
	maxCppDriftLiteral = 255
)

// ─── query S-expressions ─────────────────────────────────────────────────────
//
// NOTE: The C grammar in gotreesitter v0.20.2 does not correctly parse
// enum_specifier nodes — they cause HasError()=true at file scope. The
// enumerator node queries below run on the partial error tree and still
// return correct results for the enumerator subtrees.

// cppEnumeratorQuery extracts enumerator names (and optional explicit values).
// Works on HasError()=true trees: the enumerator subtrees are preserved even
// when the enclosing enum_specifier is not parsed.
//
// Captures: "enum.name" (identifier), "enum.value" (number_literal, optional).
const cppEnumeratorQuery = `
(enumerator
  name: (identifier) @enum.name)
`

// cppEnumeratorWithValueQuery extracts enumerators with explicit integer values.
// Captures: "enum.name" (identifier), "enum.value" (number_literal).
const cppEnumeratorWithValueQuery = `
(enumerator
  name: (identifier) @enum.name
  value: (number_literal) @enum.value)
`

// cppSwitchQuery finds switch statements whose condition is a bare identifier.
// Only runs on files with HasError()=false.
// Captures: "sw" (the switch_statement node), "scrutinee" (the identifier).
const cppSwitchQuery = `
(switch_statement
  condition: (parenthesized_expression
    (identifier) @scrutinee)) @sw
`

// cppCaseIdentQuery finds case arms whose value is an identifier (enumerator name).
// Captures: "case.id" (the identifier).
const cppCaseIdentQuery = `
(case_statement
  value: (identifier) @case.id)
`

// cppCaseIntQuery finds case arms whose value is an integer literal.
// Captures: "case.int" (the number_literal).
const cppCaseIntQuery = `
(case_statement
  value: (number_literal) @case.int)
`

// cppDefaultQuery finds case_statement nodes that are the default: arm.
// The default keyword appears as a direct named child "default" of case_statement.
// Captures: "sw.default" (the switch_statement that contains this default).
const cppDefaultQuery = `
(switch_statement
  body: (compound_statement
    (case_statement
      (default) @default.kw))) @sw.has.default
`

// cppTypedParamQuery finds typed function parameters with the enclosing
// function_definition as scope span.
//
// Using function_definition (not compound_statement / function body) is
// intentional: it gives the parameter a LARGER scope span than a local
// variable declared inside the function body. In nearest-binding resolution
// (smallest span wins), a local variable declaration inside the body has a
// smaller span (compound_statement) and therefore shadows the outer typed
// param — which is the correct C scoping rule for local-variable shadows.
//
// Captures: "param.type" (type_identifier), "param.name" (identifier),
//
//	"param.scope" (@func = the function_definition node).
const cppTypedParamQuery = `
(function_definition
  declarator: (function_declarator
    parameters: (parameter_list
      (parameter_declaration
        type: (type_identifier) @param.type
        declarator: (identifier) @param.name)))) @param.scope
`

// cppPrimParamQuery finds primitive-typed function parameters (int/char/etc.)
// using the function_definition as scope (same rationale as cppTypedParamQuery).
// These are recorded as shadow sentinels with typeName="".
// Captures: "prim.type" (primitive_type), "prim.name" (identifier),
//
//	"prim.scope" (@func = the function_definition node).
const cppPrimParamQuery = `
(function_definition
  declarator: (function_declarator
    parameters: (parameter_list
      (parameter_declaration
        type: (primitive_type) @prim.type
        declarator: (identifier) @prim.name)))) @prim.scope
`

// cppTypedLocalVarQuery finds local variable declarations typed with a
// type_identifier inside a compound_statement, capturing the compound_statement
// as the scope span.
// Covers BOTH forms:
//   - initialized:   Color x = val;  → declarator: (init_declarator ...)
//   - uninitialized: Color x;        → declarator: (identifier)
//
// Captures: "var.type" (type_identifier), "var.name" (identifier),
//
//	"var.scope" (compound_statement).
const cppTypedLocalVarQuery = `
(compound_statement
  (declaration
    type: (type_identifier) @var.type
    declarator: (init_declarator
      declarator: (identifier) @var.name))) @var.scope
(compound_statement
  (declaration
    type: (type_identifier) @var.type
    declarator: (identifier) @var.name)) @var.scope
`

// cppPrimLocalVarQuery finds primitive-typed local variable declarations,
// used as shadow sentinels (same rationale as cppPrimParamQuery).
// Covers BOTH forms:
//   - initialized:   int x = 0;  → declarator: (init_declarator ...)
//   - uninitialized: int x;      → declarator: (identifier)
//
// Captures: "prim.type" (primitive_type), "prim.name" (identifier),
//
//	"prim.scope" (compound_statement).
const cppPrimLocalVarQuery = `
(compound_statement
  (declaration
    type: (primitive_type) @prim.type
    declarator: (init_declarator
      declarator: (identifier) @prim.name))) @prim.scope
(compound_statement
  (declaration
    type: (primitive_type) @prim.type
    declarator: (identifier) @prim.name)) @prim.scope
`

// ─── data types ──────────────────────────────────────────────────────────────

// cppEnum records one enum found in the corpus.
type cppEnum struct {
	// name is the enum type name as used in declarations (e.g. "Color", "AppState").
	// For anonymous/typedef enums this is the typedef alias; for named enums it
	// is the enum tag name. Because we can only query enumerator nodes (not
	// enum_specifier nodes) on error trees, the name is extracted from
	// nearby typedef declarations or inferred from naming conventions — see
	// passC_EnumDecls for the detection strategy.
	name string

	// members is the set of enumerator identifiers.
	members map[string]bool

	// valueByMember maps member name → explicit integer value (only populated
	// when ALL members have explicit values). If the map is non-nil, it means
	// all members have explicit values and we can do Type-A integer literal checks.
	valueByMember map[string]int64

	// memberByValue maps explicit integer value → member name (reverse of valueByMember).
	memberByValue map[int64]string

	// allExplicit reports whether every member has an explicit integer value
	// assignment (enabling Type-A integer literal collision checking).
	allExplicit bool
}

// cppBinding records a variable or parameter binding in a C/C++ function.
type cppBinding struct {
	name       string // identifier name
	typeName   string // declared type name (only type_identifier, not primitives)
	scopeStart uint32 // byte offset of the enclosing function_definition
	scopeEnd   uint32
}

// cppSwitch records a switch statement found in a clean-parsing file.
type cppSwitch struct {
	file       string
	scrutinee  string // the identifier in switch (scrutinee)
	switchByte uint32 // start byte of the switch_statement node
	switchLine int
	switchEnd  uint32

	// caseIdents are the identifier-valued case arms (enumerator names).
	caseIdents []string
	// caseInts are the integer-valued case arms with their source line numbers.
	caseInts []cppCaseInt

	hasDefault bool
}

// cppCaseInt is one integer-literal case arm in a switch statement.
type cppCaseInt struct {
	value int64
	line  int
}

// ─── language handle cache ────────────────────────────────────────────────────

// cppLangHandle caches compiled queries for one C or C++ grammar.
type cppLangHandle struct {
	lang               *gts.Language
	enumeratorQ        *gts.Query
	enumeratorWithValQ *gts.Query
	switchQ            *gts.Query
	caseIdentQ         *gts.Query
	caseIntQ           *gts.Query
	defaultQ           *gts.Query
	typedParamQ        *gts.Query // typed param + body scope
	primParamQ         *gts.Query // primitive param + body scope (shadow sentinel)
	typedLocalVarQ     *gts.Query // typed local var + compound scope
	primLocalVarQ      *gts.Query // primitive local var + compound scope (shadow sentinel)
}

// loadCppLangHandle loads and compiles all queries for the grammar identified
// by sample ("x.c" or "x.cpp").
func loadCppLangHandle(sample string) (*cppLangHandle, error) {
	entry := tsregistry.DetectLanguage(sample)
	if entry == nil {
		return nil, fmt.Errorf("enum-cpp-drift: no grammar for %s", sample)
	}
	lang := entry.Language()
	var err error
	h := &cppLangHandle{lang: lang}
	if h.enumeratorQ, err = gts.NewQuery(cppEnumeratorQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile enumerator query: %w", err)
	}
	if h.enumeratorWithValQ, err = gts.NewQuery(cppEnumeratorWithValueQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile enumerator-with-val query: %w", err)
	}
	if h.switchQ, err = gts.NewQuery(cppSwitchQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile switch query: %w", err)
	}
	if h.caseIdentQ, err = gts.NewQuery(cppCaseIdentQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile case-ident query: %w", err)
	}
	if h.caseIntQ, err = gts.NewQuery(cppCaseIntQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile case-int query: %w", err)
	}
	if h.defaultQ, err = gts.NewQuery(cppDefaultQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile default query: %w", err)
	}
	if h.typedParamQ, err = gts.NewQuery(cppTypedParamQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile typed-param query: %w", err)
	}
	if h.primParamQ, err = gts.NewQuery(cppPrimParamQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile prim-param query: %w", err)
	}
	if h.typedLocalVarQ, err = gts.NewQuery(cppTypedLocalVarQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile typed-local-var query: %w", err)
	}
	if h.primLocalVarQ, err = gts.NewQuery(cppPrimLocalVarQuery, lang); err != nil {
		return nil, fmt.Errorf("enum-cpp-drift: compile prim-local-var query: %w", err)
	}
	return h, nil
}

// parseCppFile parses src with the given language handle.
func parseCppFile(h *cppLangHandle, src []byte) (*gts.Tree, error) {
	parser := gts.NewParser(h.lang)
	return parser.Parse(src)
}

// ─── pass 1: enum extraction ──────────────────────────────────────────────────

// passC_EnumDecls extracts enum definitions from a file.
//
// The gotreesitter v0.20.2 C/C++ grammar cannot parse enum_specifier nodes
// (they cause HasError()=true for the whole file), so we cannot query
// enum_specifier directly. Instead we query enumerator nodes, which ARE
// preserved in the error subtree.
//
// Because we can't get the enum type name from enum_specifier, we use a
// heuristic: enumerators found in the same contiguous block (consecutive byte
// ranges with no gap > maxEnumGap bytes) are assumed to belong to the same
// enum. Each group is stored under the key "enum_<firstMember>" (an opaque
// group key). The JOIN step compares the TYPE NAME of the switch scrutinee
// against known enum groups — but since we don't know the type name from the
// grammar, we store all enumerator names in a single flat pool and let the
// join do a member-set lookup.
//
// This means we cannot enforce "the scrutinee's type must be THIS specific enum"
// at the type level — we can only check "does the case arm match any known
// enumerator". This is intentionally conservative (precision-first): we only
// emit a Type-A lead when an integer literal in a case arm collides with a
// value in the flat enumerator pool; we only emit a Type-B lead when a binding
// whose type name exactly matches an enum name from the same file cluster is
// missing an arm.
//
// The type name match is done via a separate typedef/tag scan: we look for
// `typedef enum { ... } TypeName;` patterns using the typedef+type_identifier
// query on a clean-parsing file, or the tag-name from enum_specifier on a
// clean-parsing file. In practice for the grammar-limitation case we fall back
// to a cross-file heuristic: if a parameter in a switch file is typed with
// TypeName X, and X has members M1..Mn in the corpus-wide enumerator pool,
// we treat X as an enum and proceed.
func passC_EnumDecls(h *cppLangHandle, tree *gts.Tree, src []byte) (members map[string]bool, valByMember map[string]int64, memberByVal map[int64]string) {
	members = make(map[string]bool)
	valByMember = make(map[string]int64)
	memberByVal = make(map[int64]string)

	// Extract all enumerator names (works even on HasError trees).
	for _, m := range h.enumeratorQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name == "enum.name" {
				members[c.Node.Text(src)] = true
			}
		}
	}

	// Extract enumerators with explicit integer values.
	explicitCount := 0
	for _, m := range h.enumeratorWithValQ.Execute(tree) {
		var name string
		var val int64
		hasVal := false
		for _, c := range m.Captures {
			switch c.Name {
			case "enum.name":
				name = c.Node.Text(src)
			case "enum.value":
				v, err := strconv.ParseInt(strings.TrimSpace(c.Node.Text(src)), 10, 64)
				if err == nil {
					val = v
					hasVal = true
				}
			}
		}
		if name != "" && hasVal {
			valByMember[name] = val
			memberByVal[val] = name
			explicitCount++
		}
	}
	if explicitCount != len(members) {
		// Not all members have explicit values — clear the value maps so
		// Type-A integer collision checks are disabled (precision-first).
		valByMember = nil
		memberByVal = nil
	}

	return members, valByMember, memberByVal
}

// ─── pass 2: switch extraction ────────────────────────────────────────────────

// passC_Switches extracts switch statements, bindings, and case arms from a
// file that has HasError()=false.
//
// Bindings carry REAL scope spans derived from the enclosing function body
// (compound_statement). Nearest-binding resolution in joinCppDrift selects
// the binding with the smallest scope span that contains the switch start
// byte, correctly handling cross-function isolation and inner-block shadowing.
//
// Primitive-typed bindings (int x, char c, ...) are recorded with typeName=""
// as shadow sentinels: if the nearest enclosing binding of the scrutinee is
// primitive, the switch is not over a known enum type → emit nothing.
func passC_Switches(h *cppLangHandle, tree *gts.Tree, src []byte, relPath string) (bindings []cppBinding, switches []cppSwitch) {
	// ── typed parameter bindings (Color c → real function-body scope) ────
	for _, m := range h.typedParamQ.Execute(tree) {
		var typeName, name string
		var scopeStart, scopeEnd uint32
		hasScope := false
		for _, c := range m.Captures {
			switch c.Name {
			case "param.type":
				typeName = c.Node.Text(src)
			case "param.name":
				name = c.Node.Text(src)
			case "param.scope":
				scopeStart = c.Node.StartByte()
				scopeEnd = c.Node.EndByte()
				hasScope = true
			}
		}
		if typeName == "" || name == "" || !hasScope {
			continue
		}
		bindings = append(bindings, cppBinding{
			name:       name,
			typeName:   typeName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
		})
	}

	// ── primitive parameter shadow sentinels (int x → function-body scope) ─
	// These bindings have typeName="" and prevent a same-named outer typed
	// param from being selected as the nearest binding.
	for _, m := range h.primParamQ.Execute(tree) {
		var name string
		var scopeStart, scopeEnd uint32
		hasScope := false
		for _, c := range m.Captures {
			switch c.Name {
			case "prim.name":
				name = c.Node.Text(src)
			case "prim.scope":
				scopeStart = c.Node.StartByte()
				scopeEnd = c.Node.EndByte()
				hasScope = true
			}
		}
		if name == "" || !hasScope {
			continue
		}
		bindings = append(bindings, cppBinding{
			name:       name,
			typeName:   "", // sentinel: primitive, not an enum type
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
		})
	}

	// ── typed local variable bindings (Color x = ...; → compound scope) ──
	for _, m := range h.typedLocalVarQ.Execute(tree) {
		var typeName, name string
		var scopeStart, scopeEnd uint32
		hasScope := false
		for _, c := range m.Captures {
			switch c.Name {
			case "var.type":
				typeName = c.Node.Text(src)
			case "var.name":
				name = c.Node.Text(src)
			case "var.scope":
				scopeStart = c.Node.StartByte()
				scopeEnd = c.Node.EndByte()
				hasScope = true
			}
		}
		if typeName == "" || name == "" || !hasScope {
			continue
		}
		bindings = append(bindings, cppBinding{
			name:       name,
			typeName:   typeName,
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
		})
	}

	// ── primitive local variable shadow sentinels (int x = ...; → compound) ─
	for _, m := range h.primLocalVarQ.Execute(tree) {
		var name string
		var scopeStart, scopeEnd uint32
		hasScope := false
		for _, c := range m.Captures {
			switch c.Name {
			case "prim.name":
				name = c.Node.Text(src)
			case "prim.scope":
				scopeStart = c.Node.StartByte()
				scopeEnd = c.Node.EndByte()
				hasScope = true
			}
		}
		if name == "" || !hasScope {
			continue
		}
		bindings = append(bindings, cppBinding{
			name:       name,
			typeName:   "", // sentinel: primitive, not an enum type
			scopeStart: scopeStart,
			scopeEnd:   scopeEnd,
		})
	}

	// ── switch collection ─────────────────────────────────────────────────
	type switchEntry struct {
		scrutinee  string
		switchByte uint32
		switchEnd  uint32
		switchLine int
	}
	var switchList []switchEntry
	for _, m := range h.switchQ.Execute(tree) {
		var scrutinee string
		var swStart, swEnd uint32
		var swLine int
		for _, c := range m.Captures {
			switch c.Name {
			case "scrutinee":
				scrutinee = c.Node.Text(src)
			case "sw":
				swStart = c.Node.StartByte()
				swEnd = c.Node.EndByte()
				swLine = int(c.Node.StartPoint().Row) + 1
			}
		}
		if scrutinee == "" {
			continue
		}
		switchList = append(switchList, switchEntry{
			scrutinee:  scrutinee,
			switchByte: swStart,
			switchEnd:  swEnd,
			switchLine: swLine,
		})
	}

	if len(switchList) == 0 {
		return bindings, nil
	}

	// ── case identifier arms ──────────────────────────────────────────────
	caseIdentsBySw := make(map[uint32][]string)
	for _, m := range h.caseIdentQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name != "case.id" {
				continue
			}
			cb := c.Node.StartByte()
			for _, sw := range switchList {
				if cb >= sw.switchByte && cb < sw.switchEnd {
					caseIdentsBySw[sw.switchByte] = append(caseIdentsBySw[sw.switchByte], c.Node.Text(src))
				}
			}
		}
	}

	// ── case integer arms ─────────────────────────────────────────────────
	caseIntsBySw := make(map[uint32][]cppCaseInt)
	for _, m := range h.caseIntQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name != "case.int" {
				continue
			}
			cb := c.Node.StartByte()
			v, err := strconv.ParseInt(strings.TrimSpace(c.Node.Text(src)), 10, 64)
			if err != nil || v < 0 || v > maxCppDriftLiteral {
				continue
			}
			caseLine := int(c.Node.StartPoint().Row) + 1
			for _, sw := range switchList {
				if cb >= sw.switchByte && cb < sw.switchEnd {
					caseIntsBySw[sw.switchByte] = append(caseIntsBySw[sw.switchByte], cppCaseInt{value: v, line: caseLine})
				}
			}
		}
	}

	// ── default clause detection ──────────────────────────────────────────
	defaultSwitches := make(map[uint32]bool)
	for _, m := range h.defaultQ.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name == "sw.has.default" {
				defaultSwitches[c.Node.StartByte()] = true
			}
		}
	}

	for _, sw := range switchList {
		switches = append(switches, cppSwitch{
			file:       relPath,
			scrutinee:  sw.scrutinee,
			switchByte: sw.switchByte,
			switchLine: sw.switchLine,
			switchEnd:  sw.switchEnd,
			caseIdents: caseIdentsBySw[sw.switchByte],
			caseInts:   caseIntsBySw[sw.switchByte],
			hasDefault: defaultSwitches[sw.switchByte],
		})
	}
	return bindings, switches
}

// ─── join pass ────────────────────────────────────────────────────────────────

// cppEnumPool is the corpus-wide enum member pool, used to identify which
// type names correspond to enums and what their member sets are.
type cppEnumPool struct {
	// allMembers is the union of all enumerator names seen across the corpus.
	// Used for fast "is this an enum type name" lookup via memberTypes.
	allMembers map[string]bool

	// memberTypes maps enumerator name → set of possible type names.
	// Built from typedef scans when available, otherwise empty.
	// (Not currently populated in v1 due to grammar limitation.)
	memberTypes map[string]string

	// byTypeName maps a type_identifier name → the cppEnum definition.
	// Only populated for types whose name we can extract from the grammar
	// (requires a clean-parsing file containing a typedef enum { ... } Name;).
	byTypeName map[string]*cppEnum
}

// joinCppDrift emits Type-A and Type-B leads for one file's switches given
// the corpus-wide enum pool.
//
// dir is the directory of the switch file (forward-slash, as returned by
// filepath.ToSlash(filepath.Dir(relPath))). The pool is keyed by dir+typeName
// so that same-named enums in different directories never collide.
//
// Nearest-binding resolution: for each switch scrutinee, select the binding
// with the SMALLEST scope span containing the switch start byte. If the
// nearest binding has typeName="" (primitive shadow sentinel) or no matching
// enum exists for the dir-scoped key, emit nothing (precision-first).
func joinCppDrift(
	path string,
	dir string,
	pool *cppEnumPool,
	bindings []cppBinding,
	switches []cppSwitch,
) (typeA []store.Lead, typeB []store.Lead) {
	if len(pool.byTypeName) == 0 || len(switches) == 0 {
		return nil, nil
	}

	seenA := make(map[leadKey]bool)
	seenB := make(map[leadKey]bool)

	for i := range switches {
		sw := &switches[i]

		// Find nearest binding of the scrutinee name.
		nearestSpan := ^uint32(0)
		var nearestBinding *cppBinding
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

		// typeName=="" means the nearest binding is a primitive shadow sentinel
		// (int, char, etc.) — not an enum type. Emit nothing.
		if nearestBinding == nil || nearestBinding.typeName == "" {
			continue
		}

		// Look up the enum by dir-scoped key.
		key := dir + "/" + nearestBinding.typeName
		enum, ok := pool.byTypeName[key]
		if !ok {
			continue
		}

		covered := make(map[string]bool, len(sw.caseIdents))
		for _, id := range sw.caseIdents {
			covered[id] = true
		}

		// Type-A: integer literal in case arm whose value is in the enum's
		// explicit value set (and the enum has all-explicit values).
		if enum.allExplicit && enum.memberByValue != nil {
			for _, ci := range sw.caseInts {
				memberName, hit := enum.memberByValue[ci.value]
				if !hit {
					continue
				}
				k := leadKey{TargetLens: cppDriftTargetLens, File: path, Line: ci.line}
				if seenA[k] {
					continue
				}
				seenA[k] = true
				note := fmt.Sprintf(
					"enum-cpp-drift: case %d at %s:%d uses raw integer equal to %s::%s; "+
						"use the named enumerator to prevent silent breakage on reordering",
					ci.value, path, ci.line, enum.name, memberName,
				)
				typeA = append(typeA, store.Lead{
					PosterLens: cppDriftPosterLens,
					TargetLens: cppDriftTargetLens,
					File:       path,
					Line:       ci.line,
					Note:       truncate(note, noteMaxLen),
				})
			}
		}

		// Type-B: enumerator member not covered by any case arm, only when
		// there is no default clause. With a default the omission is intentional.
		if sw.hasDefault {
			continue
		}
		var uncovered []string
		for member := range enum.members {
			if !covered[member] {
				uncovered = append(uncovered, member)
			}
		}
		if len(uncovered) == 0 {
			continue
		}
		sort.Strings(uncovered)
		for _, member := range uncovered {
			k := leadKey{TargetLens: cppDriftTargetLens, File: path, Line: sw.switchLine}
			if seenB[k] {
				continue
			}
			seenB[k] = true
			note := fmt.Sprintf(
				"enum-cpp-drift: switch at %s:%d on %s (%s) has no case for enumerator %s; "+
					"missing arm is invisible to -Wswitch without a default",
				path, sw.switchLine, sw.scrutinee, enum.name, member,
			)
			typeB = append(typeB, store.Lead{
				PosterLens: cppDriftPosterLens,
				TargetLens: cppDriftTargetLens,
				File:       path,
				Line:       sw.switchLine,
				Note:       truncate(note, noteMaxLen),
			})
		}
	}
	return typeA, typeB
}

// ─── test-file gate ───────────────────────────────────────────────────────────

// isCTestPath reports whether a repo-relative path is a C/C++ test file.
// Test files plant deliberate defects that would produce false leads.
func isCTestPath(relPath string) bool {
	p := filepath.ToSlash(relPath)
	base := strings.ToLower(filepath.Base(p))

	// Common test file suffixes.
	for _, suf := range []string{"_test.c", "_test.cc", "_test.cpp", "_tests.c", "_tests.cc",
		"_tests.cpp", "_spec.c", "_spec.cc", "_spec.cpp"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	// Common test directory patterns.
	for _, seg := range strings.Split(p, "/") {
		switch strings.ToLower(seg) {
		case "test", "tests", "spec", "specs", "unittest", "unittests",
			"gtest", "googletest", "catch2", "testsuite":
			return true
		}
	}
	return false
}

// ─── typedef enum name extraction ────────────────────────────────────────────

// cppTypedefEnumRe matches `typedef enum { ... } TypeName;` in C source text.
// The gotreesitter v0.20.2 C/C++ grammar cannot produce a parseable
// type_definition node when the typedef wraps an enum — the whole declaration
// produces HasError()=true. We fall back to a regex for the single specific
// pattern `typedef enum ... } TypeName;` to extract the typedef alias name.
//
// The regex is deliberately narrow (matches only `typedef enum ... } Name;`
// patterns where the closing `}` is followed immediately by a word and `;`).
// This prevents false matches on non-enum typedef declarations.
var cppTypedefEnumRe = regexp.MustCompile(`(?s)\btypedef\s+(?:enum(?:\s+class)?\s*(?:[A-Za-z_]\w*)?\s*)?\{[^}]*\}\s*([A-Za-z_]\w*)\s*;`)

// passC_TypedefNamesFromSrc extracts typedef alias names that wrap enum
// declarations from raw source text. Used as a fallback when tree-sitter
// cannot parse the typedef due to the enum_specifier grammar limitation.
//
// Returns the list of typedef alias names found. If multiple names are found
// in one file (unusual), the caller should treat the file as ambiguous and
// skip (precision-first).
func passC_TypedefNamesFromSrc(src []byte) []string {
	matches := cppTypedefEnumRe.FindAllSubmatch(src, -1)
	seen := make(map[string]bool)
	var names []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := strings.TrimSpace(string(m[1]))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// cppTypedefQuery finds `typedef ... TypeName;` declarations.
// This query works only on clean-parsing files (files without enum declarations).
const cppTypedefQuery = `
(type_definition
  declarator: (type_identifier) @typedef.name)
`

// passC_TypedefNames extracts type names from typedef declarations in clean
// files (HasError()=false). Used as the primary path when a file doesn't have
// enum declarations (i.e., separate typedef file).
func passC_TypedefNames(h *cppLangHandle, tree *gts.Tree, src []byte) []string {
	q, err := gts.NewQuery(cppTypedefQuery, h.lang)
	if err != nil {
		return nil
	}
	var names []string
	seen := make(map[string]bool)
	for _, m := range q.Execute(tree) {
		for _, c := range m.Captures {
			if c.Name == "typedef.name" {
				n := c.Node.Text(src)
				if !seen[n] {
					seen[n] = true
					names = append(names, n)
				}
			}
		}
	}
	return names
}

// ─── seed entrypoint ──────────────────────────────────────────────────────────

// seedCppEnumDrift runs the C/C++ enum/switch drift miner over the snapshot.
func seedCppEnumDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	cHandle, err := loadCppLangHandle("x.c")
	if err != nil {
		return nil // grammar unavailable — degrade silently
	}
	cppHandle, err := loadCppLangHandle("x.cpp")
	if err != nil {
		return nil
	}

	// ── Pass 1: extract enumerators from all C/C++ files ──────────────────
	// We collect: (a) all enumerator names into a corpus-wide flat pool;
	// (b) file-level enum metadata for use in the type-name association step.
	//
	// Because the grammar can't give us the enum type name reliably, we also
	// collect typedef names from CLEAN files and then try to match them.

	type fileEnum struct {
		members   map[string]bool
		valByMem  map[string]int64
		memByVal  map[int64]string
		typeNames []string // typedef names declared in this file
		isClean   bool     // HasError()=false
	}

	allMembers := make(map[string]bool) // corpus-wide flat pool
	// file-level collections keyed by file path
	fileInfoByPath := make(map[string]*fileEnum)

	for _, f := range snap.Files {
		if f.Language != ingest.LangC && f.Language != ingest.LangCPP {
			continue
		}
		if isCTestPath(f.Path) {
			continue
		}

		ext := strings.ToLower(filepath.Ext(f.Path))
		var h *cppLangHandle
		if f.Language == ingest.LangCPP || ext == ".hpp" || ext == ".hh" || ext == ".hxx" || ext == ".cc" || ext == ".cpp" || ext == ".cxx" {
			h = cppHandle
		} else {
			h = cHandle
		}

		abs := filepath.Join(snap.Root, filepath.FromSlash(f.Path))
		fi, statErr := os.Stat(abs)
		if statErr != nil {
			continue
		}
		if fi.Size() > maxFileBytes {
			continue
		}
		src, readErr := os.ReadFile(abs)
		if readErr != nil {
			continue
		}

		tree, parseErr := parseCppFile(h, src)
		if parseErr != nil || tree == nil {
			sum.CppParseFailures++
			continue
		}

		isClean := !tree.RootNode().HasError()
		if !isClean {
			sum.CppParseFailures++
		}

		members, valByMem, memByVal := passC_EnumDecls(h, tree, src)

		fe := &fileEnum{
			members:  members,
			valByMem: valByMem,
			memByVal: memByVal,
			isClean:  isClean,
		}
		// Typedef name extraction:
		// - On clean files: use tree-sitter (reliable, no false positives).
		// - On error files (e.g. typedef enum { ... } Name; pattern): fall back
		//   to the regex that specifically matches `typedef ... } Name;`.
		//   This is safe because the regex is narrow and only matches the
		//   canonical typedef-enum-alias pattern that the grammar can't parse.
		if isClean {
			fe.typeNames = passC_TypedefNames(h, tree, src)
		} else if len(members) > 0 {
			fe.typeNames = passC_TypedefNamesFromSrc(src)
		}

		tree.Release()

		for m := range members {
			allMembers[m] = true
		}
		if len(members) > 0 || len(fe.typeNames) > 0 {
			fileInfoByPath[f.Path] = fe
		}
	}

	if len(allMembers) == 0 {
		return nil // no enums found in corpus
	}

	// ── Build enum pool (dir-scoped) ───────────────────────────────────────
	// Enum type names are keyed by dir+typeName to avoid cross-directory
	// collision: two directories may each define `typedef enum {...} Status;`
	// for different member sets. Merging them would corrupt the member set
	// and produce false Type-A leads when the integer value maps diverge.
	//
	// The intended join is same-directory header/impl: colors.h in dir A
	// defines Color; process.c in dir A switches on Color. An impl file in
	// dir B that happens to use Color maps to dir B's own enum, not dir A's.
	//
	// Per-file rules:
	//   – Exactly one typedef name + members → register under dir+typeName.
	//   – Multiple typedef names in one file → ambiguous, skip (precision-first).
	//   – Same dir+typeName seen twice → take the first registration and skip
	//     subsequent; cross-file same-dir repetition is rare and low-risk
	//     (both should define the same enum), but any divergence is safer to
	//     ignore than to merge.
	//   – No typedef names (anonymous/unresolvable) → skip (no type-name join).
	pool := &cppEnumPool{
		allMembers:  allMembers,
		memberTypes: make(map[string]string),
		byTypeName:  make(map[string]*cppEnum),
	}

	for fpath, fe := range fileInfoByPath {
		if len(fe.members) == 0 || len(fe.typeNames) != 1 {
			continue
		}
		typeName := fe.typeNames[0]
		// Key: dir of the file (forward-slash, no trailing slash) + "/" + typeName.
		dir := filepath.ToSlash(filepath.Dir(fpath))
		key := dir + "/" + typeName
		if _, exists := pool.byTypeName[key]; exists {
			// Same dir+type already registered (e.g. duplicate include guard).
			// Keep the first registration; skip subsequent.
			continue
		}
		allExplicit := fe.valByMem != nil && len(fe.valByMem) == len(fe.members) && len(fe.members) > 0
		pool.byTypeName[key] = &cppEnum{
			name:          typeName,
			members:       fe.members,
			valueByMember: fe.valByMem,
			memberByValue: fe.memByVal,
			allExplicit:   allExplicit,
		}
	}

	if len(pool.byTypeName) == 0 {
		return nil // no type-named enums found — no joins possible
	}

	// ── Pass 2: extract switches from clean files ──────────────────────────
	leadsPosted := 0

	for _, f := range snap.Files {
		if f.Language != ingest.LangC && f.Language != ingest.LangCPP {
			continue
		}
		if isCTestPath(f.Path) {
			continue
		}

		ext := strings.ToLower(filepath.Ext(f.Path))
		var h *cppLangHandle
		if f.Language == ingest.LangCPP || ext == ".hpp" || ext == ".hh" || ext == ".hxx" || ext == ".cc" || ext == ".cpp" || ext == ".cxx" {
			h = cppHandle
		} else {
			h = cHandle
		}

		abs := filepath.Join(snap.Root, filepath.FromSlash(f.Path))
		fi, statErr := os.Stat(abs)
		if statErr != nil {
			continue
		}
		if fi.Size() > maxFileBytes {
			continue
		}
		src, readErr := os.ReadFile(abs)
		if readErr != nil {
			continue
		}

		tree, parseErr := parseCppFile(h, src)
		if parseErr != nil || tree == nil {
			continue
		}
		if tree.RootNode().HasError() {
			// Switch queries don't work on error trees — skip.
			tree.Release()
			continue
		}

		bindings, switches := passC_Switches(h, tree, src, f.Path)
		tree.Release()

		if len(switches) == 0 {
			continue
		}

		fileDir := filepath.ToSlash(filepath.Dir(f.Path))
		typeA, typeB := joinCppDrift(f.Path, fileDir, pool, bindings, switches)
		for _, lead := range append(typeA, typeB...) {
			if err := st.AddLead(ctx, lead); err != nil {
				return fmt.Errorf("miner: enum-cpp-drift lead %s: %w", lead.File, err)
			}
			sum.CppDriftLeads++
			sum.LeadsPosted++
			leadsPosted++
			if leadsPosted >= maxCppDriftLeads {
				return nil
			}
		}
	}
	return nil
}
