package eval

// DupChannel labels which duplicate-detection channel a labeled DupPair
// exercises. The taxonomy mirrors the identity-collision shapes named in
// bugbot-ezmx's design review: paraphrase (same spot, different wording),
// cross-lens (same spot, different finder lens/vocabulary), caller/callee
// (same defect described from two different call-chain locations), and
// rename (same defect, but a symbol rename shifted the line and/or wording).
type DupChannel string

const (
	DupChannelParaphrase   DupChannel = "paraphrase"
	DupChannelCrossLens    DupChannel = "cross_lens"
	DupChannelCallerCallee DupChannel = "caller_callee"
	DupChannelRename       DupChannel = "rename"
)

// DupCandidate is one finding-shaped side of a labeled duplicate pair: just
// the fields the current identity layer's decision function
// (funnel.SimilarFinding) consults — File, Line, Desc — plus an informational
// Lens label that documents provenance for humans reading the corpus but is
// NOT consulted by the decision (SimilarFinding is deliberately lens-blind:
// see internal/funnel/cluster.go).
type DupCandidate struct {
	File string
	Line int
	Desc string
	Lens string
}

// DupPair is one ground-truth labeled pair from the offline dup-eval corpus.
// SameDefect is the human-assigned ground truth ("do A and B describe the
// same underlying defect"), assigned independent of what the current
// identity layer decides — the whole point of the corpus is to measure where
// the current layer agrees or disagrees with ground truth, INCLUDING cases
// where it is known to disagree (that disagreement is the baseline).
type DupPair struct {
	Name       string
	Channel    DupChannel
	A, B       DupCandidate
	SameDefect bool
}

