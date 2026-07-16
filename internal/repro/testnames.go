package repro

import (
	"regexp"
	"strings"
)

// testnames.go implements best-effort extraction of declared test
// identifiers from a repro plan's injected files (bugbot-u47n): tying
// structured-output ran-evidence (classifyGoEvents, parseJUnitXML) to the
// SPECIFIC test the agent wrote, instead of accepting evidence from ANY
// failing test in the targeted package/module — including an unrelated
// pre-existing failure that has nothing to do with the injected repro.
//
// Every extractor here is a DUMB REGEX SCAN over the raw file text, not a
// parser: no AST, no build-tag resolution, no comment/string-literal
// stripping. A commented-out `// func TestFoo(t *testing.T) {` still
// "declares" TestFoo by this scan. That is acceptable because these names
// are used only as a POSITIVE allowlist for evidence binding
// (bindTestEvidence) and a -run enforcement hint (validateReproCmd): an
// over-broad extraction can at worst let a foreign failure through — the
// pre-existing, unchanged behavior for that case — or accept a looser -run
// pattern than strictly necessary; it can never cause a legitimate
// promotion to be falsely rejected by under-extraction, because every
// caller already treats "no names extractable" as "leave behavior
// unchanged". This is evidence binding, not syntax validation — never use
// these extractors to reject a plan for containing an invalid test name.
var (
	goTestNameRe  = regexp.MustCompile(`func\s+(Test[A-Za-z0-9_]*|Fuzz[A-Za-z0-9_]*|Benchmark[A-Za-z0-9_]*)\s*\(`)
	pyTestFuncRe  = regexp.MustCompile(`def\s+(test_\w+)\s*\(`)
	pyTestClassRe = regexp.MustCompile(`class\s+(Test\w+)\b`)
	jsTestCallRe  = regexp.MustCompile("\\b(?:test|it)\\(\\s*['\"`](.+?)['\"`]")
)

// extractGoTestNames scans every *_test.go file in files for declared
// func TestXxx / FuzzXxx / BenchmarkXxx identifiers. Non-Go files are
// ignored. Returns nil (not an error) when no *_test.go file is present or
// none declares a matching func — callers treat that as "no names
// extractable", not "zero tests written".
func extractGoTestNames(files map[string]string) []string {
	var names []string
	for path, content := range files {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, m := range goTestNameRe.FindAllStringSubmatch(content, -1) {
			names = append(names, m[1])
		}
	}
	return names
}

// extractPyTestNames scans every .py file in files for declared
// def test_xxx() functions (pytest-style) and class TestXxx (unittest-style
// suite class; the individual test_ methods inside it are picked up by the
// def scan above).
func extractPyTestNames(files map[string]string) []string {
	var names []string
	for path, content := range files {
		if !strings.HasSuffix(path, ".py") {
			continue
		}
		for _, m := range pyTestFuncRe.FindAllStringSubmatch(content, -1) {
			names = append(names, m[1])
		}
		for _, m := range pyTestClassRe.FindAllStringSubmatch(content, -1) {
			names = append(names, m[1])
		}
	}
	return names
}

// extractJSTestNames scans JS/TS test files for test('name', ...) /
// it('name', ...) call names — Jest, Vitest, Mocha, and node:test all share
// this call shape. There is currently no structured-output path for JS
// (interpret()'s switch only handles Go and Python), so this extractor is
// not yet consumed by bindTestEvidence; it exists for feature completeness
// and so a future JS structured-output path (were one added) would not need
// a fresh extraction pass.
func extractJSTestNames(files map[string]string) []string {
	var names []string
	for path, content := range files {
		if !hasJSExt(path) {
			continue
		}
		for _, m := range jsTestCallRe.FindAllStringSubmatch(content, -1) {
			names = append(names, m[1])
		}
	}
	return names
}

// hasJSExt reports whether path has a JS/TS source extension.
func hasJSExt(path string) bool {
	for _, ext := range []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}
