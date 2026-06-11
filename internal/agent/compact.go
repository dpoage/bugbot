package agent

import (
	"fmt"
	"strings"

	"github.com/dpoage/bugbot/internal/llm"
)

// compactRecentToolResults is the number of most-recent tool-result messages
// preserved verbatim when history compaction fires. The agent almost always
// reasons over the results it just received; keeping the trailing window intact
// means compaction never blinds the next turn, while older read_file / grep
// dumps — which the model has already summarized into its assistant prose — are
// the bytes actually reclaimed.
const compactRecentToolResults = 4

// compactRearmFactor multiplies HistoryTokenBudget after each compaction so the
// next compaction only fires once history has grown materially again. Without
// re-arming, every subsequent turn would re-trip the (now barely-exceeded)
// threshold and re-pay a prefix cache miss each turn, which the cache-cost model
// shows is strictly worse than append-only. A factor of 2 means compaction is
// bounded to O(log(history/budget)) firings per run, each amortized over the
// turns of cheap appends that follow.
const compactRearmFactor = 2

// EstimateHistoryTokens approximates the billed token size of a message slice
// using the same bytes/4 heuristic the Runner's compaction trigger uses. It is
// exported so offline measurement harnesses report numbers that agree with the
// live compaction decision.
func EstimateHistoryTokens(msgs []llm.Message) int64 { return estimateTokens(msgs) }

// SimulateCompaction applies the Runner's threshold-triggered history-compaction
// policy to a single history snapshot, given the threshold currently in force
// and the running tool-name map. It returns the (possibly) compacted snapshot
// and the next threshold (re-armed upward iff a real prune occurred), exactly
// mirroring [Runner.maybeCompact]. It exists so an offline measurement can replay
// a recorded run's request snapshots through the SAME policy the live Runner
// applies, rather than a re-implementation that could drift.
//
// budget <= 0 disables compaction (returns the snapshot unchanged). recentK is
// the trailing tool-result window to preserve; pass CompactRecentToolResults to
// match the Runner.
func SimulateCompaction(msgs []llm.Message, budget, threshold int64, recentK int, toolNameByID map[string]string) (out []llm.Message, nextThreshold int64) {
	if budget <= 0 || threshold <= 0 {
		return msgs, threshold
	}
	if estimateTokens(msgs) <= threshold {
		return msgs, threshold
	}
	compacted, pruned := compactHistory(msgs, recentK, toolNameByID)
	if !pruned {
		return msgs, threshold
	}
	return compacted, threshold * compactRearmFactor
}

// CompactRecentToolResults exposes the Runner's trailing-window size so a
// measurement harness preserves the same recent tool results the live loop does.
const CompactRecentToolResults = compactRecentToolResults

// estimateTokens approximates the token cost of a message slice using a
// bytes/4 heuristic over the text the provider actually bills: message content
// plus the tool-call names and serialized arguments an assistant turn carries.
// It deliberately ignores per-message framing overhead; it is a relative signal
// for "is history big enough to compact", not a billing oracle. The same
// heuristic is used by the offline measurement harness so the trigger and the
// reported numbers agree.
func estimateTokens(msgs []llm.Message) int64 {
	var b int64
	for i := range msgs {
		b += messageBytes(msgs[i])
	}
	return b / 4
}

// messageBytes returns the approximate billed byte size of one message: its
// textual content plus, for assistant turns, each tool call's name and raw
// argument JSON.
func messageBytes(m llm.Message) int64 {
	b := int64(len(m.Content))
	for _, tc := range m.ToolCalls {
		b += int64(len(tc.Name)) + int64(len(tc.Arguments))
	}
	return b
}

// compactStubPrefix marks content that compactHistory already replaced. It lets
// a later compaction recognize an existing stub and leave it untouched, so an
// already-pruned message is never re-mutated (which would needlessly invalidate
// the prompt-cache prefix on the very next turn).
const compactStubPrefix = "[tool result pruned"

// isCompactStub reports whether content is already a compaction stub (either the
// normal or errored variant), so it must not be stubbed again.
func isCompactStub(content string) bool {
	return strings.HasPrefix(content, compactStubPrefix) ||
		strings.HasPrefix(content, "[errored tool result pruned")
}

// compactStub renders the replacement content for a pruned tool result. It keeps
// the tool name and original byte size so the model retains a breadcrumb (it can
// re-read the file if it genuinely needs the bytes back) without paying to carry
// the full dump every subsequent turn. errored results are marked so the model
// still knows the call had failed.
func compactStub(toolName string, origBytes int, isErr bool) string {
	kind := "tool result"
	if isErr {
		kind = "errored tool result"
	}
	name := toolName
	if name == "" {
		name = "tool"
	}
	return fmt.Sprintf("[%s pruned to save context: %s, %d bytes]", kind, name, origBytes)
}

// compactHistory replaces the Content of tool-result messages OLDER than the
// most recent recent-K with short stubs, IN PLACE on a fresh copy, returning the
// compacted slice and whether anything was actually pruned.
//
// It preserves, untouched:
//   - the task message (index 0) and every user/system message,
//   - every assistant turn (the model's reasoning chain and its tool calls),
//   - the most recent recentK tool-result messages,
//   - tool_call/tool_result pairing: each stubbed message keeps its Role,
//     ToolCallID, and IsError, so providers (and the ReplayClient) still see a
//     well-formed tool result answering its originating tool call. Only Content
//     shrinks.
//
// toolNameFor maps a tool-result's ToolCallID back to the tool name recorded
// when the call was issued, so the stub can name the tool; an empty/missing
// mapping degrades to a generic stub.
//
// The returned slice is always a fresh allocation when pruning occurs (the
// caller must swap it in so the prior, longer prefix is not aliased), and the
// original is returned unchanged when there is nothing to prune.
func compactHistory(msgs []llm.Message, recentK int, toolNameFor map[string]string) ([]llm.Message, bool) {
	if recentK < 0 {
		recentK = 0
	}
	// Index the tool-result messages so we know which ones fall outside the
	// trailing recent-K window. Only those become stub candidates.
	var toolIdx []int
	for i := range msgs {
		if msgs[i].Role == llm.RoleToolResult {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) <= recentK {
		// Everything is within the recent window (or there are none); nothing to
		// reclaim without touching results the next turn likely needs.
		return msgs, false
	}
	// The last recentK tool results are kept verbatim; the rest are prunable.
	keepFrom := len(toolIdx) - recentK
	prunable := make(map[int]bool, keepFrom)
	for _, idx := range toolIdx[:keepFrom] {
		prunable[idx] = true
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	pruned := false
	for _, idx := range toolIdx[:keepFrom] {
		m := out[idx]
		// Never re-stub an already-pruned result: re-mutating it would invalidate
		// the prompt-cache prefix every turn for zero byte savings. This also keeps
		// the stub's reported byte size pinned to the ORIGINAL dump, not the stub.
		if isCompactStub(m.Content) {
			continue
		}
		stub := compactStub(toolNameFor[m.ToolCallID], len(m.Content), m.IsError)
		// Skip results already smaller than their stub: stubbing would grow them.
		if len(m.Content) <= len(stub) {
			continue
		}
		m.Content = stub
		out[idx] = m
		pruned = true
	}
	return out, pruned
}
