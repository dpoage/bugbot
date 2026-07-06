// Package funnel implements Bugbot's precision-first detection pipeline: the
// staged core that turns a repository snapshot into a small set of
// adversarially-verified findings.
//
// The pipeline is deliberately tuned for precision over recall (see
// ARCHITECTURE.md): it is better to surface three real bugs than ten probable
// ones. Finder agents WANT to find bugs; the verifier's job is to kill false
// positives. Known-clean code must produce zero findings.
//
// # Stages
//
//	A. Hypothesize — parallel finder agents, one per lens, each auditing the
//	   target files (chunked) for one class of defect via RunJSON.
//	B. Triage      — drop low-confidence candidates, dedup by store fingerprint,
//	   drop suppressed fingerprints, drop candidates outside the snapshot.
//	C. Verify      — per surviving candidate, run N adversarial refuter agents;
//	   a majority "refuted" verdict kills the candidate. Survivors are Tier 2.
//	D. Persist     — upsert survivors (status open, tier 2, anchored to the
//	   commit + file content hash) inside a scan run, record per-role spend, and
//	   return a ranked Result.
//
// # Budget degradation
//
// Options.TokenBudget (0, or any value <= 0, = UNLIMITED) bounds the whole run. The funnel tracks
// cumulative spend and degrades gracefully rather than truncating silently:
// past 70% it runs only the two highest-yield lenses and a single refuter; past
// 100% it launches no new agents and finishes with what is already verified.
// Everything skipped is surfaced on the Result.
package funnel

