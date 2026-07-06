package repro

// runnerevents.go implements the structured-output verdict path: instead of
// scanning free-form stdout/stderr for per-ecosystem "did it run" text
// markers (interpret.go's original approach, still the ONLY path for
// cargo/bazel/JS/unknown ecosystems whose toolchains lack a stable machine-
// readable test-result format), Go and Python repros are steered by the
// harness into emitting a structured record of exactly which tests ran and
// which failed. When that record parses cleanly it is DISPOSITIVE — markers
// are a best-effort heuristic over human-readable text, while `go test -json`
// and a JUnit XML report are the test runner's own ground truth.
//
// The harness (not the reproducer agent) owns this: normalizeCmdForStructuredOutput
// rewrites the agent's plan.Cmd before it reaches the sandbox, and interpret()
// tries the structured path first. Either can fail closed (ok=false) — a
// build failure that never reaches the test runner, a bash -c wrapped command
// the harness declines to rewrite, an absent junitxml file — in which case
// interpret() falls through to the pre-existing marker cascade unchanged.
import (
	"encoding/json"
	"encoding/xml"
	"strings"
)

// structuredJUnitXMLPath is the workspace-relative path the harness asks
// pytest to write its JUnit XML report to, and the same path it later reads
// back via sandbox.Spec.CaptureFiles / sandbox.Result.Captured. Dot-prefixed
// so it reads as tooling scratch, not a repo artifact; validatePlan already
// refuses plans that would overwrite an existing repo file, so collision with
// a real file the agent might otherwise write is not a concern.
const structuredJUnitXMLPath = ".bugbot-repro-junit.xml"

// normalizeCmdForStructuredOutput rewrites a reproducer plan's command to ask
// the underlying test runner for a machine-readable result format, when the
// harness recognizes the command shape well enough to do so safely.
//
//   - `go test` as DIRECT argv (cmd[0]=="go", cmd[1]=="test", case-
//     insensitive): ensures "-json" is present, inserted immediately after
//     "test" so it reads naturally next to the subcommand. A command that
//     already carries -json is returned unchanged.
//   - `pytest` or `python -m pytest` as direct argv: appends
//     "--junitxml=<structuredJUnitXMLPath>" when no --junitxml flag is
//     already present, and returns structuredJUnitXMLPath as the one path the
//     caller must ask the sandbox to capture.
//   - anything else, INCLUDING a bash/sh -c wrapped go test or pytest
//     invocation: returned unchanged. A wrapped command is an opaque shell
//     string to the harness; rewriting it would mean parsing shell syntax,
//     which is out of scope. Those runs keep relying on the marker cascade,
//     exactly as before this feature existed.
//
// The returned captures slice is nil unless a capture file must be read back
// from the sandbox workspace after the run (currently only the pytest case).
func normalizeCmdForStructuredOutput(cmd []string) (normalized []string, captures []string) {
	switch {
	case isDirectGoTest(cmd):
		if hasCmdFlag(cmd, "-json") {
			return cmd, nil
		}
		out := make([]string, 0, len(cmd)+1)
		out = append(out, cmd[0], cmd[1], "-json")
		out = append(out, cmd[2:]...)
		return out, nil
	case isDirectPytest(cmd):
		if hasCmdFlag(cmd, "--junitxml") {
			return cmd, nil
		}
		out := make([]string, len(cmd), len(cmd)+1)
		copy(out, cmd)
		out = append(out, "--junitxml="+structuredJUnitXMLPath)
		return out, []string{structuredJUnitXMLPath}
	default:
		return cmd, nil
	}
}

// isDirectGoTest reports whether argv is an un-wrapped "go test ..." command.
// A bash/sh -c wrapper is intentionally NOT unwrapped here (unlike
// validatePlan's -timeout check): normalizeCmdForStructuredOutput only ever
// rewrites argv it can hand straight back to the sandbox as-is.
func isDirectGoTest(argv []string) bool {
	return len(argv) >= 2 &&
		strings.EqualFold(argv[0], "go") &&
		strings.EqualFold(argv[1], "test")
}

