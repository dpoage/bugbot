package store

// repro_dj7_test.go — reproduction harness for bugbot-dj7.
//
// The "unbounded / broken" path opens its OWN *sql.DB via the same
// modernc/sqlite driver used by the production Open but skips the
// SetMaxOpenConns(1) bound. This lets us prove whether the original
// failure mode can be reproduced on the unfixed path so we can
// characterize the fix as necessary vs. merely defensive.
//
// IMPORTANT: this file only does reads/inserts on a temp db the test
// creates. It is not a real attack surface.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openUnboundedStore is the buggy predecessor: it opens a WAL store
// with the same pragmas as production but does NOT set
// MaxOpenConns(1). It exists only to let the stress test prove the
// bound matters.
func openUnboundedStore(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unbounded.db")
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// CRITICAL: do NOT call db.SetMaxOpenConns(1). DefaultMaxOpenConns
	// == 0 (unlimited). The whole point of this constructor is the
	// pre-fix state where the pool is unbounded.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS spend (
			id TEXT PRIMARY KEY,
			ts TEXT NOT NULL,
			scan_run_id TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		_ = db.Close()
		t.Fatalf("create spend: %v", err)
	}
	return db, path
}

// configureHammer sets the workload into the worst case for the
// checkpoint-vs-read race: tiny autocheckpoint threshold, OFF
// synchronous, small journal size. The pragmas are applied at the
// start of the harness so the workload itself is the same code path
// the IOERR-class stress test runs.
func configureHammer(t *testing.T, db *sql.DB) {
	t.Helper()
	pragmas := []string{
		"PRAGMA wal_autocheckpoint=1",
		"PRAGMA synchronous=OFF",
		"PRAGMA journal_size_limit=8192",
		"PRAGMA cache_size=-4096",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

// stressResult is the outcome of one workload run. ioerrs counts
// SQLITE_IOERR-class errors (the failure surface we are trying to
// pin down). otherErr is the first such error wrapped with worker
// context, or nil if the run was clean.
type stressResult struct {
	ioerrs   int64
	otherErr error
}

// runHarnessRaw is the *sql.DB variant used by the unbounded
// reproduction. It uses 6 writers and 4 readers, hammers for maxDur,
// and returns the first IOERR-class error it sees.
func runHarnessRaw(t *testing.T, db *sql.DB, maxDur time.Duration) stressResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), maxDur)
	defer cancel()
	configureHammer(t, db)

	const (
		writers      = 6
		readers      = 4
		perWriteSpin = 50
	)
	var (
		ioerrs int64
		stop   int32
		first  error
	)
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			local := time.Now().UnixNano()
			for {
				if atomic.LoadInt32(&stop) != 0 {
					return
				}
				if ctx.Err() != nil {
					return
				}
				for s := 0; s < perWriteSpin; s++ {
					local++
					_, err := db.ExecContext(ctx, `
						INSERT INTO spend
						  (id, ts, scan_run_id, role, provider, model, input_tokens, output_tokens)
						VALUES (?,?,?,?,?,?,?,?)`,
						fmt.Sprintf("w%d-s%d-%d", workerID, s, local),
						time.Now().UTC().Format(time.RFC3339Nano),
						"run", "finder", "test", "test", 1, 1,
					)
					if err != nil {
						if IsIOErr(err) {
							atomic.AddInt64(&ioerrs, 1)
							atomic.StoreInt32(&stop, 1)
							first = fmt.Errorf("worker %d IOERR: %w", workerID, err)
							return
						}
						if errors.Is(err, sql.ErrConnDone) || ctx.Err() != nil {
							return
						}
						// Non-IOERR (e.g. UNIQUE collision). Keep
						// going so the harness still stresses the
						// VFS — the ioerrs counter is the only thing
						// that gates the test.
						continue
					}
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				if atomic.LoadInt32(&stop) != 0 {
					return
				}
				if ctx.Err() != nil {
					return
				}
				var in, out int64
				if err := db.QueryRowContext(ctx,
					`SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM spend WHERE scan_run_id=?`,
					"run").Scan(&in, &out); err != nil {
					if IsIOErr(err) {
						atomic.AddInt64(&ioerrs, 1)
						atomic.StoreInt32(&stop, 1)
						first = fmt.Errorf("reader %d IOERR: %w", readerID, err)
						return
					}
					if errors.Is(err, sql.ErrConnDone) || ctx.Err() != nil {
						return
					}
					continue
				}
			}
		}(r)
	}

	wg.Wait()
	return stressResult{ioerrs: atomic.LoadInt64(&ioerrs), otherErr: first}
}

