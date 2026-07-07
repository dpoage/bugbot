package funnel

// Acceptance tests for bugbot-rwe: when a finder agent hits the
// per-completion output cap (StopMaxTokens) while a reasoning model is
// burning its <think> block, the funnel retries once with a doubled
// per-completion cap so the model has room for think + JSON. This file
// drives runFinderWithPrompt directly with bespoke fakes — the shared
// scriptedClient in fake_test.go hardcodes StopEndTurn so it cannot
// simulate the cap-truncation signal at all.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
)

// rweFinderFake is a minimal llm.Client whose Complete branches on
// req.MaxTokens to simulate a reasoning model that burns the per-completion
// cap on its first attempt and then succeeds once given a doubled cap. Each
// request is recorded so tests can assert the doubled-cap wire request was
// actually issued. Capabilities() returns the zero value — the runner
// drops native schema-constrained decoding and falls back to the
// prompt-embedded schema, which is exactly what runFinderWithPrompt
// exercises today. Concurrency-safe; the test runner launches one agent
// at a time.
type rweFinderFake struct {
	mu            sync.Mutex
	requests      []llm.Request
	oversizedBody string // returned when req.MaxTokens exceeds the default cap
	truncatedBody string // returned at or below the default cap; must NOT parse as JSON
}

func newRweFinderFake(oversized, truncated string) *rweFinderFake {
	return &rweFinderFake{oversizedBody: oversized, truncatedBody: truncated}
}

func (c *rweFinderFake) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *rweFinderFake) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	if req.MaxTokens > DefaultMaxOutputTokens {
		// Retry path: doubled cap; return valid candidates JSON.
		return llm.Response{
			Text:       c.oversizedBody,
			StopReason: llm.StopEndTurn,
			Usage: llm.Usage{
				InputTokens:          10,
				OutputTokens:         25,
				CacheReadInputTokens: 0,
			},
		}, nil
	}
	// Standard-cap path: simulated reasoning-model <think> blob that
	// hits the cap before emitting any JSON. The runner's own
	// continuation (runner.go:400) will make ONE extra call with the
	// same cap, so returning StopMaxTokens for both halves stitches
	// into a non-empty but non-JSON blob — enough to fail RunJSON's
	// parse and the repair round-trip both.
	return llm.Response{
		Text:       c.truncatedBody,
		StopReason: llm.StopMaxTokens,
		Usage: llm.Usage{
			InputTokens:          10,
			OutputTokens:         int64(req.MaxTokens),
			CacheReadInputTokens: 0,
		},
	}, nil
}

// rweMaxTokensSeen reports the largest req.MaxTokens this fake has
// served, or 0 when no requests have been observed yet. The recovery
// test asserts that the runner issued a request larger than the default
// cap, which proves the doubling reached the wire rather than just the
// in-process runner option.
func (c *rweFinderFake) rweMaxTokensSeen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var max int
	for _, r := range c.requests {
		if r.MaxTokens > max {
			max = r.MaxTokens
		}
	}
	return max
}

// rweNonJSONClient models a finder that always returns valid (non-empty)
// assistant text that is NOT JSON, with StopEndTurn so LastStopReason does
// NOT equal StopMaxTokens. This is the "clean parse failure" case that
// must NOT trigger the doubled-cap retry: the truncation signal is absent.
type rweNonJSONClient struct {
	mu      sync.Mutex
	maxSeen int
	text    string
}

func (c *rweNonJSONClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *rweNonJSONClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	if req.MaxTokens > c.maxSeen {
		c.maxSeen = req.MaxTokens
	}
	c.mu.Unlock()
	return llm.Response{
		Text:       c.text,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 5, OutputTokens: 7},
	}, nil
}

// maxTokensSeen reports the largest req.MaxTokens this fake has served. The
// negative test asserts it never exceeds the default cap — i.e. the doubled-cap
// retry never fired for a non-truncated parse failure.
func (c *rweNonJSONClient) maxTokensSeen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSeen
}

// rweBuildFunnel constructs the smallest Funnel that lets runFinderWithPrompt
// run end-to-end: the fixture store/repo and an unlimited token budget so
// the unit is not blocked by the shared pool. Mirrors openFixture +
// RoleClients in funnel_test.go; inlined so this file does not depend on
// test-only helpers that may move.
func rweBuildFunnel(t *testing.T, finder llm.Client) (*Funnel, *budgetState) {
	t.Helper()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: finder, Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatalf("funnel.New: %v", err)
	}
	rec := &spendRecorder{ctx: context.Background(), store: st}
	budget := newBudgetState(0, rec, 1.0) // budget=0 => unlimited, no pool, no pre-turn check
	return f, budget
}

