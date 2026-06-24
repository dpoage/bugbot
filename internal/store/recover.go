package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrCorrupt is the sentinel wrapped by Check when the database fails PRAGMA
// quick_check. Callers branch with errors.Is(err, ErrCorrupt) to decide whether
// to stop writing and invoke Recover.
var ErrCorrupt = errors.New("store: database integrity check failed")

// Check runs PRAGMA quick_check on the live handle and returns nil when the
// database reports "ok". On any other result it returns an error wrapping
// ErrCorrupt with the (bounded) problem lines so an operator can branch to
// Recover. quick_check is the fast, allocation-light cousin of integrity_check
// — it skips the expensive index-vs-table cross-validation but still catches
// torn pages and out-of-range cell offsets, which is the corruption class seen
// in bugbot-4d2. It is meant to be run periodically by the daemon and on demand
// by `bugbot doctor`.
//
// Unlike Diagnose (which reopens a second connection to triage an IOERR), Check
// only inspects the live handle and never opens or mutates anything, so it is
// cheap enough to call every daemon cycle.
func (s *Store) Check(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store: not open")
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return annotateErr(s.path, "quick_check", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return annotateErr(s.path, "quick_check", err)
		}
		lines = append(lines, line)
		if len(lines) >= 20 {
			// A badly torn database can report thousands of problem lines;
			// the first handful is enough for an operator to act on.
			lines = append(lines, "… (truncated)")
			break
		}
	}
	if err := rows.Err(); err != nil {
		return annotateErr(s.path, "quick_check", err)
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrCorrupt, strings.Join(lines, "; "))
}

// RecoverReport summarizes the outcome of Recover for the operator log.
type RecoverReport struct {
	// BackupPath is where the corrupt database was moved before the rebuilt one
	// took its place. Empty only when there was no pre-existing file.
	BackupPath string
	// Salvaged maps table name → number of rows copied into the rebuilt db.
	Salvaged map[string]int
	// Partial lists tables whose read aborted mid-scan (corruption hit before
	// the table was fully drained); their Salvaged count is what survived.
	Partial []string
	// SourceOpenErr, when non-empty, means the corrupt database could not be
	// opened read-only at all; the rebuilt db is a fresh empty schema and no
	// rows were salvaged.
	SourceOpenErr string
}

// TotalSalvaged returns the sum of rows copied across all tables.
func (r *RecoverReport) TotalSalvaged() int {
	n := 0
	for _, c := range r.Salvaged {
		n += c
	}
	return n
}

