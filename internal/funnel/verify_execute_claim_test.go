package funnel

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// --- isExecutableClaim classifier tests -------------------------------------

// TestIsExecutableClaim_Table proves the classifier identifies the
// parseSARIF-cap claim from the false-positive incident (bugbot-aud) as
// executable, and rejects concurrency / I/O claims.
func TestIsExecutableClaim_Table(t *testing.T) {
	cases := []struct {
		name string
		cand Candidate
		want bool
	}{
		{
			name: "parseSARIF cap bypass claim (the FP incident)",
			cand: Candidate{
				Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
				Description: "The per-run cap on results is not enforced when the SARIF log has multiple runs; parseSARIF appends past the cap.",
				Severity:    "medium",
			},
			want: true,
		},
		{
			name: "concurrency / goroutine race claim (must be REJECTED)",
			cand: Candidate{
				Title:       "data race on shared map in goroutine",
				Description: "Two goroutines write to the cache without synchronization; the map can be corrupted.",
				Severity:    "medium",
			},
			want: false,
		},
		{
			name: "network I/O claim (must be REJECTED)",
			cand: Candidate{
				Title:       "HTTP timeout not propagated through retry",
				Description: "The HTTP client swallows the network timeout on retry, masking real failures.",
				Severity:    "medium",
			},
			want: false,
		},
		{
			name: "filesystem / disk I/O claim (must be REJECTED)",
			cand: Candidate{
				Title:       "disk write race corrupts the cache file",
				Description: "Concurrent writers can interleave filesystem writes, leaving the file half-updated.",
				Severity:    "medium",
			},
			want: false,
		},
		{
			name: "clock / wall-clock claim (must be REJECTED)",
			cand: Candidate{
				Title:       "expiry check uses wrong clock value",
				Description: "TTL uses the wrong wall clock so tokens never expire.",
				Severity:    "medium",
			},
			want: false,
		},
		{
			name: "env var claim (must be REJECTED)",
			cand: Candidate{
				Title:       "missing env var check in config",
				Description: "An env var is read without a default; the env var can be empty and break startup.",
				Severity:    "medium",
			},
			want: false,
		},
		{
			name: "pure nil-deref claim (no det-marker, no env-marker) is NOT executable",
			cand: Candidate{
				Title:       "nil deref of cfg in Greeting",
				Description: "The cfg pointer can be nil and is dereferenced before the guard.",
				Severity:    "high",
			},
			want: false,
		},
		{
			name: "integer overflow (deterministic) IS executable",
			cand: Candidate{
				Title:       "Add overflows on large ints",
				Description: "Adding two int64s near max int wraps to a negative value.",
				Severity:    "high",
			},
			want: true,
		},
		{
			name: "regex miscompile (deterministic) IS executable",
			cand: Candidate{
				Title:       "regex match succeeds on input that should be invalid",
				Description: "The pattern matches a substring of the bad payload, validating it incorrectly.",
				Severity:    "medium",
			},
			want: true,
		},
		{
			name: "truncation boundary (deterministic) IS executable",
			cand: Candidate{
				Title:       "truncate at byte boundary loses the last codepoint",
				Description: "Truncating UTF-8 by byte length can split a multi-byte rune at the boundary.",
				Severity:    "medium",
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecutableClaim(tc.cand); got != tc.want {
				t.Errorf("isExecutableClaim() = %v, want %v (title=%q)", got, tc.want, tc.cand.Title)
			}
		})
	}
}

// --- buildSandboxTool severity-floor bypass tests --------------------------

// TestBuildSandboxTool_BypassForExecutableClaim proves that a MEDIUM-severity
// executable claim receives the sandbox_exec tool even when the configured
// minimum severity is "high". This is the core fix for the parseSARIF FP.
func TestBuildSandboxTool_BypassForExecutableClaim(t *testing.T) {
	_, repo := openFixture(t)
	sb := &funnelFakeSandbox{}
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     true,
			Sandbox:     sb,
			MinSeverity: "high",
			MaxExecs:    3,
		}},
		lenses: selectLenses(nil),
	}

	// parseSARIF candidate at MEDIUM severity — below "high" floor.
	c := Candidate{
		Title:    "parseSARIF cap is bypassed by multi-run SARIF documents",
		Severity: "medium",
	}
	var execs atomic.Int32
	var millis atomic.Int64
	if tool := f.buildSandboxTool(c, &execs, &millis); tool == nil {
		t.Fatal("buildSandboxTool returned nil for an executable claim at medium severity; bypass should apply")
	}
	if tool := f.buildSandboxTool(c, &execs, &millis); tool == nil {
		t.Fatal("nil tool after second call; tool builder must be idempotent")
	}
}