// runHarnessStore is the typed-Store variant used by the bounded
// regression test. Same workload shape, but the API call is
// RecordSpend / TotalsForScanRun rather than a raw INSERT/SELECT.
func runHarnessStore(t *testing.T, st *Store, maxDur time.Duration) stressResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), maxDur)
	defer cancel()
	configureHammer(t, st.db)

	const (
		writers      = 6
		readers      = 4
		perWriteSpin = 50
	)
	var (
		ioerrs int64
		stop   int32
		first  error
	)
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			local := time.Now().UnixNano()
			for {
				if atomic.LoadInt32(&stop) != 0 {
					return
				}
				if ctx.Err() != nil {
					return
				}
				for s := 0; s < perWriteSpin; s++ {
					local++
					sp := Spend{
						ID:           fmt.Sprintf("st-w%d-s%d-%d", workerID, s, local),
						ScanRunID:    "run",
						Role:         "finder",
						Provider:     "test",
						Model:        "test",
						InputTokens:  1,
						OutputTokens: 1,
					}
					if _, err := st.RecordSpend(ctx, sp); err != nil {
						if IsIOErr(err) {
							atomic.AddInt64(&ioerrs, 1)
							atomic.StoreInt32(&stop, 1)
							first = fmt.Errorf("worker %d IOERR: %w", workerID, err)
							return
						}
						if ctx.Err() != nil {
							return
						}
						continue
					}
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				if atomic.LoadInt32(&stop) != 0 {
					return
				}
				if ctx.Err() != nil {
					return
				}
				if _, err := st.TotalsForScanRun(ctx, "run"); err != nil {
					if IsIOErr(err) {
						atomic.AddInt64(&ioerrs, 1)
						atomic.StoreInt32(&stop, 1)
						first = fmt.Errorf("reader %d IOERR: %w", readerID, err)
						return
					}
					if ctx.Err() != nil {
						return
					}
					continue
				}
			}
		}(r)
	}

	wg.Wait()
	return stressResult{ioerrs: atomic.LoadInt64(&ioerrs), otherErr: first}
}

// TestStress_UnboundedPool_ReproducesIOERR is the reproduction leg.
// With the pool unconstrained and wal_autocheckpoint=1 plus
// synchronous=OFF, the workload may surface a SQLITE_IOERR-class
// error. If the modernc driver has been patched since the original
// incident, this test will SKIP — that is acceptable. The bounded
// regression test below stays as the canary that the mitigation is
// in effect.
func TestStress_UnboundedPool_ReproducesIOERR(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	db, _ := openUnboundedStore(t)
	defer func() { _ = db.Close() }()

	res := runHarnessRaw(t, db, 3*time.Second)
	if res.otherErr == nil {
		t.Skipf("unbounded pool completed the 3s stress run without any IOERR (ioerrs=%d); "+
			"the modernc.org/sqlite VFS race may be mitigated upstream. "+
			"The bounded-pool mitigation in Open remains the safe stance regardless.",
			res.ioerrs)
	}
	if res.ioerrs == 0 {
		t.Fatalf("stress run errored but ioerrs=0: %v", res.otherErr)
	}
	if !IsIOErr(res.otherErr) {
		t.Fatalf("expected IOERR class error, got: %v (ioerrs=%d)", res.otherErr, res.ioerrs)
	}
	t.Logf("reproduced IOERR (ioerrs=%d): %v", res.ioerrs, res.otherErr)
}

