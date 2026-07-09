// funnel_types.go holds the output types extracted from funnel.go for readability.
// Pure code motion: no logic changes.
package funnel

import (
	"sort"

	"github.com/dpoage/bugbot/internal/domain"
)

// Site is one code location (file + line) that a merged candidate represents.
// The primary candidate's own File/Line is always Sites[0]. Subsequent entries
// are the other same-root-cause members that were collapsed into this primary
// during triage's expanded merge pass.
type Site struct {
	File string
	Line int
}

// Candidate is a finder-proposed bug after it has been associated with a lens
// and a fingerprint. It is the unit that flows from hypothesize through triage
// into verification.
type Candidate struct {
	Lens        string
	File        string
	Line        int
	Title       string
	Description string
	Severity    domain.Severity
	Evidence    string
	Confidence  domain.Confidence
	// DefectKind is the closed taxonomy class the finder reported (see
	// domain.DefectKind), validated by the candidate schema's enum. Set from
	// the raw finder output; drives Fingerprint v3 identity and the
	// same-symbol distinct-defect guard in clustering.
	DefectKind domain.DefectKind
	// Subject is the RAW (un-normalized) symbol name the finder reported.
	// domain.NormalizeSubject is applied where identity is minted
	// (triageState.process); this field carries the finder's original text so
	// it survives round-trips (e.g. persistOrphan) unmodified.
	Subject string
	// Fingerprint is the store dedup key (lens+file+line+title). Set in triage.
	Fingerprint string
	// LocusKey is the lens-independent location identity domain.LocusKey(file, locus):
	// the Fingerprint inputs minus the lens. Set in triage alongside Fingerprint and
	// carried onto the persisted finding; it still backs the legacy suppression
	// fallback and rename rewriting, and remains a proper subset of the
	// same-file window store.FindingsByFileWindow queries for the durable
	// cross-lens fold.
	LocusKey string
	// CorroboratingLenses lists the OTHER lenses that independently reported the
	// same underlying defect (same file, nearby line) and were collapsed into this
	// candidate during triage's location-based cross-lens dedup. It excludes this
	// candidate's own Lens and is deduplicated and sorted. Empty when no other
	// lens corroborated the finding. It is recorded for reporting only — it does
	// NOT raise the candidate's confidence.
	CorroboratingLenses []string
	// PendingID is the primary key of this candidate's row in the
	// pending_candidates write-ahead log (store/pending.go). It is set when the
	// finder unit persisted the candidate before emitting it, or carried from a
	// replayed prior-run row. Empty for candidates that were never WAL-persisted
	// (a persist failure, or a unit-test candidate built directly). Every
	// terminal-fate handler (triage drop/merge, verify survived/killed/orphaned)
	// deletes this row, so a clean run leaves the WAL empty and only an
	// interrupt leaves rows for the next run to replay.
	PendingID string
	// Sites accumulates every code location a same-root-cause merge collapsed
	// into this primary. Sites[0] is the primary's own (File, Line); later
	// entries come from merged-away members. Empty when no root-cause merges
	// occurred (single-site finding).
	Sites []Site
	// Reverify marks a candidate reconstructed from a durable OPEN Tier-3 suspected
	// finding for re-verification (ReverifySuspected). Unlike a fresh or WAL-replayed
	// candidate it has a durable open finding row and NO pending WAL row (PendingID==""),
	// so the verify kill path must transition that row out of open when refuted.
	Reverify bool
}