// TestBuildSandboxTool_FloorPreservedForNonExecutableClaim proves that a
// MEDIUM-severity non-executable claim does NOT receive the sandbox tool
// when the minimum severity is "high". The bypass is executable-only.
func TestBuildSandboxTool_FloorPreservedForNonExecutableClaim(t *testing.T) {
	_, repo := openFixture(t)
	sb := &funnelFakeSandbox{}
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     true,
			Sandbox:     sb,
			MinSeverity: "high",
			MaxExecs:    3,
		}},
		lenses: selectLenses(nil),
	}

	c := Candidate{
		Title:    "data race on shared map in goroutine",
		Severity: "medium",
	}
	var execs atomic.Int32
	var millis atomic.Int64
	if tool := f.buildSandboxTool(c, &execs, &millis); tool != nil {
		t.Error("buildSandboxTool returned a tool for a non-executable medium claim; floor must still apply")
	}
}

// TestBuildSandboxTool_BypassIgnoresFloor pins the exact contract of the
// executable-claim bypass: when the feature is on AND the sandbox is set,
// isExecutableClaim(c) is the only gate. The configured MinSeverity floor
// is BYPASSED for executable claims — the parseSARIF cap claim (a medium
// false positive) was the canonical example of why a "high" floor was
// wrong for executable claims. This is a deliberate per-claim bypass;
// MinSeverity still applies to non-executable claims (see FloorPreserved
// above).
func TestBuildSandboxTool_BypassIgnoresFloor(t *testing.T) {
	_, repo := openFixture(t)
	sb := &funnelFakeSandbox{}
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     true,
			Sandbox:     sb,
			MinSeverity: "critical", // even at the strictest floor,
			MaxExecs:    3,
		}},
		lenses: selectLenses(nil),
	}

	// An executable claim gets the tool regardless of where MinSeverity
	// is set; the bypass is unconditional for isExecutableClaim(c) when
	// the feature is enabled.
	c := Candidate{
		Title:    "parseSARIF cap is bypassed by multi-run SARIF documents",
		Severity: "low", // well below "critical"
	}
	var execs atomic.Int32
	var millis atomic.Int64
	if tool := f.buildSandboxTool(c, &execs, &millis); tool == nil {
		t.Error("executable claim at low severity should bypass the critical floor and get the tool")
	}
}

// TestBuildSandboxTool_DisabledStillGatedEvenForExecutable confirms the
// feature-off and nil-sandbox guards in buildSandboxTool are NOT touched by
// the bypass. Even an executable claim gets no tool when the feature is
// disabled, because there is no sandbox to run it in.
func TestBuildSandboxTool_DisabledStillGatedEvenForExecutable(t *testing.T) {
	_, repo := openFixture(t)
	f := &Funnel{
		repo: repo,
		opts: Options{SandboxOpts: SandboxOpts{
			Enabled:     false,
			Sandbox:     &funnelFakeSandbox{},
			MinSeverity: "high",
			MaxExecs:    3,
		}},
		lenses: selectLenses(nil),
	}
	c := Candidate{
		Title:    "parseSARIF cap is bypassed by multi-run SARIF documents",
		Severity: "medium",
	}
	var execs atomic.Int32
	var millis atomic.Int64
	if tool := f.buildSandboxTool(c, &execs, &millis); tool != nil {
		t.Error("buildSandboxTool returned a tool when the feature is disabled; only the floor can be bypassed, not the feature flag")
	}
}

// --- seat wiring tests ------------------------------------------------------

// TestSeatForCandidate_N1_NoClause proves the n==1 degraded path is preserved
// byte-identical: seatForCandidate must return the zero seat (no clause).
func TestSeatForCandidate_N1_NoClause(t *testing.T) {
	// Even with sandbox + executable claim, n==1 returns the empty seat so
	// TestRunRefuters_N1_NoSeatClause keeps passing.
	c := Candidate{
		Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
		Description: "cap is bypassed by multi-run SARIF documents",
		Severity:    "medium",
	}
	seat := seatForCandidate(0, 1, c, true)
	if seat.clause != "" {
		t.Errorf("n=1 seat clause must be empty, got %q", seat.clause)
	}
	if seat.name != "" {
		t.Errorf("n=1 seat name must be empty, got %q", seat.name)
	}
}

