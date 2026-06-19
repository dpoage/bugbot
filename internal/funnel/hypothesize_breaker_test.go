package funnel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/llm"
)

// transportErrorClient is a fake llm.Client whose Complete returns the same
// transport-level *llm.APIError on every call: StatusCode==0, Kind=ErrServer
// (the exact shape produced by the openai / google / anthropic adapters for
// non-HTTP errors — timeout, connection reset, DNS failure). It blocks every
// call on a barrier that opens only once the test has released it, so the
// dispatch loop actually contends the slot pool and the breaker state is
// observed deterministically rather than racing the goroutines to completion
// in zero wall time. Without the barrier every goroutine could finish
// before the next unit is even launched, and the "abort after threshold" claim
// would be untestable.
type transportErrorClient struct {
	mu        sync.Mutex
	calls     int
	inflight  atomic.Int32 // observed in-flight concurrency at peak
	peak      atomic.Int32 // high-water mark of inflight
	release   chan struct{}
	transport *llm.APIError
}

func newTransportErrorClient() *transportErrorClient {
	return &transportErrorClient{
		release:   make(chan struct{}),
		transport: defaultTransportErr(),
	}
}

func defaultTransportErr() *llm.APIError {
	return &llm.APIError{
		Kind:       llm.ErrServer,
		StatusCode: 0, // StatusCode==0 is the breaker-detection signal
		Provider:   "fake",
		Message:    "dial tcp: connection refused",
	}
}

func (c *transportErrorClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *transportErrorClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	now := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		old := c.peak.Load()
		if now <= old || c.peak.CompareAndSwap(old, now) {
			break
		}
	}
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	// Block until the test releases (or the context is cancelled by the
	// breaker). Either is a valid "exit" — the dispatch loop is what we
	// care about, not the precise moment the goroutine returns.
	select {
	case <-c.release:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	return llm.Response{}, c.transport
}

func (c *transportErrorClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *transportErrorClient) releaseAll() {
	close(c.release)
}

