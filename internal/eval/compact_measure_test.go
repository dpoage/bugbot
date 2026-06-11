package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/llm"
)

// This file is the offline measurement harness for bugbot-3nf (finder token
// burn). It simulates the per-finder INPUT token cost of the agent loop over the
// committed recorded finder corpus, both raw and cache-weighted (append-only
// prompt-cache model: a request's prefix that matches the previous request bills
// at cacheReadWeight, the new tail at full price). It reports the cost BEFORE and
// AFTER applying the live history-compaction policy
// (agent.SimulateCompaction), and additionally over a synthetic "runaway"
// profile matching the dogfood finding (~8-9 turns at ~60k input tokens/turn)
// that the small recorded fixtures do not exhibit.
//
// The numbers are printed (run `go test -run TestMeasure -v ./internal/eval/`)
// AND asserted: the recorded corpus must not get WORSE (compaction is byte-for-
// byte inert on short runs), and the runaway profile must show a material cache-
// weighted reduction.

// measureCacheWeight is the append-only prompt-cache discount used for the
// weighted cost model — the same 0.1 the budget uses (DefaultCacheReadBudgetWeight).
const measureCacheWeight = 0.1

// costResult is the input-token cost of one finder unit under one policy.
type costResult struct {
	raw      float64
	weighted float64
	turns    int
}

// simulateCost computes the append-only input-token cost of a sequence of
// request message snapshots. Each snapshot's prefix that is byte-identical to the
// previous snapshot's leading messages is a cache hit (billed at cacheWeight);
// the remainder bills at full price. Token counts use the bytes/4 heuristic via
// agent.EstimateHistoryTokens, so they agree with the live compaction trigger.
func simulateCost(snaps [][]llm.Message, cacheWeight float64) costResult {
	var raw, weighted float64
	var prev []llm.Message
	for _, msgs := range snaps {
		reqTok := float64(agent.EstimateHistoryTokens(msgs))
		raw += reqTok
		common := commonPrefixLen(prev, msgs)
		cachedTok := float64(agent.EstimateHistoryTokens(msgs[:common]))
		newTok := reqTok - cachedTok
		if newTok < 0 {
			newTok = 0
		}
		weighted += newTok + cacheWeight*cachedTok
		prev = msgs
	}
	return costResult{raw: raw, weighted: weighted, turns: len(snaps)}
}

// commonPrefixLen returns how many leading messages of a and b are identical. For
// append-only history b extends a, so this is the cached prefix length; once
// compaction mutates an earlier message the prefix breaks here exactly as the
// provider's cache would invalidate.
func commonPrefixLen(a, b []llm.Message) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for ; i < n; i++ {
		if !messageEqual(a[i], b[i]) {
			break
		}
	}
	return i
}

func messageEqual(x, y llm.Message) bool {
	if x.Role != y.Role || x.Content != y.Content || x.ToolCallID != y.ToolCallID || x.IsError != y.IsError {
		return false
	}
	if len(x.ToolCalls) != len(y.ToolCalls) {
		return false
	}
	for i := range x.ToolCalls {
		if x.ToolCalls[i].ID != y.ToolCalls[i].ID ||
			x.ToolCalls[i].Name != y.ToolCalls[i].Name ||
			string(x.ToolCalls[i].Arguments) != string(y.ToolCalls[i].Arguments) {
			return false
		}
	}
	return true
}

// requestSnapshots extracts the per-turn request message snapshots from a finder
// transcript, in order. Each EventRequest already stores the full conversation
// sent that turn (transcript.go), which is exactly the append-only history.
func requestSnapshots(tr *agent.Transcript) [][]llm.Message {
	var snaps [][]llm.Message
	for _, ev := range tr.Events {
		if ev.Kind == agent.EventRequest {
			snaps = append(snaps, ev.Messages)
		}
	}
	return snaps
}

// toolNameMap reconstructs ToolCallID -> tool name from a transcript's assistant
// tool calls, so compaction stubs can name their tool just like the live loop.
func toolNameMap(tr *agent.Transcript) map[string]string {
	names := map[string]string{}
	for _, ev := range tr.Events {
		if ev.Kind == agent.EventAssistant {
			for _, tc := range ev.ToolCalls {
				names[tc.ID] = tc.Name
			}
		}
	}
	return names
}

