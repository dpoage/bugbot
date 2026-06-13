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
type scriptedClient struct {
	mu          sync.Mutex
	routes      []route
	fallback    string // returned when no route matches; "" => empty candidate list
	calls       int
	inUsage     int64
	outUsage    int64
	cachedUsage int64 // subset of inUsage reported as cache reads
}

// route maps a request predicate to a JSON response body.
type route struct {
	match func(req llm.Request) bool
	body  string
}

func newScriptedClient() *scriptedClient {
	return &scriptedClient{inUsage: 100, outUsage: 50, cachedUsage: 60}
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

func (c *scriptedClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *scriptedClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++

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

// emptyCandidates is the finder JSON for "found nothing".
const emptyCandidates = `{"candidates": []}`

// notRefutedJSON / refutedJSON are canned refuter verdicts.
const (
	notRefutedJSON = `{"refuted": false, "reasoning": "I read the code; the nil path is reachable and unguarded.", "confidence": "high"}`
	refutedJSON    = `{"refuted": true, "reasoning": "The caller guards this with an explicit nil check before the call.", "confidence": "high"}`
)
