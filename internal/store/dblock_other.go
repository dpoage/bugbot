//go:build !unix

package store

// acquireWriteLock is a no-op on platforms without flock (Windows, plan9, js).
// Cross-process write serialization is unavailable there; the single-process
// MaxOpenConns(1) bound still applies. bugbot's supported deployment targets
// are unix (Linux containers), so this is graceful degradation on an
// unsupported platform, not a sanctioned concurrency mode.
func acquireWriteLock(dbPath string) (*dbLock, error) { return &dbLock{}, nil }

func (l *dbLock) release() error { return nil }
