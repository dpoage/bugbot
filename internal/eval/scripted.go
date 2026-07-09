package eval

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
)

// ScriptedClient is a concurrency-safe, deterministic llm.Client that routes
// each request to a canned response by matching against the request's system
// prompt and user message. A single instance can serve every lens's finder
// agent (or every refuter) with distinct output, which is exactly how the
// funnel drives a role client: one shared client, many content-distinct
// requests.
//
// It is the cleaned-up, exported form of the fake client the funnel's own tests
// use. The eval harness needs it as a package-level type so cases can declare
// finder/verifier behavior as data.
//
// Each Complete returns a fixed JSON text answer with no tool calls. That is
// what RunJSON needs: the agent loop makes one completion, sees no tool calls,
// finishes the turn, and RunJSON parses the final text. Usage is attached so
// budget/spend accounting has something to count.
type ScriptedClient struct {
	mu       sync.Mutex
	routes   []route
	fallback string // served when no route matches; "" => empty candidate list
	calls    int
	inUsage  int64
	outUsage int64
}

// route maps a request predicate to a JSON response body.
type route struct {
	match func(req llm.Request) bool
	body  string
}

// NewScriptedClient returns a ScriptedClient with default per-completion usage
// (so spend accounting is exercised) and the empty-candidate fallback.
func NewScriptedClient() *ScriptedClient {
	return &ScriptedClient{inUsage: 100, outUsage: 50}
}

// SetFallback sets the response served when no route matches. An empty string
// means the empty-candidate list (the clean-code default for a finder, and a
// "could not refute" no-op for a verifier that has no matching route).
func (c *ScriptedClient) SetFallback(body string) *ScriptedClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallback = body
	return c
}

// On registers a route: when match returns true for a request, body is served.
// Routes are evaluated in registration order; the first match wins.
func (c *ScriptedClient) On(match func(req llm.Request) bool, body string) *ScriptedClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.routes = append(c.routes, route{match: match, body: body})
	return c
}

// OnSystemContains routes requests whose system prompt contains sub. Use it to
// target a finder lens: the funnel composes each lens's name into the finder
// system prompt, so OnSystemContains("nil-safety/error-handling", ...) hits
// exactly that lens's finder.
func (c *ScriptedClient) OnSystemContains(sub, body string) *ScriptedClient {
	return c.On(func(req llm.Request) bool {
		return strings.Contains(req.System, sub)
	}, body)
}

// OnTaskContains routes requests whose first user message contains sub. Use it
// to target a refuter by candidate title: the funnel embeds the candidate title
// in the verifier task, so OnTaskContains("nil deref of cfg", ...) hits the
// refuters challenging that specific candidate.
func (c *ScriptedClient) OnTaskContains(sub, body string) *ScriptedClient {
	return c.On(func(req llm.Request) bool {
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, sub) {
				return true
			}
		}
		return false
	}, body)
}

// Capabilities reports an empty capability profile; the funnel's finder/verifier
// agents do not depend on any capability flag.
func (c *ScriptedClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

// Complete serves the routed (or fallback) response for req.
func (c *ScriptedClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
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
		body = EmptyCandidates
	}
	return llm.Response{
		Text:       body,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: c.inUsage, OutputTokens: c.outUsage},
	}, nil
}

// CallCount returns the number of Complete calls served so far.
func (c *ScriptedClient) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// --- canned JSON bodies & builders ---------------------------------------

// EmptyCandidates is the finder JSON for "found nothing" — the precision-first
// default outcome.
const EmptyCandidates = `{"candidates": []}`

