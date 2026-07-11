package miner

// Python config-field contradiction miner (miner:config-field-py).
//
// # Design
//
// Detects contradictions between a pydantic Field(default=X) / dataclass
// field(default=X) default value and a validator in the SAME class that
// rejects exactly that sentinel value.
//
// # Why tree-sitter (not regex)
//
// The first implementation used regex sweeps. The oracle identified two
// proven false-positive failure modes:
//
//  (a) Docstring FP: pyFieldDefRe matched `timeout: int = Field(default=0)`
//      appearing inside a triple-quoted docstring, which is a string literal
//      in the AST, not an assignment — the AST approach rejects it for free.
//
//  (b) Nested-class scope FP: pyClassDeclRe was column-0-anchored, so an
//      inner class never reset the "current class" tracking, causing an inner
//      class's validator to misjoin an outer class's field — violating the
//      same-class guard. Tree-sitter's class_definition containment provides
//      exact scoping with no manual state machine.
//
// Join key: (file path, class startByte, field name). Both the field and
// the validator query capture their enclosing class_definition node, so
// containment-based joining is structurally exact.
//
// # Detection algorithm
//
//  1. pyCFFieldQuery: find class-body typed assignments of the form:
//       fieldname: T = Field(default=SENTINEL)   where SENTINEL is integer or None
//     Capture: class.def, class.name, field.name, kwarg.val (the sentinel node).
//
//  2. pyCFValidatorQuery: find class-body decorated functions whose
//     decorator is @validator('field') or @field_validator('field')
//     and whose body contains `if param OP SENTINEL: raise ...`.
//     Capture: class.def, class.name, field.name (from decorator string),
//              guard.cond (comparison_operator node for operator extraction).
//
//  3. Join: group fields and validators by (classStartByte, fieldName).
//     For each joined pair, call cfPyContradicts to evaluate whether the
//     default is covered by the reject condition.
//
// # Precision guards
//
//   - Sentinel must be integer literal or None node (strings excluded).
//   - Field must use Field()/field() with explicit default= keyword.
//   - Validator decorator must be @validator or @field_validator.
//   - Guard must have raise in the consequence (if_statement → block → raise_statement).
//   - Both field and validator must be in the SAME class_definition node
//     (by startByte equality, not name equality — handles same-name nested classes).
//   - Test files are skipped (isPyTestPath gate).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gts "github.com/odvcencio/gotreesitter"
	tsregistry "github.com/odvcencio/gotreesitter/grammars"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	cfPyPosterLens = "miner:config-field-py"
	cfPyTargetLens = "api-contract-misuse"
	cfPyMaxLeads   = 50
)

// ─── query S-expressions ─────────────────────────────────────────────────────

// pyCFFieldIntQuery finds class-body typed field assignments with integer defaults:
//
//	fieldname: T = Field(default=<integer>)
//	fieldname: T = field(default=<integer>)
//
// Captures: class.def (class_definition node), class.name, field.name, kwarg.val (integer).
const pyCFFieldIntQuery = `
(class_definition
  name: (identifier) @class.name
  body: (block
    (assignment
      left: (identifier) @field.name
      type: (type)
      right: (call
        function: (identifier) @call.fn
        arguments: (argument_list
          (keyword_argument
            name: (identifier) @kwarg.name
            value: (integer) @kwarg.val)))))) @class.def
`

// pyCFFieldNoneQuery finds class-body typed field assignments with None defaults.
const pyCFFieldNoneQuery = `
(class_definition
  name: (identifier) @class.name
  body: (block
    (assignment
      left: (identifier) @field.name
      type: (type)
      right: (call
        function: (identifier) @call.fn
        arguments: (argument_list
          (keyword_argument
            name: (identifier) @kwarg.name
            value: (none) @kwarg.val)))))) @class.def
`

// pyCFValidatorIntQuery finds @validator/@field_validator decorated methods
// whose body has an if-guard comparing param to an integer sentinel that
// raises on match.
//
// Captures: class.def, class.name, dec.fn, field.name (from decorator string),
//
//	guard.cond (comparison_operator node — operator extracted in Go).
const pyCFValidatorIntQuery = `
(class_definition
  name: (identifier) @class.name
  body: (block
    (decorated_definition
      (decorator
        (call
          function: (identifier) @dec.fn
          arguments: (argument_list (string) @field.name)))
      (function_definition
        body: (block
          (if_statement
            condition: (comparison_operator
              (identifier)
              (integer) @guard.sentinel) @guard.cond
            consequence: (block (raise_statement)))))))) @class.def
`

// pyCFValidatorNoneQuery finds @validator methods with a `is None` guard.
const pyCFValidatorNoneQuery = `
(class_definition
  name: (identifier) @class.name
  body: (block
    (decorated_definition
      (decorator
        (call
          function: (identifier) @dec.fn
          arguments: (argument_list (string) @field.name)))
      (function_definition
        body: (block
          (if_statement
            condition: (comparison_operator
              (identifier)
              (none) @guard.sentinel) @guard.cond
            consequence: (block (raise_statement)))))))) @class.def
`