// TestSeatForCandidate_ExecutorAssigned proves the executor seat is returned
// for position 0 when n>=2, hasSandbox, and isExecutableClaim.
func TestSeatForCandidate_ExecutorAssigned(t *testing.T) {
	c := Candidate{
		Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
		Description: "cap is bypassed by multi-run SARIF documents",
		Severity:    "medium",
	}
	seat := seatForCandidate(0, 3, c, true)
	if seat.name != "executor" {
		t.Errorf("executable claim at i=0 should get executor seat, got %q", seat.name)
	}
	if seat.clause == "" {
		t.Error("executor seat must have a non-empty clause")
	}
	if !strings.Contains(seat.clause, "WRITE") && !strings.Contains(seat.clause, "write") {
		t.Error("executor clause must instruct the refuter to write a falsification test")
	}
}

// TestSeatForCandidate_NoSandbox_FallsBackToBuiltin proves the executor is
// only assigned when the sandbox_exec tool is available; without it, the
// fallback to seatForIndex preserves the reachability specialty.
func TestSeatForCandidate_NoSandbox_FallsBackToBuiltin(t *testing.T) {
	c := Candidate{
		Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
		Description: "cap is bypassed by multi-run SARIF documents",
		Severity:    "medium",
	}
	seat := seatForCandidate(0, 3, c, false)
	if seat.name == "executor" {
		t.Error("executor seat should NOT be assigned without sandbox_exec")
	}
	if seat.name != "reachability" {
		t.Errorf("expected fallback to reachability builtin, got %q", seat.name)
	}
}

// TestSeatForCandidate_NonExecutable_FallsBackToBuiltin proves the executor
// is only assigned for executable claims; non-executable claims keep the
// round-robin builtin even with sandbox enabled.
func TestSeatForCandidate_NonExecutable_FallsBackToBuiltin(t *testing.T) {
	c := Candidate{
		Title:       "data race on shared map in goroutine",
		Description: "Two goroutines write to the cache without synchronization.",
		Severity:    "medium",
	}
	seat := seatForCandidate(0, 3, c, true)
	if seat.name == "executor" {
		t.Error("executor seat should NOT be assigned for non-executable claims")
	}
	if seat.name != "reachability" {
		t.Errorf("expected fallback to reachability builtin, got %q", seat.name)
	}
}

// TestSeatForCandidate_OtherPositions_BuiltinSeats proves that when position
// 0 gets the executor seat, positions 1 and 2 keep their round-robin
// builtin specialties (semantics, guards). Diversity in the remaining seats
// is preserved.
func TestSeatForCandidate_OtherPositions_BuiltinSeats(t *testing.T) {
	c := Candidate{
		Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
		Description: "cap is bypassed by multi-run SARIF documents",
		Severity:    "medium",
	}
	// i=0: executor
	if s := seatForCandidate(0, 3, c, true); s.name != "executor" {
		t.Errorf("i=0 should be executor, got %q", s.name)
	}
	// i=1: round-robin builtin #1 = semantics
	if s := seatForCandidate(1, 3, c, true); s.name != "semantics" {
		t.Errorf("i=1 should be semantics, got %q", s.name)
	}
	// i=2: round-robin builtin #2 = guards
	if s := seatForCandidate(2, 3, c, true); s.name != "guards" {
		t.Errorf("i=2 should be guards, got %q", s.name)
	}
}

// --- prompt capture test ----------------------------------------------------

// TestRunRefuters_ExecutorClauseInPanelPrompts proves the executor seat
// clause appears in the system prompt for the executable-claim candidate
// when n>=2 and a sandbox is present, and does NOT appear when either
// condition is absent.
func TestRunRefuters_ExecutorClauseInPanelPrompts(t *testing.T) {
	ctx := context.Background()
	_, repo := openFixture(t)
	capture := &systemCaptureClient{response: notRefutedJSON}

	f := &Funnel{
		repo:    repo,
		opts:    Options{Limits: StageLimits{Refuters: 3}},
		lenses:  selectLenses(nil),
		clients: RoleClients{Verifier: capture},
	}

	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}

	// Build the parseSARIF candidate. Severity is medium to make sure the
	// claim is what gates the executor — not the severity.
	c := Candidate{
		Lens:        "analyzer",
		File:        "internal/analyzer/analyzer.go",
		Line:        567,
		Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
		Description: "cap is bypassed by multi-run SARIF documents",
		Severity:    "medium",
	}
	budget := &budgetState{}
	_, _, _, _, _, err = f.runRefuters(ctx, capture, tools, "senior Go engineer", c, 3, budget, progress.NewAgentScope(nil, progress.RoleVerifier, c.Title))
	if err != nil {
		t.Fatal(err)
	}

	if len(capture.captured) != 3 {
		t.Fatalf("expected 3 system prompts (one per seat), got %d", len(capture.captured))
	}

	// Without sandbox_exec in the tool list, no prompt should contain the
	// executor clause (hasSandbox=false → fallback to builtin).
	for i, p := range capture.captured {
		if strings.Contains(p, "YOUR REFUTATION SPECIALTY (executor)") {
			t.Errorf("prompt[%d] unexpectedly contains executor clause (no sandbox)\n%s", i, p)
		}
	}
}

