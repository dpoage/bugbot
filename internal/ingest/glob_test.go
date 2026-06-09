package ingest

import "testing"

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*", "main.go", true},
		{"**/*", "a/b/c.go", true},
		{"**/*.go", "main.go", true},
		{"**/*.go", "cmd/main.go", true},
		{"**/*.go", "cmd/main.py", false},
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false}, // single * does not cross separator
		{"vendor/**", "vendor/x/y.go", true},
		{"vendor/**", "vendor", true}, // trailing ** matches zero segments too
		{"vendor/**", "src/vendor/x.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/c", false},
		{"**/*_test.go", "internal/ingest/foo_test.go", true},
		{"**/*_test.go", "internal/ingest/foo.go", false},
		{"node_modules/**", "node_modules/pkg/index.js", true},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{".git/**", ".git/config", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.path); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestContainsWord(t *testing.T) {
	cases := []struct {
		content string
		word    string
		want    bool
	}{
		{"import helper", "helper", true},
		{"helperFunc()", "helper", false}, // not a whole word
		{"my_helper = 1", "helper", false},
		{"call(helper)", "helper", true},
		{"helper", "helper", true},
		{"a helper b", "helper", true},
		{"thehelper", "helper", false},
		{"", "helper", false},
	}
	for _, c := range cases {
		if got := containsWord([]byte(c.content), c.word); got != c.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", c.content, c.word, got, c.want)
		}
	}
}
