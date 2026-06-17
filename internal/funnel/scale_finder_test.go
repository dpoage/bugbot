package funnel

import (
	"reflect"
	"testing"
)

// TestScaleFinderForContext_UnknownIsNoOp covers the rule that an
// unknown / zero context window leaves the options exactly as they were.
// This is the path the existing funnel tests (and any openai-compatible
// endpoint that does not declare its window) hit, and it MUST be a strict
// no-op so behavior is stable.
func TestScaleFinderForContext_UnknownIsNoOp(t *testing.T) {
	base := Options{}
	got := scaleFinderForContext(base, 0)
	if !reflect.DeepEqual(got, base) {
		t.Errorf("zero contextWindow should return base Options unchanged; got %+v", got)
	}
	// Negative windows (defensive: never expected, but the rule is
	// "<= 0") must also be a no-op.
	gotNeg := scaleFinderForContext(base, -1)
	if !reflect.DeepEqual(gotNeg, base) {
		t.Errorf("negative contextWindow should return base Options unchanged; got %+v", gotNeg)
	}
}

// TestScaleFinderForContext_DefaultsCoveredByZeroPin covers the
// requirement that ContextWindow=0 leaves the funnel defaults — and the
// values resolve() synthesizes from them — untouched. It pins the exact
// numbers because the helper explicitly returns the input unchanged in
// that branch.
func TestScaleFinderForContext_DefaultsCoveredByZeroPin(t *testing.T) {
	// Build an Options the way New() would: a caller leaving every field
	// at zero produces the resolved values below.
	in := Options{}.resolve()
	// Sanity: the resolved values match the package defaults.
	if in.ChunkSize != DefaultChunkSize {
		t.Fatalf("resolved ChunkSize = %d, want %d", in.ChunkSize, DefaultChunkSize)
	}
	if in.FinderReadLines != 0 || in.FinderReadBytes != 0 {
		t.Fatalf("resolved read caps should still be 0 (resolution is at consume time); got %+v", in)
	}
	if in.FinderLimits.HistoryTokenBudget != 0 {
		t.Fatalf("resolved HistoryTokenBudget = %d, want 0 (compaction off by default)", in.FinderLimits.HistoryTokenBudget)
	}

	got := scaleFinderForContext(in, 0)
	if got.ChunkSize != DefaultChunkSize {
		t.Errorf("ChunkSize = %d, want %d (default preserved)", got.ChunkSize, DefaultChunkSize)
	}
	if got.FinderReadLines != 0 {
		t.Errorf("FinderReadLines = %d, want 0 (default sentinel preserved)", got.FinderReadLines)
	}
	if got.FinderReadBytes != 0 {
		t.Errorf("FinderReadBytes = %d, want 0 (default sentinel preserved)", got.FinderReadBytes)
	}
	if got.FinderLimits.HistoryTokenBudget != 0 {
		t.Errorf("HistoryTokenBudget = %d, want 0 (compaction stays off)", got.FinderLimits.HistoryTokenBudget)
	}
	if got.FinderHistoryTokens != 0 {
		t.Errorf("FinderHistoryTokens = %d, want 0", got.FinderHistoryTokens)
	}
}

// TestScaleFinderForContext_SmallWindowScalesDown covers the small-context
// path: chunk size and per-read caps must shrink strictly below the
// defaults, and the hard floors must be honored even on a 1k-context
// model.
func TestScaleFinderForContext_SmallWindowScalesDown(t *testing.T) {
	in := Options{}.resolve()

	const win = 8_000 // 1/8th of the 64k baseline
	got := scaleFinderForContext(in, win)

	if got.ChunkSize >= DefaultChunkSize {
		t.Errorf("ChunkSize = %d, want strictly less than default %d for 8k window",
			got.ChunkSize, DefaultChunkSize)
	}
	if got.ChunkSize < 1 {
		t.Errorf("ChunkSize = %d, want >= 1 (floor)", got.ChunkSize)
	}

	// finderReadCaps substitutes defaults for the zero sentinel; we assert
	// here on the raw Options values the helper produced, then re-resolve
	// to verify the agent sees the scaled numbers.
	if got.FinderReadLines >= DefaultFinderReadLines {
		t.Errorf("FinderReadLines = %d, want strictly less than default %d for 8k window",
			got.FinderReadLines, DefaultFinderReadLines)
	}
	if got.FinderReadLines < 100 {
		t.Errorf("FinderReadLines = %d, want >= 100 (floor)", got.FinderReadLines)
	}
	if got.FinderReadBytes >= DefaultFinderReadBytes {
		t.Errorf("FinderReadBytes = %d, want strictly less than default %d for 8k window",
			got.FinderReadBytes, DefaultFinderReadBytes)
	}
	if got.FinderReadBytes < 8*1024 {
		t.Errorf("FinderReadBytes = %d, want >= 8192 (floor)", got.FinderReadBytes)
	}

	// Compaction is OFF by default: even on a small window, the helper
	// must not enable it. The caller has to opt in.
	if got.FinderHistoryTokens != 0 || got.FinderLimits.HistoryTokenBudget != 0 {
		t.Errorf("compaction must stay off (not opted in); got FinderHistoryTokens=%d HistoryTokenBudget=%d",
			got.FinderHistoryTokens, got.FinderLimits.HistoryTokenBudget)
	}
}

