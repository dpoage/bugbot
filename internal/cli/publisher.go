package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// storePublisher is the concrete daemon.Publisher implementation: it runs the
// publish reconcile loop against a live store and gh runner. The daemon wires
// one in when cfg.Publish.Enabled is true; otherwise Deps.Publisher is nil and
// the hook is skipped.
type storePublisher struct {
	gh      ghRunner
	st      *store.Store
	cfg     config.Publish
	tierMin int
	log     *slog.Logger
}

// NewStorePublisher constructs a storePublisher. gh is typically realGH; pass
// a fakeGH in tests.
func NewStorePublisher(gh ghRunner, st *store.Store, cfg config.Publish, log *slog.Logger) *storePublisher {
	return &storePublisher{
		gh:      gh,
		st:      st,
		cfg:     cfg,
		tierMin: cfg.TierMin,
		log:     log,
	}
}

// Publish implements daemon.Publisher. It discards the human-readable summary
// lines into the daemon's logger at debug level so the log stream isn't noisy
// on every cycle.
func (p *storePublisher) Publish(ctx context.Context) error {
	// Route output to a sink that writes each line at debug level.
	w := &slogWriter{log: p.log}
	return runPublish(ctx, w, p.gh, p.st, p.cfg, p.tierMin, false /* never dry-run from daemon */)
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
var _ io.Writer = (*slogWriter)(nil)
