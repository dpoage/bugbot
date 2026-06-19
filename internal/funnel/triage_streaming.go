package funnel

import (
	"context"
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
		return nil
	}
	e.persisted = true
	late := e.stagedLenses
	e.stagedLenses = nil
	if len(late) == 0 {
		return nil
	}
	// These are store-updated by the caller AND folded into the in-memory
	// finding by run() at drain time.
	e.attachedLate = append(e.attachedLate, late...)
	return dedupLenses(late)
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

	// Incremental cluster state: maps clusterKey (location bucket) → the
	// clusters anchored in that bucket. A SLICE per bucket is load-bearing:
	// token-DISSIMILAR defects can share a location bucket (batch mergeClusters
	// splits them by jaccard into separate clusters), and a single-cluster-per-
	// bucket map lets each dissimilar arrival overwrite the bucket's pointer,
	// orphaning the previous group so its later members become spurious
	// primaries — reproduced by the recorded eval corpus.
	clusters map[string][]*internalCluster

	// registry is shared with verify goroutines for staged-lens coordination.
	registry *clusterRegistry

	// ready holds primaries to forward to verify. Drained by popReady().
	ready []Candidate

	// survivorCount is the number of cluster primaries forwarded.
	survivorCount int
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
		inScope:  inScope,
		seen:     make(map[string]bool),
		clusters: make(map[string][]*internalCluster),
		registry: reg,
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
	// Step 3: exact fingerprint dedup.
	fp := store.Fingerprint(c.Lens, c.File, c.Line, c.Title)
	if ts.seen[fp] {
		stats.DroppedDuplicate++
		dropPending()
		return nil
	}
	// Step 4: suppression check.
	suppressed, err := st.IsSuppressed(ctx, fp)
	if err != nil {
		return err
	}
	if suppressed {
		stats.DroppedSuppressed++
		ts.seen[fp] = true
		dropPending()
		return nil
	}
	ts.seen[fp] = true
	c.Fingerprint = fp

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
				ts.handleMember(ctx, st, cluster, c, stats)
				cluster.members = append(cluster.members, ic)
				ts.addClusterToBucket(canonicalClusterKey(c), cluster)
				return nil
			}
		}
	}

	// New cluster: this candidate is the primary. A candidate bridging two
	// existing clusters joins the first match above; full cluster MERGING is
	// not attempted — both primaries were already forwarded, and forwarding is
	// irreversible (documented relaxation; the batch algorithm's closure would
	// have produced one cluster only if the bridge arrived before forwarding).
	nc := &internalCluster{members: []indexedCand{ic}, fingerprint: fp}
	ts.addClusterToBucket(canonicalClusterKey(c), nc)
	ts.registry.Register(fp)
	ts.ready = append(ts.ready, c)
	ts.survivorCount++
	return nil
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

// handleMember handles a corroborating member of an existing cluster.
func (ts *triageState) handleMember(ctx context.Context, st *store.Store, cluster *internalCluster, c Candidate, stats *Stats) {
	// This member is merged into an existing cluster (its lens may be recorded as
	// corroboration, but its own claim does not proceed to verify): a triage
	// terminal fate. Drop its write-ahead-log row, best-effort. The cluster
	// primary carries the cluster forward and deletes its own row at its verify
	// terminal fate.
	_ = st.DeletePendingCandidate(ctx, c.PendingID)
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
	staged, killed := ts.registry.AddStagedLens(cluster.fingerprint, lens)
	if staged {
		return // Staged for attach at persist time.
	}
	if killed {
		return // Primary was killed; corroboration is moot.
	}
	// Primary was persisted before this member arrived: update the store directly.
	// Best-effort: a failure here loses this corroboration but doesn't abort the run.
	_ = st.AddCorroboratingLenses(ctx, cluster.fingerprint, []string{lens})
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
