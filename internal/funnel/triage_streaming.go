package funnel

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// clusterRegistry is a thread-safe registry shared between the triage
// consumer goroutine and the concurrent verify goroutines. Triage registers
// each new cluster primary; verify goroutines signal persistence; triage
// goroutine adds staged corroborating lenses; verify goroutines retrieve staged
// lenses at persist time.
//
// The registry is keyed by the primary candidate's Fingerprint.
type clusterRegistry struct {
	mu      sync.Mutex
	entries map[string]*registryEntry
}

// registryEntry tracks one cluster primary through its full lifecycle.
type registryEntry struct {
	// stagedLenses are corroborating lenses from members that arrived in triage
	// BEFORE the primary's verification completed. The verify goroutine reads
	// these at persist time to attach them to the finding.
	stagedLenses []string
	// attachedLate are lenses attached AFTER the finding persisted (the
	// late-stage TOCTOU window or a triage member arriving post-persist). The
	// store row is updated at attach time by whichever side attached; run()
	// folds these into the IN-MEMORY finding after all consumers drain, so
	// Result.Findings matches the store regardless of arrival timing.
	attachedLate []string
	// stagedSites are extra code locations staged by root-cause-merged members
	// that arrive in triage BEFORE the primary's verify goroutine persists the
	// finding. Exactly mirrors the stagedLenses mechanism: DrainStagedSites is
	// called in runVerifyAndPersist before UpsertFinding; the TOCTOU window is
	// closed by SignalPersisted's late-site return.
	stagedSites []domain.Site
	// lateSites are sites appended AFTER the finding persisted. The store row
	// is updated via AppendFindingSites; run() folds these into the in-memory
	// finding after all consumers drain.
	lateSites []domain.Site
	// persisted is true once the verify goroutine calls SignalPersisted.
	// Subsequent triage corroborating members use AddCorroboratingLenses instead
	// of staging.
	persisted bool
	// killed is true when the primary was killed or orphaned (no finding stored).
	killed bool
}

func newClusterRegistry() *clusterRegistry {
	return &clusterRegistry{entries: make(map[string]*registryEntry)}
}

// Register records a new cluster primary by its fingerprint.
// Called from the triage goroutine before forwarding to verify.
func (r *clusterRegistry) Register(fingerprint string) {
	r.mu.Lock()
	r.entries[fingerprint] = &registryEntry{}
	r.mu.Unlock()
}

// AddStagedLens records a corroborating lens from a later triage member.
// Called from the triage goroutine.
// Returns true if the lens was staged (primary not yet persisted), false if the
// primary was already persisted or killed (caller must use AddCorroboratingLenses
// or discard, respectively).
func (r *clusterRegistry) AddStagedLens(fingerprint, lens string) (staged bool, killed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok {
		return false, false
	}
	if e.killed {
		return false, true
	}
	if e.persisted {
		// Post-persist arrival: the caller updates the store row; record the
		// lens here too so run() can fold it into the in-memory finding.
		e.attachedLate = append(e.attachedLate, lens)
		return false, false
	}
	e.stagedLenses = append(e.stagedLenses, lens)
	return true, false
}

// DrainStagedLenses retrieves and clears the staged corroborating lenses for a
// primary. Called from the verify goroutine just before persisting the finding.
// Returns deduplicated, sorted lenses.
func (r *clusterRegistry) DrainStagedLenses(fingerprint string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok || len(e.stagedLenses) == 0 {
		return nil
	}
	lenses := e.stagedLenses
	e.stagedLenses = nil
	return dedupLenses(lenses)
}

// SignalPersisted records that the primary's finding has been persisted (or
// that the primary was killed/orphaned). Called from the verify goroutine.
//
// It returns any lenses staged AFTER the goroutine's DrainStagedLenses call —
// closing the TOCTOU window where a triage member arrives between drain and
// persist: AddStagedLens accepted the lens (state was not yet persisted), the
// drain had already happened, and without this return the lens would be
// stranded (never attached at persist, never store-updated by triage). The
// caller must AddCorroboratingLenses any returned lenses. Returns nil on the
// killed path: corroboration of a dead primary is moot.
func (r *clusterRegistry) SignalPersisted(fingerprint string, persisted bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok {
		return nil
	}
	if !persisted {
		e.killed = true
		e.stagedLenses = nil
		e.stagedSites = nil
		return nil
	}
	e.persisted = true
	late := e.stagedLenses
	e.stagedLenses = nil
	// Move any sites staged in the TOCTOU window into lateSites so
	// DrainLateSites can retrieve them after SignalPersisted returns.
	if len(e.stagedSites) > 0 {
		e.lateSites = append(e.lateSites, e.stagedSites...)
		e.stagedSites = nil
	}
	if len(late) == 0 {
		return nil
	}
	// These are store-updated by the caller AND folded into the in-memory
	// finding by run() at drain time.
	e.attachedLate = append(e.attachedLate, late...)
	return dedupLenses(late)
}

