package progress

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingSink captures every event for assertions. Safe for concurrent use.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingSink) Handle(ev Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

func TestEmit_NilSinkIsNoOp(t *testing.T) {
	// Must not panic.
	Emit(nil, Event{Kind: KindScanStarted})
}

func TestEmit_StampsTime(t *testing.T) {
	var got Event
	Emit(SinkFunc(func(ev Event) { got = ev }), Event{Kind: KindScanStarted})
	if got.Time.IsZero() {
		t.Fatal("Emit did not stamp Time on an unstamped event")
	}

	want := time.Unix(1000, 0)
	Emit(SinkFunc(func(ev Event) { got = ev }), Event{Kind: KindScanStarted, Time: want})
	if !got.Time.Equal(want) {
		t.Errorf("Emit overwrote a preset Time: got %v want %v", got.Time, want)
	}
}

// TestEmitReproBlocked_NamesBinaryNotModeToken pins the operator-facing
// Message wording (bugbot-813i): the go ecosystem must render its BINARY
// ("image lacks go"), never its probe-mode token ("present"), alongside the
// established js->node mapping.
func TestEmitReproBlocked_NamesBinaryNotModeToken(t *testing.T) {
	sink := &recordingSink{}
	EmitReproBlocked(sink, map[string]int{"go": 1, "js": 38})

	events := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	msgs := events[0].Message + "\n" + events[1].Message
	for _, want := range []string{
		"1 finding(s) blocked: image lacks go",
		"38 finding(s) blocked: image lacks node",
	} {
		if !strings.Contains(msgs, want) {
			t.Errorf("missing %q in messages:\n%s", want, msgs)
		}
	}
	if strings.Contains(msgs, "present") {
		t.Errorf("go probe-mode token leaked into operator-facing message:\n%s", msgs)
	}
}

func TestMulti_FansOutInOrder(t *testing.T) {
	var order []int
	var mu sync.Mutex
	mk := func(id int) EventSink {
		return SinkFunc(func(Event) {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		})
	}
	m := NewMulti(mk(1), nil, mk(2), mk(3))
	m.Handle(Event{Kind: KindScanStarted})

	if len(order) != 3 {
		t.Fatalf("want 3 deliveries (nil skipped), got %d: %v", len(order), order)
	}
	for i, id := range []int{1, 2, 3} {
		if order[i] != id {
			t.Errorf("delivery %d = %d, want %d (order not preserved)", i, order[i], id)
		}
	}
}

func TestMulti_NilReceiverSafe(t *testing.T) {
	var m *Multi
	m.Handle(Event{Kind: KindScanStarted}) // must not panic
}

func TestDiscard_Ignores(t *testing.T) {
	Discard{}.Handle(Event{Kind: KindScanStarted}) // no-op, must not panic
}

// TestNonBlockingContract verifies the documented contract that Emit returns
// promptly and never propagates a sink panic to the caller's control flow under
// concurrent emission — the property the funnel relies on so a renderer cannot
// stall or crash the pipeline. (Sinks must not panic; this guards that a slow
// sink does not serialize callers beyond its own lock.)
func TestNonBlockingContract_ConcurrentEmit(t *testing.T) {
	rec := &recordingSink{}
	const goroutines, per = 8, 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				Emit(rec, Event{Kind: KindSpendTick})
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Emit did not complete promptly; sink may be blocking callers")
	}

	if got := len(rec.snapshot()); got != goroutines*per {
		t.Errorf("recorded %d events, want %d", got, goroutines*per)
	}
}
