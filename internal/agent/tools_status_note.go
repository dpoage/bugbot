package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// statusNoteTool is a tool the agent can call to surface its current
// hypothesis or working note as an activity update. It is an internal status
// channel only — NOT a place to report findings — and is gated behind the
// Scan.StatusNotes config flag (off by default).
//
// On invocation the note is routed through the activity sink (which emits a
// KindAgentActivity event so the pane/status readers see it) and returned as
// a tool result so it appears naturally in the transcript.
type statusNoteTool struct {
	// sink routes the sanitized note to the progress system.
	sink func(activity string)
}

// NewStatusNoteTool builds the status_note Tool bound to sink. sink must be
// non-nil; it is invoked with the sanitized note each time the agent calls
// the tool. The returned Tool satisfies [agent.Tool].
func NewStatusNoteTool(sink func(activity string)) Tool {
	return statusNoteTool{sink: sink}
}

func (s statusNoteTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "status_note",
		Description: "Record a brief internal note about your current working hypothesis or " +
			"investigation direction. This is a STATUS channel for observability — it is NOT " +
			"for reporting findings. Use post_lead to report actual security issues. " +
			"The note will appear in the live activity view but does not affect your output.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"note": {
					"type": "string",
					"description": "A short single-line note about what you are currently investigating or thinking (max ~120 characters)."
				}
			},
			"required": ["note"]
		}`),
	}
}

func (s statusNoteTool) Run(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("status_note: parse args: %w", err)
	}

	// Sanitize: collapse whitespace to a single line, truncate to 120 runes.
	note := strings.Join(strings.Fields(params.Note), " ")
	runes := []rune(note)
	if len(runes) > 120 {
		note = string(runes[:119]) + "…"
	}

	s.sink(note)
	return "noted", nil
}
