package funnel

// finder_runner.go holds the finder executor (runFinder, runFinderWithPrompt)
// and the postmortem cluster extracted from hypothesize.go for readability.
// Pure code motion: no logic changes.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
)

// finderStatus classifies a finder run's parse outcome so the funnel can tell a
// genuine reliability failure apart from a deliberate budget stop.
type finderStatus int

const (
	// finderOK means the finder produced parseable candidates.
	finderOK finderStatus = iota
	// finderParseFailed means the finder ran to a non-budget stop but produced no
	// parseable JSON even after the repair round-trip — its findings are LOST.
	finderParseFailed
	// finderBudgetStopped means the finder was truncated by the shared budget
	// pool or its own token budget; an unparseable partial result here is an
	// expected budget stop, not a reliability failure.
	finderBudgetStopped
	// finderRateLimited means the finder exhausted retries against a
	// rate-limiting provider (llm.ErrRateLimited). Coverage is incomplete but
	// recoverable by lowering --concurrency or re-running — NOT lost like a
	// genuine parse failure, so this status is excluded from FinderFailures
	// and from the reliability gate.
	finderRateLimited
)

// runFinder executes a single finder agent for one lens over one task and maps
// its JSON output to Candidates tagged with the lens. The agent's limits are
// derived from the shared budget pool at launch (remaining-pool allowance plus a
// pre-turn budget check), so a finder launched late gets only the headroom left
// and one already in flight stops at its next turn once the pool is exhausted.
//
// task is the pre-built user message for the agent. Standard chunk-based units
// pass finderTask(files, leads); the diff-intent unit passes buildDiffIntentTask.
//
// The finderStatus return distinguishes a parse failure (the finder ran but
// produced no parseable JSON even after the repair round-trip, so its result is
// LOST, not a clean "found nothing") from a budget stop (the run was truncated
// by the budget pool / token budget, so an unparseable partial is expected). The
// funnel surfaces parse failures so a scan never silently reports "No findings"
// when a lens actually failed, while budget stops are accounted separately.
//
// This is a thin wrapper around runFinderWithPrompt that builds the system prompt
// from persona+lens+langs and uses the lens name as the progress label. Test code
// calls this directly; production code calls runFinderWithPrompt after composing
// the strategy-aware system prompt.
func (f *Funnel) runFinder(ctx context.Context, finder llm.Client, tools []agent.Tool, persona string, l Lens, langs []ingest.Language, task string, budget *budgetState) ([]Candidate, finderStatus, *finderPostmortem, error) {
	sysprompt := finderSystemPrompt(persona, l, langs)
	start := time.Now()
	// One AgentScope for the whole run, threaded through runFinderWithPrompt so
	// its tool-call activity and this call's Finished event share the run's
	// AgentID (see agent_runners.go's activitySinkFor doc).
	scope := progress.NewAgentScope(f.opts.Progress, progress.RoleFinder, l.Name).Start()
	cands, status, outcome, pm, err := f.runFinderWithPrompt(ctx, finder, tools, sysprompt, l.Name, l, task, budget, start, scope)
	emitAgentFinished(scope, outcome, start, err)
	return cands, status, pm, err
}

