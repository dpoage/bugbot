package funnel

// reconcile.go implements the bugbot-ezmx.4 backlog reconcile pass: a
// standalone daemon maintenance cycle (the "runVerifyDrain pattern") that
// heals duplicate OPEN findings already persisted in the store. Nothing else
// heals this class of duplicate: the streaming triage bridge-merge
// relaxation can forward two primaries a batch scan would have merged, and
// pre-v3 history accumulated duplicates before Fingerprint v3's
// defect_kind/subject identity existed. ReconcileDedup groups OPEN findings
// by file, nominates candidate pairs under the same deterministic pre-gate
// the live collision sites use (same/unknown defect_kind, SimilarFinding-
// close descriptions), and asks the bugbot-ezmx.2 dedup arbiter seam
// (runDedupArbiter) to confirm before merging -- unlike the live in-scan
// SimilarFinding match, reconcile NEVER auto-merges on the deterministic
// gate alone: every nominated pair is already two independently PERSISTED
// findings, so a wrong auto-merge is a materially worse mistake than in-run
// clustering (precision-first, same rationale dedup_arbiter.go documents).
import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// DefaultReconcileCap is the per-cycle cap on LLM dedup-arbiter invocations
// during backlog reconcile, mirroring DefaultDedupArbiterCap's rationale: a
// bounded cheap-model turn spent only on deterministic-gate survivors, never
// one per candidate pair. A daemon reconcile cycle typically finds a handful
// of nominated pairs (backlog duplicates are the exception, not the norm),
// so this is generous headroom rather than a routinely-hit ceiling.
const DefaultReconcileCap = 25

// reconcilePair is one nominated candidate pair: older survives as the
// canonical row, newer is the merge candidate that closes StatusSuperseded
// on a confident arbiter "yes".
type reconcilePair struct {
	older domain.Finding
	newer domain.Finding
}

// nominateReconcilePairs groups open findings by normalized file path and
// nominates deterministic candidate pairs within DefaultMergeWindow lines,
// compatible defect_kind (sameOrUnknownKind, checked FIRST so a kind
// mismatch is never even considered for the jaccard gate -- no wasted work,
// and it proves out as "never nominated" for kind-mismatched pairs), and
// SimilarFinding-close descriptions -- the exact same file/window/jaccard
// predicate the live collision sites gate their own merges on, so reconcile
// only ever surfaces pairs a live scan's own fold logic would also have
// flagged as a collision.
//
// Within a file, findings are ordered oldest-created-first (ties broken by
// ID for determinism) and paired greedily: each finding participates in at
// most one pair per cycle. This keeps a chain of 3+ near-duplicates from
// fanning out combinatorially -- one merge lands per cycle, and any residual
// pair is picked up by the next cycle once the merged row's status has
// settled to StatusSuperseded (excluded from the next cycle's OPEN query).
// pair.older is always the earlier-created row: ReconcileDedup's
// canonical-row convention is "the older row survives, the newer merges
// away", matching an operator's intuition that the first report of a defect
// is the one worth keeping the history of.
func nominateReconcilePairs(open []domain.Finding) []reconcilePair {
	byFile := make(map[string][]domain.Finding)
	for _, f := range open {
		nf := normPath(f.File)
		byFile[nf] = append(byFile[nf], f)
	}
	files := make([]string, 0, len(byFile))
	for nf := range byFile {
		files = append(files, nf)
	}
	sort.Strings(files)

	var pairs []reconcilePair
	for _, nf := range files {
		group := byFile[nf]
		sort.Slice(group, func(i, j int) bool {
			if group[i].CreatedAt.Equal(group[j].CreatedAt) {
				return group[i].ID < group[j].ID
			}
			return group[i].CreatedAt.Before(group[j].CreatedAt)
		})
		claimed := make([]bool, len(group))
		for i := range group {
			if claimed[i] {
				continue
			}
			for j := i + 1; j < len(group); j++ {
				if claimed[j] {
					continue
				}
				a, b := group[i], group[j]
				if a.Fingerprint == b.Fingerprint {
					continue
				}
				if !sameOrUnknownKind(a.DefectKind, b.DefectKind) {
					continue
				}
				if !SimilarFinding(a.File, a.Line, a.Description, b.File, b.Line, b.Description) {
					continue
				}
				pairs = append(pairs, reconcilePair{older: a, newer: b})
				claimed[i] = true
				claimed[j] = true
				break
			}
		}
	}
	return pairs
}

