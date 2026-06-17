package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// RunJSON runs the tool loop for task, instructing the model to return its final
// answer as a single JSON value matching schema, then unmarshals that answer
// into out (a pointer). If the model's output fails to parse OR fails the
// shallow root-level shape check, RunJSON makes one repair round-trip — sending
// the error back and asking for valid JSON only — before failing.
//
// schema is a JSON Schema (raw JSON) describing the expected shape. It is
// threaded natively as llm.Request.ResponseSchema (capability-gated: when the
// client reports StructuredOutput==true the schema is sent on the wire, so
// adapters that support native structured output can apply grammar-constrained
// decoding). The same schema is also embedded verbatim in the prompt as a
// fallback for adapters without that capability and for weak/older models. Pass
// nil to skip the schema entirely; in that case only "JSON matching out" is
// required and no native schema is sent.
//
// The returned [Outcome] is the underlying loop outcome (including the full
// transcript and any truncation). On a successful parse, out is populated and
// err is nil. If the run truncates before producing parseable JSON, or the
// repair round-trip still fails, err is non-nil but the Outcome is still
// returned for inspection.
func (r *Runner) RunJSON(ctx context.Context, task string, schema json.RawMessage, out any) (*Outcome, error) {
	prompt := task + "\n\n" + jsonInstruction(schema)

	// Reserve the last iteration for a forced finalization turn: if the model is
	// still investigating when the iteration cap is reached, it gets one final
	// completion that demands the JSON answer now, instead of the loop returning
	// dangling exploration prose that can never parse. The schema is threaded
	// natively so the finalization turn also benefits from grammar-constrained
	// output on capable adapters.
	outcome, err := r.run(ctx, prompt, finalizationPrompt(schema), schema)
	if err != nil {
		return outcome, err
	}

	// Strip + shape check first (cheap, schema-aware) — this is the check that
	// catches wrong-shape-but-valid-JSON (a bare array when the schema requires
	// an object, a missing required field, etc.). Only after the body passes
	// the root-shape check do we attempt the typed unmarshal: a typed unmarshal
	// of an array into a struct fails with "cannot unmarshal array into Go
	// value", which is misleading to a caller that wants to know the body was
	// the wrong shape. The shape check's error message is the actionable one.
	var perr error
	body, berr := stripBody(outcome.FinalText)
	if berr != nil {
		perr = berr
	} else if v := validateRootShape(schema, []byte(body)); v != nil {
		perr = v
	} else if p := json.Unmarshal([]byte(body), out); p != nil {
		perr = p
	} else {
		return outcome, nil
	}

	// A run cut short by the budget pool or its own per-run token budget has no
	// headroom for a repair completion, and a budget-stopped empty/unparseable
	// output must keep its budget TruncationReason so the funnel classifies it as
	// a budget stop, not a parse failure (bugbot-1q0). Skipping the repair here
	// also preserves the budget overshoot bound (no extra post-exhaustion call).
	if outcome.Truncated &&
		(outcome.TruncationReason == TruncTokenBudget || outcome.TruncationReason == TruncBudgetPool) {
		return outcome, fmt.Errorf("agent: model output did not parse as JSON%s: %w",
			truncationNote(outcome), perr)
	}

	// One repair round-trip: tell the model exactly what failed and demand JSON
	// only. The repair is now a single tools-less, schema-bearing completion
	// (see [Runner.repair]) so adapters that support native structured output
	// apply grammar-constrained decoding and the shape is guaranteed on the wire.
	repair := fmt.Sprintf(
		"%s\n\nYour previous output failed to parse as JSON: %s\nReturn ONLY valid JSON, with no prose, no explanation, and no markdown fences.",
		task, perr.Error(),
	)
	if len(schema) > 0 {
		repair += "\nIt must match this JSON schema:\n" + string(schema)
	}

	repairOutcome, rerr := r.repair(ctx, outcome.Transcript, repair, schema)
	if rerr != nil {
		return repairOutcome, rerr
	}
	repairBody, berr2 := stripBody(repairOutcome.FinalText)
	if berr2 != nil {
		return repairOutcome, fmt.Errorf("agent: model output did not parse as JSON after one repair%s: %w",
			truncationNote(repairOutcome), berr2)
	}
	if verr := validateRootShape(schema, []byte(repairBody)); verr != nil {
		return repairOutcome, fmt.Errorf("agent: model output did not parse as JSON after one repair%s: %w",
			truncationNote(repairOutcome), verr)
	}
	if perr2 := json.Unmarshal([]byte(repairBody), out); perr2 != nil {
		return repairOutcome, fmt.Errorf("agent: model output did not parse as JSON after one repair%s: %w",
			truncationNote(repairOutcome), perr2)
	}
	return repairOutcome, nil
}

