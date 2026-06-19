package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// openMultiPkgFixture builds a committed git repo with one single-file package
// per name in pkgs and returns the store, repo, and the sorted target paths.
func openMultiPkgFixture(t *testing.T, pkgs ...string) (*store.Store, *ingest.Repo, []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repoDir := t.TempDir()
	var targets []string
	for _, p := range pkgs {
		rel := p + "/" + p + ".go"
		abs := filepath.Join(repoDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("package "+p+"\n\nfunc F() int { return 1 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, rel)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", ".")
	runGit("commit", "-q", "-m", "seed")

	repo, err := ingest.Open(ctx, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return st, repo, targets
}

// snapAndFps is a small helper: snapshot + fingerprints for a cartograph call.
func snapAndFps(t *testing.T, repo *ingest.Repo) (*ingest.Snapshot, map[string]string) {
	t.Helper()
	ctx := context.Background()
	snap, err := repo.Snapshot(ctx, ingest.ScanFilter{})
	if err != nil {
		t.Fatal(err)
	}
	fps, err := repo.Fingerprints(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	return snap, fps
}

// countPersisted returns how many of pkgs have a non-empty stored summary.
func countPersisted(t *testing.T, st *store.Store, pkgs ...string) int {
	t.Helper()
	rows, err := st.GetPackageSummaries(context.Background(), pkgs)
	if err != nil {
		t.Fatalf("GetPackageSummaries: %v", err)
	}
	n := 0
	for _, p := range pkgs {
		if row, ok := rows[p]; ok && row.Summary != "" {
			n++
		}
	}
	return n
}

// cancelAfterFirstClient returns a real summary for its FIRST completion and
// cancels the run before the second can start. It proves the on-the-fly persist
// contract: the first package's summary must already be in the store even though
// the pass is then interrupted. With the old end-of-pass batch upsert (run with
// the now-cancelled ctx) zero summaries would survive.
type cancelAfterFirstClient struct {
	mu     sync.Mutex
	calls  int
	cancel context.CancelFunc
}

func (c *cancelAfterFirstClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *cancelAfterFirstClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n > 1 {
		return llm.Response{}, context.Canceled
	}
	resp := llm.Response{
		Text:       `{"summary":"summary for first package"}`,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
	// Cancel AFTER preparing the response so the first summary is fully produced;
	// the on-the-fly persist uses a cancel-detached context and must still save it.
	c.cancel()
	return resp, nil
}

// TestCartographer_PersistsOnTheFlyAcrossInterruption is the core regression for
// the persist-on-the-fly fix: an interrupted pass keeps the summaries already
// produced instead of discarding the whole run's work.
func TestCartographer_PersistsOnTheFlyAcrossInterruption(t *testing.T) {
	st, repo, targets := openMultiPkgFixture(t, "alpha", "bravo", "charlie")
	snap, fps := snapAndFps(t, repo)

	// MaxParallel=1 makes the interruption deterministic: alpha is summarized
	// and persisted, its completion cancels the run, and bravo/charlie are
	// skipped before they summarize.
	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		Cartographer: true,
		MaxParallel:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &cancelAfterFirstClient{cancel: cancel}

	cart := f.cartograph(ctx, &Result{}, client, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("cartograph returned nil with Cartographer=true")
	}

	// Exactly one summary persisted — proving the first was written before the
	// interruption (old code: the end-of-pass batch upsert with a cancelled ctx
	// would have persisted zero).
	if got := countPersisted(t, st, "alpha", "bravo", "charlie"); got != 1 {
		t.Errorf("persisted summaries after interruption = %d, want 1 (on-the-fly)", got)
	}
	if cart.summaries["alpha"] == "" {
		t.Errorf("alpha summary missing from cartography; on-the-fly persist should have recorded it")
	}
}

// TestCartographer_FullPassPersistsAll pins that a normal (uninterrupted)
// concurrent pass persists every uncached package and populates the cartography.
func TestCartographer_FullPassPersistsAll(t *testing.T) {
	st, repo, targets := openMultiPkgFixture(t, "alpha", "bravo", "charlie")
	snap, fps := snapAndFps(t, repo)

	client := newScriptedClient()
	client.fallback = `{"summary":"package summary"}` // valid summary JSON for every package
	f, err := New(RoleClients{Finder: client, Verifier: newScriptedClient()}, st, repo, Options{
		Cartographer: true,
		MaxParallel:  4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	cart := f.cartograph(context.Background(), &Result{}, client, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("cartograph returned nil with Cartographer=true")
	}
	if len(cart.summaries) != 3 {
		t.Errorf("cartography summaries = %d, want 3", len(cart.summaries))
	}
	if got := countPersisted(t, st, "alpha", "bravo", "charlie"); got != 3 {
		t.Errorf("persisted summaries = %d, want 3 (full pass)", got)
	}
}

// barrierClient blocks each completion until `target` of them are concurrently
// in flight, then releases all. A sequential cartographer can never reach
// target>1, so it would block until the test's context deadline — turning a
// regression to sequential generation into a visible failure.
type barrierClient struct {
	mu      sync.Mutex
	active  int
	target  int
	once    sync.Once
	reached chan struct{}
}

func (c *barrierClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *barrierClient) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	c.mu.Lock()
	c.active++
	if c.active >= c.target {
		c.once.Do(func() { close(c.reached) })
	}
	c.mu.Unlock()
	select {
	case <-c.reached:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	return llm.Response{
		Text:       `{"summary":"concurrent summary"}`,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

// TestCartographer_GeneratesConcurrently proves the parallelism: three packages
// must be summarized concurrently (barrier of 3). Under a sequential regression
// only one completion is ever in flight, the barrier never trips, and the
// context deadline fires — leaving fewer than three summaries persisted.
func TestCartographer_GeneratesConcurrently(t *testing.T) {
	st, repo, targets := openMultiPkgFixture(t, "alpha", "bravo", "charlie")
	snap, fps := snapAndFps(t, repo)

	f, err := New(RoleClients{Finder: newScriptedClient(), Verifier: newScriptedClient()}, st, repo, Options{
		Cartographer: true,
		MaxParallel:  4, // >= 3 so all three can run at once
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	client := &barrierClient{target: 3, reached: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cart := f.cartograph(ctx, &Result{}, client, snap, targets, fps, nil)
	if cart == nil {
		t.Fatal("cartograph returned nil with Cartographer=true")
	}
	if len(cart.summaries) != 3 {
		t.Errorf("cartography summaries = %d, want 3 (all generated concurrently)", len(cart.summaries))
	}
	if got := countPersisted(t, st, "alpha", "bravo", "charlie"); got != 3 {
		t.Errorf("persisted summaries = %d, want 3 (concurrent pass)", got)
	}
}
