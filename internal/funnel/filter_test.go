package funnel

import (
	"context"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// TestSnapshotHonorsFilter is the regression test for the calibration-scan
// incident: Options.Filter used to be dropped on the floor (snapshot was taken
// with a zero ScanFilter), so include/exclude globs from config.Scan were
// silently ignored and a scoped scan swept the whole repo.
func TestSnapshotHonorsFilter(t *testing.T) {
	st, repo := openFixture(t)
	defer func() { _ = st.Close() }()

	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		Discovery: DiscoveryConfig{Filter: ingest.ScanFilter{Include: []string{"bug.go"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap, err := f.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != "bug.go" {
		paths := make([]string, 0, len(snap.Files))
		for _, fl := range snap.Files {
			paths = append(paths, fl.Path)
		}
		t.Fatalf("filtered snapshot = %v, want exactly [bug.go]", paths)
	}
}