import (
	"fmt"
	"sync"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// Defaults for Options. Exported so callers (CLI flags) can present them.
const (
	// DefaultRefuters is the number of adversarial refuter agents run per
	// candidate. Three gives a meaningful majority vote without tripling cost
	// versus one.
	DefaultRefuters = 3
	// DefaultCacheReadBudgetWeight is the fraction at which cache-read input
	// tokens count against the token budget when Options leaves it unset. ~0.1
	// approximates Anthropic's cache-read discount; a conservative, cost-favoring
	// default that undercounts slightly on milder-discount providers.
	DefaultCacheReadBudgetWeight = 0.1
	// DefaultMaxParallel bounds concurrently-running agents across the run.
	DefaultMaxParallel = 4
	// DefaultChunkSize is the number of target files handed to a single finder
	// invocation. Chunking keeps each finder's context focused and lets large
	// repos parallelize within a lens.
	//
	// Sized so a finder can plausibly read every file in a chunk and still emit
	// JSON within DefaultMaxIterations (20). Calibration found 30 files structurally
	// impossible inside 20 turns: the agent ran out of iterations mid-read on a
	// 16-file chunk. With one read per file plus a few cross-reference lookups, a
	// budget of ~8 files leaves ~12 turns of headroom before forced finalization,
	// which keeps each finder coherent rather than perpetually truncated. Forced
	// finalization (see agent.RunJSON) is the safety net; the smaller chunk is the
	// structural fix so finalization is the exception, not the norm.
	DefaultChunkSize = 8

	// DefaultFinderHistoryTokens is the per-finder history-compaction threshold
	// applied when a caller asks for compaction but leaves the exact threshold
	// unset. Once a finder's estimated message history exceeds this many tokens,
	// older tool-result content is compacted to stubs once per crossing (see
	// agent.Limits.HistoryTokenBudget), preserving the task, the reasoning chain,
	// the most recent results, and tool-call pairing.
	//
	// IMPORTANT — compaction is OFF by default. The bugbot-3nf offline measurement
	// showed that under a strong ~0.1x prompt cache (the cache weight the budget
	// uses), mutating the message prefix to prune old results forfeits cache hits
	// worth MORE than the bytes reclaimed over the few remaining turns: raw input
	// tokens drop ~10-37% but CACHE-WEIGHTED cost rises ~1-43%. So compaction is a
	// raw-token / context-window-pressure / weak-cache lever, not the cache-cost
	// win the bead targeted; the cache-safe finder default is instead tighter
	// per-read caps (DefaultFinderReadLines/Bytes). Compaction stays available for
	// callers on providers with little or no prompt-cache discount, where the
	// raw-token reduction IS the real-dollar reduction. This threshold is the
	// value used when such a caller sets budgets.finder_history_tokens to 0 while
	// having explicitly opted in via Options; a negative value disables it.
	DefaultFinderHistoryTokens int64 = 60_000

	// DefaultFinderReadLines / DefaultFinderReadBytes are the per-read_file caps a
	// finder uses, tighter than the agent package defaults (2000 lines / 256 KB).
	//
	// This is the PRIMARY cache-safe lever for the finder token-burn finding
	// (bugbot-3nf). A finder re-sends its whole growing history every turn, so a
	// fat read_file result is re-billed on every later turn — quadratic in turns.
	// Tightening the cap shrinks each result at the SOURCE, before it ever enters
	// the conversation, so it never mutates an earlier message and never forfeits
	// a prompt-cache prefix. The offline measurement (internal/eval, the
	// bugbot-3nf harness) shows this cuts CACHE-WEIGHTED input ~55-77% on a
	// SYNTHETIC runaway profile (every read assumed cap-saturated; a BEST-CASE UPPER
	// BOUND, not a corpus measurement — the recorded corpus never exercises this
	// lever because its files are well below the caps). Threshold history compaction
	// — which mutates the prefix — REDUCES raw tokens but INCREASES cache-weighted
	// cost under the same ~0.1x cache, and is therefore left opt-in/off (see
	// DefaultFinderHistoryTokens).
	//
	// 800 lines / 96 KB comfortably covers a focused source file (most files under
	// analysis are far smaller); larger files are line-windowed with offset/limit,
	// and the truncation note tells the model to page if it genuinely needs more.
	//
	// NOTE — the "~55-77%" figure cited in code comments and the offline harness
	// (internal/eval/compact_measure_test.go) is a SYNTHETIC BEST-CASE UPPER BOUND,
	// not a corpus measurement. It assumes every read_file call saturates the cap
	// (the savings ARE the truncated content), and the recorded eval corpus never
	// exercises this lever because its files are well below the caps. Real savings
	// depend on whether files in the target repo actually exceed the cap.
	DefaultFinderReadLines = 800
	DefaultFinderReadBytes = 96 * 1024

	// DefaultMaxOutputTokens caps each finder/verifier completion's VISIBLE output.
	// Reasoning models (e.g. MiniMax M3) spend most of their completion allowance
	// inside <think> blocks; without an explicit, generous cap the provider default
	// can be exhausted before any JSON is emitted, yielding "empty model output".
	// 8k visible tokens comfortably fits a candidate list or a refutation verdict
	// with reasoning headroom to spare.
	DefaultMaxOutputTokens = 8192

	// softBudgetFraction is the fraction of TokenBudget past which the run
	// degrades to the highest-yield lenses and a single refuter.
	softBudgetNumer = 7
	softBudgetDenom = 10
	// degradedLensCount is how many top-yield lenses survive degradation.
	degradedLensCount = 2
	// degradedRefuters is the refuter count under degradation.
	degradedRefuters = 1

	// DefaultFinderBudgetShare is the fraction of a run's TokenBudget the finder
	// stage may consume when Options leaves FinderBudgetShare unset. The
	// remainder is RESERVED for downstream verification (and, transitively,
	// reproduction, which can only run on verified Tier-2 survivors). Without a
	// reservation the finder stage — which launches first and in bulk — routinely
	// drains the entire shared budget pool before any candidate reaches the
	// verifier, orphaning every finding as Tier-3 and starving reproduction of
	// input (bugbot-3lt live evidence: 769 finder units hard-skipped, 0 Tier-1
	// promotions across 15 runs). 0.7 keeps the breadth-heavy finder stage the
	// majority shareholder while guaranteeing the verifier a meaningful slice it
	// cannot be starved out of. Tune via budgets.finder_budget_share.
	DefaultFinderBudgetShare = 0.7

	// DefaultTokenClaim is the per-task token claim a finder or verifier run is
	// capped at when Options leaves the role's claim size unset. It bounds any
	// single agent run's per-run TokenBudget (see budgetState.runnerLimitsForPool)
	// so one breadth-heavy run cannot be granted a whole stage's reserve at
	// launch. The shared pool is charged only for tokens actually spent, so the
	// unspent part of a claim stays available to sibling runs. 1M matches
	// agent.DefaultTokenBudget so a claimed run's cap equals the historical
	// default when the sub-pool has at least one claim's worth of headroom. Tune
	// via budgets.finder_token_claim / budgets.verifier_token_claim.
	DefaultTokenClaim int64 = 1_000_000

	// DefaultArbiterTokenClaim is the per-task token claim for the
	// arbiter, which runs only on split verdicts and does materially more
	// work per run than a single refuter (bugbot-mi5.17). It is
	// deliberately ~5x the default refuter claim so a single arbiter
	// run can drive the split to ground without being clipped to a
	// refuter's per-run budget. Splits are rare (a handful per few
	// hundred findings), so the higher per-task cap is acceptable.
	DefaultArbiterTokenClaim int64 = 5_000_000

	// DefaultArbiterMaxIterations is the agent-loop iteration cap for an
	// arbiter run. The arbiter is AGENTIC: it issues follow-up tool calls
	// to ground the decisive claim before voting, so the loop must allow
	// several rounds. 50 covers realistic grounding without making a
	// single stuck arbiter run away with the budget. Overridable through
	// StageLimits.ArbiterLimits.MaxIterations.
	DefaultArbiterMaxIterations = 50

	// DefaultCartographerMaxFiles bounds the number of member files in a
	// package fed to the cartographer's per-package summary completion. The
	// summary is cached by content fingerprint and regenerated only when the
	// package changes, so it is worth feeding a generous view of a large
	// package once; 64 covers all but the largest packages in full.
	DefaultCartographerMaxFiles = 64
	// DefaultCartographerHeadLines caps the lines read from each member file
	DefaultCartographerHeadLines = 400
	// DefaultCartographerInputBytes caps the total bytes of member-file
	// content fed to a single summary completion — the hard ceiling that
	// binds first for a large package. Reasoning models have large context
	// windows and the summary is cached, so a generous 128 KB buys a
	// well-grounded summary at a one-time-per-change cost.
	DefaultCartographerInputBytes = 128 * 1024
)

// cartographyInjectMaxPkgs and cartographyInjectMaxBytes bound the size of
// the REPO CONTEXT block injected into a finder task. The bound is per-unit
// (not per-run) so a single chunk with many distinct packages cannot bloat
// one finder's task. 12 packages × ~500 bytes per summary line fits inside
// the byte cap; overflow drops the tail and notes the truncation so the
// agent knows the context is partial.
const (
	cartographyInjectMaxPkgs  = 12
	cartographyInjectMaxBytes = 6 * 1024
)

// RoleClients holds the per-role LLM clients the funnel drives. Tests inject
// fakes; the CLI builds these via config.ResolveRole. Finder and Verifier are
// required (New rejects nil). Cartographer and Arbiter are optional: when nil,
// the package-summary pass reuses Finder and the split-verdict arbiter reuses
// Verifier respectively. Reproducer is not used by this stage (Tier 1
// reproduction is a later stage) and is intentionally absent.
type RoleClients struct {
	Finder   llm.Client
	Verifier llm.Client
	// Cartographer is the client for the package-summary pass (optional; nil
	// reuses Finder). Configured via the [roles.cartographer] mapping.
	Cartographer llm.Client
	// Arbiter is the client for the split-verdict arbiter (optional; nil
	// reuses Verifier). Configured via the [roles.arbiter] mapping. The
	// arbiter only runs on the rare SPLIT refuter panel (~5%), so pointing
	// it at a stronger model sharpens the toughest calls without paying the
	// cost on every candidate.
	Arbiter llm.Client
}

// roleFinder / roleVerifier / roleCartographer / roleArbiter are the
// spend-ledger role tags wired into the per-role recorder clients (see run.go).
// The recorder routes finder AND cartographer spend to the finder sub-pool and
// everything else (including arbiter) to the verify sub-pool under a downstream
// budget reservation, so these MUST match the strings passed to
// llm.WithRecorder.
const (
	roleFinder       = "finder"
	roleVerifier     = "verifier"
	roleCartographer = "cartographer"
	roleArbiter      = "arbiter"
)

// SandboxOpts bundles the sandbox-execution knobs that gate and bound the
// sandbox_exec tool offered to refuter agents. The zero value means the
// feature is disabled.
type SandboxOpts struct {
	// Sandbox is the sandbox backend to use. Nil means the feature is
	// unconditionally unavailable regardless of other fields.
	Sandbox sandbox.Sandbox
	// Enabled gates the feature: if false (default), no refuter receives the
	// tool.
	Enabled bool
	// MinSeverity is the minimum candidate severity that qualifies for the
	// tool. Candidates below this threshold use only rhetorical reasoning.
	// Valid values: "critical", "high", "medium", "low". Empty defaults to
	// "high".
	MinSeverity string
	// MaxExecs is the per-candidate execution budget. Zero defaults to 3.
	MaxExecs int
	// DepStrategy selects how external module dependencies are made available to
	// a refuter's network-none probe (see sandbox.DepStrategy). Empty/"off"
	// keeps the current behavior; "host"/"fetch" mount a module cache. Vendored
	// repos are always detected. The one-time online prefetch for "fetch" runs
	// once, lazily, the first time a sandbox tool is built.
	DepStrategy sandbox.DepStrategy
	// SetupCmds are operator-supplied commands run inside the sandbox BEFORE the
	// main command AND BEFORE per-ecosystem offline installs. See
	// config.Sandbox.SetupCmds for the full contract and ordering rationale.
	SetupCmds [][]string
	// LocalMounts are read-only host directories bind-mounted into the sandbox,
	// independent of dep_strategy. See config.Sandbox.LocalMounts.
	LocalMounts []sandbox.ROMount
}

// ChangeContext carries commit-scoped context for a targeted (commit-triggered)
// funnel run. It is optional: nil on sweep runs and targeted runs without a
// specific commit window. When non-nil, the diff-intent lens uses it to look
// for gaps between the commit's stated intent and its implementation.
//
// ChangeContext is only meaningful on ScanTargeted runs; it is ignored on
// ScanOneshot (Sweep) and ScanSweep runs even if set.
type ChangeContext struct {
	// FromCommit and ToCommit are the inclusive range of the change (e.g. the
	// parent and the new HEAD). Both must be non-empty for the lens to fire.
	FromCommit string
	// ToCommit is the tip commit of the change window.
	ToCommit string
	// Message is the full commit message of ToCommit (from CommitMessage).
	// Truncated at 4KB in the task prompt.
	Message string
	// Diff is the raw unified diff between FromCommit and ToCommit
	// (from UnifiedDiff). May be nil if no diff is available.
	Diff []byte
	// ChangedFiles is the list of repo-relative paths modified by the change
	// (from ChangedFiles + ChangedPaths). Used in the task for context.
	ChangedFiles []string
	// BlastFiles is intentionally absent: the blast-radius dependent list is
	// derived inside hypothesize from the targets already expanded by Targeted
	// (run.go calls BlastRadius before hypothesize). Callers must not set it.
}

// Funnel runs the staged detection pipeline against a repository. It is
// constructed once per process (or per daemon) and is safe for sequential reuse
// across scans; a single scan parallelizes internally. Call [Funnel.Close] when
// done to shut down any language servers the code-navigation tools spawned.
type Funnel struct {
	clients RoleClients
	store   *store.Store
	repo    *ingest.Repo
	opts    Options
	lenses  []Lens
	// slots is the funnel-wide agent concurrency pool. Every LLM agent —
	// finder unit (low priority) or verifier candidate panel (high priority)
	// — holds one slot for its entire duration. Options.MaxParallel means
	// "concurrent agents across all roles".
	slots *slotPool

	// navOnce lazily builds the shared code-navigation tool bundle (and its
	// language-server manager) on first use, so funnels that never run agents
	// spawn nothing.
	navOnce sync.Once
	nav     *agent.CodeNav
	navErr  error
	// ownsNav is true when the funnel constructed nav itself (Options.CodeNav was
	// nil). A borrowed (injected) nav must NOT be closed by the funnel.
	ownsNav bool

	// deps is the resolved dependency strategy for the sandbox_exec tool,
	// computed in New from SandboxOpts. depPrefetchOnce ensures the one-time
	// online prefetch (DepStrategyFetch) runs at most once across all candidates.
	deps            sandbox.Resolution
	depPrefetchOnce sync.Once
	depPrefetchErr  error

	// depRoots is the read-only dep-source root set (GOROOT/src + Go module
	// cache) captured once at New. It backs the arbiter's broader read_file
	// reach so a split arbiter can read the source of a cited stdlib/third-party
	// symbol; refuters stay repo-rooted (bugbot-mi5.17/.18). Empty on a host
	// without the relevant toolchain — the arbiter then behaves as before.
	depRoots *agent.DepSourceRoots

	// hasGoDepSource gates the stdlib/dep source-reading obligation in refuter
	// and arbiter prompts. It is true only when BOTH conditions hold:
	//   (1) dep-source roots are available on the host (depRoots.Len() > 0), AND
	//   (2) the repo's dominant languages include Go.
	// Condition (2) is required because dep-source reach is Go-only today
	// (internal/agent/dep_source.go): on a Python/C++/JS repo the host may have
	// a Go toolchain installed, making depRoots non-empty, but the verifier's
	// tools cannot reach Python site-packages or C++ system headers. Hardcoding
	// GOROOT/GOMODCACHE text in prompts for such repos is a fabrication trigger.
	// Written only from run.go (once, before verify goroutines start) and
	// VerifyFinding (the daemon's strictly sequential re-verify loop on a
	// dedicated Funnel); never written concurrently with reads. Always assign
	// goDepSourceFor's result — a conditional set would latch a stale true
	// across mixed-language re-verifications.
	hasGoDepSource bool
}

// goDepSourceFor reports whether refuter and arbiter prompts should include
// the unconditional MUST-read-source obligation for stdlib/dep behavior, given
// the languages in scope (the repo's dominant languages for a scan; the
// finding's file language for a re-verification). True only when BOTH hold:
// dep-source roots exist on the host AND Go is among langs (dep-source reach
// is Go-only today). Pure: callers assign the result to f.hasGoDepSource.
func (f *Funnel) goDepSourceFor(langs []ingest.Language) bool {
	if f.depRoots.Len() == 0 {
		return false // no roots wired
	}
	for _, l := range langs {
		if l == ingest.LangGo {
			return true
		}
	}
	return false
}

// New constructs a Funnel. clients supplies the finder/verifier LLM clients,
// st is the state store, repo is the opened target repository, and opts tunes
// the run. clients.Finder and clients.Verifier must be non-nil; st and repo
// must be non-nil.
func New(clients RoleClients, st *store.Store, repo *ingest.Repo, opts Options) (*Funnel, error) {
	if clients.Finder == nil {
		return nil, fmt.Errorf("funnel: nil finder client")
	}
	if clients.Verifier == nil {
		return nil, fmt.Errorf("funnel: nil verifier client")
	}
	if st == nil {
		return nil, fmt.Errorf("funnel: nil store")
	}
	if repo == nil {
		return nil, fmt.Errorf("funnel: nil repo")
	}
	resolved := opts.resolve()

	// Resolve the dependency strategy for the sandbox_exec tool up front so
	// every refuter probe carries the same module-cache mount/env. Only relevant
	// when the sandbox feature is enabled; vendored/off repos resolve to empty.
	var deps sandbox.Resolution
	if resolved.SandboxOpts.Enabled && resolved.SandboxOpts.Sandbox != nil {
		d, err := sandbox.ResolveDeps(repo.Root(), sandbox.DepOptions{
			Strategy:     resolved.SandboxOpts.DepStrategy,
			FetchSandbox: resolved.SandboxOpts.Sandbox,
			LocalMounts:  resolved.SandboxOpts.LocalMounts,
		})
		if err != nil {
			return nil, fmt.Errorf("funnel: resolve dependency strategy: %w", err)
		}
		// Prepend operator setup_cmds BEFORE ecosystem-derived setup commands.
		// See config.Sandbox.SetupCmds for the ordering rationale.
		if len(resolved.SandboxOpts.SetupCmds) > 0 {
			d.SetupCmds = append(resolved.SandboxOpts.SetupCmds, d.SetupCmds...)
		}
		deps = d
	}

	f := &Funnel{
		clients:  clients,
		store:    st,
		repo:     repo,
		opts:     resolved,
		lenses:   selectLenses(resolved.Discovery.Lenses),
		deps:     deps,
		depRoots: agent.NewDepSourceRoots(),
		slots:    newSlotPool(resolved.Limits.MaxParallel),
	}
	if resolved.CodeNav != nil {
		// Daemon-injected: borrow, never own.
		f.nav = resolved.CodeNav
		f.ownsNav = false
	}
	return f, nil
}

// codeNav returns the shared code-navigation tool bundle, creating it (and its
// lazy language-server manager — no processes are spawned until a tool's first
// query) on first call.
func (f *Funnel) codeNav() (*agent.CodeNav, error) {
	// Fast path: a daemon-injected nav (Options.CodeNav) is set once in New and
	// never mutated, so reading it needs no synchronization. We must NOT read
	// f.nav here: in the lazy (non-injected) case f.nav is written inside
	// navOnce below, and an unsynchronized read of it would race that write.
	if f.opts.CodeNav != nil {
		return f.opts.CodeNav, nil
	}
	f.navOnce.Do(func() {
		f.nav, f.navErr = agent.NewCodeNav(f.repo.Root())
		f.ownsNav = true
	})
	return f.nav, f.navErr
}

// Close shuts down any language servers the code-navigation tools spawned.
// Safe to call multiple times, on a funnel that never ran, and on a nil receiver
// (so deferred Close calls on a partially-initialised funnel never panic).
func (f *Funnel) Close() error {
	if f == nil {
		return nil
	}
	// Synchronize with codeNav() so a Close racing the lazy init still sees
	// the bundle.
	f.navOnce.Do(func() {})
	if f.nav == nil || !f.ownsNav {
		// Either no nav was ever created, or it is daemon-owned: never close it
		// here. The daemon closes its shared CodeNav exactly once on exit.
		return nil
	}
	return f.nav.Close()
}