// Canned refuter verdicts (refutationSchema: no evidence field).
const (
	// NotRefutedJSON is a refuter that genuinely could not disprove the bug
	// (the precision-conservative survival signal).
	NotRefutedJSON = `{"refuted": false, "reasoning": "I read the code; the reported defect is reachable and unguarded.", "confidence": "high"}`
	// RefutedJSON is a refuter that disproved the bug with concrete evidence.
	RefutedJSON = `{"refuted": true, "reasoning": "A prior guard prevents the bad value before this point; the report misreads the path.", "confidence": "high"}`

	// RefutedArbiterJSON / NotRefutedArbiterJSON are the arbiter counterparts:
	// arbiterSchema REQUIRES the structured evidence field (bugbot-mi5.17), so an
	// arbiter response must carry it. A refuter verdict reused as an arbiter
	// response would fail to parse under arbiterSchema — which is exactly why a
	// split-arbiter-refutes case is a regression gate against the pre-mi5.17
	// one-shot arbiter (whose schema rejects the evidence field, falling back to
	// majorityRefuted and letting the false positive survive).
	RefutedArbiterJSON    = `{"refuted": true, "reasoning": "I read the cited code and traced every caller; the bad value cannot reach the line, so the report is refuted.", "confidence": "high", "evidence": ["fixture.go:10"]}`
	NotRefutedArbiterJSON = `{"refuted": false, "reasoning": "I read the code and the call path; the dissent's refutation does not hold and the defect is reachable.", "confidence": "high", "evidence": ["fixture.go:10"]}`
)

// DefaultEvalDefectKind / DefaultEvalSubject are the fallback defect_kind and
// subject Candidates() fills in when a case does not specify one, mirroring
// the Severity/Confidence "high" default below. Every built-in case relies on
// (file, line) — not (defect_kind, subject) — to distinguish candidates, so a
// UNIFORM default across a case's candidates preserves pre-bugbot-ezmx.1
// behavior: two candidates only converge when their locus does, exactly as
// before defect_kind/subject existed. Exported so eval.go's Suppression
// pre-seeding path (which must mint the SAME v3 fingerprint a default-kind
// candidate will) can reuse them instead of hand-duplicating the default.
const (
	DefaultEvalDefectKind = domain.DefectOther
	DefaultEvalSubject    = "eval-fixture"
)

// CandidateJSON is one finder-reported candidate, used to build finder
// responses. Fields mirror the funnel's finder schema. DefectKind/Subject
// default to DefaultEvalDefectKind/DefaultEvalSubject when empty (see
// Candidates) so existing cases that predate bugbot-ezmx.1 need no changes.
type CandidateJSON struct {
	File        string
	Line        int
	Title       string
	Description string
	Severity    string // critical|high|medium|low
	Evidence    string
	Confidence  string // high|medium|low
	DefectKind  string // one of domain.AllDefectKinds; defaults to DefaultEvalDefectKind
	Subject     string // defaults to DefaultEvalSubject
}

// Candidates renders a finder candidate-list JSON body from the given
// candidates, matching the funnel's finder schema. It is the exported builder
// cases use to declare finder output. Empty Severity/Confidence default to
// "high"; empty DefectKind/Subject default to DefaultEvalDefectKind/
// DefaultEvalSubject — so a case can declare a high-confidence bug with just
// file/line/title.
func Candidates(cands ...CandidateJSON) string {
	type wire struct {
		File        string `json:"file"`
		Line        int    `json:"line"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Severity    string `json:"severity"`
		Evidence    string `json:"evidence"`
		Confidence  string `json:"confidence"`
		DefectKind  string `json:"defect_kind"`
		Subject     string `json:"subject"`
	}
	payload := struct {
		Candidates []wire `json:"candidates"`
	}{Candidates: make([]wire, 0, len(cands))}
	for _, c := range cands {
		sev := c.Severity
		if sev == "" {
			sev = "high"
		}
		conf := c.Confidence
		if conf == "" {
			conf = "high"
		}
		kind := c.DefectKind
		if kind == "" {
			kind = string(DefaultEvalDefectKind)
		}
		subject := c.Subject
		if subject == "" {
			subject = DefaultEvalSubject
		}
		payload.Candidates = append(payload.Candidates, wire{
			File: c.File, Line: c.Line, Title: c.Title,
			Description: c.Description, Severity: sev,
			Evidence: c.Evidence, Confidence: conf,
			DefectKind: kind, Subject: subject,
		})
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// The payload is a fixed, marshalable shape; an error is impossible.
		panic("eval: marshal candidates: " + err.Error())
	}
	return string(b)
}
