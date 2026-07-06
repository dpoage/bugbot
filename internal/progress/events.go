// Package progress is the activity-visibility seam for Bugbot: a typed,
// JSON-serializable event stream emitted by the funnel and daemon, fanned out to
// pluggable sinks that render live activity.
//
// The pipeline used to run in silence — a scan could drive LLM agents for many
// minutes before printing its only output (the final summary), and the daemon
// logged sparsely. progress fixes that by letting the pipeline emit a small,
// flat set of events at every meaningful boundary (run/stage/agent start and
// finish, spend ticks, budget degradation, cycle schedule) without coupling the
// pipeline to any particular renderer.
//
// # The contract that matters
//
// Emission must NEVER block, fail, or panic the pipeline. A sink is a passive
// observer: if it is slow, full, or broken, the scan still runs to completion
// and the daemon still cycles. Every Sink here honors that — the renderers
// either take a short mutex (PaneRenderer, LogRenderer) or drop on a full buffer
// / rate-limit (SnapshotSink) — and Emit on a nil sink is a no-op so callers
// never need a nil check.
//
// # Sinks
//
//   - PaneRenderer  — in-place ANSI multi-line status pane for an interactive
//     TTY (bugbot scan in a terminal).
//   - LogRenderer   — one plain line per significant event, for piped stdout and
//     as the daemon's slog bridge.
//   - SnapshotSink  — maintains an atomically-written status.json so a separate
//     `bugbot status` can read a running scan/daemon's current activity.
//   - Multi / Discard — fanout and no-op helpers.
package progress

import (
	"fmt"
	"time"
)

// Kind enumerates the event types. It is a string so events serialize to
// self-describing JSON (and so a future sink can switch on a stable name rather
// than an integer that shifts when the set grows).
type Kind string

