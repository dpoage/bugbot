//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
)

// TestWriterLock_RefusesSecondWriter is the core single-writer guarantee: a
// second Open against the same on-disk path fails with *ErrLocked naming the
// holder. This is the invariant that prevents the concurrent-writer page
// corruption documented in bugbot-4d2 (MaxOpenConns(1) only serialized writers
// within one process; nothing stopped a second process from opening the same
// WAL db).
func TestWriterLock_RefusesSecondWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	if _, err := Open(ctx, path); err == nil {
		t.Fatal("second Open succeeded; want ErrLocked")
	} else {
		var locked *ErrLocked
		if !errors.As(err, &locked) {
			t.Fatalf("second Open error = %v (%T); want *ErrLocked", err, err)
		}
		if locked.HolderPID != os.Getpid() {
			t.Errorf("ErrLocked.HolderPID = %d; want this process %d", locked.HolderPID, os.Getpid())
		}
	}
}

// TestWriterLock_ReleasedOnClose proves the lock is not sticky: after Close the
// same path opens again. (The kernel also drops an flock on process exit, but
// Close must release it promptly so a long-lived process can hand the db to the
// next command.)
func TestWriterLock_ReleasedOnClose(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	_ = second.Close()
}

// TestOpenReadOnly_CoexistsWithWriter verifies a read-only open succeeds while a
// writer holds the lock and sees the writer's committed rows — report / leads /
// status must work during a scan (WAL allows one writer and many readers).
func TestOpenReadOnly_CoexistsWithWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	writer, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("writer Open: %v", err)
	}
	defer func() { _ = writer.Close() }()
	if _, err := writer.UpsertFinding(ctx, sampleFinding()); err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	reader, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly while writer holds lock: %v", err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.ListFindings(ctx, domain.FindingFilter{})
	if err != nil {
		t.Fatalf("reader ListFindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("reader saw %d findings; want 1", len(got))
	}
}

// TestWriterLock_MemoryNeverContends proves ":memory:" is exempt: two in-memory
// stores (used heavily by tests) open simultaneously without the writer lock.
func TestWriterLock_MemoryNeverContends(t *testing.T) {
	ctx := context.Background()
	a, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("first :memory: Open: %v", err)
	}
	defer func() { _ = a.Close() }()
	b, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("second :memory: Open: %v", err)
	}
	_ = b.Close()
}

// TestRecover_RefusesWhileWriterHolds: Recover must not rebuild a database a
// live writer is using — it acquires the same writer lock Open holds, so an
// open writer makes it refuse rather than swap the file out from under it.
func TestRecover_RefusesWhileWriterHolds(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	_, err = Recover(ctx, path)
	var locked *ErrLocked
	if !errors.As(err, &locked) {
		t.Fatalf("Recover while writer holds lock = %v; want *ErrLocked", err)
	}
}
