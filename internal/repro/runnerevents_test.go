package repro

import (
	"reflect"
	"testing"
)

// TestNormalizeCmdForStructuredOutput covers the harness-dictated rewrite:
// a direct `go test` or pytest argv gains the structured-output flag it is
// missing, an argv that already carries the flag is untouched, a bash -c
// wrapped invocation is left alone entirely (rewriting shell syntax is out of
// scope), and a non-test command passes through unchanged.
func TestNormalizeCmdForStructuredOutput(t *testing.T) {
	cases := []struct {
		name         string
		cmd          []string
		wantCmd      []string
		wantCaptures []string
	}{
		{
			name:         "go test without -json gains it after test",
			cmd:          []string{"go", "test", "-timeout", "60s", "-run", "TestBug", "./..."},
			wantCmd:      []string{"go", "test", "-json", "-timeout", "60s", "-run", "TestBug", "./..."},
			wantCaptures: nil,
		},
		{
			name:         "go test already has -json is untouched",
			cmd:          []string{"go", "test", "-json", "-run", "TestBug", "./..."},
			wantCmd:      []string{"go", "test", "-json", "-run", "TestBug", "./..."},
			wantCaptures: nil,
		},
		{
			name:         "go test with -json=true form is untouched",
			cmd:          []string{"go", "test", "-json=true", "./..."},
			wantCmd:      []string{"go", "test", "-json=true", "./..."},
			wantCaptures: nil,
		},
		{
			name:         "uppercase Go Test still matches",
			cmd:          []string{"Go", "Test", "./..."},
			wantCmd:      []string{"Go", "Test", "-json", "./..."},
			wantCaptures: nil,
		},
		{
			name:         "bash -c wrapped go test is left alone",
			cmd:          []string{"bash", "-c", "go test -run TestBug ./..."},
			wantCmd:      []string{"bash", "-c", "go test -run TestBug ./..."},
			wantCaptures: nil,
		},
		{
			name:         "sh -c wrapped pytest is left alone",
			cmd:          []string{"sh", "-c", "pytest -k test_bug"},
			wantCmd:      []string{"sh", "-c", "pytest -k test_bug"},
			wantCaptures: nil,
		},
		{
			name:         "pytest without --junitxml gains it",
			cmd:          []string{"pytest", "-k", "test_bug", "test_bug.py"},
			wantCmd:      []string{"pytest", "-k", "test_bug", "test_bug.py", "--junitxml=" + structuredJUnitXMLPath},
			wantCaptures: []string{structuredJUnitXMLPath},
		},
		{
			name:         "python -m pytest without --junitxml gains it",
			cmd:          []string{"python", "-m", "pytest", "test_bug.py"},
			wantCmd:      []string{"python", "-m", "pytest", "test_bug.py", "--junitxml=" + structuredJUnitXMLPath},
			wantCaptures: []string{structuredJUnitXMLPath},
		},
		{
			name:         "python3 -m pytest without --junitxml gains it",
			cmd:          []string{"python3", "-m", "pytest", "test_bug.py"},
			wantCmd:      []string{"python3", "-m", "pytest", "test_bug.py", "--junitxml=" + structuredJUnitXMLPath},
			wantCaptures: []string{structuredJUnitXMLPath},
		},
		{
			name:         "pytest with --junitxml already present is untouched",
			cmd:          []string{"pytest", "--junitxml=custom.xml", "test_bug.py"},
			wantCmd:      []string{"pytest", "--junitxml=custom.xml", "test_bug.py"},
			wantCaptures: nil,
		},
		{
			name:         "pytest with bare --junitxml flag form is untouched",
			cmd:          []string{"pytest", "--junitxml", "custom.xml", "test_bug.py"},
			wantCmd:      []string{"pytest", "--junitxml", "custom.xml", "test_bug.py"},
			wantCaptures: nil,
		},
		{
			name:         "non-test command untouched",
			cmd:          []string{"cargo", "test"},
			wantCmd:      []string{"cargo", "test"},
			wantCaptures: nil,
		},
		{
			name:         "empty cmd untouched",
			cmd:          nil,
			wantCmd:      nil,
			wantCaptures: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCmd, gotCaptures := normalizeCmdForStructuredOutput(tc.cmd)
			if !reflect.DeepEqual(gotCmd, tc.wantCmd) {
				t.Errorf("cmd = %v, want %v", gotCmd, tc.wantCmd)
			}
			if !reflect.DeepEqual(gotCaptures, tc.wantCaptures) {
				t.Errorf("captures = %v, want %v", gotCaptures, tc.wantCaptures)
			}
		})
	}
}

