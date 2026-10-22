package funnel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/store"
	llmkit "github.com/dpoage/llmkit"
	"github.com/dpoage/llmkit/agent"
)

// TranscriptRunID mints the RunID of an agent run whose JSONL transcript
// ("<RunID>.jsonl") joins the agent_units row with ID key:
// "<llmkit.NewRunID()>-<key>". An empty key mints a plain llmkit.NewRunID.
func TranscriptRunID(key string) llmkit.RunID {
	id := llmkit.NewRunID()
	if key == "" {
		return id
	}
	return id + llmkit.RunID("-"+key)
}

// transcriptRun returns the run option pinning a run's RunID to
// TranscriptRunID(key).
func transcriptRun(key string) agent.RunOption {
	return agent.WithRunID(TranscriptRunID(key))
}

// ParseTranscriptName parses a JSONL transcript filename written under a
// TranscriptRunID or default llmkit RunID ("<unix-millis>-<hex>[-<key>].jsonl"),
// returning the run's mint time and its agent_units key ("" when unkeyed).
func ParseTranscriptName(name string) (minted time.Time, key string, ok bool) {
	base, found := strings.CutSuffix(name, ".jsonl")
	if !found {
		return time.Time{}, "", false
	}
	parts := strings.SplitN(base, "-", 3)
	if len(parts) < 2 || len(parts[0]) != 13 || parts[1] == "" {
		return time.Time{}, "", false
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || ms < 0 {
		return time.Time{}, "", false
	}
	if len(parts) == 3 {
		key = parts[2]
	}
	return time.UnixMilli(ms), key, true
}

// keepRuns is the number of recent scan runs whose agent_unit rows are
// preserved by PruneAgentUnits. Rows for older runs are deleted to bound
// table growth. Chosen as 200 to retain enough history for yield-table
// learning without unbounded accumulation.
const keepRuns = 200

// recordFinderUnit records an agent_units row for a finder unit that was
// skipped before launch (zero tokens, zero started_at / finished_at). Best-effort:
// a failed insert is logged on result.Skipped and never aborts the scan.
func (f *Funnel) recordFinderUnit(
	ctx context.Context,
	scanRunID string,
	unitID string,
	u unit,
	launchOrder int,
	status string,
	inTokens, outTokens, cacheRead, candidates int64,
	leadsPosted int,
	result *Result,
) {
	f.recordFinderUnitWithTimeDetail(ctx, scanRunID, unitID, u, launchOrder, status, "",
		time.Time{}, time.Time{}, inTokens, outTokens, cacheRead, candidates, leadsPosted, result)
}

// recordFinderUnitWithTimeDetail records an agent_units row for a launched
// finder unit, optionally including a postmortem detail string (for
// parse_failed and budget_stopped rows). The detail is the encoded
// finderPostmortem from the failure path; it is empty for ok/skipped rows.
// Best-effort: a failed insert is logged on result.Skipped and never aborts
// the scan.
//
// unitID is minted by the caller BEFORE the finder runner was built (or, for
// a skipped unit, with nothing built at all) and keys the run's transcript
// RunID (TranscriptRunID) — passing it as the row's own ID here, instead of
// leaving AddAgentUnit generate one, is what gives the TUI an EXACT
// filename<->row join rather than a timestamp-window guess.
func (f *Funnel) recordFinderUnitWithTimeDetail(
	ctx context.Context,
	scanRunID string,
	unitID string,
	u unit,
	launchOrder int,
	status string,
	detail string,
	startedAt, finishedAt time.Time,
	inTokens, outTokens, cacheRead, candidates int64,
	leadsPosted int,
	result *Result,
) {
	row := store.AgentUnit{
		ID:              unitID,
		ScanRunID:       scanRunID,
		Role:            "finder",
		Lens:            u.lens.Name,
		Strategy:        store.AgentStrategy(u.strategy.Name),
		LaunchOrder:     launchOrder,
		Files:           u.files,
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
		Status:          store.AgentStatus(status),
		InputTokens:     inTokens,
		OutputTokens:    outTokens,
		CacheReadTokens: cacheRead,
		Candidates:      int(candidates),
		LeadsPosted:     leadsPosted,
		Detail:          detail,
	}
	if err := f.store.AddAgentUnit(ctx, row); err != nil {
		f.note(result, fmt.Sprintf("observability: AddAgentUnit (finder %s@%s status=%s): %v", u.lens.Name, u.strategy.Name, status, err))
	}
}

// recordVerifierUnit records an agent_units row for a verifier panel. The
// detail string carries seat names, refuted counts, and the arbiter verdict in
// a compact, injection-free format (counts and seat names only — no
// model-authored free text). Best-effort: a failed insert is logged on
// result.Skipped and never aborts the scan.
//
// tokens is the total input+output tokens for the panel (panel+arbiter
// survived, killed, orphaned_budget, orphaned_verify_failed.
//
// seatNames are the refuter seat names; seatRefuted is a parallel slice
// indicating which seats voted "refuted". noVerdict counts seats that produced
// no parseable verdict (infra/parse failures). arbiterRan indicates whether an
// arbiter ran. arbiterRefuted indicates the arbiter's verdict (meaningful only
// when arbiterRan is true).
//
// unitID is minted by the caller BEFORE the refuter panel / arbiter runs and
// keys ALL of their transcript RunIDs (TranscriptRunID) — passing it as the
// row's own ID here is what gives the TUI an EXACT filename<->row join. A
// verifier row can correspond to MULTIPLE transcript files (each refuter seat
// plus the arbiter, when one ran) sharing this key; the TUI picks the
// earliest-minted one.
func (f *Funnel) recordVerifierUnit(
	ctx context.Context,
	scanRunID string,
	unitID string,
	lens string,
	file string,
	launchOrder int,
	startedAt, finishedAt time.Time,
	tokens int64,
	status string,
	seatNames []string,
	seatRefuted []bool,
	noVerdict int,
	arbiterRan bool,
	arbiterRefuted bool,
	result *Result,
) {
	// Build a compact, injection-free detail string: seat names and vote
	// counts plus arbiter verdict. No model-authored free text — only
	// structured counts and names that are safe for long-term storage.
	var detail string
	if len(seatNames) > 0 {
		refutedCount := 0
		for _, r := range seatRefuted {
			if r {
				refutedCount++
			}
		}
		seatsStr := strings.Join(seatNames, ",")
		if arbiterRan {
			arbiterVerdict := "survived"
			if arbiterRefuted {
				arbiterVerdict = "refuted"
			}
			detail = fmt.Sprintf("seats=%s refuted=%d/%d arbiter=%s", seatsStr, refutedCount, len(seatNames), arbiterVerdict)
		} else {
			detail = fmt.Sprintf("seats=%s refuted=%d/%d", seatsStr, refutedCount, len(seatNames))
		}
		if noVerdict > 0 {
			detail += fmt.Sprintf(" noverdict=%d", noVerdict)
		}
	}

	// Split the combined tokens into a best-effort input/output split.
	// verify.go accumulates input+output together without separating them
	// (it uses outcome.Usage.InputTokens + outcome.Usage.OutputTokens).
	// We store the total in InputTokens and leave OutputTokens zero since
	// the split is not available at this level.
	row := store.AgentUnit{
		ID:          unitID,
		ScanRunID:   scanRunID,
		Role:        "verifier",
		Lens:        lens,
		Strategy:    "",
		LaunchOrder: launchOrder,
		Files:       []string{file},
		StartedAt:   startedAt,
		FinishedAt:  finishedAt,
		Status:      store.AgentStatus(status),
		InputTokens: tokens, // combined panel + arbiter tokens
		Candidates:  candidateSurvivedInt(status),
		Detail:      detail,
	}
	if err := f.store.AddAgentUnit(ctx, row); err != nil {
		f.note(result, fmt.Sprintf("observability: AddAgentUnit (verifier lens=%s status=%s): %v", lens, status, err))
	}
}

// candidateSurvivedInt returns 1 if the verifier status indicates the
// candidate survived, 0 otherwise. Used to populate the candidates column
// for verifier rows (1 = survived, 0 = killed or orphaned).
func candidateSurvivedInt(status string) int {
	if status == "survived" {
		return 1
	}
	return 0
}