// rweReadOnlyTools mirrors the existing test pattern
// (TestNewAgentRunner_AppliesStandardOptions at agent_runners_test.go:84):
// read-only tools are sufficient for a finder task on a fixture repo.
func rweReadOnlyTools(t *testing.T, f *Funnel) []agent.Tool {
	t.Helper()
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatalf("readOnlyTools: %v", err)
	}
	return tools
}

// TestRwe_RunFinderWithPrompt_RetryOnMaxTokensTruncation is the recovery
// acceptance for bugbot-rwe: a reasoning model that burns the per-completion
// output cap on its first attempt must recover via the doubled-cap retry so
// the unit does not silently emit zero candidates.
//
// The fake branches on req.MaxTokens. <=8192 returns a non-JSON think blob
// with StopMaxTokens (simulating a reasoning model that fills the cap before
// emitting JSON). >8192 returns the shared realCand fixture with StopEndTurn.
//
// Assertions:
//   - status == finderOK and len(cands) >= 1;
//   - the runner DID issue a request whose MaxTokens exceeded the default
//     cap (proves the doubling reached the wire, not just the in-process
//     runner option).
func TestRwe_RunFinderWithPrompt_RetryOnMaxTokensTruncation(t *testing.T) {
	ctx := context.Background()
	fake := newRweFinderFake(
		candJSON(realCand), // oversized JSON after doubling
		"okay let me think about this... no json here just <think> tokens", // under-cap truncated blob
	)
	f, budget := rweBuildFunnel(t, fake)
	tools := rweReadOnlyTools(t, f)

	tasks := finderTask([]string{"bug.go"}, nil, "")
	startedAt := time.Now()
	lens := f.lenses[0]
	scope := progress.NewAgentScope(nil, progress.RoleFinder, "rwe-find")
	cands, status, outcome, pm, err := f.runFinderWithPrompt(
		ctx, fake, tools, "you are a finder", "rwe-find", lens, tasks, budget, startedAt, scope,
	)
	if err != nil {
		t.Fatalf("runFinderWithPrompt err: %v", err)
	}
	if status != finderOK {
		t.Errorf("status = %d, want finderOK (%d) — the doubled-cap retry must rescue the unit", status, finderOK)
	}
	if len(cands) < 1 {
		t.Errorf("len(cands) = %d, want >= 1 (realCand should parse to one candidate)", len(cands))
	}
	if cands[0].File != "bug.go" {
		t.Errorf("first cand File = %q, want bug.go", cands[0].File)
	}
	if pm != nil {
		t.Errorf("pm = %+v, want nil on the success path", pm)
	}
	if outcome == nil {
		t.Fatalf("outcome is nil on the success path; caller uses it for token accounting")
	}

	// Wire-level assertion: at least one Complete call observed MaxTokens
	// larger than the default cap. AND exactly equal to the doubled
	// constant the production code uses (so a future change to the
	// doubling factor or to the apply-order story trips a test).
	if max := fake.rweMaxTokensSeen(); max <= DefaultMaxOutputTokens {
		t.Errorf("max observed MaxTokens = %d, want > %d (doubled cap must reach the wire)",
			max, DefaultMaxOutputTokens)
	}
	if max := fake.rweMaxTokensSeen(); max != finderRetryMaxOutputTokens {
		t.Errorf("max observed MaxTokens = %d, want exactly %d (2 * DefaultMaxOutputTokens)",
			max, finderRetryMaxOutputTokens)
	}
}

// TestRwe_RunFinderWithPrompt_NoRetryOnNonTruncatedParseFailure pins the
// negative side: when the parse failure is NOT caused by the per-completion
// cap (StopReason == StopEndTurn in this fake), the doubled-cap retry must
// not fire. A waste-of-budget retry would mask a real model problem behind
// "doubled the cap and still failed" without any signal that the original
// failure was a model output issue, not a token-cap issue.
//
// The rweNonJSONClient ignores MaxTokens entirely, so a wire-level
// MaxTokens gate cannot be measured here — the predicate test
// (TestRwe_ShouldRetryFinderCapPredicate below) covers the decision
// boundary directly. The integration assertion is purely about the
// runFinderWithPrompt classification: it must remain finderParseFailed.
func TestRwe_RunFinderWithPrompt_NoRetryOnNonTruncatedParseFailure(t *testing.T) {
	ctx := context.Background()
	fake := &rweNonJSONClient{
		text: "I am thinking but emit no JSON, no <think> tags, nothing parseable",
	}
	f, budget := rweBuildFunnel(t, fake)
	tools := rweReadOnlyTools(t, f)

	tasks := finderTask([]string{"bug.go"}, nil, "")
	startedAt := time.Now()
	lens := f.lenses[0]

	scope2 := progress.NewAgentScope(nil, progress.RoleFinder, "rwe-find")
	cands, status, _, pm, err := f.runFinderWithPrompt(
		ctx, fake, tools, "you are a finder", "rwe-find", lens, tasks, budget, startedAt, scope2,
	)
	if err != nil {
		t.Fatalf("runFinderWithPrompt err: %v", err)
	}
	if status != finderParseFailed {
		t.Errorf("status = %d, want finderParseFailed (%d) — non-truncated parse failure must remain a parse failure", status, finderParseFailed)
	}
	if len(cands) != 0 {
		t.Errorf("len(cands) = %d, want 0 on parse failure", len(cands))
	}
	if pm == nil {
		t.Errorf("pm = nil, want non-nil postmortem on the parse-failure path (finderPostmortemDetail is recorded by the caller)")
	}
	if max := fake.maxTokensSeen(); max > DefaultMaxOutputTokens {
		t.Errorf("max observed MaxTokens = %d, want <= %d — the doubled-cap retry must NOT fire on a non-truncated parse failure", max, DefaultMaxOutputTokens)
	}
}

