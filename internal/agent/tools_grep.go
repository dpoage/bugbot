package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// grep limits.
const (
	// defaultGrepMaxResults bounds matches when the caller doesn't specify.
	defaultGrepMaxResults = 100
	// grepHardMaxResults is the absolute ceiling regardless of requested max.
	grepHardMaxResults = 1000
	// grepMaxLineBytes caps a single matched line's reported length so a
	// minified/binary line can't blow up the result.
	grepMaxLineBytes = 1024
	// grepMaxFileBytes skips files larger than this (likely data/binaries).
	grepMaxFileBytes = 5 * 1024 * 1024
)

// grepTool runs a Go regexp across files under a repository directory.
type grepTool struct {
	root *fsRoot
}

// NewGrep returns a grep tool rooted at dir. It searches files under the
// repository for lines matching a Go regexp, returning file:line:text matches
// bounded by max_results (default 100). An optional path_glob narrows the search
// to matching repo-relative paths. Binary files are skipped. Paths are
// traversal-protected; symlinked directories are not followed out of the root.
func NewGrep(dir string) (Tool, error) {
	root, err := newFSRoot(dir)
	if err != nil {
		return nil, err
	}
	return &grepTool{root: root}, nil
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	PathGlob   string `json:"path_glob,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

func (t *grepTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "grep",
		Description: "Search repository files for lines matching a Go (RE2) regular " +
			"expression. Returns 'path:line:text' matches, bounded by max_results " +
			"(default 100). Use path_glob (e.g. '**/*.go') to restrict which files are " +
			"searched. Binary files and very large files are skipped.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Go (RE2) regular expression to match against each line."
    },
    "path_glob": {
      "type": "string",
      "description": "Optional glob over repo-relative paths (supports *, ?, **). Only matching files are searched."
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum number of matches to return (default 100).",
      "minimum": 1
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`),
	}
}

func (t *grepTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args grepArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regexp: %w", err)
	}

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = defaultGrepMaxResults
	}
	if maxResults > grepHardMaxResults {
		maxResults = grepHardMaxResults
	}

	var (
		b       strings.Builder
		count   int
		limited bool
	)

	walkErr := filepath.WalkDir(t.root.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry: skip it rather than aborting the whole search.
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if count >= maxResults {
			limited = true
			return filepath.SkipAll
		}

		// Never follow symlinks out of the root: skip symlinked dirs/files. A
		// symlink reported by WalkDir has its mode bit set on the DirEntry type.
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		rel, relErr := filepath.Rel(t.root.root, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		if args.PathGlob != "" && !matchGlob(args.PathGlob, relSlash) {
			return nil
		}

		n, hitLimit := t.searchFile(path, relSlash, re, &b, count, maxResults)
		count += n
		if hitLimit {
			limited = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}

	if count == 0 {
		return "(no matches)\n", nil
	}
	if limited {
		fmt.Fprintf(&b, "... [truncated at %d matches]\n", maxResults)
	}
	return b.String(), nil
}

// searchFile scans one file for matching lines, appending up to (maxResults -
// already) results to b. It returns the number of matches written and whether
// the global cap was reached. Binary and oversized files are skipped.
func (t *grepTool) searchFile(path, rel string, re *regexp.Regexp, b *strings.Builder, already, maxResults int) (int, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > grepMaxFileBytes {
		return 0, false
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()

	// Peek the first chunk to detect binary content (NUL byte) before scanning.
	br := bufio.NewReader(f)
	if peek, _ := br.Peek(8000); bytes.IndexByte(peek, 0) >= 0 {
		return 0, false
	}

	written := 0
	lineNo := 0
	sc := bufio.NewScanner(br)
	// Allow long lines without erroring; we cap the reported text ourselves.
	sc.Buffer(make([]byte, 0, 64*1024), 2*grepMaxFileBytes)
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if !re.MatchString(line) {
			continue
		}
		shown := line
		if len(shown) > grepMaxLineBytes {
			shown = shown[:grepMaxLineBytes] + "…"
		}
		fmt.Fprintf(b, "%s:%d:%s\n", rel, lineNo, shown)
		written++
		if already+written >= maxResults {
			return written, true
		}
	}
	return written, false
}
