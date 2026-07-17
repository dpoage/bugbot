package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/llm"
)

// ErrUnparseableOutput marks a RunJSON failure whose cause is the model's final
// answer itself: it could not be parsed as JSON, or it parsed but violated the
// declared schema. This is distinct from an infrastructure failure (LLM
// transport, context cancellation) in the underlying tool loop, which RunJSON
// returns unwrapped from r.run / r.repair. Callers that drive their own
// revision loop can test errors.Is(err, ErrUnparseableOutput) to treat a
// malformed model answer as a recoverable, retry-able outcome instead of a hard
// abort.
var ErrUnparseableOutput = errors.New("model output did not parse as JSON")

// RunJSON runs the tool loop for task, instructing the model to return its final
// answer as a single JSON value matching schema, then unmarshals that answer
// into out (a pointer). If the model's output fails to parse OR fails deep
// schema validation (see [validateSchema]), RunJSON makes one repair
// round-trip — sending the precise error back and asking for valid JSON only —
// before failing.
//
// Every call reseeds the conversation from scratch (task becomes the sole
// seed message) — this is the right default for the finder/verifier/single-
// shot callers that dominate the codebase. A caller driving a multi-round
// revision loop that wants round N+1 to remember round N's investigation
// should use [Runner.RunJSONContinue] instead; RunJSON's behavior and cost
// profile are unchanged by that method's existence.
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
	return r.runJSON(ctx, nil, task, schema, out)
}

// RunJSONContinue behaves exactly like [Runner.RunJSON] except it CONTINUES a
// prior conversation instead of reseeding one: prev.Messages (the Messages
// field of an earlier RunJSON/RunJSONContinue call's Outcome, on this same
// Runner) becomes the starting history, and task is appended as a NEW
// user turn rather than becoming the conversation's sole seed message. A nil
// prev, or a prev with no Messages, degrades to plain reseeding — identical
// to RunJSON — so a first-round caller can use this method unconditionally
// without a nil check.
//
// This is the opt-in continuation entry point: it exists so a multi-round
// revision loop (see internal/repro/repro.go Attempt) can have round N+1's
// feedback land in the SAME conversation as round N's tool-driven
// investigation, instead of discarding that investigation and asking the
// model to re-orient from a truncated summary alone. maybeCompact still
// bounds the continued history exactly as it does within a single run — no
// separate summarizer is introduced here.
func (r *Runner) RunJSONContinue(ctx context.Context, prev *Outcome, task string, schema json.RawMessage, out any) (*Outcome, error) {
	var seed []llm.Message
	if prev != nil {
		seed = prev.Messages
	}
	return r.runJSON(ctx, seed, task, schema, out)
}

