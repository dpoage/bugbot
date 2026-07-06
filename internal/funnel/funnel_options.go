// funnel_options.go holds the Options cluster extracted from funnel.go for readability.
// Pure code motion: no logic changes.
package funnel

import (
	"context"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/progress"
)

// BudgetConfig groups token-budget and per-role claim knobs. The zero value
// resolves to sensible defaults via resolve(). Consumers always see the
// resolved copy built in Options.resolve().
type BudgetConfig struct {
	// TokenBudget bounds cumulative input+output tokens for the whole run. Zero
	// or negative means unlimited (the funnel never degrades or stops).
	TokenBudget int64
	// CacheReadBudgetWeight discounts cache-read input tokens against
	// TokenBudget (0..1). Zero resolves to DefaultCacheReadBudgetWeight (~0.1);
	// set to 1.0 for raw-token accounting.
	CacheReadBudgetWeight float64
	// FinderBudgetShare is the fraction of TokenBudget (0..1) the finder stage
	// may consume; the remainder is RESERVED for downstream verification so the
	// breadth-heavy finder stage cannot drain the whole pool and orphan every
	// candidate before verification (see DefaultFinderBudgetShare). Zero resolves
	// to DefaultFinderBudgetShare (0.7). 1.0 is a legitimate value meaning the
	// finder may use the entire budget with no downstream reservation
	// (single-pool behavior). Ignored when TokenBudget is unlimited.
	FinderBudgetShare float64
	// FinderTokenClaim / VerifierTokenClaim are the per-task token claims for the
	// claimant budget system. Each finder/refuter/arbiter run is capped at its
	// role's claim so a single breadth-heavy run cannot be granted a whole
	// stage's reserve at launch. Zero resolves to DefaultTokenClaim (1M).
	// A negative value removes the per-task cap (each run may use its sub-pool's
	// full remainder). Ignored when TokenBudget is unlimited.
	FinderTokenClaim   int64
	VerifierTokenClaim int64
	// ArbiterTokenClaim is the per-task token claim for the split-verdict
	// arbiter. Zero resolves to DefaultArbiterTokenClaim (~5x VerifierTokenClaim)
	// so the arbiter can drive a split to ground without being clipped to a
	// refuter's per-run budget; splits are rare so the higher cap is marginal
	// (bugbot-mi5.17). A negative value removes the per-task cap. Ignored when
	// TokenBudget is unlimited.
	ArbiterTokenClaim int64
}

// resolve fills in BudgetConfig defaults.
func (b BudgetConfig) resolve() BudgetConfig {
	if b.CacheReadBudgetWeight == 0 {
		b.CacheReadBudgetWeight = DefaultCacheReadBudgetWeight
	}
	if b.FinderBudgetShare <= 0 {
		b.FinderBudgetShare = DefaultFinderBudgetShare
	}
	if b.FinderTokenClaim == 0 {
		b.FinderTokenClaim = DefaultTokenClaim
	}
	if b.VerifierTokenClaim == 0 {
		b.VerifierTokenClaim = DefaultTokenClaim
	}
	if b.ArbiterTokenClaim == 0 {
		b.ArbiterTokenClaim = DefaultArbiterTokenClaim
	}
	return b
}

// StageLimits groups per-stage parallelism and per-run agent limits.
// The zero value resolves to sensible defaults.
type StageLimits struct {
	// Refuters is the number of adversarial refuter agents per candidate.
	// Zero uses DefaultRefuters.
	Refuters int
	// MaxParallel bounds concurrently-running agents across all roles.
	// Zero uses DefaultMaxParallel; negative is treated as 1.
	MaxParallel int
	// ChunkSize is the number of files per finder invocation.
	// Zero uses DefaultChunkSize.
	ChunkSize int
	// FinderLimits / VerifierLimits bound each individual agent run.
	FinderLimits   agent.Limits
	VerifierLimits agent.Limits
	// ArbiterLimits bounds each individual arbiter run. The arbiter is
	// invoked only on split verdicts (bugbot-mi5.17); its task is
	// strictly harder than a refuter's, so the default gives it
	// materially more MaxIterations and a higher per-run token budget
	// than VerifierLimits. Zero fields defer to the per-stage default
	// ([DefaultArbiterLimits]); a negative value disables that cap. A
	// fully-zero ArbiterLimits is the safe opt-in: the arbiter always
	// runs with a budget, never unbounded by default.
	ArbiterLimits agent.Limits
	// FinderHistoryTokens controls opt-in finder history compaction (OFF by
	// default — see DefaultFinderHistoryTokens). Zero and negative leave it
	// disabled. A positive value opts in at that token threshold.
	// Folded into FinderLimits.HistoryTokenBudget at resolve time.
	FinderHistoryTokens int64
	// FinderReadLines / FinderReadBytes tighten the finder's per-read_file caps.
	// Zero uses DefaultFinderReadLines / DefaultFinderReadBytes. Negative
	// restores the looser agent-package defaults.
	FinderReadLines int
	FinderReadBytes int
}

