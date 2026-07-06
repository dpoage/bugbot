// Package engine hosts the cobra-free orchestration core behind every Bugbot
// CLI command: config/store bootstrap, LLM role-client resolution, funnel
// options plumbing, the advisory single-scan lock + heartbeat, and the verb
// bodies (Scan, Verify, Repro, Sweep, Review) that used to live inline in
// internal/cli's cobra RunE closures.
//
// Splitting this out serves two goals: it lets internal/cli stay a thin
// flag-parsing / presentation layer, and it gives future non-cobra frontends
// (starting with the Observer TUI, internal/tui) a single, dependency-free
// entry point — a Dispatcher — instead of re-deriving the wiring.
//
// internal/engine MUST NOT import github.com/spf13/cobra or read *cobra.Command
// flags; every input a verb needs travels through its Opts struct.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/repro"
	"github.com/dpoage/bugbot/internal/store"
)

// Mode reports whether a Dispatcher holds the store's cross-process writer
// lock (Owner) or opened it read-only because another process already looks
// like it is actively scanning (Observer).
type Mode int

const (
	// Owner holds the writer lock (or has never contended for it) and may
	// dispatch every verb. A background heartbeat goroutine keeps its
	// scan_runs row fresh for the lifetime of the Dispatcher.
	Owner Mode = iota
	// Observer opened the store read-only because ActiveScanRuns reported a
	// live run when the Dispatcher was created. Dispatch verbs refuse with
	// ErrObserver unless the caller's Opts carry Force=true, in which case
	// the Dispatcher escalates to Owner on first use.
	Observer
)

func (m Mode) String() string {
	switch m {
	case Owner:
		return "owner"
	case Observer:
		return "observer"
	default:
		return "unknown"
	}
}

// ErrObserver is the sentinel dispatch-refusal error: use errors.Is(err,
// ErrObserver) to detect it. In Owner mode, or when Force resolves the
// contention, dispatch verbs never return it. When a real conflict is found
// (Observer mode, no/failed Force escalation), the returned error is an
// *observerConflictError whose Error() text is checkScanLock's verbatim
// message (run id + pid + "pass --force") — CLI output for scan/verify/sweep
// stays byte-identical to the pre-refactor checkScanLock passthrough, while
// errors.Is(err, ErrObserver) still reports true via observerConflictError's
// Is method.
var ErrObserver = errors.New("engine: dispatch refused in observer mode (another scan appears active)")

// observerConflictError wraps checkScanLock's conflict message so ensureOwner
// can report errors.Is(err, ErrObserver) without prefixing (and thereby
// changing) the message text CLI commands print verbatim.
type observerConflictError struct{ msg string }

func (e *observerConflictError) Error() string        { return e.msg }
func (e *observerConflictError) Is(target error) bool { return target == ErrObserver }

// Dispatcher is the cobra-free orchestration core for one CLI-command
// invocation (or, for the future Observer TUI, one long-lived read-only
// session). It owns the store handle, the resolved LLM role clients, the
// injected progress sink, and — in Owner mode — the advisory writer lock and
// its heartbeat goroutine.
type Dispatcher struct {
	cfg  config.Config
	sink progress.EventSink

	store *store.Store
	mode  Mode

	finder, verifier, cartographer, arbiter llm.Client

	// repo is the most recently opened target repository, retained so a
	// single verb call's internal helpers can share it without re-opening.
	// It is NOT preserved across separate verb calls with different targets.
	repo *ingest.Repo

	// scanRepro/scanReproAttempted are set by a Scan call with DoRepro=true
	// (nil otherwise) so a subsequent ReproCatchUp call can run the post-scan
	// catch-up drain the CLI triggers after rendering the scan result.
	scanRepro          *repro.Reproducer
	scanReproAttempted *sync.Map

	hbCancel context.CancelFunc
}

// Open loads no config itself (cfg is already resolved by the caller) but
// opens the store and determines Mode by probing the single-scan advisory
// lock (store.ActiveScanRuns, the same staleAfter window checkScanLock has
// always used). Owner mode additionally starts the heartbeat goroutine that
// keeps this process's scan_runs row fresh for the Dispatcher's lifetime.
// Observer mode opens the store read-only. The finder/verifier/cartographer/
// arbiter LLM clients are resolved lazily (see ensureRoleClients) by the
// verbs that actually need them — not every verb does (`bugbot repro` only
// ever needed the reproducer-role client), so resolving them here would
// newly require finder/verifier providers to be configured for callers that
// never touch them.
//
// sink is injected, not built here: CLI commands construct the presentation
// sink (pane/snapshot/log renderer) that suits their flags, and a future
// LiveFeed will plug in the same way.
func Open(ctx context.Context, cfg config.Config, sink progress.EventSink) (*Dispatcher, error) {
	st, mode, err := openStoreForMode(ctx, cfg)
	if err != nil {
		return nil, err
	}
	d := &Dispatcher{cfg: cfg, sink: sink, store: st, mode: mode}
	if mode == Owner {
		if err := d.initOwner(ctx); err != nil {
			_ = st.Close()
			return nil, err
		}
	}
	return d, nil
}

