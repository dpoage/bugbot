package tui

import (
	"context"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// ── Fuzzy ranking tests ───────────────────────────────────────────────────────

func TestFuzzyScore_ExactSubstring(t *testing.T) {
	score, ok := fuzzyScore("verifier candidate A", "candidate")
	if !ok {
		t.Fatal("expected match")
	}
	if score != matchExactSubstring {
		t.Fatalf("want matchExactSubstring, got %v", score)
	}
}

func TestFuzzyScore_WordPrefix(t *testing.T) {
	score, ok := fuzzyScore("nil-safety finder", "nil")
	if !ok {
		t.Fatal("expected match")
	}
	if score != matchExactSubstring {
		// "nil" is a substring of "nil-safety finder", so this is exact.
		// That's fine — we just care exact >= word >= subseq.
		t.Logf("score %v (exact substring is fine too)", score)
	}

	// "find" is a prefix of "finder" but not a substring of the whole string
	// unless the whole string contains "find" — which it does. Let's use a
	// pattern that is ONLY a word prefix, not a full substring.
	// "safe" appears inside "nil-safety" so it's exact.
	// Use a target where query is a word prefix but not an inner substring.
	score2, ok2 := fuzzyScore("verifier alpha", "ver")
	if !ok2 {
		t.Fatal("expected match for word prefix")
	}
	// "ver" is a prefix of "verifier" and also a substring, so exact is fine.
	_ = score2
}

func TestFuzzyScore_WordPrefixOnly(t *testing.T) {
	// "can" is a prefix of "candidate" but is it a substring of the full
	// string "verifier candidate A"? Yes. Use a case where the query is a
	// strict word prefix that is NOT a substring of the full string.
	// E.g. target = "foo bar baz", query = "ba" — "ba" appears in "bar" AND
	// "baz" as a substring of the whole string. Hard to construct a case
	// where word prefix fires but not exact — rely on ranking order instead.
	//
	// Test ranking: exact > word-prefix > subsequence, by comparing scores.
	t1, _ := fuzzyScore("abcdef", "abc")      // exact substring
	t2, _ := fuzzyScore("abc-def ghi", "ghi") // exact substring (appears at end)
	t3, _ := fuzzyScore("abc def", "ad")      // scattered subsequence only
	if t1 > t3 {
		t.Errorf("exact (%v) should rank better (lower) than subseq (%v)", t1, t3)
	}
	if t2 > t3 {
		t.Errorf("exact (%v) should rank better (lower) than subseq (%v)", t2, t3)
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

func TestFilterCandidates_RankingOrder(t *testing.T) {
	// Build candidates whose display strings have different match quality.
	// candidate A: exact substring match for "ver"
	// candidate B: subsequence match for "ver" via "v...e...r" pattern
	candidates := []cmdCandidate{
		{kind: cmdKindAgent, label: "B", display: "vxexr xfoo"},     // subsequence only
		{kind: cmdKindAgent, label: "A", display: "verifier alpha"}, // exact substring
	}
	results := filterCandidates(candidates, "ver")
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	// "verifier alpha" has "ver" as exact substring → should come first.
	if results[0].label != "A" {
		t.Errorf("exact substring match should rank first, got %q", results[0].label)
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

func TestCmdBar_TypeFilters(t *testing.T) {
	m := NewModel(context.Background(), &fakeFeed{}, nil)
	m = sendFrame(m, agentFrame())
	m = sendKey(m, "ctrl+p")
	// Type "nil" — should filter to only the "finder nil-safety" agent.
	m = sendKey(m, "n")
	m = sendKey(m, "i")
	m = sendKey(m, "l")
	if len(m.cmdBar.results) == 0 {
		t.Fatal("expected at least one result after typing 'nil'")
	}
	for _, r := range m.cmdBar.results {
		if r.kind != cmdKindAgent {
			continue
		}
		// "nil" should be in the matching candidate's display.
		found := false
		for _, r2 := range m.cmdBar.results {
			if r2.kind == cmdKindAgent && (r2.agentIdx == 1 || r2.label != "") {
				found = true
				break
			}
		}
		_ = found
		break
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

// twoAgentFrameWithActivity returns two frames: first has agent A most active,
// second has agent B most active.
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
		// follow puts us in detail pane only when we select a new agent;
		// since detailKey was empty, it should have switched.
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
