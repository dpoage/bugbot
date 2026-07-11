package miner

// Stringly-typed drift miner — closed-enum model.
//
// Motivation: Go code that defines a named string type with a closed set of
// const values (e.g. `type Status string; const StatusActive Status = "active"`)
// is a stringly-typed enum. When a switch dispatches on such a type, a case
// using a raw literal that does not match any const value is a typo or stale
// branch; a const value never covered by any case in the switch is a missing
// arm.
//
// Detection algorithm (file-local, type-anchored):
//
//  1. passNamedStringTypes: scan for `type X string` declarations.
//
//  2. passStringEnumConsts: scan for const declarations whose type is one of
//     the named string types; record each (type, constName) → literalValue.
//
//  3. passEnumSwitches: scan for switch statements where the scrutinee can be
//     resolved to one of the named types (by looking at function parameters,
//     variable declarations, and local variable types in the same file).
//     Within each resolved switch, collect all case string literals.
//
//  4. Join — per switch block anchored to a named type:
//     a. Flag each case that uses a raw string literal NOT equal to any const
//        value of the type  (typo / dead branch).
//     b. Flag each const value of the type that is NOT handled by any case in
//        the switch (missing arm).
//
// Scope: only switches whose scrutinee resolves to a named string type in the
// SAME FILE are analyzed. Switches over raw strings, interface values, or
// externally-typed values are entirely out of scope — this is what keeps the
// miner zero-FP on external-command dispatches in interp.go and
// capabilities.go.
//
// Leads: PosterLens="miner:stringly-drift", TargetLens="api-contract-misuse".
// File/Line points at the consumer (case) site for type-A; at the switch line
// for type-B (missing arm).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

const (
	stringlyPosterLens = "miner:stringly-drift"
	stringlyTargetLens = "api-contract-misuse"
	minStringyLen      = 4
)

// stringyStoplist augments the generic entity stoplist with domain words that
// appear frequently in producer positions but carry no dispatch-seam signal.
var stringyStoplist = map[string]bool{
	// HTTP methods
	"get": true, "post": true, "put": true, "delete": true, "patch": true,
	"head": true, "options": true, "connect": true, "trace": true,
	// log levels
	"debug": true, "info": true, "warn": true, "warning": true,
	"error": true, "fatal": true, "panic": true,
	// boolean-ish sentinels
	"true": true, "false": true, "yes": true,
	// common sentinel prose
	"null": true, "none": true, "both": true,
	// JSON schema primitive types — appear in both producer and consumer
	// positions across any JSON-handling code; not discriminating
	"integer": true, "number": true, "boolean": true, "object": true,
	"array": true, "string": true,
	// Code/AST entity kind names — ubiquitous in tooling code
	"function": true, "method": true, "class": true, "module": true,
	"variable": true, "constant": true, "interface": true, "struct": true,
	"field": true, "param": true, "import": true, "package": true,
	// commonly returned action/state words — too generic
	"done": true, "next": true, "stop": true, "fail": true,
	"pass": true, "skip": true, "keep": true, "drop": true,
	"read": true, "write": true, "open": true, "close": true,
	"send": true, "recv": true, "call": true, "wait": true,
	"init": true, "exit": true, "quit": true, "kill": true,
	"load": true, "save": true, "sync": true, "push": true,
	"pull": true, "poll": true, "ping": true, "pong": true,
	"auth": true, "sign": true, "hash": true, "seal": true,
	"list": true, "find": true, "scan": true, "walk": true,
	"dump": true, "view": true, "show": true, "hide": true,
	"move": true, "copy": true, "link": true, "bind": true,
	"lock": true, "free": true, "swap": true, "wrap": true,
	"join": true, "fork": true, "exec": true, "boot": true,
	"idle": true, "busy": true, "live": true,
}

// enumConst records one constant in a named string type's const set.
type enumConst struct {
	typeName string
	name     string // Go identifier (e.g. StatusActive)
	value    string // string literal value (e.g. "active")
	line     int
}

// enumSwitch records one switch block resolved to a named string type.
type enumSwitch struct {
	file       string
	switchLine int
	typeName   string
	hasDefault bool // true when the switch has a default: clause
	// cases: entries for each case arm (may be raw string literal or
	// const identifier reference). Multi-literal cases expand to multiple entries.
	cases []enumCaseLit
}

type enumCaseLit struct {
	value    string // non-empty when a raw string literal was used
	identRef string // non-empty when a bare identifier was used (may be a const name)
	line     int
}