// applyCompaction transforms a turn-by-turn snapshot sequence through the live
// compaction policy, threading the threshold and tool-name map exactly as the
// Runner does. Critically it is STATEFUL across turns: a result stubbed on an
// earlier turn stays stubbed on every later turn, because the live Runner mutates
// its single carried-forward messages slice in place rather than re-deriving
// history from scratch each turn. We reproduce that by propagating prior stubs
// (keyed by ToolCallID) into each subsequent snapshot before re-evaluating the
// threshold — otherwise the simulation would "un-prune" a result the live loop
// has already shed, badly overstating cost and misjudging the cache tradeoff.
func applyCompaction(snaps [][]llm.Message, names map[string]string, budget int64) [][]llm.Message {
	out := make([][]llm.Message, len(snaps))
	threshold := budget
	stubbed := map[string]string{} // ToolCallID -> stub content carried forward
	for i, msgs := range snaps {
		cur := cloneMsgs(msgs)
		// Re-apply prior stubs so this snapshot reflects the carried-forward state.
		for j := range cur {
			if cur[j].Role == llm.RoleToolResult {
				if s, ok := stubbed[cur[j].ToolCallID]; ok {
					cur[j].Content = s
				}
			}
		}
		compacted, next := agent.SimulateCompaction(cur, budget, threshold, agent.CompactRecentToolResults, names)
		// Record any NEW stubs produced this turn so later turns keep them.
		for j := range compacted {
			m := compacted[j]
			if m.Role == llm.RoleToolResult && strings.HasPrefix(m.Content, "[") {
				stubbed[m.ToolCallID] = m.Content
			}
		}
		out[i] = compacted
		threshold = next
	}
	return out
}

func TestMeasureFinderTokenBurn(t *testing.T) {
	// This harness reads only the committed JSONL corpus (no git, no network), so
	// it runs in every `make test` invocation and its printed numbers are
	// reproducible.
	dir := DefaultRecordedDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no recorded corpus at %q: %v", dir, err)
	}

	// Use the funnel's default finder threshold so the measurement reflects the
	// shipped policy.
	const budget = 60_000 // funnel.DefaultFinderHistoryTokens; kept local to avoid an import cycle of intent

	var beforeRaw, beforeW, afterRaw, afterW float64
	units := 0
	t.Logf("%-44s %5s %9s %9s %9s %9s", "finder unit", "turns", "raw_b", "raw_a", "cw_b", "cw_a")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		caseDir := filepath.Join(dir, e.Name())
		paths, _ := filepath.Glob(filepath.Join(caseDir, "finder-*.jsonl"))
		for _, p := range paths {
			tr := loadTranscriptOrFail(t, p)
			snaps := requestSnapshots(tr)
			if len(snaps) == 0 {
				continue
			}
			names := toolNameMap(tr)
			before := simulateCost(snaps, measureCacheWeight)
			after := simulateCost(applyCompaction(snaps, names, budget), measureCacheWeight)

			beforeRaw += before.raw
			beforeW += before.weighted
			afterRaw += after.raw
			afterW += after.weighted
			units++
			rel := e.Name() + "/" + strings.TrimSuffix(filepath.Base(p), ".jsonl")
			t.Logf("%-44s %5d %9.0f %9.0f %9.0f %9.0f", rel, before.turns,
				before.raw, after.raw, before.weighted, after.weighted)
		}
	}
	if units == 0 {
		t.Skip("recorded corpus present but no finder transcripts")
	}

	t.Logf("RECORDED CORPUS (%d finder units), cache weight %.2f:", units, measureCacheWeight)
	t.Logf("  per-unit RAW input tokens:  before=%.0f after=%.0f", beforeRaw/float64(units), afterRaw/float64(units))
	t.Logf("  per-unit CACHE-WEIGHTED:    before=%.0f after=%.0f", beforeW/float64(units), afterW/float64(units))

	// Recall/cost safety: the small recorded fixtures sit well under the 60k
	// threshold, so compaction MUST be byte-for-byte inert — neither raw nor
	// cache-weighted cost may rise (or fall) on the real corpus.
	if afterRaw != beforeRaw {
		t.Errorf("compaction changed RAW cost on recorded corpus (%.0f -> %.0f); short runs must be untouched", beforeRaw, afterRaw)
	}
	if afterW != beforeW {
		t.Errorf("compaction changed CACHE-WEIGHTED cost on recorded corpus (%.0f -> %.0f); short runs must be untouched", beforeW, afterW)
	}

	// Now the SYNTHETIC profile the recorded fixtures do not exercise: a runaway
	// finder of ~9 turns reading large source files. This is a BEST-CASE UPPER
	// BOUND — it assumes every read_file call saturates the cap and savings equal
	// the truncated content. The recorded corpus never exercises the read-cap lever
	// because its files are well below the caps, so the percentage reductions
	// reported below are NOT corpus measurements. At the looser AGENT default
	// (2000 lines) such a file lands around ~24k tokens; the re-sent history grows
	// toward ~200k by the later turns. This is the shape bugbot-3nf measured in
	// dogfood. We compare the TWO candidate levers on it. We approximate "tokens
	// per fully-read file" as a small multiple of the line cap (numbered source
	// lines run ~12 tokens each here), so the agent default (2000 lines) and the
	// tighter finder default (DefaultFinderReadLines) give the before/after
	// per-read sizes directly.
	const tokPerLine = 12
	baseReadTokens := 2000 * tokPerLine                         // looser agent default per read
	capReadTokens := funnel.DefaultFinderReadLines * tokPerLine // tighter finder default

	base := buildRunaway(9, baseReadTokens)
	beforeCost := simulateCost(base, measureCacheWeight)

	// Lever A (DEFAULT, cache-safe): tighter per-read caps shrink each result at
	// the source, so no message is ever mutated and the prompt-cache prefix is
	// fully preserved — every saved byte is saved at full proportional value.
	capped := buildRunaway(9, capReadTokens)
	capCost := simulateCost(capped, measureCacheWeight)

	// Lever B (OPT-IN): threshold history compaction at the funnel default.
	compCost := simulateCost(applyCompaction(base, runawayNames(8), budget), measureCacheWeight)

	t.Logf("RUNAWAY PROFILE (9 turns, ~24k-token reads), cache weight %.2f:", measureCacheWeight)
	t.Logf("  baseline:                   raw=%.0f cw=%.0f", beforeCost.raw, beforeCost.weighted)
	t.Logf("  READ-CAP lever (default):   raw=%.0f (%.1f%%)  cw=%.0f (%.1f%%)",
		capCost.raw, pct(beforeCost.raw, capCost.raw), capCost.weighted, pct(beforeCost.weighted, capCost.weighted))
	t.Logf("  COMPACTION lever (opt-in):  raw=%.0f (%.1f%%)  cw=%.0f (%.1f%%)",
		compCost.raw, pct(beforeCost.raw, compCost.raw), compCost.weighted, pct(beforeCost.weighted, compCost.weighted))

	// ACCEPTANCE (bead bugbot-3nf): the DEFAULT lever must cut cache-weighted
	// input materially without recall loss. Read-caps shrink each result at the
	// source, preserving the cache, so they deliver this.
	if got := pct(beforeCost.weighted, capCost.weighted); got < 25 {
		t.Errorf("read-cap lever cut cache-weighted cost only %.1f%%, expected material (>=25%%)", got)
	}

	// FINDING (documented, asserted): compaction reduces RAW tokens but does NOT
	// materially reduce cache-weighted cost under a strong cache — it can raise it,
	// because mutating the prefix forfeits cache hits worth more than the reclaimed
	// bytes. This is WHY compaction is opt-in/off and read-caps is the default.
	if pct(beforeCost.raw, compCost.raw) <= 0 {
		t.Errorf("expected compaction to reduce RAW tokens, got %.1f%%", pct(beforeCost.raw, compCost.raw))
	}
	if pct(beforeCost.weighted, compCost.weighted) >= pct(beforeCost.weighted, capCost.weighted) {
		t.Errorf("compaction unexpectedly beat read-caps on cache-weighted cost; the cache tradeoff finding may have changed — re-examine the default lever choice")
	}
}

