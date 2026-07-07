package tui

import (
	"context"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// ── Fuzzy ranking tests ───────────────────────────────────────────────────────

func TestFuzzyScore_ExactSubstring(t *testing.T) {
	// "candidate" is a mid-string substring of the target — not a word prefix
	// (the word is "candidate" itself, but the query starts a word: it IS a
	// word prefix too). Use a query that is NOT a word start to force exact.
	// "andidate" is a substring but not a word prefix → must be matchExactSubstring.
	score, ok := fuzzyScore("verifier candidate A", "andidate")
	if !ok {
		t.Fatal("expected match")
	}
	if score != matchExactSubstring {
		t.Fatalf("want matchExactSubstring for mid-word substring, got %v", score)
	}
}

// TestFuzzyScore_WordPrefixBeatsSubstring verifies that a query which is both
// a word-prefix AND an inner substring of the full target string is classified
// as matchWordPrefix (N2: word-prefix is checked before Contains).
func TestFuzzyScore_WordPrefixBeatsSubstring(t *testing.T) {
	// "nil" is a prefix of word "nil-safety" AND a substring of the full string
	// "nil-safety finder". After N2 fix, word-prefix is checked first, so it
	// returns matchWordPrefix (better than exact substring).
	score, ok := fuzzyScore("nil-safety finder", "nil")
	if !ok {
		t.Fatal("expected match")
	}
	if score != matchWordPrefix {
		t.Fatalf("want matchWordPrefix (word-prefix checked before Contains), got %v", score)
	}
}

// TestFuzzyScore_WordPrefixOnlyTier verifies the 3-tier ordering:
// exact-substring (0) < word-prefix (1) < subsequence (2).
// (Lower matchScore value = better rank; exact-substring ranks best.)
func TestFuzzyScore_WordPrefixOnlyTier(t *testing.T) {
	// word-prefix score should be strictly better than subsequence score.
	wpScore, wpOk := fuzzyScore("verifier alpha", "ver") // "ver" is word-prefix of "verifier"
	ssScore, ssOk := fuzzyScore("abc def", "ad")         // "ad" is only a scattered subsequence

	if !wpOk {
		t.Fatal("expected word-prefix match")
	}
	if !ssOk {
		t.Fatal("expected subsequence match")
	}
	if wpScore >= ssScore {
		t.Errorf("word-prefix (%v) should rank better (lower score) than subsequence (%v)", wpScore, ssScore)
	}

	// exact-substring score should be strictly better than subsequence.
	exScore, exOk := fuzzyScore("abcdef", "bcd") // "bcd" is NOT a word prefix; it IS a substring
	if !exOk {
		t.Fatal("expected exact-substring match")
	}
	if exScore >= ssScore {
		t.Errorf("exact-substring (%v) should rank better (lower) than subsequence (%v)", exScore, ssScore)
	}
}

func TestFuzzyScore_Subsequence(t *testing.T) {
	score, ok := fuzzyScore("verifier alpha", "eia")
	if !ok {
		t.Fatal("expected match for scattered subsequence")
	}
	if score != matchSubsequence {
		t.Fatalf("want matchSubsequence, got %v", score)
	}
}

func TestFuzzyScore_NoMatch(t *testing.T) {
	_, ok := fuzzyScore("verifier alpha", "zzz")
	if ok {
		t.Fatal("expected no match")
	}
}

func TestFuzzyScore_EmptyQuery(t *testing.T) {
	_, ok := fuzzyScore("anything", "")
	if !ok {
		t.Fatal("empty query should match everything")
	}
}

func TestFuzzyScore_CaseInsensitive(t *testing.T) {
	_, ok := fuzzyScore("Verifier Alpha", "VERIFIER")
	if !ok {
		t.Fatal("expected case-insensitive match")
	}
}

// TestFilterCandidates_RankingOrder checks that word-prefix outranks scattered
// subsequence, and that exact-substring outranks scattered subsequence.
func TestFilterCandidates_RankingOrder(t *testing.T) {
	// candidate A: "verifier alpha" — "ver" is a word-prefix of "verifier"
	// candidate B: "vxexr xfoo"    — "ver" is only a scattered subsequence
	candidates := []cmdCandidate{
		{kind: cmdKindAgent, label: "B", display: "vxexr xfoo"},     // subsequence only
		{kind: cmdKindAgent, label: "A", display: "verifier alpha"}, // word-prefix (of "verifier")
	}
	results := filterCandidates(candidates, "ver")
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	// "verifier alpha" has "ver" as a word-prefix → better rank → first.
	if results[0].label != "A" {
		t.Errorf("word-prefix match should rank first, got %q", results[0].label)
	}
}

func TestFilterCandidates_NoMatch(t *testing.T) {
	candidates := []cmdCandidate{
		{display: "verifier alpha"},
	}
	results := filterCandidates(candidates, "zzz")
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// ── Command bar open / type / navigate tests ──────────────────────────────────

// agentFrame builds a Frame with two live agents, for command bar tests.
func agentFrame() Frame {
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)
	return Frame{
		HasSnapshot: true,
		Snapshot: progress.Status{
			ActiveAgents: []progress.AgentStatus{
				{Role: "verifier", Label: "candidate A", Started: t0, ActivityAt: t1},
				{Role: "finder", Label: "nil-safety", Started: time.Unix(500, 0)},
			},
		},
		Agents: []AgentView{
			{Role: "verifier", Label: "candidate A", Live: true, Started: t0, ActivityAt: t1},
			{Role: "finder", Label: "nil-safety", Live: true, Started: time.Unix(500, 0)},
		},
	}
}

func TestCmdBar_CtrlP_Opens(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")
	if !m.cmdBar.open {
		t.Fatal("command bar should be open after ctrl+p")
	}
}

// TestCmdBar_CtrlP_AlreadyOpen verifies that ctrl+p while the bar is already
// open is a no-op — it does not wipe the current query (N3).
func TestCmdBar_CtrlP_AlreadyOpen(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")
	// Type "nil" to set a query.
	m = sendKey(m, "n")
	m = sendKey(m, "i")
	m = sendKey(m, "l")
	queryBefore := m.cmdBar.input.Value()
	resultsBefore := len(m.cmdBar.results)

	// Second ctrl+p should be a no-op.
	m = sendKey(m, "ctrl+p")
	if m.cmdBar.input.Value() != queryBefore {
		t.Errorf("ctrl+p while open should not wipe query: before=%q after=%q", queryBefore, m.cmdBar.input.Value())
	}
	if len(m.cmdBar.results) != resultsBefore {
		t.Errorf("ctrl+p while open should not change results")
	}
}

func TestCmdBar_Esc_Closes(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")
	m = sendKey(m, "esc")
	if m.cmdBar.open {
		t.Fatal("command bar should be closed after esc")
	}
	// Focus and selection unchanged.
	if m.focus != paneRoster {
		t.Errorf("focus should remain paneRoster, got %v", m.focus)
	}
	if m.detailKey != "" {
		t.Errorf("detailKey should be empty after esc, got %q", m.detailKey)
	}
}

// TestCmdBar_TypeFilters asserts that typing a query narrows the result list:
// the matching candidate is included and a non-matching one is excluded (N4).
func TestCmdBar_TypeFilters(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")

	// agentFrame has two agents: "verifier candidate A" and "finder nil-safety".
	// Type "nil" — should match "finder nil-safety" but NOT "verifier candidate A".
	m = sendKey(m, "n")
	m = sendKey(m, "i")
	m = sendKey(m, "l")

	if len(m.cmdBar.results) == 0 {
		t.Fatal("expected at least one result after typing 'nil'")
	}

	// The "verifier candidate A" agent should NOT appear in results (N4).
	for _, r := range m.cmdBar.results {
		if r.kind == cmdKindAgent && r.agentKey == agentKey(m.frame.Agents[0]) {
			t.Errorf("'verifier candidate A' should be excluded after typing 'nil', but it appears in results")
		}
	}

	// The "finder nil-safety" agent SHOULD appear.
	found := false
	nilSafetyKey := agentKey(m.frame.Agents[1])
	for _, r := range m.cmdBar.results {
		if r.kind == cmdKindAgent && r.agentKey == nilSafetyKey {
			found = true
			break
		}
	}
	if !found {
		t.Error("'finder nil-safety' should be included after typing 'nil'")
	}
}

func TestCmdBar_Enter_NavigatesToAgent(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")
	// Without typing, first candidate should be first agent.
	// Press enter to select.
	m = sendKey(m, "enter")
	if m.cmdBar.open {
		t.Fatal("command bar should close on enter")
	}
	if m.focus != paneDetail {
		t.Errorf("focus should be paneDetail after agent selection, got %v", m.focus)
	}
	if m.detailKey == "" {
		t.Error("detailKey should be set after selecting agent")
	}
}

// TestCmdBar_Enter_NavigatesToFinding verifies that selecting a finding
// candidate focuses paneContext in contextModeFindings at the right cursor (B1).
func TestCmdBar_Enter_NavigatesToFinding(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	fr := Frame{
		HasSnapshot: true,
		Agents:      []AgentView{},
		World: WorldState{
			Findings: []domain.Finding{
				{ID: "f1", Title: "nil dereference", File: "pkg/foo.go"},
				{ID: "f2", Title: "use after free", File: "pkg/bar.go"},
			},
			FindingsTotal: 2,
			HasTallies:    true,
		},
	}
	m = sendFrame(m, fr)
	m = sendKey(m, "ctrl+p")

	// Expect two finding candidates.
	var findingCandidates int
	for _, r := range m.cmdBar.results {
		if r.kind == cmdKindFinding {
			findingCandidates++
		}
	}
	if findingCandidates != 2 {
		t.Fatalf("expected 2 finding candidates, got %d (total results: %d)", findingCandidates, len(m.cmdBar.results))
	}

	// Select first finding by navigating to it.
	m.cmdBar.cursor = 0
	m = sendKey(m, "enter")

	if m.cmdBar.open {
		t.Fatal("command bar should close on enter")
	}
	if m.focus != paneContext {
		t.Errorf("focus should be paneContext after finding selection, got %v", m.focus)
	}
	if m.contextMode != contextModeFindings {
		t.Errorf("contextMode should be contextModeFindings, got %v", m.contextMode)
	}
}

// TestFindings_JKMovesCursor verifies that j/k advance and retreat the cursor
// in contextModeFindings (scrollDown/scrollUp route to moveCursor, not the
// never-rendered contextView viewport).
func TestFindings_JKMovesCursor(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	fr := Frame{
		Agents: []AgentView{},
		World: WorldState{
			Findings:      []domain.Finding{{ID: "f1"}, {ID: "f2"}},
			FindingsTotal: 2,
			HasTallies:    true,
		},
	}
	m = sendFrame(m, fr)
	m.focus = paneContext
	m.contextMode = contextModeFindings
	m.cursor = 0

	m = sendKey(m, "j")
	if m.cursor != 1 {
		t.Errorf("j should advance findings cursor to 1, got %d", m.cursor)
	}
	m = sendKey(m, "k")
	if m.cursor != 0 {
		t.Errorf("k should retreat findings cursor to 0, got %d", m.cursor)
	}
}

func TestCmdBar_Enter_NavigatesToLead(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	fr := Frame{
		HasSnapshot: true,
		Agents:      []AgentView{},
		World: WorldState{
			PendingLeads: []store.Lead{
				{ID: "l1", TargetLens: "nil-safety", File: "pkg/foo.go", Note: "check nil"},
			},
			PendingLeadsTotal: 1,
		},
	}
	m = sendFrame(m, fr)
	m = sendKey(m, "ctrl+p")
	// Only candidate is the lead.
	if len(m.cmdBar.results) != 1 {
		t.Fatalf("expected 1 candidate (the lead), got %d", len(m.cmdBar.results))
	}
	if m.cmdBar.results[0].kind != cmdKindLead {
		t.Fatal("expected lead candidate")
	}
	m = sendKey(m, "enter")
	if m.cmdBar.open {
		t.Fatal("command bar should close on enter")
	}
	if m.focus != paneContext {
		t.Errorf("focus should be paneContext after lead selection, got %v", m.focus)
	}
	if m.contextMode != contextModeLeads {
		t.Errorf("contextMode should be contextModeLeads, got %v", m.contextMode)
	}
	if m.cursor != 0 {
		t.Errorf("cursor should be 0 for first lead, got %d", m.cursor)
	}
}

// TestCmdBar_LeadReResolveByID verifies that N6 stable-ID re-resolution works:
// if a lead reorders in the frame while the bar is open, navigation still
// lands on the correct lead by ID.
func TestCmdBar_LeadReResolveByID(t *testing.T) {
	lead1 := store.Lead{ID: "l1", TargetLens: "nil-safety", File: "a.go", Note: "check nil"}
	lead2 := store.Lead{ID: "l2", TargetLens: "bounds", File: "b.go", Note: "bounds check"}

	m := NewModel(context.Background(), &fakeFeed{}, nil)
	fr1 := Frame{
		Agents: []AgentView{},
		World: WorldState{
			PendingLeads:      []store.Lead{lead1, lead2},
			PendingLeadsTotal: 2,
		},
	}
	m = sendFrame(m, fr1)
	m = sendKey(m, "ctrl+p")

	// Select second lead (lead2, index 1) in the bar.
	if len(m.cmdBar.results) < 2 {
		t.Fatalf("expected ≥2 candidates, got %d", len(m.cmdBar.results))
	}
	// Find lead2 candidate in results.
	lead2Pos := -1
	for i, r := range m.cmdBar.results {
		if r.kind == cmdKindLead && r.leadID == "l2" {
			lead2Pos = i
			break
		}
	}
	if lead2Pos < 0 {
		t.Fatal("lead2 not found in results")
	}
	m.cmdBar.cursor = lead2Pos

	// Simulate frame refresh that reorders leads (lead2 now at index 0).
	fr2 := Frame{
		Agents: []AgentView{},
		World: WorldState{
			PendingLeads:      []store.Lead{lead2, lead1}, // reversed
			PendingLeadsTotal: 2,
		},
	}
	m = sendFrame(m, fr2)

	// The bar is now closed (sendFrame goes through Update/FrameMsg, which
	// doesn't close the bar). Re-open is NOT needed — bar stays open across frames.
	// But sendFrame calls Update(FrameMsg) which updates m.frame; the bar itself
	// keeps its candidate snapshot from openCmdBar time. Navigate now:
	m = sendKey(m, "enter")

	// lead2 is now at index 0 in m.frame.World.PendingLeads; re-resolution by ID
	// should place cursor at 0.
	if m.cursor != 0 {
		t.Errorf("lead2 should re-resolve to index 0 after frame reorder, cursor=%d", m.cursor)
	}
	if m.contextMode != contextModeLeads {
		t.Errorf("contextMode should be contextModeLeads, got %v", m.contextMode)
	}
}

func TestCmdBar_JKMovesSelection(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")
	if len(m.cmdBar.results) < 2 {
		t.Skip("need at least 2 candidates")
	}
	if m.cmdBar.cursor != 0 {
		t.Fatalf("initial cursor should be 0, got %d", m.cmdBar.cursor)
	}
	m = sendKey(m, "j")
	if m.cmdBar.cursor != 1 {
		t.Errorf("cursor should be 1 after j, got %d", m.cmdBar.cursor)
	}
	m = sendKey(m, "k")
	if m.cmdBar.cursor != 0 {
		t.Errorf("cursor should be 0 after k, got %d", m.cmdBar.cursor)
	}
}

// ── Follow-active-agent tests ─────────────────────────────────────────────────

// twoAgentFrameWithActivity returns a Frame with two live agents at the given ActivityAt times.
func twoAgentFrameWithActivity(aActivityAt, bActivityAt time.Time) Frame {
	return Frame{
		HasSnapshot: true,
		Snapshot: progress.Status{
			ActiveAgents: []progress.AgentStatus{
				{Role: "verifier", Label: "A", Started: time.Unix(1000, 0), ActivityAt: aActivityAt},
				{Role: "finder", Label: "B", Started: time.Unix(2000, 0), ActivityAt: bActivityAt},
			},
		},
		Agents: []AgentView{
			{Role: "verifier", Label: "A", Live: true, Started: time.Unix(1000, 0), ActivityAt: aActivityAt},
			{Role: "finder", Label: "B", Live: true, Started: time.Unix(2000, 0), ActivityAt: bActivityAt},
		},
	}
}

func TestFollowActive_Toggle(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	if m.followActive {
		t.Fatal("follow should be off initially")
	}
	m = sendKey(m, "F")
	if !m.followActive {
		t.Fatal("follow should be on after F")
	}
	m = sendKey(m, "F")
	if m.followActive {
		t.Fatal("follow should be off after second F")
	}
}

func TestFollowActive_AutoSelectsMostRecentAgent(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)

	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)

	// Frame 1: agent A (idx=0) most recently active.
	fr1 := twoAgentFrameWithActivity(t2, t1)
	m = sendFrame(m, fr1)

	// Enable follow.
	m = sendKey(m, "F")
	if !m.followActive {
		t.Fatal("follow should be on")
	}
	// applyFollowActive is called on F if frame is present.
	aKey := agentKey(fr1.Agents[0])
	if m.detailKey != aKey {
		t.Errorf("follow should select agent A (most active), detailKey=%q want %q", m.detailKey, aKey)
	}
	if m.focus != paneDetail {
		t.Errorf("focus should be paneDetail, got %v", m.focus)
	}

	// Frame 2: agent B (idx=1) becomes more recently active.
	fr2 := twoAgentFrameWithActivity(t1, t2)
	m = sendFrame(m, fr2)

	bKey := agentKey(fr2.Agents[1])
	if m.detailKey != bKey {
		t.Errorf("follow should switch to agent B after frame update, detailKey=%q want %q", m.detailKey, bKey)
	}
}