// AddStagedSite records a merged-member site from triage arriving BEFORE the
// primary's verification completed. Symmetric to AddStagedLens.
// Returns staged=true if queued for DrainStagedSites; killed=true if the
// primary is dead (site can be discarded); false,false if already persisted
// (caller must call AppendFindingSites on the store directly).
func (r *clusterRegistry) AddStagedSite(fingerprint string, s domain.Site) (staged bool, killed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok {
		return false, false
	}
	if e.killed {
		return false, true
	}
	if e.persisted {
		// Post-persist arrival: caller updates the store; record here for
		// run()-side in-memory reconciliation.
		e.lateSites = append(e.lateSites, s)
		return false, false
	}
	e.stagedSites = append(e.stagedSites, s)
	return true, false
}

// DrainStagedSites retrieves and clears the staged sites for a primary.
// Called from the verify goroutine just before UpsertFinding, alongside
// DrainStagedLenses. Returns nil when no sites were staged.
func (r *clusterRegistry) DrainStagedSites(fingerprint string) []domain.Site {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok || len(e.stagedSites) == 0 {
		return nil
	}
	sites := e.stagedSites
	e.stagedSites = nil
	return sites
}

// DrainLateSites returns sites that were added after the primary persisted
// (both the TOCTOU window and genuine post-persist arrivals). Called by run()
// after all consumers have drained, so no further additions can race the read.
func (r *clusterRegistry) DrainLateSites(fingerprint string) []domain.Site {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok || len(e.lateSites) == 0 {
		return nil
	}
	sites := e.lateSites
	e.lateSites = nil
	return sites
}

// AttachedLenses returns the lenses attached to a primary's finding after it
// persisted (deduplicated, sorted). Called by run() after all consumers have
// drained, so no further attachments can race the read.
func (r *clusterRegistry) AttachedLenses(fingerprint string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok || len(e.attachedLate) == 0 {
		return nil
	}
	return dedupLenses(e.attachedLate)
}

// IsPersistedOrKilled reports the current state of a primary. Called from the
// triage goroutine when a new corroborating member arrives.
func (r *clusterRegistry) IsPersistedOrKilled(fingerprint string) (persisted, killed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[fingerprint]
	if !ok {
		return false, false
	}
	return e.persisted, e.killed
}

// triageState is the single-goroutine incremental triage consumer state.
// It replicates triage() batch semantics but processes one candidate at a time.
//
// STREAMING INVARIANT: the set of cluster primaries forwarded to verify is
// equivalent to what batch triage() would produce for the same candidate
// multiset, with the documented relaxation that primary identity depends on
// arrival order. Cluster-level equivalence is preserved: same set of
// location-clusters, each carrying the same corroborating-lens SET.
type triageState struct {
	// Per-candidate dedup and scope. Replicates triage() steps 1-4.
	inScope map[string]bool
	seen    map[string]bool // fingerprints seen (deduped or suppressed)

	// firstLens records, per fingerprint, the Lens of the candidate that FIRST
	// established that fingerprint (whatever its eventual fate — primary,
	// root-cause member, or durable fold). Under Fingerprint v3 (lens excluded
	// from identity), a later arrival with the SAME fingerprint from a
	// DIFFERENT lens is a genuine cross-lens duplicate of one defect, not a
	// same-lens re-report; step 3 uses this to stage that lens as
	// corroboration, mirroring handleMember's cross-lens branch for the
	// cluster/root-cause merge paths.
	firstLens map[string]string

	// Incremental cluster state: maps clusterKey (location bucket) → the
	// clusters anchored in that bucket. A SLICE per bucket is load-bearing:
	// token-DISSIMILAR defects can share a location bucket (batch mergeClusters
	// splits them by jaccard into separate clusters), and a single-cluster-per-
	// bucket map lets each dissimilar arrival overwrite the bucket's pointer,
	// orphaning the previous group so its later members become spurious
	// primaries — reproduced by the recorded eval corpus.
	clusters map[string][]*internalCluster

	// fileClusters maps normPath(file) → clusters in that file, used by
	// same-root-cause broad-window (same-file) and cross-file decl/def merges.
	fileClusters map[string][]*internalCluster

	// registry is shared with verify goroutines for staged-lens coordination.
	registry *clusterRegistry

	// resolver maps (file, line) to the stable enclosing-symbol locus used by the
	// durable finding fingerprint (domain.Fingerprint). Built from snap.Root.
	resolver *LocusResolver

	// ready holds primaries to forward to verify. Drained by popReady().
	ready []Candidate

	// survivorCount is the number of cluster primaries forwarded.
	survivorCount int

	// dedupArbiter holds the LLM dedup-arbiter dependencies (bugbot-ezmx.2).
	// nil (the default from newTriageState) disables the arbiter entirely,
	// preserving every existing test's fall-through behavior; wired only by
	// run() in run_pipeline.go.
	dedupArbiter *dedupArbiterConfig
	// nav is the code-navigation seam for the code-nav root-cause fold (step
	// 5e, codenav_fold.go). Nil when code navigation was not available at
	// triageState construction (Options.CodeNav unset AND *agent.CodeNav
	// construction failed) — the fold degrades to a silent no-op in that case,
	// same as any other nav error, since it is a heuristic layered on top of
	// the identity-precise merge rules above it, never load-bearing.
	nav codeNavRefs

	// refCache memoizes ONE code-nav reference query's result per (file,
	// declaration-line, symbol) key for the lifetime of the scan, so a symbol
	// re-evaluated across multiple triage collisions (e.g. two
	// dissimilar-description candidates in the same function that both miss
	// the earlier merge rules) issues at most one query total instead of one
	// per collision.
	refCache map[string]refCacheEntry

	// primariesByKind records cluster primaries in ARRIVAL order, per
	// defect_kind, as they are minted (see process()'s "new cluster" branch).
	// The code-nav fold consults this instead of scanning ts.clusters/
	// ts.fileClusters (whose iteration order over a Go map is undefined) so
	// which same-kind target it tries is deterministic run-to-run.
	primariesByKind map[domain.DefectKind][]*internalCluster
}

