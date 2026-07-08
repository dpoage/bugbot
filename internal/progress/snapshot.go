package progress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
)

// StatusFileName is the file SnapshotSink writes (and `bugbot status` reads),
// living beside the state DB in the storage directory.
const StatusFileName = "status.json"

// snapshotInterval rate-limits status.json writes: at most one write per
// interval, plus a guaranteed final write on terminal events (scan/cycle
// finished) so the on-disk state is never stale by more than this between live
// updates and always settles correctly at the end.
const snapshotInterval = time.Second

// recentActionsCap is the maximum number of entries in AgentStatus.RecentActions.
// A ring of 8 entries covers a few turns of tool calls without consuming
// meaningful memory or making status.json noticeably larger.
const recentActionsCap = 8

// Status is the JSON document SnapshotSink persists and `bugbot status` reads.
// It is the cross-process view of a running scan or daemon: enough to tell, from
// another terminal, that Bugbot is alive and what it is doing.
type Status struct {
	// PID and LastUpdated are the staleness signals: a reader checks whether the
	// process still exists and how recently the file was touched.
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	LastUpdated time.Time `json:"last_updated"`

	// ScanKind / Commit identify the current run or cycle.
	ScanKind string `json:"scan_kind,omitempty"`
	Commit   string `json:"commit,omitempty"`

	// Stage is the current pipeline stage; ActiveAgents lists in-flight agents.
	Stage        string        `json:"stage,omitempty"`
	ActiveAgents []AgentStatus `json:"active_agents,omitempty"`

	// Counts is the running per-stage accounting.
	Counts Counts `json:"counts"`

	// Live counters tick per-event during an active stage so status shows
	// progress before the stage-finished event settles the final values.
	//
	//   LiveCandidates — incremented by each finder KindAgentFinished.Candidates
	//     during the hypothesize stage; reset to zero on KindStageFinished for
	//     hypothesize (the final Counts.Hypothesized takes over).
	//   LiveVerified / LiveKilled — incremented by KindFindingVerified /
	//     KindFindingKilled during the verify stage; reset to zero on
	//     KindStageFinished for verify.
	//
	// After stage-finish the live fields are zero and the Counts fields carry the
	// authoritative final values, so a reader always has a consistent picture.
	LiveCandidates int `json:"live_candidates,omitempty"`
	LiveTriaged    int `json:"live_triaged,omitempty"`
	LiveVerified   int `json:"live_verified,omitempty"`
	LiveKilled     int `json:"live_killed,omitempty"`

	// SpendInput / SpendOutput are cumulative tokens for the current run.
	// SpendInput includes cached tokens; SpendCacheRead is the subset served
	// from the provider's prompt cache (billed at a steep discount).
	SpendInput     int64 `json:"spend_input"`
	SpendOutput    int64 `json:"spend_output"`
	SpendCacheRead int64 `json:"spend_cache_read,omitempty"`

	// SpendTodayInput / SpendTodayOutput are the day's total spend, supplied by
	// the daemon on cycle boundaries (0 when the caller does not track it).
	SpendTodayInput  int64 `json:"spend_today_input,omitempty"`
	SpendTodayOutput int64 `json:"spend_today_output,omitempty"`

	// NextPoll / NextSweep / NextBacklog are the daemon's upcoming deadlines
	// (zero for a one-shot scan or when the backlog timer is disabled).
	NextPoll    time.Time `json:"next_poll,omitempty"`
	NextSweep   time.Time `json:"next_sweep,omitempty"`
	NextBacklog time.Time `json:"next_backlog,omitempty"`

	// LastEvent is a short human description of the most recent activity.
	LastEvent string `json:"last_event,omitempty"`

	// UnhealthyTools is the per-tool aggregation of KindToolUnhealthy events:
	// one entry per harness tool that has reported a failure, with the count of
	// failures, the worst severity seen, the latest reason, and the most recent
	// timestamp. Empty when the harness has been healthy. Materialized from the
	// sink's internal map at write time (see refreshUnhealthyTools); serialized
	// as part of Status automatically.
	UnhealthyTools []ToolHealth `json:"unhealthy_tools,omitempty"`
}

// AgentStatus is one in-flight agent in the snapshot.
type AgentStatus struct {
	Role  string `json:"role"`
	Label string `json:"label"`
	// AgentID is the run's unique identity (progress.AgentEventKey source),
	// empty when the accumulator folded events from a pre-identity emitter.
	// Included so status.json readers and Attach clients can key by run
	// identity instead of (Role, Label), which collides across concurrent
	// agents that share a label (e.g. duplicate finding titles).
	AgentID string    `json:"agent_id,omitempty"`
	Started time.Time `json:"started"`
	// Activity is the most recent short note about what this agent is doing,
	// derived from KindToolCall events via progress.Describe. Empty until the
	// first KindToolCall event arrives for this agent.
	Activity   string    `json:"activity,omitempty"`
	ActivityAt time.Time `json:"activity_at,omitempty"`
	// RecentActions is a bounded ring (cap recentActionsCap) of the most recent
	// Describe lines for this agent, newest-first. Populated from KindToolCall
	// Phase=start events so observers see what is happening as it starts.
	RecentActions []string `json:"recent_actions,omitempty"`
}

