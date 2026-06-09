package repro

import (
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// verdict is the interpretation of a single sandbox run against the
// reproduction contract.
type verdict struct {
	// demonstrated is true only when the run is a genuine demonstration of the
	// bug: a non-zero exit that is NOT explained away by a compile error,
	// missing dependency, timeout, or other infrastructure failure.
	demonstrated bool
	// reason is a short category for a non-demonstration (for Summary/display).
	reason string
	// summary is a short human-readable digest of the run's output.
	summary string
}

// interpret applies the Tier-1 promotion rules to a sandbox result.
//
// Rules:
//   - Zero exit: the repro did not fail, so it did not demonstrate the bug.
//   - Timeout: an infrastructure/quality problem, not a demonstration.
//   - Non-zero exit that looks like a build/compile failure or missing
//     dependency: NOT a demonstration (false reproduction guard).
//   - Otherwise a non-zero exit is treated as a genuine demonstration.
func interpret(res sandbox.Result) verdict {
	out := combinedOutput(res)

	switch {
	case res.TimedOut:
		return verdict{reason: "timeout", summary: trunc(out, 400)}
	case res.ExitCode == 0:
		return verdict{reason: "exit_zero", summary: trunc(out, 400)}
	case looksLikeBuildFailure(out):
		return verdict{reason: "build_error", summary: trunc(out, 400)}
	default:
		return verdict{demonstrated: true, summary: trunc(out, 400)}
	}
}

// feedback builds the corrective message sent back to the agent after a
// non-demonstrating attempt, tailored to the verdict's category and including
// the offending plan's command and the run output the agent must fix.
func (v verdict) feedback(p *Plan) string {
	var b strings.Builder
	switch v.reason {
	case "exit_zero":
		b.WriteString("Your repro ran but exited 0, so it did NOT demonstrate the bug. ")
		b.WriteString("The test must FAIL on the current buggy code. ")
		b.WriteString("Make the assertion check the CORRECT expected behavior so the bug makes it fail.")
	case "build_error":
		b.WriteString("Your repro failed to BUILD (compile error or missing dependency). ")
		b.WriteString("A build failure is NOT a reproduction. Fix the test so it compiles using only ")
		b.WriteString("the standard library and packages the repository already imports.")
	case "timeout":
		b.WriteString("Your repro timed out. Make it a fast, minimal test that returns quickly.")
	default:
		b.WriteString("Your repro did not demonstrate the bug as expected. Revise it.")
	}
	if len(p.Cmd) > 0 {
		fmt.Fprintf(&b, "\n\nCommand run: %s", strings.Join(p.Cmd, " "))
	}
	if v.summary != "" {
		fmt.Fprintf(&b, "\n\nOutput was:\n%s", v.summary)
	}
	return b.String()
}

// buildFailureMarkers are substrings (lowercased) that indicate a Go build or
// dependency-resolution failure rather than a test assertion failure. Go's
// toolchain emits "build failed" / "[build failed]" and "# command-line" style
// compile errors; missing-dependency and import errors are caught too.
var buildFailureMarkers = []string{
	"build failed",
	"[build failed]",
	"cannot find package",
	"undefined:",
	"undeclared name",
	"no required module provides package",
	"missing go.sum entry",
	"cannot find module",
	"expected declaration",
	"syntax error",
	"is not in std",
	"go: updates to go.mod needed",
	"go: downloading",
	": cannot use ",
	"too many errors",
}

// looksLikeBuildFailure reports whether the combined output indicates a
// compile/dependency failure rather than a genuine test failure. This is the
// false-reproduction guard: a repro that never compiled has not demonstrated
// anything.
func looksLikeBuildFailure(out string) bool {
	low := strings.ToLower(out)
	for _, m := range buildFailureMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// combinedOutput joins stderr and stdout for interpretation. Build errors land
// on stderr; assertion output (testing.T) lands on stdout. We scan both.
func combinedOutput(res sandbox.Result) string {
	var b strings.Builder
	if res.Stderr != "" {
		b.WriteString(res.Stderr)
	}
	if res.Stdout != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(res.Stdout)
	}
	return b.String()
}

// trunc shortens s to at most n bytes, appending an ellipsis marker when cut.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [output truncated]"
}