// ReconcileDedup runs one backlog-reconcile cycle: nominate candidate pairs
// among currently-OPEN findings (nominateReconcilePairs), confirm each
// nominated pair through the shared dedup arbiter seam up to cap
// invocations (cap <= 0 uses DefaultReconcileCap), and merge every confident
// "yes" -- AppendFindingSites + AddCorroboratingLenses fold the newer row's
// sites/lenses onto the older (canonical) row, then the newer row closes
// StatusSuperseded via SupersedeAsDuplicate with a reason referencing the
// canonical fingerprint. "no"/"unsure"/arbiter-failure all leave both rows
// untouched -- precision-first, matching the live dedup arbiter's contract.
//
// Idempotent: a merged-away row is StatusSuperseded, so the next cycle's
// OPEN-only query excludes it entirely -- re-running ReconcileDedup
// immediately after a merge nominates nothing for that pair and is a no-op
// (no new scan run, no arbiter calls, no writes).
//
// Cheap when there is nothing to nominate: the OPEN-findings query runs
// first, and an empty nomination list returns before BeginScanRun, mirroring
// VerifyDrain/SweepDrain's empty-WAL fast path.
func (f *Funnel) ReconcileDedup(ctx context.Context, cap int) (*Result, error) {
	if cap <= 0 {
		cap = DefaultReconcileCap
	}
	open, err := f.store.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		return nil, fmt.Errorf("reconcile: ListFindings: %w", err)
	}
	pairs := nominateReconcilePairs(open)
	result := &Result{}
	if len(pairs) == 0 {
		return result, nil
	}

	scanRunID, err := f.store.BeginScanRun(ctx, store.ScanReconcile, "")
	if err != nil {
		return nil, fmt.Errorf("reconcile: BeginScanRun: %w", err)
	}
	result.ScanRunID = scanRunID

	rec := &spendRecorder{ctx: ctx, store: f.store, scanRunID: scanRunID}
	verifierClient := llm.WithRecorder(f.clients.Verifier, rec, roleVerifier, "", "")
	cacheWeight := f.opts.Budget.CacheReadBudgetWeight
	budget := newBudgetState(f.opts.Budget.TokenBudget, rec, cacheWeight)

	// Interrupt-safe finalization: seal the scan_runs row on every exit path,
	// mirroring SweepDrain.
	finalize := func(s *Stats) {
		statsJSON, merr := json.Marshal(s)
		if merr != nil {
			statsJSON = []byte(`{"aborted":true}`)
		}
		fCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ferr := f.store.FinishScanRun(fCtx, scanRunID, string(statsJSON)); ferr != nil {
			f.note(result, fmt.Sprintf("reconcile: FinishScanRun failed: %v", ferr))
		}
	}
	defer func() { finalize(&result.Stats) }()

	stats := &result.Stats
	root := f.repo.Root()
	invoked := 0
	var merged []domain.Finding
	for _, p := range pairs {
		if ctx.Err() != nil {
			break
		}
		stats.ReconcileNominated++
		if invoked >= cap {
			stats.ReconcileSkippedCap++
			continue
		}
		invoked++
		stats.ReconcileArbitrated++

		excerpt := dedupCodeExcerpt(root, p.newer.File, p.newer.Line, dedupExcerptWindow)
		verdict, tokens, verr := f.runDedupArbiter(ctx, verifierClient, budget, findingDedupView(p.older), findingDedupView(p.newer), excerpt)
		stats.DedupArbiterTokens += tokens
		if verr != nil {
			stats.ReconcileFailures++
			continue
		}
		if verdict != dedupSame {
			continue
		}

		if err := f.store.AddCorroboratingLenses(ctx, p.older.Fingerprint, []string{p.newer.Lens}); err != nil {
			return nil, fmt.Errorf("reconcile: AddCorroboratingLenses: %w", err)
		}
		if err := f.store.AppendFindingSites(ctx, p.older.Fingerprint, p.newer.Sites); err != nil {
			return nil, fmt.Errorf("reconcile: AppendFindingSites: %w", err)
		}
		reason := fmt.Sprintf("backlog reconcile: merged into %s (dedup arbiter yes)", p.older.Fingerprint)
		if err := f.store.SupersedeAsDuplicate(ctx, p.newer.Fingerprint, p.older.Fingerprint, reason); err != nil {
			return nil, fmt.Errorf("reconcile: SupersedeAsDuplicate: %w", err)
		}
		stats.ReconcileMerged++
		merged = append(merged, p.newer)
	}
	result.Findings = merged
	return result, nil
}