// ToolHealth is one entry in Status.UnhealthyTools, aggregating every
// KindToolUnhealthy event for a given harness tool. Severity is the maximum
// (most-severe) value seen across events for the tool; Count is the number of
// failures aggregated; Reason is the latest failure message; LastAt is the
// timestamp of the most recent failure.
type ToolHealth struct {
	Tool     string    `json:"tool"`
	Severity string    `json:"severity"`
	Reason   string    `json:"reason,omitempty"`
	Count    int       `json:"count"`
	LastAt   time.Time `json:"last_at"`
}

// StatusAccumulator folds progress Events into an in-memory Status. It holds
// exactly the state SnapshotSink used to keep private (the live-agent map,
// the per-tool health aggregation, and the running Status) so any consumer
// that needs the identical fold — not only the status.json writer — can
// reuse it verbatim instead of re-implementing apply()'s event/field matrix.
// internal/tui's Owner-mode LiveFeed is the other caller: it applies the
// same events SnapshotSink would and reads Snapshot() to build Frames,
// guaranteeing the cockpit and status.json never disagree about what an
// event means.
//
// Safe for concurrent use: parallel agents and the daemon emit from multiple
// goroutines; all state is guarded by mu.
type StatusAccumulator struct {
	mu        sync.Mutex
	st        Status
	agents    map[string]AgentStatus
	unhealthy map[string]ToolHealth // tool -> aggregated health
	spend     spendAggregator       // per-stream cumulative spend behind st.Spend*
}

// NewStatusAccumulator builds an empty accumulator, stamping the resulting
// Status with the current PID and start time so a reader can detect a
// dead/stale writer (mirrors NewSnapshotSink's stamp).
func NewStatusAccumulator() *StatusAccumulator {
	return &StatusAccumulator{
		agents:    make(map[string]AgentStatus),
		unhealthy: make(map[string]ToolHealth),
		st: Status{
			PID:       os.Getpid(),
			StartedAt: time.Now(),
		},
	}
}

// Apply folds ev into the accumulated status and reports whether it is a
// terminal event (scan/cycle finished). Safe for concurrent use.
func (a *StatusAccumulator) Apply(ev Event) (terminal bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apply(ev)
}

// Snapshot returns the current folded Status with ActiveAgents and
// UnhealthyTools materialized from the live maps. Safe for concurrent use;
// the returned value is a copy, so the caller may hold onto it freely.
func (a *StatusAccumulator) Snapshot() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshAgents()
	a.refreshUnhealthyTools()
	return a.st
}

// SnapshotSink maintains status.json: it folds each event into a
// StatusAccumulator and writes the result (atomically, temp+rename) on a
// rate-limited cadence and always on terminal events. Writes are
// best-effort — a failed write is dropped, never surfaced — so the sink
// honors the non-blocking, never-fail contract.
//
// Safe for concurrent use: parallel agents and the daemon emit from multiple
// goroutines; all state is guarded by acc (accumulation) and mu (write
// scheduling).
type SnapshotSink struct {
	path string
	acc  *StatusAccumulator

	mu        sync.Mutex
	lastWrite time.Time
	now       func() time.Time // injectable for tests

	// daySpend lets the daemon inject today's totals (read at write time) without
	// threading them through every event.
	daySpend func() (in, out int64)
}

// NewSnapshotSink writes status.json into storageDir (the directory holding
// state.db). It stamps the snapshot with the current PID and start time so a
// reader can detect a dead/stale writer.
func NewSnapshotSink(storageDir string) *SnapshotSink {
	return &SnapshotSink{
		path: filepath.Join(storageDir, StatusFileName),
		acc:  NewStatusAccumulator(),
		now:  time.Now,
	}
}

// StatusPath returns the status.json path for a given storage directory, the
// same path NewSnapshotSink writes. `bugbot status` uses it to locate the file.
func StatusPath(storageDir string) string {
	return filepath.Join(storageDir, StatusFileName)
}

// WithDaySpend registers a getter the sink calls at each write to fill in the
// day's total spend (the daemon supplies store totals). Returns the sink for
// chaining.
func (s *SnapshotSink) WithDaySpend(fn func() (in, out int64)) *SnapshotSink {
	s.mu.Lock()
	s.daySpend = fn
	s.mu.Unlock()
	return s
}

