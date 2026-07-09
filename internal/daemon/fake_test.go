package daemon

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

// ---------------------------------------------------------------------------
// Fake clock: drives the scheduler deterministically without real wall-time.
//
// The scheduler calls newTimer(d) once per loop iteration and waits on the
// returned channel. The fake records each timer as pending. A test advances the
// loop by exactly one cycle with fire(): it releases the single in-flight timer
// (advancing virtual now by that timer's duration). Because the scheduler only
// ever has one timer outstanding at a time, fire() == "run one cycle".
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu      sync.Mutex
	cur     time.Time
	pending chan pendingTimer // buffered; one slot is plenty for the single-timer loop
}

type pendingTimer struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{cur: start, pending: make(chan pendingTimer, 1)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	c.pending <- pendingTimer{d: d, ch: ch}
	return ch, func() {
		// stop: drain the registration if the scheduler abandoned this timer (it
		// chose ctx.Done()). Non-blocking so a fired timer is a no-op to stop.
		select {
		case <-c.pending:
		default:
		}
	}
}

// fire releases the next outstanding timer, advancing virtual now by its
// duration and waking the scheduler. It blocks until a timer is registered, so
// the test stays in lockstep with the loop. Returns false if ctx is done first.
func (c *fakeClock) fire(ctx context.Context, t *testing.T) bool {
	t.Helper()
	select {
	case pt := <-c.pending:
		c.mu.Lock()
		c.cur = c.cur.Add(pt.d)
		c.mu.Unlock()
		pt.ch <- c.now()
		return true
	case <-ctx.Done():
		return false
	case <-time.After(2 * time.Second):
		t.Fatal("fakeClock.fire: no timer registered within 2s (scheduler stuck?)")
		return false
	}
}

// ---------------------------------------------------------------------------
// Fake LLM client: a concurrency-safe scripted finder/verifier, modeled on the
// funnel's own scriptedClient. It serves a single hard-coded candidate to finder
// agents and a "could not refute" verdict to refuters, so the funnel produces
// exactly one verified Tier-2 finding per scan against a fixture file.
// ---------------------------------------------------------------------------

type fakeLLM struct {
	mu    sync.Mutex
	calls int

	// finderBody / refuterBody are the JSON responses for finder and refuter
	// requests respectively. Selected by inspecting the system prompt.
	finderBody  string
	refuterBody string
	// dedupBody, when non-empty, is the JSON response served to the ezmx.2/
	// ezmx.4 dedup arbiter's fixed system prompt (routed on a stable
	// substring of it, "SAME underlying defect"). Empty means the arbiter
	// falls through to finderBody, matching the pre-ezmx.4 default: no test
	// that leaves this unset changes behavior.
	dedupBody string
}

func newFakeLLM(finderBody, refuterBody string) *fakeLLM {
	return &fakeLLM{finderBody: finderBody, refuterBody: refuterBody}
}

func (c *fakeLLM) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *fakeLLM) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	// The dedup arbiter's fixed system prompt is checked first (its text
	// contains neither "refute" nor "WRONG"). Refuters get the skeptical
	// "PROVE the bug is WRONG" system prompt; finders get a lens system
	// prompt. Route on a stable substring of each.
	body := c.finderBody
	switch {
	case c.dedupBody != "" && strings.Contains(req.System, "SAME underlying defect"):
		body = c.dedupBody
	case strings.Contains(req.System, "refute") || strings.Contains(req.System, "WRONG"):
		body = c.refuterBody
	}
	return llm.Response{
		Text:       body,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
	}, nil
}

// dedupVerdictJSON builds a canned dedup-arbiter response body, mirroring
// funnel's own (package-private) helper of the same name.
func dedupVerdictJSON(verdict, reasoning string) string {
	return `{"verdict": "` + verdict + `", "reasoning": "` + reasoning + `"}`
}

// candidate / verdict JSON bodies shared by tests. The candidate points at
// fixtureFile:fixtureLine so re-verification reads a real on-disk file.
const (
	fixtureFile = "bug.go"
	fixtureLine = 3

	candidateJSON = `{"candidates":[{"file":"bug.go","line":3,"title":"possible nil dereference","description":"x may be nil here","severity":"high","evidence":"no guard before use","confidence":"high","defect_kind":"nil-deref","subject":"f"}]}`
	emptyJSON     = `{"candidates":[]}`

	notRefutedJSON = `{"refuted":false,"reasoning":"the nil path is reachable and unguarded","confidence":"high"}`
	refutedJSON    = `{"refuted":true,"reasoning":"the caller guards this with a nil check","confidence":"high"}`
)

// ---------------------------------------------------------------------------
// Git fixture, mirroring internal/ingest's test helper.
// ---------------------------------------------------------------------------

type fixtureRepo struct {
	t   *testing.T
	dir string
}

func newFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	r := &fixtureRepo{t: t, dir: dir}
	r.git("init", "-b", "main")
	return r
}

func (r *fixtureRepo) git(args ...string) string {
	r.t.Helper()
	full := append([]string{
		"-C", r.dir,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"HOME="+r.dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (r *fixtureRepo) write(rel, content string) {
	r.t.Helper()
	abs := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *fixtureRepo) remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, filepath.FromSlash(rel))); err != nil {
		r.t.Fatal(err)
	}
}

func (r *fixtureRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-m", msg)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

func (r *fixtureRepo) open() *ingest.Repo {
	r.t.Helper()
	repo, err := ingest.Open(context.Background(), r.dir)
	if err != nil {
		r.t.Fatalf("ingest.Open: %v", err)
	}
	return repo
}

// openStore opens a temp-file store for a test. A file (not ":memory:") is used
// deliberately: the store shares one *sql.DB across a connection pool, and a
// ":memory:" database is private per connection, so pooled queries would hit
// separate empty databases. A temp file in t.TempDir() is removed by the test
// harness on cleanup.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
