package funnel

import (
	"errors"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
)

// newRateLimitErr constructs an *llm.APIError with Kind == llm.ErrRateLimited,
// matching the shape produced by openaiAdapter.normalizeErr (openai.go lines
// 208-220): errors.As on an *openai.Error sets kind = ErrRateLimited when the
// status is 429, then newAPIError wraps it. The Kind field drives errors.Is
// via APIError.Unwrap (errors.go lines 62-69). Only exported fields are set
// here; the unexported inner err field is nil, which Unwrap handles safely.
func newRateLimitErr(msg string) error {
	return &llm.APIError{
		Kind:       llm.ErrRateLimited,
		StatusCode: 429,
		Provider:   "openai",
		Message:    msg,
	}
}

// outcomeWithText builds a minimal *agent.Outcome whose FinalText is set.
func outcomeWithText(text string) *agent.Outcome {
	return &agent.Outcome{FinalText: text}
}

// TestClassifyFinderErr_RateLimit verifies that a rate-limit error
// (llm.ErrRateLimited sentinel) is classified as finderClassRateLimited even
// when the model also produced non-empty output (the error dominates).
func TestClassifyFinderErr_RateLimit(t *testing.T) {
	err := newRateLimitErr("rate limit exceeded, retry after 60s")
	// Verify the error satisfies the sentinel so the test is meaningful.
	if !errors.Is(err, llm.ErrRateLimited) {
		t.Fatal("test setup: newRateLimitErr did not produce an error satisfying llm.ErrRateLimited")
	}

	// A non-budget outcome (Truncated=false) paired with a rate-limit error.
	outcome := outcomeWithText("some partial output")
	got := classifyFinderErr(outcome, err)
	if got != finderClassRateLimited {
		t.Errorf("classifyFinderErr = %q, want %q", got, finderClassRateLimited)
	}
}

// TestClassifyFinderErr_EmptyOutput verifies that a nil error with an outcome
// whose FinalText is empty yields finderClassEmptyOutput.
func TestClassifyFinderErr_EmptyOutput(t *testing.T) {
	outcome := outcomeWithText("")
	got := classifyFinderErr(outcome, nil)
	if got != finderClassEmptyOutput {
		t.Errorf("classifyFinderErr = %q, want %q", got, finderClassEmptyOutput)
	}
}

// TestClassifyFinderErr_Unparseable verifies that a non-budget, non-rate-limit
// error with a non-empty FinalText yields finderClassUnparseable — the model
// produced something but it was not valid JSON.
func TestClassifyFinderErr_Unparseable(t *testing.T) {
	outcome := outcomeWithText("not json at all")
	err := errors.New("agent: model output did not parse as JSON after one repair: invalid character 'o'")
	got := classifyFinderErr(outcome, err)
	if got != finderClassUnparseable {
		t.Errorf("classifyFinderErr = %q, want %q", got, finderClassUnparseable)
	}
}

// TestClassifyFinderErr_BudgetStop verifies that a budget-truncated outcome
// yields finderClassBudgetStop regardless of the error.
func TestClassifyFinderErr_BudgetStop(t *testing.T) {
	outcome := &agent.Outcome{
		FinalText:        "",
		Truncated:        true,
		TruncationReason: agent.TruncBudgetPool,
	}
	got := classifyFinderErr(outcome, errors.New("some error"))
	if got != finderClassBudgetStop {
		t.Errorf("classifyFinderErr = %q, want %q", got, finderClassBudgetStop)
	}
}

// TestBuildFinderPostmortem_RateLimit verifies that a rate-limit error produces
// a postmortem with:
//   - Class = finderClassRateLimited
//   - ErrString carrying the error message
//   - RawHead / RawLen / Empty populated from the outcome
func TestBuildFinderPostmortem_RateLimit(t *testing.T) {
	rateLimitMsg := "openai rate limit: 429 too many requests"
	err := newRateLimitErr(rateLimitMsg)
	outcome := outcomeWithText("partial model text before rate limit hit")

	pm := buildFinderPostmortem(outcome, err)

	if pm.Class != finderClassRateLimited {
		t.Errorf("Class = %q, want %q", pm.Class, finderClassRateLimited)
	}
	if !strings.Contains(pm.ErrString, "429") {
		t.Errorf("ErrString = %q, want it to contain \"429\"", pm.ErrString)
	}
	if pm.Empty {
		t.Errorf("Empty = true, want false (outcome had non-empty FinalText)")
	}
	if pm.RawLen == 0 {
		t.Errorf("RawLen = 0, want > 0 (outcome had non-empty FinalText)")
	}
	if pm.RawHead == "" {
		t.Errorf("RawHead is empty, want non-empty")
	}
}

