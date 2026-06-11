package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// Read-file limits. These bound a single read_file result so a tool call cannot
// blow the model's context window or the harness's memory. They are the DEFAULTS;
// a caller can tighten them per tool via [ReadCaps] / [NewReadFileWithCaps] so a
// role that re-sends a growing history every turn (e.g. the finder) carries
// smaller per-file results without ever mutating the conversation prefix — the
// cache-safe way to slow history growth (see bugbot-3nf).
const (
	// maxReadLines caps the number of lines returned by read_file.
	maxReadLines = 2000
	// maxReadBytes caps the number of bytes read from disk for read_file.
	maxReadBytes = 256 * 1024
)

// ReadCaps bounds a single read_file result. The zero value resolves to the
// package defaults (maxReadLines / maxReadBytes), so an unset field never
// tightens unexpectedly. A negative value is treated as "default" too; to read
// effectively-unbounded files, pass an explicitly large cap.
type ReadCaps struct {
	// MaxLines caps the numbered lines returned. Zero/negative uses maxReadLines.
	MaxLines int
	// MaxBytes caps the bytes pulled off disk before line windowing. Zero/negative
	// uses maxReadBytes.
	MaxBytes int
}

// resolve substitutes package defaults for unset/negative fields.
func (c ReadCaps) resolve() ReadCaps {
	if c.MaxLines <= 0 {
		c.MaxLines = maxReadLines
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = maxReadBytes
	}
	return c
}

// readFileTool serves numbered file contents rooted at a repository directory.
type readFileTool struct {
	root *fsRoot
	caps ReadCaps
}

// NewReadFile returns a read_file tool rooted at dir. It reads a UTF-8 text file
// and returns its contents as numbered lines, with optional offset/limit
// windowing. Results are capped at ~2000 lines / 256KB; truncation is noted in
// the output. Paths are repo-relative and traversal-protected.
func NewReadFile(dir string) (Tool, error) {
	return NewReadFileWithCaps(dir, ReadCaps{})
}

