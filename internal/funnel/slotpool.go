package funnel

import (
	"context"
	"sync"
)

// slotClass identifies the priority class for a slot acquisition.
// Three classes exist, served in descending priority order by release():
//
//	slotHigh  — verifier panels: cheap, latency-sensitive, gates persistence
//	slotLow   — finder units: breadth, tolerates waiting behind verifiers
//	slotIdle  — reproducer: sandbox-heavy, latency-tolerant, served last
type slotClass int

const (
	slotHigh slotClass = iota
	slotLow
	slotIdle
)

// slotPool is the funnel-wide agent concurrency bound: every LLM agent —
// finder unit, verifier candidate panel (one slot spans the whole sequential
// panel + arbiter), or reproducer — runs inside one slot, so
// Options.MaxParallel means "concurrent agents, total" regardless of how
// stages overlap. Three priority classes exist for the streaming topology:
// released slots go to waiting HIGH acquirers (verifier-side: cheap,
// latency-sensitive, gates everything downstream) before LOW (finder
// breadth) before IDLE (reproducer: sandbox-heavy, latency-tolerant).
// FIFO within each class.
type slotPool struct {
	mu   sync.Mutex
	free int

	// waitHigh, waitLow, and waitIdle are FIFO queues of single-use channels.
	// A waiter enqueues its channel and then selects on it vs ctx.Done().
	// release() dequeues the oldest high waiter first, then oldest low, then
	// oldest idle, and sends into its channel. The channel is buffered (size 1)
	// so release never blocks even if the waiter's select has already chosen
	// ctx.Done().
	//
	// Cancellation protocol (no slot leak guarantee):
	//   On ctx.Done(), the waiter locks and tries to remove itself from the
	//   queue. If it finds itself (removed=true), it was never dequeued by
	//   release — safe to return ctx.Err() immediately.
	//   If it does NOT find itself (removed=false), release already dequeued
	//   it and WILL send to ch unconditionally (release always sends after
	//   dequeuing). The waiter must then block on <-ch and call release() to
	//   return the slot — no default branch, because there is a window between
	//   "release dequeued" and "release sent" where ch is still empty.
	//
	// The class is invariant per acquire call: a waiter always knows which
	// queue to remove itself from on cancellation (determined by the class
	// argument passed to acquire, captured in the closure).
	waitHigh []chan struct{}
	waitLow  []chan struct{}
	waitIdle []chan struct{}
}

func newSlotPool(size int) *slotPool {
	if size <= 0 {
		size = 1
	}
	return &slotPool{free: size}
}

// acquire blocks until a slot is free or ctx is done. class selects the
// priority class (slotHigh > slotLow > slotIdle). Returns ctx.Err() on
// cancellation while waiting.
func (p *slotPool) acquire(ctx context.Context, class slotClass) error {
	// Fast path: grab a free slot without waiting. DELIBERATELY no ctx check
	// here — this is load-bearing for the funnel's interrupted-run semantics:
	// with free >= 1 at stage start, the first goroutine always proceeds into
	// the agent runner even on a dead ctx, and the runner's ctx.Err() return
	// sets the stage's firstErr, classifying the run as Interrupted. If the
	// fast path rejected on a dead ctx, a cancellation landing before any unit
	// ran could let a stage return cleanly with no error and no Interrupted
	// flag.
	p.mu.Lock()
	if p.free > 0 {
		p.free--
		p.mu.Unlock()
		return nil
	}

	// Slow path: enqueue a waiter channel. The channel is buffered(1) so
	// release() can send without blocking after dequeuing.
	ch := make(chan struct{}, 1)
	switch class {
	case slotHigh:
		p.waitHigh = append(p.waitHigh, ch)
	case slotLow:
		p.waitLow = append(p.waitLow, ch)
	default: // slotIdle
		p.waitIdle = append(p.waitIdle, ch)
	}
	p.mu.Unlock()

	select {
	case <-ch:
		// Slot handed to us directly by release(). We own it.
		return nil
	case <-ctx.Done():
		// Cancellation: try to remove ourselves from the queue.
		// The class is invariant for this acquire call, so we always know
		// which queue to search.
		p.mu.Lock()
		removed := false
		switch class {
		case slotHigh:
			p.waitHigh, removed = removeFirst(p.waitHigh, ch)
		case slotLow:
			p.waitLow, removed = removeFirst(p.waitLow, ch)
		default: // slotIdle
			p.waitIdle, removed = removeFirst(p.waitIdle, ch)
		}
		p.mu.Unlock()

		if !removed {
			// release() already dequeued us (under the mutex) and will send to
			// ch unconditionally. We MUST block on <-ch (no default) because
			// there is a window between "release dequeued" and "release sent"
			// where ch is still empty. Once we receive, we own the slot and
			// must return it.
			<-ch
			p.release()
		}

		return ctx.Err()
	}
}

// release returns a slot to the pool. It hands the slot directly to the oldest
// high waiter, else the oldest low waiter, else the oldest idle waiter, else
// increments free.
func (p *slotPool) release() {
	p.mu.Lock()
	if len(p.waitHigh) > 0 {
		ch := p.waitHigh[0]
		p.waitHigh = p.waitHigh[1:]
		p.mu.Unlock()
		// ch is buffered(1); send never blocks. The waiter either receives it
		// (normal path) or — if it already cancelled and found itself removed
		// from the queue — this path is unreachable (we only send to channels
		// we dequeued, and a successfully-removed channel is no longer reachable
		// from here).
		ch <- struct{}{}
		return
	}
	if len(p.waitLow) > 0 {
		ch := p.waitLow[0]
		p.waitLow = p.waitLow[1:]
		p.mu.Unlock()
		ch <- struct{}{}
		return
	}
	if len(p.waitIdle) > 0 {
		ch := p.waitIdle[0]
		p.waitIdle = p.waitIdle[1:]
		p.mu.Unlock()
		ch <- struct{}{}
		return
	}
	p.free++
	p.mu.Unlock()
}

// removeFirst removes the first occurrence of ch from the slice and reports
// whether it was found. Returns the (possibly shorter) slice.
func removeFirst(q []chan struct{}, ch chan struct{}) ([]chan struct{}, bool) {
	for i, c := range q {
		if c == ch {
			return append(q[:i:i], q[i+1:]...), true
		}
	}
	return q, false
}
