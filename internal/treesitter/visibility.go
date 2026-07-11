package treesitter

import (
	"bytes"
	"strings"
)

// Visibility is the access level of a declared symbol. The zero value is
// VisibilityUnknown, which triggers the caller's name-heuristic fallback.
type Visibility string

const (
	// VisibilityPublic means the symbol is part of the file's external API
	// (extern linkage in C, non-static free function/type in C++, public
	// member or pub item in Rust).
	VisibilityPublic Visibility = "public"

	// VisibilityPrivate means the symbol is definitely not reachable outside
	// the translation unit: static linkage in C, anonymous namespace or static
	// free function/type in C++, no pub modifier in Rust.
	VisibilityPrivate Visibility = "private"

	// VisibilityUnknown means this file's language does not carry syntactic
	// visibility markers the analyzer can determine; callers fall back to
	// name-based heuristics.
	VisibilityUnknown Visibility = "unknown"
)

// ---- C visibility -------------------------------------------------------

// cVisibilityMap analyzes a C source and returns 0-based start row →
// Visibility for each definition row in startRows. A definition preceded by
// the "static" storage-class specifier (anywhere on the same line or up to 3
// lines above it) is private; all others are public.
func cVisibilityMap(src []byte, startRows []uint32) map[uint32]Visibility {
	lines := bytes.Split(src, []byte("\n"))
	out := make(map[uint32]Visibility, len(startRows))
	for _, row := range startRows {
		out[row] = cLineVisibility(lines, row)
	}
	return out
}

// cLineVisibility returns the visibility of a C definition whose node begins
// at row (0-based). Tree-sitter's function_definition start row is the line
// that holds the storage-class specifier and return type, so checking only
// that single line is sufficient to detect "static". Looking further back
// would cross into the preceding declaration and produce false-private results.
func cLineVisibility(lines [][]byte, row uint32) Visibility {
	if int(row) >= len(lines) {
		return VisibilityPublic
	}
	if hasStaticToken(stripLineComment(lines[row])) {
		return VisibilityPrivate
	}
	return VisibilityPublic
}

// ---- C++ visibility -----------------------------------------------------

// cppCtxKind classifies the scope a brace pair introduces.
type cppCtxKind int

const (
	cppCtxOther   cppCtxKind = iota // function body, extern "C", if/for, etc.
	cppCtxStruct                    // struct body (default public)
	cppCtxClass                     // class body (default private)
	cppCtxAnonNS                    // anonymous namespace (all members private)
	cppCtxNamedNS                   // named namespace (public by default)
)

type cppScope struct {
	kind         cppCtxKind
	accessPublic bool // current access level (meaningful for struct/class)
	depth        int  // braceDepth when this scope was pushed
}

// cppVisibilityMap analyzes a C++ source and returns 0-based start row →
// Visibility for each definition row in startRows.
//
// Rules applied in priority order:
//  1. Inside an anonymous namespace → private.
//  2. Inside a struct/class member context → access specifier governs
//     (struct default public, class default private, overridden by
//     public:/private:/protected: lines).
//  3. At file/named-namespace scope with "static" keyword → private.
//  4. Everything else → public.
func cppVisibilityMap(src []byte, startRows []uint32) map[uint32]Visibility {
	lines := bytes.Split(src, []byte("\n"))

	defRows := make(map[uint32]bool, len(startRows))
	for _, r := range startRows {
		defRows[r] = true
	}
	out := make(map[uint32]Visibility, len(startRows))

	// Stack of open scopes. Index 0 is always file scope (public).
	stack := []cppScope{{kind: cppCtxOther, accessPublic: true, depth: 0}}
	braceDepth := 0

	for rowIdx := range lines {
		row := uint32(rowIdx)
		line := stripLineComment(lines[rowIdx])
		trimmed := strings.TrimSpace(line)

		// Record visibility for any definition that starts on this row,
		// using the scope context BEFORE processing this line's braces so
		// that a '{' on the definition line does not self-affect the entry.
		if defRows[row] {
			cur := stack[len(stack)-1]
			switch {
			case cur.kind == cppCtxAnonNS:
				out[row] = VisibilityPrivate
			case cur.kind == cppCtxStruct || cur.kind == cppCtxClass:
				if cur.accessPublic {
					out[row] = VisibilityPublic
				} else {
					out[row] = VisibilityPrivate
				}
			default: // file or named-namespace scope
				if hasStaticToken(line) {
					out[row] = VisibilityPrivate
				} else {
					out[row] = VisibilityPublic
				}
			}
		}

		// Update access-specifier state for struct/class scopes.
		top := &stack[len(stack)-1]
		if top.kind == cppCtxStruct || top.kind == cppCtxClass {
			switch {
			case strings.HasPrefix(trimmed, "public:"):
				top.accessPublic = true
			case strings.HasPrefix(trimmed, "private:"):
				top.accessPublic = false
			case strings.HasPrefix(trimmed, "protected:"):
				top.accessPublic = false
			}
		}

		// Walk the line counting braces and pushing/popping scopes.
		inStr := false
		inCh := false
		for i := range len(line) {
			ch := line[i]
			if ch == '\\' && (inStr || inCh) {
				// skip next char
				continue
			}
			switch {
			case ch == '"' && !inCh:
				inStr = !inStr
			case ch == '\'' && !inStr:
				inCh = !inCh
			case !inStr && !inCh && ch == '{':
				braceDepth++
				before := strings.TrimSpace(line[:i])
				stack = append(stack, cppInferScope(before, braceDepth))
			case !inStr && !inCh && ch == '}':
				// Pop all scopes pushed at this depth.
				for len(stack) > 1 && stack[len(stack)-1].depth == braceDepth {
					stack = stack[:len(stack)-1]
				}
				if braceDepth > 0 {
					braceDepth--
				}
			}
		}
	}
	return out
}