// NewReadFileWithCaps is like [NewReadFile] but tightens the per-result line and
// byte caps. Zero-value caps fields fall back to the package defaults, so this is
// a safe superset of NewReadFile. Tighter caps shrink each read_file result at
// the source, slowing the growth of a re-sent conversation history WITHOUT
// mutating any earlier message — so unlike history compaction it never forfeits
// a prompt-cache prefix, which is why it (not compaction) is the finder's default
// token-burn lever under a strong prompt cache (see bugbot-3nf).
func NewReadFileWithCaps(dir string, caps ReadCaps) (Tool, error) {
	root, err := newFSRoot(dir)
	if err != nil {
		return nil, err
	}
	return &readFileTool{root: root, caps: caps.resolve()}, nil
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (t *readFileTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "read_file",
		Description: fmt.Sprintf(
			"Read a UTF-8 text file from the repository and return it as "+
				"numbered lines (1-based). Use offset/limit to read a window of a large "+
				"file. Output is capped at ~%d lines / %dKB; truncation is noted.",
			t.caps.MaxLines, t.caps.MaxBytes/1024),
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Repository-relative path to the file (no leading slash, no ..)."
    },
    "offset": {
      "type": "integer",
      "description": "1-based line number to start reading from. Omit or 0 to start at the top.",
      "minimum": 0
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of lines to return. Omit for the default cap.",
      "minimum": 1
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`),
	}
}

func (t *readFileTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args readFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if args.Offset < 0 {
		return "", fmt.Errorf("offset must be >= 0")
	}
	if args.Limit < 0 {
		return "", fmt.Errorf("limit must be >= 0")
	}

	abs, err := t.root.resolve(args.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %w", args.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory, not a file", args.Path)
	}

	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("cannot open %q: %w", args.Path, err)
	}
	defer func() { _ = f.Close() }()

	// Bound the bytes we pull off disk regardless of line count.
	data, err := io.ReadAll(io.LimitReader(f, int64(t.caps.MaxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("cannot read %q: %w", args.Path, err)
	}
	byteTruncated := false
	if len(data) > t.caps.MaxBytes {
		data = data[:t.caps.MaxBytes]
		byteTruncated = true
	}

	return renderNumbered(string(data), args.Offset, args.Limit, byteTruncated, t.caps), nil
}

// renderNumbered formats content as numbered lines, applying a 1-based offset
// and a line limit (capped at caps.MaxLines), and appends truncation notes.
func renderNumbered(content string, offset, limit int, byteTruncated bool, caps ReadCaps) string {
	lines := strings.Split(content, "\n")
	// A trailing newline produces a spurious final empty element; drop it so the
	// line count reflects real lines.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start > total {
		start = total
	}

	lineCap := caps.MaxLines
	if limit > 0 && limit < lineCap {
		lineCap = limit
	}
	end := start + lineCap
	lineTruncated := end < total
	if end > total {
		end = total
	}

	var b strings.Builder
	// Pad line numbers to the width of the largest shown number for alignment.
	width := len(strconv.Itoa(end))
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, lines[i])
	}
	if start >= end {
		b.WriteString("(no lines in range)\n")
	}
	if lineTruncated {
		fmt.Fprintf(&b, "... [truncated: showing lines %d-%d of %d — call read_file again with offset=%d to continue]\n", start+1, end, total, end+1)
	}
	if byteTruncated {
		fmt.Fprintf(&b, "... [truncated at %d bytes: this is a window, not the whole file — call read_file again with offset=%d (and optionally limit) to read further]\n", caps.MaxBytes, end+1)
	}
	return b.String()
}

// listDirTool lists directory entries rooted at a repository directory.
type listDirTool struct {
	root *fsRoot
}

// NewListDir returns a list_dir tool rooted at dir. It lists the entries of a
// repository directory (default: the root), reporting each entry's type and
// size, sorted with directories first then by name. Paths are repo-relative and
// traversal-protected.
func NewListDir(dir string) (Tool, error) {
	root, err := newFSRoot(dir)
	if err != nil {
		return nil, err
	}
	return &listDirTool{root: root}, nil
}

type listDirArgs struct {
	Path string `json:"path,omitempty"`
}

func (t *listDirTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "list_dir",
		Description: "List the entries of a repository directory. Omit path to list " +
			"the repository root. Each entry shows its type (dir/file/other) and size; " +
			"directories are listed first, then files, alphabetically.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Repository-relative directory path (no leading slash, no ..). Omit for the repo root."
    }
  },
  "additionalProperties": false
}`),
	}
}

func (t *listDirTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args listDirArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}

	abs, err := t.root.resolve(args.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot stat %q: %w", displayPath(args.Path), err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", displayPath(args.Path))
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("cannot list %q: %w", displayPath(args.Path), err)
	}

	type row struct {
		name  string
		isDir bool
		kind  string
		size  int64
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		r := row{name: e.Name(), isDir: e.IsDir()}
		switch {
		case e.IsDir():
			r.kind = "dir"
		case e.Type().IsRegular():
			r.kind = "file"
		default:
			r.kind = "other"
		}
		if fi, err := e.Info(); err == nil {
			r.size = fi.Size()
		}
		rows = append(rows, r)
	}
	// Directories first, then by name within each group.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].isDir != rows[j].isDir {
			return rows[i].isDir
		}
		return rows[i].name < rows[j].name
	})

	var b strings.Builder
	for _, r := range rows {
		if r.isDir {
			fmt.Fprintf(&b, "%s/\t%s\n", r.name, r.kind)
		} else {
			fmt.Fprintf(&b, "%s\t%s\t%dB\n", r.name, r.kind, r.size)
		}
	}
	if len(rows) == 0 {
		b.WriteString("(empty directory)\n")
	}
	return b.String(), nil
}

// displayPath renders an empty path as the repo root for user-facing messages.
func displayPath(p string) string {
	if p == "" {
		return "."
	}
	return filepath.ToSlash(p)
}
