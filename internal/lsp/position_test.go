package lsp

import (
	"strings"
	"testing"
)

func TestUTF16Col(t *testing.T) {
	// "héllo🎉中x": h(1B/1u) é(2B/1u) l l o(1B/1u each) 🎉(4B/2u) 中(3B/1u) x(1B/1u)
	line := "héllo🎉中x"
	xByte := strings.IndexByte(line, 'x')
	if xByte != 13 {
		t.Fatalf("fixture drift: x at byte %d, want 13", xByte)
	}

	tests := []struct {
		name    string
		byteOff int
		want    int
	}{
		{"start", 0, 0},
		{"after ascii h", 1, 1},
		{"after é (2 bytes, 1 unit)", 3, 2},
		{"before emoji", 6, 5},
		{"after emoji (4 bytes, 2 units)", 10, 7},
		{"after CJK (3 bytes, 1 unit)", 13, 8},
		{"end of line", len(line), 9},
		{"past end clamps", len(line) + 10, 9},
		{"negative clamps", -1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UTF16Col(line, tt.byteOff); got != tt.want {
				t.Errorf("UTF16Col(%q, %d) = %d, want %d", line, tt.byteOff, got, tt.want)
			}
		})
	}
}

func TestByteCol(t *testing.T) {
	line := "héllo🎉中x"
	tests := []struct {
		name string
		u16  int
		want int
	}{
		{"start", 0, 0},
		{"after h", 1, 1},
		{"after é", 2, 3},
		{"before emoji", 5, 6},
		{"after emoji", 7, 10},
		{"after CJK", 8, 13},
		{"end", 9, len(line)},
		{"past end clamps", 100, len(line)},
		{"negative clamps", -3, 0},
		// A UTF-16 offset landing mid-surrogate-pair resolves to the next rune
		// boundary.
		{"mid surrogate pair", 6, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ByteCol(line, tt.u16); got != tt.want {
				t.Errorf("ByteCol(%q, %d) = %d, want %d", line, tt.u16, got, tt.want)
			}
		})
	}
}

func TestRoundTripASCII(t *testing.T) {
	line := "func Hello() string {"
	for i := 0; i <= len(line); i++ {
		if got := UTF16Col(line, i); got != i {
			t.Fatalf("ASCII UTF16Col(%d) = %d", i, got)
		}
		if got := ByteCol(line, i); got != i {
			t.Fatalf("ASCII ByteCol(%d) = %d", i, got)
		}
	}
}
