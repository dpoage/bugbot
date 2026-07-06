package progress

import (
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/dpoage/bugbot/internal/util"
)

// LogRenderer writes one plain line per significant event. It is the non-TTY
// fallback for `bugbot scan` (piped stdout) and, via NewSlogRenderer, the
// daemon's bridge from progress events to its existing structured logger.
//
// "Significant" deliberately excludes the high-frequency, low-information events
// (agent_started, spend_tick, stage_started): logging every one would bury the
// signal. The renderer keeps the running spend internally and prints it on the
// events that matter (agent finish, stage finish, summary), so a tail of the log
// still shows live progress without a flood.
//
// LogRenderer satisfies the Sink non-blocking contract: Handle takes a short
// mutex and does one formatted write. It is safe for concurrent use.
type LogRenderer struct {
	mu  sync.Mutex
	out io.Writer
	// log is set for the slog bridge; when non-nil, events are emitted as slog
	// records instead of plain lines (out is then ignored).
	log *slog.Logger
}

// NewLogRenderer writes plain lines to out.
func NewLogRenderer(out io.Writer) *LogRenderer {
	return &LogRenderer{out: out}
}

// NewSlogRenderer bridges events onto an existing slog.Logger, used by the
// daemon so its progress events flow through the same structured handler as its
// cycle logs. A nil logger falls back to slog.Default().
func NewSlogRenderer(log *slog.Logger) *LogRenderer {
	if log == nil {
		log = slog.Default()
	}
	return &LogRenderer{log: log}
}

// Handle implements Sink.
func (r *LogRenderer) Handle(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.log != nil {
		r.handleSlog(ev)
		return
	}
	if line := r.line(ev); line != "" {
		_, _ = fmt.Fprintln(r.out, line)
	}
}

// line renders the plain-text form of an event, or "" for events not worth a
// line.
func (r *LogRenderer) line(ev Event) string {
	switch ev.Kind {
	case KindScanStarted:
		return fmt.Sprintf("scan started: kind=%s commit=%s", ev.ScanKind, util.ShortSHA(ev.Commit))
	case KindStageStarted:
		return fmt.Sprintf("stage: %s", ev.Stage)
	case KindStageFinished:
		return fmt.Sprintf("stage done: %s%s", ev.Stage, countsSuffix(ev.Counts))
	case KindAgentFinished:
		return fmt.Sprintf("agent done: %s [%s] tokens=%d dur=%s%s",
			ev.Role, ev.Label, ev.Tokens, ev.Duration.Round(timeRound), errSuffix(ev.Err))
	case KindFindingVerified:
		return fmt.Sprintf("verified: %s (%s:%d)", ev.Title, ev.File, ev.Line)
	case KindFindingKilled:
		return fmt.Sprintf("killed: %s (%s:%d)", ev.Title, ev.File, ev.Line)
	case KindBudgetDegraded:
		return "budget degraded: " + ev.Message
	case KindBudgetStopped:
		return "budget stopped: " + ev.Message
	case KindLensFailed:
		return "LENS FAILED: " + ev.Message
	case KindAgentActivity:
		// Low-volume roles only: the funnel's finder/verifier agents run in
		// large parallel batches and would flood the log with one line per
		// tool-call turn. Reproducer and patch-prover run at most a couple at a
		// time (one per finding, bounded parallelism), so their activity is
		// worth a line.
		if ev.Role != RoleReproducer && ev.Role != RolePatchProver {
			return ""
		}
		return fmt.Sprintf("agent: %s [%s] %s", ev.Role, ev.Label, ev.Activity)
	case KindReproAttempt:
		return fmt.Sprintf("repro attempt %d/%d: %s dur=%s — %s",
			ev.Attempt, ev.MaxAttempts, ev.Verdict, ev.Duration.Round(timeRound), ev.Label)
	case KindToolUnhealthy:
		return "TOOL UNHEALTHY: " + ev.Tool + " (" + ev.Severity + "): " + ev.Message
	case KindSpendTick:
		// Spend ticks are noisy; suppressed in the plain log. Summary/agent lines
		// carry the totals.
		return ""
	case KindHeatOrdered:
		return fmt.Sprintf("heat ordering applied: heat_files=%d top5=%s", ev.Count, ev.Label)
	case KindScanFinished:
		return fmt.Sprintf("scan finished: kind=%s commit=%s%s spend in=%d out=%d%s",
			ev.ScanKind, util.ShortSHA(ev.Commit), countsSuffix(ev.Counts),
			ev.InputTokens, ev.OutputTokens, cachedSuffix(ev.CacheReadTokens))
	case KindCycleScheduled:
		line := fmt.Sprintf("schedule: next_poll=%s next_sweep=%s",
			ev.NextPoll.Format(timeClock), ev.NextSweep.Format(timeClock))
		if !ev.NextBacklog.IsZero() {
			line += " next_backlog=" + ev.NextBacklog.Format(timeClock)
		}
		return line
	case KindCycleStarted:
		return fmt.Sprintf("cycle started: kind=%s", ev.ScanKind)
	case KindCycleFinished:
		return fmt.Sprintf("cycle finished: kind=%s new=%d%s spend in=%d out=%d%s",
			ev.ScanKind, ev.Count, countsSuffix(ev.Counts), ev.InputTokens, ev.OutputTokens,
			cachedSuffix(ev.CacheReadTokens))
	case KindReverify:
		if ev.Count == 0 {
			return ""
		}
		return fmt.Sprintf("re-verify: %d finding(s) auto-closed", ev.Count)
	case KindPromote:
		if ev.Count == 0 {
			return ""
		}
		return fmt.Sprintf("promote: %d finding(s) promoted to T1", ev.Count)
	default:
		return ""
	}
}