// buildRunaway constructs a finder history of `turns` turns where each turn reads
// a file of about readTokens tokens, mirroring the dogfood shape. The first turn
// is the bare task. It returns the per-turn append-only request snapshots.
func buildRunaway(turns, readTokens int) [][]llm.Message {
	blob := strings.Repeat("source line of the file under analysis\n", readTokens*4/39+1)
	msgs := []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("investigate ", 200)}}
	var snaps [][]llm.Message
	snaps = append(snaps, cloneMsgs(msgs))
	for i := 0; i < turns-1; i++ {
		id := fmt.Sprintf("call-%02d", i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: "I will read the next file to check it.",
				ToolCalls: []llm.ToolCall{{ID: id, Name: "read_file", Arguments: []byte(`{"path":"pkg/file.go"}`)}}},
			llm.Message{Role: llm.RoleToolResult, ToolCallID: id, Content: blob},
		)
		snaps = append(snaps, cloneMsgs(msgs))
	}
	return snaps
}

// runawayNames reproduces the ToolCallID -> tool-name map buildRunaway implies,
// so compaction stubs can name their tool exactly as in the live loop.
func runawayNames(reads int) map[string]string {
	names := map[string]string{}
	for i := 0; i < reads; i++ {
		names[fmt.Sprintf("call-%02d", i)] = "read_file"
	}
	return names
}

func cloneMsgs(in []llm.Message) []llm.Message {
	out := make([]llm.Message, len(in))
	copy(out, in)
	return out
}

func pct(before, after float64) float64 {
	if before == 0 {
		return 0
	}
	return 100 * (before - after) / before
}

func loadTranscriptOrFail(t *testing.T, path string) *agent.Transcript {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	tr, err := agent.LoadJSONL(f)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return tr
}