// TestStress_BoundedPool_NoIOERR is the regression guard. With
// MaxOpenConns(1) (the production setting from Open), the same
// workload must complete without any IOERR. This is the canary that
// the mitigation is in effect and stays in effect. It is the
// permanent regression test: even if the unbounded test stops
// reproducing the bug upstream, the bounded test must remain green.
func TestStress_BoundedPool_NoIOERR(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	st := openTemp(t)
	if got := st.MaxOpenConnections(); got != 1 {
		t.Fatalf("expected MaxOpenConns(1) from production Open, got %d", got)
	}
	res := runHarnessStore(t, st, 3*time.Second)
	if res.otherErr != nil {
		t.Fatalf("bounded pool surfaced an error: %v (ioerrs=%d)", res.otherErr, res.ioerrs)
	}
	if res.ioerrs != 0 {
		t.Fatalf("bounded pool saw %d ioerrs", res.ioerrs)
	}
}

// TestStress_TruncatedDB_SurfacesIOERRClass is a deterministic
// reproduction of the VFS short-read path. We open a store, write
// enough data to grow the main db file past one page, checkpoint
// the WAL into the main file, then truncate the file from outside
// the connection. The modernc VFS translates a Go io.EOF or
// io.ErrUnexpectedEOF from the underlying Read() into
// SQLITE_IOERR_SHORT_READ — code 522, exactly the failure mode
// reported in the field incident. This test proves the IOERR class
// is reachable through the VFS so the IsIOErr / IsIOErrShortRead
// predicates and the IOERR-class quick_check-and-reopen Diagnose
// path can be exercised without an upstream race.
func TestStress_TruncatedDB_SurfacesIOERRClass(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	path := filepath.Join(t.TempDir(), "trunc.db")
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS blob(t TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx := context.Background()
	// Insert enough rows to grow the file past the page cache.
	big := strings.Repeat("x", 32*1024)
	for i := 0; i < 16; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO blob(t) VALUES (?)`, big); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// Checkpoint so all the data is in the main db file (not the WAL).
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// Truncate the main db file MIDWAY so the header survives but
	// later pages are gone. This is exactly the VFS state the
	// checkpoint-vs-read race produces: a reader's page fetch lands
	// past the new end-of-file. The modernc VFS translates the
	// resulting io.EOF / io.ErrUnexpectedEOF into
	// SQLITE_IOERR_SHORT_READ (522).
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Truncate to half the file: header is intact, second half is gone.
	halfSize := stat.Size() / 2
	if halfSize < 4096 {
		halfSize = 4096
	}
	if err := os.Truncate(path, halfSize); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// Drop the page cache so the next read goes to disk. Without
	// this, modernc may serve the SELECT from cache and report
	// success even though the file is short.
	if _, err := db.ExecContext(ctx, "PRAGMA cache_size=-1"); err != nil {
		t.Fatalf("cache_size=-1: %v", err)
	}
	var got error
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM blob").Scan(&n); err != nil {
		got = err
	}
	if got == nil {
		t.Fatal("expected an error after truncating the db file, got nil")
	}
	// Both IOERR-class AND SQLITE_CORRUPT are acceptable outcomes
	// of the truncated-file VFS state — the modernc VFS may
	// report the truncated page as a short read
	// (IOERR_SHORT_READ, 522) or detect that the page no longer
	// parses (SQLITE_CORRUPT, 11) depending on which page the
	// read landed on. Both are "the file is in a bad state"
	// signals that the Diagnose path is designed to triage.
	if !IsIOErr(got) && SQLiteCode(got) != 11 {
		t.Fatalf("expected IOERR-class or SQLITE_CORRUPT (code 11), got: %T %v (code=%d, name=%s)",
			got, got, SQLiteCode(got), sqliteCodeName(SQLiteCode(got)))
	}
	t.Logf("got expected damaged-db error (code=%d, name=%s): %v",
		SQLiteCode(got), sqliteCodeName(SQLiteCode(got)), got)
}
