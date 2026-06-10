package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// ---- planPublish unit tests (pure; no gh calls) ----

// makeOpenFinding builds a minimal open finding for plan tests.
func makeOpenFinding(fp string, tier int, updatedAt time.Time) store.Finding {
	return store.Finding{
		ID:          fp,
		Fingerprint: fp,
		Title:       "title for " + fp,
		Tier:        tier,
		Status:      store.StatusOpen,
		File:        "x.go",
		Line:        1,
		UpdatedAt:   updatedAt,
	}
}

func makeFixedFinding(fp string) store.Finding {
	f := makeOpenFinding(fp, 2, time.Now())
	f.Status = store.StatusFixed
	return f
}

func makeDismissedFinding(fp string) store.Finding {
	f := makeOpenFinding(fp, 2, time.Now())
	f.Status = store.StatusDismissed
	return f
}

func makePublishedIssue(fp string, issueNumber int, state string, updatedAt time.Time) store.PublishedIssue {
	return store.PublishedIssue{
		Fingerprint: fp,
		IssueNumber: issueNumber,
		State:       state,
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
	}
}

// TestPlanPublish_Create: an open finding with no published row -> create.
func TestPlanPublish_Create(t *testing.T) {
	now := time.Now()
	open := []store.Finding{makeOpenFinding("fp1", 2, now)}
	actions := planPublish(open, nil, nil, nil, 2, true)

	if len(actions) != 1 || actions[0].op != publishOpCreate {
		t.Errorf("expected 1 create action, got %+v", actions)
	}
}

// TestPlanPublish_Skip: open finding with published row and UpdatedAt <= published.UpdatedAt -> skip.
func TestPlanPublish_Skip(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	open := []store.Finding{makeOpenFinding("fp1", 2, t0)}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", 10, "open", t0.Add(time.Second)),
	}
	actions := planPublish(open, nil, nil, published, 2, true)

	if len(actions) != 1 || actions[0].op != publishOpSkip {
		t.Errorf("expected 1 skip action, got %+v", actions)
	}
}

// TestPlanPublish_Update: open finding with published row but UpdatedAt newer -> update.
func TestPlanPublish_Update(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	t1 := t0.Add(2 * time.Hour) // finding updated after published.UpdatedAt
	open := []store.Finding{makeOpenFinding("fp1", 2, t1)}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", 10, "open", t0),
	}
	actions := planPublish(open, nil, nil, published, 2, true)

	if len(actions) != 1 || actions[0].op != publishOpUpdate {
		t.Errorf("expected 1 update action, got %+v", actions)
	}
	if actions[0].issueNumber != 10 {
		t.Errorf("issueNumber = %d, want 10", actions[0].issueNumber)
	}
}

// TestPlanPublish_TierFiltering: T3 finding excluded at tierMin=2; T0,T1,T2 included.
func TestPlanPublish_TierFiltering(t *testing.T) {
	now := time.Now()
	open := []store.Finding{
		makeOpenFinding("fp0", 0, now),
		makeOpenFinding("fp1", 1, now),
		makeOpenFinding("fp2", 2, now),
		makeOpenFinding("fp3", 3, now), // should be excluded
	}
	actions := planPublish(open, nil, nil, nil, 2, true)

	// Expect 3 creates (T0, T1, T2), not T3.
	creates := 0
	for _, a := range actions {
		if a.op == publishOpCreate {
			creates++
		}
		if a.op == publishOpCreate && a.finding.Tier == 3 {
			t.Error("T3 finding should not be created at tierMin=2")
		}
	}
	if creates != 3 {
		t.Errorf("expected 3 creates (T0/T1/T2), got %d", creates)
	}
}

// TestPlanPublish_Close: fixed finding with open published row -> close.
func TestPlanPublish_Close(t *testing.T) {
	fixed := []store.Finding{makeFixedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", 5, "open", time.Now()),
	}
	actions := planPublish(nil, fixed, nil, published, 2, true)

	if len(actions) != 1 || actions[0].op != publishOpClose {
		t.Errorf("expected 1 close action, got %+v", actions)
	}
	if actions[0].issueNumber != 5 {
		t.Errorf("issueNumber = %d, want 5", actions[0].issueNumber)
	}
}

// TestPlanPublish_DismissedClose: dismissed finding with open row and
// close_on_fixed=true -> close.
func TestPlanPublish_DismissedClose(t *testing.T) {
	dismissed := []store.Finding{makeDismissedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", 7, "open", time.Now()),
	}
	actions := planPublish(nil, nil, dismissed, published, 2, true)

	if len(actions) != 1 || actions[0].op != publishOpClose {
		t.Errorf("expected 1 close for dismissed with close_on_fixed=true, got %+v", actions)
	}
}

