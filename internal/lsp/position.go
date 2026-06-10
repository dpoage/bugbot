package lsp

import "unicode/utf8"

// UTF16Col converts a zero-based byte offset within line to the zero-based
// UTF-16 code-unit offset LSP expects. Offsets past the end of the line clamp
// to the line's UTF-16 length. An offset that lands mid-rune counts the rune it
// falls inside as not yet reached (i.e. it behaves like the rune's start).
func UTF16Col(line string, byteOff int) int {
	if byteOff < 0 {
		return 0
	}
	col := 0
	for i, r := range line {
		if i >= byteOff {
			return col
		}
		col += utf16Len(r)
	}
	return col
}

// ByteCol converts a zero-based UTF-16 code-unit offset (as reported by an LSP
// server) to a byte offset within line. Offsets past the end of the line clamp
// to len(line). An offset that lands on the low surrogate of a surrogate pair
// resolves to the next rune boundary.
func ByteCol(line string, u16 int) int {
	if u16 <= 0 {
		return 0
	}
	col := 0
	for i, r := range line {
		if col >= u16 {
			return i
		}
		col += utf16Len(r)
	}
	return len(line)
}

// utf16Len is the number of UTF-16 code units encoding r (2 for runes outside
// the BMP, which need a surrogate pair; 1 otherwise). Invalid runes decode as
// U+FFFD which is 1 unit, matching how servers treat invalid UTF-8.
func utf16Len(r rune) int {
	if r > 0xFFFF && utf8.ValidRune(r) {
		return 2
	}
	return 1
}
