package agenttools

import (
	"fmt"
	"strings"
)

// toolError prefixes a tool failure so the model recognizes it as a recoverable
// error rather than a normal result. The harness uses this for every error a
// Tool returns.
func toolError(err error) string {
	return "ERROR: " + err.Error()
}

// requireField returns an error if val (trimmed) is empty. name is the
// human-readable field name that appears in the error message.
func requireField(name, val string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

// requireLineNumber returns an error if n is less than 1. The error message
// follows the convention established by the existing tools ("line must be a
// 1-based line number").
func requireLineNumber(n int) error {
	if n < 1 {
		return fmt.Errorf("line must be a 1-based line number")
	}
	return nil
}
