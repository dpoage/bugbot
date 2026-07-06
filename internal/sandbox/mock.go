package sandbox

import (
	"context"
	"sync"
)

// Call is a single recorded invocation of Mock.Exec, capturing the Spec it was
// called with so tests can assert on what the funnel/repro stages requested.
type Call struct {
	Spec Spec
}

// MockResponse scripts the outcome of one Mock.Exec call.
type MockResponse struct {
	// Result is returned to the caller (when Err is nil).
	Result Result
	// Err, when non-nil, is returned instead of Result, simulating an
	// infrastructure failure.
	Err error
}

// Mock is a scriptable, in-memory Sandbox implementation for testing callers
// (e.g. the funnel and reproduce stages) without a real container runtime. It
// is part of the package's public contract.
//
// Responses are consumed in order via EnqueueResponse; once exhausted, the
// configured DefaultResponse is returned. A ResponseFunc, when set, takes
// precedence and is consulted for every call (enabling spec-dependent
// behavior). All calls are recorded. Mock is safe for concurrent use.
//
// Spec.Workspace is honored NEUTRALLY: Mock never touches the filesystem, so
// a caller-supplied Workspace is recorded on the Call like every other Spec
// field but otherwise ignored — it never gates or alters which scripted
// response comes back. Tests that need to distinguish an iteration-workspace
// call (e.g. try_repro) from a clean-room call (e.g. execute()) do so via a
// ResponseFunc keyed on spec.Workspace == "" themselves.
type Mock struct {
	mu sync.Mutex

	// ResponseFunc, when set, is invoked for every Exec call and its return
	// values are used verbatim. It takes precedence over the queue and the
	// default. The call number n is zero-based.
	ResponseFunc func(n int, spec Spec) (Result, error)

	// DefaultResponse is returned when the queue is empty and ResponseFunc is
	// nil.
	DefaultResponse MockResponse

	queue []MockResponse
	calls []Call
}

// NewMock constructs a Mock with the given default response.
func NewMock(def MockResponse) *Mock {
	return &Mock{DefaultResponse: def}
}

// EnqueueResponse appends a scripted response to the FIFO queue. Returns the
// Mock for chaining.
func (m *Mock) EnqueueResponse(r MockResponse) *Mock {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, r)
	return m
}

// Exec implements Sandbox: it records the call and returns the next scripted
// response (or the default).
func (m *Mock) Exec(_ context.Context, spec Spec) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := len(m.calls)
	m.calls = append(m.calls, Call{Spec: spec})

	if m.ResponseFunc != nil {
		return m.ResponseFunc(n, spec)
	}
	if len(m.queue) > 0 {
		r := m.queue[0]
		m.queue = m.queue[1:]
		return r.Result, r.Err
	}
	return m.DefaultResponse.Result, m.DefaultResponse.Err
}

// Calls returns a copy of all recorded calls in invocation order.
func (m *Mock) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// CallCount returns the number of times Exec has been invoked.
func (m *Mock) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// Reset clears recorded calls and the queued responses (DefaultResponse and
// ResponseFunc are retained).
func (m *Mock) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
	m.queue = nil
}

var _ Sandbox = (*Mock)(nil)
