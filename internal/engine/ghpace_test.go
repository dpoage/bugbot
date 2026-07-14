package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for pacedGH.now, so pacing tests
// don't depend on wall-clock time.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// sleepRecorder is a fake pacedGH.sleep that records every requested
// duration instead of actually sleeping, and advances an attached fakeClock
// (if any) by the same amount so pace() bookkeeping observes elapsed time
// consistently. It can be told to return ctx.Err() instead of sleeping, to
// simulate a cancelled context aborting a pending sleep.
type sleepRecorder struct {
	clock     *fakeClock
	durations []time.Duration
	cancelled bool // if true, every sleep call returns ctx.Err() without recording
}

func (r *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.cancelled {
		return ctx.Err()
	}
	r.durations = append(r.durations, d)
	if r.clock != nil {
		r.clock.advance(d)
	}
	return nil
}

// newTestPacedGH builds a pacedGH with injected sleep/clock, bypassing the
// exported NewPacedGH constructor so tests can reach the unexported fields
// directly.
func newTestPacedGH(inner GHRunner, rec *sleepRecorder, clock *fakeClock) *pacedGH {
	return &pacedGH{
		inner: inner,
		sleep: rec.sleep,
		now:   clock.now,
	}
}

func mutatingCall(method string) []string {
	return []string{"api", "repos/{owner}/{repo}/issues", "-X", method, "-f", "title=x"}
}

func readCall() []string {
	return []string{"api", "repos/{owner}/{repo}/issues/5"}
}

// TestPacedGH_MutatingCallsArePaced confirms two consecutive mutating calls
// through the same instance insert a ~1s pacing wait between them.
func TestPacedGH_MutatingCallsArePaced(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	calls := 0
	inner := func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		return []byte("ok"), nil
	}
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	if _, err := gh(context.Background(), mutatingCall("POST")...); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(rec.durations) != 0 {
		t.Fatalf("first mutating call should not be paced, got sleeps %v", rec.durations)
	}

	// Second mutating call immediately after: must wait ~1s.
	if _, err := gh(context.Background(), mutatingCall("PATCH")...); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(rec.durations) != 1 {
		t.Fatalf("expected exactly one pacing sleep, got %v", rec.durations)
	}
	if rec.durations[0] != ghMutationPaceInterval {
		t.Errorf("pacing wait = %v, want %v", rec.durations[0], ghMutationPaceInterval)
	}
	if calls != 2 {
		t.Errorf("inner calls = %d, want 2", calls)
	}
}

// TestPacedGH_MutatingCallSpacedOutNaturallySkipsWait confirms that if enough
// wall-clock time already elapsed between mutating calls (as tracked by the
// injected clock), no additional pacing sleep is inserted.
func TestPacedGH_MutatingCallSpacedOutNaturallySkipsWait(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	inner := func(_ context.Context, _ ...string) ([]byte, error) { return []byte("ok"), nil }
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	if _, err := gh(context.Background(), mutatingCall("POST")...); err != nil {
		t.Fatalf("first call: %v", err)
	}
	clock.advance(2 * time.Second) // plenty of natural spacing
	if _, err := gh(context.Background(), mutatingCall("PATCH")...); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(rec.durations) != 0 {
		t.Errorf("expected no pacing sleep when calls are already spaced out, got %v", rec.durations)
	}
}

// TestPacedGH_ReadsAreNotPaced confirms GET calls (no -X) never trigger a
// pacing wait, even back-to-back with no clock advance, and even
// interspersed with a prior mutating call.
func TestPacedGH_ReadsAreNotPaced(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	inner := func(_ context.Context, _ ...string) ([]byte, error) { return []byte("ok"), nil }
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	if _, err := gh(context.Background(), mutatingCall("POST")...); err != nil {
		t.Fatalf("mutating call: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := gh(context.Background(), readCall()...); err != nil {
			t.Fatalf("read call %d: %v", i, err)
		}
	}
	if len(rec.durations) != 0 {
		t.Errorf("read calls must never be paced, got sleeps %v", rec.durations)
	}
}

// TestPacedGH_RetriesRateLimitThenSucceeds confirms a rate-limited error
// retries per the fixed backoff schedule and returns the eventual success.
func TestPacedGH_RetriesRateLimitThenSucceeds(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	attempts := 0
	inner := func(_ context.Context, _ ...string) ([]byte, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("gh api ...: exit status 1: HTTP 403: You have exceeded a secondary rate limit")
		}
		return []byte("ok"), nil
	}
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	out, err := gh(context.Background(), readCall()...)
	if err != nil {
		t.Fatalf("expected eventual success, got err: %v", err)
	}
	if string(out) != "ok" {
		t.Errorf("output = %q, want ok", out)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	wantSleeps := []time.Duration{ghRetryBackoff[0], ghRetryBackoff[1]}
	if len(rec.durations) != len(wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", rec.durations, wantSleeps)
	}
	for i, d := range wantSleeps {
		if rec.durations[i] != d {
			t.Errorf("sleep[%d] = %v, want %v", i, rec.durations[i], d)
		}
	}
}

