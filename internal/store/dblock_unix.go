//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// acquireWriteLock takes a non-blocking exclusive flock on the sidecar lock
// file for dbPath. On success it records the current PID in the file (for the
// diagnostic in ErrLocked) and returns the held lock. On contention it returns
// *ErrLocked naming the holder. The kernel drops the flock when the returned
// lock's fd is closed (release) or the process exits, so there is no stale-lock
// state to clean up.
func acquireWriteLock(dbPath string) (*dbLock, error) {
	lp := lockPath(dbPath)
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open lock file %q: %w", lp, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readLockPID(f) // best-effort, for the message only
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &ErrLocked{Path: dbPath, HolderPID: holder}
		}
		return nil, fmt.Errorf("store: flock %q: %w", lp, err)
	}
	// We hold the lock. Record our PID so a future contender can name us. A
	// contender that reads between our Truncate and WriteAt simply sees an
	// empty file and reports HolderPID 0 — a cosmetic degradation, never a
	// correctness issue.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
		_ = f.Sync()
	}
	return &dbLock{f: f}, nil
}

// readLockPID parses the decimal PID the holder wrote at offset 0. Returns 0
// on any read/parse failure.
func readLockPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	return pid
}

// release drops the flock and closes the fd. Closing alone would release the
// lock; the explicit LOCK_UN makes the intent legible and releases promptly
// even if the *os.File lingers in a finalizer queue.
func (l *dbLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}