// isDirectPytest reports whether argv is an un-wrapped "pytest ..." or
// "python[3] -m pytest ..." command. See isDirectGoTest for why bash -c
// wrappers are excluded.
func isDirectPytest(argv []string) bool {
	if len(argv) >= 1 && strings.EqualFold(argv[0], "pytest") {
		return true
	}
	return len(argv) >= 3 &&
		(strings.EqualFold(argv[0], "python") || strings.EqualFold(argv[0], "python3")) &&
		argv[1] == "-m" &&
		strings.EqualFold(argv[2], "pytest")
}

// goTestEvent is one line of `go test -json` output (the stdlib's
// cmd/internal/test2json record, stable across Go releases). Only the fields
// classifyGoEvents needs are decoded; Output is kept for potential future use
// but is not currently inspected (the Action/Test/Package triple is
// sufficient ran/failed evidence and avoids re-deriving the marker scan we're
// trying to replace).
type goTestEvent struct {
	Time    string  `json:"Time"`
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// parseGoTestEvents decodes stdout as line-delimited `go test -json` events.
// ok is false when not a single line parses as JSON — the signal that -json
// was not actually in effect (e.g. the plan wrapped the command in bash -c,
// which normalizeCmdForStructuredOutput deliberately leaves untouched) or the
// runner never started (a shell/toolchain error printed a plain-text message
// to stdout instead). Individual lines that fail to decode are skipped rather
// than aborting the whole parse: a truncated capture (sandbox's max-output-
// bytes cap) can leave a partial trailing line, and the events before it are
// still good evidence.
func parseGoTestEvents(stdout string) (events []goTestEvent, ok bool) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, len(events) > 0
}

// classifyGoEvents applies positive ran-evidence rules to a decoded
// `go test -json` stream. It is called only for a non-zero, non-timeout exit
// (interpret() has already handled exit 0 and the 125/126/127 environment
// gate), so exitCode here is always some other failure code.
//
//   - Any per-test "fail" action (Test non-empty) is dispositive: a specific
//     test ran and failed. This is the structured equivalent of the
//     marker-based "positive ran-evidence" gate, minus the string matching.
//   - A package-level "fail" (Test empty) with no test ever having "run" means
//     the package never got far enough to execute a test — go's own build
//     step failed (surfaced as build-output/build-fail JSON actions on
//     Go 1.21+, or a bare package fail on older toolchains) — build_error.
//   - A package-level "fail" where at least one test DID "run" but none has a
//     recorded per-test "fail" means the test binary aborted (panic, fatal
//     signal) after starting — we have ran-evidence but no confirmed failing
//     test, so we refuse to guess which test demonstrated the bug:
//     not_demonstrated.
//   - Anything else (no fail action decoded at all, e.g. a truncated capture
//     cut off before the terminal event) is NOT dispositive: ok=false so the
//     caller falls back to the marker cascade.
func classifyGoEvents(events []goTestEvent, exitCode int) (demonstrated bool, reason VerdictReason, ok bool) {
	var testRan, testFailed, packageFailed bool
	for _, e := range events {
		switch {
		case e.Action == "run" && e.Test != "":
			testRan = true
		case e.Action == "fail" && e.Test != "":
			testFailed = true
		case e.Action == "fail" && e.Test == "":
			packageFailed = true
		}
	}
	switch {
	case testFailed:
		return true, "", true
	case packageFailed && testRan:
		return false, VerdictReasonNotDemonstrated, true
	case packageFailed && !testRan:
		return false, VerdictReasonBuildError, true
	default:
		return false, "", false
	}
}

