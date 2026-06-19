package funnel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/progress"
)

// emitAgentFinished emits an agent-finished progress event with the run's token
// usage, wall-clock duration, and error (if any). outcome may be nil when the
// run failed before producing one; tokens then default to zero.
func emitAgentFinished(sink progress.Sink, role, label string, outcome *agent.Outcome, start time.Time, err error) {
	if sink == nil {
		return
	}
	var tokens int64
	if outcome != nil {
		tokens = outcome.Usage.InputTokens + outcome.Usage.OutputTokens
	}
	ev := progress.Event{
		Kind: progress.KindAgentFinished, Role: role, Label: label,
		Tokens: tokens, Duration: time.Since(start),
	}
	if err != nil {
		ev.Err = err.Error()
	}
	progress.Emit(sink, ev)
}

// emitFinderAgentFinished emits a KindAgentFinished event for a finder unit,
// carrying the candidate count so live status counters can tick per-unit during
// the hypothesize stage. candidates is non-zero only on a successful (finderOK)
// run; callers pass zero for error/parse-fail/budget-stop paths.
func emitFinderAgentFinished(sink progress.Sink, label string, outcome *agent.Outcome, start time.Time, err error, candidates int) {
	if sink == nil {
		return
	}
	var tokens int64
	if outcome != nil {
		tokens = outcome.Usage.InputTokens + outcome.Usage.OutputTokens
	}
	ev := progress.Event{
		Kind:       progress.KindAgentFinished,
		Role:       progress.RoleFinder,
		Label:      label,
		Tokens:     tokens,
		Duration:   time.Since(start),
		Candidates: candidates,
	}
	if err != nil {
		ev.Err = err.Error()
	}
	progress.Emit(sink, ev)
}

// readOnlyTools builds the read-only code tool set (read_file, list_dir, grep,
// git_blame, git_log, plus the LSP-backed find_definition / find_references /
// find_implementations, read_symbol, find_usages, and outline) rooted at the
// repository, shared by finder and refuter agents. readCaps bounds each
// read_file result; its zero value uses the package defaults, while finders
// pass tighter caps (see Options.finderReadCaps) to slow per-turn history
// growth cache-safely. All tools are safe for concurrent use across parallel
// agents; the code-navigation tools share the funnel's lazily-started
// language-server manager, which Funnel.Close shuts down.
func (f *Funnel) readOnlyTools(readCaps agent.ReadCaps) ([]agent.Tool, error) {
	root := f.repo.Root()
	readFile, err := agent.NewReadFileWithCaps(root, readCaps)
	if err != nil {
		return nil, fmt.Errorf("funnel: read_file tool: %w", err)
	}
	listDir, err := agent.NewListDir(root)
	if err != nil {
		return nil, fmt.Errorf("funnel: list_dir tool: %w", err)
	}
	grep, err := agent.NewGrep(root)
	if err != nil {
		return nil, fmt.Errorf("funnel: grep tool: %w", err)
	}
	gitBlame, err := agent.NewGitBlame(root, nil)
	if err != nil {
		return nil, fmt.Errorf("funnel: git_blame tool: %w", err)
	}
	gitLog, err := agent.NewGitLog(root, nil)
	if err != nil {
		return nil, fmt.Errorf("funnel: git_log tool: %w", err)
	}
	nav, err := f.codeNav()
	if err != nil {
		return nil, fmt.Errorf("funnel: code navigation tools: %w", err)
	}
	return append([]agent.Tool{readFile, listDir, grep, gitBlame, gitLog}, nav.Tools()...), nil
}

// transcriptOption returns a Runner option that persists transcripts when a
// TranscriptDir is configured, or a no-op option otherwise.
func (f *Funnel) transcriptOption() agent.Option {
	if f.opts.TranscriptDir == "" {
		return func(*agent.Runner) {}
	}
	return agent.WithTranscriptDir(f.opts.TranscriptDir)
}