// Recover rebuilds a corrupt database in place, best-effort salvaging readable
// rows. It is the codified, automated form of the manual recovery performed in
// the bugbot-4d2 incident. It is NEVER invoked automatically (a false-positive
// rebuild that silently drops rows would be worse than the corruption it
// guards against) — it is an explicit operator action behind `bugbot doctor
// --repair`.
//
// Steps:
//  1. Take the cross-process writer lock so no other process is mid-write.
//  2. Build a fresh, correctly-migrated database at "<path>.rebuild" with
//     foreign-key enforcement OFF (so partially-salvaged rows referencing a
//     dropped parent still land).
//  3. Open the corrupt database read-only and, for every table in the fresh
//     schema, copy rows it can still read (INSERT OR IGNORE), tolerating a
//     mid-table read failure by keeping whatever was drained first.
//  4. quick_check the rebuilt database; abort (leaving the original untouched)
//     if it is not clean.
//  5. Move the corrupt file aside to "<path>.corrupt-<UTC timestamp>" and the
//     rebuilt file into place.
//
// The store MUST NOT be open elsewhere in this process when Recover runs (it
// acquires the same writer lock Open holds). Callers run it against a closed
// path, e.g. from a CLI command before opening the store normally.
func Recover(ctx context.Context, path string) (*RecoverReport, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("store: cannot recover path %q", path)
	}

	lock, err := acquireWriteLock(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.release() }()

	rep := &RecoverReport{Salvaged: map[string]int{}}

	// 1. Fresh, migrated rebuild target with FK enforcement off for salvage.
	rebuildPath := path + ".rebuild"
	removeDBFiles(rebuildPath)
	fresh, err := sql.Open("sqlite", rebuildPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(OFF)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, annotateErr(rebuildPath, "recover.open_rebuild", err)
	}
	fresh.SetMaxOpenConns(1)
	if err := fresh.PingContext(ctx); err != nil {
		_ = fresh.Close()
		return nil, annotateErr(rebuildPath, "recover.ping_rebuild", err)
	}
	if err := migrate(ctx, fresh); err != nil {
		_ = fresh.Close()
		return nil, annotateErr(rebuildPath, "recover.migrate_rebuild", err)
	}

	// 2. Open the corrupt source read-only, best-effort.
	src, srcErr := sql.Open("sqlite", path+"?mode=ro&_pragma=busy_timeout(2000)")
	if srcErr == nil {
		src.SetMaxOpenConns(1)
		srcErr = src.PingContext(ctx)
	}
	if srcErr == nil {
		salvageRows(ctx, src, fresh, rep)
		_ = src.Close()
	} else {
		rep.SourceOpenErr = srcErr.Error()
		if src != nil {
			_ = src.Close()
		}
	}

	// 3. The rebuilt db must itself be clean before we trust it.
	var chk string
	if err := fresh.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&chk); err != nil {
		_ = fresh.Close()
		return rep, annotateErr(rebuildPath, "recover.verify_rebuild", err)
	}
	if err := fresh.Close(); err != nil {
		return rep, annotateErr(rebuildPath, "recover.close_rebuild", err)
	}
	if chk != "ok" {
		return rep, fmt.Errorf("store: rebuilt database failed quick_check (%q); original left untouched at %s", chk, path)
	}

	// 4. Swap: corrupt aside, rebuilt into place. The backup name carries a
	//    nanosecond UTC timestamp so repeated repairs of the same path never
	//    collide, and we refuse to overwrite an existing backup so an earlier
	//    forensic copy is never silently clobbered.
	rep.BackupPath = path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if _, statErr := os.Stat(rep.BackupPath); statErr == nil {
		return rep, fmt.Errorf("store: backup path %q already exists; refusing to overwrite an earlier forensic copy", rep.BackupPath)
	}
	if err := os.Rename(path, rep.BackupPath); err != nil {
		if !os.IsNotExist(err) {
			return rep, fmt.Errorf("store: back up corrupt db %q: %w", path, err)
		}
		rep.BackupPath = "" // nothing was there to back up
	} else {
		// Move the corrupt WAL/SHM alongside so they don't shadow the new file.
		_ = os.Rename(path+"-wal", rep.BackupPath+"-wal")
		_ = os.Rename(path+"-shm", rep.BackupPath+"-shm")
	}
	if err := os.Rename(rebuildPath, path); err != nil {
		return rep, fmt.Errorf("store: install rebuilt db at %q: %w", path, err)
	}
	// The rebuild's own WAL/SHM are checkpointed+removed on its Close above in
	// the common case; sweep any stragglers so the new db opens clean.
	_ = os.Rename(rebuildPath+"-wal", path+"-wal")
	_ = os.Rename(rebuildPath+"-shm", path+"-shm")
	return rep, nil
}

// salvageRows copies every readable row of every fresh-schema table from src
// into dst, recording counts and partial-read tables in rep. dst has FK
// enforcement off, so rows whose parent did not survive still land.
func salvageRows(ctx context.Context, src, dst *sql.DB, rep *RecoverReport) {
	for _, table := range listTables(ctx, dst) {
		// schema_migrations is owned by migrate() on the fresh db; copying it
		// would be a no-op at best and could regress the version at worst.
		if table == "schema_migrations" {
			continue
		}
		cols := tableColumns(ctx, dst, table)
		if len(cols) == 0 {
			continue
		}
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		}
		colList := strings.Join(quoted, ", ")
		insert := fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (%s) VALUES (%s)`,
			strings.ReplaceAll(table, `"`, `""`), colList, buildPlaceholders(len(cols)))

		rows, err := src.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM "%s"`,
			colList, strings.ReplaceAll(table, `"`, `""`)))
		if err != nil {
			rep.Partial = append(rep.Partial, table)
			continue
		}
		n := 0
		partial := false
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				partial = true
				break
			}
			if _, err := dst.ExecContext(ctx, insert, vals...); err != nil {
				// A single unsalvageable row (e.g. a NOT NULL violation from a
				// garbled cell) is skipped, not fatal.
				continue
			}
			n++
		}
		if err := rows.Err(); err != nil {
			partial = true
		}
		_ = rows.Close()
		rep.Salvaged[table] = n
		if partial {
			rep.Partial = append(rep.Partial, table)
		}
	}
}

// listTables returns the user table names in db (excluding sqlite internal
// tables), best-effort: a query error yields an empty list rather than failing
// the whole recovery.
func listTables(ctx context.Context, db *sql.DB) []string {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// tableColumns returns the column names of table in db via PRAGMA table_info,
// in schema order. Empty on any error.
func tableColumns(ctx context.Context, db *sql.DB, table string) []string {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info("%s")`,
		strings.ReplaceAll(table, `"`, `""`)))
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return cols
		}
		cols = append(cols, name)
	}
	return cols
}

// removeDBFiles deletes a SQLite database file and its WAL/SHM sidecars,
// ignoring missing-file errors. Used to clear a stale rebuild target.
func removeDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
