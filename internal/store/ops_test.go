package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// forceBusyErr produces a genuine SQLITE_BUSY *sqlite.Error: one connection
// holds the WAL write lock via an immediate transaction, and a second writer —
// opened with busy_timeout(0) so it does not wait — fails to acquire it. This
// is the only deterministic way to get a real transient sqlite error (the type
// has no exported constructor), so the retry tests below exercise the true
// classification path rather than a stand-in sentinel.
func forceBusyErr(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)&_txlock=immediate"

	a, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.SetMaxOpenConns(1)
	if _, err := a.ExecContext(ctx, `CREATE TABLE t(x)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	b, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.SetMaxOpenConns(1)

	// a holds the write lock for the rest of the test via an immediate tx.
	txA, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin a: %v", err)
	}
	t.Cleanup(func() { _ = txA.Rollback() })
	if _, err := txA.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err != nil {
		t.Fatalf("insert a: %v", err)
	}

	// b cannot get the write lock and, with busy_timeout(0), returns now.
	_, err = b.ExecContext(ctx, `INSERT INTO t VALUES (2)`)
	if err == nil {
		t.Fatal("expected SQLITE_BUSY from the second concurrent writer")
	}
	return err
}

// TestIsTransient_OnlyBusyAndShortRead is the safety property of the retry
// handler: nil and a non-transient SQLite error (here a PRIMARY KEY constraint
// violation) must NOT be classified transient — misclassifying a logic error as
// transient would loop maxOpAttempts times before surfacing the real cause.
func TestIsTransient_OnlyBusyAndShortRead(t *testing.T) {
	if isTransient(nil) {
		t.Error("isTransient(nil) = true; want false")
	}

	ctx := context.Background()
	st := openTemp(t)
	if _, err := st.db.ExecContext(ctx, `CREATE TABLE t_pk (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO t_pk (id) VALUES (1)`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, dupErr := st.db.ExecContext(ctx, `INSERT INTO t_pk (id) VALUES (1)`)
	if dupErr == nil {
		t.Fatal("expected a PRIMARY KEY constraint error on duplicate insert")
	}
	if isTransient(dupErr) {
		t.Errorf("constraint error %v classified transient; must not be retried", dupErr)
	}
	if code := SQLiteCode(dupErr); code != sqliteCONSTRAINTPrimaryKey {
		t.Errorf("dup insert code = %d; want %d (SQLITE_CONSTRAINT_PRIMARYKEY)", code, sqliteCONSTRAINTPrimaryKey)
	}
}

// TestIsTransient_BusyIsTransient proves the positive classification with a real
// SQLITE_BUSY error: it MUST be retried.
func TestIsTransient_BusyIsTransient(t *testing.T) {
	busy := forceBusyErr(t)
	if !isTransient(busy) {
		t.Fatalf("real SQLITE_BUSY (code=%d) classified non-transient; want transient: %v",
			SQLiteCode(busy), busy)
	}
}

// TestRetry_RetriesTransientUntilBudgetThenSurfaces: a persistently-transient
// op is retried exactly maxOpAttempts times, then the error is surfaced (we do
// not spin forever, and we do not swallow the failure).
func TestRetry_RetriesTransientUntilBudgetThenSurfaces(t *testing.T) {
	busy := forceBusyErr(t)
	if !isTransient(busy) {
		t.Fatalf("precondition: forced error not classified transient (code=%d)", SQLiteCode(busy))
	}
	st := openTemp(t)
	calls := 0
	err := st.retry(context.Background(), func() error {
		calls++
		return busy
	})
	if calls != maxOpAttempts {
		t.Errorf("op called %d times; a persistently-transient error must retry maxOpAttempts=%d", calls, maxOpAttempts)
	}
	if !errors.Is(err, busy) {
		t.Errorf("retry returned %v; want the transient error surfaced after exhausting retries", err)
	}
}

// TestRetry_CtxCancelAbortsMidBackoff: a cancelled context aborts the retry loop
// at the first backoff rather than burning the whole attempt budget.
func TestRetry_CtxCancelAbortsMidBackoff(t *testing.T) {
	busy := forceBusyErr(t)
	st := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the first attempt's backoff

	calls := 0
	err := st.retry(ctx, func() error {
		calls++
		return busy
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("retry under cancelled ctx returned %v; want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("op called %d times; cancel must abort after the first transient attempt (want 1)", calls)
	}
}

// TestRetry_NonTransientReturnsAfterOneAttempt: the loop must NOT retry a
// non-transient error — it returns it after exactly one call.
func TestRetry_NonTransientReturnsAfterOneAttempt(t *testing.T) {
	st := openTemp(t)
	sentinel := errors.New("boom")
	calls := 0
	err := st.retry(context.Background(), func() error {
		calls++
		return sentinel
	})
	if calls != 1 {
		t.Errorf("op called %d times; a non-transient error must not retry (want 1)", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("retry returned %v; want the sentinel", err)
	}
}

// TestRetry_SuccessSingleAttempt: a succeeding op runs exactly once.
func TestRetry_SuccessSingleAttempt(t *testing.T) {
	st := openTemp(t)
	calls := 0
	if err := st.retry(context.Background(), func() error { calls++; return nil }); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls != 1 {
		t.Errorf("op called %d times; want 1", calls)
	}
}
