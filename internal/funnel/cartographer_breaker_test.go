package funnel

import (
	"context"
	"path"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

// cartographerTransportErrClient is a fake llm.Client whose Complete always
// returns the same transport-level *llm.APIError on every call
// (StatusCode==0, Kind=ErrServer) — the exact breaker-detection shape
// produced by the openai/google/anthropic adapters for timeouts, connection
// resets, DNS failures. calls is atomic so the test can assert the exact
// number of LLM calls the breaker let through.
type cartographerTransportErrClient struct {
	calls atomic.Int32
}

func newCartographerTransportErrClient() *cartographerTransportErrClient {
	return &cartographerTransportErrClient{}
}

func (c *cartographerTransportErrClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *cartographerTransportErrClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.calls.Add(1)
	return llm.Response{}, &llm.APIError{
		Kind:       llm.ErrServer,
		StatusCode: 0,
		Provider:   "fake",
		Message:    "dial tcp: connection refused",
	}
}

// cartographerFirstSuccessThenTransportClient returns a valid summary JSON
// on its FIRST Complete call and a transport error on every call thereafter.
// It exercises the "first success disarms the breaker" path: with
// MaxParallel=1 the first unit's success is recorded before any later unit
// runs, so the subsequent transport failures are observed with the breaker
// already disarmed.
type cartographerFirstSuccessThenTransportClient struct {
	calls atomic.Int32
}

func (c *cartographerFirstSuccessThenTransportClient) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}

func (c *cartographerFirstSuccessThenTransportClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	n := c.calls.Add(1)
	if n == 1 {
		return llm.Response{
			Text:       `{"summary":"valid first summary"}`,
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5},
		}, nil
	}
	return llm.Response{}, &llm.APIError{
		Kind:       llm.ErrServer,
		StatusCode: 0,
		Provider:   "fake",
		Message:    "dial tcp: connection refused",
	}
}

// cartographerParseFailClient is a fake llm.Client whose Complete always
// returns a well-formed HTTP response (NO transport error) carrying output
// that is not valid summary JSON, so RunJSON's parse + one-shot repair both
// fail and summarizePackage returns a non-transport (parse) error. It models
// the output-token-exhaustion / malformed-JSON failure mode (cf. bugbot-rwe),
// which must NEVER arm the transport breaker.
type cartographerParseFailClient struct {
	calls atomic.Int32
}

func (c *cartographerParseFailClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *cartographerParseFailClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.calls.Add(1)
	return llm.Response{
		Text:       "this is not valid summary json",
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5},
	}, nil
}

// pkgOf returns the package directory for a target file path. Targets in
// openMultiPkgFixture have the form "<pkg>/<pkg>.go", so path.Dir yields
// the package key the cartography memo is keyed by.
func pkgOf(target string) string { return path.Dir(target) }

// TestCartographer_TransportBreaker_AbortsGeneration pins bugbot-1r9:
// against an unreachable provider, the lazy-mode cartographer aborts
// further generations once a threshold of transport-class failures is
// observed with zero successful generations, instead of grinding through
// every package and burning the retry budget package-by-package.
//
// Setup: many packages (>> threshold), the fake client returns a
// transport error on every call. We launch one goroutine per package;
// the first MaxParallel goroutines hit generate() concurrently, each
// observes a transport failure, the breaker trips on the threshold-th
// failure, and any goroutine that ENTERS generate() AFTER the trip
// short-circuits without calling Complete. The test asserts (a) the
// Complete call count is well below the package count (proving further
// generations were skipped) and (b) the tripped flag is set.
//
// Note: without a synchronization barrier, the goroutines race through
// generate() and the exact Complete-call count is scheduling-dependent.
// The breaker guarantees that the count is at most MaxParallel * a small
// grace (for goroutines that entered generate() concurrently with the
// tripping goroutine and only saw the trip flag on their next sf.Do
// call), and well below the package count.
func TestCartographer_TransportBreaker_AbortsGeneration(t *testing.T) {
	pkgs := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	st, repo, targets := openMultiPkgFixture(t, pkgs...)
	snap, fps := snapAndFps(t, repo)

	client := newCartographerTransportErrClient()
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
		Limits:   StageLimits{MaxParallel: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	cart := f.newCartographer(context.Background(), &Result{}, client, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("newCartographer returned nil")
	}

	var wg sync.WaitGroup
	for _, tgt := range targets {
		tgt := tgt
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cart.getSummary(context.Background(), pkgOf(tgt))
		}()
	}
	wg.Wait()

	if !cart.breakerTripped.Load() {
		t.Errorf("breakerTripped = false, want true (transport threshold should have tripped)")
	}
	if cart.anySuccess.Load() {
		t.Errorf("anySuccess = true, want false (no successful generation should have occurred)")
	}
	got := client.calls.Load()
	// Past the trip, generate() short-circuits without calling Complete.
	// Allow up to MaxParallel + a small grace (4 here) for goroutines that
	// entered generate() concurrently with the tripping goroutine. The
	// point is: the count must NOT scale with the number of packages.
	upperBound := int32(cart.breakerThreshold()) + 4
	if got > upperBound {
		t.Errorf("client.Complete calls = %d, want <= %d (breaker should have short-circuited most packages)", got, upperBound)
	}
	if got < cart.breakerThreshold() {
		t.Errorf("client.Complete calls = %d, want at least threshold (%d) — breaker never reached the threshold", got, cart.breakerThreshold())
	}
}