// TestBuildFinderPostmortem_EmptyOutput verifies the empty-output case:
// no error, outcome with empty FinalText.
func TestBuildFinderPostmortem_EmptyOutput(t *testing.T) {
	outcome := outcomeWithText("")

	pm := buildFinderPostmortem(outcome, nil)

	if pm.Class != finderClassEmptyOutput {
		t.Errorf("Class = %q, want %q", pm.Class, finderClassEmptyOutput)
	}
	if pm.ErrString != "" {
		t.Errorf("ErrString = %q, want empty (err was nil)", pm.ErrString)
	}
	if !pm.Empty {
		t.Errorf("Empty = false, want true")
	}
	if pm.RawLen != 0 {
		t.Errorf("RawLen = %d, want 0", pm.RawLen)
	}
	if pm.RawHead != "" {
		t.Errorf("RawHead = %q, want empty", pm.RawHead)
	}
}

// TestBuildFinderPostmortem_Unparseable verifies the generic unparseable case:
// non-nil error, non-empty FinalText, not a rate-limit, not budget-truncated.
func TestBuildFinderPostmortem_Unparseable(t *testing.T) {
	rawText := `{"candidates": [{"incomplete": true`
	parseErr := errors.New("agent: model output did not parse as JSON: unexpected EOF")
	outcome := outcomeWithText(rawText)

	pm := buildFinderPostmortem(outcome, parseErr)

	if pm.Class != finderClassUnparseable {
		t.Errorf("Class = %q, want %q", pm.Class, finderClassUnparseable)
	}
	if !strings.Contains(pm.ErrString, "unexpected EOF") {
		t.Errorf("ErrString = %q, want it to contain the parse error", pm.ErrString)
	}
	if pm.RawHead != rawText {
		t.Errorf("RawHead = %q, want %q", pm.RawHead, rawText)
	}
	if pm.RawLen != len(rawText) {
		t.Errorf("RawLen = %d, want %d", pm.RawLen, len(rawText))
	}
}

// TestBuildFinderPostmortem_RawCap verifies that RawHead is capped at
// finderPostmortemRawCap bytes while RawLen reflects the full length.
func TestBuildFinderPostmortem_RawCap(t *testing.T) {
	// Build a raw text longer than the cap.
	longText := strings.Repeat("x", finderPostmortemRawCap+100)
	parseErr := errors.New("parse error")
	outcome := outcomeWithText(longText)

	pm := buildFinderPostmortem(outcome, parseErr)

	if len(pm.RawHead) != finderPostmortemRawCap {
		t.Errorf("len(RawHead) = %d, want %d (cap)", len(pm.RawHead), finderPostmortemRawCap)
	}
	if pm.RawLen != len(longText) {
		t.Errorf("RawLen = %d, want %d (full length)", pm.RawLen, len(longText))
	}
}

// TestBuildFinderPostmortem_HadThink verifies that HadThink is set when the
// raw output contains a "<think>" span (reasoning-model thought block).
func TestBuildFinderPostmortem_HadThink(t *testing.T) {
	thinkText := "<think>let me reason about this</think>not json"
	parseErr := errors.New("parse error")
	outcome := outcomeWithText(thinkText)

	pm := buildFinderPostmortem(outcome, parseErr)

	if !pm.HadThink {
		t.Errorf("HadThink = false, want true (text contained <think>)")
	}
}

// TestBuildFinderPostmortem_NilOutcome verifies nil-safety: a nil outcome
// (budget-pool pre-turn stop, no model call happened) must not panic.
func TestBuildFinderPostmortem_NilOutcome(t *testing.T) {
	// nil outcome + nil error: this happens on a pure budget pool stop where
	// the runner never issued a completion.
	pm := buildFinderPostmortem(nil, nil)

	if pm.Class != finderClassEmptyOutput {
		t.Errorf("Class = %q, want %q (nil outcome => empty output)", pm.Class, finderClassEmptyOutput)
	}
	if pm.RawLen != 0 {
		t.Errorf("RawLen = %d, want 0 (nil outcome)", pm.RawLen)
	}
}

// TestFinderPostmortemDetail_RateLimitShape verifies that finderPostmortemDetail
// encodes the classification and error string in a way an operator can grep for.
func TestFinderPostmortemDetail_RateLimitShape(t *testing.T) {
	err := newRateLimitErr("429 rate limited")
	outcome := outcomeWithText("partial")
	pm := buildFinderPostmortem(outcome, err)
	detail := finderPostmortemDetail(pm)

	if !strings.Contains(detail, "class=rate-limited") {
		t.Errorf("detail missing class=rate-limited: %s", detail)
	}
	if !strings.Contains(detail, "429") {
		t.Errorf("detail missing 429 in err string: %s", detail)
	}
}
