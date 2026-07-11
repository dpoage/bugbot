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

// ---- unified C/C++ line sanitizer ---------------------------------------
//
// sanitizeCLine produces a "clean" version of one source line suitable for
// structural scanning (brace counting, keyword detection, access-specifier
// matching). It applies ALL of the following masking operations in a single
// pass so that no half-lexer can be tricked by the other's blind spots:
//
//  1. Cross-line block comments (/* … */): the caller threads inBC across
//     loop iterations so a /* that opens on one line is still "in comment"
//     on the next. Characters inside block comments are replaced with spaces
//     (not deleted) to preserve column offsets for the `line[:i]` prefix used
//     by cppInferScope.
//
//  2. C-style char literals ('x', '\n', '\\'): the inCh state prevents a
//     char containing a double-quote (e.g. '"') from flipping the string
//     state. Without this, char q = '"'; poisons the rest of the line.
//
//  3. String literals ("…"): braces and slashes inside strings are replaced
//     with spaces so they never reach the brace counter or keyword scanner.
//
//  4. Line comments (//): once seen outside all other contexts, the rest of
//     the line is discarded.
//
// The returned string has the same length as the input up to the point where
// a // terminates it. Chars inside comments/strings are replaced with ' '.
// This makes line[:i] prefix slices (used by cppInferScope) always safe.
//
// inBC is a pointer to a bool that persists across lines in the calling loop.
func sanitizeCLine(line []byte, inBC *bool) string {
	out := make([]byte, len(line))
	copy(out, line)

	i := 0
	for i < len(out) {
		// Inside a block comment: scan for closing */.
		if *inBC {
			if i+1 < len(out) && out[i] == '*' && out[i+1] == '/' {
				out[i] = ' '
				out[i+1] = ' '
				*inBC = false
				i += 2
			} else {
				out[i] = ' '
				i++
			}
			continue
		}

		ch := out[i]

		// Opening block comment: /* (can open and close on the same line).
		if ch == '/' && i+1 < len(out) && out[i+1] == '*' {
			out[i] = ' '
			out[i+1] = ' '
			i += 2
			*inBC = true
			// Keep going — the */ might be on the same line.
			continue
		}

		// Line comment: everything from // onward is removed.
		if ch == '/' && i+1 < len(out) && out[i+1] == '/' {
			return string(out[:i])
		}

		// Char literal: 'x' or '\n' or '\\'.
		if ch == '\'' {
			out[i] = ' '
			i++
			// Escaped char: '\X'
			if i < len(out) && out[i] == '\\' {
				out[i] = ' '
				i++
			}
			// The char value.
			if i < len(out) && out[i] != '\'' {
				out[i] = ' '
				i++
			}
			// Closing quote.
			if i < len(out) && out[i] == '\'' {
				out[i] = ' '
				i++
			}
			continue
		}

		// String literal: replace contents with spaces.
		if ch == '"' {
			out[i] = ' '
			i++
			for i < len(out) {
				c := out[i]
				if c == '\\' {
					out[i] = ' '
					i++
					if i < len(out) {
						out[i] = ' '
						i++
					}
					continue
				}
				if c == '"' {
					out[i] = ' '
					i++
					break
				}
				out[i] = ' '
				i++
			}
			continue
		}

		i++
	}
	return string(out)
}

// ---- C visibility -------------------------------------------------------

// cVisibilityMap analyzes a C source and returns 0-based start row →
// Visibility for each definition row in startRows. A definition with the
// "static" storage-class specifier on its start row is private; all others
// are public.
func cVisibilityMap(src []byte, startRows []uint32) map[uint32]Visibility {
	rawLines := bytes.Split(src, []byte("\n"))

	// Build sanitized lines (block-comment state threaded).
	inBC := false
	lines := make([]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = sanitizeCLine(l, &inBC)
	}

	out := make(map[uint32]Visibility, len(startRows))
	for _, row := range startRows {
		if int(row) >= len(lines) {
			out[row] = VisibilityPublic
			continue
		}
		if hasStaticToken(lines[row]) {
			out[row] = VisibilityPrivate
		} else {
			out[row] = VisibilityPublic
		}
	}
	return out
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
//
// The sanitizeCLine pre-pass blanks out block comments, char literals, string
// literals, and line-comment tails before the brace counter or access-specifier
// scanner ever sees the line. inBC is threaded across lines so multi-line block
// comments are fully masked even when /* and */ appear on different lines.
func cppVisibilityMap(src []byte, startRows []uint32) map[uint32]Visibility {
	rawLines := bytes.Split(src, []byte("\n"))

	// Single sanitization pass: produces clean lines with consistent masking.
	inBC := false
	cleanLines := make([]string, len(rawLines))
	for i, l := range rawLines {
		cleanLines[i] = sanitizeCLine(l, &inBC)
	}

	defRows := make(map[uint32]bool, len(startRows))
	for _, r := range startRows {
		defRows[r] = true
	}
	out := make(map[uint32]Visibility, len(startRows))

	// Stack of open scopes. Index 0 is always file scope (public).
	stack := []cppScope{{kind: cppCtxOther, accessPublic: true, depth: 0}}
	braceDepth := 0

	for rowIdx := range cleanLines {
		row := uint32(rowIdx)
		line := cleanLines[rowIdx]
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
		// After sanitization these lines are guaranteed comment-free.
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

		// Walk the clean line counting braces and pushing/popping scopes.
		// No string/comment tracking needed here: sanitizeCLine already
		// replaced all masked chars with spaces.
		for i := range len(line) {
			ch := line[i]
			switch ch {
			case '{':
				braceDepth++
				before := strings.TrimSpace(line[:i])
				stack = append(stack, cppInferScope(before, braceDepth))
			case '}':
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
// "pub(...)" modifier on the definition row → public; absence → private.
// pub(crate)/pub(super) count as public: crate-internal is still cross-file
// visible, which is what the deep-refs closure cares about.
//
// The definition row is the tag node's start row. In tree-sitter-rust the
// visibility_modifier is the FIRST CHILD of the item node (outer #[...]
// attributes are separate sibling nodes), so when an item has a pub modifier
// it is always on the node's start row — the same-line check is complete.
// Checking any earlier line would read the PREVIOUS item's declaration and
// inherit its visibility (activation bug caught by
// TestOutlineVisibility_Rust_PubActivation).
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
	return VisibilityPrivate
}

// ---- shared helpers ------------------------------------------------------

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
