package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// parseInto strips reasoning-model think blocks and any markdown fences from
// text, then unmarshals the result into out. It returns a descriptive error on
// failure.
func parseInto(text string, out any) error {
	body := stripFences(stripThinkBlocks(text))
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("empty model output")
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return err
	}
	return nil
}

// leadingThinkRE matches a single <think>...</think> or <thinking>...</thinking>
// block anchored at the start of the (whitespace-trimmed) text. It is
// case-insensitive ((?i)), spans newlines ((?s) makes . match '\n'), and is
// non-greedy so it stops at the first closing tag rather than swallowing JSON
// that legitimately contains the literal "</think>" inside a string value.
//
// Stripping is anchored at the leading edge and repeated, so only think blocks
// that PRECEDE the JSON payload are removed; a "<think>" appearing inside the
// JSON body is left untouched. This is the documented limitation: think blocks
// must wrap the payload, not be embedded within it.
var leadingThinkRE = regexp.MustCompile(`(?is)^\s*<think(?:ing)?>.*?</think(?:ing)?>`)

// unclosedThinkRE matches an UNCLOSED trailing think tag: an opening
// <think>/<thinking> with no closing tag, running to end of input. Reasoning
// models truncated mid-thought emit this. Anchored at the leading edge (after
// closed leading blocks are removed) so it only strips a think span that
// precedes — or replaces — the payload, never one embedded inside JSON.
var unclosedThinkRE = regexp.MustCompile(`(?is)^\s*<think(?:ing)?>.*$`)

// stripThinkBlocks removes reasoning-model think spans that wrap the JSON
// payload. Real reasoning models (e.g. MiniMax M3) emit one or more
// "<think>...</think>" blocks inline in message content before the actual
// answer; without stripping, RunJSON would burn its repair round-trip on every
// such reply.
//
// It strips repeatedly from the leading edge: multiple consecutive closed
// blocks are all removed, then a single unclosed trailing <think> (a truncated
// thought) is dropped. It deliberately does NOT strip blocks embedded inside the
// JSON body, so a JSON string value that legitimately contains the literal
// "<think>" survives intact.
func stripThinkBlocks(s string) string {
	out := s
	for {
		stripped := leadingThinkRE.ReplaceAllString(out, "")
		if stripped == out {
			break
		}
		out = stripped
	}
	// After all closed leading blocks are gone, drop a single unclosed trailing
	// think span (truncation). Only fires when an opening tag still leads the
	// remaining text, so a well-formed JSON payload is never touched.
	out = unclosedThinkRE.ReplaceAllString(out, "")
	return out
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
