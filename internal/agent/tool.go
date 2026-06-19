package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// Tool is a single capability the model may invoke during a run. The harness
// advertises every tool's [llm.ToolDef] to the model and dispatches matching
// tool calls to [Tool.Run].
//
// Run receives the raw JSON arguments the model produced (validate/unmarshal
// them yourself) and returns a textual result. An error returned from Run is
// *not* a loop failure: the harness wraps it as an "ERROR:"-prefixed
// tool-result message and lets the model decide how to recover. Reserve errors
// for tool-level problems (bad arguments, file not found); never use them to
// signal that the loop should abort.
//
// Run must honor ctx cancellation. Implementations are invoked sequentially
// within a single run, so they need not be safe for concurrent calls from one
// Runner; but a Tool shared across concurrent Runners must be.
type Tool interface {
	// Def returns the tool's declaration (name, description, JSON-schema
	// parameters) as advertised to the model.
	Def() llm.ToolDef
	// Run executes the tool with the model-supplied arguments and returns the
	// textual result to feed back to the model.
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// toolError prefixes a tool failure so the model recognizes it as a recoverable
// error rather than a normal result. The harness uses this for every error a
// Tool returns.
func toolError(err error) string {
	return "ERROR: " + err.Error()
}

// unmarshalArgs decodes raw JSON tool arguments into dst. It returns a
// well-formed error the runner will surface as "ERROR: invalid arguments: …"
// when the model produced malformed JSON.
func unmarshalArgs(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// requireField returns an error if val (trimmed) is empty. name is the
// human-readable field name that appears in the error message.
func requireField(name, val string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

// requireLineNumber returns an error if n is less than 1. The error message
// follows the convention established by the existing tools ("line must be a
// 1-based line number").
func requireLineNumber(n int) error {
	if n < 1 {
		return fmt.Errorf("line must be a 1-based line number")
	}
	return nil
}

// toolSet indexes tools by name for dispatch and collects their defs for the
// request.
type toolSet struct {
	byName map[string]Tool
	defs   []llm.ToolDef
}

// newToolSet builds a dispatch table from tools. Later tools with a duplicate
// name win, mirroring map-assignment semantics; the defs slice preserves the
// first-seen order of the deduplicated set.
func newToolSet(tools []Tool) toolSet {
	ts := toolSet{byName: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		name := t.Def().Name
		if _, seen := ts.byName[name]; !seen {
			ts.defs = append(ts.defs, t.Def())
		}
		ts.byName[name] = t
	}
	return ts
}

// lookup returns the tool registered under name, if any.
func (ts toolSet) lookup(name string) (Tool, bool) {
	t, ok := ts.byName[name]
	return t, ok
}
