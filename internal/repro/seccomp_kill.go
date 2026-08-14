package repro

import (
	"strings"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// seccompArchKillExitCode is 128 + SIGSYS (31 on Linux): the exit code a
// sandbox backend's own process supervision (bwrap, acting as its
// sandbox's pid-1 reaper) reports when the DIRECT command it launched was
// killed by the seccomp architecture-mismatch guard (bugbot-6dph): a
// syscall issued under a non-native CPU architecture — a 32-bit/compat
// binary, the x32 ABI, or a 64-bit process using the legacy int-0x80
// 32-bit syscall entry point. Measured directly against a real kill: the
// process never gets to write any output before dying, so no text
// signature is available for THIS shape — the exit code alone is the
// signature. Deliberately narrow: no ordinary build tool or test-runner
// convention uses 159 as an exit code.
const seccompArchKillExitCode = 159

// seccompArchKillTextMarker is Go's os/exec.ProcessState.String() text for
// a process killed by SIGSYS ("signal: bad system call", optionally
// followed by "(core dumped)") — the signature left behind when a NESTED
// subprocess (not the sandboxed COMMAND bugbot itself launched) is the one
// killed, e.g. `go test` forking a helper that shells out to a 32-bit
// binary. The outer runner keeps running, reports the child's death as
// ordinary text, and exits with ITS OWN code — which is why
// seccompArchKillExitCode alone cannot catch this shape: the runner's own
// exit code is unrelated to 159.
const seccompArchKillTextMarker = "bad system call"

// seccompArchKillSummary is the operator/agent-facing explanation attached
// to a vetoed verdict, naming the mechanism so a killed harness process
// never reads as an unexplained "the finding did not reproduce."
const seccompArchKillSummary = "the sandbox's seccomp filter killed a process for using a non-native CPU instruction path (a 32-bit/compat binary, the x32 ABI, or a legacy 32-bit syscall entry point invoked from a 64-bit process); this is a sandbox-harness limitation, not a bug reproduction (bugbot-6dph)"

// seccompArchKilled reports whether res looks like a bwrap seccomp
// architecture-mismatch kill rather than a genuine command outcome, from
// either of the two independent, unambiguous signatures a real kill
// leaves (see the two constants above — both measured directly against a
// live bwrap seccomp kill, not inferred).
//
// EVERY classifier that turns a sandbox.Result into a verdict (interpret,
// patchVerdict, classifySmoke, classifyPlaybookProbe) MUST check this at
// the SAME precedence as res.InfraKilled() — before any exit-code switch,
// structured-output parse (go test -json / JUnit XML), or marker cascade —
// so a seccomp-killed helper process can never be read as "the command
// completed and its output says X" (mirrors res.InfraKilled()'s own doc
// contract exactly, for a kill this package detects heuristically instead
// of one sandbox.Exec recorded as a direct fact: unlike TimedOut/
// WorkspaceQuotaExceeded, Exec has no reliable way to know IN ADVANCE that
// a NESTED subprocess kill is coming, so this lives in repro's existing
// marker-heuristic apparatus rather than as a new sandbox.Result field).
//
// Placing this ahead of interpret()'s structured-output path (not merely
// at the same tier as its 0a/0b sanitizer markers, which sit AFTER that
// path) is deliberate: a nested kill inside `go test` reports as an
// ordinary per-test FAILURE event in `go test -json` output (the parent
// process sees a normal Go error from its own subprocess call and reports
// it via t.Fatalf, indistinguishable from a real assertion failure to the
// JSON decoder), so a check placed only at the 0a/0b tier would already be
// bypassed by that structured path returning demonstrated=true first.
func seccompArchKilled(res sandbox.Result, out string) bool {
	return res.ExitCode == seccompArchKillExitCode || strings.Contains(strings.ToLower(out), seccompArchKillTextMarker)
}