// TestHypothesize_TransportErrorBreaker proves bugbot-2uz: against an
// unreachable provider, the finder stage aborts early once a threshold of
// transport-class parse failures accumulates with zero finderOK successes,
// instead of waiting for every (seats x ~4s) retry to time out. The
// already-recorded FinderFailures keep Stats.MostFindersFailed() true, and
// Stats.FinderAborted surfaces the abort reason distinctly from a normal
// "all units ran and failed" run.
//
// Setup: the finder client returns the same *llm.APIError{StatusCode: 0,
// Kind: ErrServer} on every call (the breaker-detection shape) and blocks
// every call on a barrier that opens only AFTER the test has observed the
// breaker trip. That ordering is what makes the test deterministic: the
// dispatch loop can only have launched up to MaxParallel units when the
// breaker trips, because further launches require the loop to drain those
// in-flight units first.
//
// Threshold = max(3, MaxParallel); with the default MaxParallel=4 the
// threshold is 4. The test asserts the breaker tripped, the stage reported
// unreliable, and the FinderRuns count never reached all sweep units
// (proving the abort, not "every unit ran and failed").
func TestHypothesize_TransportErrorBreaker(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	const maxParallel = 4
	// Exactly threshold: with the default MaxParallel the breaker trips
	// after this many transport failures. We use a small MaxParallel so the
	// test is fast and the assertion is easy to follow.
	finder := newTransportErrorClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Limits: StageLimits{MaxParallel: maxParallel},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Launch the sweep on its own goroutine: it will block on the transport
	// client's release channel until we close it. We give it a generous
	// budget so the test does not flake on slow CI: 2s is well above the
	// threshold (which trips after ~maxParallel blocked calls plus a
	// scheduling round) and well below the un-broken path's worst case
	// (~45s for 4 attempts * 4s of backoff per seat).
	type sweepResult struct {
		res *Result
		err error
	}
	resultCh := make(chan sweepResult, 1)
	go func() {
		res, err := f.Sweep(ctx)
		resultCh <- sweepResult{res: res, err: err}
	}()

	// Wait for the breaker to trip. We poll the in-flight concurrency and
	// the call count: when the call count reaches exactly the threshold
	// (with all calls blocked) we know the dispatch loop has recorded
	// maxParallel transport failures and the breaker has fired. We give
	// the test 2s — far longer than a single scheduling round on any
	// reasonable CI.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if finder.callCount() >= maxParallel {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if finder.callCount() < maxParallel {
		t.Fatalf("transport client never reached %d calls within deadline; breaker assertion invalid", maxParallel)
	}

	// Release the barrier so the in-flight units can return and the sweep
	// can complete. After this the breaker will have already tripped
	// (guarded by CompareAndSwap so only one goroutine fires runCancel;
	// the others observe breakerTripped=true and skip the increment).
	finder.releaseAll()

	// Bound the sweep completion: with the breaker tripped the remaining
	// units never launch and the in-flight ones return promptly after the
	// barrier opens (or get cancelled by runCancel before they return).
	// 2s is far more than enough.
	var res *Result
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("Sweep should not error on a breaker trip: %v", r.err)
		}
		res = r.res
	case <-time.After(2 * time.Second):
		t.Fatal("Sweep did not complete within 2s of breaker trip")
	}

	// Breaker effect: only the in-flight units ever ran; the remaining
	// sweep units never launched. With the default lens set and a Go
	// fixture repo the sweep has many more than maxParallel units, so a
	// correct breaker means FinderRuns is much less than the full unit
	// count.
	if res.Stats.FinderRuns >= goSweepUnits() {
		t.Errorf("FinderRuns = %d, want < %d (breaker should have aborted further launches)", res.Stats.FinderRuns, goSweepUnits())
	}
	if res.Stats.FinderRuns != maxParallel {
		t.Errorf("FinderRuns = %d, want %d (the threshold / MaxParallel in-flight units)", res.Stats.FinderRuns, maxParallel)
	}
	if res.Stats.FinderFailures != maxParallel {
		t.Errorf("FinderFailures = %d, want %d (every in-flight unit recorded a parse failure)", res.Stats.FinderFailures, maxParallel)
	}
	if res.Stats.FinderRateLimited != 0 {
		t.Errorf("FinderRateLimited = %d, want 0 (transport errors are NOT rate-limit)", res.Stats.FinderRateLimited)
	}
	if !res.Stats.FinderAborted {
		t.Error("FinderAborted = false, want true (breaker should have tripped)")
	}
	// MostFindersFailed reports the run as unreliable: the recorded
	// failures already exceed half the recorded runs (in fact equal them,
	// because every recorded run failed). The "breaker aborted" reason is
	// also surfaced via FinderAborted so downstream consumers can
	// distinguish "every unit ran and failed" from "we aborted early".
	if !res.Stats.MostFindersFailed() {
		t.Errorf("MostFindersFailed() = false, want true (FinderRuns=%d, FinderFailures=%d)", res.Stats.FinderRuns, res.Stats.FinderFailures)
	}
	if res.Stats.FinderReliable() {
		t.Error("FinderReliable() = true, want false when every finder failed")
	}
	// Sanity: a skipped-note on Result.Skipped per failed lens, matching
	// the existing parse-failure surfacing.
	if len(res.Skipped) == 0 {
		t.Error("expected per-lens failure notes on Result.Skipped, got none")
	}
}

// TestHypothesize_HealthyFinderBreakerNeverTrips is the control case:
// a healthy finder run (one that returns parseable candidates) must leave
// the breaker permanently disarmed. We assert both that the breaker never
// trips AND that all sweep units ran to completion (i.e. behaviour is
// unchanged from the pre-breaker world).
func TestHypothesize_HealthyFinderBreakerNeverTrips(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := finderOnNilLens(newScriptedClient())
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep should not error on healthy finders: %v", err)
	}

	// All sweep units ran and the breaker never tripped.
	if res.Stats.FinderAborted {
		t.Error("FinderAborted = true, want false (healthy run must not trip the breaker)")
	}
	if res.Stats.FinderReliable() {
		// Expected for a healthy run.
	} else {
		t.Errorf("FinderReliable() = false, want true (no finder failed)")
	}
	if res.Stats.MostFindersFailed() {
		t.Error("MostFindersFailed() = true, want false (no finder failed)")
	}
	// Healthy-finding assertion: the real bug survived the pipeline. This
	// pins that the breaker did not silently short-circuit any verification
	// or persistence work — behaviour is identical to the pre-breaker world.
	if len(res.Findings) != 1 {
		t.Errorf("findings = %d, want 1 (the real bug)", len(res.Findings))
	}
}

