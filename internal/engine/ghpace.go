package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ghpace.go wraps GHRunner with pacing and bounded retry aimed squarely at
// GitHub's *secondary* rate limit on content-creating requests (issue/comment
// creation and updates). GitHub asks API consumers to leave roughly a second
// between requests that create content, and to back off hard — not
// exponentially fast — when it trips the limiter and starts rejecting with
// "secondary rate limit"/"abuse detection" text. Without this, a publish run
// that opens or updates a batch of issues back-to-back reliably gets throttled
// partway through, silently dropping the remainder of the run's writes.
//
// This is deliberately asymmetric with internal/llm's retry wrapper
// (WithRetry / retryClient), which honors a server-supplied Retry-After
// header and backs off exponentially with jitter. gh CLI does not expose
// response headers to its callers — RealGH only ever sees gh's stderr text
// folded into the returned error — so there is no Retry-After to read here.
// Instead we use a fixed backoff schedule tuned to GitHub's documented
// secondary-limit guidance (wait "at least one minute" before retrying, cool
// down further if still limited), rather than pretending precision we don't
// have.
const (
	// ghMutationPaceInterval is the minimum spacing between the *start* of
	// consecutive mutating (POST/PATCH/PUT/DELETE) gh api calls made through a
	// single NewPacedGH instance. GitHub's secondary rate limit on content
	// creation is commonly triggered by requests fired in a tight loop; ~1s
	// spacing is the pace GitHub's own docs suggest to stay under it. Read
	// calls (GET, the gh api default) are never paced.
	ghMutationPaceInterval = time.Second
)

// ghRetryBackoff is the fixed sleep schedule between retry attempts once a
// call has been classified as rate-limited. len(ghRetryBackoff) is the retry
// count; total attempts = len+1. The schedule is deliberately not
// exponential: GitHub's secondary-limit guidance is to wait roughly a minute
// and try again, not to hammer with quickly-growing backoff, and since gh
// never surfaces a Retry-After we have no better signal to scale by.
var ghRetryBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}

// ErrGHRateLimited is the sentinel wrapped into the error NewPacedGH returns
// once it has exhausted retries against a rate-limited gh call. Callers use
// errors.Is(err, ErrGHRateLimited) to distinguish "GitHub is throttling us,
// try again later" from any other gh failure (auth, 404, validation, ...).
var ErrGHRateLimited = errors.New("gh: rate limited")

// ghRateLimitPhrases are case-insensitive substrings that identify a
// rate-limit response in gh's stderr text. They come from GitHub's own
// secondary-rate-limit and primary-rate-limit error bodies. A bare "HTTP 403"
// is deliberately excluded: 403 alone is gh's generic "forbidden" status and
// covers plain auth/permission failures far more often than rate limiting, so
// treating it as a rate limit would retry (and delay reporting) real auth
// errors. Only the more specific phrasing below counts — including explicit
// "429" status phrasings, but never a bare digit scan, since RealGH folds the
// full gh invocation (including issue/PR numbers) into the returned error
// text and a numeric issue number like #429 must never be misread as a 429
// status.
var ghRateLimitPhrases = []string{
	"secondary rate limit",
	"abuse detection",
	"submitted too quickly",
	"rate limit exceeded",
	"api rate limit",
	"http 429",
	"429 too many requests",
}

// IsGHRateLimited reports whether err represents a GitHub rate limit —
// either the ErrGHRateLimited sentinel wrapped by NewPacedGH, or raw gh
// stderr text (from a runner not routed through NewPacedGH, or bubbled up via
// %w somewhere else) that matches one of GitHub's known rate-limit phrasings.
// nil is never rate limited.
//
// Deliberately NOT included: any bare "429" digit scan. RealGH folds the
// full gh invocation into the returned error (ghrunner.go's stderr-into-error
// pattern), so a permanent failure against an issue/PR numbered 429 (e.g.
// "repos/{owner}/{repo}/issues/429: HTTP 403: Resource not accessible") would
// contain the digits "429" without being a rate limit at all. Matching only
// full phrases like "429 too many requests"/"http 429" avoids that trap.
func IsGHRateLimited(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrGHRateLimited) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, phrase := range ghRateLimitPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// isMutatingGHCall reports whether args represent a mutating gh api call, per
// the exact convention every call site in this repo uses: an "-X" flag
// immediately followed by POST, PATCH, PUT, or DELETE (see
// internal/cli/publish.go's ghCreateIssue/ghUpdateIssue/ghCommentIssue/
// ghPatchIssueClosed and internal/engine/review.go's issue/PR-comment and
// resolve-thread actions). Calls without an -X (the gh api default, GET) are
// reads and are never paced or specially throttled.
func isMutatingGHCall(args []string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-X" {
			continue
		}
		switch args[i+1] {
		case "POST", "PATCH", "PUT", "DELETE":
			return true
		}
	}
	return false
}

