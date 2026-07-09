// Package control implements Bugbot daemon's optional IPC control socket: a
// Unix domain socket that lets a separately-running `bugbot tui` ATTACH to a
// live daemon, streaming its progress events and dispatching scan/verify/
// repro/sweep verbs, instead of degrading to a read-only Observer view of
// status.json (see internal/tui's Feed doc comments — this is the third Feed
// implementation the seam was designed for).
//
// # Transport
//
// A Unix domain socket at a config-provided path (default:
// "<storage-dir>/daemon.sock", next to status.json), permissions 0600. The
// daemon unlinks a stale socket file on startup and removes it on shutdown.
// The socket is disabled by default: no config knob set, no listener, zero
// behavior change (see config.ControlSocket).
//
// # Wire protocol
//
// Newline-delimited JSON in both directions over the same connection, each
// frame/request carrying a "v" protocol version (always 1 today — a future
// incompatible change bumps this so either side can refuse cleanly instead
// of misparsing).
//
//   - Server -> client: Frame values. FrameKindEvent carries one
//     progress.Event (the same stream LiveFeed folds via progress.EventSink);
//     FrameKindStatus carries a periodic progress.Status snapshot (folded
//     from the same event stream, mirroring LiveFeed's StatusAccumulator) so
//     a client that only just connected still gets a coherent picture rather
//     than waiting for the next event; FrameKindReply carries a
//     DispatchReply answering an earlier client Request by ID.
//   - Client -> server: Request values, {v, id, verb, opts}. The server
//     enqueues the verb for the daemon's scheduler to run at the next cycle
//     boundary (never concurrently with an in-flight cycle — see
//     internal/daemon's dispatch queue) and replies once the verb has run to
//     completion (not merely accepted): the reply's Summary is only known
//     once the verb finishes, and this matches how the in-process dispatch
//     palette already behaves (it blocks until Scan/Verify/Repro/Sweep
//     return). See bugbot-2p8z.4's --design notes for the full rationale.
//
// # Backpressure
//
// The server never blocks or fails the daemon's scheduler loop for a slow,
// stuck, or absent client: each client connection owns a bounded, buffered
// outbound channel: fanning out an event/status frame is a non-blocking
// send that silently drops the frame when the channel is full (mirroring
// internal/tui's LiveFeed drop-on-full wakeup policy). A dispatch Reply is
// sent through the same channel and can be dropped the same way if the
// client is sufficiently stalled — that client simply never learns the
// verb completed; the verb itself still ran and the daemon's own state
// (store, status.json) reflects it.
package control

import (
	"github.com/dpoage/bugbot/internal/progress"
)

// ProtocolVersion is the current wire protocol version. Every Frame and
// Request carries it so either side can detect a future incompatible
// change and refuse cleanly instead of misparsing.
const ProtocolVersion = 1

// Verb identifies one dispatchable RPC verb. The set is intentionally
// table-driven (see Verbs) rather than an enum-with-switch scattered across
// the codebase, so a future verb (e.g. sibling bugbot-2p8z.5's review) is
// additive: append to Verbs and add one case in the daemon-side dispatch
// executor (internal/engine's DispatchVerb) without touching the transport.
type Verb string

const (
	VerbScan      Verb = "scan"
	VerbVerify    Verb = "verify"
	VerbRepro     Verb = "repro"
	VerbSweep     Verb = "sweep"
	VerbReconcile Verb = "reconcile"
)

// Verbs lists every verb this build of the protocol accepts, in stable
// order. Table-driven per bugbot-2p8z.4's scope pin: review (bugbot-2p8z.5)
// is deliberately NOT included here — it lands as an additive entry once
// its engine-side extraction exists. VerbReconcile (bugbot-7bjl) is the
// on-demand trigger for the backlog-reconcile dedup pass landed timer-only
// in bugbot-ezmx.4.
var Verbs = []Verb{VerbScan, VerbVerify, VerbRepro, VerbSweep, VerbReconcile}

// Known reports whether v is one of Verbs.
func (v Verb) Known() bool {
	for _, k := range Verbs {
		if k == v {
			return true
		}
	}
	return false
}