// Handle implements Sink.
func (s *SnapshotSink) Handle(ev Event) {
	terminal := s.acc.Apply(ev)

	s.mu.Lock()
	now := s.now()
	due := terminal || s.lastWrite.IsZero() || now.Sub(s.lastWrite) >= snapshotInterval
	if !due {
		s.mu.Unlock()
		return
	}
	s.lastWrite = now
	daySpend := s.daySpend
	s.mu.Unlock()

	snap := s.acc.Snapshot()
	snap.LastUpdated = now
	if daySpend != nil {
		snap.SpendTodayInput, snap.SpendTodayOutput = daySpend()
	}
	writeStatusAtomic(s.path, snap)
}

// apply folds an event into the in-memory status and reports whether it is a
// terminal event that must force an immediate write. Caller holds mu.
func (a *StatusAccumulator) apply(ev Event) (terminal bool) {
	s := a
	switch ev.Kind {
	case KindScanStarted, KindCycleStarted:
		s.st.ScanKind = ev.ScanKind
		s.st.Commit = ev.Commit
		// Reset live counters at scan start, not only at stage finish: an
		// aborted scan returns without emitting StageFinished, and the daemon
		// reuses one accumulator across cycles — without this reset the next
		// cycle's status would show the dead run's "candidates so far" until
		// its own stage completes.
		s.st.LiveCandidates = 0
		s.st.LiveTriaged = 0
		s.st.LiveVerified = 0
		s.st.LiveKilled = 0
		s.st.LastEvent = "started " + ev.ScanKind
	case KindStageStarted:
		s.st.Stage = ev.Stage
		s.st.LastEvent = "stage: " + ev.Stage
	case KindStageFinished:
		if ev.Counts != nil {
			s.st.Counts = mergeMax(s.st.Counts, *ev.Counts)
		}
		// Reset live counters now that the stage-finished values are authoritative.
		switch ev.Stage {
		case StageHypothesize:
			s.st.LiveCandidates = 0
		case StageTriage:
			s.st.LiveTriaged = 0
		case StageVerify:
			s.st.LiveVerified = 0
			s.st.LiveKilled = 0
		}
		s.st.LastEvent = "stage done: " + ev.Stage
	case KindAgentStarted:
		s.agents[AgentEventKey(ev)] = AgentStatus{
			Role: ev.Role, Label: ev.Label, AgentID: ev.AgentID, Started: ev.Time,
		}
	case KindAgentFinished:
		delete(s.agents, AgentEventKey(ev))
		if ev.Role == RoleFinder && ev.Candidates > 0 {
			s.st.LiveCandidates += ev.Candidates
		}
		s.st.LastEvent = ev.Role + " done: " + ev.Label
	case KindToolCall:
		// Update the activity note in-place only when the agent is already
		// tracked. A stray/late event must not resurrect a finished agent.
		if a, ok := s.agents[AgentEventKey(ev)]; ok {
			line := Describe(ev)
			a.Activity = line
			a.ActivityAt = ev.Time
			// Maintain a bounded ring of recent actions (Phase=start only, so
			// the observer sees the action as it begins rather than doubled).
			if ev.Phase == "start" {
				a.RecentActions = pushRing(a.RecentActions, line, recentActionsCap)
			}
			s.agents[AgentEventKey(ev)] = a
		}
	case KindReproAttempt:
		// Same fold as KindToolCall: surface the round as the tracked agent's
		// activity note (a reader of `bugbot status` sees round progress the
		// same way it sees tool-call activity), plus LastEvent so it is visible
		// even once the agent has finished and been removed.
		note := fmt.Sprintf("attempt %d/%d: %s", ev.Attempt, ev.MaxAttempts, ev.Verdict)
		if a, ok := s.agents[AgentEventKey(ev)]; ok {
			a.Activity = note
			a.ActivityAt = ev.Time
			s.agents[AgentEventKey(ev)] = a
		}
		s.st.LastEvent = "repro " + note + " — " + ev.Label
	case KindSpendTick:
		s.spend.tick(ev.Role, ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens)
		s.st.SpendInput, s.st.SpendOutput, s.st.SpendCacheRead = s.spend.totals()
	case KindFindingVerified:
		s.st.Counts.Verified++
		s.st.LiveVerified++
		s.st.LastEvent = "verified: " + ev.Title
	case KindCandidateTriaged:
		s.st.LiveTriaged++
		s.st.LastEvent = "triaged: " + ev.Title
	case KindFindingKilled:
		s.st.LiveKilled++
		s.st.LastEvent = "killed: " + ev.Title
	case KindBudgetDegraded:
		s.st.LastEvent = "budget degraded"
	case KindBudgetStopped:
		s.st.LastEvent = "budget stopped"
	case KindToolUnhealthy:
		s.applyToolUnhealthy(ev)
		s.st.LastEvent = "tool unhealthy: " + ev.Tool + " (" + ev.Severity + ")"
	case KindCycleScheduled:
		s.st.NextPoll = ev.NextPoll
		s.st.NextSweep = ev.NextSweep
		s.st.NextBacklog = ev.NextBacklog
	case KindScanFinished, KindCycleFinished:
		if ev.Counts != nil {
			s.st.Counts = mergeMax(s.st.Counts, *ev.Counts)
		}
		if ev.InputTokens > 0 || ev.OutputTokens > 0 {
			// Final totals come from the funnel's recorder — update that stream
			// only, so repro spend ticked under RoleReproducer survives.
			s.spend.tick("", ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens)
			s.st.SpendInput, s.st.SpendOutput, s.st.SpendCacheRead = s.spend.totals()
		}
		s.st.Stage = ""
		// Belt-and-suspenders: live counters are reset per stage and on scan
		// start; clearing on finish too means an idle daemon never shows live
		// remnants between runs.
		s.st.LiveCandidates = 0
		s.st.LiveTriaged = 0
		s.st.LiveVerified = 0
		s.st.LiveKilled = 0
		s.st.LastEvent = "finished " + ev.ScanKind
		return true
	}
	return false
}

