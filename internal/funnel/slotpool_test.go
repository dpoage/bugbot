package funnel

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlotPool_CapacityUnderLoad verifies that the pool never allows more than
// size concurrent holders. N goroutines each acquire, record peak, and release;
// the high-water mark must not exceed size.
func TestSlotPool_CapacityUnderLoad(t *testing.T) {
	const size = 3
	const workers = 20
	p := newSlotPool(size)

	var (
		mu      sync.Mutex
		current int
		peak    int
	)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.acquire(context.Background(), slotLow); err != nil {
				t.Errorf("acquire failed: %v", err)
				return
			}
			mu.Lock()
			current++
			if current > peak {
				peak = current
			}
			mu.Unlock()

			// hold briefly to let contention build
			time.Sleep(time.Millisecond)

			mu.Lock()
			current--
			mu.Unlock()
			p.release()
		}()
	}
	wg.Wait()

	if peak > size {
		t.Errorf("peak concurrent holders = %d, want <= %d (pool size)", peak, size)
	}
	if peak == 0 {
		t.Error("peak = 0 — no goroutine ever acquired (vacuous test)")
	}

	// After all goroutines finish, the pool must be fully drained back to size.
	// Acquire all slots, then release them — must not block.
	for range size {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if err := p.acquire(ctx, slotLow); err != nil {
			cancel()
			t.Fatalf("post-run acquire failed: %v (pool not fully restored)", err)
		}
		cancel()
	}
	for range size {
		p.release()
	}
}