// runFinderWithPrompt is the core finder executor. It accepts a pre-composed
// system prompt and a progress label so callers (hypothesize) can inject
// strategy clauses and use strategy-qualified labels (lens@strategy) without
// rebuilding the prompt inside this function.
//
// startedAt is the wall-clock time the caller captured before invoking this
// function; the caller is responsible for emitting KindAgentFinished (with the
// Candidates count it derives from the returned candidates slice) after this
// function returns. scope is the SAME AgentScope the caller already Started
// for this run — runFinderWithPrompt neither mints nor Starts a scope itself;
// it only threads scope into the runner's activity sink so every KindToolCall
// this run emits carries the run's AgentID, matching the Started/Finished
// bracket the caller owns (see agent_runners.go's activitySinkFor doc and
// bugbot-r7ub).
//
// The returned *agent.Outcome carries the agent's Usage (InputTokens /
// OutputTokens / CacheReadInputTokens) that the caller uses to populate the
// per-unit observability row. The Outcome is non-nil as long as the agent ran
// at least one turn; callers must handle nil (budget-pool pre-turn stop).
//
// On any no-parse failure path (finderParseFailed or finderBudgetStopped) the
// returned *finderPostmortem is non-nil and carries the classification,
// underlying error string, raw model output head, and token counts needed to
// diagnose the failure post-run. On finderOK the postmortem is nil. The
// postmortem is built here — where err is still live — so err is never
// silently dropped: it flows into pm.ErrString and pm.Class.
//
// Threading seam: runFinderWithPrompt has no store access and does not record
// the agent_units row; it returns the postmortem so the goroutine caller
// (hypothesize) can fold it into the Detail field of the
// recordFinderUnitWithTimeDetail call. This keeps the recording at a single
// site and avoids threading a store reference into this function.
func (f *Funnel) runFinderWithPrompt(ctx context.Context, finder llm.Client, tools []agent.Tool, sysprompt, label string, l Lens, task string, budget *budgetState, startedAt time.Time, scope progress.AgentScope, extraOpts ...agent.Option) ([]Candidate, finderStatus, *agent.Outcome, *finderPostmortem, error) {
	// attempt runs one finder pass: it builds the runner (layering any
	// per-attempt options on top of the standard finder set), runs RunJSON, and
	// maps the result into candidates or a classified failure + postmortem. It
	// is invoked once normally and, on a max-tokens-truncation parse failure,
	// once more at a doubled output cap (bugbot-rwe).
	attempt := func(extra ...agent.Option) ([]Candidate, finderStatus, *agent.Outcome, *finderPostmortem, error) {
		opts := append([]agent.Option{f.activitySinkFor(scope)}, extraOpts...)
		opts = append(opts, extra...)
		runner := f.newAgentRunner(finder, tools, sysprompt, budget.finderRunnerLimits(f.opts.Limits.FinderLimits), opts...)

		var out candidateList
		outcome, err := runner.RunJSON(ctx, task, candidatesSchema, &out)
		if err != nil {
			// A finder that fails to produce parseable JSON yields no candidates
			// rather than aborting the whole scan: one lens/chunk failing must not
			// sink the others. Context cancellation is the exception — propagate it.
			if ctx.Err() != nil {
				return nil, finderOK, outcome, nil, ctx.Err()
			}
			// Distinguish a genuine parse failure from a budget stop. If the run was
			// truncated by the shared budget pool or its own token budget, an
			// unparseable partial is the expected consequence of stopping early, not a
			// reliability problem — classify it as a budget stop so it does not inflate
			// the finder-failure count. Otherwise its findings are LOST: report a parse
			// failure so a scan never silently prints "No findings" when a lens broke.
			//
			// In both cases, build a postmortem capturing the classification, the
			// underlying err (which carries the classified provider error — e.g. 429 +
			// Retry-After from llm.APIError — or the parse error message), and the raw
			// model output head. err is intentionally NOT discarded here; it flows into
			// the postmortem so the next real failure is diagnosable from stored data.
			pm := buildFinderPostmortem(outcome, err)
			if budgetStopped(outcome) {
				return nil, finderBudgetStopped, outcome, &pm, nil
			}
			// Rate-limit exhaustion is not a lost-finding failure: the provider
			// throttled us after the retry budget was spent. Coverage is incomplete
			// but recoverable (lower --concurrency / re-run) and the retry client
			// already honored Retry-After. Classify distinctly so it never
			// inflates FinderFailures or trips the SCAN RELIABILITY WARNING; the
			// postmortem already carries Class=finderClassRateLimited via
			// classifyFinderErr.
			if errors.Is(err, llm.ErrRateLimited) {
				return nil, finderRateLimited, outcome, &pm, nil
			}
			return nil, finderParseFailed, outcome, &pm, nil
		}

		cands := make([]Candidate, 0, len(out.Candidates))
		for _, rc := range out.Candidates {
			cands = append(cands, Candidate{
				Lens:        l.Name,
				File:        rc.File,
				Line:        rc.Line,
				Title:       rc.Title,
				Description: rc.Description,
				Severity:    normalizeSeverity(domain.Severity(rc.Severity)),
				Evidence:    rc.Evidence,
				Confidence:  normalizeConfidence(domain.Confidence(rc.Confidence)),
				DefectKind:  domain.DefectKind(rc.DefectKind),
				Subject:     rc.Subject,
			})
		}
		return cands, finderOK, outcome, nil, nil
	}

	cands, status, outcome, pm, err := attempt()
	// bugbot-rwe: a finder unit lost to per-completion max-tokens truncation — a
	// reasoning model (e.g. MiniMax-M3) that spent the DefaultMaxOutputTokens cap
	// inside <think> blocks before emitting JSON — is recoverable. Retry ONCE at a
	// doubled per-completion cap so the model has room for think + JSON. The
	// doubled WithMaxTokens is layered last and so overrides newAgentRunner's
	// default. shouldRetryFinderCap gates this tightly (see its doc).
	if shouldRetryFinderCap(status, outcome, err) {
		cands, status, outcome, pm, err = attempt(agent.WithMaxTokens(finderRetryMaxOutputTokens))
	}
	return cands, status, outcome, pm, err
}

