package funnel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dpoage/bugbot/internal/store"
)

// runReproAttempt executes the Options.Repro hook for a single finding using an
// IDLE-priority slot. It is the per-finding body for the in-run repro dispatcher.
//
// Claim check (no-double-attempt): before invoking the hook, the finding is
// re-read from the store. The attempt is skipped when ANY of:
//   - ReproPath is non-empty — the finding has already been promoted (Tier-1+
//     repro_path), whether by an earlier in-run attempt, the daemon drain, or
//     an explicit mutation. Patch-prover-exhausted findings fall here once
//     the prover wrote the path.
//   - NeedsHuman AND ReproWitness is non-empty — the below-quorum verifier
//     survivor has already received its witness artifact; do not re-attempt.
//   - NeedsHuman alone does NOT skip in-run repros anymore (bugbot-w1bh):
//     a below-quorum finding (NeedsHuman, no ReproPath) gets exactly ONE
//     witness attempt via this path. The repro hook writes ReproWitness,
//     not ReproPath, so the finding stays excluded from the daemon backlog
//     (OpenBacklog's !NeedsHuman filter) and from the patch-prover cascade.
//     Repro-as-evidence is now decoupled from repro-as-promotion.
//
// This mirrors OpenBacklog in spirit (no concurrent in-run + daemon-drain
// attempt on the same finding) but is NOT identical: OpenBacklog filters out
// ALL NeedsHuman because the daemon backlog is a promotion path, while this
// gate admits a below-quorum finding exactly once to record the witness.
//
// The IDLE slot is the lowest-priority class (slotIdle): a waiting repro
// goroutine is served AFTER any pending high (verifier) or low (finder)
// waiters. This keeps the reproduce stage from displacing discovery or
// verification under contention.
//
// Errors from the hook are noted best-effort via f.note; they never abort the
// scan. This function always returns, even on ctx cancellation (the hook
// receives the cancelled ctx and should respect it).
//
// The hook takes no overflow parameters; overflow draining is handled by the
// caller (Funnel) after runReproAttempt returns.
func (f *Funnel) runReproAttempt(ctx context.Context, finding store.Finding, scanRunID string) {
	// Register this finding in the durable repro queue (idempotent). The queue
	// provides the authoritative claim/skip gate across all three dispatch paths
	// (in-run hook, daemon backlog drain, `bugbot repro` CLI). The Repro hook
	// (PromoteOne) will claim and mark the row; here we only ensure it exists.
	if _, err := f.store.EnqueueRepro(ctx, finding.Fingerprint); err != nil {
		// Best-effort: queue insert failed (ctx cancelled, etc.). Fall through to
		// the fast-path claim check below; the hook will attempt claim itself.
		if ctx.Err() != nil {
			return
		}
	}

	// Fast-path claim check using the queue: skip if the row is already running,
	// done, or abandoned, OR if the old finding-field guards fire (backward
	// compatibility with findings that predate the queue table).
	if ra, err := f.store.GetReproAttempt(ctx, finding.Fingerprint); err == nil {
		switch ra.State {
		case store.ReproStateDone, store.ReproStateRunning, store.ReproStateAbandoned:
			return
		}
	} else {
		// Queue row missing or DB error: fall back to the finding-field check.
		// TODO: remove this fallback once every in-flight finding is guaranteed
		// to have a repro_attempts row (i.e. after the 018 migration has been
		// live for at least one scan cycle). At that point the queue is the sole
		// claim gate and the old flag-combination inference can be deleted.
		current, ferr := f.store.GetFindingByFingerprint(ctx, finding.Fingerprint)
		if ferr != nil {
			return
		}
		if current.ReproPath != "" || (current.NeedsHuman && current.ReproWitness != "") {
			return
		}
	}

	// Acquire an IDLE-priority slot. This blocks if all slots are held but
	// gives way to any high (verifier) or low (finder) waiter — the reproducer
	// is latency-tolerant and loses nothing by waiting its turn.
	if err := f.slots.acquire(ctx, slotIdle); err != nil {
		// Context cancelled while waiting for a slot. The hook is not called;
		// the finding will be caught by the daemon backlog drain on the next
		// cycle.
		return
	}
	defer f.slots.release()

	// Re-check after slot acquisition: another goroutine may have promoted or
	// witnessed while we were waiting.
	current2, err := f.store.GetFindingByFingerprint(ctx, finding.Fingerprint)
	if err == nil && (current2.ReproPath != "" || (current2.NeedsHuman && current2.ReproWitness != "")) {
		return
	}

	startedAt := time.Now()
	hookErr := f.opts.Repro(ctx, scanRunID, finding)
	finishedAt := time.Now()

	// Record agent_units row: role='reproducer'. The hook owns the actual
	// promotion or witness; we derive the outcome status by re-reading
	// the store after the hook returns.
	//
	// Status vocabulary (reproducer role, documented in store/agentunits.go):
	//   reproduced    — finding promoted to Tier-1 (ReproPath now set)
	//   witnessed     — below-quorum (NeedsHuman) finding's repro hook fired and
	//                   wrote ReproWitness. Tier is unchanged; this is repro
	//                   evidence only, no promotion and no patch-prover
	//                   cascade. Newly added in bugbot-w1bh.
	//   exhausted     — all attempts failed; finding stays at its prior tier
	//  — hook returned an error before any sandbox run
	//   infra_error   — hook returned a non-nil error (infrastructure failure)
	//
	// Tokens: the hook closure (built by the CLI) wires its own llm.Recorder
	// into the reproducer client, so spend is already attributed to the scan
	// run's ledger via the CLI's rec. We record zero tokens here rather than
	// double-counting; the spend ledger is the authoritative source.
	// (scan_run_id flows via the hook closure's rec.SetScanRun call in cli/scan.go.)
	var status string
	var detail string
	if hookErr != nil {
		status = "infra_error"
		detail = fmt.Sprintf("hook_error=%s elapsed_ms=%d", hookErr.Error(), finishedAt.Sub(startedAt).Milliseconds())
	} else {
		// Re-read to derive outcome.
		after, rerr := f.store.GetFindingByFingerprint(ctx, finding.Fingerprint)
		if rerr != nil {
			status = "infra_error"
			detail = fmt.Sprintf("post_hook_read_error=%s elapsed_ms=%d", rerr.Error(), finishedAt.Sub(startedAt).Milliseconds())
		} else if after.ReproPath != "" {
			status = "reproduced"
			detail = fmt.Sprintf("tier=%d elapsed_ms=%d", after.Tier, finishedAt.Sub(startedAt).Milliseconds())
		} else if after.NeedsHuman && after.ReproWitness != "" {
			// Below-quorum witness recorded: hook wrote ReproWitness but did NOT
			// promote. Tier is preserved (NeedsHuman path, no Tier-1 promotion).
			status = "witnessed"
			detail = fmt.Sprintf("tier=%d needs_human=true elapsed_ms=%d", after.Tier, finishedAt.Sub(startedAt).Milliseconds())
		} else if after.NeedsHuman {
			status = "exhausted"
			detail = fmt.Sprintf("needs_human=true elapsed_ms=%d", finishedAt.Sub(startedAt).Milliseconds())
		} else {
			status = "exhausted"
			detail = fmt.Sprintf("elapsed_ms=%d", finishedAt.Sub(startedAt).Milliseconds())
		}
	}

	row := store.AgentUnit{
		ScanRunID:  scanRunID,
		Role:       "reproducer",
		Lens:       finding.Lens,
		Files:      []string{finding.File},
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Status:     store.AgentStatus(status),
		// InputTokens: 0 — spend is attributed via the hook closure's recorder,
		// not double-counted here. See comment above.
		Candidates: reproSucceededInt(status),
		Detail:     detail,
	}
	if aerr := f.store.AddAgentUnit(ctx, row); aerr != nil {
		// Best-effort: a failed insert never aborts the scan.
		_ = aerr
	}
}

