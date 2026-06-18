package funnel

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// Estimate is a pre-scan projection of a single funnel run. It is produced by
// EstimateScan WITHOUT making any LLM call: the work breakdown (files, packages,
// chunks, lenses, finder units, cartographer packages) is EXACT — computed with
// the same functions the real run uses, so it can never drift from reality — and
// the token/duration figures are projected from recorded history (or labeled
// built-in priors when no history exists).
type Estimate struct {
	Kind   store.ScanKind
	Commit string

	// --- Deterministic work breakdown (exact) ---------------------------

	// Files is the number of in-scope target files (the whole snapshot on a
	// Sweep; the blast radius intersected with scope on a Targeted scan).
	Files int
	// Packages is the number of distinct packages the targets span.
	Packages int
	// Chunks is the number of finder file-chunks (chunkByLanguage).
	Chunks int
	// Lenses is the number of active lenses selected for this run (before the
	// per-chunk language gate). FinderUnits is the figure that actually drives
	// cost; Lenses is reported for context.
	Lenses int
	// FinderUnits is the EXACT number of finder agents that will launch: the
	// per-chunk (lens × strategy) units plus the diff-intent unit (when
	// applicable) plus one unit per cross-language seam. It is the dominant,
	// predictable cost driver and the basis of the token/time projection.
	FinderUnits int
	// Seams is the number of cross-language-boundary units (one per seam).
	Seams int
	// DiffIntent reports whether a diff-intent unit will be emitted (Targeted
	// run with a populated ChangeContext).
	DiffIntent bool

	// --- Cartographer (deterministic) -----------------------------------

	CartographerEnabled bool
	// CartographerPackages is the number of packages the cartographer pass
	// considers (packages spanned by targets). CartographerUncached is the
	// subset whose cached summary is missing or stale and therefore needs a
	// fresh one-shot completion this run — the only ones that cost tokens.
	CartographerPackages int
	CartographerUncached int

	// --- Projection ------------------------------------------------------

	// Calibrated is true when the projection is based on recorded scan history;
	// false means built-in priors were used (flagged in the rendered output).
	Calibrated bool
	// SampleRuns is the number of historical runs the calibration drew from.
	SampleRuns int
	// SampleMatched is true when the calibration sample was restricted to runs
	// of the same kind AND cartographer mode (the highest-fidelity sample);
	// false means the sample was broadened across kinds/modes for lack of
	// enough matching runs.
	SampleMatched bool
	// TokensPerUnit is the input+output tokens per finder unit used for the
	// projection (calibrated total-run tokens ÷ finder units, so it amortizes
	// triage/verify/cartographer spend; or a prior).
	TokensPerUnit float64
	// ThroughputTokPerSec is the calibrated token throughput used to project
	// wall time. Zero when no history is available (duration is then unknown).
	ThroughputTokPerSec float64

	// EstTokens / EstDuration are the point projections; the Low/High pair is a
	// coarse band around them. EstDuration is zero when throughput is unknown.
	EstTokens       int64
	EstTokensLow    int64
	EstTokensHigh   int64
	EstDuration     time.Duration
	EstDurationLow  time.Duration
	EstDurationHigh time.Duration
}

const (
	// estimateHistoryLimit bounds how many recent finished runs the calibration
	// reads. Recent runs reflect the current model/config; older ones add noise.
	estimateHistoryLimit = 50
	// estimateMinMatchedSamples is the minimum number of same-kind, same-
	// cartographer-mode runs required before the calibration prefers that
	// high-fidelity sample over the broader all-runs sample.
	estimateMinMatchedSamples = 3
	// defaultEstTokensPerUnit is the per-finder-unit token prior used ONLY when
	// no usable history exists. It is a deliberately rough placeholder — real
	// per-unit cost varies widely by model, repo, and caching — and is
	// superseded by calibration as soon as one finished run is recorded.
	defaultEstTokensPerUnit = 100_000.0
	// defaultEstCartoTokensPerPkg is the per-uncached-package token prior for
	// the cartographer pass, added to the prior projection when the pass is on.
	defaultEstCartoTokensPerPkg = 4_000.0
	// estimateLowFactor / estimateHighFactor form the coarse projection band.
	// Per-unit cost is high-variance, so the band is intentionally wide rather
	// than falsely precise.
	estimateLowFactor  = 0.5
	estimateHighFactor = 2.0
)

