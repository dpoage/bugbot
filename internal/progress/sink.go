package progress

import "time"

// EventSink consumes progress events. Implementations MUST treat Handle as a passive
// observation: it must never block the caller for long, never panic, and never
// return an error that could fail the pipeline (it returns nothing by design).
// The funnel and daemon call Handle on their hot path; a slow or broken sink
// must degrade to dropping/rate-limiting, never to stalling the scan.
//
// Implementations must be safe for concurrent use: parallel finder/verifier
// agents emit from multiple goroutines.
type EventSink interface {
	Handle(Event)
}

// Emit sends ev to sink, defaulting ev.Time to now when unset. A nil sink is a
// no-op, so callers can hold an optional sink without nil-checking at every
// emission point. This is the single choke point the pipeline calls.
func Emit(sink EventSink, ev Event) {
	if sink == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	sink.Handle(ev)
}

// SinkFunc adapts a plain function to the EventSink interface.
type SinkFunc func(Event)

// Handle implements EventSink.
func (f SinkFunc) Handle(ev Event) { f(ev) }

// Discard is an EventSink that ignores every event. Useful as a default and in tests.
type Discard struct{}

// Handle implements EventSink.
func (Discard) Handle(Event) {}

// Multi fans one event out to several sinks in order. A nil entry is skipped.
// Multi itself honors the non-blocking contract only as well as its members do;
// it adds no buffering of its own (each member owns its own backpressure
// policy).
type Multi struct {
	sinks []EventSink
}

// NewMulti builds a fanout sink over the given sinks. Nil sinks are retained but
// skipped at Handle time, so the slice length is stable for callers that index
// it; in practice callers just pass the sinks they have.
func NewMulti(sinks ...EventSink) *Multi {
	return &Multi{sinks: sinks}
}

// Handle implements EventSink, forwarding to every non-nil member.
func (m *Multi) Handle(ev Event) {
	if m == nil {
		return
	}
	for _, s := range m.sinks {
		if s != nil {
			s.Handle(ev)
		}
	}
}