// TestPlanPublish_CloseOnFixedFalse: fixed finding -> NO close action when close_on_fixed=false.
func TestPlanPublish_CloseOnFixedFalse(t *testing.T) {
	fixed := []store.Finding{makeFixedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", 5, "open", time.Now()),
	}
	actions := planPublish(nil, fixed, nil, published, 2, false /* close_on_fixed=false */)

	for _, a := range actions {
		if a.op == publishOpClose {
			t.Errorf("close action should not be planned when close_on_fixed=false")
		}
	}
}

// TestPlanPublish_AlreadyClosed: fixed finding with already-closed published row -> skip (no action).
func TestPlanPublish_AlreadyClosed(t *testing.T) {
	fixed := []store.Finding{makeFixedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", 5, "closed", time.Now()),
	}
	actions := planPublish(nil, fixed, nil, published, 2, true)

	for _, a := range actions {
		if a.op == publishOpClose {
			t.Errorf("should not close an already-closed issue")
		}
	}
}

// ---- applyPublish with fakeGH ----

// setupPublishStore opens a fresh store, seeds one open T2 finding, and
// returns the store, the finding, and the DB path for re-open.
func setupPublishStore(t *testing.T) (*store.Store, store.Finding) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := store.Finding{
		Fingerprint: store.Fingerprint("race", "x.go", 7, "boom"),
		Title:       "boom",
		Description: "desc",
		Reasoning:   "trace",
		Severity:    "high",
		Tier:        2,
		Status:      store.StatusOpen,
		Lens:        "race",
		File:        "x.go",
		Line:        7,
		CommitSHA:   "c1abc",
	}
	f, err = st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	return st, f
}

// TestApplyPublish_Create: fakeGH records a create call, number is parsed and
// persisted, body starts with fingerprint marker.
func TestApplyPublish_Create(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	gh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":42}`))

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	// Verify the create call was made.
	createCalls := gh.callsContaining("repos/{owner}/{repo}/issues -X POST")
	if len(createCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d; all calls: %v", len(createCalls), gh.calls)
	}
	// Title must match.
	if title, ok := argValue(createCalls[0], "title"); !ok || title != f.Title {
		t.Errorf("title = %q, want %q", title, f.Title)
	}
	// Body must start with the fingerprint marker.
	body, _ := argValue(createCalls[0], "body")
	wantMarker := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("body does not start with marker %q\nbody = %q", wantMarker, body[:min(len(body), 80)])
	}
	// Label must be passed.
	labelFound := false
	for _, arg := range createCalls[0] {
		if arg == "labels[]=bugbot" {
			labelFound = true
		}
	}
	if !labelFound {
		t.Errorf("label 'bugbot' not found in create call args: %v", createCalls[0])
	}

	// The published_issues row must be written.
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.IssueNumber != 42 {
		t.Errorf("issue_number = %d, want 42", pi.IssueNumber)
	}
	if pi.State != "open" {
		t.Errorf("state = %q, want open", pi.State)
	}
}

// TestApplyPublish_Close: a fixed finding with an open published row triggers
// the comment POST then the PATCH state=closed.
func TestApplyPublish_Close(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	// Pre-record the published issue.
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, 77, "open"); err != nil {
		t.Fatalf("seed published: %v", err)
	}
	// Mark the finding fixed.
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}

	gh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("issues/77/comments", []byte(`{"id":1}`)).
		on("issues/77 -X PATCH", []byte(`{"number":77}`))

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	// Should have posted the comment.
	commentCalls := gh.callsContaining("issues/77/comments")
	if len(commentCalls) != 1 {
		t.Errorf("expected 1 comment call, got %d", len(commentCalls))
	}
	// Then patched state=closed.
	patchCalls := gh.callsContaining("issues/77 -X PATCH")
	if len(patchCalls) != 1 {
		t.Errorf("expected 1 patch call, got %d", len(patchCalls))
	}
	state, ok := argValue(patchCalls[0], "state")
	if !ok || state != "closed" {
		t.Errorf("PATCH state = %q, want closed", state)
	}

	// published_issues row must be updated to closed.
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.State != "closed" {
		t.Errorf("published state = %q, want closed", pi.State)
	}
}

// TestApplyPublish_DryRun: dry-run makes zero gh calls.
func TestApplyPublish_DryRun(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	gh := newFakeGH().on("repo view", []byte("https://github.com/owner/repo\n"))

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, true /* dry-run */); err != nil {
		t.Fatalf("runPublish dry-run: %v", err)
	}

	// Only the repo view call is allowed (it's part of resolveRepoURL); no create/patch.
	for _, call := range gh.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "POST") || strings.Contains(joined, "PATCH") {
			t.Errorf("dry-run should not make write calls; got: %v", call)
		}
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("dry-run output should contain 'dry-run': %s", buf.String())
	}
}