// TestHypothesize_TransportErrorAfterSuccessBreakerStaysDisarmed proves the
// "anySucceeded disarms permanently" property: once a finder returns parseable
// output, a later transport failure does NOT arm the breaker counter and does
// NOT trip it — a flaky network mid-run cannot abort the stage after a working
// start.
//
// Determinism: the finder is count-based — the FIRST Complete call returns the
// real candidate (success); every later call returns a transport error. With
// MaxParallel=1 each unit runs to completion before the next acquires the slot,
// and the slot is released only AFTER the finderOK path sets anySucceeded, so
// the first unit always disarms the breaker before any transport unit is
// observed. The outcome is independent of which lens the scheduler runs first.
func TestHypothesize_TransportErrorAfterSuccessBreakerStaysDisarmed(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	inner := newScriptedClient()
	inner.fallback = candJSON(realCand) // any request → the real bug
	finder := &firstSuccessThenTransportClient{
		inner:     inner,
		transport: defaultTransportErr(),
	}
	verifier := verifierRouting(newScriptedClient())

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Limits: StageLimits{MaxParallel: 1}, // serialize so the first success disarms before any failure
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep should not error: %v", err)
	}

	// The breaker never tripped: the first unit succeeded, disarming it before
	// any transport failure could be counted.
	if res.Stats.FinderAborted {
		t.Error("FinderAborted = true, want false (early success disarms the breaker)")
	}
	// Guard: later units must actually have failed with transport errors,
	// otherwise the disarm path is not exercised at all.
	if res.Stats.FinderFailures == 0 {
		t.Error("FinderFailures = 0, want >0 (units after the first must fail with transport errors)")
	}
	// The real finding survived end-to-end — the breaker did not short-circuit
	// verification/persistence after the success.
	foundReal := false
	for _, fnd := range res.Findings {
		if fnd.File == "bug.go" && fnd.Line == 10 {
			foundReal = true
			break
		}
	}
	if !foundReal {
		t.Errorf("expected the real bug.go:10 finding to survive; got %+v", res.Findings)
	}
}

// firstSuccessThenTransportClient returns a successful (parseable) response on
// its FIRST Complete call and a transport error on every call thereafter. It
// exercises the "first success disarms the breaker" path: with MaxParallel=1
// the first unit's success is recorded before any later unit runs, so the
// subsequent transport failures are observed with the breaker already disarmed.
type firstSuccessThenTransportClient struct {
	inner     *scriptedClient
	transport *llm.APIError
	n         atomic.Int32
}

func (c *firstSuccessThenTransportClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *firstSuccessThenTransportClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if c.n.Add(1) == 1 {
		return c.inner.Complete(ctx, req)
	}
	return llm.Response{}, c.transport
}

