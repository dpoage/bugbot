package progress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// StatusFileName is the file SnapshotSink writes (and `bugbot status` reads),
// living beside the state DB in the storage directory.
const StatusFileName = "status.json"

// snapshotInterval rate-limits status.json writes: at most one write per
// interval, plus a guaranteed final write on terminal events (scan/cycle
// finished) so the on-disk state is never stale by more than this between live
// updates and always settles correctly at the end.
const snapshotInterval = time.Second

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
}

// AgentStatus is one in-flight agent in the snapshot.
type AgentStatus struct {
	Role    string    `json:"role"`
	Label   string    `json:"label"`
	Started time.Time `json:"started"`
}

// SnapshotSink maintains status.json: it folds each event into an in-memory
// Status and writes it (atomically, temp+rename) on a rate-limited cadence and
// always on terminal events. Writes are best-effort — a failed write is dropped,
// never surfaced — so the sink honors the non-blocking, never-fail contract.
//
// Safe for concurrent use: parallel agents and the daemon emit from multiple
// goroutines; all state is guarded by mu.
type SnapshotSink struct {
	path string

	mu        sync.Mutex
	st        Status
	agents    map[string]AgentStatus
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
	now := time.Now()
	return &SnapshotSink{
		path:   filepath.Join(storageDir, StatusFileName),
		agents: make(map[string]AgentStatus),
		now:    time.Now,
		st: Status{
			PID:       os.Getpid(),
			StartedAt: now,
		},
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
	s.mu.Lock()
	terminal := s.apply(ev)
	now := s.now()
	due := terminal || s.lastWrite.IsZero() || now.Sub(s.lastWrite) >= snapshotInterval
	if due {
		s.lastWrite = now
		s.st.LastUpdated = now
		s.refreshAgents()
		if s.daySpend != nil {
			s.st.SpendTodayInput, s.st.SpendTodayOutput = s.daySpend()
		}
		snap := s.st // copy under lock
		s.mu.Unlock()
		writeStatusAtomic(s.path, snap)
		return
	}
	s.mu.Unlock()
}

// apply folds an event into the in-memory status and reports whether it is a
// terminal event that must force an immediate write. Caller holds mu.
func (s *SnapshotSink) apply(ev Event) (terminal bool) {
	switch ev.Kind {
	case KindScanStarted, KindCycleStarted:
		s.st.ScanKind = ev.ScanKind
		s.st.Commit = ev.Commit
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
		case StageVerify:
			s.st.LiveVerified = 0
			s.st.LiveKilled = 0
		}
		s.st.LastEvent = "stage done: " + ev.Stage
	case KindAgentStarted:
		s.agents[agentKey(ev.Role, ev.Label)] = AgentStatus{
			Role: ev.Role, Label: ev.Label, Started: ev.Time,
		}
	case KindAgentFinished:
		delete(s.agents, agentKey(ev.Role, ev.Label))
		if ev.Role == RoleFinder && ev.Candidates > 0 {
			s.st.LiveCandidates += ev.Candidates
		}
		s.st.LastEvent = ev.Role + " done: " + ev.Label
	case KindSpendTick:
		s.st.SpendInput = ev.InputTokens
		s.st.SpendOutput = ev.OutputTokens
		s.st.SpendCacheRead = ev.CacheReadTokens
	case KindFindingVerified:
		s.st.Counts.Verified++
		s.st.LiveVerified++
		s.st.LastEvent = "verified: " + ev.Title
	case KindFindingKilled:
		s.st.LiveKilled++
		s.st.LastEvent = "killed: " + ev.Title
	case KindBudgetDegraded:
		s.st.LastEvent = "budget degraded"
	case KindBudgetStopped:
		s.st.LastEvent = "budget stopped"
	case KindCycleScheduled:
		s.st.NextPoll = ev.NextPoll
		s.st.NextSweep = ev.NextSweep
		s.st.NextBacklog = ev.NextBacklog
	case KindScanFinished, KindCycleFinished:
		if ev.Counts != nil {
			s.st.Counts = mergeMax(s.st.Counts, *ev.Counts)
		}
		if ev.InputTokens > 0 || ev.OutputTokens > 0 {
			s.st.SpendInput = ev.InputTokens
			s.st.SpendOutput = ev.OutputTokens
			s.st.SpendCacheRead = ev.CacheReadTokens
		}
		s.st.Stage = ""
		s.st.LastEvent = "finished " + ev.ScanKind
		return true
	}
	return false
}

// refreshAgents rebuilds the sorted ActiveAgents slice from the live map.
func (s *SnapshotSink) refreshAgents() {
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