// junitTestcase is one <testcase> element from a JUnit XML report. It is
// matched generically against BOTH possible document shapes pytest emits
// (see junitDoc): pytest's <failure>/<error> children carry the assertion
// traceback and the collection-error banner respectively.
type junitTestcase struct {
	Classname string       `xml:"classname,attr"`
	Name      string       `xml:"name,attr"`
	Failure   *junitDetail `xml:"failure"`
	Error     *junitDetail `xml:"error"`
}

// junitDetail is the <failure>/<error> element body: pytest sets the
// "message" attribute to a short banner ("collection failure" for a module
// that failed to import) and puts the full traceback in the element text.
type junitDetail struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// junitTestsuite is a <testsuite> element, present when the document root is
// the pytest-modern <testsuites> wrapper.
type junitTestsuite struct {
	Testcases []junitTestcase `xml:"testcase"`
}

// junitDoc matches either JUnit XML shape pytest can produce depending on
// version: a <testsuites> root wrapping one or more <testsuite> elements
// (modern pytest), or a bare <testsuite> root whose <testcase> children are
// this struct's own direct children (older pytest). encoding/xml matches
// child elements by tag regardless of what the actual root element is named,
// so declaring both tags on one struct handles both shapes without a two-pass
// parse: whichever shape is present, the corresponding field populates and
// the other stays empty.
type junitDoc struct {
	Testsuites []junitTestsuite `xml:"testsuite"`
	Testcases  []junitTestcase  `xml:"testcase"`
}

// allTestcases flattens junitDoc's two possible shapes into one list.
func (d junitDoc) allTestcases() []junitTestcase {
	cases := d.Testcases
	for _, ts := range d.Testsuites {
		cases = append(cases, ts.Testcases...)
	}
	return cases
}

// isCollectionError reports whether tc's <error> represents pytest failing to
// even collect the test (a broken import, a syntax error in the test module)
// rather than a test that ran and errored. pytest's own convention is to set
// the error's message attribute to the literal string "collection failure"
// (bugbot-ym09 fixture verified against a real pytest 9.x run); that string
// is checked case-insensitively against both the message and the body text in
// case a future pytest version moves the banner into the traceback instead of
// the attribute.
func isCollectionError(tc junitTestcase) bool {
	if tc.Error == nil {
		return false
	}
	haystack := strings.ToLower(tc.Error.Message + " " + tc.Error.Text)
	return strings.Contains(haystack, "collection failure") || strings.Contains(haystack, "collecting")
}

// parseJUnitXML decodes a JUnit XML report (as captured from
// structuredJUnitXMLPath) and applies the same positive-ran-evidence
// discipline as classifyGoEvents. Called only for a non-zero, non-timeout
// exit, mirroring classifyGoEvents.
//
//   - Any <testcase><failure> is dispositive ran-and-failed evidence: pytest
//     only emits <failure> for a test that was collected, executed, and
//     asserted incorrectly — demonstrated.
//   - Any <testcase><error> that isCollectionError identifies is the
//     false-reproduction guard's structured equivalent of build_error: the
//     module never finished importing, so no test in it ever ran.
//   - Anything else — no data (a nil/empty byte slice, e.g. the file was
//     never written because the run crashed before pytest could produce a
//     report), malformed XML, or a report with no failure/collection-error
//     evidence — is NOT dispositive: ok=false so the caller falls back to the
//     marker cascade.
func parseJUnitXML(data []byte) (demonstrated bool, reason VerdictReason, ok bool) {
	if len(data) == 0 {
		return false, "", false
	}
	var doc junitDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return false, "", false
	}
	cases := doc.allTestcases()
	if len(cases) == 0 {
		return false, "", false
	}
	var sawFailure, sawCollectionError bool
	for _, tc := range cases {
		if tc.Failure != nil {
			sawFailure = true
		}
		if isCollectionError(tc) {
			sawCollectionError = true
		}
	}
	switch {
	case sawFailure:
		return true, "", true
	case sawCollectionError:
		return false, VerdictReasonBuildError, true
	default:
		return false, "", false
	}
}