// EstimateScan computes a pre-scan Estimate for a run of the given kind without
// spending any tokens. For ScanTargeted, changedFiles drives the blast-radius
// target set exactly as Funnel.Targeted does; for any other kind the whole
// in-scope snapshot is the target set, as in Funnel.Sweep. The diff-intent unit
// is counted when kind == ScanTargeted and a ChangeContext is configured on the
// funnel (Options.ChangeContext), mirroring hypothesize.
func (f *Funnel) EstimateScan(ctx context.Context, kind store.ScanKind, changedFiles []string) (*Estimate, error) {
	snap, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}

	targets, err := f.estimateTargets(ctx, snap, kind, changedFiles)
	if err != nil {
		return nil, err
	}

	est := &Estimate{
		Kind:                kind,
		Commit:              snap.Commit,
		Files:               len(targets),
		CartographerEnabled: f.opts.Cartographer,
	}

	// Resolve the finder chunk size exactly as run() does: a small-context
	// model scales the chunk size down. Work on a copy so the funnel's options
	// are not mutated by a read-only estimate.
	opts := f.opts
	if window := f.clients.Finder.Capabilities().ContextWindow; window > 0 {
		opts = scaleFinderForContext(opts, window)
	}

	langs := ingest.DominantLanguages(snap)
	lenses := lensesByYield(f.lenses, langs)
	est.Lenses = len(lenses)

	chunks := chunkByLanguage(targets, opts.ChunkSize)
	est.Chunks = len(chunks)

	// Exact finder-unit count, computed with the production builders so the
	// estimate cannot drift from what hypothesize launches.
	chunkUnits := buildUnits(lenses, builtinStrategies(), chunks, nil)
	finderUnits := len(chunkUnits)

	if kind == store.ScanTargeted && f.opts.ChangeContext != nil {
		est.DiffIntent = true
		finderUnits++
	}

	seams := ingest.EnumerateSeams(snap)
	est.Seams = len(seams)
	finderUnits += len(seams)
	est.FinderUnits = finderUnits

	// Cartographer: count the packages spanned by targets and how many need a
	// fresh summary (cache miss/stale), mirroring cartograph's cache check.
	pkgMembers := packagesSpanned(targets)
	est.Packages = len(pkgMembers)
	if f.opts.Cartographer && len(pkgMembers) > 0 {
		if err := f.estimateCartographer(ctx, snap, targets, pkgMembers, est); err != nil {
			return nil, err
		}
	}

	// Project tokens and duration from recorded history (or priors).
	f.projectEstimate(ctx, est)
	return est, nil
}

// estimateTargets reproduces the target-selection of Sweep / Targeted so the
// estimate scopes over exactly the files the real run would audit.
func (f *Funnel) estimateTargets(ctx context.Context, snap *ingest.Snapshot, kind store.ScanKind, changedFiles []string) ([]string, error) {
	if kind != store.ScanTargeted {
		targets := make([]string, len(snap.Files))
		for i, file := range snap.Files {
			targets[i] = file.Path
		}
		return targets, nil
	}
	radius, err := f.repo.BlastRadius(ctx, snap, changedFiles)
	if err != nil {
		return nil, fmt.Errorf("funnel: blast radius: %w", err)
	}
	inScope := make(map[string]bool, len(snap.Files))
	for _, file := range snap.Files {
		inScope[file.Path] = true
	}
	var targets []string
	for _, p := range radius {
		if inScope[p] {
			targets = append(targets, p)
		}
	}
	sort.Strings(targets)
	return targets, nil
}

// estimateCartographer fills the cartographer fields by replaying cartograph's
// fingerprint + cache lookup without generating any summaries.
func (f *Funnel) estimateCartographer(ctx context.Context, snap *ingest.Snapshot, targets []string, pkgMembers map[string][]string, est *Estimate) error {
	fps, err := f.repo.Fingerprints(ctx, snap)
	if err != nil {
		return fmt.Errorf("funnel: fingerprints: %w", err)
	}
	pkgFingerprints := make(map[string]string, len(pkgMembers))
	for pkg, members := range pkgMembers {
		pkgFingerprints[pkg] = packageFingerprint(pkg, members, fps)
	}
	est.CartographerPackages = len(pkgFingerprints)
	// A store read failure degrades to "all uncached" — the conservative
	// (higher-cost) estimate, never a crash.
	cached, err := f.store.GetPackageSummaries(ctx, sortedKeys(pkgFingerprints))
	if err != nil {
		est.CartographerUncached = len(pkgFingerprints)
		return nil
	}
	uncached := 0
	for pkg, fp := range pkgFingerprints {
		if row, ok := cached[pkg]; ok && row.Fingerprint == fp && row.Summary != "" {
			continue
		}
		uncached++
	}
	est.CartographerUncached = uncached
	return nil
}