// TestApplyPublish_RepoURLFailureDegrades: if repo view fails, body has no
// permalink but the command succeeds.
func TestApplyPublish_RepoURLFailureDegrades(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	gh := newFakeGH().
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":1}`))
	// No route for "repo view" -> fakeGH returns error -> resolveRepoURL returns "".

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish should succeed even without repo URL: %v", err)
	}

	// Body should not contain a permalink link.
	createCalls := gh.callsContaining("repos/{owner}/{repo}/issues -X POST")
	if len(createCalls) != 1 {
		t.Fatalf("expected 1 create call")
	}
	body, _ := argValue(createCalls[0], "body")
	if strings.Contains(body, "blob/") {
		t.Errorf("body contains permalink despite missing repo URL; body=%q", body)
	}

	// But fingerprint marker still first.
	wantMarker := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("body does not start with marker even without permalink")
	}
}

// TestApplyPublish_GHMissing: a fake runner that returns an exec.ErrNotFound
// style error surfaces a clear error message.
func TestApplyPublish_GHMissing(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	// Fake that returns "executable file not found" for all calls.
	notFoundGH := func(_ context.Context, args ...string) ([]byte, error) {
		return nil, &ghNotFoundErr{}
	}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	err := runPublish(ctx, &buf, notFoundGH, st, cfg, 2, false)
	if err == nil {
		t.Fatal("expected error for missing gh binary")
	}
	if !strings.Contains(err.Error(), "gh CLI is required") {
		t.Errorf("error should mention gh CLI requirement; got: %v", err)
	}
}

// ghNotFoundErr mimics the error returned when gh is not on PATH.
type ghNotFoundErr struct{}

func (e *ghNotFoundErr) Error() string { return "executable file not found in $PATH" }

// TestApplyPublish_Idempotence: running publish twice creates only one issue.
func TestApplyPublish_Idempotence(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	callCount := 0
	gh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":55}`))

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}

	// First run: creates the issue.
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, gh.run, st, cfg, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	for _, c := range gh.calls {
		if strings.Contains(strings.Join(c, " "), "POST") {
			callCount++
		}
	}
	if callCount != 1 {
		t.Errorf("first run: expected 1 POST, got %d", callCount)
	}

	// Second run: the finding's UpdatedAt has not changed, so it should be skipped.
	gh2 := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":56}`))

	var buf2 strings.Builder
	if err := runPublish(ctx, &buf2, gh2.run, st, cfg, 2, false); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// No new POST on second run (idempotent).
	for _, c := range gh2.calls {
		if strings.Contains(strings.Join(c, " "), "POST") {
			t.Errorf("second run should not create a new issue (idempotent)")
		}
	}
	if !strings.Contains(buf2.String(), "skipped=1") {
		t.Errorf("second run should report skipped=1; got: %s", buf2.String())
	}
}

// ---- Command-level integration test ----

// TestPublishCmd_Via_Setup uses the report_test.go setup() pattern: a real
// on-disk store, seeded via setup(), and the publish command invoked via run().
func TestPublishCmd_Via_Setup(t *testing.T) {
	cfgPath, _, f := setup(t)

	// Inject the fake gh before the test runs. Restore after.
	old := publishGH
	defer func() { publishGH = old }()
	fgh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":100}`))
	publishGH = fgh.run

	out, err := run(t, cfgPath, "publish")
	if err != nil {
		t.Fatalf("publish: %v\nout: %s", err, out)
	}
	if !strings.Contains(out, "created=1") {
		t.Errorf("expected created=1 in output: %s", out)
	}

	// Re-run: idempotent — no second create.
	fgh2 := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":101}`))
	publishGH = fgh2.run

	out2, err2 := run(t, cfgPath, "publish")
	if err2 != nil {
		t.Fatalf("second publish: %v\nout: %s", err2, out2)
	}
	for _, c := range fgh2.calls {
		if strings.Contains(strings.Join(c, " "), "POST") {
			t.Errorf("second publish should not POST again (idempotent); calls: %v", fgh2.calls)
		}
	}

	_ = f // silence unused warning; finding was seeded via setup()
}

// min is a helper used in test assertions.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestApplyPublish_CloseOrdering pins comment-before-PATCH: the timeline
// comment explaining the close must land before the state change.
func TestApplyPublish_CloseOrdering(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, 77, "open"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatal(err)
	}

	gh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("issues/77/comments", []byte(`{"id":1}`)).
		on("issues/77 -X PATCH", []byte(`{"number":77}`))

	var buf strings.Builder
	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	commentIdx, patchIdx := -1, -1
	for i, call := range gh.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "issues/77/comments") {
			commentIdx = i
		}
		if strings.Contains(joined, "issues/77 -X PATCH") {
			patchIdx = i
		}
	}
	if commentIdx == -1 || patchIdx == -1 || commentIdx > patchIdx {
		t.Errorf("comment must precede PATCH: commentIdx=%d patchIdx=%d calls=%v", commentIdx, patchIdx, gh.calls)
	}
}

