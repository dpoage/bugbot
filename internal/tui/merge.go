package tui

import (
	"sort"

	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
)

// mergeAgents concatenates live in-flight agents with finished historical
// units into one display list, sorted by Started ascending. It is a
// concatenation rather than a join: agent_units rows are only written when a
// unit finishes, so a live entry and its eventual historical row never
// coexist for the same physical invocation (see AgentView doc comment).
//
// transcriptDir is passed through to discoverTranscript for each historical
// unit; empty disables lookup entirely (no directory configured).
func mergeAgents(live []progress.AgentStatus, hist []store.AgentUnit, transcriptDir string) []AgentView {
	views := make([]AgentView, 0, len(live)+len(hist))

	for _, u := range hist {
		views = append(views, AgentView{
			Role:            string(u.Role),
			Label:           historicalLabel(u),
			Lens:            u.Lens,
			Strategy:        string(u.Strategy),
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
			TranscriptPath:  discoverTranscript(transcriptDir, u),
		})
	}

	for _, a := range live {
		views = append(views, AgentView{
			Role:       a.Role,
			Label:      a.Label,
			Live:       true,
			Started:    a.Started,
			Activity:   a.Activity,
			ActivityAt: a.ActivityAt,
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
