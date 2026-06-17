package funnel

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
)

// End-to-end boundary integration tests for bugbot-jwd. The mechanism that
// threads each boundary's JSON schema through llm.Request.ResponseSchema
// (capability-gated) is implemented by gd3 + w88; these tests PROVE that
// every declared schema (candidatesSchema, refutationSchema) reaches the
// wire as ResponseSchema at its boundary when the client reports
// StructuredOutput=true, and is absent when the cap is off. The repair
// path is a single tools-less, schema-bearing completion — verified
// explicitly for the finder boundary below.
//
// What is intentionally NOT in scope: parser-correctness of the JSON
// itself (covered by the unit tests for typed unmarshal). The point of
// this file is the WIRE contract, end-to-end through the funnel
// orchestrators that each pipeline stage uses (runFinder, runRefuters,
// runArbiter).

// runFinderUnlimited executes runFinder against the fixture's first
// lens with an unlimited (no-pool) budget so a successful completion is
// never truncated by spend gating.
func runFinderUnlimited(t *testing.T, finder llm.Client, tools []agent.Tool, f *Funnel) []Candidate {
	t.Helper()
	ctx := context.Background()
	// Unlimited budget: empty budgetState means a nil pool, so every
	// pre-turn check is a no-op. (See newBudgetState: budget <= 0 ⇒
	// unlimited.)
	budget := &budgetState{}
	cands, status, _, err := f.runFinder(
		ctx, finder, tools, "senior Go engineer", f.lenses[0],
		[]ingest.Language{ingest.LangGo}, finderTask([]string{"bug.go"}, nil), budget,
	)
	if err != nil {
		t.Fatalf("runFinder: %v", err)
	}
	if status != finderOK {
		t.Fatalf("runFinder status = %d, want finderOK (%d)", status, finderOK)
	}
	return cands
}

// scriptedSequenceClient is a one-shot llm.Client that returns each of
// its scripted bodies exactly once (in order), then falls back to the
// fallback body for any subsequent call. It records every
// llm.Request so the boundary tests can assert on wire-level fields.
// It is a STANDALONE client (not an embedding of scriptedClient) so its
// recording mutex can be taken without recursing into
// scriptedClient.Complete's own mu.Lock — a re-entrant attempt there
// would deadlock on sync.Mutex's non-reentrant lock.
type scriptedSequenceClient struct {
	mu       sync.Mutex
	caps     llm.Capabilities
	bodies   []string
	idx      int
	fallback string
	requests []llm.Request
}

func newScriptedSequenceClient(fallback string, bodies ...string) *scriptedSequenceClient {
	return &scriptedSequenceClient{
		caps:     llm.Capabilities{StructuredOutput: true},
		bodies:   bodies,
		fallback: fallback,
	}
}

func (c *scriptedSequenceClient) Capabilities() llm.Capabilities { return c.caps }

func (c *scriptedSequenceClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	c.requests = append(c.requests, req)
	body := c.fallback
	if c.idx < len(c.bodies) {
		body = c.bodies[c.idx]
		c.idx++
	}
	c.mu.Unlock()
	return llm.Response{
		Text:       body,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (c *scriptedSequenceClient) allRequests() []llm.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.Request, len(c.requests))
	copy(out, c.requests)
	return out
}

// newFunnelForFinder wires a Funnel whose Finder is the given
// client. Funnel.New requires a non-nil finder, so callers must
// always pass one (cap-on, cap-off, scriptedSequenceClient, or a
// placeholder newScriptedClient when only the verifier is under
// test).
func newFunnelForFinder(t *testing.T, finder llm.Client, verifier llm.Client) (*Funnel, []agent.Tool) {
	t.Helper()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		Lenses: []string{"nil-safety/error-handling"}, ChunkSize: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatalf("readOnlyTools: %v", err)
	}
	return f, tools
}

