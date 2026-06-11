package funnel

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// TestDiffIntentLens_InBuiltins verifies that diff-intent appears in
// BuiltinLenses with yield 95, a non-empty Specialization, and the correct
// position in yield-descending order (after nil-safety/100, before concurrency/90).
func TestDiffIntentLens_InBuiltins(t *testing.T) {
	lenses := BuiltinLenses()

	var foundIdx = -1
	for i := range lenses {
		if lenses[i].Name == "diff-intent" {
			foundIdx = i
			break
		}
	}
	if foundIdx < 0 {
		t.Fatal("diff-intent not found in BuiltinLenses()")
	}
	l := lenses[foundIdx]
	if l.Yield != 95 {
		t.Errorf("diff-intent yield = %d, want 95", l.Yield)
	}
	if l.Specialization == "" {
		t.Error("diff-intent Specialization is empty")
	}
	// Must follow a higher-yield lens.
	if foundIdx == 0 {
		t.Error("diff-intent should not be first; nil-safety/error-handling (100) must precede it")
	}
	if lenses[foundIdx-1].Yield < 95 {
		t.Errorf("lens before diff-intent has yield %d; diff-intent must follow higher-yield lenses", lenses[foundIdx-1].Yield)
	}
	// Must precede a lower-yield lens (if one exists).
	if foundIdx+1 < len(lenses) && lenses[foundIdx+1].Yield > 95 {
		t.Errorf("lens after diff-intent has yield %d; diff-intent must precede lower-yield lenses", lenses[foundIdx+1].Yield)
	}
}

// TestDiffIntentTask_Truncation verifies that buildDiffIntentTask truncates
// diffs larger than diffIntentDiffCap and appends the exact marker text, while
// smaller diffs are included in full without a marker.
func TestDiffIntentTask_Truncation(t *testing.T) {
	// Build a diff just over the cap to force truncation.
	bigDiff := make([]byte, diffIntentDiffCap+100)
	for i := range bigDiff {
		bigDiff[i] = 'x'
	}
	cc := &ChangeContext{
		FromCommit: "abc",
		ToCommit:   "def",
		Message:    "fix the thing",
		Diff:       bigDiff,
		BlastFiles: []string{"a.go"},
	}
	task := buildDiffIntentTask(cc)
	if !strings.Contains(task, "[diff truncated at 48KB]") {
		t.Error("large diff should carry truncation marker")
	}

	// Small diff: no truncation marker, full content included.
	small := []byte("--- a\n+++ b\n@@ -1 +1 @@ val()\n-validate()\n+noop()\n")
	cc2 := &ChangeContext{
		Message: "remove validation",
		Diff:    small,
	}
	task2 := buildDiffIntentTask(cc2)
	if strings.Contains(task2, "[diff truncated") {
		t.Error("small diff should not be truncated")
	}
	if !strings.Contains(task2, "validate()") {
		t.Error("small diff content should appear verbatim in task")
	}
}

// TestDiffIntentTask_EmbedsMsgAndBlast confirms that the commit message and
// blast-radius file list both appear in the built task.
func TestDiffIntentTask_EmbedsMsgAndBlast(t *testing.T) {
	cc := &ChangeContext{
		FromCommit: "a",
		ToCommit:   "b",
		Message:    "validate input before calling downstream",
		Diff:       []byte("diff output"),
		BlastFiles: []string{"pkg/service.go", "pkg/handler.go"},
	}
	task := buildDiffIntentTask(cc)
	if !strings.Contains(task, "validate input before calling downstream") {
		t.Errorf("task missing commit message: %q", task)
	}
	if !strings.Contains(task, "pkg/service.go") {
		t.Errorf("task missing blast file: %q", task)
	}
	if !strings.Contains(task, "pkg/handler.go") {
		t.Errorf("task missing blast file: %q", task)
	}
}

// TestDiffIntentTask_NilDiffHandled confirms buildDiffIntentTask does not
// panic and includes a "(not available)" notice when Diff is nil.
func TestDiffIntentTask_NilDiffHandled(t *testing.T) {
	cc := &ChangeContext{
		Message: "add something",
		Diff:    nil,
	}
	task := buildDiffIntentTask(cc)
	if !strings.Contains(task, "(not available)") {
		t.Errorf("nil diff should produce '(not available)' in task: %q", task)
	}
}

// TestBuildDiffIntentTask_48KBCapExact confirms the cap constant value.
func TestBuildDiffIntentTask_48KBCapExact(t *testing.T) {
	if diffIntentDiffCap != 48*1024 {
		t.Errorf("diffIntentDiffCap = %d, want %d", diffIntentDiffCap, 48*1024)
	}
}