// TestRwe_ShouldRetryFinderCapPredicate pins the retry decision boundary
// so the predicate's contract is locked independent of the integration
// cases above. Constructed outcomes drive every branch the predicate must
// reject; only the "StopMaxTokens outcome AND parse-failed status"
// combination should return true.
//
// This focused unit test stands in for a full budget-pool simulation in
// the budget-stopped branch (NEGATIVE 2). Spinning up a budget pool in a
// unit test requires NewBudgetPool + an agent.Limits{BudgetCheck:
// pool.Check} option plus a runner that actually reaches the pre-turn
// check — meaningfully more plumbing than the predicate boundary
// warrants, and the accept-or-reject decision is what bugbot-rwe pins.
func TestRwe_ShouldRetryFinderCapPredicate(t *testing.T) {
	makeOutcome := func(lastStop llm.StopReason, trunc bool, reason string) *agent.Outcome {
		o := &agent.Outcome{LastStopReason: lastStop}
		if trunc {
			o.Truncated = true
			o.TruncationReason = reason
		}
		return o
	}

	cases := []struct {
		name    string
		status  finderStatus
		outcome *agent.Outcome
		err     error
		want    bool
		why     string
	}{
		{
			name:    "stop-max-tokens parse-failed => retry",
			status:  finderParseFailed,
			outcome: makeOutcome(llm.StopMaxTokens, false, ""),
			err:     errors.New("parse error"),
			want:    true,
			why:     "the canonical signal: cap-truncated final completion + parse-failure classification",
		},
		{
			name:    "stop-end-turn parse-failed => no retry",
			status:  finderParseFailed,
			outcome: makeOutcome(llm.StopEndTurn, false, ""),
			err:     errors.New("parse error"),
			want:    false,
			why:     "model produced malformed JSON without hitting the cap; retry would not help and would mask the real failure",
		},
		{
			name:    "budget-stopped status => no retry",
			status:  finderBudgetStopped,
			outcome: makeOutcome(llm.StopMaxTokens, true, agent.TruncTokenBudget),
			err:     nil,
			want:    false,
			why:     "budget stops are not parse failures; the retried unit would never reach this branch in practice",
		},
		{
			name:    "rate-limited status => no retry",
			status:  finderRateLimited,
			outcome: makeOutcome(llm.StopMaxTokens, false, ""),
			err:     errors.New("429 too many requests"),
			want:    false,
			why:     "rate-limit exhaustion is recoverable by lowering --concurrency; retrying the same path is wasteful",
		},
		{
			name:    "nil outcome => no retry",
			status:  finderParseFailed,
			outcome: nil,
			err:     errors.New("no outcome"),
			want:    false,
			why:     "no LastStopReason means no canonical cap-truncation signal; safer to skip",
		},
		{
			name:    "budget-stopped outcome + parse-failed status => status guard is authoritative",
			status:  finderParseFailed, // hypothetical: defense-in-depth guard fires
			outcome: makeOutcome(llm.StopMaxTokens, true, agent.TruncBudgetPool),
			err:     errors.New("parse error"),
			want:    false,
			why:     "budgetStopped guard rejects even when status was mis-routed; belt-and-suspenders",
		},
		{
			name:    "stop-max-tokens parse-failed but rate-limited err => no retry",
			status:  finderParseFailed,
			outcome: makeOutcome(llm.StopMaxTokens, false, ""),
			err:     fmt.Errorf("provider 429: %w", llm.ErrRateLimited),
			want:    false,
			why:     "a wrapped rate-limit sentinel must be rejected by the final guard even on the cap-truncation path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRetryFinderCap(tc.status, tc.outcome, tc.err)
			if got != tc.want {
				t.Errorf("shouldRetryFinderCap = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}