func TestFollowActive_ManualNavDisablesFollow(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)

	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	fr := twoAgentFrameWithActivity(t2, t1)
	m = sendFrame(m, fr)
	m = sendKey(m, "F")
	if !m.followActive {
		t.Fatal("follow should be on")
	}

	// Manual j navigation in roster pane should disable follow.
	m.focus = paneRoster
	m = sendKey(m, "j")
	if m.followActive {
		t.Error("manual j navigation should disable follow-active")
	}
}

func TestFollowActive_ManualEnterDisablesFollow(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)

	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	fr := twoAgentFrameWithActivity(t2, t1)
	m = sendFrame(m, fr)
	m = sendKey(m, "F")
	if !m.followActive {
		t.Fatal("follow should be on")
	}

	// Manual enter drill-in should disable follow.
	m.focus = paneRoster
	m.cursor = 0
	m = sendKey(m, "enter")
	if m.followActive {
		t.Error("manual enter drill-in should disable follow-active")
	}
}

func TestFollowActive_CmdBarAgentNavigationDisablesFollow(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	fr := agentFrame()
	m = sendFrame(m, fr)
	m = sendKey(m, "F")
	if !m.followActive {
		t.Fatal("follow should be on")
	}

	// Open command bar, enter on first agent.
	m = sendKey(m, "ctrl+p")
	m = sendKey(m, "enter")
	if m.followActive {
		t.Error("cmdBar agent navigation should disable follow-active")
	}
}

func TestMostRecentlyActiveAgent_NoLive(t *testing.T) {
	views := []AgentView{
		{Role: "finder", Live: false},
	}
	got := mostRecentlyActiveAgent(views)
	if got != -1 {
		t.Errorf("want -1 when no live agents, got %d", got)
	}
}

func TestMostRecentlyActiveAgent_PicksLatest(t *testing.T) {
	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	views := []AgentView{
		{Role: "A", Live: true, ActivityAt: t1},
		{Role: "B", Live: true, ActivityAt: t2},
	}
	got := mostRecentlyActiveAgent(views)
	if got != 1 {
		t.Errorf("want index 1 (most recent ActivityAt), got %d", got)
	}
}