// realGoTestJSONFailing is trimmed, real `go test -json ./...` output captured
// from a scratch module with one failing and one passing test (go1.25,
// generated for bugbot-ym09 — see the PR description for the exact repro
// steps). It exercises the demonstrated path: a per-test "fail" action.
const realGoTestJSONFailing = `{"Time":"2026-07-06T15:23:59.690643746-06:00","Action":"start","Package":"fixture"}
{"Time":"2026-07-06T15:23:59.692244457-06:00","Action":"run","Package":"fixture","Test":"TestAddsWrong"}
{"Time":"2026-07-06T15:23:59.692262143-06:00","Action":"output","Package":"fixture","Test":"TestAddsWrong","Output":"=== RUN   TestAddsWrong\n"}
{"Time":"2026-07-06T15:23:59.692288751-06:00","Action":"output","Package":"fixture","Test":"TestAddsWrong","Output":"    fixture_test.go:9: got 2, want 3\n"}
{"Time":"2026-07-06T15:23:59.69229582-06:00","Action":"output","Package":"fixture","Test":"TestAddsWrong","Output":"--- FAIL: TestAddsWrong (0.00s)\n"}
{"Time":"2026-07-06T15:23:59.692299151-06:00","Action":"fail","Package":"fixture","Test":"TestAddsWrong","Elapsed":0}
{"Time":"2026-07-06T15:23:59.692303734-06:00","Action":"run","Package":"fixture","Test":"TestPasses"}
{"Time":"2026-07-06T15:23:59.692305755-06:00","Action":"output","Package":"fixture","Test":"TestPasses","Output":"=== RUN   TestPasses\n"}
{"Time":"2026-07-06T15:23:59.692308511-06:00","Action":"output","Package":"fixture","Test":"TestPasses","Output":"--- PASS: TestPasses (0.00s)\n"}
{"Time":"2026-07-06T15:23:59.692311964-06:00","Action":"pass","Package":"fixture","Test":"TestPasses","Elapsed":0}
{"Time":"2026-07-06T15:23:59.692314293-06:00","Action":"output","Package":"fixture","Output":"FAIL\n"}
{"Time":"2026-07-06T15:23:59.69252347-06:00","Action":"output","Package":"fixture","Output":"FAIL\tfixture\t0.002s\n"}
{"Time":"2026-07-06T15:23:59.692533199-06:00","Action":"fail","Package":"fixture","Elapsed":0.002}
`

// realGoTestJSONBuildFail is real `go test -json ./...` output (go1.25) for a
// package that fails to COMPILE: the JSON stream is well-formed (build
// failures are themselves reported as build-output/build-fail JSON actions on
// modern Go), but no "run" action for any test ever appears — the package
// never got that far.
const realGoTestJSONBuildFail = `{"ImportPath":"fixture2 [fixture2.test]","Action":"build-output","Output":"# fixture2 [fixture2.test]\n"}
{"ImportPath":"fixture2 [fixture2.test]","Action":"build-output","Output":"./fixture_test.go:6:2: undefined: undefinedFunc\n"}
{"ImportPath":"fixture2 [fixture2.test]","Action":"build-fail"}
{"Time":"2026-07-06T15:24:03.809994606-06:00","Action":"start","Package":"fixture2"}
{"Time":"2026-07-06T15:24:03.810029532-06:00","Action":"output","Package":"fixture2","Output":"FAIL\tfixture2 [build failed]\n"}
{"Time":"2026-07-06T15:24:03.810035409-06:00","Action":"fail","Package":"fixture2","Elapsed":0,"FailedBuild":"fixture2 [fixture2.test]"}
`