// internalCluster tracks one location cluster in the triage goroutine.
// members holds EVERY member (primary first): membership checks must run
// against the full member list (any-member, matching batch clusterAccepts
// semantics), not the primary alone — primary-only membership is strictly
// weaker and breaks transitive chains (A~B, B~C, A≁C must form ONE cluster).
type internalCluster struct {
	members     []indexedCand // all members, primary first
	fingerprint string        // primary's fingerprint (cluster registry key)
}

// newTriageState creates a triageState for one run.
func newTriageState(snap *ingest.Snapshot) (*triageState, *clusterRegistry) {
	inScope := make(map[string]bool, len(snap.Files))
	for _, file := range snap.Files {
		inScope[file.Path] = true
	}
	reg := newClusterRegistry()
	return &triageState{
		inScope:         inScope,
		seen:            make(map[string]bool),
		firstLens:       make(map[string]string),
		clusters:        make(map[string][]*internalCluster),
		fileClusters:    make(map[string][]*internalCluster),
		registry:        reg,
		resolver:        NewLocusResolver(snap.Root),
		primariesByKind: make(map[domain.DefectKind][]*internalCluster),
	}, reg
}

// process applies triage steps 1-4 and incremental clustering to one candidate.
// Cluster primaries are appended to ready; corroborating members are staged or
// forwarded to AddCorroboratingLenses depending on whether the primary has been
// persisted.
//
// Fatal errors (store I/O, ctx cancel) are returned; stats are updated.
func (ts *triageState) process(ctx context.Context, st *store.Store, stats *Stats, c Candidate) error {
	// dropPending removes this candidate's write-ahead-log row when it reaches a
	// triage terminal fate (dropped here, or merged in handleMember),
	// best-effort. A lingering row self-heals on the next run (replayed, then
	// re-dropped). On ctx cancellation the delete is a no-op, so an interrupted
	// run keeps its in-flight rows for replay.
	dropPending := func() { _ = st.DeletePendingCandidate(ctx, c.PendingID) }
	// Step 1: low confidence.
	if c.Confidence == domain.ConfidenceLow {
		stats.DroppedLowConfidence++
		dropPending()
		return nil
	}
	// Step 2: out of scope.
	if !ts.inScope[c.File] {
		stats.DroppedOutOfScope++
		dropPending()
		return nil
	}
	// Step 3: exact fingerprint dedup. The fingerprint is the durable, cross-scan
	// identity (locus + defect_kind + subject); see domain.FingerprintV3. Lens
	// is deliberately NOT part of identity: two different lenses reporting the
	// same defect_kind/subject at the same locus mint the IDENTICAL fingerprint
	// and collide right here, with no reliance on description similarity.
	locus := ts.resolver.Resolve(c.File, c.Line)
	locusKey := domain.LocusKey(c.File, locus)
	// Reverify candidates carry a durable row's already-minted identity
	// (findingToCandidate copies Fingerprint/DefectKind/Subject verbatim from
	// the stored Finding); re-deriving fp here would, for a PRE-v3 row (empty
	// DefectKind/Subject persisted before this bead), mint FingerprintV3(...,
	// "", "") — never equal to that row's stored v2-scheme fingerprint — and
	// UpsertFinding would insert a NEW row instead of updating it, orphaning
	// the original T3 finding as an untouched duplicate. Keeping the stored
	// fingerprint for Reverify candidates avoids that identity-scheme jump;
	// it matches findingToCandidate's own doc ("Fingerprint is set from the
	// finding to keep the verify-stage signals... consistent").
	fp := c.Fingerprint
	if !c.Reverify {
		fp = domain.FingerprintV3(c.File, locus, c.DefectKind, c.Subject)
	}
	if ts.seen[fp] {
		stats.DroppedDuplicate++
		// Same identity as an earlier survivor. The locus no longer carries the
		// line or title, so a collision at a new line is a genuine extra site of
		// the same bug, not a true no-op duplicate: stage it onto the primary so
		// the finding reports every location. AddStagedSite is a no-op when fp has
		// no live cluster (suppressed or never registered); AppendFindingSites
		// dedups by (file,line) and returns ErrNotFound (ignored) when the primary
		// has not yet persisted a row.
		site := domain.Site{File: c.File, Line: c.Line}
		if staged, killed := ts.registry.AddStagedSite(fp, site); !staged && !killed {
			_ = st.AppendFindingSites(ctx, fp, []domain.Site{site})
		}
		// A DIFFERENT lens minting the SAME v3 fingerprint is a genuine
		// cross-lens duplicate of one defect (identical locus, defect_kind, and
		// subject) — record it as corroboration exactly as handleMember does
		// for the cluster/root-cause merge paths, just via the exact-fingerprint
		// path instead of jaccard. Not counted in MergedCrossLens: per that
		// field's doc it tracks the location-based merge of a DIFFERENT
		// fingerprint onto a primary; this is DroppedDuplicate's own case
		// (identical fingerprint) and already accounted for above. A same-lens
		// repeat (the common re-scan case) is intentionally NOT staged as
		// corroboration of itself.
		if !strings.EqualFold(ts.firstLens[fp], c.Lens) {
			if staged, killed := ts.registry.AddStagedLens(fp, c.Lens); !staged && !killed {
				_ = st.AddCorroboratingLenses(ctx, fp, []string{c.Lens})
			}
		}
		dropPending()
		return nil
	}
	// Step 4: suppression check. locusKey backs the legacy (pre-v3) fallback:
	// a suppression recorded before defect_kind/subject existed can only have
	// been keyed by (lens, file, locus); IsSuppressed's legacy path matches on
	// locusKey alone for those rows so suppression coverage survives the v2->v3
	// cutover (see internal/store/migrations for the backfill). legacyLocusKey
	// additionally backs the bugbot-ezmx.5 content-anchor cutover: a
	// suppression minted before the content anchor existed was keyed on the
	// bare "L:<line>" fallback locus, which the resolver's content anchor no
	// longer reproduces at this file/line, so both are checked.
	legacyLocusKey := domain.LocusKey(c.File, ts.resolver.LegacyLocus(c.Line))
	suppressed, err := st.IsSuppressed(ctx, fp, locusKey, legacyLocusKey)
	if err != nil {
		return err
	}
	if suppressed {
		stats.DroppedSuppressed++
		ts.seen[fp] = true
		ts.firstLens[fp] = c.Lens
		dropPending()
		return nil
	}
	ts.seen[fp] = true
	ts.firstLens[fp] = c.Lens
	c.Fingerprint = fp
	c.LocusKey = locusKey

	// Step 5: incremental clustering. Membership is ANY-MEMBER: the candidate
	// joins a cluster if it is window-near AND token-similar to any existing
	// member (clusterAccepts — the same rule the batch algorithm used), which
	// preserves transitive chains. Checking the primary alone would be strictly
	// weaker and let chain members escape as extra primaries (extra verify
	// panels, extra false positives — reproduced by the eval corpus).
	ic := indexedCand{c: c, pos: ts.survivorCount, tok: descTokens(c.Description)}
	seenClusters := make(map[*internalCluster]bool, 4)
	for _, key := range clusterKeysForCandidate(c) {
		for _, cluster := range ts.clusters[key] {
			if seenClusters[cluster] {
				continue
			}
			seenClusters[cluster] = true
			if clusterAccepts(cluster.members, ic, DefaultMergeWindow) {
				// Member of an existing cluster: record corroboration, extend
				// the member list so later chain links can bridge through this
				// member, and alias this member's bucket to the cluster so the
				// chain stays discoverable as it spans buckets.
				ts.handleMember(ctx, st, cluster, c, stats, mergeWindow)
				cluster.members = append(cluster.members, ic)
				ts.addClusterToBucket(canonicalClusterKey(c), cluster)
				return nil
			}
			// bugbot-ezmx.2: near + kind-compatible but jaccard fell short of
			// mergeSimilarityThreshold — the collision the prose cliff cannot
			// resolve on its own. Spend one bounded LLM dedup-arbiter turn; fold
			// exactly like a jaccard-confirmed match on a confident "yes", leave
			// the candidate to continue down its normal path (next cluster, then
			// 5b/5c/5d, then new primary) on "no"/"unsure"/cap-exhausted.
			if member, collided := clusterJaccardCollision(cluster.members, ic, DefaultMergeWindow); collided {
				if ts.dedupVerdictFor(ctx, candidateDedupView(member.c), candidateDedupView(c), stats) == dedupSame {
					ts.handleMember(ctx, st, cluster, c, stats, mergeWindow)
					cluster.members = append(cluster.members, ic)
					ts.addClusterToBucket(canonicalClusterKey(c), cluster)
					return nil
				}
			}
		}
	}

	// Step 5b: same-root-cause merge — same file, beyond DefaultMergeWindow.
	// Checks clusters that share the same normalized file path.
	normFile := normPath(c.File)
	for _, cluster := range ts.fileClusters[normFile] {
		if seenClusters[cluster] {
			continue // already handled in the window-based pass above
		}
		seenClusters[cluster] = true
		if sameFileSameRootCause(cluster.members, ic) {
			ts.handleMember(ctx, st, cluster, c, stats, mergeRootCause)
			cluster.members = append(cluster.members, ic)
			return nil
		}
	}

	// Step 5c: cross-file decl/def same-root-cause merge (e.g. Foo.cpp + Foo.hpp).
	for _, candFile := range ts.crossFilePeerKeys(c.File) {
		for _, cluster := range ts.fileClusters[candFile] {
			if seenClusters[cluster] {
				continue
			}
			seenClusters[cluster] = true
			if crossFileDeclDefSameRootCause(cluster.members, ic) {
				ts.handleMember(ctx, st, cluster, c, stats, mergeRootCause)
				cluster.members = append(cluster.members, ic)
				// Also index this cross-file member under its own file key so
				// further members in the same file can bridge through it.
				ts.addToFileClusters(normFile, cluster)
				return nil
			}
		}
	}

	// Step 5d: durable cross-lens fold. In-memory clustering (5/5b/5c) only sees
	// clusters from THIS triage pass — it cannot see a primary persisted by a
	// prior (interrupted or completed) run, whose cluster state was lost on
	// restart or was never seen this run at all. Window-lookup the findings
	// table by the same-file line window (the same breadth publish-side
	// SimilarFinding adoption uses), across open/fixed/dismissed rows: an OPEN
	// hit from a DIFFERENT lens describing the same defect folds in as
	// corroboration; a DISMISSED hit suppresses the candidate outright (it is
	// a re-discovery of something already rejected); a FIXED hit reopens as a
	// regression instead of minting a new finding. All three are triage
	// terminal fates — the whole point is that they never reach verify, so no
	// refuter panel is spent re-litigating a fold the store already settled.
	// Same-lens hits are left to the fingerprint upsert. Idempotent: the lens
	// and site sets dedup, and status transitions are themselves idempotent,
	// so a replay converges.
	if folded, ferr := ts.durableCrossLensFold(ctx, st, ic, stats); ferr != nil {
		return ferr
	} else if folded {
		dropPending()
		return nil
	}
	// Step 5e: code-nav root-cause fold — the reference-hop generalization of
	// 5c beyond literal source/header mates to arbitrary caller/callee files
	// (the common multi-site case in Go, where crossFilePeerKeys never
	// matches: .go has no paired extension in sourceExtensions). See
	// codenav_fold.go for the full contract; it is a bounded, best-effort
	// heuristic that never blocks or fails triage.
	if folded, ferr := ts.codeNavRootCauseFold(ctx, st, ic, locus, stats); ferr != nil {
		return ferr
	} else if folded {
		dropPending()
		return nil
	}

	// New cluster: this candidate is the primary. A candidate bridging two
	// existing clusters joins the first match above; full cluster MERGING is
	// not attempted — both primaries were already forwarded, and forwarding is
	// irreversible (documented relaxation; the batch algorithm's closure would
	// have produced one cluster only if the bridge arrived before forwarding).
	nc := &internalCluster{members: []indexedCand{ic}, fingerprint: fp}
	ts.addClusterToBucket(canonicalClusterKey(c), nc)
	ts.addToFileClusters(normFile, nc)
	ts.registry.Register(fp)
	// Record this primary as a same-defect_kind fold target for LATER
	// candidates' code-nav root-cause fold (step 5e above). Empty defect_kind
	// (legacy Reverify rows) is never indexed here: the fold treats empty as a
	// mismatch, so it would never be looked up under "" anyway.
	if c.DefectKind != "" {
		ts.primariesByKind[c.DefectKind] = append(ts.primariesByKind[c.DefectKind], nc)
	}
	// Initialize Sites with the primary's own location.
	c.Sites = []Site{{File: c.File, Line: c.Line}}
	ts.ready = append(ts.ready, c)
	ts.survivorCount++
	return nil
}

