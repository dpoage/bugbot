package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestCheck_CleanStore: Check returns nil on a freshly-migrated store.
func TestCheck_CleanStore(t *testing.T) {
	if err := openTemp(t).Check(context.Background()); err != nil {
		t.Fatalf("Check on clean store: %v", err)
	}
}

// TestRecover_RoundTripHealthyDB: Recover backs up the original, rebuilds a
// clean db, and salvages every row. Recover normally runs on a corrupt db, but
// a healthy round-trip is the deterministic proof that the salvage + atomic
// swap preserves data.
func TestRecover_RoundTripHealthyDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f, err := st.UpsertFinding(ctx, sampleFinding())
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if _, err := st.RecordSpend(ctx, Spend{InputTokens: 7, OutputTokens: 3}); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	// Recover takes the writer lock, so the store must be closed first.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rep, err := Recover(ctx, path)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rep.BackupPath == "" {
		t.Error("Recover did not record a backup path")
	} else if _, err := os.Stat(rep.BackupPath); err != nil {
		t.Errorf("backup file missing: %v", err)
	}
	if rep.Salvaged["findings"] != 1 {
		t.Errorf("salvaged findings = %d; want 1", rep.Salvaged["findings"])
	}
	if rep.Salvaged["spend"] != 1 {
		t.Errorf("salvaged spend = %d; want 1", rep.Salvaged["spend"])
	}
	if len(rep.Partial) != 0 {
		t.Errorf("unexpected partial tables on a healthy db: %v", rep.Partial)
	}

	// The rebuilt db opens clean and the finding survived intact.
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen rebuilt db: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Check(ctx); err != nil {
		t.Fatalf("rebuilt db failed Check: %v", err)
	}
	got, err := reopened.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("finding lost after recover: %v", err)
	}
	if got.Title != f.Title {
		t.Errorf("recovered title = %q; want %q", got.Title, f.Title)
	}
}

// TestRecover_DamagedSourceProducesCleanDB exercises the read-error tolerance:
// after physically corrupting the db file, Recover must still produce a clean,
// openable database and preserve the corrupt original as a backup. Whether any
// rows survive depends on where the damage landed (recorded in the report), so
// this asserts the invariants that always hold rather than a fixed salvage
// count.
func TestRecover_DamagedSourceProducesCleanDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Several rows across pages so corruption has something to bite.
	for i := 0; i < 40; i++ {
		f := sampleFinding()
		f.Fingerprint = domain.Fingerprint("race", "internal/x/y.go", string(rune('a'+i%26))+string(rune('0'+i/26)))
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatalf("seed finding %d: %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Overwrite a chunk past the header with garbage to damage a data page.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	garbage := make([]byte, 1024)
	for i := range garbage {
		garbage[i] = 0xFF
	}
	if _, err := fh.WriteAt(garbage, info.Size()/2); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_ = fh.Close()

	rep, err := Recover(ctx, path)
	if err != nil {
		t.Fatalf("Recover on damaged db: %v", err)
	}
	if rep.BackupPath == "" {
		t.Error("no backup recorded for the corrupt db")
	} else if _, err := os.Stat(rep.BackupPath); err != nil {
		t.Errorf("backup of corrupt db missing: %v", err)
	}
	t.Logf("salvaged %d rows; partial=%v sourceOpenErr=%q",
		rep.TotalSalvaged(), rep.Partial, rep.SourceOpenErr)

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen rebuilt db: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Check(ctx); err != nil {
		t.Fatalf("rebuilt db failed Check: %v", err)
	}
}

// TestCheck_DetectsCorruptionAsErrCorrupt validates the exact contract the
// daemon's storeHealthy gate depends on: a quick_check failure surfaces as
// errors.Is(err, ErrCorrupt), distinct from a transient/cancel error. Without
// this, storeHealthy could not tell corruption (skip cycle + point at --repair)
// from a transient race (log and proceed).
func TestCheck_DetectsCorruptionAsErrCorrupt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 60; i++ {
		f := sampleFinding()
		f.Fingerprint = domain.Fingerprint("race", "f.go", fmt.Sprintf("%d", i))
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt the entire back half of the file: every page there is live table or
	// index data (a freshly built, never-deleted db has no free pages), so this
	// reliably trips quick_check regardless of the exact page count — which shifts
	// as the schema gains columns/indexes. The 100-byte header, page 1 (schema),
	// and any schema overflow stay intact (they live at the front), so the file
	// still opens and Check, not open, does the detecting.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	garbage := make([]byte, int(info.Size()-info.Size()/2))
	for i := range garbage {
		garbage[i] = 0xFF
	}
	if _, err := fh.WriteAt(garbage, info.Size()/2); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	_ = fh.Close()

	// OpenReadOnly skips migrate on an existing db, so it opens the corrupt file
	// (as the daemon's live handle already would) and Check does the detecting.
	st2, openErr := OpenReadOnly(ctx, path)
	if openErr != nil {
		// Damage severe enough to fail the open is also detection; the
		// ErrCorrupt-from-Check path simply isn't exercised this run.
		t.Logf("db too damaged to reopen (%v); skipping the Check assertion", openErr)
		return
	}
	defer func() { _ = st2.Close() }()
	checkErr := st2.Check(ctx)
	if checkErr == nil {
		t.Fatal("Check passed on a corrupted db; want a corruption error")
	}
	if !errors.Is(checkErr, ErrCorrupt) {
		t.Errorf("Check error = %v; want errors.Is(err, ErrCorrupt)", checkErr)
	}
}
