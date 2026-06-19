package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/llm"
)

// Git tool limits.
const (
	// gitBlamMaxLines is the hard cap on lines returned by git_blame. A range
	// wider than this is clamped and noted in the output.
	gitBlameMaxLines = 100
	// gitBlameMaxBytes caps the total output of git_blame (32KB).
	gitBlameMaxBytes = 32 * 1024
	// gitLogDefaultCount is the default number of commits returned by git_log.
	gitLogDefaultCount = 10
	// gitLogMaxCount is the hard ceiling for max_count in git_log.
	gitLogMaxCount = 50
	// gitLogMaxBytes caps the total output of git_log (32KB).
	gitLogMaxBytes = 32 * 1024
	// gitExecTimeout is the per-command deadline for git subprocess calls.
	gitExecTimeout = 10 * time.Second
)

// gitRunner executes `git -C dir <args...>` and returns its combined stdout
// bytes. It is a function type so tests can inject fake git output without
// needing a real git installation.
type gitRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// realGitRunner is the production gitRunner: exec git with a timeout, returning
// stdout on success and a descriptive error (trimmed stderr) on failure.
func realGitRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// Wrap the caller's context with a hard per-command deadline.
	ctx, cancel := context.WithTimeout(ctx, gitExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// gitBlameTool serves bounded git blame output rooted at a repository directory.
type gitBlameTool struct {
	root    *fsRoot
	repoDir string
	run     gitRunner
}

// NewGitBlame returns a git_blame tool rooted at dir. It runs git blame over a
// bounded line range (max 100 lines) and returns the annotated output. Use this
// to identify who last changed a suspicious code section and when — useful for
// spotting recently-introduced regressions or churn around a bug site. Results
// are bounded at 32KB. Paths are repo-relative and traversal-protected.
//
// runner may be nil to use the real git subprocess.
func NewGitBlame(dir string, runner gitRunner) (Tool, error) {
	root, err := newFSRoot(dir)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		runner = realGitRunner
	}
	return &gitBlameTool{root: root, repoDir: dir, run: runner}, nil
}

type gitBlameArgs struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