// namedStringTypeRe matches `type Foo string` (defined type only).
// It deliberately does NOT match `type Foo = string` aliases — aliases are
// identity-string and would re-admit the raw-string FP class the miner is
// designed to avoid. Capture 1 = type name.
var namedStringTypeRe = regexp.MustCompile(`^\s*type\s+(\w+)\s+string\b`)

// constTypedRe matches a const declaration with an explicit named type and a
// string literal value.  Handles both block and single-line forms:
//
//	Foo TypeName = "value"
//
// Capture 1 = const identifier, capture 2 = type name, capture 3 = value.
var constTypedRe = regexp.MustCompile(`^\s*(\w+)\s+(\w+)\s*=\s*"([^"\\]{1,128})"`)

// switchScrutineeRe captures the expression between `switch` and `{`.
// Capture 1 = scrutinee expression (may be empty for switch { ... }).
var switchScrutineeRe = regexp.MustCompile(`^\s*switch\s+([^{]+)\s*\{`)

// caseMultiLitRe matches a case line and captures all comma-separated quoted
// string literals (no escape support needed for enum values).
// We split the case-line ourselves after detecting `case `.
var caseLineRe = regexp.MustCompile(`^\s*case\s+(.+):`)

// singleStringRe extracts "..." literals from a case expression list.
var singleStringRe = regexp.MustCompile(`"([^"\\]{0,128})"`)

// varDeclRe matches `name TypeName` in a func param list (two adjacent identifiers).
// Capture 1 = var name, capture 2 = type name.
// NOTE: do NOT use this for `var` statements — it greedily matches `var name`
// as the pair instead of `name Type`. Use varTypedDeclRe / varInferredDeclRe for that.
var varDeclRe = regexp.MustCompile(`\b(\w+)\s+(\w+)\b`)

// varTypedDeclRe matches a `var name TypeName` statement (with explicit type, single name).
// Capture 1 = variable name, capture 2 = type name.
// The optional `= ...` suffix is intentionally ignored — the type is explicit.
// For grouped `var name, other TypeName` use isVarRebindTarget / isVarTypedEnum instead.
var varTypedDeclRe = regexp.MustCompile(`^\s*var\s+(\w+)\s+(\w+)\b`)

// shortDeclLHSRe extracts all comma-separated identifiers on the LHS of `:=`.
// It matches the entire `ident, ident, ... :=` prefix so we can check all names.
// This covers: `name :=`, `a, name :=`, `for name :=`, `for k, name := range`.
var shortDeclLHSRe = regexp.MustCompile(`^([^:=]+):=`)

// wordRe extracts word tokens (identifiers) from a string.
var wordRe = regexp.MustCompile(`\b(\w+)\b`)

