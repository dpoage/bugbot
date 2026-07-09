package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// fakeArbiterClient is a minimal offline llm.Client for Dispatcher.Reconcile
// tests: it always answers with the same canned dedup-arbiter verdict JSON
// regardless of the request, since ReconcileDedup's only LLM traffic here is
// dedup-arbiter calls through the Verifier role (funnel/reconcile.go's
// runDedupArbiter). No routing/matching logic is needed because each test
// arbitrates at most one distinct pair at a time.
type fakeArbiterClient struct{ body string }

func (c *fakeArbiterClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *fakeArbiterClient) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{
		Text:       c.body,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
	}, nil
}

// panicOnCompleteClient satisfies llm.Client for roles that must never be
// invoked in a given test (e.g. the Finder role, which funnel.New requires
// non-nil but ReconcileDedup never calls since it runs no finder agents) --
// a panic proves that invariant instead of silently masking a regression.
type panicOnCompleteClient struct{}

func (panicOnCompleteClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (panicOnCompleteClient) Complete(context.Context, llm.Request) (llm.Response, error) {
	panic("engine: this client role must not be invoked by Reconcile in this test")
}

func dedupVerdictJSON(verdict, reasoning string) string {
	return `{"verdict": "` + verdict + `", "reasoning": "` + reasoning + `"}`
}

// reconcileCloseDescA/B are a SimilarFinding-close pair (mirrors the fixture
// funnel/reconcile_test.go uses): worded almost identically so the
// deterministic pre-gate nominates them regardless of defect kind.
const (
	reconcileCloseDescA = "the config pointer may be nil and is dereferenced without a guard in Greeting"
	reconcileCloseDescB = "the config pointer may be nil and is dereferenced without any guard inside Greeting"
)

// seedReconcilePair opens cfg's store, seeds an older/newer OPEN finding pair
// at the same file within the merge window with compatible defect_kind and
// SimilarFinding-close descriptions (nominateReconcilePairs' exact
// deterministic gate), then closes the handle -- so it never contends with a
// subsequent Open()'s own writer-lock probe/acquisition.
func seedReconcilePair(t *testing.T, ctx context.Context, cfg config.Config, olderLine, newerLine int, suffix string) (older, newer domain.Finding) {
	t.Helper()
	st, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatalf("open store to seed: %v", err)
	}
	defer func() { _ = st.Close() }()

	mk := func(line int, title, desc string) domain.Finding {
		fp := domain.Fingerprint("nil-safety", "bug.go", fmt.Sprintf("%d|%s", line, title))
		f, err := st.UpsertFinding(ctx, domain.Finding{
			Fingerprint: fp,
			Title:       title,
			Description: desc,
			Severity:    "high",
			Tier:        2,
			Status:      domain.StatusOpen,
			Lens:        "nil-safety",
			File:        "bug.go",
			Line:        line,
			DefectKind:  domain.DefectNilDeref,
			Subject:     title,
		})
		if err != nil {
			t.Fatalf("seed finding %q: %v", title, err)
		}
		return f
	}
	older = mk(olderLine, "older nil deref "+suffix, reconcileCloseDescA)
	newer = mk(newerLine, "newer nil deref "+suffix, reconcileCloseDescB)
	return older, newer
}

// TestReconcile_ConfidentYesMerges is the CLI-facing acceptance case: a
// seeded duplicate pair judged "yes" by a scripted arbiter merges through
// Dispatcher.Reconcile exactly like it does through the daemon's
// runReconcile and funnel.ReconcileDedup directly, and the store reflects
// the newer row closed StatusSuperseded.
func TestReconcile_ConfidentYesMerges(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	older, newer := seedReconcilePair(t, ctx, cfg, 10, 12, "merge")

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	d.finder = panicOnCompleteClient{}
	d.verifier = &fakeArbiterClient{body: dedupVerdictJSON("yes", "same nil-deref defect, worded differently")}

	res, err := d.Reconcile(ctx, ReconcileOpts{Out: io.Discard})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if res.Result.Stats.ReconcileMerged != 1 {
		t.Fatalf("ReconcileMerged = %d, want 1", res.Result.Stats.ReconcileMerged)
	}
	if res.Result.Stats.ReconcileNominated != 1 {
		t.Fatalf("ReconcileNominated = %d, want 1", res.Result.Stats.ReconcileNominated)
	}

	got, err := d.store.GetFinding(ctx, newer.ID)
	if err != nil {
		t.Fatalf("GetFinding(newer): %v", err)
	}
	if got.Status != domain.StatusSuperseded {
		t.Errorf("newer finding Status = %v, want StatusSuperseded", got.Status)
	}
	stillOpen, err := d.store.GetFinding(ctx, older.ID)
	if err != nil {
		t.Fatalf("GetFinding(older): %v", err)
	}
	if stillOpen.Status != domain.StatusOpen {
		t.Errorf("older (canonical) finding Status = %v, want StatusOpen", stillOpen.Status)
	}
}

