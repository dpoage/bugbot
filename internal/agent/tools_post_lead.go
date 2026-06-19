package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// maxLeadNoteLen bounds the note field: it is stored verbatim and rendered
// into a future finder prompt, so it must stay short.
const maxLeadNoteLen = 500

// PostLeadTool lets a finder agent record a cross-lens suspicion on the
// blackboard so the target lens's next finder run can investigate it. It is
// deliberately NOT direct agent-to-agent communication: the post is persisted
// in the store, consumed deterministically at the start of the target lens's
// next hypothesize pass, and is fully testable without any agent coordination.
//
// The tool is ONLY offered to finder agents — never to refuter agents. Refuter
// independence (no shared state or context with finders) is what kills false
// positives; adding the blackboard to refuters would let a bias planted by one
// finder contaminate independent verification. See the comment in
// hypothesize.go where refuter tools are assembled.
//
// Each instance carries the poster lens name (injected at construction time by
// the funnel so the posting agent cannot impersonate another lens) and the
// set of valid lens names (for validation and error messages). The onPost
// callback is where the funnel injects the store write and stats increment,
// keeping the agent package free of store imports — the same pattern as
// SandboxExecTool's onExec.
type PostLeadTool struct {
	posterLens  string
	validLenses map[string]bool
	sortedNames []string // for deterministic error messages
	onPost      func(targetLens, file string, line int, note string, confidence float64) error
}

// NewPostLeadTool builds a post_lead tool instance for one finder agent.
// posterLens is the lens this finder is working on. validLensNames is the full
// set of lens names (typically all builtin lens names) — an unknown target_lens
// returns a validation error listing the valid names. onPost is called on every
// valid post; the funnel supplies the implementation (store write + counter
// increment).
func NewPostLeadTool(posterLens string, validLensNames []string, onPost func(targetLens, file string, line int, note string, confidence float64) error) *PostLeadTool {
	valid := make(map[string]bool, len(validLensNames))
	sorted := make([]string, len(validLensNames))
	for i, n := range validLensNames {
		valid[n] = true
		sorted[i] = n
	}
	sort.Strings(sorted)
	return &PostLeadTool{
		posterLens:  posterLens,
		validLenses: valid,
		sortedNames: sorted,
		onPost:      onPost,
	}
}

// postLeadArgs is the JSON schema for the tool arguments.
type postLeadArgs struct {
	TargetLens string   `json:"target_lens"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	Note       string   `json:"note"`
	Confidence *float64 `json:"confidence,omitempty"` // optional 0..1; clamped, defaults to 0
}

// Def implements agent.Tool.
func (t *PostLeadTool) Def() llm.ToolDef {
	return llm.ToolDef{
		Name: "post_lead",
		Description: "Record a suspicion that belongs to a DIFFERENT lens's area of expertise." +
			" Use this when you notice something outside your assigned focus that another" +
			" lens should investigate — for example, a nil-safety finder noticing inconsistent" +
			" locking. The lead is stored on the blackboard and injected into the target lens's" +
			" next scan run. Prefer to use this for OTHER lenses' territory; report your own" +
			" lens's findings as candidates in the normal output." +
			" target_lens must be a known lens name; file must be a repo-relative path;" +
			" line must be >= 1; note must be non-empty.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "target_lens": {
      "type": "string",
      "description": "The lens that should investigate this suspicion (must be a known lens name)."
    },
    "file": {
      "type": "string",
      "description": "Repo-relative path to the file containing the suspicious code."
    },
    "line": {
      "type": "integer",
      "minimum": 1,
      "description": "1-based line number of the suspicious code."
    },
    "note": {
      "type": "string",
      "maxLength": 500,
      "description": "Brief description of the suspicion for the target lens's finder agent (at most 500 characters)."
    },
    "confidence": {
      "type": "number",
      "minimum": 0,
      "maximum": 1,
      "description": "Poster's confidence in this tip (0..1). Omit to default to 0."
    }
  },
  "required": ["target_lens", "file", "line", "note"],
  "additionalProperties": false
}`),
	}
}

// Run implements agent.Tool. It validates the arguments, then calls onPost. A
// validation error is returned as a tool error so the model can retry with
// corrected arguments.
func (t *PostLeadTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args postLeadArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}

	// Validate target_lens.
	if !t.validLenses[args.TargetLens] {
		return "", fmt.Errorf("unknown target_lens %q; valid lens names are: %s",
			args.TargetLens, strings.Join(t.sortedNames, ", "))
	}

	// Validate line.
	if args.Line < 1 {
		return "", fmt.Errorf("line must be >= 1, got %d", args.Line)
	}

	// Validate note. Length is bounded (mirrored by maxLength in the schema)
	// because the note is stored verbatim and later interpolated into another
	// finder's task prompt — unbounded model-authored text there is both a
	// storage-bloat and a prompt-pollution vector.
	if strings.TrimSpace(args.Note) == "" {
		return "", fmt.Errorf("note must be non-empty")
	}
	if len(args.Note) > maxLeadNoteLen {
		return "", fmt.Errorf("note must be at most %d bytes, got %d", maxLeadNoteLen, len(args.Note))
	}

	// Validate file: must be a repo-local relative path. IsLocal rejects
	// absolute paths AND ".." traversal in one check, so a model cannot store
	// a lead pointing outside the repository.
	if args.File == "" {
		return "", fmt.Errorf("file must be non-empty")
	}
	if !filepath.IsLocal(args.File) {
		return "", fmt.Errorf("file must be a repo-relative path inside the repository, got %q", args.File)
	}

	confidence := 0.0
	if args.Confidence != nil {
		confidence = *args.Confidence
		if confidence < 0 {
			confidence = 0
		} else if confidence > 1 {
			confidence = 1
		}
	}

	if err := t.onPost(args.TargetLens, args.File, args.Line, args.Note, confidence); err != nil {
		return "", fmt.Errorf("post lead: %w", err)
	}

	return fmt.Sprintf("lead posted to %q: %s:%d — %s", args.TargetLens, args.File, args.Line, args.Note), nil
}
