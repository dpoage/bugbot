package store

import (
	"fmt"
	"os"
)

// dbLock is a held cross-process advisory write lock on the state database. It
// guarantees at most one writer process per on-disk path — the invariant that
// prevents the concurrent-writer page corruption documented in bugbot-4d2.
// MaxOpenConns(1) only serializes writers WITHIN a single process; nothing
// stopped a second bugbot process from opening the same WAL database and
// interleaving writes, which is what cross-contaminated pages in the field.
//
// The lock is advisory (flock/LOCK_EX): cooperating bugbot processes honor it,
// but it does not stop an unrelated tool from writing the file. The kernel
// releases an flock automatically when the holding fd is closed or the process
// dies, so a crashed writer never leaves a stale lock behind — there is nothing
// to time out and no recovery step. The lock is held for the lifetime of a
// writable *Store and dropped by Close.
type dbLock struct {
	f *os.File
}

// ErrLocked is returned by Open when another process already holds the writer
// lock on the same database path. HolderPID is the PID that process recorded
// in the lock file (0 when it could not be read — e.g. the holder had not yet
// written its PID, or the file was empty).
type ErrLocked struct {
	Path      string
	HolderPID int
}

func (e *ErrLocked) Error() string {
	if e.HolderPID > 0 {
		return fmt.Sprintf("store: database %q is locked by another bugbot process (pid %d); only one writer per state db is allowed (use a read-only command, or wait for the writer to finish)", e.Path, e.HolderPID)
	}
	return fmt.Sprintf("store: database %q is locked by another bugbot process; only one writer per state db is allowed", e.Path)
}

// lockPath is the sidecar advisory-lock file for a database path. It is kept
// separate from the database file so acquiring the lock never opens or touches
// db pages (and so the lock survives a Recover that replaces the db file).
func lockPath(dbPath string) string { return dbPath + ".lock" }