// TestApplyPublish_ClosingResume pins the interrupted-close recovery: a row in
// state "closing" means the auto-close comment already landed, so the resume
// run must PATCH only — never a second comment.
func TestApplyPublish_ClosingResume(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, 77, "closing"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatal(err)
	}

	gh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("issues/77 -X PATCH", []byte(`{"number":77}`))
	// NOTE: no route for issues/77/comments — a comment POST would error.

	var buf strings.Builder
	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish (resume) should not re-comment: %v", err)
	}
	if n := len(gh.callsContaining("issues/77/comments")); n != 0 {
		t.Errorf("resume must not post a second auto-close comment, posted %d", n)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if pi.State != "closed" {
		t.Errorf("state after resume = %q, want closed", pi.State)
	}
}

// TestApplyPublish_PendingRecovery covers both arms of the interrupted-create
// recovery: marker found on GitHub -> adopt the issue number without creating;
// marker absent -> create normally. Either way the row must land in "open".
func TestApplyPublish_PendingRecovery(t *testing.T) {
	t.Run("adopts existing issue by marker", func(t *testing.T) {
		ctx := context.Background()
		st, f := setupPublishStore(t)
		if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, 0, "pending"); err != nil {
			t.Fatal(err)
		}

		existing := `[{"number":99,"body":"<!-- bugbot:fp=` + f.Fingerprint + ` -->\n\nbody"}]`
		gh := newFakeGH().
			on("repo view", []byte("https://github.com/owner/repo\n")).
			on("issues?state=all", []byte(existing))
		// NOTE: no create route — a create attempt would error.

		var buf strings.Builder
		cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
		if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
			t.Fatalf("runPublish (recover-adopt): %v", err)
		}
		pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if pi.IssueNumber != 99 || pi.State != "open" {
			t.Errorf("recovered row = #%d state=%q, want #99 open", pi.IssueNumber, pi.State)
		}
	})

	t.Run("creates when no marker found", func(t *testing.T) {
		ctx := context.Background()
		st, f := setupPublishStore(t)
		if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, 0, "pending"); err != nil {
			t.Fatal(err)
		}

		gh := newFakeGH().
			on("repo view", []byte("https://github.com/owner/repo\n")).
			on("issues?state=all", []byte(`[]`)).
			on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":43}`))

		var buf strings.Builder
		cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
		if err := runPublish(ctx, &buf, gh.run, st, cfg, 2, false); err != nil {
			t.Fatalf("runPublish (recover-create): %v", err)
		}
		pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if pi.IssueNumber != 43 || pi.State != "open" {
			t.Errorf("recovered row = #%d state=%q, want #43 open", pi.IssueNumber, pi.State)
		}
	})
}

// TestRenderIssueBody_ReasoningCapped pins the GitHub 65536-char body limit
// mitigation: an oversized reasoning trace is truncated with a marker.
func TestRenderIssueBody_ReasoningCapped(t *testing.T) {
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		Reasoning:   strings.Repeat("x", 40*1024),
	}
	body := renderIssueBody(f, "")
	if len(body) > 36*1024 {
		t.Errorf("body length %d exceeds expected cap envelope", len(body))
	}
	if !strings.Contains(body, "truncated by bugbot") {
		t.Error("truncation marker missing from capped body")
	}
}

// TestStorePublisher_WarnsOnceOnMissingGH pins the daemon latch: the first
// missing-gh error warns and disables; subsequent cycles are silent no-ops.
func TestStorePublisher_WarnsOnceOnMissingGH(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	calls := 0
	ghMissing := func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("gh api: exec: %w", exec.ErrNotFound)
	}

	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := NewStorePublisher(ghMissing, st, config.Publish{TierMin: 2, CloseOnFixed: true}, log)

	if err := p.Publish(ctx); err != nil {
		t.Fatalf("first cycle: publish must swallow gh-missing, got %v", err)
	}
	afterFirst := calls // a single run may make several gh calls before failing

	for i := 1; i < 3; i++ {
		if err := p.Publish(ctx); err != nil {
			t.Fatalf("cycle %d: publish must swallow gh-missing, got %v", i, err)
		}
	}

	if got := strings.Count(logBuf.String(), "publish disabled"); got != 1 {
		t.Errorf("warn count = %d, want exactly 1\nlog:\n%s", got, logBuf.String())
	}
	if calls != afterFirst {
		t.Errorf("gh invoked %d more times after the latch; later cycles must be no-ops", calls-afterFirst)
	}
}