// resolve fills in StageLimits defaults.
func (l StageLimits) resolve() StageLimits {
	if l.Refuters <= 0 {
		l.Refuters = DefaultRefuters
	}
	if l.MaxParallel == 0 {
		l.MaxParallel = DefaultMaxParallel
	}
	if l.MaxParallel < 0 {
		l.MaxParallel = 1
	}
	if l.ChunkSize <= 0 {
		l.ChunkSize = DefaultChunkSize
	}
	if l.FinderHistoryTokens > 0 {
		l.FinderLimits.HistoryTokenBudget = l.FinderHistoryTokens
	} else {
		l.FinderLimits.HistoryTokenBudget = 0
	}
	// The arbiter is agentic and needs more model turns than a refuter to drive
	// a split to ground, so a zero MaxIterations resolves to the larger
	// DefaultArbiterMaxIterations here (the runner would otherwise apply the
	// 20-turn refuter default). A negative value is preserved (cap disabled).
	// The per-run token allowance is governed by arbiterClaim in
	// arbiterRunnerLimits, so ArbiterLimits.TokenBudget is left untouched.
	if l.ArbiterLimits.MaxIterations == 0 {
		l.ArbiterLimits.MaxIterations = DefaultArbiterMaxIterations
	}
	return l
}

// FeatureFlags groups optional feature toggles. All default to off/false.
type FeatureFlags struct {
	// Cartographer enables the per-package summary pass (bugbot-mi5.7).
	Cartographer bool
	// StatusNotes enables the status_note tool for finder and verifier agents.
	StatusNotes bool
	// ToolComplaints enables the report_tool_issue tool for finder and verifier
	// agents, letting them flag a broken harness tool with a severity. Off by
	// default; the always-on objective tool-health sink is unaffected by this flag.
	ToolComplaints bool
	// DisableHeatOrdering suppresses churn-heat reordering in Sweep.
	// Set to true to restore alphabetical ordering (e.g. for deterministic testing).
	DisableHeatOrdering bool
}

// DiscoveryConfig groups snapshot-scoping and change-context knobs.
type DiscoveryConfig struct {
	// Filter scopes the snapshot to the configured include/exclude globs.
	Filter ingest.ScanFilter
	// Lenses, when non-empty, restricts the finder stage to the named built-in lenses.
	Lenses []string
	// ChangeContext, when non-nil, provides commit-scoped information for a
	// targeted run. Only honoured on ScanTargeted runs.
	ChangeContext *ChangeContext
}

// Options configures a single funnel run. The zero value is valid: every field
// resolves to a sensible default.
//
// Budget, Limits, Features, and Discovery are orthogonal single-concern groups.
// The remaining fields (Progress, Repro, CodeNav, TranscriptDir, SandboxOpts)
// are wiring/IO concerns kept at the top level.
type Options struct {
	// Budget groups token-budget and per-role claim knobs.
	Budget BudgetConfig
	// Limits groups per-stage parallelism and per-run agent limits.
	Limits StageLimits
	// Features groups optional feature toggles.
	Features FeatureFlags
	// Discovery groups snapshot-scoping and change-context knobs.
	Discovery DiscoveryConfig
	// SandboxOpts configures the sandbox_exec tool offered to refuter agents.
	SandboxOpts SandboxOpts
	// Progress, when non-nil, receives activity events as the run proceeds.
	Progress progress.EventSink
	// Repro, when non-nil, is invoked in-run for each Tier-2 finding that
	// survives verification. Must be safe for concurrent use.
	Repro func(ctx context.Context, scanRunID string, finding domain.Finding) error
	// CodeNav, when non-nil, is a pre-constructed code-navigation bundle that the
	// funnel BORROWS rather than owns. Nil causes the funnel to construct its own.
	CodeNav *agent.CodeNav
	// TranscriptDir, when non-empty, makes every agent auto-save its transcript.
	TranscriptDir string
}

// resolve fills in defaults without mutating the caller's Options.
func (o Options) resolve() Options {
	o.Budget = o.Budget.resolve()
	o.Limits = o.Limits.resolve()
	return o
}

// finderReadCaps resolves the per-read_file caps for finder agents from Options,
// substituting the funnel finder defaults for unset fields and honoring a
// negative request as "use the looser agent-package defaults".
func (o Options) finderReadCaps() agent.ReadCaps {
	caps := agent.ReadCaps{}
	switch {
	case o.Limits.FinderReadLines < 0:
		caps.MaxLines = 0
	case o.Limits.FinderReadLines == 0:
		caps.MaxLines = DefaultFinderReadLines
	default:
		caps.MaxLines = o.Limits.FinderReadLines
	}
	switch {
	case o.Limits.FinderReadBytes < 0:
		caps.MaxBytes = 0
	case o.Limits.FinderReadBytes == 0:
		caps.MaxBytes = DefaultFinderReadBytes
	default:
		caps.MaxBytes = o.Limits.FinderReadBytes
	}
	return caps
}