// ─── language handle cache ────────────────────────────────────────────────────

type cfPyLangHandle struct {
	lang           *gts.Language
	fieldIntQ      *gts.Query
	fieldNoneQ     *gts.Query
	validatorIntQ  *gts.Query
	validatorNoneQ *gts.Query
}

var cfPyLangHandleCache *cfPyLangHandle

func loadCFPyLangHandle() (*cfPyLangHandle, error) {
	if cfPyLangHandleCache != nil {
		return cfPyLangHandleCache, nil
	}
	entry := tsregistry.DetectLanguage("x.py")
	if entry == nil {
		return nil, fmt.Errorf("config-field-py: no grammar for x.py")
	}
	lang := entry.Language()
	h := &cfPyLangHandle{lang: lang}
	var err error
	compile := func(name, q string) *gts.Query {
		if err != nil {
			return nil
		}
		query, e := gts.NewQuery(q, lang)
		if e != nil {
			err = fmt.Errorf("config-field-py: compile %s query: %w", name, e)
			return nil
		}
		return query
	}
	h.fieldIntQ = compile("field-int", pyCFFieldIntQuery)
	h.fieldNoneQ = compile("field-none", pyCFFieldNoneQuery)
	h.validatorIntQ = compile("validator-int", pyCFValidatorIntQuery)
	h.validatorNoneQ = compile("validator-none", pyCFValidatorNoneQuery)
	if err != nil {
		return nil, err
	}
	cfPyLangHandleCache = h
	return h, nil
}

// ─── data types ──────────────────────────────────────────────name──────────────

type cfPyField struct {
	classStart uint32 // startByte of the class_definition node (join key)
	className  string
	fieldName  string
	sentinel   string // integer string or "None"
	line       int    // 1-based line of the field assignment
}

type cfPyValidator struct {
	classStart uint32 // startByte of the class_definition node (join key)
	className  string
	fieldName  string // from the decorator string (unquoted)
	rejectOp   string // comparison operator text
	sentinel   string // rejected value text
	line       int    // 1-based line of the decorator
}

// ─── pass functions ───────────────────────────────────────────────────────────

// passCFPyFields extracts Field(default=X) declarations from class bodies.
func passCFPyFields(h *cfPyLangHandle, tree *gts.Tree, src []byte) []cfPyField {
	var out []cfPyField

	extractFromQuery := func(q *gts.Query, sentinelType string) {
		for _, m := range q.Execute(tree) {
			var classStart uint32
			var className, fieldName, callFn, kwargName, sentinelVal string
			var fieldLine int
			for _, c := range m.Captures {
				switch c.Name {
				case "class.def":
					classStart = c.Node.StartByte()
				case "class.name":
					className = c.Node.Text(src)
				case "field.name":
					fieldName = c.Node.Text(src)
					fieldLine = int(c.Node.StartPoint().Row) + 1
				case "call.fn":
					callFn = c.Node.Text(src)
				case "kwarg.name":
					kwargName = c.Node.Text(src)
				case "kwarg.val":
					sentinelVal = c.Node.Text(src)
				}
			}
			// Gate: only Field() or field() calls with default= keyword.
			if callFn != "Field" && callFn != "field" {
				continue
			}
			if kwargName != "default" {
				continue
			}
			if fieldName == "" || sentinelVal == "" || className == "" {
				continue
			}
			out = append(out, cfPyField{
				classStart: classStart,
				className:  className,
				fieldName:  fieldName,
				sentinel:   sentinelVal,
				line:       fieldLine,
			})
		}
	}

	extractFromQuery(h.fieldIntQ, "integer")
	extractFromQuery(h.fieldNoneQ, "none")
	return out
}

// passCFPyValidators extracts @validator/@field_validator declarations.
func passCFPyValidators(h *cfPyLangHandle, tree *gts.Tree, src []byte) []cfPyValidator {
	var out []cfPyValidator

	extractFromQuery := func(q *gts.Query) {
		for _, m := range q.Execute(tree) {
			var classStart uint32
			var className, decFn, fieldNameRaw, sentinelVal string
			var condNode *gts.Node
			var decLine int
			for _, c := range m.Captures {
				switch c.Name {
				case "class.def":
					classStart = c.Node.StartByte()
				case "class.name":
					className = c.Node.Text(src)
				case "dec.fn":
					decFn = c.Node.Text(src)
					decLine = int(c.Node.StartPoint().Row) + 1
				case "field.name":
					fieldNameRaw = c.Node.Text(src) // still quoted: 'timeout'
				case "guard.sentinel":
					sentinelVal = c.Node.Text(src)
				case "guard.cond":
					condNode = c.Node
				}
			}
			if decFn != "validator" && decFn != "field_validator" {
				continue
			}
			fieldName := unquotePyString(fieldNameRaw)
			if fieldName == "" || sentinelVal == "" || condNode == nil {
				continue
			}
			// Extract operator from comparison_operator node: child[1] is the op token.
			rejectOp := cfPyExtractOp(condNode, h.lang, src)
			if rejectOp == "" {
				continue
			}
			out = append(out, cfPyValidator{
				classStart: classStart,
				className:  className,
				fieldName:  fieldName,
				rejectOp:   rejectOp,
				sentinel:   sentinelVal,
				line:       decLine,
			})
		}
	}

	extractFromQuery(h.validatorIntQ)
	extractFromQuery(h.validatorNoneQ)
	return out
}