// TestIsCommitKind covers the isCommitKind predicate for all ScanKind values.
func TestIsCommitKind(t *testing.T) {
	cases := []struct {
		kind store.ScanKind
		want bool
	}{
		{store.ScanTargeted, true},
		{store.ScanOneshot, true},
		{store.ScanSweep, false},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := isCommitKind(tc.kind); got != tc.want {
			t.Errorf("isCommitKind(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// TestHypothesize_DiffIntentZeroOnSweep asserts that diff-intent emits zero
// tasks on a sweep (no ChangeContext). The taxonomy lenses emit one task each
// per chunk; diff-intent contributes nothing additional.
// Concretely: a single-file repo with 1 chunk should produce nLenses-1 finder
// calls (all taxonomy lenses, no diff-intent).
func TestHypothesize_DiffIntentZeroOnSweep(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// diff-intent emits zero chunk tasks, so total finder calls == nLenses-1.
	nTaxonomy := len(BuiltinLenses()) - 1
	if finder.callCount() != nTaxonomy {
		t.Errorf("sweep finder calls = %d, want %d (no diff-intent task on sweep)",
			finder.callCount(), nTaxonomy)
	}
}

// TestHypothesize_DiffIntentNilCC asserts that diff-intent emits zero tasks
// when ChangeContext is nil, even on a Targeted run.
func TestHypothesize_DiffIntentNilCC(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	finder := newScriptedClient()
	verifier := newScriptedClient()

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel:   1,
		ChangeContext: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Targeted(ctx, []string{"bug.go"})
	if err != nil {
		t.Fatal(err)
	}

	nTaxonomy := len(BuiltinLenses()) - 1
	if finder.callCount() != nTaxonomy {
		t.Errorf("targeted (nil CC) finder calls = %d, want %d (no diff-intent)",
			finder.callCount(), nTaxonomy)
	}
}

// TestHypothesize_DiffIntentOneTaskOnTargeted asserts that exactly one
// diff-intent finder task fires when ChangeContext is non-nil on a Targeted run,
// and that the resulting candidate flows through the pipeline to a persisted finding.
func TestHypothesize_DiffIntentOneTaskOnTargeted(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	diffIntentCand := `{"file": "bug.go", "line": 5, "title": "intent gap: validation removed",
		"description": "diff removes the validate() call the message claims to add",
		"severity": "high", "evidence": "line 5 shows removal", "confidence": "high"}`

	finder := newScriptedClient().onSystemContains("diff-intent", candJSON(diffIntentCand))
	verifier := newScriptedClient().onTaskContains("intent gap", notRefutedJSON)

	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		MaxParallel: 1,
		ChangeContext: &ChangeContext{
			FromCommit: "abc",
			ToCommit:   "def",
			Message:    "add validation before use",
			Diff:       []byte("--- a/bug.go\n+++ b/bug.go\n@@ -1 +1 @@\n-validate(x)\n+noop()\n"),
			BlastFiles: []string{"bug.go"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.Targeted(ctx, []string{"bug.go"})
	if err != nil {
		t.Fatal(err)
	}

	if res.Stats.Hypothesized == 0 {
		t.Error("expected at least one candidate from diff-intent lens on targeted scan with ChangeContext")
	}

	// The diff-intent candidate must survive triage and verification.
	found := false
	for _, fnd := range res.Findings {
		if fnd.Lens == "diff-intent" {
			found = true
		}
	}
	if !found {
		t.Errorf("diff-intent finding not in results; findings=%+v stats=%+v", res.Findings, res.Stats)
	}

	// Total finder calls: nTaxonomy + 1 diff-intent task.
	nTaxonomy := len(BuiltinLenses()) - 1
	wantCalls := nTaxonomy + 1
	if finder.callCount() != wantCalls {
		t.Errorf("finder calls = %d, want %d (nTaxonomy=%d + 1 diff-intent)",
			finder.callCount(), wantCalls, nTaxonomy)
	}
}

// TestHypothesize_DiffIntentTaskContent confirms the task sent to the
// diff-intent finder embeds the commit message and, when the diff exceeds the
// cap, the truncation marker. Uses taskRecordingClient to capture requests.
func TestHypothesize_DiffIntentTaskContent(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)

	bigDiff := make([]byte, diffIntentDiffCap+500)
	for i := range bigDiff {
		bigDiff[i] = '-'
	}

	rec := newTaskRecordingClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: rec, Verifier: verifier}, st, repo, Options{
		MaxParallel: 1,
		ChangeContext: &ChangeContext{
			FromCommit: "a",
			ToCommit:   "b",
			Message:    "sentinel-message-12345",
			Diff:       bigDiff,
			BlastFiles: []string{"bug.go"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Targeted(ctx, []string{"bug.go"})
	if err != nil {
		t.Fatal(err)
	}

	// Find the diff-intent task among all captured user messages.
	var diffIntentTask string
	for _, task := range rec.userMessages() {
		if strings.Contains(task, "sentinel-message-12345") {
			diffIntentTask = task
			break
		}
	}
	if diffIntentTask == "" {
		t.Fatalf("no task contained the sentinel commit message; tasks=%v", rec.userMessages())
	}
	if !strings.Contains(diffIntentTask, "[diff truncated at 48KB]") {
		t.Error("large diff should carry truncation marker in task")
	}
	if !strings.Contains(diffIntentTask, "bug.go") {
		t.Error("blast file should appear in diff-intent task")
	}
}

// --- task recording client --------------------------------------------------

// taskRecordingClient wraps a scriptedClient and records the first user
// message in every Complete call, for asserting on task content.
type taskRecordingClient struct {
	mu   sync.Mutex
	msgs []string
}

func newTaskRecordingClient() *taskRecordingClient { return &taskRecordingClient{} }

func (c *taskRecordingClient) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (c *taskRecordingClient) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	// Capture the first user message so tests can assert on task content.
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			c.mu.Lock()
			c.msgs = append(c.msgs, m.Content)
			c.mu.Unlock()
			break
		}
	}
	return llm.Response{
		Text:       emptyCandidates,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 50},
	}, nil
}

func (c *taskRecordingClient) userMessages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.msgs))
	copy(out, c.msgs)
	return out
}
