package funnel

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
)

// arbiterPromptMarker is a stable substring of arbiterSystemPrompt used in
// these tests to verify which client served the arbiter completion. Refuter
// system prompts (verifierSystemPrompt) do NOT contain it.
const arbiterPromptMarker = "serving as the deciding arbiter"

// TestM8z_ArbiterEscalation_EscalatesToArbiterClient verifies the split-panel
// path: when the funnel is constructed with a dedicated Arbiter client, the
// arbiter completion is served by THAT client (not the verifier's). This is
// the core behavior of bugbot-m8z: route the ~5% split verdicts to a stronger
// model when the operator configures one.
//
// Harness layout:
//   - fakeVerifier: serves the 2-refuter panel ONLY. Call-ordinal routing —
//     the first refuter request → refutedJSON, every subsequent →
//     notRefutedJSON (fallback) — yields a split panel (1 refuted, 1 not). It
//     must never serve the arbiter; the test asserts callCount==2 and that no
//     arbiter-prompt request reaches it.
//   - fakeArbiter: a DISTINCT client serving ONLY the arbiter completion,
//     routed on the "PANEL VERDICTS" task substring → notRefutedArbiterJSON
//     (arbiterSchema requires an evidence field, so a refuter verdict would not
//     parse as an arbiter response here).
//
// Drives the full pipeline through Sweep so the standard verify-stream path
// is exercised (refuter fanout → split detection → arbiter escalation).
func TestM8z_ArbiterEscalation_EscalatesToArbiterClient(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains(
		"nil-safety/error-handling",
		candJSON(realCand),
	)

	// Verifier serves the 2-refuter panel ONLY (and must NOT serve the arbiter
	// — that is the whole point of the escalation). Call-ordinal routing: the
	// first refuter request → refuted, every subsequent → not-refuted, which
	// produces a split panel.
	fakeVerifier := newScriptedClient()
	callIdx := 0
	fakeVerifier.on(func(_ llm.Request) bool {
		cur := callIdx
		callIdx++
		return cur == 0
	}, refutedJSON)
	fakeVerifier.fallback = notRefutedJSON

	// Distinct arbiter client — serves ONLY the arbiter completion.
	fakeArbiter := newScriptedClient()
	fakeArbiter.onTaskContains("PANEL VERDICTS", notRefutedArbiterJSON)

	f, err := New(RoleClients{
		Finder:   finder,
		Verifier: fakeVerifier,
		Arbiter:  fakeArbiter,
	}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}},
		Limits:    StageLimits{Refuters: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Split verdict → arbiter ran exactly once.
	if res.Stats.ArbiterRuns != 1 {
		t.Errorf("ArbiterRuns = %d, want 1 (split panel)", res.Stats.ArbiterRuns)
	}
	// Arbiter said not-refuted → 1 finding survives.
	if len(res.Findings) != 1 {
		t.Errorf("want 1 survived finding, got %d", len(res.Findings))
	}

	// Fake arbiter must have served at least one completion.
	if got := fakeArbiter.callCount(); got == 0 {
		t.Fatalf("fakeArbiter served 0 completions — escalation did not route to it " +
			"(verifier would have served the arbiter)")
	}
	// Verifier should have served exactly the 2 refuter panel calls. If it
	// served the arbiter too, callCount > 2.
	if got := fakeVerifier.callCount(); got != 2 {
		t.Errorf("fakeVerifier served %d completions, want 2 (refuter panel only); "+
			"values > 2 indicate the verifier served the arbiter too", got)
	}

	// The fakeArbiter's served system prompt must include the arbiter marker.
	if !anySystemContains(fakeArbiter, arbiterPromptMarker) {
		t.Errorf("fakeArbiter's served system prompt(s) did not match arbiterSystemPrompt marker %q",
			arbiterPromptMarker)
	}
	// The fakeVerifier's requests must NOT include the arbiter marker.
	if anySystemContains(fakeVerifier, arbiterPromptMarker) {
		t.Errorf("fakeVerifier received an arbiter-prompt request — escalation leaked to verifier")
	}
}

// TestM8z_ArbiterEscalation_NilArbiterReusesVerifier pins the fallback
// contract: when Arbiter is nil the split arbiter is served by the verifier
// client — byte-identical to today's behavior pre bugbot-m8z.
func TestM8z_ArbiterEscalation_NilArbiterReusesVerifier(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient().onSystemContains(
		"nil-safety/error-handling",
		candJSON(realCand),
	)

	// One client covers everything: refuter panel + arbiter.
	// makeCallCountVerifier(1, notRefutedArbiterJSON) gives: first call →
	// refuted, then notRefuted fallback; arbiter → notRefutedArbiterJSON.
	fake := makeCallCountVerifier(1, notRefutedArbiterJSON)

	f, err := New(RoleClients{
		Finder:   finder,
		Verifier: fake,
		Arbiter:  nil, // explicit fallback
	}, st, repo, Options{
		Discovery: DiscoveryConfig{Lenses: []string{"nil-safety/error-handling"}},
		Limits:    StageLimits{Refuters: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res.Stats.ArbiterRuns != 1 {
		t.Errorf("ArbiterRuns = %d, want 1 (split panel)", res.Stats.ArbiterRuns)
	}
	if len(res.Findings) != 1 {
		t.Errorf("want 1 survived finding, got %d", len(res.Findings))
	}
	// 2 refuters + 1 arbiter = 3 completions on the single fake.
	if got := fake.callCount(); got != 3 {
		t.Errorf("fake served %d completions, want 3 (refuter panel + arbiter reused verifier)", got)
	}
	if !anySystemContains(fake, arbiterPromptMarker) {
		t.Errorf("fake verifier did not receive an arbiter completion — fallback path did not serve arbiter")
	}
}

// anySystemContains returns true if any recorded request's System field
// contains sub. m8z-prefixed per the assignment's naming convention; the
// fake_test.go-internal helpers (callCount, allRequests) live in package
// funnel so we reuse them.
func anySystemContains(c *scriptedClient, sub string) bool {
	for _, r := range c.allRequests() {
		if strings.Contains(r.System, sub) {
			return true
		}
	}
	return false
}