// cfPyExtractOp extracts the operator text from a comparison_operator node.
// The operator is the anonymous token at child[1] between the two operands.
func cfPyExtractOp(condNode *gts.Node, lang *gts.Language, src []byte) string {
	if condNode == nil {
		return ""
	}
	// comparison_operator children: [operand, op_token, operand]
	// The operator is child[1] (anonymous, not named).
	if condNode.ChildCount() < 3 {
		return ""
	}
	op := condNode.Child(1)
	if op == nil {
		return ""
	}
	return strings.TrimSpace(op.Text(src))
}

// cfPyContradicts reports whether a field's default is rejected by a validator.
//
// Examples:
//
//	default=0 + reject v<=0  → yes (0 <= 0)
//	default=-1 + reject v<0  → yes (-1 < 0)
//	default=0 + reject v==0  → yes
//	default=5 + reject v<=0  → no  (5 > 0)
//	default=None + reject is None → yes
func cfPyContradicts(field cfPyField, val cfPyValidator) bool {
	fSentinel := field.sentinel
	vSentinel := val.sentinel
	op := val.rejectOp

	// None sentinel.
	if fSentinel == "None" {
		return op == "is" && vSentinel == "None"
	}
	if vSentinel == "None" {
		return false
	}

	fi, err1 := strconv.ParseInt(fSentinel, 10, 64)
	vi, err2 := strconv.ParseInt(vSentinel, 10, 64)
	if err1 != nil || err2 != nil {
		return false
	}

	switch op {
	case "==":
		return fi == vi
	case "<=":
		return fi <= vi
	case "<":
		return fi < vi
	case ">=":
		return fi >= vi
	case ">":
		return fi > vi
	case "!=":
		return fi != vi
	}
	return false
}

// ─── seed entrypoint ──────────────────────────────────────────────────────────

func seedConfigFieldPyContradictions(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	h, err := loadCFPyLangHandle()
	if err != nil {
		return nil // grammar unavailable — degrade silently
	}

	leadsPosted := 0
	seen := make(map[leadKey]bool)

	for _, f := range snap.Files {
		if f.Language != ingest.LangPython {
			continue
		}
		if isPyTestPath(f.Path) {
			continue
		}
		abs := filepath.Join(snap.Root, filepath.FromSlash(f.Path))
		fi, statErr := os.Stat(abs)
		if statErr != nil {
			continue
		}
		if fi.Size() > maxFileBytes {
			continue
		}
		raw, readErr := os.ReadFile(abs)
		if readErr != nil {
			continue
		}

		parser := gts.NewParser(h.lang)
		tree, parseErr := parser.Parse(raw)
		if parseErr != nil || tree == nil {
			continue
		}
		if tree.RootNode().HasError() {
			tree.Release()
			continue
		}

		fields := passCFPyFields(h, tree, raw)
		validators := passCFPyValidators(h, tree, raw)
		tree.Release()

		if len(fields) == 0 || len(validators) == 0 {
			continue
		}

		// Join on (classStart, fieldName): same class_definition node + same field name.
		for _, field := range fields {
			for _, val := range validators {
				if field.classStart != val.classStart {
					continue // different class_definition nodes
				}
				if field.fieldName != val.fieldName {
					continue
				}
				if !cfPyContradicts(field, val) {
					continue
				}
				k := leadKey{TargetLens: cfPyTargetLens, File: f.Path, Line: field.line}
				if seen[k] {
					continue
				}
				seen[k] = true
				note := fmt.Sprintf(
					"config-field-py: %s.%s has default=%s but validator at line %d rejects it (%s %s)",
					field.className, field.fieldName, field.sentinel,
					val.line, val.rejectOp, val.sentinel,
				)
				if addErr := st.AddLead(ctx, store.Lead{
					PosterLens: cfPyPosterLens,
					TargetLens: cfPyTargetLens,
					File:       f.Path,
					Line:       field.line,
					Note:       truncate(note, noteMaxLen),
				}); addErr != nil {
					return fmt.Errorf("miner: config-field-py lead %s: %w", f.Path, addErr)
				}
				sum.ConfigFieldPyLeads++
				sum.LeadsPosted++
				leadsPosted++
				if leadsPosted >= cfPyMaxLeads {
					return nil
				}
			}
		}
	}
	return nil
}