// sanitizeLine blanks the content of inline double-quoted string literals,
// rune literals, and strips trailing // line comments from a Go source line.
// This prevents brace counters and identifier matchers from reacting to `}`,
// `"`, or identifier-shaped text inside string/rune values or comments.
//
// Rune literals: while outside a double-quoted string or comment, a `'` opens
// a rune literal; content is blanked until the closing `'`, respecting
// backslash escapes (`'\”`, `'\n'`, `'\\'`). A `'` inside a double-quoted
// string does NOT start a rune, and a `"` inside a rune does NOT start a string.
//
// It does NOT handle backtick strings (covered by buildBacktickMask) or block
// comments (covered by buildBlockCommentMask). The result has the same byte
// length as the input so line offsets remain valid.
func sanitizeLine(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	inString := false
	inRune := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if inString {
			if ch == '\\' {
				// Escape sequence: write both bytes as spaces.
				b.WriteByte(' ')
				if i+1 < len(line) {
					i++
					b.WriteByte(' ')
				}
				continue
			}
			if ch == '"' {
				inString = false
				b.WriteByte(ch)
				continue
			}
			// Inside string — blank the content.
			b.WriteByte(' ')
			continue
		}
		if inRune {
			if ch == '\\' {
				// Escape sequence: write both bytes as spaces.
				b.WriteByte(' ')
				if i+1 < len(line) {
					i++
					b.WriteByte(' ')
				}
				continue
			}
			if ch == '\'' {
				inRune = false
				b.WriteByte(ch)
				continue
			}
			// Inside rune literal — blank the content.
			b.WriteByte(' ')
			continue
		}
		// Outside string and rune.
		if ch == '"' {
			inString = true
			b.WriteByte(ch)
			continue
		}
		if ch == '\'' {
			inRune = true
			b.WriteByte(ch)
			continue
		}
		// Check for trailing line comment.
		if ch == '/' && i+1 < len(line) && line[i+1] == '/' {
			// Blank the rest of the line.
			for ; i < len(line); i++ {
				b.WriteByte(' ')
			}
			break
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// isShortDeclTarget returns true if name appears as any LHS identifier before :=.
// Handles all comma-separated positions and for-range forms.
func isShortDeclTarget(line, name string) bool {
	m := shortDeclLHSRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	for _, tok := range wordRe.FindAllString(m[1], -1) {
		if tok == name {
			return true
		}
	}
	return false
}

// isVarRebindTarget returns true if name appears as any declared identifier in
// a `var` statement on this line. It handles:
//
//	var name T          (single typed)
//	var name, other T   (grouped typed)
//	var name = expr     (single inferred — no type provable)
//	var name, other = exprs  (grouped inferred)
func isVarRebindTarget(line, name string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "var ") {
		return false
	}
	// Named list before a type keyword or `=`.
	// Strip `var ` then split on `,` until we hit something that isn't an ident.
	rest := strings.TrimSpace(trimmed[4:]) // after `var `
	// Collect comma-separated idents at the front.
	for _, tok := range strings.FieldsFunc(rest, func(r rune) bool {
		return r == ',' || r == '\t' || r == ' '
	}) {
		tok = strings.TrimSpace(tok)
		if !wordRe.MatchString(tok) {
			break
		}
		if tok == name {
			return true
		}
		// If the token doesn't look like a pure ident (contains `=`), stop.
		if strings.ContainsAny(tok, "=()") {
			break
		}
	}
	return false
}

// varTypedEnumType returns the enum type if the line is `var name EnumType` (single
// name, explicit type that is a known named type). Returns "" for grouped, inferred,
// or non-enum typed declarations.
func varTypedEnumType(line string, namedTypes map[string]bool) (varName, typeName string) {
	m := varTypedDeclRe.FindStringSubmatch(line)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// buildBlockCommentMask returns a per-line boolean mask: true if the line is
// inside a /* ... */ block comment (best-effort, ignores strings containing /*).
func buildBlockCommentMask(lines []string) []bool {
	mask := make([]bool, len(lines))
	inBlock := false
	for i, line := range lines {
		if inBlock {
			mask[i] = true
			if strings.Contains(line, "*/") {
				inBlock = false
			}
		} else {
			if idx := strings.Index(line, "/*"); idx >= 0 {
				before := line[:idx]
				if strings.Count(before, `"`)%2 == 0 {
					inBlock = true
					mask[i] = true
					if strings.Contains(line[idx+2:], "*/") {
						inBlock = false
					}
				}
			}
		}
	}
	return mask
}

// buildBacktickMask returns a per-line boolean mask: true if the line is
// entirely inside a backtick raw-string literal. This prevents matching
// string literals inside raw strings or shell heredocs.
func buildBacktickMask(lines []string) []bool {
	mask := make([]bool, len(lines))
	inRaw := false
	for i, line := range lines {
		if inRaw {
			mask[i] = true
			if idx := strings.Index(line, "`"); idx >= 0 {
				inRaw = false
			}
		} else {
			count := strings.Count(line, "`")
			if count%2 == 1 {
				// odd number of backticks: we enter a raw string on this line
				// The line itself is partially in raw string; mark it masked
				// to avoid false hits inside the raw portion.
				mask[i] = true
				inRaw = true
			}
		}
	}
	return mask
}

// passNamedStringTypes scans a Go source file for `type X string` declarations
// and returns the set of named string type names found.
func passNamedStringTypes(content string) map[string]bool {
	types := make(map[string]bool)
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)
	btMask := buildBacktickMask(lines)
	for i, line := range lines {
		if blockMask[i] || btMask[i] {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if m := namedStringTypeRe.FindStringSubmatch(line); m != nil {
			types[m[1]] = true
		}
	}
	return types
}

// passStringEnumConsts scans a Go source file for const declarations whose
// type is one of the named string types and returns the enumConst slice.
func passStringEnumConsts(content string, namedTypes map[string]bool) []enumConst {
	if len(namedTypes) == 0 {
		return nil
	}
	var out []enumConst
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)
	btMask := buildBacktickMask(lines)
	for i, line := range lines {
		if blockMask[i] || btMask[i] {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		m := constTypedRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		constName, typeName, value := m[1], m[2], m[3]
		if !namedTypes[typeName] {
			continue
		}
		out = append(out, enumConst{
			typeName: typeName,
			name:     constName,
			value:    value,
			line:     i + 1,
		})
	}
	return out
}

// resolveScrutineeType tries to determine the named string type of the switch
// scrutinee expression, scoped to the innermost enclosing function and walking
// outward through enclosing function scopes.
//
// scopeStarts is a list of 0-based line indices (outermost first, innermost last).
// For each scope from innermost to outermost, we scan ALL lines before the switch
// and take the NEAREST (last) binding of the scrutinee name:
//
//   - `var name EnumType` (single name, explicit enum type) → return that type.
//   - Name as ANY LHS token before `:=`                    → return "" (shadow).
//   - Name in ANY position in a `var` statement (grouped,
//     inferred, or non-enum typed)                          → return "" (shadow).
//   - `name EnumType` in param list / other non-var context → return that type.
//   - `name NonEnumType`                                    → return "" (non-enum typed).
//   - No binding in this scope                             → try next outer scope.
//
// Shadow detection is STRUCTURAL (token-as-LHS-target), not form-enumerated:
//   - isShortDeclTarget: checks all comma-separated names on the LHS of `:=`,
//     covering `name :=`, `a, name :=`, `for name :=`, `for k, name := range`.
//   - isVarRebindTarget: checks all comma-separated names in a `var` statement,
//     covering single, grouped, typed, and inferred forms.
//
// A new syntactic form cannot reintroduce the shadow class as long as it uses
// `:=` or `var` to introduce a new binding — both are covered structurally.
// Bare assignment `name = expr` (no `:=`, no `var`) preserves the existing type
// in Go; it is not a shadow and is left to normal binding resolution.
//
// lines is the full file split on "\n"; switchIdx is the 0-based index of the
// switch line. Each scope covers lines[scopeStarts[j]:switchIdx].
func resolveScrutineeType(scrutinee string, namedTypes map[string]bool, lines []string, scopeStarts []int, switchIdx int) string {
	scrutinee = strings.TrimSpace(scrutinee)
	// Only handle simple identifiers (no dots, no calls).
	if strings.ContainsAny(scrutinee, ".()[] \t") {
		return ""
	}
	end := switchIdx
	if end > len(lines) {
		end = len(lines)
	}
	if end < 0 {
		end = 0
	}

	// binding records what a single line says about the scrutinee name.
	type binding struct {
		isEnum   bool   // true → typed decl with a known enum type
		typeName string // only valid when isEnum=true
	}

	// Walk from innermost scope outward (scopeStarts is outermost-first, so
	// iterate in reverse).
	for j := len(scopeStarts) - 1; j >= 0; j-- {
		start := scopeStarts[j]
		if start < 0 || start >= end {
			continue
		}
		scopeLines := lines[start:end]
		blockMask := buildBlockCommentMask(scopeLines)
		btMask := buildBacktickMask(scopeLines)

		// Scan all lines in this scope and record the LAST binding of scrutinee.
		// "Last" = nearest to the switch (highest index i).
		found := false
		var last binding

		for i, line := range scopeLines {
			if blockMask[i] || btMask[i] {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}

			// Structural check 1: `:=` — scrutinee as any LHS token.
			// Covers: `name :=`, `a, name :=`, `for name :=`, `for k, name := range`.
			// ANY appearance as a `:=` LHS target is an unprovoable-type shadow.
			if isShortDeclTarget(line, scrutinee) {
				found = true
				last = binding{isEnum: false}
				continue
			}

			// Structural check 2: `var` — scrutinee as any declared name.
			// Handles single/grouped, typed/inferred forms.
			if isVarRebindTarget(line, scrutinee) {
				found = true
				// Only `var name EnumType` (single name) is provably typed.
				vn, tn := varTypedEnumType(line, namedTypes)
				if vn == scrutinee && namedTypes[tn] {
					last = binding{isEnum: true, typeName: tn}
				} else {
					last = binding{isEnum: false}
				}
				continue
			}

			// Check typed declaration: `name TypeName` in param lists and other contexts.
			// Skip lines that start with `var` — handled above by isVarRebindTarget.
			// Sanitize first so that a `name EnumType` word-pair inside a string
			// literal or trailing // comment does not falsely resolve the scrutinee
			// to an enum type (Finding 3).
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "var ") {
				continue
			}
			sanitized := sanitizeLine(line)
			for _, m := range varDeclRe.FindAllStringSubmatch(sanitized, -1) {
				varName, typeName := m[1], m[2]
				if varName != scrutinee {
					continue
				}
				found = true
				if namedTypes[typeName] {
					last = binding{isEnum: true, typeName: typeName}
				} else {
					last = binding{isEnum: false}
				}
			}
		}

		if found {
			if last.isEnum {
				return last.typeName
			}
			return ""
		}
		// No binding in this scope — try the next outer scope.
	}
	return ""
}

// caseIdentRe extracts bare identifier tokens from a case expression list
// (tokens that are not string literals and not punctuation).
var caseIdentRe = regexp.MustCompile(`\b([A-Za-z_]\w*)\b`)

// extractCaseEntries extracts all case arm entries from a case line.
// It returns enumCaseLit records for each sub-expression:
//   - string literal:  value set, identRef empty
//   - bare identifier: identRef set, value empty
//
// Handles multi-literal cases like `case "a", "b", SomeConst:`.
func extractCaseEntries(caseLine string) []enumCaseLit {
	m := caseLineRe.FindStringSubmatch(caseLine)
	if m == nil {
		return nil
	}
	expr := m[1]
	lineNo := 0 // caller fills in

	// Split on commas, then classify each token.
	parts := strings.Split(expr, ",")
	var out []enumCaseLit
	for _, part := range parts {
		part = strings.TrimSpace(part)
		// String literal?
		if sm := singleStringRe.FindStringSubmatch(part); sm != nil {
			out = append(out, enumCaseLit{value: sm[1], line: lineNo})
			continue
		}
		// Bare identifier (may be a const name)?
		if im := caseIdentRe.FindStringSubmatch(part); im != nil {
			// Skip Go keywords that appear in case lists.
			switch im[1] {
			case "case", "default", "nil", "true", "false":
				continue
			}
			out = append(out, enumCaseLit{identRef: im[1], line: lineNo})
		}
	}
	return out
}

// extractCaseLiterals extracts all quoted string literals from a case line,
// handling multi-literal cases like `case "a", "b", "c":`.
// Retained for passStringConsumers compatibility.
func extractCaseLiterals(caseLine string) []string {
	m := caseLineRe.FindStringSubmatch(caseLine)
	if m == nil {
		return nil
	}
	expr := m[1]
	var vals []string
	for _, sm := range singleStringRe.FindAllStringSubmatch(expr, -1) {
		vals = append(vals, sm[1])
	}
	return vals
}

// funcAnywhereRe detects the `func` keyword anywhere on a line (not just at
// the start), to catch func literals: `return func(...)`, `go func()`, etc.
var funcAnywhereRe = regexp.MustCompile(`\bfunc\b`)

// defaultClauseRe matches a bare default: clause line.
var defaultClauseRe = regexp.MustCompile(`^\s*default\s*:`)

// passEnumSwitches finds switch blocks in the file whose scrutinee resolves to
// a named string type, and returns the resolved enumSwitch records.
func passEnumSwitches(path, content string, namedTypes map[string]bool) []enumSwitch {
	if len(namedTypes) == 0 {
		return nil
	}
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)
	btMask := buildBacktickMask(lines)
	type pendingSwitch struct {
		switchLine int
		typeName   string
		depth      int // brace depth when switch { was opened
	}

	// funcScope tracks the line index where a function body starts.
	// We maintain a stack so that closures (func literals) shadow outer functions.
	type funcScope struct {
		startLine int // 0-based line index of the func declaration
		bodyDepth int // brace depth AFTER the opening { of this func body
	}
	var funcStack []funcScope
	pendingFunc := -1 // 0-based line index of a func keyword awaiting its opening {
	var out []enumSwitch
	var stack []pendingSwitch
	braceDepth := 0

	// pendingCase accumulates a wrapped case expression across continuation lines.
	// gofmt preserves `case "a",\n\t\t"b":` — the expression spans two lines.
	pendingCase := ""    // non-empty while accumulating
	pendingCaseLine := 0 // lineNo of the `case` keyword line

	for i, line := range lines {
		lineNo := i + 1
		if blockMask[i] || btMask[i] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		// Sanitize the line before brace counting so that `}` inside a string
		// literal or a trailing // comment does not skew braceDepth (Finding 1).
		sanitized := sanitizeLine(line)

		// Detect function declarations (top-level, methods, and func literals)
		// so we can scope scrutinee resolution to the innermost enclosing body.
		if funcAnywhereRe.MatchString(line) {
			pendingFunc = i
		}

		// Track brace depth using the sanitized line; push/pop func scopes and switch blocks.
		for _, ch := range sanitized {
			switch ch {
			case '{':
				braceDepth++
				// If a func keyword was seen and this { opens its body, push a scope.
				if pendingFunc >= 0 {
					funcStack = append(funcStack, funcScope{startLine: pendingFunc, bodyDepth: braceDepth})
					pendingFunc = -1
				}
			case '}':
				braceDepth--
				// Pop any switch whose block just closed.
				if len(stack) > 0 && braceDepth < stack[len(stack)-1].depth {
					stack = stack[:len(stack)-1]
				}
				// Pop any func scope whose body just closed.
				if len(funcStack) > 0 && braceDepth < funcStack[len(funcStack)-1].bodyDepth {
					funcStack = funcStack[:len(funcStack)-1]
				}
			}
		}

		// Detect switch keyword.
		if m := switchScrutineeRe.FindStringSubmatch(line); m != nil {
			scrutinee := strings.TrimSpace(m[1])
			// Resolve scrutinee type by walking scope from innermost func outward.
			// Build a slice of start-line indices (outermost first).
			scopeStarts := make([]int, 0, len(funcStack)+1)
			for _, fs := range funcStack {
				scopeStarts = append(scopeStarts, fs.startLine)
			}
			if len(scopeStarts) == 0 {
				// No enclosing func detected; fall back to scanning from file top.
				scopeStarts = []int{0}
			}
			typeName := resolveScrutineeType(scrutinee, namedTypes, lines, scopeStarts, i)
			if typeName != "" {
				stack = append(stack, pendingSwitch{
					switchLine: lineNo,
					typeName:   typeName,
					depth:      braceDepth,
				})
				out = append(out, enumSwitch{
					file:       path,
					switchLine: lineNo,
					typeName:   typeName,
				})
			}
			continue
		}

		// Collect case entries for the innermost active switch.
		if len(stack) == 0 {
			continue
		}
		sw := &stack[len(stack)-1]
		var esw *enumSwitch
		for j := range out {
			if out[j].switchLine == sw.switchLine && out[j].file == path {
				esw = &out[j]
				break
			}
		}
		if esw == nil {
			continue
		}
		// Detect default clause — suppress type-B for this switch.
		if defaultClauseRe.MatchString(line) {
			esw.hasDefault = true
			continue
		}

		// Handle multi-line case expressions (Finding 2).
		// gofmt may emit `case "b",` on one line and `"c":` on the next.
		// Accumulate until the expression ends with `:` (outside parens/brackets).
		if pendingCase != "" {
			// Continuation line: append and check for terminator.
			pendingCase += " " + trimmed
			// Check if the accumulated expression is terminated.
			if isCompleteCaseExpr(pendingCase) {
				entries := extractCaseEntries(pendingCase)
				for k := range entries {
					entries[k].line = pendingCaseLine
				}
				esw.cases = append(esw.cases, entries...)
				pendingCase = ""
				pendingCaseLine = 0
			}
			continue
		}

		// Check for a new case line.
		if strings.HasPrefix(trimmed, "case ") || trimmed == "case" {
			if isCompleteCaseExpr(trimmed) {
				entries := extractCaseEntries(line)
				for k := range entries {
					entries[k].line = lineNo
				}
				esw.cases = append(esw.cases, entries...)
			} else {
				// Incomplete: start accumulation.
				pendingCase = trimmed
				pendingCaseLine = lineNo
			}
			continue
		}
	}

	return out
}