// TestIsTransportError pins the detection predicate in isolation: nil-safe,
// matches *llm.APIError with StatusCode==0 (any Kind, including the
// adapter-style ErrServer/0 shape), and does NOT match StatusCode!=0 (a
// genuine 5xx is NOT a transport failure and must not trip the breaker).
// This is the breaker-detection keystone; if the predicate regressed, the
// end-to-end test would silently lose its trip signal.
func TestIsTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "transport-shaped APIError (StatusCode=0, Kind=ErrServer)",
			err:  &llm.APIError{Kind: llm.ErrServer, StatusCode: 0, Provider: "fake"},
			want: true,
		},
		{
			name: "transport-shaped APIError (StatusCode=0, Kind=ErrOverloaded)",
			err:  &llm.APIError{Kind: llm.ErrOverloaded, StatusCode: 0, Provider: "fake"},
			want: true,
		},
		{
			name: "genuine 5xx APIError (StatusCode!=0)",
			err:  &llm.APIError{Kind: llm.ErrServer, StatusCode: 503, Provider: "fake"},
			want: false,
		},
		{
			name: "rate-limit APIError (StatusCode=429) MUST NOT be classified as transport",
			err: &llm.APIError{
				Kind: llm.ErrRateLimited, StatusCode: 429, Provider: "fake",
			},
			want: false,
		},
		{
			name: "non-APIError (plain error) is not transport-shaped",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "wrapped transport APIError (errors.As chain) still detected",
			err: func() error {
				inner := &llm.APIError{Kind: llm.ErrServer, StatusCode: 0, Provider: "fake"}
				return wrappedErr{inner: inner}
			}(),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransportError(tc.err); got != tc.want {
				t.Errorf("isTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// wrappedErr lets a test wrap an error in a non-APIError type while still
// supporting errors.As for the inner APIError. It models a caller that
// decorated a transport error with extra context (e.g. a retry wrapper).
type wrappedErr struct{ inner error }

func (w wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrappedErr) Unwrap() error { return w.inner }

// TestClassifyFinderErr_TransportError pins that classifyFinderErr routes
// transport errors to finderClassTransportError (the postmortem class the
// breaker looks at), not finderClassEmptyOutput or finderClassUnparseable.
// This is the postmortem-classification keystone for bugbot-2uz: a
// regression here would silently mask transport errors as empty-output
// runs, and the breaker would never trip.
func TestClassifyFinderErr_TransportError(t *testing.T) {
	transportErr := &llm.APIError{Kind: llm.ErrServer, StatusCode: 0, Provider: "fake"}
	cls := classifyFinderErr(nil, transportErr)
	if cls != finderClassTransportError {
		t.Errorf("classifyFinderErr(transport) = %q, want %q", cls, finderClassTransportError)
	}

	// Rate-limit must still classify distinctly — it is NOT a transport
	// error and the breaker must not count it.
	rateLimitErr := &llm.APIError{Kind: llm.ErrRateLimited, StatusCode: 429, Provider: "fake"}
	cls = classifyFinderErr(nil, rateLimitErr)
	if cls != finderClassRateLimited {
		t.Errorf("classifyFinderErr(rate-limit) = %q, want %q", cls, finderClassRateLimited)
	}

	// Genuine 5xx (StatusCode != 0) is NOT transport — it is a parse-style
	// failure because the runner produced an error but it is not the
	// breaker-detection shape.
	serverErr := &llm.APIError{Kind: llm.ErrServer, StatusCode: 503, Provider: "fake"}
	cls = classifyFinderErr(nil, serverErr)
	if cls == finderClassTransportError {
		t.Errorf("classifyFinderErr(5xx) = %q, must not be transport (StatusCode!=0)", cls)
	}
}

// TestStats_FinderAbortedField pins the field's presence and JSON tag, so
// downstream JSON consumers (e.g. the scan_runs stats blob) see the
// new field distinctly. omitempty means a healthy run serializes the same
// JSON it did before, preserving backward compatibility.
func TestStats_FinderAbortedField(t *testing.T) {
	var s Stats
	if s.FinderAborted {
		t.Error("zero Stats.FinderAborted = true, want false")
	}
	// Sanity: omitempty keeps a healthy run's JSON blob identical to the
	// pre-breaker wire format, so existing consumers see no diff.
	s.FinderRuns = 2
	b, err := json.Marshal(Stats{FinderRuns: 2})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "finder_aborted") {
		t.Errorf("zero-value FinderAborted must be omitted from JSON, got %s", b)
	}
	s2 := Stats{FinderAborted: true}
	b, err = json.Marshal(s2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"finder_aborted":true`) {
		t.Errorf("set FinderAborted must appear in JSON, got %s", b)
	}
}