// TestRunRefuters_ExecutorClauseWithSandboxTool proves the executor clause
// appears at position 0 (and only position 0) when the tool set includes
// sandbox_exec. We build a separate Funnel with the sandbox enabled and
// observe the captured system prompts.
func TestRunRefuters_ExecutorClauseWithSandboxTool(t *testing.T) {
	ctx := context.Background()
	_, repo := openFixture(t)
	capture := &systemCaptureClient{response: notRefutedJSON}

	sb := &funnelFakeSandbox{}
	f := &Funnel{
		repo:    repo,
		clients: RoleClients{Verifier: capture},
		opts: Options{
			Limits: StageLimits{Refuters: 3},
			SandboxOpts: SandboxOpts{
				Enabled:     true,
				Sandbox:     sb,
				MinSeverity: "high",
				MaxExecs:    3,
			},
		},
		lenses: selectLenses(nil),
	}

	// Build a tool list that includes the sandbox_exec tool. Reuse
	// buildSandboxTool for the parseSARIF candidate to make the test
	// self-consistent (it will bypass the floor and produce a real tool).
	c := Candidate{
		Lens:        "analyzer",
		File:        "internal/analyzer/analyzer.go",
		Line:        567,
		Title:       "parseSARIF cap is bypassed by multi-run SARIF documents",
		Description: "cap is bypassed by multi-run SARIF documents",
		Severity:    "medium",
	}
	var execs atomic.Int32
	var millis atomic.Int64
	sbTool := f.buildSandboxTool(c, &execs, &millis)
	if sbTool == nil {
		t.Fatal("buildSandboxTool should have produced a tool for the parseSARIF claim")
	}

	tools, err := f.readOnlyTools(agent.ReadCaps{})
	if err != nil {
		t.Fatal(err)
	}
	tools = append(tools, sbTool)

	budget := &budgetState{}
	_, _, _, _, _, err = f.runRefuters(ctx, capture, tools, "senior Go engineer", c, 3, budget, progress.NewAgentScope(nil, progress.RoleVerifier, c.Title))
	if err != nil {
		t.Fatal(err)
	}

	if len(capture.captured) != 3 {
		t.Fatalf("expected 3 system prompts, got %d", len(capture.captured))
	}

	// Position 0 must contain the executor clause.
	if !strings.Contains(capture.captured[0], "YOUR REFUTATION SPECIALTY (executor)") {
		t.Errorf("prompt[0] should contain executor clause for parseSARIF + sandbox, got:\n%.200s", capture.captured[0])
	}
	// Positions 1 and 2 must contain their builtin seat names.
	if !strings.Contains(capture.captured[1], "semantics") {
		t.Errorf("prompt[1] should contain semantics, got:\n%.200s", capture.captured[1])
	}
	if !strings.Contains(capture.captured[2], "guards") {
		t.Errorf("prompt[2] should contain guards, got:\n%.200s", capture.captured[2])
	}
	// The executor clause must appear in exactly one of the three prompts.
	executorCount := 0
	for _, p := range capture.captured {
		if strings.Contains(p, "YOUR REFUTATION SPECIALTY (executor)") {
			executorCount++
		}
	}
	if executorCount != 1 {
		t.Errorf("executor clause should appear in exactly 1 prompt, appeared in %d", executorCount)
	}
}

// --- end-to-end replay test: the FP incident --------------------------------