// truncationNote returns a short parenthetical when the run's final completion
// stopped at the output token cap, so a max_tokens truncation is distinguishable
// in the error from a model that simply produced malformed JSON. Empty
// otherwise.
func truncationNote(o *Outcome) string {
	if o != nil && o.LastStopReason == llm.StopMaxTokens {
		return " (output truncated at the max-tokens cap)"
	}
	return ""
}

// finalizationPrompt is the user-role message injected on the reserved final
// turn when RunJSON's loop is about to hit the iteration cap. It tells the model
// to stop investigating and emit only the JSON answer, optionally restating the
// schema.
func finalizationPrompt(schema json.RawMessage) string {
	var b strings.Builder
	b.WriteString("You have reached the end of your investigation budget. STOP investigating now. ")
	b.WriteString("Do NOT call any more tools. Output ONLY your final answer as a single JSON value — ")
	b.WriteString("no prose, no explanation, no markdown fences — based on what you have already found. ")
	b.WriteString("If you found nothing, return the empty result the schema allows.")
	if len(schema) > 0 {
		b.WriteString("\nThe JSON must match this JSON schema:\n")
		b.Write(schema)
	}
	return b.String()
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

// stripBody returns the cleaned JSON body the model produced (think blocks and
// markdown fences removed), exactly the bytes the typed unmarshal and
// [validateRootShape] both inspect. An all-whitespace or empty body is
// reported as an explicit "empty model output" error so the caller can treat
// it as a parse failure and surface a clear message.
func stripBody(text string) (string, error) {
	body := stripFences(stripThinkBlocks(text))
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("empty model output")
	}
	return body, nil
}

// validateRootShape checks the SHALLOW root-level shape of body against
// schema. It is deliberately lenient — it does NOT recurse into items or
// properties and ignores additionalProperties. The two checks it performs are:
//
//  1. The root JSON type matches the schema's top-level "type" when set
//     (e.g. body is an object when schema requires an object, not a bare
//     array).
//  2. When the body is an object, every name in the schema's top-level
//     "required" array is present as a key.
//
// Empty/nil schema is a no-op. The function returns a descriptive error on
// mismatch (catching a bare-array-where-an-object-was-required or a missing
// required field) so the RunJSON caller can route it through the repair path
// — the whole point of this check is to catch wrong-shape-but-valid-JSON that
// a typed unmarshal of `out` cannot detect (the typed unmarshal succeeds, the
// caller gets a struct with default values, and downstream code silently
// misbehaves).
func validateRootShape(schema json.RawMessage, body []byte) error {
	if len(schema) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return fmt.Errorf("schema is not valid JSON: %w", err)
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("body is not valid JSON: %w", err)
	}
	actualType := jsonTypeOf(parsed)
	if wantType, _ := root["type"].(string); wantType != "" && wantType != actualType {
		return fmt.Errorf("root JSON type %q does not match schema type %q", actualType, wantType)
	}
	// Required-keys check: only meaningful when the body is an object. If the
	// schema requires fields but the body is not an object, the type check
	// above already caught it.
	req, _ := root["required"].([]any)
	if len(req) == 0 {
		return nil
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return nil
	}
	for _, name := range req {
		key, _ := name.(string)
		if key == "" {
			continue
		}
		if _, present := m[key]; !present {
			return fmt.Errorf("root object missing required field %q", key)
		}
	}
	return nil
}

// jsonTypeOf returns the JSON Schema name for the JSON value v: one of
// "null", "boolean", "number", "string", "array", "object". It returns "" for
// values it does not recognize — the caller treats that as "no usable type"
// and skips the type-mismatch check.
func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64, float32, int, int32, int64, uint, uint32, uint64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return ""
	}
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
// thought) is dropped. It deliberately does NOT strip blocks embedded inside
// the JSON body, so a JSON string value that legitimately contains the literal
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
// ```json...```) from s if present, returning the inner content. If no fence
// is found, s is returned trimmed. This makes RunJSON tolerant of models that
// wrap JSON in fences despite instructions not to.
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