// TestCartographer_Breaker_DisarmsOnSuccess pins the "anySuccess disarms
// permanently" property: once a lazy generate() returns a summary, a later
// transport failure does NOT arm the breaker counter and does NOT trip it
// — a flaky network mid-run cannot abort the cartographer after a working
// start.
//
// Determinism: MaxParallel=1 forces a sequential pass, so the first
// unit's success is recorded BEFORE any later unit runs. Every subsequent
// unit's transport error is observed with the breaker already disarmed
// (anySuccess==true short-circuits the count branch), and the Complete
// call count therefore scales linearly with the number of packages. The
// breaker must never trip.
func TestCartographer_Breaker_DisarmsOnSuccess(t *testing.T) {
	pkgs := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	st, repo, targets := openMultiPkgFixture(t, pkgs...)
	snap, fps := snapAndFps(t, repo)

	client := &cartographerFirstSuccessThenTransportClient{}
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
		Limits:   StageLimits{MaxParallel: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	cart := f.newCartographer(context.Background(), &Result{}, client, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("newCartographer returned nil")
	}

	for _, tgt := range targets {
		_, _ = cart.getSummary(context.Background(), pkgOf(tgt))
	}

	if cart.breakerTripped.Load() {
		t.Errorf("breakerTripped = true, want false (any-success disarm should prevent the trip)")
	}
	if !cart.anySuccess.Load() {
		t.Errorf("anySuccess = false, want true (first unit's success must disarm)")
	}
	if got := client.calls.Load(); got < int32(len(pkgs)) {
		t.Errorf("client.Complete calls = %d, want %d (every package must attempt generation)", got, len(pkgs))
	}
}

// TestCartographer_Breaker_IgnoresNonTransportFailures pins the breaker's
// transport-only classification: a storm of NON-transport failures (here parse
// failures — a valid HTTP response with malformed JSON, the output-exhaustion
// shape from bugbot-rwe) must NEVER trip the breaker. Only transport-class
// failures (StatusCode==0) signal a systemic outage; a provider that is
// reachable but returning junk is an isolated-unit problem, so the cartographer
// must keep attempting every package rather than short-circuiting.
func TestCartographer_Breaker_IgnoresNonTransportFailures(t *testing.T) {
	pkgs := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	st, repo, targets := openMultiPkgFixture(t, pkgs...)
	snap, fps := snapAndFps(t, repo)

	client := &cartographerParseFailClient{}
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		Features: FeatureFlags{Cartographer: true},
		Limits:   StageLimits{MaxParallel: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	cart := f.newCartographer(context.Background(), &Result{}, client, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("newCartographer returned nil")
	}

	var wg sync.WaitGroup
	for _, tgt := range targets {
		tgt := tgt
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cart.getSummary(context.Background(), pkgOf(tgt))
		}()
	}
	wg.Wait()

	if cart.breakerTripped.Load() {
		t.Errorf("breakerTripped = true, want false (non-transport parse failures must not arm the breaker)")
	}
	if cart.anySuccess.Load() {
		t.Errorf("anySuccess = true, want false (no summary parsed successfully)")
	}
	// No short-circuit: every package was attempted (>= one Complete per
	// package; RunJSON's repair round-trip may add more).
	if got := client.calls.Load(); got < int32(len(pkgs)) {
		t.Errorf("client.Complete calls = %d, want >= %d (breaker must not short-circuit non-transport failures)", got, len(pkgs))
	}
}