// TestPacedGH_ExhaustedRetriesWrapSentinel confirms that once all retries are
// exhausted, the returned error satisfies errors.Is(err, ErrGHRateLimited)
// and still carries the last underlying gh error text.
func TestPacedGH_ExhaustedRetriesWrapSentinel(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	attempts := 0
	underlying := errors.New("gh api ...: exit status 1: HTTP 403: submitted too quickly")
	inner := func(_ context.Context, _ ...string) ([]byte, error) {
		attempts++
		return nil, underlying
	}
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	_, err := gh(context.Background(), readCall()...)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !errors.Is(err, ErrGHRateLimited) {
		t.Errorf("errors.Is(err, ErrGHRateLimited) = false, err: %v", err)
	}
	if got := attempts; got != len(ghRetryBackoff)+1 {
		t.Errorf("attempts = %d, want %d (4 total)", got, len(ghRetryBackoff)+1)
	}
	if len(rec.durations) != len(ghRetryBackoff) {
		t.Errorf("sleeps = %v, want %d entries matching schedule %v", rec.durations, len(ghRetryBackoff), ghRetryBackoff)
	}
	for i, d := range ghRetryBackoff {
		if rec.durations[i] != d {
			t.Errorf("sleep[%d] = %v, want %v", i, rec.durations[i], d)
		}
	}
	if !contains(err.Error(), "submitted too quickly") {
		t.Errorf("error should retain underlying gh text, got: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// TestPacedGH_BareAuthErrorNotRetried confirms a bare-403 auth failure (no
// rate-limit phrase) is returned immediately with no retry and no sleep —
// the same trap publish_test.go's TestIsGHGoneOrNotFound guards against for
// isGHGoneOrNotFound, mirrored here for the rate-limit classifier.
func TestPacedGH_BareAuthErrorNotRetried(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	attempts := 0
	authErr := errors.New("gh api ...: exit status 1: HTTP 403: Must have admin rights")
	inner := func(_ context.Context, _ ...string) ([]byte, error) {
		attempts++
		return nil, authErr
	}
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	_, err := gh(context.Background(), readCall()...)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if !errors.Is(err, authErr) {
		t.Errorf("expected original auth error to propagate unwrapped-ish, got: %v", err)
	}
	if errors.Is(err, ErrGHRateLimited) {
		t.Errorf("bare 403 auth error must not classify as rate limited")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry)", attempts)
	}
	if len(rec.durations) != 0 {
		t.Errorf("expected no sleep for non-rate-limit error, got %v", rec.durations)
	}
}

// TestPacedGH_CancelledContextAbortsPendingSleep confirms a cancelled ctx
// aborts a pending pacing/backoff sleep and the call returns promptly with
// the context error, rather than the sleep recorder's duration being
// honored.
func TestPacedGH_CancelledContextAbortsPendingSleep(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock, cancelled: true}
	inner := func(_ context.Context, _ ...string) ([]byte, error) {
		return nil, errors.New("gh api ...: HTTP 403: secondary rate limit")
	}
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := gh(ctx, mutatingCall("POST")...)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected wrapped context.Canceled, got: %v", err)
	}
}

// TestPacedGH_CancelledContextAbortsPendingPacingWait confirms pacing waits
// (not just retry backoff) are also aborted by a cancelled context.
func TestPacedGH_CancelledContextAbortsPendingPacingWait(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	rec := &sleepRecorder{clock: clock}
	inner := func(_ context.Context, _ ...string) ([]byte, error) { return []byte("ok"), nil }
	p := newTestPacedGH(inner, rec, clock)
	gh := p.run

	if _, err := gh(context.Background(), mutatingCall("POST")...); err != nil {
		t.Fatalf("first call: %v", err)
	}

	rec.cancelled = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := gh(ctx, mutatingCall("PATCH")...)
	if err == nil {
		t.Fatal("expected error from cancelled context during pacing wait")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected wrapped context.Canceled, got: %v", err)
	}
}

// TestIsGHRateLimited table-tests the classifier against every documented
// phrase plus two traps: the bare-403-with-issue-#404 case (mirroring
// internal/cli/publish_test.go:1584's isGHGoneOrNotFound rigor) and a
// permanent failure against an issue/PR literally numbered 429 — RealGH
// folds the full gh invocation (including the target issue number) into the
// returned error text, so "issues/429" must never be misread as an HTTP 429
// status.
func TestIsGHRateLimited(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sentinel", ErrGHRateLimited, true},
		{"wrapped-sentinel", fmt.Errorf("gh: rate limited after 4 attempt(s): %w", ErrGHRateLimited), true},
		{"secondary-rate-limit", errors.New("HTTP 403: You have exceeded a secondary rate limit"), true},
		{"abuse-detection", errors.New("HTTP 403: triggered an abuse detection mechanism"), true},
		{"submitted-too-quickly", errors.New("HTTP 422: was submitted too quickly"), true},
		{"rate-limit-exceeded", errors.New("Rate limit exceeded for user"), true},
		{"api-rate-limit", errors.New("API rate limit exceeded"), true},
		{"http-429", errors.New("HTTP 429: Too Many Requests"), true},
		{"bare-429-status", errors.New("429 Too Many Requests"), true},
		{"case-insensitive", errors.New("SECONDARY RATE LIMIT hit"), true},
		{"bare-403-on-issue-404", fmt.Errorf("publish: update issue #404: gh api repos/{owner}/{repo}/issues/404: HTTP 403 rate limited"), false},
		{"bare-403-auth", errors.New("gh api ...: exit status 1: HTTP 403: Must have admin rights"), false},
		{"unrelated-401", errors.New("gh: HTTP 401 Unauthorized"), false},
		{"unrelated-network", errors.New("dial tcp: connection refused"), false},
		{"generic", errors.New("boom"), false},
		{"issue-number-not-429", errors.New("update issue #1429 failed"), false},
		{"issue-429-permanent-403", fmt.Errorf("gh api repos/{owner}/{repo}/issues/429 -X PATCH ...: exit status 1: HTTP 403: Resource not accessible by integration"), false},
		{"issue-429-comment-mention", errors.New("update issue #429 failed"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGHRateLimited(tc.err); got != tc.want {
				t.Errorf("IsGHRateLimited(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
