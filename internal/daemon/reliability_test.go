package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/funnel"
)

// TestWarnIfUnreliable proves the daemon sweep/scan path loudly flags an
// untrustworthy finder stage (mirroring the CLI banner) and stays silent when
// the stage was reliable. This is the daemon side of the "indistinguishable from
// clean" trust fix: an unattended sweep must not treat a broken run as clean.
func TestWarnIfUnreliable(t *testing.T) {
	cases := []struct {
		name       string
		stats      funnel.Stats
		wantWarn   bool
		wantPhrase string
	}{
		{
			name:     "reliable run is silent",
			stats:    funnel.Stats{FinderRuns: 4, FinderFailures: 0},
			wantWarn: false,
		},
		{
			name:       "no finders ran",
			stats:      funnel.Stats{FinderRuns: 0},
			wantWarn:   true,
			wantPhrase: "no finder agents ran",
		},
		{
			name:       "most finders failed",
			stats:      funnel.Stats{FinderRuns: 4, FinderFailures: 3},
			wantWarn:   true,
			wantPhrase: "most finders failed",
		},
		{
			name:       "some finders failed",
			stats:      funnel.Stats{FinderRuns: 4, FinderFailures: 1},
			wantWarn:   true,
			wantPhrase: "coverage incomplete",
		},
		{
			name:     "budget stops alone do not trip the warning",
			stats:    funnel.Stats{FinderRuns: 4, FinderFailures: 0, FinderBudgetStopped: 2},
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			d := &Daemon{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}

			d.warnIfUnreliable(&funnel.Result{Stats: tc.stats})

			logged := buf.String()
			gotWarn := strings.Contains(logged, "RELIABILITY WARNING")
			if gotWarn != tc.wantWarn {
				t.Fatalf("warn emitted = %v, want %v; log=%q", gotWarn, tc.wantWarn, logged)
			}
			if tc.wantWarn {
				if !strings.Contains(strings.ToLower(logged), tc.wantPhrase) {
					t.Errorf("warning missing phrase %q; log=%q", tc.wantPhrase, logged)
				}
				if !strings.Contains(logged, "level=WARN") {
					t.Errorf("expected WARN level; log=%q", logged)
				}
			}
		})
	}
}
