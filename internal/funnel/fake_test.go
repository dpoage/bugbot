package funnel

import (
	"context"
	"strings"
	"sync"

	"github.com/dpoage/bugbot/internal/llm"
)

// scriptedClient is a concurrency-safe fake llm.Client for funnel tests. It
// routes each request to a response by matching against the request's system
// prompt and user message, so a single client instance can serve every lens's
// finder agent (or every refuter) with distinct, deterministic output.
//
// Each completion returns a fixed JSON text answer with no tool calls, which is
// exactly what RunJSON needs: the agent loop makes one completion, sees no tool
// calls, finishes the turn, and RunJSON parses FinalText. Usage is attached so
// budget/spend accounting has something to count.
//
// Tests that need to verify the agent-layer gate for native schema-constrained
// output flip caps (via newScriptedClientWithCaps) so the client reports
// StructuredOutput=true; otherwise the zero value is the historical default
// (no caps, all behavior unchanged). The requests field records every
// llm.Request the client sees so tests can assert on wire-level fields like
// ResponseSchema and Tools without standing up a recorder.
type scriptedClient struct {
	mu          sync.Mutex
	routes      []route
	fallback    string // returned when no route matches; "" => empty candidate list
	calls       int
	inUsage     int64
	outUsage    int64
	cachedUsage int64 // subset of inUsage reported as cache reads
	// caps is the static capability profile this client reports. The zero
	// value (StructuredOutput=false) is the historical default; tests that
	// need to verify the agent-layer gate for native schema-constrained
	// output flip this on via newScriptedClientWithCaps. Add (or set) only
	// — never change a field's meaning — so existing tests keep their
	// behavior.
	caps llm.Capabilities
	// requests records every llm.Request Complete sees, in arrival order.
	// Lock-protected; tests inspect it via allRequests. The recording is
	// purely additive: it does not change the response the client returns.
	requests []llm.Request
}

// route maps a request predicate to a JSON response body.
type route struct {
	match func(req llm.Request) bool
	body  string
}

func newScriptedClient() *scriptedClient {
	return &scriptedClient{inUsage: 100, outUsage: 50, cachedUsage: 60}
}

// newScriptedClientWithCaps returns a fresh scriptedClient that reports
// the given capability profile. It does NOT copy an existing client's
// mutex (which would violate sync.Mutex's "must not be copied after
// first use" rule and produce a client that deadlocks on its first
// Lock). Use this for structured-output tests that need
// StructuredOutput=true on a single client.
func newScriptedClientWithCaps(caps llm.Capabilities) *scriptedClient {
	return &scriptedClient{inUsage: 100, outUsage: 50, cachedUsage: 60, caps: caps}
}

// on registers a route: when match returns true for a request, body is served.
// Routes are evaluated in registration order; the first match wins.
func (c *scriptedClient) on(match func(req llm.Request) bool, body string) *scriptedClient {
	c.routes = append(c.routes, route{match: match, body: body})
	return c
}

// onSystemContains routes requests whose system prompt contains sub.
func (c *scriptedClient) onSystemContains(sub, body string) *scriptedClient {
	return c.on(func(req llm.Request) bool {
		return strings.Contains(req.System, sub)
	}, body)
}

// onTaskContains routes requests whose first user message contains sub.
func (c *scriptedClient) onTaskContains(sub, body string) *scriptedClient {
	return c.on(func(req llm.Request) bool {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, sub) {
				return true
			}
		}
		return false
	}, body)
}

func (c *scriptedClient) Capabilities() llm.Capabilities { return c.caps }

func (c *scriptedClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.requests = append(c.requests, req)

	body := c.fallback
	for _, r := range c.routes {
		if r.match(req) {
			body = r.body
			break
		}
	}
	if body == "" {
		body = emptyCandidates
	}
	return llm.Response{
		Text:       body,
		StopReason: llm.StopEndTurn,
		Usage: llm.Usage{
			InputTokens: c.inUsage, OutputTokens: c.outUsage,
			CacheReadInputTokens: c.cachedUsage,
		},
	}, nil
}

func (c *scriptedClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// allRequests returns a copy of the recorded requests, in arrival order.
// Returned slice is detached from the client's internal state so the caller
// can inspect it without holding the lock.
func (c *scriptedClient) allRequests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.Request, len(c.requests))
	copy(out, c.requests)
	return out
}

// emptyCandidates is the finder JSON for "found nothing".
const emptyCandidates = `{"candidates": []}`

// notRefutedJSON / refutedJSON are canned refuter verdicts (refutationSchema:
// no evidence field). notRefutedArbiterJSON / refutedArbiterJSON are the
// arbiter counterparts: arbiterSchema REQUIRES the evidence field
// (bugbot-mi5.17), so a refuter verdict reused as an arbiter response would
// fail to parse. Split-panel tests route the arbiter response to these.
const (
	notRefutedJSON = `{"refuted": false, "reasoning": "I read the code; the nil path is reachable and unguarded.", "confidence": "high"}`
	refutedJSON    = `{"refuted": true, "reasoning": "The caller guards this with an explicit nil check before the call.", "confidence": "high"}`

	notRefutedArbiterJSON = `{"refuted": false, "reasoning": "I read the cited code and traced the call path; the dissent's refutation does not hold and the bug stands.", "confidence": "high", "evidence": ["f.go:1"]}`
	refutedArbiterJSON    = `{"refuted": true, "reasoning": "I confirmed every caller guards the value before the call, so the claimed bad state cannot reach the line.", "confidence": "high", "evidence": ["f.go:1"]}`
)
