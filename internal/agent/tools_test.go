package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixtureTree builds a small repo tree for tool tests and returns its root.
//
//	root/
//	  README.md         "hello world\nsecond line\n"
//	  main.go           "package main\nfunc main() { foo() }\n"
//	  pkg/util.go       "package pkg\nfunc foo() int { return 42 }\n"
//	  pkg/data.bin      <NUL bytes>
//	  empty/            (empty dir)
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "README.md"), "hello world\nsecond line\n")
	mustWrite(t, filepath.Join(root, "main.go"), "package main\nfunc main() { foo() }\n")
	mustMkdir(t, filepath.Join(root, "pkg"))
	mustWrite(t, filepath.Join(root, "pkg", "util.go"), "package pkg\nfunc foo() int { return 42 }\n")
	mustWrite(t, filepath.Join(root, "pkg", "data.bin"), "\x00\x01\x02foo\x00")
	mustMkdir(t, filepath.Join(root, "empty"))
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestFSRoot_Resolve_Traversal(t *testing.T) {
	root := fixtureTree(t)
	fr, err := NewFSRoot(root)
	if err != nil {
		t.Fatalf("NewFSRoot: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"plain file", "README.md", false},
		{"nested file", "pkg/util.go", false},
		{"root via empty", "", false},
		{"dot", ".", false},
		{"forward-slash nested", "pkg/util.go", false},
		{"dotdot escape", "../secret", true},
		{"deep dotdot escape", "pkg/../../secret", true},
		{"absolute path", "/etc/passwd", true},
		{"dotdot only", "..", true},
		{"sneaky dotdot mid", "a/b/../../../x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fr.Resolve(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Errorf("resolve(%q) = %q, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Errorf("resolve(%q) unexpected error: %v", tc.path, err)
			}
		})
	}
}

func TestFSRoot_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := fixtureTree(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "top secret\n")

	// A symlink inside the root pointing at a file outside it.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A symlink inside the root pointing at a directory outside it.
	dirLink := filepath.Join(root, "outdir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	fr, err := NewFSRoot(root)
	if err != nil {
		t.Fatalf("NewFSRoot: %v", err)
	}

	if _, err := fr.Resolve("escape"); err == nil {
		t.Error("resolve via file symlink escaping root should fail")
	}
	if _, err := fr.Resolve("outdir/secret.txt"); err == nil {
		t.Error("resolve through dir symlink escaping root should fail")
	}
}

func TestReadFile(t *testing.T) {
	root := fixtureTree(t)
	tool, err := NewReadFile(root)
	if err != nil {
		t.Fatalf("NewReadFile: %v", err)
	}
	ctx := context.Background()

	t.Run("numbered lines", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{"path":"README.md"}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "1\thello world") || !strings.Contains(out, "2\tsecond line") {
			t.Errorf("output missing numbered lines:\n%s", out)
		}
	})

	t.Run("offset and limit", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{"path":"README.md","offset":2,"limit":1}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "2\tsecond line") {
			t.Errorf("offset window wrong:\n%s", out)
		}
		if strings.Contains(out, "hello world") {
			t.Errorf("offset should have skipped line 1:\n%s", out)
		}
	})

	t.Run("missing file is tool error", func(t *testing.T) {
		_, err := tool.Run(ctx, []byte(`{"path":"nope.txt"}`))
		if err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("directory is tool error", func(t *testing.T) {
		_, err := tool.Run(ctx, []byte(`{"path":"pkg"}`))
		if err == nil {
			t.Error("expected error reading a directory")
		}
	})

	t.Run("traversal rejected", func(t *testing.T) {
		_, err := tool.Run(ctx, []byte(`{"path":"../escape"}`))
		if err == nil {
			t.Error("expected traversal rejection")
		}
	})

	t.Run("bad args", func(t *testing.T) {
		if _, err := tool.Run(ctx, []byte(`{`)); err == nil {
			t.Error("expected invalid-args error")
		}
		if _, err := tool.Run(ctx, []byte(`{"path":""}`)); err == nil {
			t.Error("expected empty-path error")
		}
	})
}

func TestListDir(t *testing.T) {
	root := fixtureTree(t)
	tool, err := NewListDir(root)
	if err != nil {
		t.Fatalf("NewListDir: %v", err)
	}
	ctx := context.Background()

	t.Run("root listing dirs first", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// Directories (empty/, pkg/) should precede files (README.md, main.go).
		emptyIdx := strings.Index(out, "empty/")
		readmeIdx := strings.Index(out, "README.md")
		if emptyIdx < 0 || readmeIdx < 0 || emptyIdx > readmeIdx {
			t.Errorf("dirs should sort before files:\n%s", out)
		}
		if !strings.Contains(out, "pkg/\tdir") {
			t.Errorf("missing pkg dir entry:\n%s", out)
		}
		if !strings.Contains(out, "main.go\tfile\t") {
			t.Errorf("missing file size entry:\n%s", out)
		}
	})

	t.Run("empty dir", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{"path":"empty"}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "(empty directory)") {
			t.Errorf("expected empty marker:\n%s", out)
		}
	})

	t.Run("file is error", func(t *testing.T) {
		if _, err := tool.Run(ctx, []byte(`{"path":"README.md"}`)); err == nil {
			t.Error("expected error listing a file")
		}
	})

	t.Run("traversal rejected", func(t *testing.T) {
		if _, err := tool.Run(ctx, []byte(`{"path":".."}`)); err == nil {
			t.Error("expected traversal rejection")
		}
	})
}

func TestGrep(t *testing.T) {
	root := fixtureTree(t)
	tool, err := NewGrep(root)
	if err != nil {
		t.Fatalf("NewGrep: %v", err)
	}
	ctx := context.Background()

	t.Run("matches across files", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{"pattern":"foo"}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "main.go:2:") || !strings.Contains(out, "pkg/util.go:2:") {
			t.Errorf("expected matches in main.go and pkg/util.go:\n%s", out)
		}
	})

	t.Run("path glob narrows", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{"pattern":"foo","path_glob":"pkg/**"}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if strings.Contains(out, "main.go:") {
			t.Errorf("glob should have excluded main.go:\n%s", out)
		}
		if !strings.Contains(out, "pkg/util.go:") {
			t.Errorf("glob should include pkg/util.go:\n%s", out)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		out, err := tool.Run(ctx, []byte(`{"pattern":"zzzznotfound"}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "(no matches)") {
			t.Errorf("expected no-match marker:\n%s", out)
		}
	})

	t.Run("binary skipped", func(t *testing.T) {
		// data.bin contains 'foo' but is binary (has NUL) and must be skipped.
		out, err := tool.Run(ctx, []byte(`{"pattern":"foo"}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if strings.Contains(out, "data.bin") {
			t.Errorf("binary file should be skipped:\n%s", out)
		}
	})

	t.Run("invalid regexp is tool error", func(t *testing.T) {
		if _, err := tool.Run(ctx, []byte(`{"pattern":"("}`)); err == nil {
			t.Error("expected invalid-regexp error")
		}
	})

	t.Run("max_results caps", func(t *testing.T) {
		// Pattern matching every line; cap at 1.
		out, err := tool.Run(ctx, []byte(`{"pattern":".","max_results":1}`))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		matches := strings.Count(out, ":") // crude; each match line has file:line:
		if !strings.Contains(out, "truncated at 1 matches") {
			t.Errorf("expected truncation note (matches=%d):\n%s", matches, out)
		}
	})
}
