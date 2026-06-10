package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

// bigSpendClient is a scripted llm.Client that reports a large, fixed Usage on
// every completion and always requests a tool so the loop never finishes on its
// own — it can only be stopped by a limit or the shared budget pool. Every
// completion charges the pool, mirroring how the funnel's recorder feeds it.
type bigSpendClient struct {
	pool      *BudgetPool
	perCall   int64 // input+output tokens reported (and charged) per completion
	callCount atomic.Int64
}

func (c *bigSpendClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *bigSpendClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.callCount.Add(1)
	// Charge the pool exactly as the funnel's spend recorder would, on the same
	// completion the model "spent" the tokens. Split across input/output to mimic
	// the input-dominated reality from the calibration run.
	in := c.perCall - c.perCall/10
	out := c.perCall - in
	c.pool.Add(in + out)
	return llm.Response{
		StopReason: llm.StopToolUse,
		ToolCalls:  []llm.ToolCall{{ID: "c", Name: "noop", Arguments: []byte(`{}`)}},
		Usage:      llm.Usage{InputTokens: in, OutputTokens: out},
	}, nil
}

// noopTool always succeeds, keeping the loop turning until a budget stops it.
type noopTool struct{}

func (noopTool) Def() llm.ToolDef {
	return llm.ToolDef{Name: "noop", Description: "noop", Parameters: []byte(`{"type":"object"}`)}
}
func (noopTool) Run(ctx context.Context, args json.RawMessage) (string, error) { return "ok", nil }

// TestBudgetPool_OvershootBound is the acceptance test for shared-pool budget
// enforcement: with a pool of B tokens, P concurrent runners that each report a
// large per-call spend, and a pre-turn BudgetCheck hook, total spend must never
// exceed B plus at most one in-flight model-call per runner. This is the bound
// the runaway calibration run violated (2.7x overshoot) because the gate only
// stopped launches, not in-flight turns.
func TestBudgetPool_OvershootBound(t *testing.T) {
	const (
		budget   int64 = 1_000_000
		perCall  int64 = 150_000 // each turn is a big chunk of the budget
		parallel       = 4
	)

	pool := NewBudgetPool(budget)

	// Each runner consults the shared pool pre-turn and has effectively unlimited
	// per-run budget/iterations, so ONLY the shared pool can stop it. This is the
	// adversarial case: without the pre-turn check, every runner would run to its
	// own limit and blow far past B.
	limits := Limits{
		MaxIterations: -1,
		TokenBudget:   -1,
		BudgetCheck:   pool.Check,
	}

	var wg sync.WaitGroup
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &bigSpendClient{pool: pool, perCall: perCall}
			r := NewRunner(client, []Tool{noopTool{}}, "sys", WithLimits(limits))
			out, err := r.Run(context.Background(), "task")
			if err != nil {
				t.Errorf("Run: %v", err)
				return
			}
			if !out.Truncated || out.TruncationReason != TruncBudgetPool {
				t.Errorf("expected TruncBudgetPool stop, got truncated=%v reason=%q",
					out.Truncated, out.TruncationReason)
			}
		}()
	}
	wg.Wait()

	// The bound: total spend <= B + (one in-flight model-call * P). Each runner
	// can be mid-turn when the pool tips over the limit, so each may contribute at
	// most one extra perCall beyond the shared ceiling.
	total := pool.Spent()
	upperBound := budget + perCall*int64(parallel)
	if total > upperBound {
		t.Fatalf("overshoot exceeded bound: spent=%d, bound=B(%d)+P*perCall(%d)=%d",
			total, budget, perCall*int64(parallel), upperBound)
	}
	// Sanity: the pool must actually have been driven past the budget (otherwise
	// the bound is trivially satisfied by under-spending).
	if total < budget {
		t.Fatalf("pool never reached budget: spent=%d < B=%d", total, budget)
	}
	t.Logf("spent=%d within bound %d (budget=%d, overshoot=%d, %.2fx)",
		total, upperBound, budget, total-budget, float64(total)/float64(budget))
}

// TestBudgetPool_StopsInFlight verifies a single in-flight runner stops at the
// next turn boundary once the pool is exhausted, bounding its own overshoot to
// one model-call.
func TestBudgetPool_StopsInFlight(t *testing.T) {
	const budget int64 = 500_000
	const perCall int64 = 60_000
	pool := NewBudgetPool(budget)

	client := &bigSpendClient{pool: pool, perCall: perCall}
	r := NewRunner(client, []Tool{noopTool{}}, "sys", WithLimits(Limits{
		MaxIterations: -1, TokenBudget: -1, BudgetCheck: pool.Check,
	}))
	out, err := r.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.TruncationReason != TruncBudgetPool {
		t.Fatalf("reason = %q, want %q", out.TruncationReason, TruncBudgetPool)
	}
	// A single runner overshoots by at most one perCall: it issues calls until the
	// pre-turn check sees the pool at/over budget, which happens at most one call
	// after crossing.
	if got, want := pool.Spent(), budget+perCall; got > want {
		t.Fatalf("single-runner overshoot = %d, want <= %d", got, want)
	}
}

// TestBudgetPool_Unlimited confirms a non-positive limit never stops a run.
func TestBudgetPool_Unlimited(t *testing.T) {
	pool := NewBudgetPool(0)
	if err := pool.Check(); err != nil {
		t.Fatalf("unlimited pool Check = %v, want nil", err)
	}
	pool.Add(1 << 40)
	if err := pool.Check(); err != nil {
		t.Fatalf("unlimited pool Check after huge Add = %v, want nil", err)
	}
	if rem := pool.Remaining(); rem <= 0 {
		t.Fatalf("unlimited pool Remaining = %d, want large positive sentinel", rem)
	}
}