// durableCrossLensFold absorbs candidate ic into an already-persisted finding
// at the same file, within the same line window SimilarFinding uses, when it
// is the same defect under a different identity than an exact-fingerprint
// match would catch. It exists because in-memory clustering is rebuilt from
// scratch each run: a primary persisted by a prior run — interrupted, or
// simply not seen by this run's own candidate pool — is invisible to it, so a
// re-discovered sibling would otherwise become a second finding.
// FindingsByFileWindow is an indexed lookup returning the handful of findings
// near one line in one file.
//
// The lookup is widened from the original exact-locusKey/OPEN-only point
// lookup: a locus-key hit is a proper subset of the same-file window (same
// enclosing symbol implies same file, nearby lines), so this also catches the
// case a locus-key match could not — the candidate's fingerprint (and derived
// locus_key) drifted because the enclosing symbol was renamed, even though
// the code didn't meaningfully move. It also now considers fixed/dismissed
// rows, not just open ones, so a re-discovered duplicate of an already-
// resolved defect dies here instead of reaching verify.
//
// Under Fingerprint v3, an exact-fingerprint match (identical locus,
// defect_kind, AND subject — regardless of lens) is already handled by the
// ordinary upsert-by-fingerprint path, so it is skipped here as a no-op, not
// folded. What remains for this function is the genuinely ambiguous case:
// same-file window, SAME defect_kind (a different defect_kind at this locus
// is, by design, a distinct bug — see domain.FingerprintV3 — and must never
// fold), but a subject/description that differs enough to mint a different
// fingerprint (e.g. the model phrased the subject differently). description
// jaccard (SimilarFinding) is the tiebreaker for exactly that residual case.
// bugbot-ezmx.2's LLM dedup arbiter is a further fallback on a SimilarFinding
// miss, but ONLY for the OPEN branch (a plain corroboration fold): the
// dismissed-suppression and fixed-regression branches are STATE-CHANGING and
// stay deterministic-only (SimilarFinding must actually match) — an arbiter
// false-positive there would silently suppress or reopen a finding instead
// of merely keeping an extra candidate around, which is a materially worse
// mistake than the arbiter is trusted for. The broader root-cause layers
// (5b/5c) are deliberately NOT mirrored here, so the durable path only ever
// under-merges, never more aggressively. Reverify candidates are excluded:
// they own a durable row to re-judge and must not be absorbed elsewhere.
// Returns true when the candidate was folded (a triage terminal fate). The
// store writes dedup, so the fold is idempotent on replay.
func (ts *triageState) durableCrossLensFold(ctx context.Context, st *store.Store, ic indexedCand, stats *Stats) (bool, error) {
	c := ic.c
	if c.Reverify || c.LocusKey == "" {
		return false, nil
	}
	existing, err := st.FindingsByFileWindow(ctx, c.File, c.Line, DefaultMergeWindow,
		[]domain.Status{domain.StatusOpen, domain.StatusFixed, domain.StatusDismissed})
	if err != nil {
		return false, err
	}
	for _, f := range existing {
		if f.Fingerprint == c.Fingerprint {
			// Identical v3 identity (locus + defect_kind + subject): the ordinary
			// upsert-by-fingerprint path already lands this candidate on the same
			// row once it clears verify. Nothing to fold here.
			continue
		}
		if f.DefectKind != "" && f.DefectKind != c.DefectKind {
			// A DIFFERENT defect_kind in this window is a distinct defect by
			// design — never fold across kinds, regardless of description overlap.
			continue
		}
		if strings.EqualFold(f.Lens, c.Lens) {
			// Same lens, same defect_kind, but the fingerprint differs (a subject
			// phrasing drift within one lens's own re-report): not a cross-lens
			// fold case; leave it to the exact-fingerprint upsert path.
			continue
		}
		similar := SimilarFinding(c.File, c.Line, c.Description, f.File, f.Line, f.Description)
		switch f.Status {
		case domain.StatusDismissed:
			// Deterministic-only: a maintainer's dismissal is only ever mirrored
			// onto the candidate on an actual SimilarFinding match, never on an
			// arbiter guess — see the function doc.
			if !similar {
				continue
			}
			// The re-discovery matches a defect a maintainer already rejected:
			// suppress this candidate's own fingerprint exactly as if it had been
			// dismissed directly, so future scans skip it at the step-4
			// IsSuppressed check too (converges with bugbot-oiem, which owns the
			// suppression-side identity concern).
			reason := fmt.Sprintf("durable fold: same-file-window match of dismissed finding %s", f.Fingerprint)
			if err := st.AddSuppression(ctx, c.Fingerprint, reason); err != nil {
				return false, err
			}
			stats.DroppedSuppressedDurable++
			return true, nil
		case domain.StatusFixed:
			// Deterministic-only, same rationale as the dismissed branch: a
			// regression reopen is a state-changing mutation of a settled row.
			if !similar {
				continue
			}
			// The re-discovery matches a defect already marked fixed: treat it as
			// a regression of THAT row instead of minting a new finding. Identity
			// and history (tier, repro artifacts) are preserved by
			// ReopenAsRegression; the new lens/site are attached exactly as the
			// open-finding fold below does.
			if err := st.ReopenAsRegression(ctx, f.Fingerprint); err != nil {
				return false, err
			}
			if err := st.AddCorroboratingLenses(ctx, f.Fingerprint, []string{c.Lens}); err != nil {
				return false, err
			}
			if err := st.AppendFindingSites(ctx, f.Fingerprint, []domain.Site{{File: c.File, Line: c.Line}}); err != nil {
				return false, err
			}
			stats.RegressionReopened++
			return true, nil
		default: // domain.StatusOpen
			if !similar {
				// bugbot-ezmx.2: same-file window, same/unknown defect_kind,
				// different lens, but jaccard (SimilarFinding's tiebreaker) fell
				// short — spend one bounded LLM dedup-arbiter turn before giving
				// up on this row. A confident "yes" folds exactly like a
				// SimilarFinding match; "no", "unsure", or cap-exhausted leave the
				// candidate to the next existing finding (or, having exhausted
				// existing, a new primary). Restricted to this OPEN branch only —
				// see the function doc for why dismissed/fixed stay deterministic.
				if ts.dedupVerdictFor(ctx, candidateDedupView(c), findingDedupView(f), stats) != dedupSame {
					continue
				}
			}
			// The row was just read under the single-writer lock, so a non-nil
			// error here is a genuine I/O failure, not a benign race — propagate it.
			if err := st.AddCorroboratingLenses(ctx, f.Fingerprint, []string{c.Lens}); err != nil {
				return false, err
			}
			if err := st.AppendFindingSites(ctx, f.Fingerprint, []domain.Site{{File: c.File, Line: c.Line}}); err != nil {
				return false, err
			}
			stats.MergedCrossLensDurable++
			return true, nil
		}
	}
	return false, nil
}