// projectEstimate fills the projection fields of est from recorded scan history,
// or from built-in priors when no usable history exists.
func (f *Funnel) projectEstimate(ctx context.Context, est *Estimate) {
	tokensPerUnit, throughput, sample, matched := f.calibrate(ctx, est.Kind, est.CartographerEnabled)

	if sample > 0 {
		est.Calibrated = true
		est.SampleRuns = sample
		est.SampleMatched = matched
		est.TokensPerUnit = tokensPerUnit
		est.ThroughputTokPerSec = throughput
		est.EstTokens = int64(math.Round(float64(est.FinderUnits) * tokensPerUnit))
	} else {
		// No history: priors. Model the finder stage per-unit and add the
		// cartographer's marginal cost (which calibration would otherwise have
		// amortized into the per-unit figure).
		est.TokensPerUnit = defaultEstTokensPerUnit
		tokens := float64(est.FinderUnits) * defaultEstTokensPerUnit
		if est.CartographerEnabled {
			tokens += float64(est.CartographerUncached) * defaultEstCartoTokensPerPkg
		}
		est.EstTokens = int64(math.Round(tokens))
	}

	est.EstTokensLow = int64(math.Round(float64(est.EstTokens) * estimateLowFactor))
	est.EstTokensHigh = int64(math.Round(float64(est.EstTokens) * estimateHighFactor))

	if est.ThroughputTokPerSec > 0 {
		secs := float64(est.EstTokens) / est.ThroughputTokPerSec
		est.EstDuration = time.Duration(secs * float64(time.Second))
		est.EstDurationLow = time.Duration(secs * estimateLowFactor * float64(time.Second))
		est.EstDurationHigh = time.Duration(secs * estimateHighFactor * float64(time.Second))
	}
}

// calibrate derives per-unit token cost and token throughput from recent
// finished runs. It prefers a high-fidelity sample (same kind AND cartographer
// mode, at least estimateMinMatchedSamples runs); failing that it falls back to
// every usable run. A run is usable when it launched finder units, spent tokens,
// and has a positive wall duration. Returns sample=0 when no run qualifies.
func (f *Funnel) calibrate(ctx context.Context, kind store.ScanKind, cartoOn bool) (tokensPerUnit, throughput float64, sample int, matched bool) {
	runs, err := f.store.RunMetrics(ctx, estimateHistoryLimit)
	if err != nil || len(runs) == 0 {
		return 0, 0, 0, false
	}

	var usable, matchedRuns []store.RunMetric
	for _, r := range runs {
		if r.FinderRuns <= 0 || r.TotalTokens() <= 0 {
			continue
		}
		if !r.FinishedAt.After(r.StartedAt) {
			continue
		}
		usable = append(usable, r)
		if r.Kind == kind && r.CartographerEnabled == cartoOn {
			matchedRuns = append(matchedRuns, r)
		}
	}

	chosen := usable
	matched = false
	if len(matchedRuns) >= estimateMinMatchedSamples {
		chosen = matchedRuns
		matched = true
	}
	if len(chosen) == 0 {
		return 0, 0, 0, false
	}

	var sumTokens, sumUnits int64
	var sumWallSec float64
	for _, r := range chosen {
		sumTokens += r.TotalTokens()
		sumUnits += int64(r.FinderRuns)
		sumWallSec += r.FinishedAt.Sub(r.StartedAt).Seconds()
	}
	if sumUnits > 0 {
		tokensPerUnit = float64(sumTokens) / float64(sumUnits)
	}
	if sumWallSec > 0 {
		throughput = float64(sumTokens) / sumWallSec
	}
	return tokensPerUnit, throughput, len(chosen), matched
}