// finderRetryMaxOutputTokens is the doubled per-completion output cap used on the
// single retry of a finder unit lost to max-tokens truncation (bugbot-rwe). A
// reasoning model that spent the DefaultMaxOutputTokens cap inside <think> blocks
// before emitting JSON gets room for think + JSON on the retry.
const finderRetryMaxOutputTokens = 2 * DefaultMaxOutputTokens

// shouldRetryFinderCap reports whether a finder pass that produced no candidates
// should be retried once at the doubled per-completion output cap. It fires only
// for the bugbot-rwe failure mode: a parse failure whose proximate cause was the
// per-completion max-tokens cap — Outcome.LastStopReason == llm.StopMaxTokens, the
// canonical cap-truncation signal also used by truncationNote. It rejects budget
// stops (no headroom to retry), rate limits (recover by lowering concurrency),
// non-truncated malformed JSON (a bigger cap would not help), and a nil outcome.
func shouldRetryFinderCap(status finderStatus, outcome *agent.Outcome, err error) bool {
	if status != finderParseFailed {
		return false
	}
	if outcome == nil || outcome.LastStopReason != llm.StopMaxTokens {
		return false
	}
	if budgetStopped(outcome) {
		return false
	}
	return !errors.Is(err, llm.ErrRateLimited)
}

// finderFailureClass is a coarse classification of why a finder failed to
// produce parseable output. It is recorded in the agent_units Detail column so
// an operator can distinguish transient provider problems from model output
// issues without reading raw logs.
type finderFailureClass string

const (
	// finderClassRateLimited means the provider returned a 429 / rate-limit
	// response (llm.ErrRateLimited) after all retry attempts were exhausted.
	finderClassRateLimited finderFailureClass = "rate-limited"
	// finderClassEmptyOutput means the model returned an empty text body — no
	// think blocks, no JSON, nothing parseable.
	finderClassEmptyOutput finderFailureClass = "empty-output"
	// finderClassUnparseable means the model returned text but it was not valid
	// JSON even after the repair round-trip.
	finderClassUnparseable finderFailureClass = "unparseable"
	// finderClassBudgetStop means the run was cut short by the shared token
	// budget pool or the run's own token budget before producing parseable JSON.
	finderClassBudgetStop finderFailureClass = "budget-stop"
	// finderClassTransportError means the provider was unreachable: a
	// transport / connection failure surfaced as an *llm.APIError with
	// StatusCode==0 (the shape produced by the openai / google / anthropic
	// adapters for non-HTTP errors — timeout, connection reset, DNS failure).
	// Distinct from rate-limit (the provider is reachable but throttling) and
	// from parse failures (the provider answered; the model output is the
	// problem). This class drives the finder-stage circuit breaker
	// (bugbot-2uz): N concurrent transport errors with zero successes is a
	// strong signal the provider is down, and the funnel aborts further
	// launches instead of waiting for every retry to time out.
	finderClassTransportError finderFailureClass = "transport-error"
)