// addClusterToBucket registers cluster under the bucket key unless already
// present (a cluster may be aliased into several buckets as its members span
// windows; buckets may hold several token-dissimilar clusters).
func (ts *triageState) addClusterToBucket(key string, cluster *internalCluster) {
	for _, existing := range ts.clusters[key] {
		if existing == cluster {
			return
		}
	}
	ts.clusters[key] = append(ts.clusters[key], cluster)
}

// mergeKind distinguishes which Stats counter a corroborating member
// increments in handleMember, and whether the merge is "root-cause-shaped"
// (always stages a corroborating lens on a cross-lens hit; the ordinary
// window merge only counts it once via MergedCrossLens/MergedWithinLens).
type mergeKind int

const (
	// mergeWindow is the ordinary step-5 proximity + description-similarity
	// merge.
	mergeWindow mergeKind = iota
	// mergeRootCause is the step-5b (same-file broad-window) or step-5c
	// (cross-file decl/def) same-root-cause merge.
	mergeRootCause
	// mergeCodeNav is the step-5e code-nav reference-hop fold
	// (codenav_fold.go): a candidate one hop from an in-run cluster primary
	// of the same defect_kind, discovered via the code-navigation backend
	// rather than filename pattern matching or description similarity.
	mergeCodeNav
)

