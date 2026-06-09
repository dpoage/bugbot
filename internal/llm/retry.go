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

	// sleep is an injection point for tests; nil uses a real timer.
	sleep func(ctx context.Context, d time.Duration) error
	// rng is an injection point for deterministic jitter in tests; nil uses a
	// package-level source.
	rng func() float64
}

// DefaultRetryConfig returns sensible defaults: 4 attempts, 500ms base,
// 30s cap, 20% jitter.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 4,
		BaseDelay:   500 * time.Millisecond,
		MaxDelay:    30 * time.Second,
		Jitter:      0.2,
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

		resp, err := r.inner.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Never retry once the caller's context is done.
		if ctx.Err() != nil {
			return Response{}, err
		}
		if !isRetryable(err) {
			return Response{}, err
		}
	}
	return Response{}, lastErr
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
