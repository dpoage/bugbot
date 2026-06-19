package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// blockingClient blocks each Complete call until its context is done, then
// returns ctx.Err(). The blockUntil slice lets a per-attempt blocking call be
// configured independently from later (succeeding) attempts.
type blockingClient struct {
	// blockAttempts is the number of leading attempts that block until their
	// (per-attempt) context expires. Attempts beyond this succeed.
	blockAttempts int
	calls         int
}

func (b *blockingClient) Capabilities() Capabilities { return Capabilities{} }

func (b *blockingClient) Complete(ctx context.Context, req Request) (Response, error) {
	i := b.calls
	b.calls++
	if i < b.blockAttempts {
		<-ctx.Done()
		// Mimic an adapter that received a transport/timeout error it could not
		// classify: it returns the raw context error (bare context.DeadlineExceeded).
		return Response{}, ctx.Err()
	}
	return Response{Text: "ok", StopReason: StopEndTurn}, nil
}

// TestRetry_PerAttemptTimeout_ReturnsFast asserts that a single attempt whose
// inner Complete never returns is bounded by RequestTimeout: Complete returns
// within roughly the timeout (times MaxAttempts) rather than blocking forever,
// and surfaces a deadline-exceeded-flavored error.
func TestRetry_PerAttemptTimeout_ReturnsFast(t *testing.T) {
	inner := &blockingClient{blockAttempts: 100} // always blocks
	cfg := DefaultRetryConfig()
	cfg.MaxAttempts = 2
	cfg.RequestTimeout = 50 * time.Millisecond
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return nil } // skip backoff

	client := WithRetry(inner, cfg)

	done := make(chan struct{})
	var err error
	go func() {
		_, err = client.Complete(context.Background(), simpleRequest())
		close(done)
	}()

	// 2 attempts * 50ms timeout, generous slack for scheduling.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Complete did not return; per-attempt timeout failed to bound a blocked request")
	}

	if err == nil {
		t.Fatal("expected an error from a perpetually-blocking client")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want 2 (timed-out attempt is retried up to MaxAttempts)", inner.calls)
	}
}

// TestRetry_PerAttemptTimeout_IsRetryable asserts that a per-attempt timeout
// does NOT abort the retry loop: attempt 1 blocks (times out), attempt 2
// succeeds, and Complete returns the success.
func TestRetry_PerAttemptTimeout_IsRetryable(t *testing.T) {
	inner := &blockingClient{blockAttempts: 1} // first attempt blocks, then succeeds
	cfg := DefaultRetryConfig()
	cfg.MaxAttempts = 3
	cfg.RequestTimeout = 50 * time.Millisecond
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	client := WithRetry(inner, cfg)
	resp, err := client.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v, want success after a timed-out first attempt", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want 2 (timeout retried, second attempt succeeded)", inner.calls)
	}
}

// TestRetry_PerAttemptTimeout_WrappedAsServerError covers the production path
// where an adapter wraps an unclassified context-deadline error into an
// *APIError{Kind: ErrServer} (as openaiAdapter.normalizeErr does). That is
// retryable, so the loop must still recover on a later success.
func TestRetry_PerAttemptTimeout_WrappedAsServerError(t *testing.T) {
	inner := &wrappingBlockClient{blockAttempts: 1}
	cfg := DefaultRetryConfig()
	cfg.MaxAttempts = 3
	cfg.RequestTimeout = 50 * time.Millisecond
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	client := WithRetry(inner, cfg)
	resp, err := client.Complete(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Complete: %v, want success after a wrapped timeout", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if inner.calls != 2 {
		t.Errorf("calls = %d, want 2", inner.calls)
	}
}

// wrappingBlockClient blocks then returns the timeout wrapped as an *APIError,
// matching how the openai adapter normalizes an unrecognized transport error.
type wrappingBlockClient struct {
	blockAttempts int
	calls         int
}

func (w *wrappingBlockClient) Capabilities() Capabilities { return Capabilities{} }

func (w *wrappingBlockClient) Complete(ctx context.Context, req Request) (Response, error) {
	i := w.calls
	w.calls++
	if i < w.blockAttempts {
		<-ctx.Done()
		return Response{}, newAPIError("fake", 0, 0, ErrServer, ctx.Err().Error(), ctx.Err())
	}
	return Response{Text: "ok", StopReason: StopEndTurn}, nil
}

// TestRetry_ParentCancelStillAbortsImmediately asserts the existing semantics
// are preserved: when the CALLER'S context is cancelled mid-attempt, Complete
// aborts and does NOT retry, even though the per-attempt timeout machinery now
// derives a child context. This is the subtle correctness point — a cancelled
// parent must not be confused with a per-attempt deadline.
func TestRetry_ParentCancelStillAbortsImmediately(t *testing.T) {
	inner := &blockingClient{blockAttempts: 100} // always blocks until ctx done
	cfg := DefaultRetryConfig()
	cfg.MaxAttempts = 4
	cfg.RequestTimeout = 10 * time.Second // long: the parent cancel must win, not the timeout
	cfg.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	client := WithRetry(inner, cfg)

	// Cancel the parent shortly after the (blocking) first attempt begins.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	var err error
	go func() {
		_, err = client.Complete(ctx, simpleRequest())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Complete did not return after parent cancellation")
	}

	if err == nil {
		t.Fatal("expected an error after parent cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled (parent cancellation)", err)
	}
	if inner.calls != 1 {
		t.Errorf("calls = %d, want 1 (parent cancellation must abort, not retry)", inner.calls)
	}
}

// TestWithRetry_DefaultsRequestTimeout asserts the backstop: a caller-supplied
// RetryConfig that leaves RequestTimeout zero still gets DefaultRequestTimeout,
// so no construction path can produce a client with an unbounded request.
func TestWithRetry_DefaultsRequestTimeout(t *testing.T) {
	rc, ok := WithRetry(&fakeClient{}, RetryConfig{MaxAttempts: 2}).(*retryClient)
	if !ok {
		t.Fatal("WithRetry did not return a *retryClient")
	}
	if rc.cfg.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("RequestTimeout = %v, want default %v", rc.cfg.RequestTimeout, DefaultRequestTimeout)
	}
}

// TestWithRetry_JitterClamped verifies that WithRetry clamps Jitter to [0,1].
func TestWithRetry_JitterClamped(t *testing.T) {
	tests := []struct {
		name  string
		input float64
		want  float64
	}{
		{"negative clamped to 0", -0.5, 0},
		{"above 1 clamped to 1", 1.5, 1},
		{"zero preserved", 0, 0},
		{"one preserved", 1, 1},
		{"valid mid-range preserved", 0.2, 0.2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, ok := WithRetry(&fakeClient{}, RetryConfig{MaxAttempts: 1, Jitter: tc.input}).(*retryClient)
			if !ok {
				t.Fatal("WithRetry did not return a *retryClient")
			}
			if rc.cfg.Jitter != tc.want {
				t.Errorf("Jitter = %v, want %v", rc.cfg.Jitter, tc.want)
			}
		})
	}
}
