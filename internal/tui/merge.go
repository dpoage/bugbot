package tui

import (
	"sort"
	"time"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// mergeAgents concatenates live in-flight agents with finished historical
// units into one display list, sorted by Started ascending. It is a plain
// concatenation, not a join, with no dedup key: agent_units rows are written
// synchronously the instant a unit finishes, but the on-disk status.json
// still lists it under ActiveAgents until the next rate-limited write, so a
// live entry and its eventual historical row MAY briefly coexist for up to
// one snapshot interval (see AgentView doc comment). The window is
// read-only/cosmetic and self-heals on the next frame.
//
// transcriptDirs is passed through to discoverTranscript for each historical
// unit; nil/empty disables lookup entirely (no directory configured).
func mergeAgents(live []progress.AgentStatus, hist []store.AgentUnit, transcriptDirs []string) []AgentView {
	views := make([]AgentView, 0, len(live)+len(hist))

	for _, u := range hist {
		views = append(views, AgentView{
			Role:            string(u.Role),
			Label:           historicalLabel(u),
			Lens:            u.Lens,
			Strategy:        string(u.Strategy),
			UnitID:          u.ID,
			Started:         u.StartedAt,
			FinishedAt:      u.FinishedAt,
			Status:          string(u.Status),
			InputTokens:     u.InputTokens,
			OutputTokens:    u.OutputTokens,
			CacheReadTokens: u.CacheReadTokens,
			Candidates:      u.Candidates,
			LeadsPosted:     u.LeadsPosted,
			Detail:          u.Detail,
			Files:           u.Files,
			TranscriptPath:  discoverTranscript(transcriptDirs, u),
		})
	}

	for _, a := range live {
		views = append(views, AgentView{
			Role:       a.Role,
			Label:      a.Label,
			AgentID:    a.AgentID,
			Live:       true,
			Started:    a.Started,
			Activity:   a.Activity,
			ActivityAt: a.ActivityAt,
			// RecentActions: AgentStatus.RecentActions is a copy (snapshot.go
			// pushRing always returns a new slice; AgentView takes ownership here).
			RecentActions: a.RecentActions,
		})
	}

	sort.SliceStable(views, func(i, j int) bool { return views[i].Started.Before(views[j].Started) })
	return views
}

// historicalLabel builds a display label for a finished agent_units row: the
// lens, plus the strategy in parens for finders that have one.
func historicalLabel(u store.AgentUnit) string {
	if u.Strategy != "" {
		return u.Lens + " (" + string(u.Strategy) + ")"
	}
	return u.Lens
}

// agentKey returns a stable identity for a, used to re-resolve the drilled-in
// agent across frame refreshes (see Model.detailKey) without depending on its
// position in frame.Agents, which mergeAgents rebuilds from scratch every
// frame. Historical entries key off the durable agent_units primary key; live
// entries key off AgentID when the emitter set one (stable and unique for the
// run's whole lifetime, unlike role+label+start which can collide when two
// concurrent agents share a label). Live entries from a pre-identity emitter
// fall back to role+label+start time, which is stable for the lifetime of a
// single invocation but not collision-proof across duplicate labels.
func agentKey(a AgentView) string {
	if a.UnitID != "" {
		return "unit:" + a.UnitID
	}
	if a.AgentID != "" {
		return "live-id:" + a.AgentID
	}
	return "live:" + a.Role + "\x00" + a.Label + "\x00" + a.Started.UTC().Format(time.RFC3339Nano)
}

// findAgentByKey returns the index of the agent in views whose agentKey
// matches key, or (-1, false) when no longer present (e.g. a live agent
// finished and flipped to a historical row with a different key, or the
// scan run rotated out of ListAgentUnits' window).
func findAgentByKey(views []AgentView, key string) (int, bool) {
	for i, v := range views {
		if agentKey(v) == key {
			return i, true
		}
	}
	return -1, false
}