// BuiltinDupPairs returns the seeded labeled duplicate-pair corpus. It covers
// four channels with several pairs each, mixing SameDefect=true and
// SameDefect=false pairs per channel so the eval measures both recall (real
// duplicates caught) and precision (distinct defects NOT merged) of the
// current v2 identity layer's cross-scan similarity decision
// (funnel.SimilarFinding: same file, lines within funnel.DefaultMergeWindow,
// description-token Jaccard >= the merge threshold).
//
// Some pairs are deliberately chosen to fail under the CURRENT layer — a
// heavily reworded paraphrase, a cross-lens pair whose vocabularies barely
// overlap, a caller/callee pair split across files, a rename that drifted the
// line past the merge window — because bugbot-ezmx.8 lands the BASELINE:
// the corpus is labeled by ground truth, not by what current code does, and
// showing where v2 already falls short is exactly the point (later identity
// work, e.g. bugbot-ezmx.1's v3, is judged against this same corpus).
func BuiltinDupPairs() []DupPair {
	return []DupPair{
		// --- paraphrase: same location, reworded description -----------------
		{
			Name:    "paraphrase-nil-deref-close-wording",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "server/handler.go", Line: 88,
				Desc: "response writer may be nil when the request context is cancelled, causing a nil pointer dereference",
				Lens: "nil-safety"},
			B: DupCandidate{File: "server/handler.go", Line: 88,
				Desc: "possible nil dereference on the response writer if the request context is already cancelled",
				Lens: "nil-safety"},
			SameDefect: true,
		},
		{
			Name:    "paraphrase-resource-leak-close-wording",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "ingest/reader.go", Line: 142,
				Desc: "file handle opened for the manifest is never closed on the error return path, leaking a file descriptor",
				Lens: "resource-leak"},
			B: DupCandidate{File: "ingest/reader.go", Line: 142,
				Desc: "manifest file descriptor leaks because the early error return skips the close call",
				Lens: "resource-leak"},
			SameDefect: true,
		},
		{
			Name:    "paraphrase-off-by-one-close-wording",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "internal/buf/ring.go", Line: 55,
				Desc: "loop bound uses <= instead of < so the ring buffer index writes one slot past the end",
				Lens: "boundary"},
			B: DupCandidate{File: "internal/buf/ring.go", Line: 55,
				Desc: "off by one in the ring buffer bound allows an out of range write past the last slot",
				Lens: "boundary"},
			SameDefect: true,
		},
		{
			Name:    "paraphrase-heavy-rewrite-false-negative",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "auth/token.go", Line: 210,
				Desc: "expired refresh tokens are still accepted because the expiry comparison uses the wrong clock source",
				Lens: "auth"},
			B: DupCandidate{File: "auth/token.go", Line: 210,
				Desc: "stale credentials pass validation: the freshness check reads time from a source that never advances in this path",
				Lens: "auth"},
			// Same bug, but the vocabulary barely overlaps (only generic words
			// like "the", "path" survive tokenization, and those are stripped
			// or don't carry signal) — a known miss for the current jaccard
			// guard. Labeled true by ground truth; expect the current layer
			// to score this a false negative.
			SameDefect: true,
		},
		{
			Name:    "paraphrase-distinct-nearby-defects",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "internal/buf/ring.go", Line: 55,
				Desc: "off by one in the ring buffer bound allows an out of range write past the last slot",
				Lens: "boundary"},
			B: DupCandidate{File: "internal/buf/ring.go", Line: 58,
				Desc: "the ring buffer's read cursor is never reset after a wraparound, returning stale bytes",
				Lens: "boundary"},
			// Two real, distinct defects three lines apart in the same
			// function. Precision probe: description overlap is low, so the
			// current layer should correctly keep these separate.
			SameDefect: false,
		},

		// --- cross-lens: same location, different finder vocabulary ---------
		{
			Name:    "cross-lens-sql-injection-agreeing-wording",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "store/query.go", Line: 301,
				Desc: "user-supplied filter value is concatenated directly into the SQL query string, allowing SQL injection",
				Lens: "security"},
			B: DupCandidate{File: "store/query.go", Line: 301,
				Desc: "the filter string is interpolated into the query without parameterization, an injection risk",
				Lens: "correctness"},
			SameDefect: true,
		},
		{
			Name:    "cross-lens-race-agreeing-wording",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "engine/scheduler.go", Line: 77,
				Desc: "the worker pool counter is incremented without holding the mutex, a data race under -race",
				Lens: "concurrency"},
			B: DupCandidate{File: "engine/scheduler.go", Line: 77,
				Desc: "concurrent goroutines mutate the pool counter outside the mutex, causing a race condition",
				Lens: "reliability"},
			SameDefect: true,
		},
		{
			Name:    "cross-lens-security-vs-perf-vocabulary-false-negative",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "api/upload.go", Line: 64,
				Desc: "request bodies are read fully into memory with no size cap, letting an attacker exhaust the heap",
				Lens: "security"},
			B: DupCandidate{File: "api/upload.go", Line: 64,
				Desc: "unbounded allocation per request under load causes high GC pressure and latency spikes",
				Lens: "performance"},
			// One root cause (no read-size cap), two completely different
			// framings from two lenses. Labeled true; the security/perf
			// vocabularies barely overlap, so this is a known cross-lens miss
			// — exactly the "prose cliff" the bead calls out.
			SameDefect: true,
		},
		{
			Name:    "cross-lens-distinct-bugs-same-function",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "api/upload.go", Line: 64,
				Desc: "request bodies are read fully into memory with no size cap, letting an attacker exhaust the heap",
				Lens: "security"},
			B: DupCandidate{File: "api/upload.go", Line: 66,
				Desc: "the content-type header is trusted without validation, allowing a MIME confusion attack",
				Lens: "security"},
			// Two real, distinct security findings from the SAME lens two
			// lines apart. Precision probe.
			SameDefect: false,
		},
		{
			Name:    "cross-lens-lock-order-agreeing-wording",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "store/tx.go", Line: 19,
				Desc: "the transaction lock is acquired before the cache lock here, while the writer path takes them in reverse order, a deadlock",
				Lens: "concurrency"},
			B: DupCandidate{File: "store/tx.go", Line: 19,
				Desc: "lock ordering here is transaction then cache, opposite of the writer, which can deadlock",
				Lens: "static-analysis"},
			SameDefect: true,
		},

		// --- caller/callee: same defect, different call-chain location ------
		{
			Name:    "caller-callee-nil-deref-root-cause-vs-symptom",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/parse/decode.go", Line: 40,
				Desc: "Decode returns a nil *Header on a truncated input instead of an error",
				Lens: "nil-safety"},
			B: DupCandidate{File: "internal/parse/consumer.go", Line: 512,
				Desc: "the header returned by Decode is dereferenced immediately without a nil check, panicking on truncated input",
				Lens: "nil-safety"},
			// Same defect (Decode's contract violation), reported once at the
			// root cause and once at the call site that crashes on it. Far
			// apart in both file and line — the current layer's merge window
			// is same-file only, so this is an expected recall gap (false
			// negative) the caller/callee channel exists to surface.
			SameDefect: true,
		},
		{
			Name:    "caller-callee-unchecked-error-root-cause-vs-symptom",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/config/load.go", Line: 12,
				Desc: "Load swallows the file read error and returns a zero-value Config instead of propagating it",
				Lens: "error-handling"},
			B: DupCandidate{File: "cmd/bugbot/main.go", Line: 30,
				Desc: "main proceeds with an empty config after Load silently fails, then panics dereferencing config fields",
				Lens: "error-handling"},
			SameDefect: true,
		},
		{
			Name:    "caller-callee-close-in-scope-should-merge",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/repro/agent.go", Line: 220,
				Desc: "runTurn does not check the sandbox exec error before reading its stdout",
				Lens: "error-handling"},
			B: DupCandidate{File: "internal/repro/agent.go", Line: 224,
				Desc: "the exec error from runTurn's sandbox call is dropped, so a failed command's empty stdout is parsed anyway",
				Lens: "error-handling"},
			// Root cause and symptom are 4 lines apart in the SAME file and
			// share enough vocabulary to be within window+jaccard: this pair
			// is expected to be caught by the current layer, giving the
			// caller/callee channel at least one true positive.
			SameDefect: true,
		},
		{
			Name:    "caller-callee-unrelated-bugs-adjacent-call-chain",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/parse/decode.go", Line: 40,
				Desc: "Decode returns a nil *Header on a truncated input instead of an error",
				Lens: "nil-safety"},
			B: DupCandidate{File: "internal/parse/decode.go", Line: 43,
				Desc: "the length prefix is read as a signed int and never checked for negative values before allocating a buffer",
				Lens: "boundary"},
			// Two real, distinct bugs three lines apart in the callee. Not a
			// caller/callee duplicate — precision probe for the channel.
			SameDefect: false,
		},

		// --- rename: same defect, symbol renamed / code shifted --------------
		{
			Name:    "rename-small-shift-should-merge",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/store/query.go", Line: 90,
				Desc: "the userID filter is compared with strings.EqualFold, allowing a case-insensitive account takeover",
				Lens: "security"},
			B: DupCandidate{File: "internal/store/query.go", Line: 96,
				// accountID replaces userID after a rename; a few lines of
				// unrelated refactor were inserted above it.
				Desc: "the accountID filter is compared with strings.EqualFold, allowing a case-insensitive account takeover",
				Lens: "security"},
			// Within the merge window (6 lines) and high token overlap even
			// after the rename — expected true positive.
			SameDefect: true,
		},
		{
			Name:    "rename-large-shift-false-negative",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/funnel/hypothesize.go", Line: 120,
				Desc: "chunkBudget is computed from the stale finder count captured before the retry loop adds more finders",
				Lens: "correctness"},
			B: DupCandidate{File: "internal/funnel/hypothesize.go", Line: 205,
				// chunkBudget renamed to unitBudget and the function moved
				// during a refactor, landing 85 lines away — outside
				// DefaultMergeWindow (10 lines).
				Desc: "unitBudget is computed from the stale finder count captured before the retry loop adds more finders",
				Lens: "correctness"},
			// Same defect, but the rename's line drift exceeds the merge
			// window: expected false negative under the current layer. This
			// is exactly the gap bugbot-ezmx.6 (rename tracking) targets.
			SameDefect: true,
		},
		{
			Name:    "rename-wording-drift-false-negative",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/tui/palette.go", Line: 150,
				Desc: "the fuzzyMatch scorer never resets its memo between queries, so stale scores leak into the next search",
				Lens: "correctness"},
			B: DupCandidate{File: "internal/tui/palette.go", Line: 154,
				// fuzzyMatch renamed to rankCandidates during a rewrite; the
				// description was also reworded, dropping shared vocabulary.
				Desc: "rankCandidates keeps its cache across calls, so an older search's entries pollute a fresh one",
				Lens: "correctness"},
			// Same defect (stale-memo leak), renamed AND reworded — both the
			// window and the jaccard guard are stressed. Expected false
			// negative; the compounding case for the rename channel.
			SameDefect: true,
		},
		{
			Name:    "rename-unrelated-symbol-collision",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/store/query.go", Line: 90,
				Desc: "the userID filter is compared with strings.EqualFold, allowing a case-insensitive account takeover",
				Lens: "security"},
			B: DupCandidate{File: "internal/store/query.go", Line: 93,
				Desc: "the orgID filter has no bounds check and accepts an empty string, matching every organization's rows",
				Lens: "security"},
			// A distinct bug on a nearby renamed-looking symbol (orgID vs
			// userID/accountID) — precision probe for the rename channel:
			// proximity alone must not merge unrelated defects.
			SameDefect: false,
		},
	}
}
