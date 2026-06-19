package funnel

// scaleFinderForContext returns a copy of o with chunk size, finder per-read
// caps, and (when already enabled) history-compaction threshold adjusted to
// fit contextWindow tokens.
//
// Why: the funnel's defaults (DefaultChunkSize / DefaultFinderReadLines /
// DefaultFinderReadBytes) are tuned for ~64k-200k-context models. A small-
// context weak model — e.g. an 8k local LLM behind an openai-compatible
// endpoint — silently overflows with one-size defaults: chunked file lists
// and large per-read results saturate the conversation window before the
// agent can finish reading its targets. Adjusting the chunk size and read
// caps down proportionally slows that history growth at the source.
//
// Rules (kept conservative so the helper is a no-op for any caller that
// does not opt in to capability-driven behavior):
//
//   - contextWindow <= 0 (UNKNOWN) -> the input options are returned
//     unchanged. This is the default for fake clients in funnel tests and
//     for arbitrary openai-compatible endpoints without a declared window,
//     and the unchanged-when-unknown property guarantees existing tests
//     and call paths see no behavior change.
//   - contextWindow >= scaleBaselineContextWindow (the typical
//     large/normal floor) -> the input options are returned unchanged. We
//     do NOT inflate beyond defaults; that would break behavior stability
//     for large-context strong models.
//   - contextWindow in (0, scaleBaselineContextWindow) -> chunk size,
//     FinderReadLines, and FinderReadBytes are scaled DOWN by the ratio
//     (contextWindow / baseline), with hard floors so the finder can still
//     make progress on a tiny window.
//
// "Explicit non-default Options values are preserved": the helper only
// overrides a value when the caller left it at the package default. For
// FinderReadLines / FinderReadBytes the default IS the zero value (the
// Options struct does not resolve them; finderReadCaps substitutes
// DefaultFinderReadLines/Bytes at consume time), so the check is
// `o.Limits.FinderReadLines == 0`. For ChunkSize the resolved value is always
// non-zero (resolve() fills in DefaultChunkSize when the caller leaves it
// at zero), so the check is "still equal to DefaultChunkSize" — the
// accepted-by-spec trade-off documented above. FinderHistoryTokens is
// scaled only when the caller has already opted in by setting a positive
// value; the off-by-default invariant is preserved.
func scaleFinderForContext(o Options, contextWindow int) Options {
	if contextWindow <= 0 || contextWindow >= scaleBaselineContextWindow {
		return o
	}

	// Linear scaling factor in (0, 1). A 64k model gets the defaults (and
	// would already have returned above); an 8k model gets 1/8th; smaller
	// still gets the floor values.
	ratio := float64(contextWindow) / float64(scaleBaselineContextWindow)

	// ChunkSize: only override if the caller left it at the default. The
	// resolved value is DefaultChunkSize, and any other positive value is
	// treated as explicit.
	if o.Limits.ChunkSize == DefaultChunkSize {
		o.Limits.ChunkSize = clampChunk(int(float64(DefaultChunkSize) * ratio))
	}

	// Per-read caps: 0 means "use DefaultFinderReadLines/Bytes"; any other
	// value (positive = explicit, negative = "use looser agent defaults")
	// is preserved.
	if o.Limits.FinderReadLines == 0 {
		o.Limits.FinderReadLines = clampReadLines(int(float64(DefaultFinderReadLines) * ratio))
	}
	if o.Limits.FinderReadBytes == 0 {
		o.Limits.FinderReadBytes = clampReadBytes(int(float64(DefaultFinderReadBytes) * ratio))
	}

	// History compaction: ONLY scale when the caller already opted in with
	// a positive threshold. Compaction is intentionally off by default
	// (DefaultFinderHistoryTokens is a reference, not an on-switch), and we
	// do not flip the on-switch here — that would change the on-the-wire
	// cache-safety profile. The scaled value tracks the context window
	// (~25% of window) so a small-context model compacts at a small, sane
	// threshold rather than the 60k reference that would never trip.
	if o.Limits.FinderHistoryTokens > 0 {
		scaled := int64(float64(contextWindow) * scaleHistoryFraction)
		if scaled < scaleFloorHistoryTokens {
			scaled = scaleFloorHistoryTokens
		}
		o.Limits.FinderHistoryTokens = scaled
		o.Limits.FinderLimits.HistoryTokenBudget = scaled
	}

	return o
}

// scaleBaselineContextWindow is the context-window size above which we leave
// the defaults untouched. 64_000 is a comfortable floor for the strong
// models the default knobs were tuned against: Anthropic 4.x, GPT-4-class
// models, and Gemini 2.x all sit well above this. Models at exactly 64k
// already have the default behavior.
const scaleBaselineContextWindow = 64_000

// scaleHistoryFraction sets the per-run compaction threshold to this
// fraction of the context window when the caller opted in to compaction.
// 0.25 is the conventional "compact when history is a quarter of the
// window" rule of thumb — small enough to keep responses in budget, large
// enough to give the agent several turns between compactions.
const scaleHistoryFraction = 0.25

// Hard floors for the scaled values. A finder still has to make progress on
// a 1k-context model; these floors are tight but not zero.
const (
	scaleFloorChunk         = 1
	scaleFloorReadLines     = 100
	scaleFloorReadBytes     = 8 * 1024 // 8 KB
	scaleFloorHistoryTokens = 1_000
)

func clampChunk(n int) int {
	if n < scaleFloorChunk {
		return scaleFloorChunk
	}
	return n
}

func clampReadLines(n int) int {
	if n < scaleFloorReadLines {
		return scaleFloorReadLines
	}
	return n
}

func clampReadBytes(n int) int {
	if n < scaleFloorReadBytes {
		return scaleFloorReadBytes
	}
	return n
}
