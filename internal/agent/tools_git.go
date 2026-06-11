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

	// Trim each line to a bounded length to prevent minified/generated code
	// from producing enormous single lines. git blame's default output has
	// fixed-width fields, so normal lines are short.
	const maxLineBytes = 256
	for _, line := range strings.SplitAfter(string(out), "\n") {
		if len(line) > maxLineBytes {
			b.WriteString(line[:maxLineBytes])
			b.WriteString("…\n")
		} else {
			b.WriteString(line)
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

	result := string(out)
	if len(result) > gitLogMaxBytes {
		result = result[:gitLogMaxBytes] +
			fmt.Sprintf("\n... [truncated at %dKB]\n", gitLogMaxBytes/1024)
	}
	if strings.TrimSpace(result) == "" {
		return "(no commits in history — repository may be empty or file is untracked)\n", nil
	}
	return result, nil
}
