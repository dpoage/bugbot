package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	// Tolerate at most ONE gap: anything looser would mask migrations that
	// silently failed to apply.
	if rows < version-1 {
		t.Fatalf("migration rows (%d) too far below max version (%d); at most one gap is expected", rows, version)
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

// TestOpen_RejectsDSNInjectionViaQuestionMark verifies that a path containing
// '?' is rejected before MkdirAll runs — no files are created in the temp dir.
func TestOpen_RejectsDSNInjectionViaQuestionMark(t *testing.T) {
	dir := t.TempDir()
	// Mirror the real attack shape: a path whose '?' injects a pragma override.
	path := filepath.Join(dir, "state.db?_pragma=journal_mode(DELETE)")

	_, err := Open(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for path containing '?', got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error should name the offending path; got: %v", err)
	}

	// MkdirAll must not have run: the temp dir must still be empty.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty temp dir after rejection, got %d entries", len(entries))
	}
}

// TestOpen_RejectsDSNInjectionViaHash verifies that a path containing '#' is
// rejected before any filesystem side-effects occur.
func TestOpen_RejectsDSNInjectionViaHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db#fragment")

	_, err := Open(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for path containing '#', got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error should name the offending path; got: %v", err)
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty temp dir after rejection, got %d entries", len(entries))
	}
}

// TestOpen_AllowsAmpersandInPath verifies that '&' alone (inert outside a query
// string) does not trigger the rejection — we must not over-restrict.
func TestOpen_AllowsAmpersandInPath(t *testing.T) {
	// Construct a path under a temp dir using a directory name with '&'.
	dir := t.TempDir()
	subdir := filepath.Join(dir, "data&files")
	path := filepath.Join(subdir, "state.db")

	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open with '&' in path should succeed, got: %v", err)
	}
	defer func() { _ = st.Close() }()
}

// TestOpen_PragmasAreEffective proves that WAL mode and foreign-key enforcement
// are actually in effect on a normally-opened store. This is the core acceptance
// criterion: the pragma block cannot be silently overridden.
func TestOpen_PragmasAreEffective(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	var journalMode string
	if err := st.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %q", journalMode)
	}

	var foreignKeys int
	if err := st.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("expected foreign_keys=1, got %d", foreignKeys)
	}
}