// realGoTestJSONFatalError is real, trimmed `go test -json ./...` output
// (go1.25.10) for a test that triggers "fatal error: concurrent map
// writes": the Go runtime terminates the whole process immediately,
// bypassing recover(), so the stream has a "run" for the test that was
// executing but no per-test "fail" — only the package-level one. This is
// the real-world shape classifyGoEvents must NOT treat as dispositive (see
// TestClassifyGoEvents): it is a genuine crash, and Go's "fatal error:"
// ran-marker in interpret.go's fallback cascade is what promotes it.
const realGoTestJSONFatalError = `{"Time":"2026-07-06T16:00:16.81793128-06:00","Action":"start","Package":"fixture4"}
{"Time":"2026-07-06T16:00:16.819367559-06:00","Action":"run","Package":"fixture4","Test":"TestConcurrentMapWrite"}
{"Time":"2026-07-06T16:00:16.819382231-06:00","Action":"output","Package":"fixture4","Test":"TestConcurrentMapWrite","Output":"=== RUN   TestConcurrentMapWrite\n"}
{"Time":"2026-07-06T16:00:16.819396107-06:00","Action":"output","Package":"fixture4","Test":"TestConcurrentMapWrite","Output":"fatal error: concurrent map writes\n"}
{"Time":"2026-07-06T16:00:16.82254106-06:00","Action":"output","Package":"fixture4","Test":"TestConcurrentMapWrite","Output":"\n"}
{"Time":"2026-07-06T16:00:16.82254945-06:00","Action":"output","Package":"fixture4","Test":"TestConcurrentMapWrite","Output":"goroutine 10 [running]:\n"}
{"Time":"2026-07-06T16:00:16.822806848-06:00","Action":"output","Package":"fixture4","Output":"FAIL\tfixture4\t0.005s\n"}
{"Time":"2026-07-06T16:00:16.822822711-06:00","Action":"fail","Package":"fixture4","Elapsed":0.005}
`

func TestParseGoTestEvents(t *testing.T) {
	events, ok := parseGoTestEvents(realGoTestJSONFailing)
	if !ok {
		t.Fatal("parseGoTestEvents ok = false, want true for well-formed -json output")
	}
	if len(events) != 13 {
		t.Fatalf("len(events) = %d, want 13", len(events))
	}

	// Plain-text (non-JSON) stdout, as produced by a bash -c wrapped `go test`
	// that normalizeCmdForStructuredOutput declined to rewrite: zero lines
	// parse, so the caller must fall back to markers.
	_, ok = parseGoTestEvents("FAIL\tfixture2 [build failed]\nFAIL\n")
	if ok {
		t.Error("parseGoTestEvents ok = true for plain-text output, want false")
	}

	_, ok = parseGoTestEvents("")
	if ok {
		t.Error("parseGoTestEvents ok = true for empty stdout, want false")
	}
}

