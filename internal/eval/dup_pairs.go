package eval

import (
	"strings"

	"github.com/dpoage/bugbot/internal/domain"
)

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

// DupCandidate is one finding-shaped side of a labeled duplicate pair.
//
// File/Line/Desc/Lens are what funnel.SimilarFinding (the tiebreaker stage)
// consults; Lens is informational provenance only — SimilarFinding is
// deliberately lens-blind (see internal/funnel/cluster.go).
//
// DefectKind/Subject are the bugbot-ezmx.1 v3-identity ground truth for this
// candidate: the defect_kind and subject symbol a real triage run would have
// attached, independent of Desc's wording. Two candidates describing the
// SAME defect carry the SAME (kind, subject) where the corpus's narrative
// says a real finder would agree on them (this is what lets cross-lens
// duplicates converge at exact fingerprint equality without going through
// description similarity at all); a distinct nearby defect carries a
// different kind and/or subject specifically to probe the kind gate and
// exact-fingerprint stages independently of Desc's jaccard overlap.
//
// Source is optional synthetic Go source text backing File (see synthSource
// in dup_eval.go): when non-empty, RunDupEval writes it to a scratch
// directory so the REAL funnel.LocusResolver (bugbot-ezmx.5's symbol/content
// anchors) resolves Line to a locus exactly as it would against a real repo
// checkout, instead of degrading to the line-number fallback. Candidates
// that share a File must supply byte-identical Source (or leave it empty on
// the second occurrence) since it backs the same file once per corpus run.
type DupCandidate struct {
	File       string
	Line       int
	Desc       string
	Lens       string
	DefectKind domain.DefectKind
	Subject    string
	Source     string
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

// --- synthetic source fixtures ---------------------------------------------
//
// Each constant below backs one corpus file with real (if minimal) Go source
// text so funnel.NewLocusResolver's tree-sitter symbol anchor — the SAME
// locus derivation triage_streaming.go step 3 uses against a real repo — can
// resolve an enclosing declaration for every candidate line the corpus below
// references in that file, instead of degrading to the "L:<line>" fallback.
// See synthSource's doc in dup_eval.go for why this is necessary and how it
// avoids re-implementing the resolver.

// srcHandlerGo backs server/handler.go: a single Greeting function covering
// line 88 (the paraphrase nil-deref pair).
var srcHandlerGo = synthSource("fixture", funcSpec{name: "Greeting", start: 80, end: 92})

// srcReaderGo backs ingest/reader.go: a single ReadManifest function covering
// line 142 (the paraphrase resource-leak pair).
var srcReaderGo = synthSource("fixture", funcSpec{name: "ReadManifest", start: 135, end: 148})

// srcRingGo backs internal/buf/ring.go: a single Push function spanning both
// line 55 (the off-by-one bound check) and line 58 (the stale read-cursor
// bug three lines later) so the paraphrase off-by-one pair and the
// paraphrase distinct-nearby-defects precision probe share the SAME
// enclosing locus, exactly like two real bugs three lines apart in one
// function would — the kind/subject mismatch, not locus, is what must keep
// the precision probe from merging.
var srcRingGo = synthSource("fixture", funcSpec{name: "Push", start: 50, end: 62})

// srcTokenGo backs auth/token.go: a single IsExpired function covering line
// 210 (the paraphrase heavy-rewrite pair — the case whose wording overlap is
// deliberately too low for the SimilarFinding tiebreaker, so it must
// converge at exact-fingerprint equality on the shared locus/kind/subject).
var srcTokenGo = synthSource("fixture", funcSpec{name: "IsExpired", start: 204, end: 216})

// srcQueryGo backs internal/store/query.go across TWO unrelated channels
// that happen to share the file name: cross-lens's sql-injection pair at
// line 301, and the rename channel's three pairs clustered around lines
// 90-96. Two functions, far apart, so their loci never collide.
var srcQueryGo = synthSource("fixture",
	funcSpec{name: "filterByAccount", start: 85, end: 100},
	funcSpec{name: "buildFilterSQL", start: 295, end: 306},
)

// srcSchedulerGo backs engine/scheduler.go: a single runWorker function
// covering line 77 (the cross-lens race pair).
var srcSchedulerGo = synthSource("fixture", funcSpec{name: "runWorker", start: 70, end: 84})

// srcUploadGo backs api/upload.go: a single readBody function spanning both
// line 64 (the unbounded-read pair, both the cross-lens vocabulary-drift
// case and the precision probe) and line 66 (the precision probe's distinct
// MIME-confusion defect three lines later) — same enclosing function, so
// again kind/subject (not locus) must separate the precision probe.
var srcUploadGo = synthSource("fixture", funcSpec{name: "readBody", start: 58, end: 70})

// srcTxGo backs store/tx.go: a single Commit function covering line 19 (the
// cross-lens lock-order pair).
var srcTxGo = synthSource("fixture", funcSpec{name: "Commit", start: 12, end: 26})

// srcDecodeGo backs internal/parse/decode.go: a single Decode function
// spanning both line 40 (nil-Header contract violation, referenced by the
// caller/callee root-cause pair) and line 43 (the unrelated negative-length
// precision probe three lines later, same enclosing function).
var srcDecodeGo = synthSource("fixture", funcSpec{name: "Decode", start: 35, end: 48})

// srcConsumerGo backs internal/parse/consumer.go: a single HandleFrame
// function covering line 512 (the caller/callee root-cause pair's symptom
// site, in a DIFFERENT file from Decode — this pair stays a false negative
// under the composed v3 stack because FingerprintV3 keys on file, so a
// cross-file root-cause/symptom split can never mint the same fingerprint;
// only a same-file merge window closes that gap).
var srcConsumerGo = synthSource("fixture", funcSpec{name: "HandleFrame", start: 505, end: 518})

// srcLoadGo backs internal/config/load.go: a single Load function covering
// line 12 (the caller/callee unchecked-error root cause; also a cross-file
// split, same false-negative shape as decode.go/consumer.go above).
var srcLoadGo = synthSource("fixture", funcSpec{name: "Load", start: 6, end: 18})

// srcMainGo backs cmd/bugbot/main.go: a single main function covering line
// 30 (the caller/callee unchecked-error symptom site).
var srcMainGo = synthSource("fixture", funcSpec{name: "main", start: 22, end: 34})

// srcAgentGo backs internal/repro/agent.go: a single runTurn function
// spanning both line 220 and line 224 (the caller/callee close-in-scope
// pair — same file, close together, already a true positive pre-v3).
var srcAgentGo = synthSource("fixture", funcSpec{name: "runTurn", start: 215, end: 230})

// srcHypothesizeGo backs internal/funnel/hypothesize.go: a single
// computeChunkBudget function spanning BOTH line 120 and line 205. The
// corpus's rename-large-shift pair models a refactor that inserted ~80
// lines of unrelated code ABOVE the bug (an internal variable rename,
// chunkBudget -> unitBudget, is part of the same edit) without moving it out
// of computeChunkBudget itself. That is exactly the drift
// bugbot-ezmx.5's symbol-anchored locus is designed to survive: the
// enclosing declaration's NAME is unchanged, so both lines resolve to the
// identical locus and the pair converges at exact-fingerprint equality
// despite an 85-line drift that leaves it outside DefaultMergeWindow and
// therefore invisible to the SimilarFinding tiebreaker alone.
var srcHypothesizeGo = synthSource("fixture", funcSpec{name: "computeChunkBudget", start: 30, end: 260})

// srcPaletteGo backs internal/tui/palette.go with TWO functions: fuzzyMatch
// (covering line 150) and rankCandidates (covering line 154). Unlike
// hypothesize.go above, the corpus's rename-wording-drift pair models the
// enclosing FUNCTION itself being renamed (fuzzyMatch -> rankCandidates), not
// just an internal variable — a genuinely different top-level declaration
// name, so the symbol-anchored locus does NOT converge, and the reworded
// description also falls under the SimilarFinding jaccard threshold. This
// pair is expected to remain a false negative under the composed v3 stack:
// a real function-level rename is a gap this identity layer alone cannot
// close (it would need store-level rename tracking to correlate the two
// fingerprints, which is out of scope for a pure predicate eval).
var srcPaletteGo = synthSource("fixture",
	funcSpec{name: "fuzzyMatch", start: 145, end: 152},
	funcSpec{name: "rankCandidates", start: 153, end: 160},
)

// funcSpec places one synthetic top-level Go function within a generated
// source fixture; see synthSource.
type funcSpec struct {
	name       string
	start, end int // 1-based, inclusive; start is the "func" line, end the closing brace line
}

// synthSource renders minimal (non-compiling-checked, but syntactically
// valid) Go source text containing one package clause and the given
// functions, each occupying EXACTLY its declared [start,end] line range. It
// exists so the dup-pair corpus can back a File/Line pair with real source
// text and exercise funnel.LocusResolver's actual tree-sitter symbol anchor
// — the identical mechanism triage_streaming.go step 3 uses to compute
// domain.FingerprintV3's locus argument against a real repo checkout —
// without checking a fabricated multi-hundred-line fixture repo into the
// corpus file itself. The function bodies carry no real statements (blank
// lines only): only the declaration's name and line span matter to locus
// resolution, which reads no further into the body.
func synthSource(pkg string, specs ...funcSpec) string {
	maxLine := 1
	for _, s := range specs {
		if s.end > maxLine {
			maxLine = s.end
		}
	}
	lines := make([]string, maxLine+1) // 1-based; index 0 unused
	lines[1] = "package " + pkg
	for _, s := range specs {
		lines[s.start] = "func " + s.name + "() {"
		lines[s.end] = "}"
	}
	return strings.Join(lines[1:], "\n") + "\n"
}

// BuiltinDupPairs returns the seeded labeled duplicate-pair corpus. It covers
// four channels with several pairs each, mixing SameDefect=true and
// SameDefect=false pairs per channel so the eval measures both recall (real
// duplicates caught) and precision (distinct defects NOT merged).
//
// RunDupEval scores the pairs against the COMPOSED deterministic v3 identity
// stack (kind gate -> exact FingerprintV3 equality -> SimilarFinding
// tiebreak; see scoreV3 in dup_eval.go), not funnel.SimilarFinding alone.
// Some pairs are still deliberately chosen to fail under that composed
// stack — a function-level rename, a cross-file root-cause/symptom split —
// because showing where the CURRENT layer falls short is exactly the
// corpus's point: it is labeled by ground truth, not by what current code
// does, and remaining gaps are what later identity work is judged against.
func BuiltinDupPairs() []DupPair {
	return []DupPair{
		// --- paraphrase: same location, reworded description -----------------
		{
			Name:    "paraphrase-nil-deref-close-wording",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "server/handler.go", Line: 88,
				Desc: "response writer may be nil when the request context is cancelled, causing a nil pointer dereference",
				Lens: "nil-safety", DefectKind: domain.DefectNilDeref, Subject: "Greeting", Source: srcHandlerGo},
			B: DupCandidate{File: "server/handler.go", Line: 88,
				Desc: "possible nil dereference on the response writer if the request context is already cancelled",
				Lens: "nil-safety", DefectKind: domain.DefectNilDeref, Subject: "Greeting", Source: srcHandlerGo},
			SameDefect: true,
		},
		{
			Name:    "paraphrase-resource-leak-close-wording",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "ingest/reader.go", Line: 142,
				Desc: "file handle opened for the manifest is never closed on the error return path, leaking a file descriptor",
				Lens: "resource-leak", DefectKind: domain.DefectResourceLeak, Subject: "ReadManifest", Source: srcReaderGo},
			B: DupCandidate{File: "ingest/reader.go", Line: 142,
				Desc: "manifest file descriptor leaks because the early error return skips the close call",
				Lens: "resource-leak", DefectKind: domain.DefectResourceLeak, Subject: "ReadManifest", Source: srcReaderGo},
			SameDefect: true,
		},
		{
			Name:    "paraphrase-off-by-one-close-wording",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "internal/buf/ring.go", Line: 55,
				Desc: "loop bound uses <= instead of < so the ring buffer index writes one slot past the end",
				Lens: "boundary", DefectKind: domain.DefectBounds, Subject: "Push", Source: srcRingGo},
			B: DupCandidate{File: "internal/buf/ring.go", Line: 55,
				Desc: "off by one in the ring buffer bound allows an out of range write past the last slot",
				Lens: "boundary", DefectKind: domain.DefectBounds, Subject: "Push", Source: srcRingGo},
			SameDefect: true,
		},
		{
			Name:    "paraphrase-heavy-rewrite-false-negative",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "auth/token.go", Line: 210,
				Desc: "expired refresh tokens are still accepted because the expiry comparison uses the wrong clock source",
				Lens: "auth", DefectKind: domain.DefectContractViolation, Subject: "IsExpired", Source: srcTokenGo},
			B: DupCandidate{File: "auth/token.go", Line: 210,
				Desc: "stale credentials pass validation: the freshness check reads time from a source that never advances in this path",
				Lens: "auth", DefectKind: domain.DefectContractViolation, Subject: "IsExpired", Source: srcTokenGo},
			// Same bug, but the vocabulary barely overlaps (only generic words
			// like "the", "path" survive tokenization, and those are stripped
			// or don't carry signal) — a known miss for the SimilarFinding
			// jaccard guard. Under the composed v3 stack it is still caught:
			// same file/line -> same locus, same ground-truth kind/subject ->
			// identical FingerprintV3, so it converges at exact-fp WITHOUT
			// ever consulting description similarity. Labeled true.
			SameDefect: true,
		},
		{
			Name:    "paraphrase-distinct-nearby-defects",
			Channel: DupChannelParaphrase,
			A: DupCandidate{File: "internal/buf/ring.go", Line: 55,
				Desc: "off by one in the ring buffer bound allows an out of range write past the last slot",
				Lens: "boundary", DefectKind: domain.DefectBounds, Subject: "writeIndex", Source: srcRingGo},
			B: DupCandidate{File: "internal/buf/ring.go", Line: 58,
				Desc: "the ring buffer's read cursor is never reset after a wraparound, returning stale bytes",
				Lens: "boundary", DefectKind: domain.DefectLogic, Subject: "readCursor", Source: srcRingGo},
			// Two real, distinct defects three lines apart in the same
			// function (same locus). Precision probe: kinds differ
			// (bounds vs logic), so the kind gate alone must keep these
			// separate, independent of locus or description overlap.
			SameDefect: false,
		},

		// --- cross-lens: same location, different finder vocabulary ---------
		{
			Name:    "cross-lens-sql-injection-agreeing-wording",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "internal/store/query.go", Line: 301,
				Desc: "user-supplied filter value is concatenated directly into the SQL query string, allowing SQL injection",
				Lens: "security", DefectKind: domain.DefectInjection, Subject: "buildFilterSQL", Source: srcQueryGo},
			B: DupCandidate{File: "internal/store/query.go", Line: 301,
				Desc: "the filter string is interpolated into the query without parameterization, an injection risk",
				Lens: "correctness", DefectKind: domain.DefectInjection, Subject: "buildFilterSQL", Source: srcQueryGo},
			SameDefect: true,
		},
		{
			Name:    "cross-lens-race-agreeing-wording",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "engine/scheduler.go", Line: 77,
				Desc: "the worker pool counter is incremented without holding the mutex, a data race under -race",
				Lens: "concurrency", DefectKind: domain.DefectRace, Subject: "runWorker", Source: srcSchedulerGo},
			B: DupCandidate{File: "engine/scheduler.go", Line: 77,
				Desc: "concurrent goroutines mutate the pool counter outside the mutex, causing a race condition",
				Lens: "reliability", DefectKind: domain.DefectRace, Subject: "runWorker", Source: srcSchedulerGo},
			SameDefect: true,
		},
		{
			Name:    "cross-lens-security-vs-perf-vocabulary-false-negative",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "api/upload.go", Line: 64,
				Desc: "request bodies are read fully into memory with no size cap, letting an attacker exhaust the heap",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "readBody", Source: srcUploadGo},
			B: DupCandidate{File: "api/upload.go", Line: 64,
				Desc: "unbounded allocation per request under load causes high GC pressure and latency spikes",
				Lens: "performance", DefectKind: domain.DefectOther, Subject: "readBody", Source: srcUploadGo},
			// One root cause (no read-size cap), two completely different
			// framings from two lenses. The security/perf vocabularies
			// barely overlap, so this is a known SimilarFinding miss — the
			// "prose cliff" the bead calls out. Under the composed v3 stack
			// it converges at exact-fp: same file/line/locus, and both
			// lenses' triage-assigned kind/subject agree (a real finder does
			// not invent a new defect_kind per lens for the same root
			// cause). Labeled true.
			SameDefect: true,
		},
		{
			Name:    "cross-lens-distinct-bugs-same-function",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "api/upload.go", Line: 64,
				Desc: "request bodies are read fully into memory with no size cap, letting an attacker exhaust the heap",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "readBody", Source: srcUploadGo},
			B: DupCandidate{File: "api/upload.go", Line: 66,
				Desc: "the content-type header is trusted without validation, allowing a MIME confusion attack",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "contentTypeCheck", Source: srcUploadGo},
			// Two real, distinct security findings from the SAME lens two
			// lines apart (same locus, same kind — the kind gate alone
			// cannot separate this one). Precision probe: subjects differ,
			// so exact-fp does not merge them, and description overlap is
			// too low for the SimilarFinding tiebreaker either.
			SameDefect: false,
		},
		{
			Name:    "cross-lens-lock-order-agreeing-wording",
			Channel: DupChannelCrossLens,
			A: DupCandidate{File: "store/tx.go", Line: 19,
				Desc: "the transaction lock is acquired before the cache lock here, while the writer path takes them in reverse order, a deadlock",
				Lens: "concurrency", DefectKind: domain.DefectRace, Subject: "Commit", Source: srcTxGo},
			B: DupCandidate{File: "store/tx.go", Line: 19,
				Desc: "lock ordering here is transaction then cache, opposite of the writer, which can deadlock",
				Lens: "static-analysis", DefectKind: domain.DefectRace, Subject: "Commit", Source: srcTxGo},
			SameDefect: true,
		},

		// --- caller/callee: same defect, different call-chain location ------
		{
			Name:    "caller-callee-nil-deref-root-cause-vs-symptom",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/parse/decode.go", Line: 40,
				Desc: "Decode returns a nil *Header on a truncated input instead of an error",
				Lens: "nil-safety", DefectKind: domain.DefectContractViolation, Subject: "Decode", Source: srcDecodeGo},
			B: DupCandidate{File: "internal/parse/consumer.go", Line: 512,
				Desc: "the header returned by Decode is dereferenced immediately without a nil check, panicking on truncated input",
				Lens: "nil-safety", DefectKind: domain.DefectNilDeref, Subject: "HandleFrame", Source: srcConsumerGo},
			// Same defect (Decode's contract violation), reported once at the
			// root cause and once at the call site that crashes on it. A
			// DIFFERENT file, so FingerprintV3's file component alone rules
			// out exact-fp regardless of locus, and SimilarFinding's window
			// is same-file only. Ground-truth kind even differs deliberately
			// (contract-violation at the root cause vs the nil-deref it
			// causes at the symptom) since a real cross-file root-cause/
			// symptom pair is not internally consistent about which single
			// defect_kind names it. Expected false negative under every
			// layer in this stack — closing cross-file root-cause merges is
			// out of scope for a same-file identity/tiebreak composition.
			SameDefect: true,
		},
		{
			Name:    "caller-callee-unchecked-error-root-cause-vs-symptom",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/config/load.go", Line: 12,
				Desc: "Load swallows the file read error and returns a zero-value Config instead of propagating it",
				Lens: "error-handling", DefectKind: domain.DefectUncheckedError, Subject: "Load", Source: srcLoadGo},
			B: DupCandidate{File: "cmd/bugbot/main.go", Line: 30,
				Desc: "main proceeds with an empty config after Load silently fails, then panics dereferencing config fields",
				Lens: "error-handling", DefectKind: domain.DefectNilDeref, Subject: "main", Source: srcMainGo},
			// Same cross-file shape as above: expected false negative.
			SameDefect: true,
		},
		{
			Name:    "caller-callee-close-in-scope-should-merge",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/repro/agent.go", Line: 220,
				Desc: "runTurn does not check the sandbox exec error before reading its stdout",
				Lens: "error-handling", DefectKind: domain.DefectUncheckedError, Subject: "runTurn", Source: srcAgentGo},
			B: DupCandidate{File: "internal/repro/agent.go", Line: 224,
				Desc: "the exec error from runTurn's sandbox call is dropped, so a failed command's empty stdout is parsed anyway",
				Lens: "error-handling", DefectKind: domain.DefectUncheckedError, Subject: "runTurn", Source: srcAgentGo},
			// Root cause and symptom are 4 lines apart in the SAME file and
			// share enough vocabulary to be within window+jaccard — already
			// caught by the SimilarFinding tiebreaker pre-v3, and the shared
			// locus/kind here would also converge at exact-fp if the subject
			// (both "runTurn") coincided at the same line, which it does not
			// — this pair specifically exercises the tiebreaker stage.
			SameDefect: true,
		},
		{
			Name:    "caller-callee-unrelated-bugs-adjacent-call-chain",
			Channel: DupChannelCallerCallee,
			A: DupCandidate{File: "internal/parse/decode.go", Line: 40,
				Desc: "Decode returns a nil *Header on a truncated input instead of an error",
				Lens: "nil-safety", DefectKind: domain.DefectContractViolation, Subject: "Decode", Source: srcDecodeGo},
			B: DupCandidate{File: "internal/parse/decode.go", Line: 43,
				Desc: "the length prefix is read as a signed int and never checked for negative values before allocating a buffer",
				Lens: "boundary", DefectKind: domain.DefectBounds, Subject: "Decode", Source: srcDecodeGo},
			// Two real, distinct bugs three lines apart in the callee, same
			// locus (same enclosing function) but different kind — the kind
			// gate alone keeps these separate. Precision probe.
			SameDefect: false,
		},

		// --- rename: same defect, symbol renamed / code shifted --------------
		{
			Name:    "rename-small-shift-should-merge",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/store/query.go", Line: 90,
				Desc: "the userID filter is compared with strings.EqualFold, allowing a case-insensitive account takeover",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "userID", Source: srcQueryGo},
			B: DupCandidate{File: "internal/store/query.go", Line: 96,
				// accountID replaces userID after a rename; a few lines of
				// unrelated refactor were inserted above it. Subject
				// genuinely changed (that IS the rename), so exact-fp does
				// not fire; the SimilarFinding tiebreaker (same file, within
				// the merge window, high token overlap even after the
				// rename) is what closes this one.
				Desc: "the accountID filter is compared with strings.EqualFold, allowing a case-insensitive account takeover",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "accountID", Source: srcQueryGo},
			SameDefect: true,
		},
		{
			Name:    "rename-large-shift-should-merge",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/funnel/hypothesize.go", Line: 120,
				Desc: "chunkBudget is computed from the stale finder count captured before the retry loop adds more finders",
				Lens: "correctness", DefectKind: domain.DefectLogic, Subject: "computeChunkBudget", Source: srcHypothesizeGo},
			B: DupCandidate{File: "internal/funnel/hypothesize.go", Line: 205,
				// chunkBudget renamed to unitBudget, and ~80 lines of an
				// unrelated refactor were inserted ABOVE it, but the bug
				// stays inside the SAME enclosing computeChunkBudget
				// function it always was — the drift bugbot-ezmx.5's
				// symbol-anchored locus exists to survive. Subject is
				// recorded as the enclosing function (the identity a real
				// triage run would anchor to), not the renamed local
				// variable, so it agrees on both sides.
				Desc: "unitBudget is computed from the stale finder count captured before the retry loop adds more finders",
				Lens: "correctness", DefectKind: domain.DefectLogic, Subject: "computeChunkBudget", Source: srcHypothesizeGo},
			// 85 lines of drift is outside DefaultMergeWindow, so the
			// SimilarFinding tiebreaker alone still misses this — but the
			// composed v3 stack now converges at exact-fp on the shared
			// symbol locus. This is the recall gain bugbot-ezmx.5/.6 (locus
			// resolution surviving edits above the bug) buys the rename
			// channel; renamed from *-false-negative to *-should-merge.
			SameDefect: true,
		},
		{
			Name:    "rename-wording-drift-false-negative",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/tui/palette.go", Line: 150,
				Desc: "the fuzzyMatch scorer never resets its memo between queries, so stale scores leak into the next search",
				Lens: "correctness", DefectKind: domain.DefectLogic, Subject: "fuzzyMatch", Source: srcPaletteGo},
			B: DupCandidate{File: "internal/tui/palette.go", Line: 154,
				// fuzzyMatch renamed to rankCandidates during a rewrite — a
				// genuine top-level FUNCTION rename, not just a local
				// variable, so the symbol-anchored locus itself changes;
				// the description was also reworded, dropping shared
				// vocabulary.
				Desc: "rankCandidates keeps its cache across calls, so an older search's entries pollute a fresh one",
				Lens: "correctness", DefectKind: domain.DefectLogic, Subject: "rankCandidates", Source: srcPaletteGo},
			// Same defect (stale-memo leak), renamed AND reworded — both the
			// locus (different enclosing function) and the SimilarFinding
			// jaccard guard are stressed. Expected false negative even under
			// the composed v3 stack: correlating fingerprints across a
			// literal function rename needs store-level rename tracking,
			// which is out of scope for this pure predicate composition.
			SameDefect: true,
		},
		{
			Name:    "rename-unrelated-symbol-collision",
			Channel: DupChannelRename,
			A: DupCandidate{File: "internal/store/query.go", Line: 90,
				Desc: "the userID filter is compared with strings.EqualFold, allowing a case-insensitive account takeover",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "userID", Source: srcQueryGo},
			B: DupCandidate{File: "internal/store/query.go", Line: 93,
				Desc: "the orgID filter has no bounds check and accepts an empty string, matching every organization's rows",
				Lens: "security", DefectKind: domain.DefectOther, Subject: "orgID", Source: srcQueryGo},
			// A distinct bug on a nearby renamed-looking symbol (orgID vs
			// userID/accountID), same locus, same kind, but a different
			// subject — precision probe for the rename channel: proximity
			// and locus alone must not merge unrelated defects.
			SameDefect: false,
		},
	}
}