const (
	// KindScanStarted marks the beginning of a funnel run (Sweep/Targeted).
	// Fields: ScanKind, Commit.
	KindScanStarted Kind = "scan_started"
	// KindStageStarted / KindStageFinished bracket a pipeline stage
	// (hypothesize, triage, verify, persist).
	// Fields: Stage. KindStageFinished also sets Counts (see Counts nil semantics).
	KindStageStarted  Kind = "stage_started"
	KindStageFinished Kind = "stage_finished"
	// KindAgentStarted / KindAgentFinished bracket one finder or verifier agent
	// run.
	// Fields: Role, Label. KindAgentFinished also sets Tokens, Duration, Err
	// (Err is empty on success), and Candidates (finder only; zero for verifiers).
	KindAgentStarted  Kind = "agent_started"
	KindAgentFinished Kind = "agent_finished"
	// KindAgentActivity carries a short human-readable note about what an
	// in-flight agent is currently doing (e.g. "reading main.go", "running
	// sandbox"). Emitted at most once per tool-call turn by the runner; the
	// snapshot and renderers update the relevant AgentStatus.Activity in place.
	// This is NOT a terminal event.
	// Fields: Role, Label, Activity.
	KindAgentActivity Kind = "agent_activity"
	// KindSpendTick reports cumulative token spend as it accrues.
	// Fields: InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens.
	KindSpendTick Kind = "spend_tick"
	// KindBudgetDegraded / KindBudgetStopped mirror the funnel's budget
	// degradation and hard-stop decisions (also surfaced on Result).
	// Fields: Message.
	KindBudgetDegraded Kind = "budget_degraded"
	KindBudgetStopped  Kind = "budget_stopped"
	// KindFindingVerified reports a candidate that survived adversarial
	// verification (a Tier-2 survivor).
	// Fields: File, Line, Title.
	KindFindingVerified Kind = "finding_verified"
	// KindFindingKilled reports a candidate that was definitively refuted by the
	// adversarial verification panel. One event per killed candidate, emitted as
	// the verdict is reached (not deferred to stage-finish), so live status
	// counters tick as the verify stage progresses.
	// Fields: File, Line, Title.
	KindFindingKilled Kind = "finding_killed"
	// KindCandidateTriaged reports one candidate forwarded by the streaming
	// triage consumer to verification (a cluster primary). Live-counter tick;
	// the authoritative total still arrives with StageFinished(triage).
	// Fields: Label.
	KindCandidateTriaged Kind = "candidate_triaged"
	// KindLensFailed reports a finder (or refuter) agent that produced no
	// parseable output: its findings, if any, are LOST. Renderers should surface
	// this prominently — it means an empty result is untrustworthy, not clean.
	// Fields: Role, Label, Err.
	KindLensFailed Kind = "lens_failed"
	// KindToolUnhealthy reports a HARNESS-side tool that failed: a tool the
	// Bugbot runner itself relies on (e.g. the sandbox runtime, a code-search
	// helper, a test runner) is broken or unavailable. This is harness meta,
	// NEVER a finding about the target repository — renderers must surface it
	// PROMINENTLY so the operator can distinguish a clean empty result from one
	// that could not be produced because a tool was down. Fields: Role, Label,
	// Tool, Severity, Message (carries the human-readable reason).
	KindToolUnhealthy Kind = "tool_unhealthy"
	// KindScanFinished marks the end of a funnel run and carries the stats
	// summary.
	// Fields: Counts (non-nil on normal completion; nil only if abandoned before
	// any stage completed), InputTokens, OutputTokens, CacheReadTokens,
	// CacheCreationTokens.
	KindScanFinished Kind = "scan_finished"
	// KindHeatOrdered reports that Sweep reordered its targets by churn heat.
	// HeatFiles carries the number of files with non-zero heat; Label carries a
	// human-readable top-5 summary (path:score pairs). HeatOrdered is always true
	// when this event fires (it is not emitted when heat is disabled or the map is
	// empty).
	// Fields: Count (number of files with non-zero heat), Label (top-N summary).
	KindHeatOrdered Kind = "heat_ordered"
	// KindCycleScheduled reports the daemon's next poll/sweep deadlines.
	// Fields: NextPoll, NextSweep, NextBacklog (zero when repro is disabled).
	KindCycleScheduled Kind = "cycle_scheduled"
	// KindCycleStarted / KindCycleFinished bracket one daemon cycle.
	// Fields: ScanKind. KindCycleFinished also sets Count (new findings this cycle).
	KindCycleStarted  Kind = "cycle_started"
	KindCycleFinished Kind = "cycle_finished"
	// KindReverify / KindPromote report post-cycle re-verification and
	// reproduction-promotion outcomes.
	// Fields: Count.
	KindReverify Kind = "reverify"
	KindPromote  Kind = "promote"
	// KindSweepSummary is emitted once per Sweep call, before the scan starts,
	// with a summary of the sweep's target set: total file count, never-scanned
	// count, and changed-since-scan count. Count carries the total targets;
	// Message carries the human-readable summary. Renderers can use this to show
	// context about the upcoming sweep without waiting for it to finish.
	// Fields: Count (total targets), Message.
	KindSweepSummary Kind = "sweep_summary"
	// KindReproAttempt reports one repro round (initial plan or a revision) for
	// a single finding: the reproducer agent proposed a plan, it ran in the
	// sandbox, and the round was interpreted to a verdict. Emitted once per
	// round from repro.Reproducer.Attempt, in addition to (not instead of) the
	// bracketing KindAgentStarted/KindAgentFinished pair for the whole
	// multi-round attempt. Low-volume (bounded by MaxAttempts, default 2), so
	// renderers can surface every round rather than suppressing it as noise.
	// Fields: Role (RoleReproducer), Label (finding title), Attempt,
	// MaxAttempts, Verdict, Duration.
	KindReproAttempt Kind = "repro_attempt"
)

// Stage names the pipeline stage an event belongs to. Kept as plain strings so
// the funnel and any renderer agree on a stable vocabulary.
const (
	StageHypothesize = "hypothesize"
	StageTriage      = "triage"
	StageVerify      = "verify"
	StagePersist     = "persist"
)

// Role names the agent role an Agent event belongs to. Finder and verifier are
// the funnel's core stages; cartographer, reproducer, patch-prover, and
// severity are the extra-feature agents that surface through the same
// AgentScope observability seam.
const (
	RoleFinder       = "finder"
	RoleVerifier     = "verifier"
	RoleCartographer = "cartographer"
	RoleReproducer   = "reproducer"
	RolePatchProver  = "patch-prover"
	RoleSeverity     = "severity"
)