// openStoreForMode probes ActiveScanRuns (read-only, so the probe itself
// never contends for the writer lock) to decide whether this Dispatcher
// should be Owner or Observer, then opens the store accordingly.
func openStoreForMode(ctx context.Context, cfg config.Config) (*store.Store, Mode, error) {
	probe, err := store.OpenReadOnly(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, Owner, fmt.Errorf("open store: %w", err)
	}
	active, activeErr := probe.ActiveScanRuns(ctx, staleScanWindow)
	_ = probe.Close()
	if activeErr != nil {
		return nil, Owner, fmt.Errorf("scan lock check: %w", activeErr)
	}
	// Exclude our own pid, mirroring checkScanLock: a long-lived process that
	// already owns an active/heartbeating scan_runs row (e.g. a re-Opening
	// owner) must not misclassify itself as Observer.
	selfPID := os.Getpid()
	foreign := false
	for _, r := range active {
		if r.PID != selfPID {
			foreign = true
			break
		}
	}
	if !foreign {
		st, err := store.Open(ctx, cfg.Storage.Path)
		if err != nil {
			return nil, Owner, fmt.Errorf("open store: %w", err)
		}
		return st, Owner, nil
	}
	st, err := store.OpenReadOnly(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, Owner, fmt.Errorf("open store: %w", err)
	}
	return st, Observer, nil
}

// initOwner starts the heartbeat goroutine. It is called once from Open
// (fresh Owner) and again from escalateToOwner (Observer forced to Owner
// mid-dispatch).
func (d *Dispatcher) initOwner(ctx context.Context) error {
	hbCtx, cancel := context.WithCancel(ctx)
	d.hbCancel = cancel
	go runHeartbeat(hbCtx, d.store, os.Getpid())
	return nil
}

// ensureRoleClients lazily resolves the finder/verifier/cartographer/arbiter
// LLM clients on first use. Building them is deferred out of Open/initOwner
// (and out of ensureOwner's escalation path) because not every verb needs
// them: `bugbot repro` only ever needed the reproducer-role client (built
// separately by BuildReproducer/Repro), so eagerly resolving finder/verifier
// here would newly require finder/verifier providers to be configured for a
// repro-only invocation — a behavior regression relative to the pre-refactor
// CLI. Scan/Verify/Sweep/Review call this before they touch d.finder et al.
func (d *Dispatcher) ensureRoleClients(ctx context.Context) error {
	if d.finder != nil || d.verifier != nil || d.arbiter != nil {
		return nil
	}
	finder, verifier, cartographer, arbiter, err := BuildRoleClients(ctx, &d.cfg)
	if err != nil {
		return err
	}
	d.finder, d.verifier, d.cartographer, d.arbiter = finder, verifier, cartographer, arbiter
	return nil
}

// ensureOwner is the shared gate every dispatch verb calls first. In Owner
// mode it is a no-op. In Observer mode it runs the same heuristic
// checkScanLock has always run (fresh-heartbeat run belonging to another
// pid): with force=false a real conflict yields an *observerConflictError
// (errors.Is(err, ErrObserver) is true; Error() is checkScanLock's verbatim
// "another scan is already running ... pass --force" message); with
// force=true (or no real conflict found), the Dispatcher escalates to a
// writable store and Owner mode so the verb can proceed.
func (d *Dispatcher) ensureOwner(ctx context.Context, force bool) error {
	if d.mode == Owner {
		return nil
	}
	if err := checkScanLock(ctx, d.store, force, os.Getpid()); err != nil {
		return &observerConflictError{msg: err.Error()}
	}
	return d.escalateToOwner(ctx)
}

// escalateToOwner reopens the store writable and promotes the Dispatcher to
// Owner mode. Called only after ensureOwner has decided escalation is safe.
func (d *Dispatcher) escalateToOwner(ctx context.Context) error {
	_ = d.store.Close()
	st, err := store.Open(ctx, d.cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	d.store = st
	d.mode = Owner
	return d.initOwner(ctx)
}

// Mode reports the Dispatcher's current lock ownership.
func (d *Dispatcher) Mode() Mode { return d.mode }

// Config returns the resolved configuration the Dispatcher was opened with.
func (d *Dispatcher) Config() config.Config { return d.cfg }

// Close releases the heartbeat goroutine (Owner mode) and the store handle.
func (d *Dispatcher) Close() error {
	if d.hbCancel != nil {
		d.hbCancel()
	}
	if d.store == nil {
		return nil
	}
	return d.store.Close()
}

// openRepo opens the target repository and remembers it on the Dispatcher for
// the duration of the current verb call.
func (d *Dispatcher) openRepo(ctx context.Context, target string) (*ingest.Repo, error) {
	repo, err := ingest.Open(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("open target: %w", err)
	}
	d.repo = repo
	return repo, nil
}