// cppInferScope inspects the text before an opening brace and returns the
// appropriate scope record. before is the trimmed text on the line before the
// '{'; depth is the brace depth after counting the '{'.
func cppInferScope(before string, depth int) cppScope {
	// Anonymous namespace: "namespace" keyword with no following identifier.
	if isCppAnonNamespace(before) {
		return cppScope{kind: cppCtxAnonNS, accessPublic: false, depth: depth}
	}
	// Named namespace.
	if hasWholeWord(before, "namespace") {
		return cppScope{kind: cppCtxNamedNS, accessPublic: true, depth: depth}
	}
	// struct → default public.
	if hasWholeWord(before, "struct") {
		return cppScope{kind: cppCtxStruct, accessPublic: true, depth: depth}
	}
	// class → default private.
	if hasWholeWord(before, "class") {
		return cppScope{kind: cppCtxClass, accessPublic: false, depth: depth}
	}
	return cppScope{kind: cppCtxOther, accessPublic: true, depth: depth}
}

// isCppAnonNamespace reports whether before (the text preceding '{') is an
// anonymous namespace opener: the word "namespace" appears and is NOT
// immediately followed by a plain identifier name.
func isCppAnonNamespace(before string) bool {
	idx := strings.LastIndex(before, "namespace")
	if idx < 0 {
		return false
	}
	// Confirm "namespace" is a whole word.
	if idx > 0 && isIdentChar(before[idx-1]) {
		return false
	}
	rest := strings.TrimSpace(before[idx+len("namespace"):])
	if rest == "" {
		return true // "namespace" then immediate '{'
	}
	// If the next char is an identifier start, it's a named namespace.
	return !isIdentChar(rest[0])
}

// ---- Rust visibility -----------------------------------------------------

// rustVisibilityMap analyzes a Rust source and returns 0-based start row →
// Visibility for each definition row in startRows. Presence of a "pub" or
// "pub(...)" modifier on the definition line or on a preceding non-attribute
// line (up to 5 lines back) → public; absence → private.
//
// This function is implemented and tested but its results are only consumed
// once the Rust grammar lands (tdq5.1). Until then no .rs tags are produced
// by tagFile, so the map is never queried in production.
func rustVisibilityMap(src []byte, startRows []uint32) map[uint32]Visibility {
	lines := bytes.Split(src, []byte("\n"))
	out := make(map[uint32]Visibility, len(startRows))
	for _, row := range startRows {
		out[row] = rustLineVisibility(lines, row)
	}
	return out
}

func rustLineVisibility(lines [][]byte, row uint32) Visibility {
	if int(row) >= len(lines) {
		return VisibilityPrivate
	}
	if hasPubToken(lines[row]) {
		return VisibilityPublic
	}
	// Walk backward past attribute lines (#[...]) to find pub on a prior line.
	for i := 1; i <= 5; i++ {
		prev := int(row) - i
		if prev < 0 {
			break
		}
		trimmed := bytes.TrimSpace(lines[prev])
		if len(trimmed) == 0 {
			break // blank line between declarations
		}
		if len(trimmed) > 0 && trimmed[0] == '#' {
			continue // attribute line — keep scanning up
		}
		if hasPubToken(lines[prev]) {
			return VisibilityPublic
		}
		break
	}
	return VisibilityPrivate
}

// ---- shared helpers ------------------------------------------------------

// stripLineComment removes a C/C++ "//" line comment and everything after it.
// Block comments are not handled — this is best-effort for visibility detection.
func stripLineComment(line []byte) string {
	s := string(line)
	inStr := false
	for i := range len(s) - 1 {
		ch := s[i]
		if ch == '\\' && inStr {
			continue
		}
		if ch == '"' {
			inStr = !inStr
			continue
		}
		if !inStr && ch == '/' && s[i+1] == '/' {
			return s[:i]
		}
	}
	return s
}

// hasStaticToken reports whether s contains the keyword "static" as a whole
// word (not a substring of another identifier).
func hasStaticToken(s string) bool {
	return hasWholeWord(s, "static")
}

// hasPubToken reports whether line contains the Rust "pub" visibility modifier
// as a whole word. "pub(crate)" / "pub(super)" etc. also match because the
// three characters "pub" are followed by '(' which is not an identifier char.
func hasPubToken(line []byte) bool {
	s := string(line)
	if idx := strings.Index(s, "//"); idx >= 0 {
		s = s[:idx]
	}
	return hasWholeWord(s, "pub")
}

// hasWholeWord reports whether keyword kw appears as a whole word in s.
func hasWholeWord(s, kw string) bool {
	for {
		idx := strings.Index(s, kw)
		if idx < 0 {
			return false
		}
		before := idx == 0 || !isIdentChar(s[idx-1])
		after := idx+len(kw) >= len(s) || !isIdentChar(s[idx+len(kw)])
		if before && after {
			return true
		}
		s = s[idx+len(kw):]
	}
}

// isIdentChar reports whether c is a valid C/C++/Rust identifier character.
func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
