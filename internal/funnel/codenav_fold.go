package funnel

// codenav_fold.go implements triage step 5e: the code-nav root-cause fold.
// See triage_streaming.go's process() step-5e call site and the mergeCodeNav
// doc for how this fits among the other merge layers.

import (
	"context"
	"strings"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// codeNavRefs is the seam triage's code-nav root-cause fold uses to ask "what
// references this symbol". Its signature exactly matches
// (*agent.CodeNav).References, so *agent.CodeNav satisfies it with no
// adapter; tests stub it directly, without a real language server.
type codeNavRefs interface {
	References(ctx context.Context, file string, line int, symbol string) ([]agent.RefLocation, error)
}

// refCacheEntry memoizes one code-nav query's outcome (including a failure,
// so a persistently erroring symbol is not re-queried within the same scan).
type refCacheEntry struct {
	locs []agent.RefLocation
	err  error
}

// symbolFromLocus extracts the symbol name from an "S:<kind>\x00<name>"
// locus (see LocusResolver.Resolve). ok is false for any fallback form (the
// content-anchored "C:..." or bare "L:<line>"), neither of which carries a
// symbol name to query code navigation with.
func symbolFromLocus(locus string) (string, bool) {
	if !strings.HasPrefix(locus, "S:") {
		return "", false
	}
	rest := locus[2:]
	i := strings.IndexByte(rest, 0)
	if i < 0 {
		return "", false
	}
	return rest[i+1:], true
}

// codeNavNominationExcludedKinds are the catch-all defect_kind values excluded
// from step 5e's nomination entirely. "logic" and "other" are low-signal:
// they dominate real findings' kind distribution (almost anything can be
// tagged one of the two when a finder is unsure), so kind-matching on them
// carries none of the specificity the other seven kinds provide — nominating
// on kind+hop alone for these two would flag far more pairs than the arbiter
// could usefully adjudicate, for the least meaningful signal in the set.
var codeNavNominationExcludedKinds = map[domain.DefectKind]bool{
	domain.DefectLogic: true,
	domain.DefectOther: true,
}

// codeNavRootCauseFold is triage step 5e: the reference-hop generalization of
// the filename-pattern-based cross-file same-root-cause merge (5c). Real
// multi-site Go defects are commonly reported by one finder at a call site
// and another finder inside the called function's body — files with no
// filename relationship crossFilePeerKeys can discover (.go has no paired
// extension in sourceExtensions; even in C/C++, unrelated .cpp files
// reporting the two sides of one bug never share a stem). Only code
// navigation — "does A reference B" — can bridge that gap.
//
// The reference hop is a NOMINATION, never a decision on its own: kind+hop
// alone is not a merge signal (a caller can reference a callee for a hundred
// reasons that have nothing to do with either finding). A nominated pair is
// routed through the SAME LLM dedup arbiter (dedup_arbiter.go,
// ts.dedupVerdictFor) the jaccard-gate collisions at steps 5 and 5d use — the
// fold proceeds ONLY on a typed "yes". "no", "unsure", a nil arbiter
// (disabled), or a cap-exhausted scan all keep both candidates as separate
// primaries, identically.
//
// Conservative by further design (this heuristic is tried LAST before minting
// a new primary):
//
//   - ic.c.DefectKind must be NON-EMPTY and match the target's exactly.
//     Unlike sameOrUnknownKind, an empty kind is a MISMATCH here, not a
//     wildcard: a candidate with no defect_kind gives the reference-hop check
//     no signal to distinguish "same bug, different site" from "two unrelated
//     bugs that happen to be one call apart".
//   - The catch-all kinds "logic"/"other" never nominate (see
//     codeNavNominationExcludedKinds).
//   - ic.c.Reverify candidates never nominate, mirroring durableCrossLensFold's
//     own invariant: a Reverify candidate owns a durable row being re-judged
//     and must not be absorbed elsewhere.
//   - Bounded to AT MOST ONE code-nav query per collision: ic's own enclosing
//     symbol is queried for its references exactly once (memoized in
//     ts.refCache, keyed on file+declaration-line+symbol, so a symbol
//     re-evaluated across multiple collisions this scan is never re-queried),
//     and every candidate target (in-run cluster primary, then durably
//     persisted open finding) is checked against that SAME result set in
//     memory. This is directional — it only discovers "ic's symbol is
//     referenced from inside the target's code" (e.g. ic is a callee-side
//     report and the target is the caller), never the reverse — a deliberate
//     trade against issuing a second query per candidate target.
//   - Nav absence (ts.nav == nil), an unresolvable locus (a fallback anchor,
//     no symbol), or a query error/timeout all degrade to "no fold" — this
//     heuristic must never block or fail triage.
//   - In-run cluster targets are tried in ARRIVAL order (ts.primariesByKind),
//     not in the order code navigation happens to return references — LSP
//     reference ordering is not a meaningful tie-break for which target wins.
//
// On a confirmed hit, ic is attached as a site of the target: an in-run
// cluster via handleMember(mergeCodeNav) (counted in
// Stats.MergedRootCauseCodeNav, which — like MergedRootCause — counts toward
// DuplicateRate: it collapses two members of this run's own candidate pool),
// or a durably persisted open finding via the same store calls
// durableCrossLensFold uses (counted in Stats.MergedCrossLensDurableCodeNav,
// excluded from DuplicateRate for the same reason MergedCrossLensDurable is:
// cross-scan reconciliation, not in-run dedup).
func (ts *triageState) codeNavRootCauseFold(ctx context.Context, st *store.Store, ic indexedCand, locus string, stats *Stats) (bool, error) {
	if ts.nav == nil || ic.c.DefectKind == "" || ic.c.Reverify {
		return false, nil
	}
	if codeNavNominationExcludedKinds[ic.c.DefectKind] {
		return false, nil
	}
	sym, ok := symbolFromLocus(locus)
	if !ok {
		return false, nil
	}
	entry, ok := ts.resolver.enclosing(ic.c.File, ic.c.Line)
	if !ok {
		return false, nil
	}

	refs, err := ts.refs(ctx, normPath(ic.c.File), entry.StartLine, sym)
	if err != nil || len(refs) == 0 {
		// A nav problem or a symbol with no references: not a fold, never an
		// error — this fold is a heuristic and must never block triage.
		return false, nil
	}

	// In-run cluster targets first (no additional I/O to find them), tried in
	// ARRIVAL order.
	for _, cluster := range ts.primariesByKind[ic.c.DefectKind] {
		primary := cluster.members[0]
		pe, ok := ts.resolver.enclosing(primary.c.File, primary.c.Line)
		if !ok {
			continue
		}
		pFile := normPath(primary.c.File)
		hop := false
		for _, ref := range refs {
			if normPath(ref.File) == pFile && ref.Line >= pe.StartLine && ref.Line <= pe.EndLine {
				hop = true
				break
			}
		}
		if !hop {
			continue
		}
		// Nomination only: the hop is not a decision. Route through the arbiter;
		// fold ONLY on a confident "yes".
		if ts.dedupVerdictFor(ctx, candidateDedupView(primary.c), candidateDedupView(ic.c), stats) != dedupSame {
			continue
		}
		ts.handleMember(ctx, st, cluster, ic.c, stats, mergeCodeNav)
		cluster.members = append(cluster.members, ic)
		return true, nil
	}

	// Durably persisted OPEN findings: for each reference location, look up
	// findings in the same-file window (the same indexed lookup
	// durableCrossLensFold uses), gate on the SAME defect_kind, then route
	// the pair through the arbiter exactly like the in-run branch above.
	for _, ref := range refs {
		findings, ferr := st.FindingsByFileWindow(ctx, ref.File, ref.Line, DefaultMergeWindow, []domain.Status{domain.StatusOpen})
		if ferr != nil {
			return false, ferr
		}
		for _, f := range findings {
			if f.DefectKind != ic.c.DefectKind {
				continue
			}
			if ts.dedupVerdictFor(ctx, candidateDedupView(ic.c), findingDedupView(f), stats) != dedupSame {
				continue
			}
			if err := st.AppendFindingSites(ctx, f.Fingerprint, []domain.Site{{File: ic.c.File, Line: ic.c.Line}}); err != nil {
				return false, err
			}
			if !strings.EqualFold(f.Lens, ic.c.Lens) {
				if err := st.AddCorroboratingLenses(ctx, f.Fingerprint, []string{ic.c.Lens}); err != nil {
					return false, err
				}
			}
			stats.MergedCrossLensDurableCodeNav++
			return true, nil
		}
	}
	return false, nil
}

// refs issues at most one code-nav reference query per (file, line, symbol)
// per scan, memoizing the result in ts.refCache. The declaration line is part
// of the key (not just file+symbol) so a same-named function/method declared
// on a different line — a legitimate collision in a large file, or across two
// distinctly-resolved locus entries — never reuses another symbol's answer.
// Called with the CANDIDATE's own enclosing file+declaration-line+symbol —
// never per target — so a collision that checks N candidate targets still
// issues at most one query.
func (ts *triageState) refs(ctx context.Context, file string, line int, sym string) ([]agent.RefLocation, error) {
	key := file + "\x00" + itoa(line) + "\x00" + sym
	if ts.refCache == nil {
		ts.refCache = make(map[string]refCacheEntry)
	}
	if e, ok := ts.refCache[key]; ok {
		return e.locs, e.err
	}
	locs, err := ts.nav.References(ctx, file, line, sym)
	ts.refCache[key] = refCacheEntry{locs: locs, err: err}
	return locs, err
}