// runJSON is the shared implementation behind RunJSON and RunJSONContinue.
// seed is nil for RunJSON (reseed) or a prior Outcome's Messages for
// RunJSONContinue (continue).
func (r *Runner) runJSON(ctx context.Context, seed []llm.Message, task string, schema json.RawMessage, out any) (*Outcome, error) {
	prompt := task + "\n\n" + jsonInstruction(schema)

	// Reserve the last iteration for a forced finalization turn: if the model is
	// still investigating when the iteration cap is reached, it gets one final
	// completion that demands the JSON answer now, instead of the loop returning
	// dangling exploration prose that can never parse. The schema is threaded
	// natively so the finalization turn also benefits from grammar-constrained
	// output on capable adapters.
	outcome, err := r.run(ctx, seed, prompt, finalizationPrompt(schema), schema)
	if err != nil {
		return outcome, err
	}

	// Strip + deep schema validation first (cheap, schema-aware): this catches
	// valid-JSON-but-contract-violating output that a typed unmarshal silently
	// tolerates — a bad enum (severity "blocker"), a candidate missing its
	// nested "evidence", an empty required string, an empty repro "files" map.
	// Only after the body satisfies the full schema do we attempt the typed
	// unmarshal: an early unmarshal of a wrong-shaped body yields a misleading
	// "cannot unmarshal …" error, where the schema violation is the actionable
	// one and is what the repair round-trip echoes back to the model.
	perr := parseJSONInto(outcome.FinalText, schema, out)
	if perr == nil {
		return outcome, nil
	}

	// Schema-guided rescue (bugbot-9fac): weak models frequently prefix the
	// final JSON with prose ("Based on my investigation… {…}") or leave a
	// mangled head, both of which fail the leading-value parse above. Before
	// burning the repair round-trip — a tools-less, HISTORY-LESS single
	// completion that must reproduce the whole answer blind and often
	// fabricates — scan the cleaned output for the first embedded JSON value
	// that ALREADY satisfies the schema. The schema is the arbiter, so an
	// incidental json-ish fragment in the prose cannot hijack the answer.
	if body, ok := rescueBody(outcome.FinalText, schema); ok {
		if uerr := json.Unmarshal([]byte(body), out); uerr == nil {
			return outcome, nil
		}
	}

	// A run cut short by the budget pool or its own per-run token budget has no
	// headroom for a repair completion, and a budget-stopped empty/unparseable
	// output must keep its budget TruncationReason so the funnel classifies it as
	// a budget stop, not a parse failure (bugbot-1q0). Skipping the repair here
	// also preserves the budget overshoot bound (no extra post-exhaustion call).
	if outcome.Truncated &&
		(outcome.TruncationReason == TruncTokenBudget || outcome.TruncationReason == TruncBudgetPool) {
		return outcome, fmt.Errorf("agent: %w%s: %w",
			ErrUnparseableOutput, truncationNote(outcome), perr)
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
	// repair() reopened the streamed transcript (O_APPEND) to record its
	// turn; close that fd here so it does not outlive the call. Over a long
	// backlog run every repaired finding would otherwise leak one fd until a
	// GC finalizer happened to run. Deferred so it fires on all paths below.
	if repairOutcome.Transcript != nil {
		defer repairOutcome.Transcript.closeStream()
	}
	// The repair completion runs against its own throwaway single-turn history
	// (see [Runner.repair]), not outcome.messages, so it never sees — and
	// therefore repairOutcome.Messages never carries — the run's investigation.
	// Propagate the pre-repair conversation onto repairOutcome regardless of
	// how repair turns out, so a caller chaining RunJSONContinue after a round
	// that needed repair still continues from the real investigation instead
	// of an empty history.
	repairOutcome.Messages = outcome.Messages
	if rerr != nil {
		return repairOutcome, rerr
	}
	if perr2 := parseJSONInto(repairOutcome.FinalText, schema, out); perr2 != nil {
		// Same rescue as the pre-repair path: a repair completion that
		// wrapped a schema-valid answer in prose still counts.
		if body, ok := rescueBody(repairOutcome.FinalText, schema); ok {
			if uerr := json.Unmarshal([]byte(body), out); uerr == nil {
				return repairOutcome, nil
			}
		}
		return repairOutcome, fmt.Errorf("agent: %w after one repair%s: %w",
			ErrUnparseableOutput, truncationNote(repairOutcome), perr2)
	}
	return repairOutcome, nil
}

// parseJSONInto strips text to its JSON body (think blocks and fences
// removed, leading complete value extracted — see [stripBody]), deep-validates
// it against schema (see [validateSchema]), and unmarshals it into out. The
// first failure is returned in that precedence order, so the actionable
// schema violation — not a misleading typed-unmarshal error — is what the
// repair round-trip echoes back to the model.
func parseJSONInto(text string, schema json.RawMessage, out any) error {
	body, err := stripBody(text)
	if err != nil {
		return err
	}
	if verr := validateSchema(schema, []byte(body)); verr != nil {
		return verr
	}
	return json.Unmarshal([]byte(body), out)
}

// rescueBody scans the cleaned model output for the first embedded JSON
// value that satisfies schema, tolerating prose or mangled bytes BEFORE the
// value — the case [stripBody] deliberately does not handle (it only
// extracts a LEADING complete value). Returns ("", false) when schema is
// empty (no arbiter, no safe way to pick a candidate) or when no candidate
// both decodes as a complete JSON value and passes deep validation.
//
// Candidate starts are '{' / '[' bytes in the cleaned text; each is decoded
// with encoding/json's Decoder (which respects string/escape boundaries and
// rejects incomplete values, so a truncated tail is never rescued). The scan
// is bounded to keep pathological outputs (brace-dense code dumps) cheap;
// decode failures on non-JSON braces cost one token read each.
func rescueBody(text string, schema json.RawMessage) (string, bool) {
	if len(schema) == 0 {
		return "", false
	}
	body := stripFences(llm.StripThinkBlocks(text))
	const maxCandidates = 64
	tried := 0
	for i := 0; i < len(body) && tried < maxCandidates; i++ {
		if body[i] != '{' && body[i] != '[' {
			continue
		}
		tried++
		dec := json.NewDecoder(strings.NewReader(body[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil || len(raw) == 0 {
			continue
		}
		if validateSchema(schema, []byte(raw)) != nil {
			continue
		}
		return string(raw), true
	}
	return "", false
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
// [validateSchema] both inspect. An all-whitespace or empty body is
// reported as an explicit "empty model output" error so the caller can treat
// it as a parse failure and surface a clear message.
//
// If the cleaned body contains a complete JSON value followed by trailing
// content (e.g. `{...},{...}` or `{...},`), stripBody extracts only the
// first complete value so that validateSchema and json.Unmarshal see clean
// JSON. This makes RunJSON tolerant of models that append prose or a second
// value after a valid answer. If the leading value is incomplete or missing,
// the original cleaned body is returned unchanged so the existing repair
// round-trip logic still drives recovery.
func stripBody(text string) (string, error) {
	body := stripFences(llm.StripThinkBlocks(text))
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("empty model output")
	}
	// Extract exactly the first complete JSON value. encoding/json's Decoder
	// respects string/escape boundaries and errors on an incomplete leading
	// value, so this never mis-splits on braces or commas inside strings and
	// never rescues a truncated value.
	dec := json.NewDecoder(strings.NewReader(body))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err == nil && len(raw) > 0 {
		return string(raw), nil
	}
	return body, nil
}

// validateSchema deeply validates body against schema. Unlike a shallow
// root-only check, it walks the schema and the parsed instance together and
// enforces the JSON-Schema subset the bugbot phase schemas actually use:
//
//   - type — object/array/string/integer/number/boolean/null (integer accepts
//     only integral numbers; number accepts any).
//   - required — at EVERY object level, not just the root (a candidate missing
//     its "evidence", a plan missing "cmd").
//   - properties — recursively.
//   - additionalProperties — false rejects unknown keys; a subschema validates
//     the values of free-form maps (the repro/patch "files" object keyed by
//     path with string values).
//   - items — recursively, for every array element.
//   - enum — exact membership (a severity of "blocker", a confidence of
//     "Medium").
//   - minItems / minProperties / minLength / maxLength / minimum / maximum.
//
// Errors are path-qualified (e.g. candidates[0].severity) so the RunJSON
// repair round-trip can tell the model exactly what to fix. This is the
// harness-side guarantee that every agent->phase JSON boundary is bounded by
// its declared schema even when the provider's StructuredOutput capability is
// off and no grammar-constrained decoding happened on the wire (the dominant
// weak-model path). An empty/nil schema is a no-op.
//
// The validator is a deliberately closed subset: it does NOT implement $ref,
// allOf/anyOf/oneOf, pattern, format, or multipleOf, because no bugbot phase
// schema uses them. Unknown keywords are ignored, not rejected.
func validateSchema(schema json.RawMessage, body []byte) error {
	if len(schema) == 0 {
		return nil
	}
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return fmt.Errorf("schema is not valid JSON: %w", err)
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("body is not valid JSON: %w", err)
	}
	return validateNode("", root, parsed)
}

// validateNode validates val against a single schema node, which is either a
// map (a normal schema object) or a bool (a JSON-Schema boolean schema: true
// accepts anything, false rejects everything). Any other node shape is treated
// as "no constraint" and accepted.
func validateNode(path string, schema, val any) error {
	switch s := schema.(type) {
	case bool:
		if !s {
			return fmt.Errorf("%s: value is not permitted (schema is false)", loc(path))
		}
		return nil
	case map[string]any:
		if err := checkType(path, s, val); err != nil {
			return err
		}
		if err := checkEnum(path, s, val); err != nil {
			return err
		}
		// Structural keywords apply only to the matching instance type
		// (standard JSON-Schema semantics): a "minItems" on a string instance,
		// say, is simply inapplicable. The type mismatch (if any) was already
		// reported by checkType above.
		switch v := val.(type) {
		case map[string]any:
			return validateObject(path, s, v)
		case []any:
			return validateArray(path, s, v)
		case string:
			return validateString(path, s, v)
		case float64:
			return validateNumber(path, s, v)
		}
		return nil
	default:
		return nil
	}
}

// checkType enforces the schema's "type" keyword, which may be a single type
// name or an array of acceptable names.
func checkType(path string, schema map[string]any, val any) error {
	t, ok := schema["type"]
	if !ok {
		return nil
	}
	switch want := t.(type) {
	case string:
		if !matchesType(want, val) {
			return typeErr(path, jsonTypeOf(val), want)
		}
	case []any:
		for _, w := range want {
			if ws, ok := w.(string); ok && matchesType(ws, val) {
				return nil
			}
		}
		return typeErr(path, jsonTypeOf(val), typeListString(want))
	}
	return nil
}

// matchesType reports whether val satisfies the JSON-Schema type name want.
// Numbers always decode to float64 through encoding/json, so "integer" is an
// integral float64 and "number" is any float64. Unknown type names accept.
func matchesType(want string, val any) bool {
	switch want {
	case "object":
		_, ok := val.(map[string]any)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "string":
		_, ok := val.(string)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "number":
		_, ok := val.(float64)
		return ok
	case "integer":
		f, ok := val.(float64)
		return ok && !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f)
	case "null":
		return val == nil
	default:
		return true
	}
}

// checkEnum enforces "enum" membership. Values are compared by canonical-JSON
// equality, which is exact for the scalar enums the phase schemas use.
func checkEnum(path string, schema map[string]any, val any) error {
	raw, ok := schema["enum"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	target := jsonString(val)
	for _, e := range list {
		if jsonString(e) == target {
			return nil
		}
	}
	return fmt.Errorf("%s: value %s is not one of the allowed values %s",
		loc(path), target, jsonString(list))
}

// validateObject enforces required, properties, additionalProperties, and
// minProperties on an object instance. required is checked first (the most
// actionable failure), then per-property recursion proceeds over SORTED keys so
// the first reported violation is deterministic across runs.
func validateObject(path string, schema, obj map[string]any) error {
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			key, _ := r.(string)
			if key == "" {
				continue
			}
			if _, present := obj[key]; !present {
				return requiredErr(path, key)
			}
		}
	}
	if n, ok := numKeyword(schema, "minProperties"); ok && len(obj) < int(n) {
		return fmt.Errorf("%s: object has %d properties, fewer than the required minimum %d",
			loc(path), len(obj), int(n))
	}
	props, _ := schema["properties"].(map[string]any)
	addl, hasAddl := schema["additionalProperties"]
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := obj[k]
		if sub, ok := props[k]; ok {
			if err := validateNode(childPath(path, k), sub, v); err != nil {
				return err
			}
			continue
		}
		if !hasAddl {
			continue // additionalProperties defaults to true (permit)
		}
		switch a := addl.(type) {
		case bool:
			if !a {
				return fmt.Errorf("%s: unexpected property %q is not allowed by the schema",
					loc(path), k)
			}
		case map[string]any:
			if err := validateNode(childPath(path, k), a, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateArray enforces minItems/maxItems and recurses into each element
// against the "items" subschema.
func validateArray(path string, schema map[string]any, arr []any) error {
	if n, ok := numKeyword(schema, "minItems"); ok && len(arr) < int(n) {
		return fmt.Errorf("%s: array has %d items, fewer than the required minimum %d",
			loc(path), len(arr), int(n))
	}
	if n, ok := numKeyword(schema, "maxItems"); ok && len(arr) > int(n) {
		return fmt.Errorf("%s: array has %d items, more than the allowed maximum %d",
			loc(path), len(arr), int(n))
	}
	if items, ok := schema["items"]; ok {
		for i, el := range arr {
			if err := validateNode(indexPath(path, i), items, el); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateString enforces minLength/maxLength, counted in Unicode code points
// (JSON-Schema semantics), not bytes.
func validateString(path string, schema map[string]any, s string) error {
	if n, ok := numKeyword(schema, "minLength"); ok {
		if l := utf8.RuneCountInString(s); l < int(n) {
			return fmt.Errorf("%s: string length %d is below the minimum %d", loc(path), l, int(n))
		}
	}
	if n, ok := numKeyword(schema, "maxLength"); ok {
		if l := utf8.RuneCountInString(s); l > int(n) {
			return fmt.Errorf("%s: string length %d exceeds the maximum %d", loc(path), l, int(n))
		}
	}
	return nil
}

// validateNumber enforces minimum/maximum on a numeric instance.
func validateNumber(path string, schema map[string]any, f float64) error {
	if n, ok := numKeyword(schema, "minimum"); ok && f < n {
		return fmt.Errorf("%s: value %s is below the minimum %s",
			loc(path), trimNum(f), trimNum(n))
	}
	if n, ok := numKeyword(schema, "maximum"); ok && f > n {
		return fmt.Errorf("%s: value %s exceeds the maximum %s",
			loc(path), trimNum(f), trimNum(n))
	}
	return nil
}

// typeErr formats a type-mismatch error, preserving the historical root-level
// phrasing ("root JSON type ...") so callers and tests that match on it stay
// stable while nested mismatches gain a path prefix.
func typeErr(path, actual, want string) error {
	if path == "" {
		return fmt.Errorf("root JSON type %q does not match schema type %q", actual, want)
	}
	return fmt.Errorf("%s: JSON type %q does not match schema type %q", path, actual, want)
}

// requiredErr formats a missing-required-field error, preserving the
// historical root-level phrasing ("root object missing required field ...").
func requiredErr(path, key string) error {
	if path == "" {
		return fmt.Errorf("root object missing required field %q", key)
	}
	return fmt.Errorf("%s: missing required field %q", path, key)
}

// loc renders a path for error messages, using "root" for the empty path.
func loc(path string) string {
	if path == "" {
		return "root"
	}
	return path
}

// childPath extends path with an object property name.
func childPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// indexPath extends path with an array index.
func indexPath(path string, i int) string {
	return fmt.Sprintf("%s[%d]", loc(path), i)
}

// numKeyword reads a numeric schema keyword (minItems, minLength, minimum, …).
// JSON numbers decode to float64, so the keyword is a float64 when present.
func numKeyword(schema map[string]any, name string) (float64, bool) {
	v, ok := schema[name]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// typeListString renders a "type" array (e.g. ["string","null"]) as
// "string|null" for an error message.
func typeListString(types []any) string {
	parts := make([]string, 0, len(types))
	for _, t := range types {
		if s, ok := t.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "|")
}

// jsonString renders v as canonical JSON for an error message (and for enum
// equality). Falls back to %v on the (unexpected) marshal error.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// trimNum renders a float64 without a trailing ".0" for whole numbers, so a
// "minimum: 1" violation reads "value 0 is below the minimum 1", not "0.0".
func trimNum(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
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
