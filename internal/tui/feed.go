// Package tui implements Bugbot's agent-first terminal cockpit: a read-only
// (Observer) or, in a future child, dispatch-enabled (Owner) full-screen view
// of a scan/daemon's live activity and accumulated world state.
//
// The package is split along one seam: Feed produces Frame values however it
// likes (a ticker over status.json + the store for Observer; an in-process
// event channel for the future Owner-mode LiveFeed); Model is a pure
// bubbletea reducer over Frame and key input that does not know or care which
// Feed implementation is driving it.
package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// Mode distinguishes who holds the store's writer lock. Owner is a cockpit
// that also runs (or will run) dispatch against an in-process engine;
// Observer is strictly read-only because a live scan/daemon elsewhere holds
// the writer lock (internal/store/dblock.go). The Model's screens and reducer
// are IDENTICAL in both modes — only the Feed implementation differs.
type Mode int

const (
	// Owner means this process holds (or can take) the writer lock. No
	// implementation exists yet in this package; it is reserved for a future
	// LiveFeed + dispatch child.
	Owner Mode = iota
	// Observer means this process opened the store read-only because another
	// process holds the writer lock, or because the operator explicitly asked
	// for a read-only view.
	Observer
)

// String renders the mode for header/status display.
func (m Mode) String() string {
	if m == Owner {
		return "owner"
	}
	return "observer"
}

// Feed is the seam between Model and however frames get produced.
//
// SnapshotFeed (this package) is the Observer implementation: Next returns a
// ~1s ticker that re-reads status.json and the store's world-state helpers.
// A future LiveFeed (Owner mode) will instead select on a live
// progress.EventSink channel fed directly by an in-process funnel/daemon —
// same interface, so plugging it in requires NO change to Model, Update, or
// View: only a different Feed value passed to Run/NewModel.
//
// Next MUST be safe to call repeatedly and cheaply: the Model calls it once
// at Init and again every time it finishes handling the tea.Msg the previous
// call's tea.Cmd resolved to, so a Feed drives its own cadence (a ticker for
// SnapshotFeed; a blocking channel receive for the future LiveFeed).
// Implementations must not block Next itself — only the returned tea.Cmd may
// block/sleep, per bubbletea's command contract.
type Feed interface {
	// Next returns a tea.Cmd that resolves to a tea.Msg (a FrameMsg in every
	// implementation so far) once the feed has something new to report.
	Next() tea.Cmd
	// Mode reports which mode produced this feed, so the Model can decide
	// whether dispatch-only affordances make sense (Observer never shows
	// them).
	Mode() Mode
	// Close releases any resources the feed owns (e.g. a read-only store
	// handle). Called once by Run after the tea.Program returns.
	Close() error
}

// Compile-time assertion that SnapshotFeed satisfies Feed.
var _ Feed = (*SnapshotFeed)(nil)

// leadPreviewMax bounds the pending-lead preview in a Frame; the full list is
// reachable from the Leads screen.
const leadPreviewMax = 3

// staleAfter mirrors internal/cli/status.go's staleAfter: how long without a
// status.json update before a running scan/daemon is treated as dead.
const staleAfter = 2 * time.Minute

// Frame is one rendered snapshot of the world, as understood by whichever
// Feed produced it. Every field is best-effort: a Feed that cannot reach a
// data source degrades that section to its zero value rather than failing —
// the Model always has something to render.
type Frame struct {
	// Snapshot is the live activity snapshot (progress.Status), when one
	// could be read. HasSnapshot is false when no scan/daemon has ever
	// written status.json in this storage directory.
	Snapshot    progress.Status
	HasSnapshot bool
	// Stale reports that Snapshot looks dead (old LastUpdated or a gone
	// PID) — the same rule `bugbot status` uses (internal/cli/status.go
	// isStale). A stale or absent snapshot means the Cockpit renders the
	// "idle" static view: world-state only, no live agents.
	Stale bool

	World WorldState

	// Agents is live Status.ActiveAgents merged with historical
	// store.AgentUnit rows for World.LastRun, sorted by Started ascending
	// (launch order reads top-to-bottom). Empty when no store is available.
	Agents []AgentView

	// ActionRows is the per-agent structured tool-call ring, populated by
	// LiveFeed (Owner mode) from KindToolCall events. Empty in SnapshotFeed
	// (Observer) mode — that mode uses AgentView.RecentActions instead.
	// Keyed by agentFeedKey(role, label) -> ordered rows (oldest first).
	ActionRows map[string][]ActionRow
}