func (t *gitBlameTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "git_blame",
		Description: fmt.Sprintf(
			"Show the commit, date, and author for each line in a file range, using "+
				"'git blame'. Use this to identify when a suspicious section was last "+
				"changed and by whom — particularly useful for spotting recently-"+
				"introduced regressions or understanding who to ask about a bug. "+
				"The range is clamped to %d lines; output is bounded at %dKB. "+
				"Returns an error if the repository has no git history or the file "+
				"is not tracked — rely on file content alone in that case.",
			gitBlameMaxLines, gitBlameMaxBytes/1024),
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Repository-relative path to the file (no leading slash, no ..)."
    },
    "line_start": {
      "type": "integer",
      "description": "First line number (1-based) of the range to blame.",
      "minimum": 1
    },
    "line_end": {
      "type": "integer",
      "description": "Last line number (1-based, inclusive) of the range to blame.",
      "minimum": 1
    }
  },
  "required": ["path", "line_start", "line_end"],
  "additionalProperties": false
}`),
	}
}

func (t *gitBlameTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args gitBlameArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if args.LineStart <= 0 {
		return "", fmt.Errorf("line_start must be >= 1")
	}
	if args.LineEnd <= 0 {
		return "", fmt.Errorf("line_end must be >= 1")
	}
	if args.LineStart > args.LineEnd {
		return "", fmt.Errorf("line_start (%d) must be <= line_end (%d)", args.LineStart, args.LineEnd)
	}

	// Validate path through the fsRoot to prevent traversal.
	if _, err := t.root.resolve(args.Path); err != nil {
		return "", err
	}

	// Clamp the range to gitBlameMaxLines, noting the clamp in output.
	start := args.LineStart
	end := args.LineEnd
	clamped := false
	if end-start+1 > gitBlameMaxLines {
		end = start + gitBlameMaxLines - 1
		clamped = true
	}

	rangeArg := fmt.Sprintf("-L%d,%d", start, end)
	out, err := t.run(ctx, t.repoDir,
		"blame", rangeArg, "--date=short",
		"--", args.Path)
	if err != nil {
		return "", fmt.Errorf(
			"git blame failed for %q (not a git repository or file not tracked — "+
				"rely on file content instead): %w", args.Path, err)
	}

	var b strings.Builder
	if clamped {
		fmt.Fprintf(&b, "[note: range clamped to %d lines (%d-%d); call again with a different line_start to see more]\n",
			gitBlameMaxLines, start, end)
	}

	// Trim each entry to a bounded length to prevent minified/generated
	// code from producing enormous single lines. git blame's default
	// output has fixed-width fields, so normal entries are short.
	//
	// Inside the "(author date line)" prefix, the author is attacker-
	// controllable free text (in public repos). Flatten newlines and
	// collapse internal whitespace so an injected newline cannot fabricate
	// a new tool-output line or section. The hash, date, line number, and
	// the line content itself are not flattened: the line content is a
	// single source line (no embedded newline) and the rest are
	// structural fields. The author's newlines are stripped here, matching
	// the funnel's lead-note / refuter-reasoning guard.
	//
	// We walk the output as a stream rather than per-line, because a
	// newline inside the author would split a single blame entry across
	// multiple "lines" and defeat any line-oriented parse. The delimiter
	// between entries is a `\n` that follows the closing `) ` of the
	// meta region. Everything between the opening `(` of the meta region
	// and its closing `) ` is the author/date/line, and any newline that
	// appears inside that span is treated as whitespace.
	const maxEntryBytes = 256
	writeBlame := func(line string) {
		if len(line) > maxEntryBytes {
			b.WriteString(line[:maxEntryBytes])
			b.WriteString("…\n")
			return
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	rest := string(out)
	for len(rest) > 0 {
		// Locate the meta region. A valid blame entry starts with a hash
		// and a " (" prefix. If we don't find one, emit the rest as-is
		// (nothing to sanitize) and stop.
		i := strings.Index(rest, " (")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		// Emit everything up to and including the opening "(".
		b.WriteString(rest[:i+2])
		afterOpen := rest[i+2:]
		// Find the closing ") " of the meta region.
		j := strings.Index(afterOpen, ") ")
		if j < 0 {
			// Unterminated meta region. Emit what we have and stop; the
			// author cannot be safely parsed and we won't risk a forged
			// section header by guessing.
			b.WriteString(afterOpen)
			break
		}
		// The meta region is afterOpen[:j]. Flatten any newlines/tabs to
		// single spaces via strings.Fields, then take everything except
		// the last two tokens (date, line-number) as the author.
		meta := afterOpen[:j]
		parts := strings.Fields(meta)
		if len(parts) < 3 {
			// Malformed meta region (no recognizable date/line). Emit
			// the original bytes and continue.
			b.WriteString(afterOpen[:j+2])
			rest = afterOpen[j+2:]
			continue
		}
		author := strings.Join(strings.Fields(strings.Join(parts[:len(parts)-2], " ")), " ")
		date := parts[len(parts)-2]
		lineno := parts[len(parts)-1]
		// Emit "<author> <date> <lineno>) " then continue past the meta
		// region.
		b.WriteString(author)
		b.WriteByte(' ')
		b.WriteString(date)
		b.WriteByte(' ')
		b.WriteString(lineno)
		b.WriteString(") ")
		afterMeta := afterOpen[j+2:]
		// The entry content runs from here to the next newline (or end
		// of output). Newlines in the content are not part of well-formed
		// blame output, but a hostile author could still attempt to inject
		// them. We scan up to the next \n to find the end of this entry.
		if nl := strings.IndexByte(afterMeta, '\n'); nl >= 0 {
			writeBlame(afterMeta[:nl])
			rest = afterMeta[nl+1:]
		} else {
			writeBlame(afterMeta)
			rest = ""
		}
	}

	result := b.String()
	if len(result) > gitBlameMaxBytes {
		result = result[:gitBlameMaxBytes] +
			fmt.Sprintf("\n... [truncated at %dKB]\n", gitBlameMaxBytes/1024)
	}
	if result == "" {
		return "(no blame output — file may be untracked or repository has no commits)\n", nil
	}
	return result, nil
}

// gitLogTool serves bounded git log output rooted at a repository directory.
type gitLogTool struct {
	repoDir string
	run     gitRunner
}

// NewGitLog returns a git_log tool rooted at dir. It runs git log to show the
// commit history for the whole repository or a specific file. Use this to
// understand how recently and how often a file has been changed — files with
// high churn near a suspicious function are strong regression candidates.
// Output is bounded at 32KB. The max_count is capped at 50.
//
// Note: git pathspec magic (:/ and :(glob) forms) is accepted for the path
// argument and scopes to the full repository, which may exceed the snapshot's
// scan-filter scope. This is accepted because the call is read-only and all
// results are repo-contained.
//
// runner may be nil to use the real git subprocess.
func NewGitLog(dir string, runner gitRunner) (Tool, error) {
	if runner == nil {
		runner = realGitRunner
	}
	return &gitLogTool{repoDir: dir, run: runner}, nil
}

type gitLogArgs struct {
	Path     string `json:"path,omitempty"`
	MaxCount int    `json:"max_count,omitempty"`
}

func (t *gitLogTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "git_log",
		Description: fmt.Sprintf(
			"Show the recent commit history for the repository or a specific file, "+
				"using 'git log'. Each entry shows the short hash, date, author, and "+
				"subject. Use this to understand churn patterns and recency — a file "+
				"that changed many times recently is a strong candidate for "+
				"recently-introduced bugs. max_count defaults to %d and is capped at %d. "+
				"Returns an error if the repository has no history — rely on file "+
				"content alone in that case.",
			gitLogDefaultCount, gitLogMaxCount),
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Optional repository-relative file path to scope the log to (no leading slash, no ..). Omit to show the whole-repo log."
    },
    "max_count": {
      "type": "integer",
      "description": "Maximum number of commits to return (default 10, max 50).",
      "minimum": 1
    }
  },
  "additionalProperties": false
}`),
	}
}