// handleSlog emits the event as a structured record on the daemon's logger. It
// mirrors the plain-line significance filter: only events worth a record are
// emitted, keyed by a stable msg so they group with the daemon's own logs.
func (r *LogRenderer) handleSlog(ev Event) {
	switch ev.Kind {
	case KindStageFinished:
		r.log.Info("progress: stage", "stage", ev.Stage,
			"hypothesized", count(ev.Counts).Hypothesized,
			"triaged", count(ev.Counts).Triaged,
			"verified", count(ev.Counts).Verified,
			"killed", count(ev.Counts).Killed)
	case KindAgentFinished:
		r.log.Info("progress: agent", "role", ev.Role, "label", ev.Label,
			"tokens", ev.Tokens, "duration", ev.Duration.String(), "err", ev.Err)
	case KindFindingVerified:
		r.log.Info("progress: verified", "title", ev.Title, "file", ev.File, "line", ev.Line)
	case KindHeatOrdered:
		r.log.Info("progress: heat ordering applied", "heat_files", ev.Count, "top5", ev.Label)
	case KindBudgetDegraded:
		r.log.Warn("progress: budget degraded", "detail", ev.Message)
	case KindBudgetStopped:
		r.log.Warn("progress: budget stopped", "detail", ev.Message)
	case KindLensFailed:
		r.log.Warn("progress: lens failed", "role", ev.Role, "label", ev.Label, "detail", ev.Message)
	case KindAgentActivity:
		if ev.Role != RoleReproducer && ev.Role != RolePatchProver {
			return
		}
		r.log.Info("progress: agent activity", "role", ev.Role, "label", ev.Label, "activity", ev.Activity)
	case KindReproAttempt:
		r.log.Info("progress: repro attempt", "attempt", ev.Attempt, "max_attempts", ev.MaxAttempts,
			"verdict", ev.Verdict, "duration", ev.Duration.String(), "label", ev.Label)
	case KindToolUnhealthy:
		r.log.Warn("progress: tool unhealthy", "tool", ev.Tool, "severity", ev.Severity, "detail", ev.Message)
	default:
		// Cycle/scan/schedule events are already logged by the daemon's own cycle
		// logging; agent_started/spend_tick are too noisy for slog. Drop them.
	}
}

// count returns a non-nil Counts so field access is safe.
func count(c *Counts) Counts {
	if c == nil {
		return Counts{}
	}
	return *c
}

func countsSuffix(c *Counts) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf(" hypothesized=%d triaged=%d verified=%d killed=%d",
		c.Hypothesized, c.Triaged, c.Verified, c.Killed)
}

func errSuffix(err string) string {
	if err == "" {
		return ""
	}
	return " err=" + err
}

// cachedSuffix renders the cache-read token count, or "" when no cache
// activity was reported (the common case for endpoints without caching).
func cachedSuffix(cached int64) string {
	if cached == 0 {
		return ""
	}
	return fmt.Sprintf(" cached=%d", cached)
}