// reproSucceededInt returns 1 when the reproducer status indicates a successful
// reproduction (Tier-1 promotion), 0 otherwise. Mirrors candidateSurvivedInt
// for the verifier role.
func reproSucceededInt(status string) int {
	if status == "reproduced" {
		return 1
	}
	return 0
}

// reproQueue carries survived Tier-2 findings from verify goroutines to the
// in-run repro consumer. enqueue NEVER blocks (the spec's hard requirement:
// a slow repro backlog must not stall verification): a full channel falls
// back to the overflow slice, which run() drains sequentially after all
// channel-path attempts complete. A finding lands in exactly ONE of the two
// paths (the select is mutually exclusive), so exactly-once delivery holds by
// construction; the claim check in runReproAttempt is the backstop. In
// practice the consumer is a spawn-only loop that drains the channel faster
// than verify can fill it, so the overflow path fires only under scheduler
// starvation — it is defensive machinery, tested directly below rather than
// through contrived integration timing.
type reproQueue struct {
	ch       chan store.Finding
	mu       sync.Mutex
	overflow []store.Finding
}

func newReproQueue(buffer int) *reproQueue {
	return &reproQueue{ch: make(chan store.Finding, buffer)}
}

// enqueue delivers f to the channel when there is room, else stages it in the
// overflow slice. Never blocks.
func (q *reproQueue) enqueue(f store.Finding) {
	select {
	case q.ch <- f:
	default:
		q.mu.Lock()
		q.overflow = append(q.overflow, f)
		q.mu.Unlock()
	}
}

// drainOverflow returns the staged overflow findings exactly once: a second
// call returns nil unless new findings were staged in between.
func (q *reproQueue) drainOverflow() []store.Finding {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.overflow
	q.overflow = nil
	return out
}