// TestStructuredOutput_FinderCarriesCandidatesSchema asserts that the
// finder's first wire request carries candidatesSchema as
// ResponseSchema when the client reports StructuredOutput=true, and
// the parsed candidates round-trip end-to-end.
func TestStructuredOutput_FinderCarriesCandidatesSchema(t *testing.T) {
	cap := newScriptedClientWithCaps(llm.Capabilities{StructuredOutput: true})
	cap.fallback = candJSON(realCand)
	f, tools := newFunnelForFinder(t, cap, newScriptedClient())

	cands := runFinderUnlimited(t, cap, tools, f)
	if len(cands) != 1 || cands[0].File != "bug.go" || cands[0].Line != 10 {
		t.Fatalf("finder cands = %+v, want one realCand-shaped candidate", cands)
	}

	// Wire contract: at least one completion reached the client,
	// and every completion carries the candidatesSchema verbatim.
	reqs := cap.allRequests()
	if len(reqs) == 0 {
		t.Fatalf("finder client saw no completions")
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(candidatesSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (candidatesSchema):\n%s", i, got, string(candidatesSchema))
		}
	}
}

// TestStructuredOutput_FinderNoCapPassthrough is the negative
// control: when the client reports StructuredOutput=false (zero
// value), the agent gate MUST drop the schema on the wire (no-cap
// passthrough) but the parsed result is still correct. This locks in
// the "no-cap = legacy behavior" half of the gate.
func TestStructuredOutput_FinderNoCapPassthrough(t *testing.T) {
	cap := newScriptedClient() // caps zero value: StructuredOutput=false
	cap.fallback = candJSON(realCand)
	f, tools := newFunnelForFinder(t, cap, newScriptedClient())

	cands := runFinderUnlimited(t, cap, tools, f)
	if len(cands) != 1 {
		t.Fatalf("finder cands = %+v, want one candidate", cands)
	}

	reqs := cap.allRequests()
	if len(reqs) == 0 {
		t.Fatalf("finder client saw no completions")
	}
	for i, r := range reqs {
		if len(r.ResponseSchema) != 0 {
			t.Errorf("req %d: ResponseSchema on wire = %s, want empty (no-cap passthrough)", i, string(r.ResponseSchema))
		}
	}
}

// TestStructuredOutput_FinderRepairCarriesSchema is the boundary
// repair test: the first answer is valid JSON but the WRONG SHAPE (a
// bare array where the schema requires an object with "candidates").
// RunJSON detects the root-type mismatch, fires a single repair
// completion, and the repair must (a) carry the schema on the wire,
// and (b) be tools-less. This locks in the w88 constrained-repair
// contract at the finder boundary specifically.
func TestStructuredOutput_FinderRepairCarriesSchema(t *testing.T) {
	// scriptedSequenceClient returns the array on the first call,
	// then a well-shaped candidates object on every subsequent call
	// — exactly the shape-fail-then-repair sequence the test
	// asserts.
	cap := newScriptedSequenceClient(candJSON(realCand),
		`[{"file":"a","line":1}]`)
	f, tools := newFunnelForFinder(t, cap, newScriptedClient())

	cands := runFinderUnlimited(t, cap, tools, f)
	if len(cands) != 1 || cands[0].File != "bug.go" {
		t.Fatalf("finder cands = %+v, want one realCand candidate after shape repair", cands)
	}

	reqs := cap.allRequests()
	if len(reqs) < 2 {
		t.Fatalf("finder client saw %d completions, want ≥ 2 (main + repair)", len(reqs))
	}
	// Both the main and the repair completion must carry the
	// schema.
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(candidatesSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (candidatesSchema)", i, got)
		}
	}
	// The repair request must be tools-less so adapters that only
	// honor schema on tool-free completions (e.g. Anthropic) also
	// apply grammar-constrained decoding on the retry.
	repair := reqs[len(reqs)-1]
	if len(repair.Tools) != 0 {
		t.Errorf("repair request carried %d tool(s), want 0 (tools-less constrained completion)", len(repair.Tools))
	}
}

// TestStructuredOutput_RefuterCarriesRefutationSchema asserts that
// the refuter boundary's RunJSON completion carries refutationSchema
// as ResponseSchema when the client reports StructuredOutput=true.
// The refuter runs through runRefuters, which is called once per
// panel seat. We drive a 1-seat panel (n=1) so the contract is the
// same shape as production but with a single, predictable request.
func TestStructuredOutput_RefuterCarriesRefutationSchema(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatalf("readOnlyTools: %v", err)
	}

	verifier := newScriptedClientWithCaps(llm.Capabilities{StructuredOutput: true})
	verifier.fallback = notRefutedJSON

	c := Candidate{
		Lens:  "nil-safety/error-handling",
		File:  "bug.go",
		Line:  10,
		Title: "real bug for refuter test",
	}
	budget := &budgetState{}
	verdicts, _, _, failed, stopped, err := f.runRefuters(ctx, verifier, tools, "senior Go engineer", c, 1, budget)
	if err != nil {
		t.Fatalf("runRefuters: %v", err)
	}
	if stopped {
		t.Fatalf("runRefuters stopped=true, want false (unlimited budget)")
	}
	if failed != 0 {
		t.Errorf("runRefuters failed=%d, want 0", failed)
	}
	if len(verdicts) != 1 || verdicts[0].Refuted {
		t.Errorf("verdicts = %+v, want one not-refuted verdict", verdicts)
	}

	reqs := verifier.allRequests()
	if len(reqs) == 0 {
		t.Fatalf("verifier client saw no completions")
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(refutationSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (refutationSchema)", i, got)
		}
	}
}

