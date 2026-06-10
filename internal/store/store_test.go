package store

import (
	"context"
	"path/filepath"
	"testing"
)

// openTemp opens a fresh on-disk store in a temp dir. Using a real file (not
// :memory:) exercises parent-dir creation and WAL behaviour. The driver is
// pure-Go, so no CGO toolchain is required.
func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "state.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpen_MigratesFromEmptyAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "a", "b", "state.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	// All tables plus schema_migrations must exist.
	for _, table := range []string{
		"findings", "suppressions", "file_state", "scan_runs", "spend",
		"leads", "published_issues",
		"schema_migrations",
	} {
		var name string
		err := st.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
	}

	var version int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version < 1 {
		t.Fatalf("expected at least version 1, got %d", version)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open the same file: migrations must be a no-op and the version stable.
	st2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = st2.Close() }()

	var version2 int
	if err := st2.DB().QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`).Scan(&version2); err != nil {
		t.Fatalf("read version after reopen: %v", err)
	}
	if version2 != version {
		t.Fatalf("schema version changed on reopen: %d -> %d", version, version2)
	}

	var rows int
	if err := st2.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	// rows may be less than version when version numbers have intentional gaps
	// (e.g. 005 is reserved for a parallel branch while 006 is already applied).
	// We only require that at least one migration row exists and that rows does
	// not exceed version.
	if rows < 1 {
		t.Fatalf("expected at least 1 migration row, got %d", rows)
	}
	if rows > version {
		t.Fatalf("more migration rows (%d) than max version (%d)", rows, version)
	}
}

func TestLoadMigrations_SortedAndWellFormed(t *testing.T) {
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("expected at least one migration")
	}
	for i := 1; i < len(migs); i++ {
		if migs[i-1].version >= migs[i].version {
			t.Fatalf("migrations not strictly ascending: %d then %d",
				migs[i-1].version, migs[i].version)
		}
	}
}

func TestFingerprint_StableAndNormalized(t *testing.T) {
	a := Fingerprint("nil-deref", "pkg/foo/bar.go", 42, "Possible nil pointer dereference")
	b := Fingerprint("nil-deref", "pkg/foo/bar.go", 42, "Possible nil pointer dereference")
	if a != b {
		t.Fatal("fingerprint should be deterministic")
	}

	// Backslashes, case, and extra whitespace in the title must normalize away.
	c := Fingerprint("NIL-DEREF", "pkg\\foo\\bar.go", 42, "possible  nil   pointer dereference")
	if a != c {
		t.Fatalf("fingerprint should normalize path/case/space:\n %s\n %s", a, c)
	}

	// A different line is a different fingerprint.
	d := Fingerprint("nil-deref", "pkg/foo/bar.go", 43, "Possible nil pointer dereference")
	if a == d {
		t.Fatal("different line must produce a different fingerprint")
	}
}