// TestScaleFinderForContext_TinyWindowHonorsFloors covers the worst-case
// end of the scale: a model with a tiny declared context window (1k)
// still gets workable chunk/read values via the hard floors, never
// collapsing to zero.
func TestScaleFinderForContext_TinyWindowHonorsFloors(t *testing.T) {
	in := Options{}.resolve()
	got := scaleFinderForContext(in, 1_000)

	if got.ChunkSize < 1 {
		t.Errorf("ChunkSize = %d, want >= 1 (floor)", got.ChunkSize)
	}
	if got.FinderReadLines < 100 {
		t.Errorf("FinderReadLines = %d, want >= 100 (floor)", got.FinderReadLines)
	}
	if got.FinderReadBytes < 8*1024 {
		t.Errorf("FinderReadBytes = %d, want >= 8192 (floor)", got.FinderReadBytes)
	}
}

// TestScaleFinderForContext_LargeWindowNoChange covers the
// "do NOT inflate" rule: a model with a context window at or above the
// baseline sees the resolved defaults exactly as New() left them.
func TestScaleFinderForContext_LargeWindowNoChange(t *testing.T) {
	in := Options{}.resolve()
	for _, win := range []int{scaleBaselineContextWindow, 100_000, 200_000, 1_000_000} {
		got := scaleFinderForContext(in, win)
		if got.ChunkSize != DefaultChunkSize {
			t.Errorf("win=%d: ChunkSize = %d, want %d (large window should not inflate/shrink)",
				win, got.ChunkSize, DefaultChunkSize)
		}
		if got.FinderReadLines != 0 {
			t.Errorf("win=%d: FinderReadLines = %d, want 0 (sentinel preserved for large window)",
				win, got.FinderReadLines)
		}
		if got.FinderReadBytes != 0 {
			t.Errorf("win=%d: FinderReadBytes = %d, want 0 (sentinel preserved for large window)",
				win, got.FinderReadBytes)
		}
	}
}

// TestScaleFinderForContext_ExplicitValuesPreserved covers the rule
// that a caller who explicitly set a non-default value (e.g. ChunkSize=2
// to force fine-grained chunking in tests) does NOT have that value
// silently overridden by capability-driven scaling.
func TestScaleFinderForContext_ExplicitValuesPreserved(t *testing.T) {
	// ChunkSize=2 (explicit, != default 8), FinderReadLines=300 and
	// FinderReadBytes=4096 (explicit, non-zero), FinderHistoryTokens=1234
	// (explicit opt-in, positive).
	in := Options{
		ChunkSize:           2,
		FinderReadLines:     300,
		FinderReadBytes:     4096,
		FinderHistoryTokens: 1234,
	}.resolve()

	got := scaleFinderForContext(in, 8_000)

	if got.ChunkSize != 2 {
		t.Errorf("explicit ChunkSize=2 was overridden to %d", got.ChunkSize)
	}
	if got.FinderReadLines != 300 {
		t.Errorf("explicit FinderReadLines=300 was overridden to %d", got.FinderReadLines)
	}
	if got.FinderReadBytes != 4096 {
		t.Errorf("explicit FinderReadBytes=4096 was overridden to %d", got.FinderReadBytes)
	}
	// History: the caller opted in (positive). Scaling kicks in. The
	// value must change, and HistoryTokenBudget must mirror it.
	if got.FinderHistoryTokens == 1234 {
		t.Errorf("explicit opt-in FinderHistoryTokens=1234 should have been scaled; got %d", got.FinderHistoryTokens)
	}
	if got.FinderLimits.HistoryTokenBudget != got.FinderHistoryTokens {
		t.Errorf("HistoryTokenBudget = %d, FinderHistoryTokens = %d (limits must mirror the opt-in value)",
			got.FinderLimits.HistoryTokenBudget, got.FinderHistoryTokens)
	}
	if got.FinderLimits.HistoryTokenBudget < scaleFloorHistoryTokens {
		t.Errorf("HistoryTokenBudget = %d, want >= %d (floor)", got.FinderLimits.HistoryTokenBudget, scaleFloorHistoryTokens)
	}
}