// FrameMsg is the tea.Msg a Feed resolves Next()'s tea.Cmd to when it has a
// new Frame ready.
type FrameMsg Frame

// WorldState is the accumulated (as opposed to live) picture of a bugbot
// installation: what stands found, what needs a human, the blackboard, what
// is synced to GitHub, and today's spend.
//
// This intentionally mirrors cli.worldState (internal/cli/worldstate.go):
// package cli is not importable from internal/tui, and its fetch/render
// helpers are being relocated to internal/engine by a sibling change, so this
// type is gathered directly from the store here rather than shared.
type WorldState struct {
	Tallies    domain.FindingTallies
	HasTallies bool

	// PendingLeads is newest-first, capped at leadPreviewMax; PendingLeadsTotal
	// is the true count on the blackboard.
	PendingLeads      []store.Lead
	PendingLeadsTotal int

	Published map[store.IssueState]int // empty = never published

	DaySpend    store.SpendTotals
	HasDaySpend bool
	// DayBudgetLimit is budgets.per_day_tokens (0 = unlimited).
	DayBudgetLimit int64
	// CacheReadWeight is budgets.cache_read_weight, needed to compute the same
	// chargeable-token percentage the day-budget gate uses.
	CacheReadWeight float64

	LastRun    store.ScanRun
	HasLastRun bool
}

// AgentView is one row in the merged agent list: either a live in-flight
// agent (Live=true, sourced from progress.Status.ActiveAgents) or a finished
// historical unit (sourced from store.AgentUnit for the latest scan run).
// agent_units rows are written synchronously the instant a unit finishes,
// while the on-disk status.json snapshot is rate-limited to at most one
// write per second (progress.snapshotInterval) — so a live entry and its
// eventual historical row MAY briefly coexist for up to one snapshot
// interval after the unit finishes, until the next status.json write drops
// it from ActiveAgents. The merge is a plain concatenation with no dedup
// key; the window is read-only and cosmetic (spend/tallies come from the
// ledger and findings table, never from agent rows, so nothing is
// double-counted).
type AgentView struct {
	Role     string
	Label    string // display label: live Status.Label, or "lens[/strategy]" historically
	Lens     string
	Strategy string

	Live bool

	// UnitID is store.AgentUnit.ID for a historical entry, empty for a live
	// one. Combined with Role/Label/Started it is the stable identity a
	// drilled-in agent is tracked by (see agentKey in merge.go), so a 1s
	// frame refresh can never silently swap the agent behind the detail
	// screen.
	UnitID string

	Started    time.Time
	FinishedAt time.Time // zero while live or for skipped units

	Activity   string // live only; most recent short activity note
	ActivityAt time.Time

	Status string // historical only (store.AgentStatus); "" while live

	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	Candidates      int
	LeadsPosted     int
	Detail          string
	Files           []string

	// TranscriptPath is a best-effort discovered JSONL transcript for this
	// unit (see discoverTranscript). Empty means none found: most agents
	// never get a transcript, since store.Repro.TranscriptDir only covers
	// reproducer/patch-prover units.
	TranscriptPath string

	// RecentActions is the Observer-mode bounded ring of recent Describe
	// strings for this agent (newest-first), sourced from
	// AgentStatus.RecentActions in status.json. Empty in Owner mode (use
	// Frame.ActionRows instead).
	RecentActions []string
}
