package llm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClient is a programmable Client for testing the wrappers in isolation.
type fakeClient struct {
	caps      Capabilities
	responses []Response
	errs      []error
	calls     int
}

func (f *fakeClient) Capabilities() Capabilities { return f.caps }

func (f *fakeClient) Complete(ctx context.Context, req Request) (Response, error) {
	i := f.calls
	f.calls++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	var resp Response
	if i < len(f.responses) {
		resp = f.responses[i]
	}
	return resp, err
}

func rateLimitErr(retryAfter time.Duration) error {
	return &APIError{Kind: ErrRateLimited, StatusCode: 429, RetryAfter: retryAfter, Provider: "fake", Message: "429"}
}

func TestRetry_SucceedsAfterRateLimit(t *testing.T) {
	fc := &fakeClient{
		errs:      []error{rateLimitErr(0), rateLimitErr(0), nil},
		responses: []Response{{}, {}, {Text: "ok", StopReason: StopEndTurn}},
	}
	var slept []time.Duration
	cfg := DefaultRetryConfig()
	cfg.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	cfg.rng = func() float64 { return 0.5 } // deterministic: jitter factor = 1.0

	client := WithRetry(fc, cfg)
	resp, err := client.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if fc.calls != 3 {
		t.Errorf("calls = %d, want 3", fc.calls)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(slept))
	}
	// Exponential backoff: 500ms, then 1s (jitter factor 1.0).
	if slept[0] != 500*time.Millisecond {
		t.Errorf("slept[0] = %v, want 500ms", slept[0])
	}
	if slept[1] != 1*time.Second {
		t.Errorf("slept[1] = %v, want 1s", slept[1])
	}
}

func TestRetry_HonorsRetryAfter(t *testing.T) {
	fc := &fakeClient{
		errs:      []error{rateLimitErr(3 * time.Second), nil},
		responses: []Response{{}, {Text: "ok"}},
	}
	var slept []time.Duration
	cfg := DefaultRetryConfig()
	cfg.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }

	client := WithRetry(fc, cfg)
	if _, err := client.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Errorf("slept = %v, want [3s] (Retry-After honored over backoff)", slept)
	}
}

func TestRetry_DoesNotRetryAuth(t *testing.T) {
	authErr := &APIError{Kind: ErrAuth, StatusCode: 401, Provider: "fake", Message: "401"}
	fc := &fakeClient{errs: []error{authErr}}
	cfg := DefaultRetryConfig()
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	client := WithRetry(fc, cfg)
	_, err := client.Complete(context.Background(), simpleRequest())
	if !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
	if fc.calls != 1 {
		t.Errorf("calls = %d, want 1 (auth is not retryable)", fc.calls)
	}
}

func TestRetry_ExhaustsAttempts(t *testing.T) {
	fc := &fakeClient{errs: []error{rateLimitErr(0), rateLimitErr(0), rateLimitErr(0), rateLimitErr(0)}}
	cfg := DefaultRetryConfig() // 4 attempts
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	client := WithRetry(fc, cfg)
	_, err := client.Complete(context.Background(), simpleRequest())
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	if fc.calls != 4 {
		t.Errorf("calls = %d, want 4", fc.calls)
	}
}

func TestRetry_StopsOnContextCancel(t *testing.T) {
	fc := &fakeClient{errs: []error{rateLimitErr(0), rateLimitErr(0)}}
	cfg := DefaultRetryConfig()
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return context.Canceled }

	client := WithRetry(fc, cfg)
	_, err := client.Complete(context.Background(), simpleRequest())
	// The first attempt fails (retryable), the backoff sleep returns
	// context.Canceled, so we surface the last real error.
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want last ErrRateLimited", err)
	}
	if fc.calls != 1 {
		t.Errorf("calls = %d, want 1", fc.calls)
	}
}

func TestRecorder_RecordsUsageOnSuccess(t *testing.T) {
	fc := &fakeClient{responses: []Response{{Text: "x", Usage: Usage{InputTokens: 100, OutputTokens: 50}}}}
	var mu sync.Mutex
	var events []UsageEvent
	rec := RecorderFunc(func(ev UsageEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	client := WithRecorder(fc, rec, "finder", "anthropic", "claude-test")
	if _, err := client.Complete(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Role != "finder" || ev.Provider != "anthropic" || ev.Model != "claude-test" {
		t.Errorf("event tags = %+v", ev)
	}
	if ev.Usage.InputTokens != 100 || ev.Usage.OutputTokens != 50 {
		t.Errorf("event usage = %+v, want {100 50}", ev.Usage)
	}
}

func TestRecorder_NoRecordOnError(t *testing.T) {
	fc := &fakeClient{errs: []error{errors.New("boom")}}
	var count int
	rec := RecorderFunc(func(ev UsageEvent) { count++ })

	client := WithRecorder(fc, rec, "finder", "p", "m")
	if _, err := client.Complete(context.Background(), simpleRequest()); err == nil {
		t.Fatal("expected error")
	}
	if count != 0 {
		t.Errorf("recorded %d events on error, want 0", count)
	}
}

func TestSerialize_TruncatesParallelToolCalls(t *testing.T) {
	fc := &fakeClient{
		caps: Capabilities{ParallelToolCalls: false},
		responses: []Response{{
			ToolCalls: []ToolCall{
				{ID: "1", Name: "a", Arguments: json.RawMessage(`{}`)},
				{ID: "2", Name: "b", Arguments: json.RawMessage(`{}`)},
				{ID: "3", Name: "c", Arguments: json.RawMessage(`{}`)},
			},
			StopReason: StopToolUse,
		}},
	}
	client := WithSerializedToolCalls(fc)
	resp, err := client.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "1" {
		t.Errorf("kept tool call ID = %q, want first (1)", resp.ToolCalls[0].ID)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if client.Capabilities().ParallelToolCalls {
		t.Error("wrapped Capabilities should report ParallelToolCalls=false")
	}
}

func TestSerialize_NoOpForParallelCapableClient(t *testing.T) {
	fc := &fakeClient{caps: Capabilities{ParallelToolCalls: true}}
	client := WithSerializedToolCalls(fc)
	// Must return the same client unchanged (no wrapping).
	if _, ok := client.(*serializingClient); ok {
		t.Error("parallel-capable client was wrapped; want passthrough")
	}
}
