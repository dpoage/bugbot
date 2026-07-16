package repro

import (
	"reflect"
	"sort"
	"testing"
)

// sortedNames is a small helper so extraction-order (map iteration) never
// makes these tests flaky.
func sortedNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

func TestExtractGoTestNames(t *testing.T) {
	files := map[string]string{
		"bug_test.go": "package p\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\nfunc helper() {}\nfunc BenchmarkBar(b *testing.B) {}\nfunc FuzzBaz(f *testing.F) {}\n",
		"helper.go":   "package p\n\nfunc TestLooksLikeATestButIsntATestFile() {}\n", // not *_test.go: ignored
		"README.md":   "func TestNotGoCode() {}\n",                                   // not *_test.go: ignored
	}
	got := sortedNames(extractGoTestNames(files))
	want := []string{"BenchmarkBar", "FuzzBaz", "TestFoo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractGoTestNames = %v, want %v", got, want)
	}
}

func TestExtractGoTestNames_NoDeclarations(t *testing.T) {
	files := map[string]string{"bug_test.go": "package p\n"}
	if got := extractGoTestNames(files); got != nil {
		t.Errorf("extractGoTestNames = %v, want nil for a file with no func Test/Fuzz/Benchmark", got)
	}
}

func TestExtractPyTestNames(t *testing.T) {
	files := map[string]string{
		"test_bug.py": "import unittest\n\ndef test_foo():\n    assert False\n\nclass TestSuite(unittest.TestCase):\n    def test_bar(self):\n        assert False\n",
		"helper.py":   "def not_a_test():\n    pass\n",
		"bug.go":      "func test_should_be_ignored() {}\n", // not .py: ignored
	}
	got := sortedNames(extractPyTestNames(files))
	want := []string{"TestSuite", "test_bar", "test_foo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractPyTestNames = %v, want %v", got, want)
	}
}

func TestExtractJSTestNames(t *testing.T) {
	files := map[string]string{
		"bug.test.js": "test('adds numbers', () => { expect(1+1).toBe(2); });\nit(\"subtracts\", () => {});\n",
		"bug.test.ts": "import { test } from 'node:test';\ntest('node test name', () => {});\n",
		"README.md":   "test('should be ignored', () => {});\n",
	}
	got := sortedNames(extractJSTestNames(files))
	want := []string{"adds numbers", "node test name", "subtracts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractJSTestNames = %v, want %v", got, want)
	}
}