// handleMember handles a corroborating member of an existing cluster. kind
// selects which Stats counter is incremented and, for mergeRootCause/
// mergeCodeNav, always stages a corroborating lens on a cross-lens hit (see
// mergeKind).
func (ts *triageState) handleMember(ctx context.Context, st *store.Store, cluster *internalCluster, c Candidate, stats *Stats, kind mergeKind) {
	// This member is merged into an existing cluster (its lens may be recorded as
	// corroboration, but its own claim does not proceed to verify): a triage
	// terminal fate. Drop its write-ahead-log row, best-effort. The cluster
	// primary carries the cluster forward and deletes its own row at its verify
	// terminal fate.
	_ = st.DeletePendingCandidate(ctx, c.PendingID)

	// Stage this member's site so it reaches the persisted Finding. Because the
	// primary may already be forwarded to verify (ts.ready drained), we mirror
	// the corroborating-lens mechanism: stage in the registry for
	// DrainStagedSites to pick up at persist time, or update the store directly
	// when the primary is already persisted.
	site := domain.Site{File: c.File, Line: c.Line}
	siteStaged, siteKilled := ts.registry.AddStagedSite(cluster.fingerprint, site)
	if !siteStaged && !siteKilled {
		// Primary already persisted; update the store row directly. Best-effort.
		_ = st.AppendFindingSites(ctx, cluster.fingerprint, []domain.Site{site})
	}

	if kind == mergeRootCause || kind == mergeCodeNav {
		if kind == mergeRootCause {
			stats.MergedRootCause++
		} else {
			stats.MergedRootCauseCodeNav++
		}
		// For root-cause merges that are also same-lens, no new corroborating lens.
		if strings.EqualFold(c.Lens, cluster.members[0].c.Lens) {
			return
		}
		// Cross-lens root-cause: also record corroboration so the lens is visible.
		lens := c.Lens
		staged2, killed2 := ts.registry.AddStagedLens(cluster.fingerprint, lens)
		if staged2 || killed2 {
			return
		}
		_ = st.AddCorroboratingLenses(ctx, cluster.fingerprint, []string{lens})
		return
	}

	if strings.EqualFold(c.Lens, cluster.members[0].c.Lens) {
		// Same-lens merge: within-lens, no new corroborating lens.
		stats.MergedWithinLens++
		return
	}
	// Cross-lens merge: record corroboration.
	stats.MergedCrossLens++
	lens := c.Lens

	// Try to stage the lens in the registry. If the primary was already
	// persisted, use AddCorroboratingLenses directly. If killed, discard.
	staged3, killed3 := ts.registry.AddStagedLens(cluster.fingerprint, lens)
	if staged3 {
		return // Staged for attach at persist time.
	}
	if killed3 {
		return // Primary was killed; corroboration is moot.
	}
	// Primary was persisted before this member arrived: update the store directly.
	// Best-effort: a failure here loses this corroboration but doesn't abort the run.
	_ = st.AddCorroboratingLenses(ctx, cluster.fingerprint, []string{lens})
}

