package agent

import (
	"fmt"
	"os"
	"strings"
)

// navMaxFileBytes is the shared ceiling for files loaded by code-navigation and
// grep tools. Files larger than this are skipped: they are likely generated,
// minified, or binary assets that yield noisy or memory-hostile results.
const navMaxFileBytes = 5 * 1024 * 1024

// readFileLines loads a file's lines, bounded by navMaxFileBytes. Errors and
// oversized files yield nil; callers treat nil as "best-effort unavailable"
// since snippets are decorative rather than authoritative.
func readFileLines(path string) []string {
	info, err := os.Stat(path)
	if err != nil || info.Size() > navMaxFileBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

// readLine returns the trimmed text of the 1-based line in the file at abs.
// It enforces size and bounds checks that produce model-actionable errors so
// the model can correct its arguments rather than seeing a raw OS error.
func readLine(abs string, line int) (string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file")
	}
	if info.Size() > navMaxFileBytes {
		return "", fmt.Errorf("file is too large for code navigation (%d bytes)", info.Size())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if line > len(lines) {
		return "", fmt.Errorf("line %d is past the end of the file (%d lines)", line, len(lines))
	}
	return strings.TrimRight(lines[line-1], "\r"), nil
}

// symbolColumn locates symbol on lineText and returns the byte offset of the
// identifier the LSP query should target. A dotted symbol ("pkg.Hello",
// "recv.method") matches as written but the returned offset points at its
// final segment, since LSP positions must land inside a single identifier
// token. When the full symbol is absent, the final segment alone is tried so
// models that over-qualify still succeed.
func symbolColumn(lineText, symbol string) (int, error) {
	symbol = strings.TrimSpace(symbol)
	symbol = strings.TrimSuffix(symbol, "()")

	if off, ok := findIdentifier(lineText, symbol); ok {
		// Point inside the last identifier segment of a qualified name.
		if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
			return off + i + 1, nil
		}
		return off, nil
	}
	if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
		if off, ok := findIdentifier(lineText, symbol[i+1:]); ok {
			return off, nil
		}
	}
	return 0, fmt.Errorf("symbol %q not found on the line", symbol)
}