func TestClassifyGoEvents(t *testing.T) {
	cases := []struct {
		name             string
		stdout           string
		exitCode         int
		wantDemonstrated bool
		wantReason       VerdictReason
		wantOK           bool
	}{
		{
			name:             "real failing test JSON demonstrates",
			stdout:           realGoTestJSONFailing,
			exitCode:         1,
			wantDemonstrated: true,
			wantOK:           true,
		},
		{
			name:       "real build-fail JSON with no run action is build_error",
			stdout:     realGoTestJSONBuildFail,
			exitCode:   1,
			wantReason: VerdictReasonBuildError,
			wantOK:     true,
		},
		{
			name: "package fail after a test ran but no per-test fail is not dispositive",
			stdout: `{"Action":"run","Package":"p","Test":"TestX"}
{"Action":"fail","Package":"p"}
`,
			exitCode: 2,
			wantOK:   false,
		},
		{
			// Real `go test -json` output (go1.25.10) for a "fatal error:
			// concurrent map writes" — the Go runtime terminates the whole
			// process immediately, bypassing recover(), so the stream has a
			// "run" for the test that was executing but no per-test "fail"
			// event, only the package-level one. This is a genuine crash
			// demonstration, not a build/collection failure: classifyGoEvents
			// must NOT return a confident verdict here (ok=false), so
			// interpret()'s marker fallback (Go's "fatal error:" ran-marker)
			// is the one that promotes it.
			name:     "real fatal runtime error (concurrent map write) is not dispositive",
			stdout:   realGoTestJSONFatalError,
			exitCode: 2,
			wantOK:   false,
		},
		{
			name: "no fail action at all is not dispositive",
			stdout: `{"Action":"run","Package":"p","Test":"TestX"}
{"Action":"pass","Package":"p","Test":"TestX"}
`,
			exitCode: 1,
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, ok := parseGoTestEvents(tc.stdout)
			if !ok {
				t.Fatalf("parseGoTestEvents ok = false for %q", tc.stdout)
			}
			demonstrated, reason, ok := classifyGoEvents(events)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if demonstrated != tc.wantDemonstrated {
				t.Errorf("demonstrated = %v, want %v", demonstrated, tc.wantDemonstrated)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// realJUnitFailing is real `pytest --junitxml=...` output (pytest 9.1.1) for
// a single failing assertion test.
const realJUnitFailing = `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="0" failures="1" skipped="0" tests="1" time="0.016" timestamp="2026-07-06T15:25:39.396653-06:00" hostname="oerlikon"><testcase classname="test_fail" name="test_addition" time="0.000"><failure message="assert (1 + 1) == 3">def test_addition():
&gt;       assert 1 + 1 == 3
E       assert (1 + 1) == 3

test_fail.py:2: AssertionError</failure></testcase></testsuite></testsuites>`

// realJUnitCollectionError is real pytest output for a test module that
// fails to import (a broken dependency), so pytest never collects — let
// alone runs — the test function inside it.
const realJUnitCollectionError = `<?xml version="1.0" encoding="utf-8"?><testsuites name="pytest tests"><testsuite name="pytest" errors="1" failures="0" skipped="0" tests="1" time="0.076" timestamp="2026-07-06T15:25:42.791708-06:00" hostname="oerlikon"><testcase classname="" name="test_broken_import" time="0.000"><error message="collection failure">ImportError while importing test module '/tmp/tmp.vDKOBzZy7U/test_broken_import.py'.
Hint: make sure your test modules/packages have valid Python names.
Traceback:
/run/current-system/sw/lib/python3.13/importlib/__init__.py:88: in import_module
    return _bootstrap._gcd_import(name[level:], package, level)
           ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
test_broken_import.py:1: in &lt;module&gt;
    import nonexistent_module_xyz
E   ModuleNotFoundError: No module named 'nonexistent_module_xyz'</error></testcase></testsuite></testsuites>`

func TestParseJUnitXML(t *testing.T) {
	cases := []struct {
		name             string
		data             []byte
		wantDemonstrated bool
		wantReason       VerdictReason
		wantOK           bool
	}{
		{
			name:             "real failing testcase demonstrates",
			data:             []byte(realJUnitFailing),
			wantDemonstrated: true,
			wantOK:           true,
		},
		{
			name:       "real collection error is build_error",
			data:       []byte(realJUnitCollectionError),
			wantReason: VerdictReasonBuildError,
			wantOK:     true,
		},
		{
			name:             "bare testsuite root (no testsuites wrapper) with a failure",
			data:             []byte(`<testsuite name="pytest"><testcase classname="t" name="test_x"><failure message="boom">boom</failure></testcase></testsuite>`),
			wantDemonstrated: true,
			wantOK:           true,
		},
		{
			name:   "absent file falls back to markers",
			data:   nil,
			wantOK: false,
		},
		{
			name:   "empty bytes falls back to markers",
			data:   []byte{},
			wantOK: false,
		},
		{
			name:   "unparseable garbage falls back to markers",
			data:   []byte("not xml at all"),
			wantOK: false,
		},
		{
			name:   "well-formed XML with a passing testcase only is not dispositive",
			data:   []byte(`<testsuites><testsuite><testcase classname="t" name="test_x" time="0.001"></testcase></testsuite></testsuites>`),
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			demonstrated, reason, ok := parseJUnitXML(tc.data)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if demonstrated != tc.wantDemonstrated {
				t.Errorf("demonstrated = %v, want %v", demonstrated, tc.wantDemonstrated)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