// ToolIssue is one aggregated harness tool-health problem on Stats.ToolIssues,
// deduplicated by (Source, Tool, Severity) with Count occurrences. Source is
// "infra" (objective: a *agent.ToolHealthError surfaced at the runner dispatch
// seam) or "agent" (subjective: an agent called report_tool_issue). Severity is
// one of the domain.Severity values. This is harness-meta, never a finding.
type ToolIssue struct {
	Source   string `json:"source"`
	Tool     string `json:"tool"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// Stats is the per-stage funnel accounting recorded on the scan run.
type Stats struct {
	// Hypothesized is the raw candidate count emitted by all finder agents.
	Hypothesized int `json:"hypothesized"`
	// Resumed is the count of pending candidates from prior interrupted runs
	// that were replayed into this run's triage/verify pipeline (skipping
	// re-hypothesize). These flow through the same triage and verify stages as
	// fresh candidates, so they are also counted in Triaged/Verified/Killed.
	// Zero on a run with no interrupted predecessor — the common case. See the
	// pending_candidates write-ahead log (store/pending.go).
	Resumed int `json:"resumed,omitempty"`
	// Triaged is the candidate count surviving triage (the input to verify).
	Triaged int `json:"triaged"`
	// Verified is the count surviving adversarial verification (Tier 2).
	Verified int `json:"verified"`
	// Killed is candidates that entered verification but were majority-refuted.
	Killed int `json:"killed"`
	// Suspected is the count of budget-orphaned candidates persisted as Tier 3
	// suspected: they passed triage but the run hit its hard budget before their
	// verification completed, so they are kept (not dropped) for human review.
	Suspected int `json:"suspected,omitempty"`
	// DroppedLowConfidence / DroppedDuplicate / DroppedSuppressed /
	// DroppedOutOfScope break down the triage losses.
	DroppedLowConfidence int `json:"dropped_low_confidence"`
	DroppedDuplicate     int `json:"dropped_duplicate"`
	DroppedSuppressed    int `json:"dropped_suppressed"`
	DroppedOutOfScope    int `json:"dropped_out_of_scope"`
	// MergedWithinLens / MergedCrossLens break down the location-based cross-lens
	// dedup losses in triage: after exact-fingerprint dedup, surviving candidates
	// are clustered by location (same file, nearby line) and only the cluster's
	// primary proceeds to verification. Each collapsed (non-primary) member is
	// counted here — MergedWithinLens when its lens equals the primary's lens
	// (the same lens reported the same defect twice with different wording),
	// MergedCrossLens when it came from a different lens. These are distinct from
	// DroppedDuplicate (exact fingerprint match): a merged member here is a
	// DIFFERENT fingerprint that nonetheless points at the same underlying bug.
	// Under Fingerprint v3 (locus+defect_kind+subject, lens excluded from
	// identity), a genuine cross-lens duplicate more often collides at the
	// exact-fingerprint step instead of reaching this location-based merge —
	// that case increments ONLY DroppedDuplicate, deliberately NOT
	// MergedCrossLens, to keep the two fields non-overlapping.
	MergedWithinLens int `json:"merged_within_lens"`
	MergedCrossLens  int `json:"merged_cross_lens"`
	// MergedRootCause counts candidates collapsed by the same-root-cause merge
	// (same-file broad-window or cross-file decl/def) — distinct from
	// MergedWithinLens/MergedCrossLens which track the tighter 10-line window.
	MergedRootCause int `json:"merged_root_cause,omitempty"`
	// MergedRootCauseCodeNav counts candidates collapsed by the code-nav
	// root-cause fold (triage_streaming.go step 5e): a candidate one reference
	// hop (a direct call/use, per the code-navigation backend) from an in-run
	// cluster primary of the SAME defect_kind — the generalization of
	// MergedRootCause beyond same-file/decl-def-pair shapes to arbitrary
	// caller/callee files, the common multi-site case in Go. Counted
	// SEPARATELY from MergedRootCause (not folded into it) so the heuristic's
	// yield and precision can be measured independently of the well-calibrated
	// jaccard-based merges; included in DuplicateRate's numerator because,
	// like MergedRootCause, it collapses two members of THIS run's own
	// candidate pool.
	MergedRootCauseCodeNav int `json:"merged_root_cause_codenav,omitempty"`
	// MergedCrossLensDurable counts candidates absorbed by durableCrossLensFold
	// (triage_streaming.go): a WAL-replayed (or otherwise re-discovered)
	// candidate folded into an OPEN finding PERSISTED BY A PRIOR RUN at the
	// same file-line window, discovered via an indexed store lookup rather
	// than this run's in-memory clustering. Unlike MergedCrossLens (this run's
	// own in-memory cross-lens merge), this is cross-scan reconciliation — the
	// same SimilarFinding predicate the publish-time backlog adoption uses —
	// so it is counted SEPARATELY and excluded from DuplicateRate's in-run
	// scope.
	MergedCrossLensDurable int `json:"merged_cross_lens_durable,omitempty"`
	// DroppedSuppressedDurable counts candidates absorbed by
	// durableCrossLensFold into a DISMISSED finding at the same file-line
	// window instead of an open one: the candidate is a re-discovery of a
	// defect a maintainer already rejected, so it is suppressed exactly as if
	// it had matched that dismissal by exact fingerprint (bugbot-oiem is the
	// convergent suppression-side concern — both paths land on the same
	// suppressions row). Cross-scan reconciliation like MergedCrossLensDurable;
	// excluded from DuplicateRate for the same reason.
	DroppedSuppressedDurable int `json:"dropped_suppressed_durable,omitempty"`
	// RegressionReopened counts candidates absorbed by durableCrossLensFold
	// into a FIXED finding at the same file-line window: the candidate is
	// treated as a regression of the previously-fixed defect (store.
	// ReopenAsRegression flips the row back to open in place) rather than a
	// brand-new finding. Cross-scan reconciliation like MergedCrossLensDurable;
	// excluded from DuplicateRate for the same reason.
	RegressionReopened int `json:"regression_reopened,omitempty"`
	// DedupArbiterRuns is the number of LLM dedup-arbiter invocations
	// (bugbot-ezmx.2): one bounded, zero-tool completion spent per location
	// collision the jaccard gate could not resolve on its own (same locus/kind,
	// description similarity below mergeSimilarityThreshold), at both the
	// in-run cluster collision site (triage_streaming.go step 5) and the
	// durableCrossLensFold SimilarFinding fallback (OPEN-status branch only).
	// Bounded by DefaultDedupArbiterCap per scan — a handful, not one per
	// candidate.
	DedupArbiterRuns int `json:"dedup_arbiter_runs,omitempty"`
	// DedupArbiterMerges is the subset of DedupArbiterRuns that returned a
	// confident "yes" and were folded via the caller's normal merge path
	// (handleMember / AddCorroboratingLenses+AppendFindingSites). These merges
	// are ALSO counted by MergedWithinLens/MergedCrossLens (step 5) or
	// MergedCrossLensDurable (durable fold) — the same code path that would
	// have counted them had jaccard alone accepted the match — so
	// DedupArbiterMerges is a visibility overlay, not an additional summand:
	// it does NOT feed DuplicateRate's numerator itself, preserving numerator
	// disjointness (see DuplicateRate's doc).
	DedupArbiterMerges int `json:"dedup_arbiter_merges,omitempty"`
	// DedupArbiterSkippedCap counts collisions that qualified for the dedup
	// arbiter but were skipped because DefaultDedupArbiterCap was already
	// exhausted this scan — graceful pass-through: both candidates are kept
	// as distinct findings exactly as an "unsure" verdict would produce.
	DedupArbiterSkippedCap int `json:"dedup_arbiter_skipped_cap,omitempty"`
	// DedupArbiterFailures counts dedup-arbiter runs that produced no
	// parseable verdict (infra or parse failure); these are treated as
	// "unsure" (no merge), never as a silent kill or merge.
	DedupArbiterFailures int `json:"dedup_arbiter_failures,omitempty"`
	// DedupArbiterTokens is the total input+output tokens spent by dedup
	// arbiter runs (a subset of InputTokens+OutputTokens), mirroring
	// ArbiterTokens for the split-verdict arbiter.
	DedupArbiterTokens int64 `json:"dedup_arbiter_tokens,omitempty"`
	// MergedCrossLensDurableCodeNav counts candidates absorbed by the code-nav
	// root-cause fold's open-finding branch (triage_streaming.go step 5e): a
	// candidate one reference hop from an OPEN finding PERSISTED BY A PRIOR RUN
	// (or earlier in this run) at a DIFFERENT locus, same defect_kind. Like
	// MergedCrossLensDurable it reconciles against a finding this run's own
	// in-memory clustering cannot see, so it is counted SEPARATELY and, for the
	// same reason, EXCLUDED from DuplicateRate's in-run scope.
	MergedCrossLensDurableCodeNav int `json:"merged_cross_lens_durable_codenav,omitempty"`

	// ReconcileNominated is the number of candidate pairs the backlog
	// reconcile cycle (bugbot-ezmx.4) nominated: OPEN findings sharing a
	// normalized file, within DefaultMergeWindow lines, compatible
	// defect_kind, and SimilarFinding-close descriptions. Kind-mismatched or
	// far-apart pairs are never nominated (no arbiter spend).
	ReconcileNominated int `json:"reconcile_nominated,omitempty"`
	// ReconcileArbitrated is the subset of ReconcileNominated actually sent
	// to the dedup arbiter (bounded by the reconcile per-cycle cap; the
	// remainder count as ReconcileSkippedCap).
	ReconcileArbitrated int `json:"reconcile_arbitrated,omitempty"`
	// ReconcileMerged is the subset of ReconcileArbitrated that returned a
	// confident "yes": the newer row was folded into the older (canonical)
	// row via AppendFindingSites/AddCorroboratingLenses and closed
	// StatusSuperseded.
	ReconcileMerged int `json:"reconcile_merged,omitempty"`
	// ReconcileSkippedCap counts nominated pairs skipped because the
	// per-cycle reconcile cap was already exhausted -- graceful
	// pass-through, both findings kept open for the next cycle.
	ReconcileSkippedCap int `json:"reconcile_skipped_cap,omitempty"`
	// ReconcileFailures counts arbiter runs that produced no parseable
	// verdict (infra or parse failure); treated as "unsure" (no merge).
	ReconcileFailures int `json:"reconcile_failures,omitempty"`

	// FinderRuns is the number of finder (lens, chunk) agents that actually
	// launched (i.e. were not skipped by budget degradation/stop). FinderFailures
	// is how many of those produced NO parseable output even after the repair
	// round-trip — their findings are lost, not absent. A scan with
	// FinderFailures > 0 must never report a bare "No findings": that result is
	// untrustworthy. See internal/cli/scan.go and reliabilityWarning.
	FinderRuns     int `json:"finder_runs"`
	FinderFailures int `json:"finder_failures"`
	// FinderBudgetStopped counts finders that ran but were truncated by a budget
	// limit (their own token budget or the shared budget pool) before producing
	// parseable output. These are deliberate budget stops, NOT reliability
	// failures: they are excluded from FinderFailures so a budget-limited scan is
	// never misreported as having broken finders. Their partial coverage is noted
	// under Result.Skipped instead.
	FinderBudgetStopped int `json:"finder_budget_stopped,omitempty"`
	// FinderRateLimited counts finders that exhausted the retry budget against
	// a rate-limiting provider (llm.ErrRateLimited). Distinct from
	// FinderFailures: the provider throttled us, the findings are NOT lost in
	// the model-output sense — they were never produced because the run
	// never completed. Coverage is incomplete but recoverable by lowering
	// --concurrency or re-running, so this is excluded from FinderReliable()
	// and MostFindersFailed(). A rate-limited-only run is "reliable but
	// coverage-incomplete", which is the intended distinction from a genuine
	// parse failure.
	FinderRateLimited int `json:"finder_rate_limited,omitempty"`
	// VerifierRuns / VerifierFailures mirror the above for refuter agents. A
	// refuter that produces no parseable verdict is still "not refuted" so it can
	// never silently kill a candidate, but it is EXCLUDED from the survive-trust
	// quorum (genuineVerdicts) and counted here so the verification's reliability
	// is visible. A panel where every seat fails (zero genuine verdicts) is
	// orphaned as T3 suspected rather than promoted as verified (bugbot-8rd).
	VerifierRuns     int `json:"verifier_runs"`
	VerifierFailures int `json:"verifier_failures"`
	// ToolIssues records harness TOOL-health problems observed this run: genuine
	// tool-infra failures captured objectively at the dispatch seam (Source
	// "infra") plus agent-filed report_tool_issue complaints (Source "agent").
	// Non-empty means some tool misbehaved, so a low/empty finding count may be
	// incomplete rather than clean. Surfaced in the scan summary and as
	// KindToolUnhealthy progress events.
	ToolIssues []ToolIssue `json:"tool_issues,omitempty"`
	// ArbiterRuns is the number of COMPLETED arbiter agents launched to decide
	// split (mixed refuted/not-refuted) panel verdicts. A run cut short by a
	// budget stop is NOT counted here — it is counted in ArbiterBudgetStops
	// instead, so ArbiterRuns + ArbiterBudgetStops partitions all arbiter
	// invocations.
	ArbiterRuns int `json:"arbiter_runs,omitempty"`
	// ArbiterKills is the number of candidates the arbiter decided to kill
	// (arbiter returned refuted=true).
	ArbiterKills int `json:"arbiter_kills,omitempty"`
	// ArbiterFailures is the number of arbiter agents that produced no parseable
	// verdict; on failure the run falls back to majorityRefuted.
	ArbiterFailures int `json:"arbiter_failures,omitempty"`
	// ArbiterTokens is the total input+output tokens spent by arbiter runs (a
	// subset of InputTokens+OutputTokens), counted for COMPLETED and
	// budget-stopped runs alike. ArbiterBudgetStops counts arbiter runs cut
	// short by a budget stop (their own per-run claim or the shared pool).
	// Together they make the arbiter's cost and starvation rate observable: the
	// stop RATE is ArbiterBudgetStops / (ArbiterRuns + ArbiterBudgetStops), so a
	// too-small ArbiterTokenClaim surfaces as a high stop rate (bugbot-mi5.17 AC6).
	ArbiterTokens      int64 `json:"arbiter_tokens,omitempty"`
	ArbiterBudgetStops int   `json:"arbiter_budget_stops,omitempty"`
	// InputTokens / OutputTokens is the run's total token spend. InputTokens
	// includes cached tokens (the llm.Usage convention).
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CacheReadTokens / CacheCreationTokens are the subsets of InputTokens
	// served from / written to the provider's prompt cache, for reporting
	// cache savings.
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	// CartographerEnabled records whether the package-summary pass
	// (scan.cartographer) was active for this run. Persisted so the
	// valid-findings-per-token series — Verified / (InputTokens+OutputTokens),
	// one point per scan run over started_at — can be sliced by cartographer
	// on/off. That ratio, not raw token count, is how the feature earns its
	// keep: a new agent adds tokens by construction, so the question is whether
	// the injected context buys more verified findings per token spent.
	CartographerEnabled bool `json:"cartographer_enabled"`
	// SandboxExecs is the total number of sandbox_exec tool calls made by
	// refuter agents during the verification stage. Zero when the feature is
	// disabled or unused.
	SandboxExecs int `json:"sandbox_execs,omitempty"`
	// SandboxExecMillis is the total wall-clock time spent in sandbox
	// executions, in milliseconds.
	SandboxExecMillis int64 `json:"sandbox_exec_millis,omitempty"`
	// LeadsPosted is the number of cross-lens leads successfully posted to the
	// blackboard by finder agents during this run.
	LeadsPosted int `json:"leads_posted,omitempty"`
	// LeadsConsumed is the number of pending cross-lens leads that were claimed
	// and injected into finder tasks at the start of this run's hypothesize stage.
	LeadsConsumed int `json:"leads_consumed,omitempty"`
	// HeatOrdered reports whether the Sweep targets were reordered by
	// churn-heat before chunking (i.e. heat ordering ran AND produced a
	// non-trivial reordering compared to alphabetical). False when heat
	// ordering was disabled, git history was unavailable, or the heat map
	// was empty.
	HeatOrdered bool `json:"heat_ordered,omitempty"`
	// HeatFiles is the number of files in the heat map that scored above
	// zero for this Sweep. Zero when heat ordering was disabled or git
	// history was unavailable.
	HeatFiles int `json:"heat_files,omitempty"`
	// SweepNeverScanned is the number of files in the sweep's group 1 (never
	// scanned or at epoch sentinel). Zero when heat ordering is disabled.
	SweepNeverScanned int `json:"sweep_never_scanned,omitempty"`
	// SweepChangedSinceScan is the number of files admitted to the sweep's
	// group 1 because their current fingerprint differs from the content hash
	// recorded at their last scan. Zero when heat ordering is disabled.
	SweepChangedSinceScan int `json:"sweep_changed_since_scan,omitempty"`
	// CoveredFiles is the count of files that were actually covered (i.e. at
	// least one finderOK unit ran against them) in this run.
	CoveredFiles int `json:"covered_files,omitempty"`
	// Interrupted is set when the scan run was cancelled (context deadline
	// exceeded or context cancellation, e.g. SIGINT). The stats reflect whatever
	// stages completed before the interruption. The scan_runs row is sealed with
	// finished_at set so no row is left dangling.
	Interrupted bool `json:"interrupted,omitempty"`
	// Aborted is set when the scan run exited due to an unexpected internal
	// error (not a context cancellation). Partial stats are recorded and the
	// scan_runs row is sealed so no row is left dangling.
	Aborted bool `json:"aborted,omitempty"`
	// FinderAborted is set when the finder-stage circuit breaker tripped
	// (bugbot-2uz): a transport-error threshold was reached with zero
	// finderOK successes, so the funnel stopped launching further finder
	// units and cancelled in-flight ones. The already-recorded
	// FinderFailures are kept — MostFindersFailed() still reports the run as
	// unreliable — but this flag surfaces the abort reason distinctly from a
	// normal "all units ran and failed" run. A downstream consumer can tell
	// "we ran every unit and they all failed" from "we aborted after the
	// first wave of transport failures and never launched the rest".
	FinderAborted bool `json:"finder_aborted,omitempty"`
	// SeamsFound is the number of cross-language contract surfaces
	// (shared data files + shared env vars) discovered by
	// ingest.EnumerateSeams on this run's snapshot. The boundary lens
	// emits one custom finder unit per seam, so SeamsFound is also the
	// upper bound on SeamsCovered.
	SeamsFound int `json:"seams_found,omitempty"`
	// SeamsCovered is the count of seams that produced a finished (ok or
	// budget-truncated) finder unit. Equal to SeamsFound minus the seams
	// whose units were budget-skipped or never launched because the run
	// stopped early. SeamsCovered <= SeamsFound always.
	SeamsCovered int `json:"seams_covered,omitempty"`
}

// FinderReliable reports whether the finder stage produced trustworthy coverage:
// at least one finder ran and none of the finders that ran failed to parse. When
// false, an empty or sparse finding set is suspect — some lens's output was lost,
// not genuinely clean.
func (s Stats) FinderReliable() bool {
	return s.FinderRuns > 0 && s.FinderFailures == 0
}

// DuplicateRate is the fraction of the candidate pool that ENTERED triage
// this run — fresh finder output (Hypothesized) plus WAL-replayed pending
// candidates (Resumed), i.e. everything triage actually judged — that triage
// identified as a duplicate of some other candidate: exact-fingerprint
// the in-run location-based and same-root-cause merges (MergedWithinLens +
// MergedCrossLens + MergedRootCause + MergedRootCauseCodeNav — the code-nav
// fold's in-run branch, see its own doc for why it counts here). The
// denominator MUST include Resumed:
// a WAL-replayed candidate can be dropped/merged exactly like a fresh one, so
// counting it in the numerator but not the denominator can push the rate
// above 1.0 on a resumed run.
//
// Deliberately scoped to in-run triage dedup: it does NOT count
// MergedCrossLensDurable, MergedCrossLensDurableCodeNav, DroppedSuppressedDurable,
// or RegressionReopened (a WAL-replayed or re-discovered candidate folded into /
// suppressed against / reopening a finding a PRIOR run — or this run's own
// earlier candidate — persisted) or cross-scan backlog adoption (SimilarFinding
// at publish time) — all of these reconcile against a DIFFERENT run's findings,
// not this run's own candidate pool. Zero when the denominator is zero (nothing
// to have a rate over).
func (s Stats) DuplicateRate() float64 {
	pool := s.Hypothesized + s.Resumed
	if pool == 0 {
		return 0
	}
	dup := s.DroppedDuplicate + s.MergedWithinLens + s.MergedCrossLens + s.MergedRootCause + s.MergedRootCauseCodeNav
	return float64(dup) / float64(pool)
}

// MostFindersFailed reports whether a strict majority of the finders that ran
// failed to produce parseable output. A scan in this state has effectively no
// signal and should exit nonzero so automation does not treat it as "clean".
func (s Stats) MostFindersFailed() bool {
	return s.FinderRuns > 0 && s.FinderFailures*2 > s.FinderRuns
}

// Result summarizes a completed funnel run for the caller.
type Result struct {
	// ScanRunID is the store scan-run this funnel recorded under.
	ScanRunID string
	// Commit is the snapshot commit the scan ran against.
	Commit string
	// Findings are the persisted Tier 2 survivors, sorted critical-first.
	Findings []domain.Finding
	// Stats is the per-stage accounting.
	Stats Stats
	// Degraded reports whether the run crossed the soft budget and reduced its
	// lens set / refuter count.
	Degraded bool
	// Stopped reports whether the run hit the hard budget: it stopped launching
	// new agents and truncated in-flight ones at their next turn boundary.
	Stopped bool
	// Skipped lists human-readable notes about work the run deliberately did not
	// do (degradation, hard-budget stops). Never silent truncation.
	Skipped []string
	// CoveredFiles is the deduplicated, sorted list of files that were actually
	// covered by at least one finderOK unit in this run. Files from parse-failed,
	// budget-stopped, or budget-skipped units are NOT included. The diff-intent
	// custom unit (files == nil) contributes nothing here.
	CoveredFiles []string
}

// sortFindings orders findings critical-first (highest Rank first), then by
// file/line for stable output. domain.Severity.Rank() uses higher=more-severe
// (critical=4, low=1), so critical-first means si > sj.
func sortFindings(fs []domain.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		si, sj := fs[i].Severity.Rank(), fs[j].Severity.Rank()
		if si != sj {
			return si > sj
		}
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		return fs[i].Line < fs[j].Line
	})
}
