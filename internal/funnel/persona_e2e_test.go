package funnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// pythonBugSrc is a tiny Python source whose only role is to make the snapshot's
// dominant language Python, so the funnel seeds a Python persona. The "bug" is
// scripted, not actually detected by reading the file.
const pythonBugSrc = `def greeting(cfg):
    # cfg may be None; attribute access then raises AttributeError.
    return "hello " + cfg.name
`

// newPythonFixtureRepo creates a real git repo whose tracked source is a single
// Python file, so ingest classifies the snapshot as Python-dominant.
func newPythonFixtureRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "greet.py")
	if err := os.WriteFile(p, []byte(pythonBugSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
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
	return dir
}

// personaCapturingClient wraps a scriptedClient and records every system prompt
// it is asked to complete, so a test can assert what persona the funnel seeded.
type personaCapturingClient struct {
	*scriptedClient
	mu      sync.Mutex
	systems []string
}

func newPersonaCapturingClient(inner *scriptedClient) *personaCapturingClient {
	return &personaCapturingClient{scriptedClient: inner}
}

func (c *personaCapturingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.mu.Lock()
	c.systems = append(c.systems, req.System)
	c.mu.Unlock()
	return c.scriptedClient.Complete(ctx, req)
}

func (c *personaCapturingClient) seenSystems() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.systems))
	copy(out, c.systems)
	return out
}

// TestSweep_PythonRepo_AdaptedPersona is the non-Go acceptance fixture: over a
// Python-dominant snapshot, the finder and verifier system prompts must carry
// the Python persona and must NOT carry the hardcoded "Go engineer" framing.
func TestSweep_PythonRepo_AdaptedPersona(t *testing.T) {
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo, err := ingest.Open(ctx, newPythonFixtureRepo(t))
	if err != nil {
		t.Fatal(err)
	}

	const pyCand = `{"file": "greet.py", "line": 3, "title": "None deref of cfg in greeting",
		"description": "cfg may be None", "severity": "high",
		"evidence": "greeting returns cfg.name without a None check", "confidence": "high",
		"defect_kind": "nil-deref", "subject": "greeting"}`

	finderInner := newScriptedClient().onSystemContains("nil-safety/error-handling", candJSON(pyCand))
	verifierInner := newScriptedClient()
	verifierInner.onTaskContains("None deref of cfg in greeting", notRefutedJSON)

	finder := newPersonaCapturingClient(finderInner)
	verifier := newPersonaCapturingClient(verifierInner)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	assertPythonPersona := func(role string, systems []string) {
		if len(systems) == 0 {
			t.Fatalf("%s: no system prompts captured", role)
		}
		for i, s := range systems {
			if !strings.Contains(s, "Python") {
				t.Errorf("%s prompt %d missing Python persona:\n%.120q", role, i, s)
			}
			if strings.Contains(s, "Go engineer") {
				t.Errorf("%s prompt %d still uses hardcoded 'Go engineer':\n%.120q", role, i, s)
			}
		}
	}

	assertPythonPersona("finder", finder.seenSystems())
	assertPythonPersona("verifier", verifier.seenSystems())
}