// pacedGH is the unexported state behind NewPacedGH's returned GHRunner. It
// tracks the wall-clock time of the last mutating call so consecutive
// mutations through the same instance can be spaced out, and injects sleep/
// now so tests run instantly with a fake clock instead of real timers.
type pacedGH struct {
	inner GHRunner

	mu           sync.Mutex
	lastMutation time.Time

	// sleep pauses for d, honoring ctx cancellation. Overridable by tests.
	sleep func(ctx context.Context, d time.Duration) error
	// now returns the current time. Overridable by tests for a fake clock.
	now func() time.Time
}

// NewPacedGH wraps inner with secondary-rate-limit-aware pacing and bounded
// retry (see the package doc above for rationale). Every mutating call
// (POST/PATCH/PUT/DELETE) made through the returned GHRunner is spaced at
// least ghMutationPaceInterval after the previous mutating call made through
// the *same* returned runner; GET calls pass straight through with no delay.
// If inner returns an error IsGHRateLimited classifies as a rate limit, the
// call is retried after a fixed backoff (ghRetryBackoff) up to
// len(ghRetryBackoff) times; any other error is returned immediately with no
// retry. Once retries are exhausted, the last error is wrapped together with
// ErrGHRateLimited so callers can still detect the rate-limit condition via
// errors.Is.
//
// One instance should be shared for the lifetime of a single publish/review
// run (that's where back-to-back mutating calls actually occur); a fresh
// instance per process is fine since pacing doesn't need to span processes.
func NewPacedGH(inner GHRunner) GHRunner {
	p := &pacedGH{
		inner: inner,
		sleep: defaultGHSleep,
		now:   time.Now,
	}
	return p.run
}

// defaultGHSleep is the production sleep implementation: it waits for d or
// until ctx is cancelled, whichever comes first, mirroring
// internal/llm/retry.go's doSleep so both retry paths in the codebase treat
// context cancellation identically.
func defaultGHSleep(ctx context.Context, d time.Duration) error {
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

// run is the GHRunner bound to p. It paces mutating calls, then delegates to
// p.inner with retry-on-rate-limit.
func (p *pacedGH) run(ctx context.Context, args ...string) ([]byte, error) {
	if isMutatingGHCall(args) {
		if err := p.pace(ctx); err != nil {
			return nil, err
		}
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		out, err := p.inner(ctx, args...)
		if err == nil {
			return out, nil
		}
		if !IsGHRateLimited(err) {
			return out, err
		}
		lastErr = err
		if attempt >= len(ghRetryBackoff) {
			return nil, fmt.Errorf("gh: rate limited after %d attempt(s): %w: %w", attempt+1, lastErr, ErrGHRateLimited)
		}
		if sleepErr := p.sleep(ctx, ghRetryBackoff[attempt]); sleepErr != nil {
			return nil, fmt.Errorf("gh: rate limited, retry aborted: %w: %w", lastErr, sleepErr)
		}
	}
}

// pace blocks, if needed, until ghMutationPaceInterval has elapsed since the
// last mutating call this instance made, then records the current call's
// start time as the new lastMutation. The wait happens outside the mutex so
// concurrent callers don't serialize on anything but the timestamp bookkeeping
// itself.
func (p *pacedGH) pace(ctx context.Context) error {
	p.mu.Lock()
	now := p.now()
	var wait time.Duration
	if !p.lastMutation.IsZero() {
		if elapsed := now.Sub(p.lastMutation); elapsed < ghMutationPaceInterval {
			wait = ghMutationPaceInterval - elapsed
		}
	}
	p.mu.Unlock()

	if wait > 0 {
		if err := p.sleep(ctx, wait); err != nil {
			return fmt.Errorf("gh: pacing wait aborted: %w", err)
		}
	}

	p.mu.Lock()
	p.lastMutation = p.now()
	p.mu.Unlock()
	return nil
}