// TestSlotPool_Priority verifies that with 0 free slots, queued HIGH waiters
// are served before LOW waiters, LOW before IDLE, and within each class FIFO
// order is respected.
//
// To guarantee queue ordering without relying on goroutine scheduling, we use
// a "token-pass" pattern: each goroutine waits on a start channel that the
// test sequentially closes, ensuring queue entries are appended in known order.
func TestSlotPool_Priority(t *testing.T) {
	// Bounded waiter context: a class-starvation regression must FAIL fast
	// here, not hang until the go test global timeout (a prior version of the
	// vacuity check deadlocked instead of failing).
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()

	p := newSlotPool(1)

	// Drain the one free slot so the pool has 0 free.
	if err := p.acquire(waitCtx, slotLow); err != nil {
		t.Fatal(err)
	}

	// Record acquisition order.
	var orderMu sync.Mutex
	var order []string

	// We enqueue waiters in controlled sequence:
	// low0, low1, low2, high0, high1, idle0, idle1
	type entry struct {
		name  string
		class slotClass
	}
	seq := []entry{
		{"low0", slotLow},
		{"low1", slotLow},
		{"low2", slotLow},
		{"high0", slotHigh},
		{"high1", slotHigh},
		{"idle0", slotIdle},
		{"idle1", slotIdle},
	}

	var acquired sync.WaitGroup
	startChs := make([]chan struct{}, len(seq))
	queuedChs := make([]chan struct{}, len(seq))
	for i := range seq {
		startChs[i] = make(chan struct{})
		queuedChs[i] = make(chan struct{})
	}

	for i, e := range seq {
		acquired.Add(1)
		i, e := i, e
		go func() {
			// Wait for our turn to call acquire so we enqueue in order.
			<-startChs[i]
			// Signal that we're about to block (call acquire).
			close(queuedChs[i])
			if err := p.acquire(waitCtx, e.class); err != nil {
				t.Errorf("waiter %q: acquire failed: %v", e.name, err)
				acquired.Done()
				return
			}
			orderMu.Lock()
			order = append(order, e.name)
			orderMu.Unlock()
			p.release()
			acquired.Done()
		}()
	}

	// Sequence the goroutines: close startCh[i], wait for queuedCh[i] (the
	// goroutine is about to call acquire), then spin until the pool's queue
	// length actually reflects the new waiter. Reading the queue lengths under
	// the pool mutex makes the ordering deterministic — a wall-clock sleep
	// here would be a scheduler assumption that can flake on a loaded host.
	queuedLen := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.waitHigh) + len(p.waitLow) + len(p.waitIdle)
	}
	for i := range seq {
		close(startChs[i])
		<-queuedChs[i]
		for queuedLen() < i+1 {
			runtime.Gosched()
		}
	}

	// All waiters are now queued in order. Release the drained slot.
	p.release()
	acquired.Wait()

	orderMu.Lock()
	got := make([]string, len(order))
	copy(got, order)
	orderMu.Unlock()

	if len(got) != len(seq) {
		t.Fatalf("only %d of %d waiters completed: %v", len(got), len(seq), got)
	}

	// HIGH waiters (high0, high1) must both appear before any low or idle waiter.
	highSeen := 0
	for _, name := range got {
		if name == "high0" || name == "high1" {
			highSeen++
		} else {
			// This is a low or idle waiter; all highs must already be done.
			if highSeen < 2 {
				t.Errorf("priority violated: non-high waiter %q completed before all highs; order=%v", name, got)
			}
			break
		}
	}

	// LOW waiters must all appear before any idle waiter.
	idleSeen := 0
	for _, name := range got {
		switch name {
		case "idle0", "idle1":
			idleSeen++
		case "low0", "low1", "low2":
			if idleSeen > 0 {
				t.Errorf("priority violated: low waiter %q completed after idle; order=%v", name, got)
			}
		}
	}

	// Lows must be in FIFO order (low0, low1, low2).
	var lowOrder []string
	for _, name := range got {
		switch name {
		case "low0", "low1", "low2":
			lowOrder = append(lowOrder, name)
		}
	}
	wantLow := []string{"low0", "low1", "low2"}
	for i, want := range wantLow {
		if i >= len(lowOrder) || lowOrder[i] != want {
			t.Errorf("low FIFO order: got %v, want %v", lowOrder, wantLow)
			break
		}
	}

	// Idles must be in FIFO order (idle0, idle1).
	var idleOrder []string
	for _, name := range got {
		if name == "idle0" || name == "idle1" {
			idleOrder = append(idleOrder, name)
		}
	}
	wantIdle := []string{"idle0", "idle1"}
	for i, want := range wantIdle {
		if i >= len(idleOrder) || idleOrder[i] != want {
			t.Errorf("idle FIFO order: got %v, want %v", idleOrder, wantIdle)
			break
		}
	}

	// Idle waiters must both appear AFTER all low waiters (priority violated if
	// any idle appears before all lows).
	lowSeenCount := 0
	for _, name := range got {
		switch name {
		case "low0", "low1", "low2":
			lowSeenCount++
		case "idle0", "idle1":
			if lowSeenCount < 3 {
				t.Errorf("priority violated: idle waiter %q completed before all lows; order=%v", name, got)
			}
		}
	}
}

// TestSlotPool_CancelHammer is the trap-1 test: N goroutines acquire with
// contexts that are randomly cancelled while releases happen simultaneously.
// After the storm the pool's accounting must be exactly intact: acquiring size
// slots without blocking must succeed. All three classes are exercised.
//
// Run with -count=50 and -race to exercise the cancellation-vs-handoff race.
func TestSlotPool_CancelHammer(t *testing.T) {
	const size = 4
	const acquirers = 40
	p := newSlotPool(size)

	ctx, cancel := context.WithCancel(context.Background())

	// Track successful acquisitions so we can verify releases balance.
	var held atomic.Int32

	var wg sync.WaitGroup
	for i := range acquirers {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			// Some goroutines get a very short per-goroutine timeout to trigger
			// cancellation races while the pool is active.
			localCtx := ctx
			if i%3 == 0 {
				var localCancel context.CancelFunc
				localCtx, localCancel = context.WithTimeout(ctx, time.Millisecond)
				defer localCancel()
			}
			// Exercise all three classes: high, low, idle in rotation.
			var class slotClass
			switch i % 3 {
			case 0:
				class = slotHigh
			case 1:
				class = slotLow
			default:
				class = slotIdle
			}
			err := p.acquire(localCtx, class)
			if err != nil {
				// Cancelled: pool must not have leaked our slot.
				return
			}
			held.Add(1)
			// Hold briefly to let concurrency build.
			time.Sleep(time.Duration(i%3) * time.Millisecond)
			p.release()
			held.Add(-1)
		}()
	}

	// Cancel the shared context partway through to trigger in-flight cancellations.
	time.Sleep(5 * time.Millisecond)
	cancel()

	wg.Wait()

	// All holders must have released.
	if h := held.Load(); h != 0 {
		t.Errorf("held = %d after all goroutines done; want 0 (slot leak)", h)
	}

	// Drain pool: must succeed exactly `size` times without blocking.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer drainCancel()
	for i := range size {
		if err := p.acquire(drainCtx, slotLow); err != nil {
			t.Fatalf("post-hammer acquire[%d] failed: %v (pool accounting broken)", i, err)
		}
	}
	// A (size+1)th acquire must block (pool is empty).
	emptyCtx, emptyCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer emptyCancel()
	if err := p.acquire(emptyCtx, slotLow); err == nil {
		t.Error("extra acquire succeeded on an empty pool — pool overcounted free slots")
	}
	for range size {
		p.release()
	}
}

