package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RunJSON runs the tool loop for task, instructing the model to return its final
// answer as a single JSON value matching schema, then unmarshals that answer
// into out (a pointer). If the model's output fails to parse, RunJSON makes one
// repair round-trip — sending the parse error back and asking for valid JSON
// only — before failing.
//
// schema is a JSON Schema (raw JSON) describing the expected shape; it is
// embedded verbatim in the instruction appended to the task. Pass nil to omit
// the schema and only require "JSON matching out".
//
// The returned [Outcome] is the underlying loop outcome (including the full
// transcript and any truncation). On a successful parse, out is populated and
// err is nil. If the run truncates before producing parseable JSON, or the
// repair round-trip still fails, err is non-nil but the Outcome is still
// returned for inspection.
func (r *Runner) RunJSON(ctx context.Context, task string, schema json.RawMessage, out any) (*Outcome, error) {
	prompt := task + "\n\n" + jsonInstruction(schema)

	outcome, err := r.Run(ctx, prompt)
	if err != nil {
		return outcome, err
	}

	if perr := parseInto(outcome.FinalText, out); perr == nil {
		return outcome, nil
	} else {
		// One repair round-trip: tell the model exactly what failed and demand
		// JSON only. We continue the same conceptual task; a fresh Run keeps the
		// loop simple and bounded by the same limits.
		repair := fmt.Sprintf(
			"%s\n\nYour previous output failed to parse as JSON: %s\nReturn ONLY valid JSON, with no prose, no explanation, and no markdown fences.",
			task, perr.Error(),
		)
		if schema != nil {
			repair += "\nIt must match this JSON schema:\n" + string(schema)
		}

		repairOutcome, rerr := r.Run(ctx, repair)
		if rerr != nil {
			return repairOutcome, rerr
		}
		// Surface the repair attempt's outcome (its transcript reflects the retry).
		if perr2 := parseInto(repairOutcome.FinalText, out); perr2 != nil {
			return repairOutcome, fmt.Errorf("agent: model output did not parse as JSON after one repair: %w", perr2)
		}
		return repairOutcome, nil
	}
}

// jsonInstruction builds the appended instruction telling the model to emit only
// JSON, optionally matching schema.
func jsonInstruction(schema json.RawMessage) string {
	var b strings.Builder
	b.WriteString("Respond with ONLY a single JSON value as your final answer — ")
	b.WriteString("no prose before or after, and no markdown code fences.")
	if len(schema) > 0 {
		b.WriteString(" The JSON must match this JSON schema:\n")
		b.Write(schema)
	}
	return b.String()
}

// parseInto strips any markdown fences from text and unmarshals the result into
// out. It returns a descriptive error on failure.
func parseInto(text string, out any) error {
	body := stripFences(text)
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("empty model output")
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return err
	}
	return nil
}

// stripFences removes a single surrounding markdown code fence (```...``` or
// ```json...```) from s if present, returning the inner content. If no fence is
// found, s is returned trimmed. This makes RunJSON tolerant of models that wrap
// JSON in fences despite instructions not to.
func stripFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// Drop the opening fence line (which may carry a language tag like "json").
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return t
	}
	inner := t[nl+1:]
	// Trim the trailing closing fence.
	if idx := strings.LastIndex(inner, "```"); idx >= 0 {
		inner = inner[:idx]
	}
	return strings.TrimSpace(inner)
}