// isCompleteCaseExpr returns true when the case expression (from the `case`
// keyword to the end of the accumulated string) is terminated by a `:` that is
// not inside parentheses, brackets, or braces.
func isCompleteCaseExpr(expr string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inStr {
			if ch == '\\' {
				i++
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// isIdentifierShaped returns true when s looks like a programmatic token:
// snake_case, camelCase, kebab-case, or dot-separated (e.g. "user.created").
// It rejects prose (spaces), format strings (%), path prefixes (/),
// pure numbers, and strings that are too short or too long.
func isIdentifierShaped(s string) bool {
	if len(s) < minStringyLen || len(s) > 64 {
		return false
	}
	// Reject if it contains spaces, tabs, format verbs, newlines.
	if strings.ContainsAny(s, " \t%\r\n") {
		return false
	}
	// Reject pure numbers.
	allDigit := true
	for _, c := range s {
		if c < '0' || c > '9' {
			allDigit = false
			break
		}
	}
	if allDigit {
		return false
	}
	// Must start with a letter (not / or . or digit — those are paths/URLs/numbers).
	first := rune(s[0])
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return false
	}
	// Allowed interior characters: letters, digits, underscore, hyphen, dot, slash.
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
			(c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// isStringyStopped returns true if s appears in either stoplist.
func isStringyStopped(s string) bool {
	lower := strings.ToLower(s)
	return genericEntityStoplist[lower] || stringyStoplist[lower]
}

// consumerSite is retained for unit tests of passStringConsumers.
type consumerSite struct {
	file     string
	line     int
	literal  string
	switchID int
}

// producerSite is retained for unit tests of passStringProducers.
type producerSite struct {
	file    string
	line    int
	literal string
}

// switchKeyRe matches the `switch` keyword that opens a switch block.
var switchKeyRe = regexp.MustCompile(`^\s*switch\b`)

// producerStringRe matches string literals in common producer positions:
//
//	return "..."   = "..."   := "..."   f("...")   , "..."
var producerStringRe = regexp.MustCompile(`(?:return|=|:=|,|\()\s*"([^"\\]{1,128})"`)

// passStringConsumers extracts consumer string literals from switch case arms,
// grouped by switch block (switchID = line of the `switch` keyword).
// Retained for unit tests; the closed-enum seed does not use it directly.
func passStringConsumers(path, content string) []consumerSite {
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)
	btMask := buildBacktickMask(lines)

	var out []consumerSite
	currentSwitchID := -1

	for lineIdx, line := range lines {
		lineNo := lineIdx + 1

		if blockMask[lineIdx] || btMask[lineIdx] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if switchKeyRe.MatchString(line) {
			currentSwitchID = lineNo
		}

		// Extract ALL comma-separated string literals on this case line.
		if strings.Contains(line, "case ") {
			lits := extractCaseLiterals(line)
			for _, lit := range lits {
				if !isIdentifierShaped(lit) || isStringyStopped(lit) {
					continue
				}
				id := currentSwitchID
				if id < 0 {
					id = lineNo
				}
				out = append(out, consumerSite{
					file:     path,
					line:     lineNo,
					literal:  lit,
					switchID: id,
				})
			}
		}
	}
	return out
}

// passStringProducers extracts producer string literals from Go source content.
// Retained for unit tests; the closed-enum seed does not use it directly.
func passStringProducers(path, content string) []producerSite {
	lines := strings.Split(content, "\n")
	blockMask := buildBlockCommentMask(lines)
	btMask := buildBacktickMask(lines)

	var out []producerSite
	for lineIdx, line := range lines {
		lineNo := lineIdx + 1
		if blockMask[lineIdx] || btMask[lineIdx] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, m := range producerStringRe.FindAllStringSubmatch(line, -1) {
			lit := m[1]
			if !isIdentifierShaped(lit) || isStringyStopped(lit) {
				continue
			}
			out = append(out, producerSite{
				file:    path,
				line:    lineNo,
				literal: lit,
			})
		}
	}
	return out
}

// seedStringlyDrift runs the closed-enum stringly-typed drift pass over the
// snapshot and posts leads. Called from Seed (miner.go) after the existing
// passes.
//
// Only files that define at least one named string type with const values are
// analyzed. Switches over raw strings, untyped strings, or external-command
// tokens are entirely out of scope.
func seedStringlyDrift(ctx context.Context, snap *ingest.Snapshot, st *store.Store, sum *Summary) error {
	seen := make(map[leadKey]bool)

	// Collect all file paths; sort for determinism.
	type fileEntry struct {
		path    string
		content string
	}
	var files []fileEntry
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
		files = append(files, fileEntry{path: f.Path, content: string(data)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	for _, fe := range files {
		// Phase 1: discover named string types in this file.
		namedTypes := passNamedStringTypes(fe.content)
		if len(namedTypes) == 0 {
			continue // no named string types → nothing to analyze
		}

		// Phase 2: collect the const set for each named type.
		consts := passStringEnumConsts(fe.content, namedTypes)
		if len(consts) == 0 {
			continue // type declared but no const values → not a closed enum
		}

		// Build type → set-of-values map.
		typeValues := make(map[string]map[string]bool)
		for _, c := range consts {
			if typeValues[c.typeName] == nil {
				typeValues[c.typeName] = make(map[string]bool)
			}
			typeValues[c.typeName][c.value] = true
		}

		// Phase 3: find switches anchored to one of these types.
		switches := passEnumSwitches(fe.path, fe.content, namedTypes)

		for _, sw := range switches {
			constVals, ok := typeValues[sw.typeName]
			if !ok || len(constVals) == 0 {
				continue
			}

			// Build constName → value lookup for this type.
			constNameVal := make(map[string]string) // constName → literal value
			for _, c := range consts {
				if c.typeName == sw.typeName {
					constNameVal[c.name] = c.value
				}
			}

			// Compute which const values are covered by this switch.
			// A value is covered if:
			//   (a) a raw string literal case matches it exactly, OR
			//   (b) a bare identifier case is a const name whose value matches it.
			coveredByLiteral := make(map[string]bool) // literal value → covered
			coveredByIdent := make(map[string]bool)   // literal value → covered via ident
			hasAnyLiteral := false

			for _, c := range sw.cases {
				if c.value != "" {
					// Raw string literal case.
					hasAnyLiteral = true
					coveredByLiteral[c.value] = true
				} else if c.identRef != "" {
					// Identifier case — look up its value.
					if val, ok := constNameVal[c.identRef]; ok {
						coveredByIdent[val] = true
					}
				}
			}

			// Type-A: only when the switch uses raw string literals.
			// Flag each literal that does not match any const value of the type.
			if hasAnyLiteral {
				for _, c := range sw.cases {
					if c.value == "" {
						continue // identifier case — not a literal drift candidate
					}
					if constVals[c.value] {
						continue // matches a defined const value
					}
					k := leadKey{TargetLens: stringlyTargetLens, File: fe.path, Line: c.line}
					if seen[k] {
						continue
					}
					seen[k] = true

					note := fmt.Sprintf(
						"stringly-drift: case literal %q at %s:%d does not match "+
							"any const value of type %s; likely a typo or stale branch",
						c.value, fe.path, c.line, sw.typeName,
					)
					note = truncate(note, noteMaxLen)

					if err := st.AddLead(ctx, store.Lead{
						PosterLens: stringlyPosterLens,
						TargetLens: stringlyTargetLens,
						File:       fe.path,
						Line:       c.line,
						Note:       note,
					}); err != nil {
						return fmt.Errorf("miner: stringly-drift lead %s:%d: %w", fe.path, c.line, err)
					}
					sum.StringlyDriftLeads++
					sum.LeadsPosted++
					if sum.LeadsPosted >= maxLeads {
						return nil
					}
				}
			}

			// Type-B: const value not covered by any case (literal or ident).
			// Only fire when the switch uses at least one raw string literal arm
			// (indicating the author is using raw literals for dispatch).
			// Switches that use only const identifiers are correct by construction.
			// Switches with a default: clause explicitly handle remaining values —
			// suppress type-B to avoid false positives on the explicit-subset idiom.
			if !hasAnyLiteral || sw.hasDefault {
				continue
			}
			// Collect and sort uncovered values for deterministic output.
			var uncovered []string
			for val := range constVals {
				if !coveredByLiteral[val] && !coveredByIdent[val] {
					uncovered = append(uncovered, val)
				}
			}
			sort.Strings(uncovered)
			for _, val := range uncovered {
				k := leadKey{TargetLens: stringlyTargetLens, File: fe.path, Line: sw.switchLine}
				if seen[k] {
					continue
				}
				seen[k] = true

				note := fmt.Sprintf(
					"stringly-drift: switch at %s:%d handles type %s but "+
						"missing case for const value %q",
					fe.path, sw.switchLine, sw.typeName, val,
				)
				note = truncate(note, noteMaxLen)

				if err := st.AddLead(ctx, store.Lead{
					PosterLens: stringlyPosterLens,
					TargetLens: stringlyTargetLens,
					File:       fe.path,
					Line:       sw.switchLine,
					Note:       note,
				}); err != nil {
					return fmt.Errorf("miner: stringly-drift lead %s:%d: %w", fe.path, sw.switchLine, err)
				}
				sum.StringlyDriftLeads++
				sum.LeadsPosted++
				if sum.LeadsPosted >= maxLeads {
					return nil
				}
			}
		}
	}

	return nil
}