// finderPostmortem is captured on every no-parse finder failure (both
// finderParseFailed and finderBudgetStopped paths). It preserves enough
// evidence to diagnose the root cause post-run without re-running the scan.
//
// Storage: encoded into the Detail column of the existing agent_units row so
// the artifact is retrievable via the existing "report units" CLI path. The
// grain is already one row per unit execution — extending Detail avoids a new
// table, mirrors the existing verifier detail pattern, and keeps the schema
// unchanged. Recording failures are best-effort and never abort the scan.
type finderPostmortem struct {
	// Class is the coarse failure classification.
	Class finderFailureClass
	// ErrString is err.Error() from the runner, nil-safe (empty string when err
	// was nil, e.g. a pure budget stop with no underlying error).
	ErrString string
	// RawHead is the first finderPostmortemRawCap bytes of the model's output
	// (outcome.FinalText). Capped to prevent unbounded storage.
	RawHead string
	// RawLen is the full byte-length of outcome.FinalText before capping.
	RawLen int
	// Empty is true when outcome.FinalText was empty.
	Empty bool
	// HadThink is true when outcome.FinalText contained a <think> span (a
	// reasoning-model thought block). Detected by substring search; no parse.
	HadThink bool
	// TruncationReason is outcome.TruncationReason, nil-safe.
	TruncationReason string
	// InputTokens / OutputTokens / CacheReadTokens from outcome.Usage.
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

// finderPostmortemRawCap is the maximum bytes of raw model output preserved in
// the postmortem. 4 KB captures enough context for diagnosis without bloating
// the Detail column.
const finderPostmortemRawCap = 4 * 1024

// classifyFinderErr maps the underlying runner error to a finderFailureClass.
// It uses errors.Is against llm.ErrRateLimited (the sentinel produced by
// llm.APIError.Unwrap when the provider returned a 429). The outcome and err
// are both nil-safe.
func classifyFinderErr(outcome *agent.Outcome, err error) finderFailureClass {
	if budgetStopped(outcome) {
		return finderClassBudgetStop
	}
	if err != nil && errors.Is(err, llm.ErrRateLimited) {
		return finderClassRateLimited
	}
	if isTransportError(err) {
		// Provider unreachable (timeout / connection reset / DNS failure).
		// Distinct from rate-limit (provider reachable, throttling) and from
		// parse failures (the runner did not return an error at all). This
		// branch also handles the bare APIError{Kind: ErrServer, StatusCode: 0}
		// shape that the adapters return for non-HTTP transport failures.
		return finderClassTransportError
	}
	finalText := ""
	if outcome != nil {
		finalText = outcome.FinalText
	}
	if finalText == "" {
		return finderClassEmptyOutput
	}
	return finderClassUnparseable
}

// isTransportError reports whether err represents a transport / connection
// failure: an *llm.APIError with StatusCode==0 (the shape produced by the
// openai / google / anthropic adapters for non-HTTP errors — timeouts,
// connection resets, DNS failures). Both the bare
// "APIError{Kind: ErrServer, StatusCode: 0}" shape and any other
// APIError{StatusCode: 0} variant match (Kind may be ErrServer, ErrOverloaded,
// or any unrecognized transport-level failure surfaced through the adapter).
// Distinct from rate-limit (errors.Is(err, llm.ErrRateLimited) — provider
// reachable, throttling) and from parse failures (no error from the runner at
// all). nil-safe.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 0
	}
	return false
}

// buildFinderPostmortem constructs a finderPostmortem from a failed run. Both
// outcome and err may be nil (budget-pool pre-turn stop with no error).
func buildFinderPostmortem(outcome *agent.Outcome, err error) finderPostmortem {
	pm := finderPostmortem{
		Class: classifyFinderErr(outcome, err),
	}
	if err != nil {
		pm.ErrString = err.Error()
	}
	if outcome != nil {
		raw := outcome.FinalText
		pm.RawLen = len(raw)
		pm.Empty = raw == ""
		pm.HadThink = strings.Contains(raw, "<think>")
		if len(raw) > finderPostmortemRawCap {
			raw = raw[:finderPostmortemRawCap]
		}
		pm.RawHead = raw
		pm.TruncationReason = outcome.TruncationReason
		pm.InputTokens = outcome.Usage.InputTokens
		pm.OutputTokens = outcome.Usage.OutputTokens
		pm.CacheReadTokens = outcome.Usage.CacheReadInputTokens
	}
	return pm
}

// finderPostmortemDetail encodes a finderPostmortem as a compact string for
// the agent_units Detail column. The format mirrors the verifier detail style:
// structured key=value pairs, injection-safe (no model-authored free text in
// the structured portion; raw output is capped and included after a separator
// for direct inspection).
func finderPostmortemDetail(pm finderPostmortem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "class=%s empty=%v had_think=%v raw_len=%d trunc=%q in=%d out=%d cache=%d err=%q",
		pm.Class, pm.Empty, pm.HadThink, pm.RawLen,
		pm.TruncationReason, pm.InputTokens, pm.OutputTokens, pm.CacheReadTokens,
		pm.ErrString,
	)
	if pm.RawHead != "" {
		fmt.Fprintf(&b, " raw_head=%q", pm.RawHead)
	}
	return b.String()
}