// DispatchOpts is the flattened, wire-serializable subset of the four
// verbs' engine.*Opts structs that makes sense to accept over the socket.
// Fields not relevant to a given verb are simply ignored by the executor
// (mirrors how the CLI's per-command flags only ever populate the fields
// that command cares about). Out/ErrOut writers are deliberately absent:
// they are process-local (and, on the daemon side, would violate the
// "never write to stdout/stderr while an alt-screen TUI is active"
// invariant on the CLIENT anyway) — dispatch output is summarized, not
// streamed as raw text.
type DispatchOpts struct {
	Target    string `json:"target,omitempty"`
	Since     string `json:"since,omitempty"`
	Force     bool   `json:"force,omitempty"`
	Suspected bool   `json:"suspected,omitempty"` // verify only
	MaxN      int    `json:"max_n,omitempty"`     // repro only
	Cap       int    `json:"cap,omitempty"`       // reconcile only: dedup-arbiter invocation cap, <=0 means funnel.DefaultReconcileCap
}

// Request is a client->server dispatch RPC. ID correlates the eventual
// Reply; the client picks it (e.g. a monotonic counter or random string)
// and must not reuse an in-flight ID.
type Request struct {
	V    int          `json:"v"`
	ID   string       `json:"id"`
	Verb Verb         `json:"verb"`
	Opts DispatchOpts `json:"opts"`
}

// DispatchSummary is the structured, reduced result of a completed dispatch
// verb — NOT a serialized engine.*Result (those carry full funnel.Result/
// repro.Summary trees that are neither meant to cross a process boundary
// nor needed by the palette, which only ever renders a short count-based
// summary line). The fields cover exactly what internal/tui's existing
// scan/verify/repro/sweepSummary helpers need to render the identical text
// Owner mode shows for an in-process dispatch.
type DispatchSummary struct {
	// FindingCount is populated for scan and sweep (len of the resulting
	// funnel.Result.Findings).
	FindingCount int `json:"finding_count,omitempty"`
	// HasResult distinguishes "ran, found nothing" (true, FindingCount=0)
	// from "nothing to do, no result section at all" (false) for sweep;
	// scan always sets it true (a scan always has a Result unless the
	// server-side verb call itself errored).
	HasResult bool `json:"has_result,omitempty"`
	// HasDrain / drain semantics for verify: mirrors HasResult but for
	// VerifyResult.Drain.
	HasDrain bool `json:"has_drain,omitempty"`
	// Attempted / HasSummary / Skipped are repro-only, mirroring
	// engine.ReproResult's Summary/Skipped fields.
	Attempted  int    `json:"attempted,omitempty"`
	HasSummary bool   `json:"has_summary,omitempty"`
	Skipped    string `json:"skipped,omitempty"`
	// ReconcileNominated/Arbitrated/Merged/SkippedCap are reconcile-only,
	// mirroring funnel.Stats' Reconcile* counters (the reduced-counts
	// philosophy applied to the same fields the timer path already
	// persists in the scan run's Stats JSON).
	ReconcileNominated  int `json:"reconcile_nominated,omitempty"`
	ReconcileArbitrated int `json:"reconcile_arbitrated,omitempty"`
	ReconcileMerged     int `json:"reconcile_merged,omitempty"`
	ReconcileSkippedCap int `json:"reconcile_skipped_cap,omitempty"`
}

// DispatchReply is a server->client answer to a Request with a matching ID.
// Exactly one of Summary (OK=true) or Error (OK=false) is meaningful.
type DispatchReply struct {
	ID      string           `json:"id"`
	OK      bool             `json:"ok"`
	Summary *DispatchSummary `json:"summary,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// FrameKind discriminates a server->client Frame's payload.
type FrameKind string

const (
	FrameKindEvent  FrameKind = "event"
	FrameKindStatus FrameKind = "status"
	FrameKindReply  FrameKind = "reply"
)

// Frame is one server->client message. Exactly one payload field is set,
// matching Kind.
type Frame struct {
	V      int              `json:"v"`
	Kind   FrameKind        `json:"kind"`
	Event  *progress.Event  `json:"event,omitempty"`
	Status *progress.Status `json:"status,omitempty"`
	Reply  *DispatchReply   `json:"reply,omitempty"`
}
