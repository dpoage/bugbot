package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/funnel"
	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// This file is the offline measurement harness for bugbot-3nf (finder token
// burn). It computes the per-finder INPUT token cost of the agent loop over the
// committed recorded finder corpus, both raw and cache-weighted (append-only
// prompt-cache model: a request's prefix that matches the previous request bills
// at cacheReadWeight, the new tail at full price), and over a synthetic "runaway"
// profile matching the dogfood finding (~8-9 turns at ~60k input tokens/turn)
// driven through a real agent.Runner with and without
// Limits.HistoryTokenBudget compaction.
//
// The numbers are printed (run `go test -run TestMeasure -v ./internal/eval/`)
// AND asserted: compaction cannot fire on the recorded corpus (every request is
// under the threshold), and the runaway profile must show a material cache-
// weighted reduction from the read-cap lever.

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
func simulateCost(snaps [][]llmkit.Message, cacheWeight float64) costResult {
	var raw, weighted float64
	var prev []llmkit.Message
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
func commonPrefixLen(a, b []llmkit.Message) int {
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

func messageEqual(x, y llmkit.Message) bool {
	if x.Role != y.Role || x.Text() != y.Text() || x.ToolCallID != y.ToolCallID || x.IsError != y.IsError {
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
// transcript, in order: each completion event records the full conversation
// sent that turn, which is exactly the append-only history.
func requestSnapshots(tr *agent.Transcript) [][]llmkit.Message {
	var snaps [][]llmkit.Message
	for _, ev := range tr.Record {
		if ev.Kind == llmkit.KindCompletion && ev.Completion != nil {
			snaps = append(snaps, ev.Completion.Request.Messages)
		}
	}
	return snaps
}

// liveSnapshots drives task through a real agent.Runner serving resps, with
// tools, under HistoryTokenBudget budget (0 = no compaction), and returns the
// message history of every wire request — compaction applied exactly as the
// live loop applies it.
func liveSnapshots(t *testing.T, task string, resps []llmkit.Response, tools []agent.Tool, budget int64) [][]llmkit.Message {
	t.Helper()
	var snaps [][]llmkit.Message
	hooks := agent.Hooks{BeforeCompletion: func(_ context.Context, _ int, req *llmkit.Request) {
		snaps = append(snaps, cloneMsgs(req.Messages))
	}}
	r := agent.NewRunner(agent.NewReplayClientFromResponses(resps, llmkit.Capabilities{}), tools, "sys",
		agent.WithHooks(hooks), agent.WithLimits(agent.Limits{HistoryTokenBudget: budget, MaxIterations: -1, TokenBudget: -1}))
	if _, err := r.Run(context.Background(), task); err != nil {
		t.Fatalf("runaway run: %v", err)
	}
	return snaps
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

	var beforeRaw, beforeW float64
	var peak int64
	units := 0
	t.Logf("%-44s %5s %9s %9s %9s", "finder unit", "turns", "raw", "cw", "peak")
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
			before := simulateCost(snaps, measureCacheWeight)
			var unitPeak int64
			for _, s := range snaps {
				unitPeak = max(unitPeak, agent.EstimateHistoryTokens(s))
			}
			peak = max(peak, unitPeak)

			beforeRaw += before.raw
			beforeW += before.weighted
			units++
			rel := e.Name() + "/" + strings.TrimSuffix(filepath.Base(p), ".jsonl")
			t.Logf("%-44s %5d %9.0f %9.0f %9d", rel, before.turns, before.raw, before.weighted, unitPeak)
		}
	}
	if units == 0 {
		t.Skip("recorded corpus present but no finder transcripts")
	}

	t.Logf("RECORDED CORPUS (%d finder units), cache weight %.2f:", units, measureCacheWeight)
	t.Logf("  per-unit RAW input tokens:  %.0f", beforeRaw/float64(units))
	t.Logf("  per-unit CACHE-WEIGHTED:    %.0f", beforeW/float64(units))

	// Recall/cost safety: compaction fires only once a request's history
	// exceeds the threshold, so on the recorded corpus it must be inert.
	if peak >= budget {
		t.Errorf("a recorded finder request reached %d history tokens (threshold %d); compaction would fire on short runs", peak, budget)
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

	base := liveSnapshots(t, runawayTask, runawayResponses(9), []agent.Tool{runawayReadTool(baseReadTokens)}, 0)
	beforeCost := simulateCost(base, measureCacheWeight)

	// Lever A (DEFAULT, cache-safe): tighter per-read caps shrink each result at
	// the source, so no message is ever mutated and the prompt-cache prefix is
	// fully preserved — every saved byte is saved at full proportional value.
	capped := liveSnapshots(t, runawayTask, runawayResponses(9), []agent.Tool{runawayReadTool(capReadTokens)}, 0)
	capCost := simulateCost(capped, measureCacheWeight)

	// Lever B (OPT-IN): threshold history compaction at the funnel default.
	compacted := liveSnapshots(t, runawayTask, runawayResponses(9), []agent.Tool{runawayReadTool(baseReadTokens)}, budget)
	compCost := simulateCost(compacted, measureCacheWeight)

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

// runawayTask is the runaway finder's task: the first turn's bare user message.
var runawayTask = strings.Repeat("investigate ", 200)

// runawayResponses scripts a finder run of `turns` completions: turns-1
// read_file calls, then a final answer, mirroring the dogfood shape.
func runawayResponses(turns int) []llmkit.Response {
	resps := make([]llmkit.Response, 0, turns)
	for i := range turns - 1 {
		resps = append(resps, llmkit.Response{
			Text:       "I will read the next file to check it.",
			StopReason: llmkit.StopToolUse,
			ToolCalls:  []llmkit.ToolCall{{ID: fmt.Sprintf("call-%02d", i), Name: "read_file", Arguments: json.RawMessage(`{"path":"pkg/file.go"}`)}},
		})
	}
	return append(resps, llmkit.Response{Text: "done", StopReason: llmkit.StopEndTurn})
}

// runawayReadTool is a read_file tool returning a file of about readTokens
// tokens on every call.
func runawayReadTool(readTokens int) agent.Tool {
	return staticTool{name: "read_file", result: strings.Repeat("source line of the file under analysis\n", readTokens*4/39+1)}
}

// cloneMsgs deep-copies msgs' content blocks so a later in-place compaction
// cannot rewrite an earlier snapshot.
func cloneMsgs(in []llmkit.Message) []llmkit.Message {
	out := make([]llmkit.Message, len(in))
	for i, m := range in {
		m.Content = slices.Clone(m.Content)
		out[i] = m
	}
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