// addToFileClusters registers cluster under the file key unless already present.
func (ts *triageState) addToFileClusters(normFile string, cluster *internalCluster) {
	for _, existing := range ts.fileClusters[normFile] {
		if existing == cluster {
			return
		}
	}
	ts.fileClusters[normFile] = append(ts.fileClusters[normFile], cluster)
}

// crossFilePeerKeys returns the normalized file paths of potential source/header
// mates for file. Used in step 5c to look up clusters from paired files.
// Only same-directory same-stem paired-extension keys are returned, matching
// the isSrcHdrPair requirement (prevents cross-directory same-stem matches).
func (ts *triageState) crossFilePeerKeys(file string) []string {
	norm := normPath(file)
	ext := fileExt(file)
	mates, ok := sourceExtensions[ext]
	if !ok {
		return nil
	}
	stem := fileStem(file)
	dir := fileDir(file)
	// Collect all file-cluster keys that share same dir + stem + a mating extension.
	var keys []string
	seen := make(map[string]bool)
	for _, mateExt := range mates {
		for k := range ts.fileClusters {
			if seen[k] || k == norm {
				continue
			}
			if fileDir(k) == dir && fileStem(k) == stem && fileExt(k) == mateExt {
				keys = append(keys, k)
				seen[k] = true
			}
		}
	}
	return keys
}