// TestReplay_ParseSARIFCap_RefutedByExecutor is the full end-to-end test
// for the bugbot-aud incident. It builds a medium-severity parseSARIF-cap
// claim (the same shape as the GH #64 false positive), enables the
// sandbox with a fake that returns a clean exit / cap-holds output, and
// uses a scripted verifier that issues sandbox_exec on the executor seat
// then returns refutedJSON. The candidate MUST be refuted (3/3) and
// dropped from findings — the executor seat kills the FP that a static
// re-read would have rubber-stamped.
func TestReplay_ParseSARIFCap_RefutedByExecutor(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	// Finder reports a single MEDIUM-severity parseSARIF candidate. This
	// is the candidate from the bugbot-aud incident (GH #64). Severity is
	// medium so the severity floor would normally block sandbox_exec — the
	// executable-claim bypass is what allows the executor seat to act.
	// The file is bug.go (in the fixture) so triage keeps it in-scope.
	const parseSARIFCand = `{"file":"bug.go","line":10,` +
		`"title":"parseSARIF cap is bypassed by multi-run SARIF documents",` +
		`"description":"parseSARIF appends past the cap when SARIF has multiple runs",` +
		`"severity":"medium","evidence":"cap is bypassed","confidence":"high",` +
		`"defect_kind":"bounds","subject":"parseSARIF"}`
	// boundary-conditions is a real builtin lens; the candidate's Lens
	// field is populated from the lens name (not the JSON), so the
	// resulting Candidate has Lens="boundary-conditions" with the
	// parseSARIF claim text — exactly the shape that exercises the
	// isExecutableClaim path.
	finder := newScriptedClient().onSystemContains(
		"boundary-conditions",
		candJSON(parseSARIFCand),
	)

	// Scripted verifier: first completion issues a sandbox_exec tool call
	// (as the executor's clause instructs); second completion (after the
	// tool result is fed back) returns a refutedJSON verdict. toolCallClient
	// does not care which seat the prompt is for; it always emits the tool
	// call first. This is the exact pattern used in
	// TestSweep_SandboxExec_StatsAggregated above.
	sbCallJSON := `{"cmd":["go","test","-run","TestParseSARIF","./internal/analyzer/..."],` +
		`"files":{}}`
	verifier := newToolCallClient(sbCallJSON, refutedJSON)

	// Fake sandbox returns the empirical refutation: clean exit (the cap
	// held) and stdout showing the 200 results under cap. In a real run
	// the executor would have written a small probe in /tmp via the
	// `files` argument; the fake just short-circuits to the answer.
	sb := &funnelFakeSandbox{
		result: sandbox.Result{
			ExitCode: 0,
			Stdout:   "200 results returned, cap respected\n",
			Duration: 12 * time.Millisecond,
		},
	}

	sandboxOpts := SandboxOpts{
		Sandbox:     sb,
		Enabled:     true,
		MinSeverity: "high", // floor; the parseSARIF claim is medium, so the
		// floor would gate it out — the executable-claim bypass
		// is what makes the sandbox tool available.
		MaxExecs: 3,
	}

	f, err := New(
		RoleClients{Finder: finder, Verifier: verifier},
		st, repo,
		Options{
			Discovery:   DiscoveryConfig{Lenses: []string{"boundary-conditions"}},
			Limits:      StageLimits{Refuters: 3},
			SandboxOpts: sandboxOpts,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The candidate must be refuted and dropped. Pre-fix, this was a
	// false-positive that survived: 3/3 refuters re-read the code and
	// rubber-stamped the report. With the executor seat, at least one
	// refuter writes a probe, sees the cap hold, and refutes — the panel
	// becomes 3/3 (or at least 2/3) and the candidate is killed.
	if len(res.Findings) != 0 {
		// Surface enough context to debug if the replay drifts.
		var titles []string
		for _, fnd := range res.Findings {
			titles = append(titles, fnd.Title)
		}
		t.Errorf("parseSARIF FP should be refuted (3/3 refuters with executor seat), but %d finding(s) survived: %v",
			len(res.Findings), titles)
	}

	// The sandbox must have been called at least once. With 3 refuters
	// all using the scripted tool-call client, we expect exactly 3 calls
	// (one per refuter's first completion).
	if sb.calls.Load() < 1 {
		t.Errorf("sandbox was called %d times; expected at least 1 (the executor's run)", sb.calls.Load())
	}

	// Stats must reflect the executions.
	if res.Stats.SandboxExecs < 1 {
		t.Errorf("Stats.SandboxExecs = %d, want >= 1", res.Stats.SandboxExecs)
	}
	if res.Stats.SandboxExecMillis <= 0 {
		t.Errorf("Stats.SandboxExecMillis = %d, want > 0", res.Stats.SandboxExecMillis)
	}

	// Sanity: the parseSARIF candidate must be counted in Killed, not
	// Verified. Pre-fix, this was the false-positive that survived: the
	// run's Verified counter would have included it.
	if res.Stats.Killed < 1 {
		t.Errorf("Stats.Killed = %d, want >= 1 (the parseSARIF candidate)", res.Stats.Killed)
	}
}