// noteMu guards Result.Skipped, which parallel stages append to concurrently.
var noteMu sync.Mutex

// note appends a human-readable skip/degradation note to the Result. It is safe
// for concurrent use by parallel stage goroutines.
func (f *Funnel) note(result *Result, msg string) {
	noteMu.Lock()
	result.Skipped = append(result.Skipped, msg)
	noteMu.Unlock()
}

// normalizeSeverity coerces a model-supplied severity to one of the four valid
// values, defaulting unknown/empty to "medium" so ranking and reporting always
// have a usable value.
func normalizeSeverity(s string) string {
	switch s {
	case "critical", "high", "medium", "low":
		return s
	default:
		return "medium"
	}
}

// sandboxMinSeverity returns the effective minimum severity for sandbox
// gating, defaulting to "high" when empty.
func sandboxMinSeverity(s string) string {
	switch s {
	case "critical", "high", "medium", "low":
		return s
	default:
		return "high"
	}
}

// sandboxMaxExecs returns the effective per-candidate execution budget,
// defaulting to 3 when zero or negative.
func sandboxMaxExecs(n int) int {
	if n <= 0 {
		return 3
	}
	return n
}

// buildSandboxTool builds the sandbox_exec tool for a candidate if the feature
// is enabled and the candidate's severity meets the minimum threshold. It
// returns nil when the tool should not be offered. sbExecs and sbMillis are
// run-spanning atomic counters shared across all candidates; the per-call
// onExec callback increments them from the tool's goroutine.
func (f *Funnel) buildSandboxTool(c Candidate, sbExecs *atomic.Int32, sbMillis *atomic.Int64) agent.Tool {
	opts := f.opts.SandboxOpts
	if !opts.Enabled || opts.Sandbox == nil {
		return nil
	}
	// The severity floor is bypassed for executable claims: a claim about a
	// deterministic / pure function's input->output behavior is most cleanly
	// falsified by running it, and suppressing the sandbox below "high" is
	// what allowed the parseSARIF-cap false positive (bugbot-aud, GH #64) to
	// slip through. Environmental / I/O claims still pay the threshold cost;
	// only isExecutableClaim(c) candidates opt in. The default min severity
	// is unchanged — the bypass is per-claim, not per-config.
	executable := isExecutableClaim(c)
	minSev := sandboxMinSeverity(opts.MinSeverity)
	if !executable && severityRank(c.Severity) > severityRank(minSev) {
		// Candidate is BELOW the threshold (higher rank number = less severe).
		return nil
	}
	maxExec := sandboxMaxExecs(opts.MaxExecs)
	onExec := func(d time.Duration) {
		sbExecs.Add(1)
		sbMillis.Add(d.Milliseconds())
	}
	return agent.NewSandboxExecTool(opts.Sandbox, f.repo.Root(), maxExec, f.deps.ROMounts, f.deps.Env, f.deps.SetupCmds, onExec)
}

// ensureDepPrefetch runs the one-time online dependency prefetch (only set for
// DepStrategyFetch) at most once across all candidates. It is safe for
// concurrent callers. A prefetch failure is recorded and returned on every
// subsequent call so callers can degrade (skip the sandbox tool) rather than
// hand a refuter a cache that cannot resolve modules.
func (f *Funnel) ensureDepPrefetch(ctx context.Context) error {
	if f.deps.Prefetch == nil {
		return nil
	}
	f.depPrefetchOnce.Do(func() {
		f.depPrefetchErr = f.deps.Prefetch(ctx)
	})
	return f.depPrefetchErr
}

// normalizeConfidence coerces a model-supplied confidence to one of the three
// valid values. Unknown/empty maps to "low" so that ambiguous output is treated
// conservatively (low-confidence candidates are dropped in triage).
func normalizeConfidence(s string) string {
	switch s {
	case "high", "medium", "low":
		return s
	default:
		return "low"
	}
}