// flush is a no-op in the streaming model: all primaries are forwarded
// immediately when first seen. Called when candCh is closed.
func (ts *triageState) flush() {}

// popReady drains and returns the ready primaries (cluster primaries waiting to
// be forwarded to verify).
func (ts *triageState) popReady() []Candidate {
	out := ts.ready
	ts.ready = nil
	return out
}

// clusterKeysForCandidate returns the bucket keys to check for an incoming
// candidate: its own bucket plus adjacent buckets so window-spanning pairs
// are caught.
func clusterKeysForCandidate(c Candidate) []string {
	norm := normPath(c.File)
	bucket := (c.Line / DefaultMergeWindow) * DefaultMergeWindow
	keys := make([]string, 0, 3)
	if bucket > 0 {
		keys = append(keys, clusterKey(norm, bucket-DefaultMergeWindow))
	}
	keys = append(keys, clusterKey(norm, bucket))
	keys = append(keys, clusterKey(norm, bucket+DefaultMergeWindow))
	return keys
}

// canonicalClusterKey returns the bucket key for storing a new cluster primary.
func canonicalClusterKey(c Candidate) string {
	bucket := (c.Line / DefaultMergeWindow) * DefaultMergeWindow
	return clusterKey(normPath(c.File), bucket)
}

func clusterKey(normFile string, bucket int) string {
	return normFile + "\x00" + itoa(bucket)
}

// itoa converts a non-negative int to decimal string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + itoa(-n)
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

// dedupLenses deduplicates and sorts a slice of lens names.
func dedupLenses(lenses []string) []string {
	if len(lenses) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(lenses))
	for _, l := range lenses {
		seen[l] = true
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