func (t *gitLogTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args gitLogArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}

	count := args.MaxCount
	if count <= 0 {
		count = gitLogDefaultCount
	}
	if count > gitLogMaxCount {
		count = gitLogMaxCount
	}

	gitArgs := []string{
		"log",
		fmt.Sprintf("-n%d", count),
		"--date=short",
		"--format=%h %ad %an %s",
	}
	if args.Path != "" {
		gitArgs = append(gitArgs, "--", args.Path)
	}

	out, err := t.run(ctx, t.repoDir, gitArgs...)
	if err != nil {
		return "", fmt.Errorf(
			"git log failed (not a git repository or no history — "+
				"rely on file content instead): %w", err)
	}
	// The format string is "%h %ad %an %s". The author (%an) and subject
	// (%s) are attacker-controllable free text in public repos. Flatten
	// each rendered log entry so an injected newline in the author or
	// subject cannot fabricate a new tool-output line or section. The
	// hash and date are structural fields and are not flattened. This
	// matches the funnel's lead-note / refuter-reasoning guard: collapse
	// whitespace with strings.Fields and re-join with single spaces.
	//
	// We treat the output as a stream of entries, not lines. A newline
	// inside the author or subject would otherwise spill a single
	// commit across multiple "lines" and let the attacker forge a new
	// entry. A new entry is recognized by a leading "<short hash>
	// <YYYY-MM-DD> " prefix (matching `%h` and `%ad`). Lines that do
	// not start with such a prefix are continuations of the previous
	// entry and are joined to it with a space before flattening.
	//
	// `%h` is a 7-char short hash, but a longer `--abbrev` is harmless
	// here — we only require a token of hex digits to begin an entry.
	var logOut strings.Builder
	var current strings.Builder
	flushEntry := func() {
		if current.Len() == 0 {
			return
		}
		entry := current.String()
		current.Reset()
		fields := strings.Fields(entry)
		if len(fields) < 3 {
			// Not a well-formed entry (no recognizable hash/date).
			// Pass through untouched — no attacker-controllable fields
			// to flatten and we don't want to drop diagnostic noise.
			logOut.WriteString(entry)
			logOut.WriteByte('\n')
			return
		}
		hash := fields[0]
		date := fields[1]
		subject := strings.Join(strings.Fields(fields[len(fields)-1]), " ")
		author := strings.Join(strings.Fields(strings.Join(fields[2:len(fields)-1], " ")), " ")
		fmt.Fprintf(&logOut, "%s %s %s %s\n", hash, date, author, subject)
	}
	// A new entry begins with "<hex digits> <YYYY-MM-DD> " (matching
	// `%h` and `%ad`). A line that does not start with that pattern is
	// treated as a continuation of the previous entry.
	isEntryStart := func(line string) bool {
		// Find the first space; everything before is the hash token.
		sp := strings.IndexByte(line, ' ')
		if sp <= 0 {
			return false
		}
		for i := 0; i < sp; i++ {
			c := line[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
		// Expect a 10-char YYYY-MM-DD date immediately after the space.
		if len(line) < sp+1+10 {
			return false
		}
		date := line[sp+1 : sp+1+10]
		if date[4] != '-' || date[7] != '-' {
			return false
		}
		for _, hi := range []int{0, 1, 2, 3, 5, 6, 8, 9} {
			if c := date[hi]; c < '0' || c > '9' {
				return false
			}
		}
		// Followed by a space.
		return len(line) >= sp+1+10+1 && line[sp+1+10] == ' '
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if isEntryStart(line) {
			flushEntry()
			current.WriteString(line)
		} else {
			// Continuation of the previous entry (likely an attacker-
			// injected newline inside the author/subject). Join with a
			// single space; strings.Fields later will collapse any
			// embedded multi-space runs.
			if current.Len() > 0 {
				current.WriteByte(' ')
			}
			current.WriteString(line)
		}
	}
	flushEntry()
	result := logOut.String()

	if len(result) > gitLogMaxBytes {
		result = result[:gitLogMaxBytes] +
			fmt.Sprintf("\n... [truncated at %dKB]\n", gitLogMaxBytes/1024)
	}
	if strings.TrimSpace(result) == "" {
		return "(no commits in history — repository may be empty or file is untracked)\n", nil
	}
	return result, nil
}