// TestReconcile_CapHonored proves --cap flows through: with two nominatable
// pairs and Cap=1, only one is arbitrated and the second is surfaced as
// skipped-cap rather than silently dropped or merged.
func TestReconcile_CapHonored(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	seedReconcilePair(t, ctx, cfg, 10, 12, "pair-a")
	seedReconcilePair(t, ctx, cfg, 100, 102, "pair-b")

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	d.finder = panicOnCompleteClient{}
	d.verifier = &fakeArbiterClient{body: dedupVerdictJSON("yes", "same defect")}

	res, err := d.Reconcile(ctx, ReconcileOpts{Cap: 1, Out: io.Discard})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	s := res.Result.Stats
	if s.ReconcileNominated != 2 {
		t.Fatalf("ReconcileNominated = %d, want 2", s.ReconcileNominated)
	}
	if s.ReconcileArbitrated != 1 {
		t.Fatalf("ReconcileArbitrated = %d, want 1 (cap=1)", s.ReconcileArbitrated)
	}
	if s.ReconcileSkippedCap != 1 {
		t.Fatalf("ReconcileSkippedCap = %d, want 1", s.ReconcileSkippedCap)
	}
}

// TestReconcile_EmptyBacklogFastPath proves the empty-nomination path prints
// the explicit "nothing to reconcile" line and opens no scan run (ScanRunID
// stays empty -- funnel.ReconcileDedup's fast path returns before
// BeginScanRun), and never touches the finder or verifier clients.
func TestReconcile_EmptyBacklogFastPath(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	d, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	d.finder = panicOnCompleteClient{}
	d.verifier = panicOnCompleteClient{}

	var out strings.Builder
	res, err := d.Reconcile(ctx, ReconcileOpts{Out: &out})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if res.Result.ScanRunID != "" {
		t.Errorf("ScanRunID = %q, want empty (no scan run opened on the empty-nomination fast path)", res.Result.ScanRunID)
	}
	if !strings.Contains(out.String(), "no duplicate candidates nominated") {
		t.Errorf("output = %q, want an explicit nothing-to-reconcile line", out.String())
	}
}

// TestReconcile_WriterLockHeldSurfacesErrLocked proves the store-locked case
// (a running daemon holding the writer flock) fails fast with a wrapped
// *store.ErrLocked rather than hanging or silently proceeding read-only.
// With no foreign scan_runs row to detect (an idle daemon between cycles),
// engine.Open's own openStoreForMode hits the held flock directly -- the CLI
// command maps this to an actionable message (see actionableLockError).
func TestReconcile_WriterLockHeldSurfacesErrLocked(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	// Hold the writer lock for the whole test, exactly like a running daemon
	// would: a live *store.Store with the on-disk flock held, and no
	// scan_runs row (an idle daemon between cycles). With no foreign active
	// scan run to detect, openStoreForMode's own store.Open attempt is what
	// hits the flock -- so Open() itself fails fast here, before Dispatcher.
	// Reconcile is ever reached. That is the actionable failure the CLI
	// command must map to a helpful message (see actionableLockError).
	holder, err := store.Open(ctx, cfg.Storage.Path)
	if err != nil {
		t.Fatalf("seed writer lock: %v", err)
	}
	defer func() { _ = holder.Close() }()

	_, err = Open(ctx, cfg, nil)
	if err == nil {
		t.Fatal("Open() while the writer lock is held error = nil, want *store.ErrLocked")
	}
	var locked *store.ErrLocked
	if !errors.As(err, &locked) {
		t.Fatalf("Open() error = %v (%T), want *store.ErrLocked", err, err)
	}
}