// TestStructuredOutput_RefuterNoCapPassthrough is the negative
// control for the refuter boundary: StructuredOutput=false must drop
// the schema on the wire while still producing a correct verdict.
func TestStructuredOutput_RefuterNoCapPassthrough(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatalf("readOnlyTools: %v", err)
	}

	verifier := newScriptedClient() // caps zero value
	verifier.fallback = notRefutedJSON

	c := Candidate{Lens: "l", File: "f.go", Line: 1, Title: "t"}
	budget := &budgetState{}
	if _, _, _, _, _, err := f.runRefuters(ctx, verifier, tools, "engineer", c, 1, budget); err != nil {
		t.Fatalf("runRefuters: %v", err)
	}
	for i, r := range verifier.allRequests() {
		if len(r.ResponseSchema) != 0 {
			t.Errorf("req %d: ResponseSchema = %s, want empty (no-cap passthrough)", i, string(r.ResponseSchema))
		}
	}
}

// TestStructuredOutput_ArbiterCarriesRefutationSchema is the
// analogous assertion for the arbiter: when the panel verdict is
// split, runArbiter's single completion also carries
// refutationSchema. We drive a 2-seat panel that produces a split
// verdict (first refutes, second not-refuted), then the arbiter
// (3rd call) also not-refuted.
func TestStructuredOutput_ArbiterCarriesRefutationSchema(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatalf("readOnlyTools: %v", err)
	}

	// 2-seat panel: first seat refutes, second does not → split.
	// 3rd call is the arbiter, also not-refuted.
	verifier := newScriptedSequenceClient(notRefutedJSON,
		refutedJSON, notRefutedJSON, notRefutedJSON)

	c := Candidate{Lens: "l", File: "f.go", Line: 1, Title: "split candidate"}
	budget := &budgetState{}
	verdicts, seats, _, _, _, err := f.runRefuters(ctx, verifier, tools, "engineer", c, 2, budget)
	if err != nil {
		t.Fatalf("runRefuters: %v", err)
	}
	if !isSplitVerdict(verdicts) {
		t.Fatalf("expected split verdict, got %+v", verdicts)
	}

	av, _, _, aerr := f.runArbiter(ctx, verifier, tools, "engineer", c, verdicts, seats, budget)
	if aerr != nil {
		t.Fatalf("runArbiter: %v", aerr)
	}
	if av == nil || av.Refuted {
		t.Fatalf("arbiter verdict = %+v, want not-refuted", av)
	}

	// Every request — both refuter seats and the arbiter — must
	// carry refutationSchema as ResponseSchema (cap on).
	reqs := verifier.allRequests()
	if len(reqs) < 3 {
		t.Fatalf("verifier client saw %d completions, want ≥ 3 (2 refuters + arbiter)", len(reqs))
	}
	for i, r := range reqs {
		if got := string(r.ResponseSchema); got != string(refutationSchema) {
			t.Errorf("req %d: ResponseSchema = %s\nwant (refutationSchema)", i, got)
		}
	}
	// Sanity: the arbiter task message is in the user content of
	// the last request.
	arbiterReq := reqs[len(reqs)-1]
	if !strings.Contains(arbiterReq.Messages[0].Content, "PANEL VERDICTS") {
		t.Errorf("arbiter user message did not contain PANEL VERDICTS marker:\n%s", arbiterReq.Messages[0].Content)
	}
	// Tools are intentionally NOT asserted for the arbiter here:
	// unlike the repair path (which is the documented "tools-less
	// constrained completion" gate at w88), the arbiter's normal
	// run is a fresh RunJSON with the read-only investigation tools
	// still on the wire. The schema-on-the-wire contract holds
	// regardless.
}
