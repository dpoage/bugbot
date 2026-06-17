package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	// "modernc.org/sqlite" is required at test time because errors.As needs
	// to match a *sqlite.Error the driver actually returns. The annotation
	// helper above is the production-side consumer; this test exercises the
	// real path by triggering a genuine SQL error.
	_ "modernc.org/sqlite"
)

// TestAnnotateErr_FormatsPathOpCode is the core acceptance check: a future
// SQLITE_IOERR_SHORT_READ (522) must be distinguishable from BUSY (5) and
// FULL (13) in the formatted log line. We force a real *sqlite.Error by
// inserting a duplicate primary key into spend, then assert the wrapper
// renders path + op + code + name + original message.
func TestAnnotateErr_FormatsPathOpCode(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	// Force a UNIQUE violation on spend.id by inserting the same id twice.
	// This is a *sqlite.Error with code SQLITE_CONSTRAINT (19), which is
	// sufficient to prove the *sqlite.Error type-assertion path works.
	dupID := "test-dup-id"
	sp := Spend{ID: dupID, ScanRunID: "r1", Role: "finder", InputTokens: 1, OutputTokens: 1}
	if _, err := st.RecordSpend(ctx, sp); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := st.RecordSpend(ctx, sp)
	if err == nil {
		t.Fatal("expected unique-constraint error on duplicate id")
	}

	var ann *annotatedError
	if !errors.As(err, &ann) {
		t.Fatalf("expected *annotatedError, got %T: %v", err, err)
	}
	if ann.Path != st.Path() {
		t.Errorf("Path = %q, want %q", ann.Path, st.Path())
	}
	if ann.Op != "record_spend" {
		t.Errorf("Op = %q, want %q", ann.Op, "record_spend")
	}
	if ann.Code != 1555 {
		t.Errorf("Code = %d, want 1555 (SQLITE_CONSTRAINT_PRIMARYKEY)", ann.Code)
	}
	if ann.CodeName != "SQLITE_CONSTRAINT_PRIMARYKEY" {
		t.Errorf("CodeName = %q, want SQLITE_CONSTRAINT_PRIMARYKEY", ann.CodeName)
	}
	if !strings.Contains(ann.Err.Error(), "UNIQUE") && !strings.Contains(ann.Err.Error(), "constraint") {
		t.Errorf("underlying error did not mention UNIQUE/constraint: %v", ann.Err)
	}

	// The formatted message must include the path, the op, the code, and
	// the SQLite name — in that order, so a log line lets a human pick the
	// row out of a wall of text.
	msg := ann.Error()
	for _, want := range []string{"store:", "record_spend", st.Path(), "[1555]", "SQLITE_CONSTRAINT_PRIMARYKEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("formatted error missing %q: %s", want, msg)
		}
	}
}

// TestAnnotateErr_NonSQLiteFallback proves the helper does not panic or
// mis-format when the underlying error is not a *sqlite.Error (e.g. a Go
// stdlib error from a closed connection, a context cancellation, an I/O
// error from the OS). The code is reported as -1 with name "SQLITE_NONSQLITE"
// so the formatter stays regular.
func TestAnnotateErr_NonSQLiteFallback(t *testing.T) {
	base := errors.New("disk gone")
	wrapped := annotateErr("/tmp/x.db", "ping", base)

	var ann *annotatedError
	if !errors.As(wrapped, &ann) {
		t.Fatalf("expected *annotatedError, got %T", wrapped)
	}
	if ann.Code != -1 {
		t.Errorf("Code = %d, want -1 for non-sqlite error", ann.Code)
	}
	if ann.CodeName != "SQLITE_NONSQLITE" {
		t.Errorf("CodeName = %q, want SQLITE_NONSQLITE", ann.CodeName)
	}
	// errors.Unwrap must reach the original error.
	if errors.Unwrap(wrapped) != base {
		t.Errorf("errors.Unwrap did not return the base error")
	}
	if !strings.Contains(wrapped.Error(), "disk gone") {
		t.Errorf("formatted error should preserve the base message: %s", wrapped.Error())
	}
}

// TestAnnotateErr_NilReturnsNil pins the obvious contract: passing a nil
// error in must produce a nil error out, so call sites can `return
// annotateErr(...), err` without a nil-check.
func TestAnnotateErr_NilReturnsNil(t *testing.T) {
	if got := annotateErr("/tmp/x.db", "open", nil); got != nil {
		t.Errorf("annotateErr(nil) = %v, want nil", got)
	}
}

// TestSQLiteCode_IsolatesKnownCodes asserts IsIOErr / IsIOErrShortRead /
// SQLiteCode classify real *sqlite.Error values correctly. We trigger
// CONSTRAINT (19) here for portability; the IOERR class is exercised by
// the formatter test below and by the stress test that uses the
// low-wal_autocheckpoint path.
func TestSQLiteCode_IsolatesKnownCodes(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	dupID := "test-dup-2"
	sp := Spend{ID: dupID, ScanRunID: "r1", InputTokens: 1, OutputTokens: 1}
	if _, err := st.RecordSpend(ctx, sp); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := st.RecordSpend(ctx, sp)
	if err == nil {
		t.Fatal("expected unique-constraint error")
	}
	if got := SQLiteCode(err); got != 1555 {
		t.Errorf("SQLiteCode = %d, want 1555 (CONSTRAINT_PRIMARYKEY)", got)
	}
	if IsIOErr(err) {
		t.Errorf("CONSTRAINT (1555) must not classify as IOERR class")
	}
	if IsIOErrShortRead(err) {
		t.Errorf("CONSTRAINT (1555) must not classify as IOERR_SHORT_READ")
	}
}