// applyToolUnhealthy folds one KindToolUnhealthy event into the per-tool
// aggregation map. Caller holds mu.
//
// Aggregation semantics: Count increments on every event for the same Tool;
// Reason is overwritten with the latest Message; LastAt is overwritten with the
// latest Time; Severity keeps the MAX rank seen across events for the tool. On
// a severity parse failure the new value is dropped and the existing entry's
// Severity is preserved (unrecognized strings cannot rank, so there is no
// meaningful max to compute).
func (s *StatusAccumulator) applyToolUnhealthy(ev Event) {
	cur, ok := s.unhealthy[ev.Tool]
	if !ok {
		cur = ToolHealth{Tool: ev.Tool}
	}
	cur.Count++
	cur.Reason = ev.Message
	cur.LastAt = ev.Time

	if sev, parsed := domain.ParseSeverity(ev.Severity); parsed {
		if !ok || sev.Rank() > domain.Severity(cur.Severity).Rank() {
			cur.Severity = sev.String()
		}
	} else if !ok {
		// First sighting AND unparseable: keep the raw token so the operator
		// can still see what the runner reported. Subsequent unparseable
		// events do not clobber an already-valid severity.
		cur.Severity = ev.Severity
	}
	s.unhealthy[ev.Tool] = cur
}

// refreshAgents rebuilds the sorted ActiveAgents slice from the live map.
func (s *StatusAccumulator) refreshAgents() {
	keys := make([]string, 0, len(s.agents))
	for k := range s.agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AgentStatus, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.agents[k])
	}
	s.st.ActiveAgents = out
}

// refreshUnhealthyTools materializes the sorted Status.UnhealthyTools slice from
// the live unhealthy map. Sorting by Tool keeps the JSON output stable so a
// reader's diff against the previous snapshot shows only real changes.
func (s *StatusAccumulator) refreshUnhealthyTools() {
	if len(s.unhealthy) == 0 {
		s.st.UnhealthyTools = nil
		return
	}
	keys := make([]string, 0, len(s.unhealthy))
	for k := range s.unhealthy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ToolHealth, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.unhealthy[k])
	}
	s.st.UnhealthyTools = out
}

// writeStatusAtomic marshals st and writes it to path via a temp file + rename,
// so a reader never observes a partially-written document. Best-effort: any
// error (including a missing directory) is swallowed to honor the never-fail
// contract.
func writeStatusAtomic(path string, st Status) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".status-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// ReadStatus reads and parses status.json at path. It returns os.ErrNotExist
// (via the underlying read) when the file is absent, which `bugbot status`
// treats as "no activity recorded".
func ReadStatus(path string) (Status, error) {
	var st Status
	data, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// mergeMax returns the element-wise maximum of two Counts, so a later stage's
// partial counts never lower an earlier-set value.
func mergeMax(a, b Counts) Counts {
	if b.Hypothesized > a.Hypothesized {
		a.Hypothesized = b.Hypothesized
	}
	if b.Triaged > a.Triaged {
		a.Triaged = b.Triaged
	}
	if b.Verified > a.Verified {
		a.Verified = b.Verified
	}
	if b.Killed > a.Killed {
		a.Killed = b.Killed
	}
	return a
}

// pushRing prepends item to ring and trims it to at most maxLen entries. The
// result is newest-first: index 0 is the most recent action.
func pushRing(ring []string, item string, maxLen int) []string {
	ring = append([]string{item}, ring...)
	if len(ring) > maxLen {
		ring = ring[:maxLen]
	}
	return ring
}
