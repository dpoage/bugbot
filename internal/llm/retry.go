package llm

import (
	"context"
	"math/rand"
	"time"
)

// RetryConfig tunes the shared retry wrapper. The zero value is not usable;
// callers should start from DefaultRetryConfig.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts (initial try + retries). Must
	// be >= 1.
	MaxAttempts int
	// BaseDelay is the first backoff interval; subsequent delays grow
	// exponentially.
	BaseDelay time.Duration
	// MaxDelay caps any single backoff interval.
	MaxDelay time.Duration
	// Jitter, in [0,1], is the fraction of each delay randomized to avoid
	// thundering herds. 0.2 means the delay is multiplied by a random factor in
	// [0.8, 1.2].
	Jitter float64

	// RequestTimeout is the per-attempt wall-clock deadline applied to each
	// inner Complete call. It exists to bound a single provider request: a
	// stalled HTTP round-trip that never returns would otherwise block a finder
	// goroutine forever and hang the whole scan (the parent context carries no
	// per-call deadline). Zero or negative means DefaultRequestTimeout.
	//
	// The timeout is applied to a child context derived from the caller's ctx,
	// so it expires as context.DeadlineExceeded on the ATTEMPT only — never as
	// cancellation of the parent. A timed-out attempt is treated as a transient
	// transport failure and is retried (see Complete).
	RequestTimeout time.Duration

	// sleep is an injection point for tests; nil uses a real timer.
	sleep func(ctx context.Context, d time.Duration) error
	// rng is an injection point for deterministic jitter in tests; nil uses a
	// package-level source.
	rng func() float64
}

// DefaultRequestTimeout bounds a single LLM request attempt. It is the
// backstop against a provider that accepts a request and then never responds:
// the in-flight HTTP round-trip is aborted via context, the attempt fails with
// context.DeadlineExceeded, and the retry loop tries again rather than blocking
// a finder/refuter/arbiter goroutine indefinitely (which previously hung an
// entire scan at the funnel's wg.Wait()).
//
// 5 minutes is chosen to comfortably exceed a legitimate slow completion —
// large reasoning-model responses with big output budgets can take a couple of
// minutes — while still being far below the multi-minute, no-progress stalls
// that motivated this fix. It is per-attempt, so the worst case before giving
// up entirely is roughly MaxAttempts * (RequestTimeout + backoff).
const DefaultRequestTimeout = 5 * time.Minute

// DefaultRetryConfig returns sensible defaults: 4 attempts, 500ms base,
// 30s cap, 20% jitter, and a 5m per-attempt request timeout.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    4,
		BaseDelay:      500 * time.Millisecond,
		MaxDelay:       30 * time.Second,
		Jitter:         0.2,
		RequestTimeout: DefaultRequestTimeout,
	}
}

// retryClient wraps a Client with exponential-backoff-with-jitter retries on
// transient failures (429/5xx/timeouts), honoring Retry-After when present.
type retryClient struct {
	inner Client
	cfg   RetryConfig
}

// WithRetry wraps c so that Complete retries transient failures per cfg.
// Capabilities is delegated unchanged. Wrapping is composable with WithRecorder
// and WithSerializedToolCalls.
func WithRetry(c Client, cfg RetryConfig) Client {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	return &retryClient{inner: c, cfg: cfg}
}

func (r *retryClient) Capabilities() Capabilities { return r.inner.Capabilities() }

func (r *retryClient) Complete(ctx context.Context, req Request) (Response, error) {
	var lastErr error
	for attempt := 0; attempt < r.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := r.backoff(attempt, lastErr)
			if err := r.doSleep(ctx, delay); err != nil {
				// Context cancelled while waiting; surface the last real error if
				// we have one, otherwise the context error.
				if lastErr != nil {
					return Response{}, lastErr
				}
				return Response{}, err
			}
		}

		resp, err := r.attempt(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Never retry once the CALLER'S context is done. We check the parent ctx
		// (not the per-attempt child) so a per-attempt RequestTimeout — which
		// expires only the child as context.DeadlineExceeded — is NOT mistaken
		// for parent cancellation. Parent cancellation/deadline aborts the loop
		// here; a stalled-request timeout falls through to the retryable check
		// below and is retried like any other transient transport failure.
		if ctx.Err() != nil {
			return Response{}, err
		}
		if !isRetryable(err) {
			return Response{}, err
		}
	}
	return Response{}, lastErr
}

// attempt runs one inner Complete under a per-attempt wall-clock deadline
// derived from ctx. The child context bounds a single provider request so a
// stalled round-trip (server accepts the request then never responds) aborts as
// context.DeadlineExceeded instead of blocking forever. cancel is always called
// before returning, so the timer is released whether the call succeeds, fails,
// or times out — and it does not leak across loop iterations (no defer-in-loop).
//
// Because the deadline lives on the child, parent cancellation semantics are
// untouched: if the parent ctx is cancelled the child is too, but the caller's
// loop distinguishes the two by inspecting the parent ctx.Err() directly.
func (r *retryClient) attempt(ctx context.Context, req Request) (Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	return r.inner.Complete(attemptCtx, req)
}

// backoff computes the delay before the given attempt (1-indexed for the first
// retry). It prefers a server-supplied Retry-After when present, otherwise uses
// exponential backoff with jitter.
func (r *retryClient) backoff(attempt int, lastErr error) time.Duration {
	if d, ok := retryAfter(lastErr); ok {
		if d > r.cfg.MaxDelay && r.cfg.MaxDelay > 0 {
			return r.cfg.MaxDelay
		}
		return d
	}

	// Exponential: base * 2^(attempt-1).
	delay := r.cfg.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if r.cfg.MaxDelay > 0 && delay >= r.cfg.MaxDelay {
			delay = r.cfg.MaxDelay
			break
		}
	}
	if r.cfg.MaxDelay > 0 && delay > r.cfg.MaxDelay {
		delay = r.cfg.MaxDelay
	}

	if r.cfg.Jitter > 0 {
		// factor in [1-jitter, 1+jitter].
		factor := 1 + r.cfg.Jitter*(2*r.random()-1)
		delay = time.Duration(float64(delay) * factor)
	}
	return delay
}

func (r *retryClient) random() float64 {
	if r.cfg.rng != nil {
		return r.cfg.rng()
	}
	return rand.Float64()
}

func (r *retryClient) doSleep(ctx context.Context, d time.Duration) error {
	if r.cfg.sleep != nil {
		return r.cfg.sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