// Counts is the per-stage accounting carried on stage-finished events and the
// final summary. Fields mirror funnel.Stats but live here so progress does not
// import funnel (the funnel imports progress, not the reverse).
//
// Nil-vs-zero semantics: a nil *Counts pointer means "no accounting available"
// (the stage did not reach a point where counts were meaningful, or the event
// kind does not carry counts at all). A non-nil &Counts{} with all-zero fields
// means the stage ran to completion but produced nothing. Consumers MUST treat
// nil as "unavailable" and non-nil as "present but possibly zero"; they MUST NOT
// substitute nil for &Counts{} or vice versa.
type Counts struct {
	Hypothesized int `json:"hypothesized,omitempty"`
	Triaged      int `json:"triaged,omitempty"`
	Verified     int `json:"verified,omitempty"`
	Killed       int `json:"killed,omitempty"`
	// FinderFailures is how many finder agents produced no parseable output this
	// run. Non-zero means the result is suspect: some lens's findings were lost,
	// so an empty/sparse finding set is not a clean bill of health.
	FinderFailures int `json:"finder_failures,omitempty"`
}

// Event is one progress record. It is deliberately flat and JSON-serializable:
// a single struct with a Kind discriminator and a superset of fields, so sinks
// can switch on Kind and read only the fields that kind populates. Unused fields
// are zero and omitted from JSON.
//
// Time is set by Emit when zero, so callers never have to stamp events.
//
// Field/Kind matrix — each Kind documents its fields in the Kind const comment
// above. Fields not listed for a given Kind are zero (string="", int=0,
// pointer=nil, time=zero) and MUST be ignored by consumers. Validate() checks
// the most common invariant violations but is advisory: Emit does not call it,
// so construction errors reach sinks as-is for observability, not silence.
type Event struct {
	Kind Kind      `json:"kind"`
	Time time.Time `json:"time"`

	// ScanKind / Commit identify the run (scan_started, cycle_*). ScanKind is the
	// store scan kind string ("sweep"/"targeted"/"oneshot") or, for the daemon,
	// the cycle kind.
	ScanKind string `json:"scan_kind,omitempty"`
	Commit   string `json:"commit,omitempty"`

	// Stage / Counts describe a stage boundary.
	// Counts follows nil-vs-zero semantics: see Counts type documentation.
	Stage  string  `json:"stage,omitempty"`
	Counts *Counts `json:"counts,omitempty"`

	// Role / Label / Tokens / Duration / Err describe an agent run. Label is the
	// lens name (finder) or candidate title (verifier). Err is a message, not an
	// error value, so the event stays JSON-serializable.
	Role     string        `json:"role,omitempty"`
	Label    string        `json:"label,omitempty"`
	Tokens   int64         `json:"tokens,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
	Err      string        `json:"err,omitempty"`

	// InputTokens / OutputTokens carry cumulative spend on spend_tick and the
	// final summary. InputTokens includes cached tokens (the llm.Usage
	// convention); CacheReadTokens / CacheCreationTokens are the subsets served
	// from / written to the provider's prompt cache.
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`

	// File / Line / Title describe a verified or killed finding.
	File  string `json:"file,omitempty"`
	Line  int    `json:"line,omitempty"`
	Title string `json:"title,omitempty"`

	// Candidates is set on KindAgentFinished for finder (RoleFinder) agents: it
	// carries the number of candidates emitted by that finder unit. This lets live
	// status counters tick as each finder completes rather than waiting for the
	// stage-finished event. Zero for verifier agents and when no candidates were
	// found (not omitted so a zero count is distinguishable from an unset field
	// in typed consumers; JSON omitempty keeps wire size small).
	Candidates int `json:"candidates,omitempty"`

	// Activity carries a short single-line note for KindAgentActivity events:
	// what the in-flight agent is currently doing, derived from its tool calls
	// (e.g. "reading main.go", "running sandbox"). Unused on all other events.
	Activity string `json:"activity,omitempty"`

	// NextPoll / NextSweep / NextBacklog carry the daemon schedule
	// (cycle_scheduled). NextBacklog is zero when the backlog-repro timer is
	// disabled (EnableRepro=false) or when its next firing is effectively never.
	NextPoll    time.Time `json:"next_poll,omitempty"`
	NextSweep   time.Time `json:"next_sweep,omitempty"`
	NextBacklog time.Time `json:"next_backlog,omitempty"`

	// Count is a generic integer payload: re-verified-closed count (reverify),
	// promoted count (promote), or new findings (cycle_finished).
	Count int `json:"count,omitempty"`

	// Message is a free-form human note (budget degradation reason, skip reason).
	Message string `json:"message,omitempty"`

	// Tool names the harness-side tool that failed. Set only on
	// KindToolUnhealthy events; zero/empty on every other kind. Sinks surface it
	// prominently so the operator can tell which Bugbot tool was down.
	Tool string `json:"tool,omitempty"`

	// Severity classifies the tool-health failure (critical/high/medium/low).
	// Set only on KindToolUnhealthy events; empty on every other kind.
	// Sinks use it to choose how prominently to render the failure and to
	// aggregate per-tool health by max-severity across events.
	Severity string `json:"severity,omitempty"`

	// Attempt / MaxAttempts / Verdict describe a KindReproAttempt round: the
	// 1-based round number, the configured cap (repro.Options.MaxAttempts,
	// resolved), and the round's outcome — "demonstrated" on success, or a
	// repro.VerdictReason string ("exit_zero", "timeout", "invalid_plan",
	// "unparseable_plan", …) on a non-demonstrating round. Role/Label/Duration
	// are reused from the agent-run fields above (Role is always
	// RoleReproducer; Label is the finding title).
	Attempt     int    `json:"attempt,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
	Verdict     string `json:"verdict,omitempty"`
}

// Validate returns an error for clearly-invalid Kind/field combinations.
// It is advisory: Emit does not call it and existing emitters need not call it.
// Use it in tests or consumer code where catching construction errors is
// worthwhile without blocking the happy path.
//
// Checked invariants:
//   - Kind must be non-empty.
//   - KindStageStarted/KindStageFinished require Stage to be non-empty.
//   - KindStageFinished with a nil Counts is not an error (stage may have been
//     abandoned), but KindStageFinished with a Stage and a nil Counts is flagged
//     as a warning (use &Counts{} to signal "ran but produced nothing").
//   - KindAgentStarted/KindAgentFinished/KindAgentActivity require Role and Label.
//   - KindFindingVerified/KindFindingKilled require File.
//   - KindToolUnhealthy requires Role, Label, Tool, and Severity (Message is the
//     human-readable reason and is permitted but not required).
//   - KindReproAttempt requires Role, Label, and Verdict.
func (e Event) Validate() error {
	if e.Kind == "" {
		return fmt.Errorf("progress: event has empty Kind")
	}
	switch e.Kind {
	case KindStageStarted, KindStageFinished:
		if e.Stage == "" {
			return fmt.Errorf("progress: %s event missing Stage", e.Kind)
		}
	case KindAgentStarted, KindAgentFinished, KindAgentActivity:
		if e.Role == "" {
			return fmt.Errorf("progress: %s event missing Role", e.Kind)
		}
		if e.Label == "" {
			return fmt.Errorf("progress: %s event missing Label", e.Kind)
		}
		if e.Kind == KindAgentActivity && e.Activity == "" {
			return fmt.Errorf("progress: %s event missing Activity", e.Kind)
		}
	case KindFindingVerified, KindFindingKilled:
		if e.File == "" {
			return fmt.Errorf("progress: %s event missing File", e.Kind)
		}
	case KindToolUnhealthy:
		if e.Role == "" {
			return fmt.Errorf("progress: %s event missing Role", e.Kind)
		}
		if e.Label == "" {
			return fmt.Errorf("progress: %s event missing Label", e.Kind)
		}
		if e.Tool == "" {
			return fmt.Errorf("progress: %s event missing Tool", e.Kind)
		}
		if e.Severity == "" {
			return fmt.Errorf("progress: %s event missing Severity", e.Kind)
		}
	case KindReproAttempt:
		if e.Role == "" {
			return fmt.Errorf("progress: %s event missing Role", e.Kind)
		}
		if e.Label == "" {
			return fmt.Errorf("progress: %s event missing Label", e.Kind)
		}
		if e.Verdict == "" {
			return fmt.Errorf("progress: %s event missing Verdict", e.Kind)
		}
	}
	return nil
}