// TestScaleFinderForContext_ExplicitNegativeReadCapsPreserved covers the
// rule that a caller who asked for the looser agent-package read defaults
// (negative sentinel) is NOT forced into a tighter scaled value. A
// negative value is an EXPLICIT choice; only the zero "use funnel
// default" sentinel gets scaled.
func TestScaleFinderForContext_ExplicitNegativeReadCapsPreserved(t *testing.T) {
	in := Options{
		FinderReadLines: -1,
		FinderReadBytes: -1,
	}.resolve()

	got := scaleFinderForContext(in, 4_000)

	if got.FinderReadLines != -1 {
		t.Errorf("explicit FinderReadLines=-1 was overridden to %d", got.FinderReadLines)
	}
	if got.FinderReadBytes != -1 {
		t.Errorf("explicit FinderReadBytes=-1 was overridden to %d", got.FinderReadBytes)
	}
}

// TestScaleFinderForContext_HistoryCompactionOnlyWhenOptedIn covers the
// "do not enable history compaction by default" rule. A caller that
// left FinderHistoryTokens at zero (compaction off) must not see it
// flipped on by scaling, regardless of how small the context window is.
func TestScaleFinderForContext_HistoryCompactionOnlyWhenOptedIn(t *testing.T) {
	in := Options{}.resolve() // FinderHistoryTokens = 0 (compaction off)
	for _, win := range []int{1_000, 4_000, 8_000, 16_000, 32_000} {
		got := scaleFinderForContext(in, win)
		if got.FinderHistoryTokens != 0 {
			t.Errorf("win=%d: FinderHistoryTokens = %d, want 0 (compaction must not auto-enable)",
				win, got.FinderHistoryTokens)
		}
		if got.FinderLimits.HistoryTokenBudget != 0 {
			t.Errorf("win=%d: HistoryTokenBudget = %d, want 0 (compaction must not auto-enable)",
				win, got.FinderLimits.HistoryTokenBudget)
		}
	}
}

// TestScaleFinderForContext_HistoryCompactionScalesWhenOptedIn covers
// the positive opt-in path: when the caller has set FinderHistoryTokens
// to a positive value, scaling kicks in and produces a threshold
// proportional to the context window, with the floor.
func TestScaleFinderForContext_HistoryCompactionScalesWhenOptedIn(t *testing.T) {
	in := Options{FinderHistoryTokens: 60_000}.resolve()
	got := scaleFinderForContext(in, 8_000)
	// 25% of 8_000 = 2_000, which is above the 1_000 floor.
	if got.FinderHistoryTokens != 2_000 {
		t.Errorf("FinderHistoryTokens = %d, want 2000 (25%% of 8000)", got.FinderHistoryTokens)
	}
	if got.FinderLimits.HistoryTokenBudget != 2_000 {
		t.Errorf("HistoryTokenBudget = %d, want 2000", got.FinderLimits.HistoryTokenBudget)
	}

	// On a tiny window, the floor wins.
	got2 := scaleFinderForContext(in, 1_000)
	// 25% of 1_000 = 250, clamped to 1_000.
	if got2.FinderHistoryTokens != scaleFloorHistoryTokens {
		t.Errorf("FinderHistoryTokens = %d, want %d (floor)", got2.FinderHistoryTokens, scaleFloorHistoryTokens)
	}
}
