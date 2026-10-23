package funnel

// Exploration-only switches (explore/finder-recall), read from the comma-separated
// BUGBOT_EXPERIMENTS; unset is production behavior.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	llmkit "github.com/dpoage/llmkit"
)

const (
	expLogicLens  = "logic-lens"
	expSoftEmpty  = "soft-empty"
	expSizeChunks = "size-chunks"
	// expFinderExec gives finders run_code, unsandboxed: only for runs already
	// inside a disposable container (the Harbor task environment).
	expFinderExec = "finder-exec"
)

// About three 800-line read_file pages per chunk.
const sizeChunkLines = 2500

const (
	finderExecMax     = 8
	finderExecTimeout = 120 * time.Second
	finderExecOutCap  = 16 * 1024
)

func experimentOn(name string) bool {
	for _, e := range strings.Split(os.Getenv("BUGBOT_EXPERIMENTS"), ",") {
		if strings.TrimSpace(e) == name {
			return true
		}
	}
	return false
}

var logicLens = Lens{
	Name: "logic/wrong-result",
	Core: "Hunt for wrong-result logic defects: code that runs without error but " +
		"computes the wrong answer. Compare what each function, method, and branch " +
		"is documented or named to do with what it actually does: an input " +
		"combination that falls into a branch meant for another case; inverted, " +
		"incomplete, or overlapping conditions; using the wrong variable, attribute, " +
		"or result (for example a value left over from a discarded iteration); " +
		"arithmetic or aggregation whose operator or value type silently changes " +
		"the meaning; and state updated in the wrong order. Construct the concrete " +
		"input that produces the wrong result and trace it through the code before " +
		"reporting.",
}

const softEmptyParagraph = `Report every concrete defect you can confirm by reading the code; a confirmed bug with a precise location is valuable even when it looks minor. Return an empty list only after you have examined every target file and found nothing you can confirm. Do not pad the list with speculative entries.`

const finderExecParagraph = `

You can also run code: run_code executes a shell command in a scratch copy of the repository with the project's environment installed. When you suspect a wrong result or a crash, write a small probe (for example python -c or a short pytest run) with the concrete input and run it. A demonstrated wrong output or traceback is the strongest evidence you can attach; a probe that behaves correctly means do not report. You have a small budget of runs, so probe suspicions rather than exploring.`

// chunkByLines packs files in input order into chunks of at most maxFiles
// files and sizeChunkLines lines; an oversized file gets its own chunk.
func chunkByLines(root string, files []string, maxFiles int) []fileChunk {
	var out []fileChunk
	var cur []string
	curLines := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, fileChunk{files: cur, langs: chunkLangs(cur)})
			cur, curLines = nil, 0
		}
	}
	for _, f := range files {
		n := countLines(filepath.Join(root, f))
		if len(cur) > 0 && (curLines+n > sizeChunkLines || (maxFiles > 0 && len(cur) >= maxFiles)) {
			flush()
		}
		cur = append(cur, f)
		curLines += n
	}
	flush()
	return out
}

func countLines(path string) int {
	fh, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	n := 0
	for sc.Scan() {
		n++
	}
	return n
}

// execWorkspace is one scratch copy of the repo per process, so probes cannot
// edit the scanned tree; finders share it.
type execWorkspace struct {
	once sync.Once
	dir  string
	err  error
}

var finderExecWS execWorkspace

func (w *execWorkspace) get(root string) (string, error) {
	w.once.Do(func() {
		dir, err := os.MkdirTemp("", "bugbot-exec-ws-")
		if err != nil {
			w.err = err
			return
		}
		if out, err := exec.Command("cp", "-a", root+"/.", dir).CombinedOutput(); err != nil {
			w.err = fmt.Errorf("copy repo for run_code: %v: %s", err, out)
			return
		}
		w.dir = dir
	})
	return w.dir, w.err
}

// runCodeTool is per finder unit: its exec budget is not shared.
type runCodeTool struct {
	root string
	used atomic.Int32
}

func newRunCodeTool(root string) *runCodeTool { return &runCodeTool{root: root} }

func (t *runCodeTool) Def() llmkit.ToolDef {
	return llmkit.ToolDef{
		Name: "run_code",
		Description: fmt.Sprintf("Run a shell command (bash -c) in a scratch copy of the repository"+
			" with the project's environment installed, to demonstrate or rule out a suspected bug."+
			" Returns exit code, stdout, and stderr. Budget: %d runs; timeout %s each.",
			finderExecMax, finderExecTimeout),
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "minLength": 1, "description": "Shell command, e.g. python -c '...' or python -m pytest path::test -x -q"}
  },
  "required": ["command"],
  "additionalProperties": false
}`),
	}
}

func (t *runCodeTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("run_code: need a non-empty command")
	}
	if n := t.used.Add(1); n > finderExecMax {
		return fmt.Sprintf("run_code budget exhausted (%d runs used); continue by reading code.", finderExecMax), nil
	}
	dir, err := finderExecWS.get(t.root)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, finderExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", args.Command)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	code := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if runErr != nil {
		code = -1
		fmt.Fprintf(&stderr, "\n[run_code: %v]", runErr)
	}
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(&stderr, "\n[run_code: timed out after %s]", finderExecTimeout)
	}
	return fmt.Sprintf("exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s", code, capOut(stdout.Bytes()), capOut(stderr.Bytes())), nil
}

func capOut(b []byte) string {
	if len(b) <= finderExecOutCap {
		return string(b)
	}
	return string(b[:finderExecOutCap]) + fmt.Sprintf("\n[truncated: %d of %d bytes shown]", finderExecOutCap, len(b))
}