// TestSlotPool_CancelAlreadyCancelled verifies that acquire on an already-
// cancelled context returns ctx.Err() quickly without hanging.
func TestSlotPool_CancelAlreadyCancelled(t *testing.T) {
	p := newSlotPool(1)
	// Drain the free slot so the next acquire must queue.
	if err := p.acquire(context.Background(), slotLow); err != nil {
		t.Fatal(err)
	}

	// Acquire on an already-cancelled context: must return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := p.acquire(ctx, slotIdle)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected error on already-cancelled ctx, got nil")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("acquire took %v; expected near-instant return on cancelled ctx", elapsed)
	}
	p.release()
}

// TestSlotPool_IdleServedLast verifies the vacuity-check property:
// with pool size 1 and both a low and idle waiter queued, the low waiter
// is served first (idle is lowest priority). This test MUST FAIL if idle is
// served before low (vacuity check (a)).
func TestSlotPool_IdleServedLast(t *testing.T) {
	// Bounded waiter context: a class-starvation regression must FAIL fast
	// here, not hang until the go test global timeout (a prior version of the
	// vacuity check deadlocked instead of failing).
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()

	p := newSlotPool(1)

	// Drain the one free slot.
	if err := p.acquire(waitCtx, slotLow); err != nil {
		t.Fatal(err)
	}

	var orderMu sync.Mutex
	var order []string

	// Enqueue: idle first (will be blocked by the low waiter that follows).
	type entry struct {
		name  string
		class slotClass
	}
	seq := []entry{
		{"idle", slotIdle},
		{"low", slotLow},
	}

	var wg sync.WaitGroup
	startChs := make([]chan struct{}, len(seq))
	queuedChs := make([]chan struct{}, len(seq))
	for i := range seq {
		startChs[i] = make(chan struct{})
		queuedChs[i] = make(chan struct{})
	}

	for i, e := range seq {
		wg.Add(1)
		i, e := i, e
		go func() {
			defer wg.Done()
			<-startChs[i]
			close(queuedChs[i])
			if err := p.acquire(waitCtx, e.class); err != nil {
				t.Errorf("waiter %q: acquire failed: %v", e.name, err)
				return
			}
			orderMu.Lock()
			order = append(order, e.name)
			orderMu.Unlock()
			p.release()
		}()
	}

	queuedLen := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.waitHigh) + len(p.waitLow) + len(p.waitIdle)
	}
	for i := range seq {
		close(startChs[i])
		<-queuedChs[i]
		for queuedLen() < i+1 {
			runtime.Gosched()
		}
	}

	p.release()
	wg.Wait()

	orderMu.Lock()
	got := order
	orderMu.Unlock()

	if len(got) != 2 {
		t.Fatalf("want 2 completions, got %d: %v", len(got), got)
	}
	// low must precede idle.
	if got[0] != "low" || got[1] != "idle" {
		t.Errorf("priority violated: order=%v; want [low idle] (idle must be served last)", got)
	}
}
