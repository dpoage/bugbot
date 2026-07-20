package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/daemon"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/tracker"
)

// storePublisher is the concrete daemon.Publisher implementation: it runs the
// publish reconcile loop against a live store and issue tracker. The daemon
// wires one in when cfg.Publish.Enabled is true; otherwise Deps.Publisher is
// nil and the hook is skipped.
type storePublisher struct {
	tr      tracker.Tracker
	st      *store.Store
	cfg     config.Publish
	tierMin int
	log     *slog.Logger
	// disabled latches when the tracker's client tooling is missing
	// (tracker.ErrMissingPrereq, e.g. the CLI binary the adapter shells out
	// to is not on PATH): that condition is stable for the daemon's
	// lifetime, so we warn ONCE and stop attempting publishes instead of
	// re-warning every cycle.
	disabled atomic.Bool
}

// NewStorePublisher constructs a storePublisher over the given tracker. The
// daemon wires one in when cfg.Publish.Enabled is true; otherwise
// Deps.Publisher stays nil and the post-cycle hook is skipped.
func NewStorePublisher(tr tracker.Tracker, st *store.Store, cfg config.Publish, log *slog.Logger) *storePublisher {
	return &storePublisher{
		tr:      tr,
		st:      st,
		cfg:     cfg,
		tierMin: cfg.TierMin,
		log:     log,
	}
}

// Publish implements daemon.Publisher. It discards the human-readable summary
// lines into the daemon's logger at debug level so the log stream isn't noisy
// on every cycle. A missing tracker prerequisite warns once and latches the
// publisher off for the daemon's lifetime; other errors are returned to the
// caller (which logs but never fails the cycle).
func (p *storePublisher) Publish(ctx context.Context) error {
	if p.disabled.Load() {
		return nil
	}
	// Route output to a sink that writes each line at debug level.
	w := &slogWriter{log: p.log}
	err := runPublish(ctx, w, p.tr, p.st, p.cfg, p.tierMin, false /* never dry-run from daemon */)
	if errors.Is(err, tracker.ErrMissingPrereq) {
		p.disabled.Store(true)
		p.log.Warn("daemon: publish disabled for this run: tracker prerequisite missing", "err", err)
		return nil
	}
	return err
}

// slogWriter is an io.Writer that writes each newline-terminated line to the
// slog logger at debug level. The daemon uses it to suppress publish summary
// lines from the operator-visible INFO stream without losing them entirely.
type slogWriter struct {
	log *slog.Logger
	buf []byte
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := indexOf(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if line != "" {
			w.log.Debug("daemon: publish", "msg", line)
		}
	}
	return len(p), nil
}

// indexOf returns the index of b in buf, or -1 if not found.
func indexOf(buf []byte, b byte) int {
	for i, c := range buf {
		if c == b {
			return i
		}
	}
	return -1
}

// Ensure storePublisher satisfies the io.Writer interface (used internally via
// slogWriter) and the daemon.Publisher interface (verified at compile time by
// the daemon package via the interface type).
var (
	_ io.Writer        = (*slogWriter)(nil)
	_ daemon.Publisher = (*storePublisher)(nil)
)