// TestSQLiteCodeName_ShortRead is a unit-level check on the code-name
// table. We do not trigger a real 522 (it is a race-condition symptom, not
// a deterministic driver return), but the table is what makes a future 522
// log self-explaining, so it gets a direct test.
func TestSQLiteCodeName_ShortRead(t *testing.T) {
	cases := map[int]string{
		0:          "SQLITE_OK",
		5:          "SQLITE_BUSY",
		13:         "SQLITE_FULL",
		10:         "SQLITE_IOERR",
		266:        "SQLITE_IOERR_READ",
		522:        "SQLITE_IOERR_SHORT_READ",
		0xdeadbeef: "SQLITE_0xdeadbeef", // unknown codes get a hex name
	}
	for code, want := range cases {
		if got := sqliteCodeName(code); got != want {
			t.Errorf("sqliteCodeName(%d) = %q, want %q", code, got, want)
		}
	}
}

// TestIsIOErrShortRead_NilSafe pins the nil-safety contract on the public
// predicate. Operators / log filters use this in conditional branches, and
// a panic on nil would crash the very log line meant to triage the failure.
func TestIsIOErrShortRead_NilSafe(t *testing.T) {
	if IsIOErrShortRead(nil) {
		t.Error("IsIOErrShortRead(nil) = true, want false")
	}
	if IsIOErr(nil) {
		t.Error("IsIOErr(nil) = true, want false")
	}
	if SQLiteCode(nil) != 0 {
		t.Errorf("SQLiteCode(nil) = %d, want 0", SQLiteCode(nil))
	}
}

// TestAnnotateErr_AcceptsRawSQLiteError is a backstop: the predicate
// helpers (IsIOErr / IsIOErrShortRead / SQLiteCode) must continue to work
// when the error is the *annotatedError that annotateErr returned, not
// only the raw *sqlite.Error. This is the IOERR-triage path's whole
// point — a caller that catches a wrapped error must be able to ask
// "was this a 522?" without first unwrapping it themselves.
func TestAnnotateErr_AcceptsRawSQLiteError(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	dupID := "test-dup-3"
	sp := Spend{ID: dupID, ScanRunID: "r1", InputTokens: 1, OutputTokens: 1}
	if _, err := st.RecordSpend(ctx, sp); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := st.RecordSpend(ctx, sp)
	if err == nil {
		t.Fatal("expected unique-constraint error")
	}
	if IsIOErr(err) {
		t.Errorf("CONSTRAINT must not classify as IOERR even when wrapped")
	}
	if IsIOErrShortRead(err) {
		t.Errorf("CONSTRAINT must not classify as IOERR_SHORT_READ even when wrapped")
	}
	if got := SQLiteCode(err); got != 1555 {
		t.Errorf("SQLiteCode through wrapper = %d, want 1555", got)
	}
}

// TestDiagnose_OnCleanStore is the IOERR-triage acceptance test: on a
// well-formed store, Diagnose runs PRAGMA quick_check on the existing
// connection AND on a separately-opened short-lived connection, then
// returns nil. The live *sql.DB handle must not be rebound (a
// concurrent caller holding the same *Store pointer must continue to
// see the same *sql.DB after Diagnose).
func TestDiagnose_OnCleanStore(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Touch the schema so quick_check has something to validate.
	if _, err := st.BeginScanRun(ctx, ScanSweep, "abc"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Capture the live *sql.DB pointer so we can prove Diagnose
	// does NOT swap it.
	liveDB := st.db
	livePath := st.path

	if err := st.Diagnose(ctx); err != nil {
		t.Fatalf("Diagnose on clean store returned err: %v", err)
	}

	if st.db != liveDB {
		t.Fatalf("Diagnose rebound the live *sql.DB pointer (concurrent callers would race)")
	}
	if st.path != livePath {
		t.Fatalf("Diagnose changed the store path: %q != %q", st.path, livePath)
	}

	// Concurrent-style follow-up: after Diagnose, the original
	// handle must still answer queries.
	if _, err := st.BeginScanRun(ctx, ScanSweep, "def"); err != nil {
		t.Fatalf("post-Diagnose BeginScanRun: %v", err)
	}
}

// TestOpen_WriterPoolBounded is the mitigation acceptance: Open must
// configure the *sql.DB with MaxOpenConns(1). This is a property of the
// open store, not a separate configuration, so we read it through the
func TestOpen_WriterPoolBounded(t *testing.T) {
	st := openTemp(t)
	if got := st.MaxOpenConnections(); got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 (writer pool must be bound)", got)
	}
	// And sanity-check that the underlying *sql.DB reports the same.
	if got := st.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("db.Stats().MaxOpenConnections = %d, want 1", got)
	}
}
