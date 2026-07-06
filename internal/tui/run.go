package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dpoage/bugbot/internal/config"
)

// Run builds the Observer feed for cfg and drives a full-screen bubbletea
// program until the user quits (q / ctrl-c). It performs no LLM or network
// calls: SnapshotFeed only reads status.json and the local read-only store.
func Run(ctx context.Context, cfg config.Config) error {
	feed, err := NewSnapshotFeed(ctx, cfg)
	if err != nil {
		return err
	}
	defer feed.Close()

	p := tea.NewProgram(NewModel(feed), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = p.Run()
	return err
}
