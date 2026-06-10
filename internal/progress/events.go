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

import "time"

// Kind enumerates the event types. It is a string so events serialize to
// self-describing JSON (and so a future sink can switch on a stable name rather
// than an integer that shifts when the set grows).
type Kind string

const (
	// KindScanStarted marks the beginning of a funnel run (Sweep/Targeted).
	KindScanStarted Kind = "scan_started"
	// KindStageStarted / KindStageFinished bracket a pipeline stage
	// (hypothesize, triage, verify, persist). Finished carries the stage counts.
	KindStageStarted  Kind = "stage_started"
	KindStageFinished Kind = "stage_finished"
	// KindAgentStarted / KindAgentFinished bracket one finder or verifier agent
	// run. Finished carries tokens, duration, and any error.
	KindAgentStarted  Kind = "agent_started"
	KindAgentFinished Kind = "agent_finished"
	// KindSpendTick reports cumulative token spend as it accrues.
	KindSpendTick Kind = "spend_tick"
	// KindBudgetDegraded / KindBudgetStopped mirror the funnel's budget
	// degradation and hard-stop decisions (also surfaced on Result).
	KindBudgetDegraded Kind = "budget_degraded"
	KindBudgetStopped  Kind = "budget_stopped"
	// KindFindingVerified reports a candidate that survived adversarial
	// verification (a Tier-2 survivor).
	KindFindingVerified Kind = "finding_verified"
	// KindScanFinished marks the end of a funnel run and carries the stats
	// summary.
	KindScanFinished Kind = "scan_finished"
	// KindCycleScheduled reports the daemon's next poll/sweep deadlines.
	KindCycleScheduled Kind = "cycle_scheduled"
	// KindCycleStarted / KindCycleFinished bracket one daemon cycle.
	KindCycleStarted  Kind = "cycle_started"
	KindCycleFinished Kind = "cycle_finished"
	// KindReverify / KindPromote report post-cycle re-verification and
	// reproduction-promotion outcomes.
	KindReverify Kind = "reverify"
	KindPromote  Kind = "promote"
)

// Stage names the pipeline stage an event belongs to. Kept as plain strings so
// the funnel and any renderer agree on a stable vocabulary.
const (
	StageHypothesize = "hypothesize"
	StageTriage      = "triage"
	StageVerify      = "verify"
	StagePersist     = "persist"
)

// Role names the agent role an Agent event belongs to.
const (
	RoleFinder   = "finder"
	RoleVerifier = "verifier"
)

// Counts is the per-stage accounting carried on stage-finished events and the
// final summary. Fields mirror funnel.Stats but live here so progress does not
// import funnel (the funnel imports progress, not the reverse).
type Counts struct {
	Hypothesized int `json:"hypothesized,omitempty"`
	Triaged      int `json:"triaged,omitempty"`
	Verified     int `json:"verified,omitempty"`
	Killed       int `json:"killed,omitempty"`
}

// Event is one progress record. It is deliberately flat and JSON-serializable:
// a single struct with a Kind discriminator and a superset of fields, so sinks
// can switch on Kind and read only the fields that kind populates. Unused fields
// are zero and omitted from JSON.
//
// Time is set by Emit when zero, so callers never have to stamp events.
type Event struct {
	Kind Kind      `json:"kind"`
	Time time.Time `json:"time"`

	// ScanKind / Commit identify the run (scan_started, cycle_*). ScanKind is the
	// store scan kind string ("sweep"/"targeted"/"oneshot") or, for the daemon,
	// the cycle kind.
	ScanKind string `json:"scan_kind,omitempty"`
	Commit   string `json:"commit,omitempty"`

	// Stage / Counts describe a stage boundary.
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

	// File / Line / Title describe a verified finding.
	File  string `json:"file,omitempty"`
	Line  int    `json:"line,omitempty"`
	Title string `json:"title,omitempty"`

	// NextPoll / NextSweep carry the daemon schedule (cycle_scheduled).
	NextPoll  time.Time `json:"next_poll,omitempty"`
	NextSweep time.Time `json:"next_sweep,omitempty"`

	// Count is a generic integer payload: re-verified-closed count (reverify),
	// promoted count (promote), or new findings (cycle_finished).
	Count int `json:"count,omitempty"`

	// Message is a free-form human note (budget degradation reason, skip reason).
	Message string `json:"message,omitempty"`
}
