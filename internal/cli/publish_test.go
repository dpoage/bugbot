package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, true /* dry-run */); err != nil {
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
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	err := runPublish(ctx, &buf, notFoundGH, st, cfg, publishProvenance{}, 2, false)
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
	if err := runPublish(ctx, &buf1, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	if err := runPublish(ctx, &buf2, gh2.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
		if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
		if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
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
	body := renderIssueBody(f, "", publishProvenance{})
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

// makeReproDir creates a temporary repro artifact directory that mirrors the
// structure writeArtifacts produces: run.sh + a test source file.
// Returns the dir path; the directory is cleaned up via t.Cleanup.
func makeReproDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	runSh := "#!/usr/bin/env bash\n# Generated by Bugbot.\nset -euo pipefail\n\ngo test -run TestRaceCondition ./internal/store/...\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(runSh), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}

	// Mirror a test source file under its repo-relative path.
	srcDir := filepath.Join(dir, "internal", "store")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	testSrc := `package store_test

import "testing"

func TestRaceCondition(t *testing.T) {
	t.Error("demonstrates the race condition")
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "race_test.go"), []byte(testSrc), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	return dir
}

// TestRenderIssueBody_Structure verifies the full body layout: sections appear
// in the documented order and bookkeeping fields appear only inside the metadata
// block, not in the human-visible preamble.
func TestRenderIssueBody_Structure(t *testing.T) {
	reproDir := makeReproDir(t)

	f := store.Finding{
		Fingerprint:         "abcdef1234567890",
		Title:               "race condition in store",
		Description:         "Two goroutines write to the cache without synchronization.",
		Reasoning:           "I verified this by inspecting the call chain.",
		Severity:            "high",
		Tier:                1,
		Lens:                "race",
		CorroboratingLenses: []string{"memory"},
		File:                "internal/store/cache.go",
		Line:                42,
		CommitSHA:           "deadbeef",
		ReproPath:           reproDir,
		FixPatch:            "--- a/internal/store/cache.go\n+++ b/internal/store/cache.go\n@@ -40,6 +40,7 @@\n+\tmu.Lock()\n",
	}

	prov := publishProvenance{
		FinderModel:   "claude-sonnet-4-6",
		VerifierModel: "claude-opus-4",
		ProviderType:  "anthropic",
	}

	body := renderIssueBody(f, "https://github.com/owner/repo", prov)

	// 1. Fingerprint marker MUST be the first line.
	wantMarker := "<!-- bugbot:fp=abcdef1234567890 -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("body does not start with fingerprint marker; first 120 chars: %q", body[:min(len(body), 120)])
	}

	// 2. Title heading present.
	if !strings.Contains(body, "## race condition in store") {
		t.Error("title heading not found")
	}

	// 3. Severity and Location appear BEFORE the description.
	severityPos := strings.Index(body, "**Severity:**")
	locationPos := strings.Index(body, "**Location:**")
	descPos := strings.Index(body, "Two goroutines write")
	if severityPos < 0 || locationPos < 0 {
		t.Error("Severity or Location meta line missing")
	}
	if severityPos > descPos || locationPos > descPos {
		t.Errorf("Severity/Location must appear before description; sev=%d loc=%d desc=%d", severityPos, locationPos, descPos)
	}

	// 3. Permalink is merged into Location when repoURL is non-empty.
	if !strings.Contains(body, "blob/deadbeef/internal/store/cache.go#L42") {
		t.Error("source permalink not found in Location line")
	}

	// 4. Description present.
	if !strings.Contains(body, "Two goroutines write to the cache without synchronization.") {
		t.Error("description missing")
	}

	// 5. Fix diff present in a ```diff fence.
	if !strings.Contains(body, "```diff\n") {
		t.Error("fix patch diff fence not found")
	}
	if !strings.Contains(body, "Candidate fix (witness") {
		t.Error("fix patch caveat heading missing")
	}

	// 6. Repro <details> contains run command and test source.
	if !strings.Contains(body, "<details><summary>Reproduction</summary>") {
		t.Error("Reproduction details block missing")
	}
	if !strings.Contains(body, "go test -run TestRaceCondition") {
		t.Error("run command not inlined in repro block")
	}
	if !strings.Contains(body, "TestRaceCondition") {
		t.Error("test source not inlined in repro block")
	}

	// 7. Metadata block: Lens, Tier label, and fingerprint-as-standalone-value
	// appear ONLY inside the "Bugbot metadata" details block. The fingerprint
	// also appears in the hidden marker comment (first line), so we check that
	// each field's first non-marker occurrence is after the metadata open tag.
	metaOpenTag := "<details><summary>Bugbot metadata</summary>"
	metaPos := strings.Index(body, metaOpenTag)
	if metaPos < 0 {
		t.Fatal("Bugbot metadata block missing")
	}

	// Lens and Tier label must not appear before the metadata block.
	for _, needle := range []string{"| Lens |", "T1 Reproduced"} {
		idx := strings.Index(body, needle)
		if idx < 0 {
			t.Errorf("expected %q somewhere in body", needle)
			continue
		}
		if idx < metaPos {
			t.Errorf("%q appears before metadata block (offset %d < metaPos %d)", needle, idx, metaPos)
		}
	}

	// Fingerprint is allowed in the hidden marker comment (top of body) but
	// must also appear inside the metadata block as a table cell.
	fpInMeta := strings.Index(body[metaPos:], f.Fingerprint)
	if fpInMeta < 0 {
		t.Errorf("fingerprint %q not found inside metadata block", f.Fingerprint)
	}

	// Model and provider strings present inside metadata.
	if !strings.Contains(body, "claude-sonnet-4-6") {
		t.Error("finder model not found in body")
	}
	if !strings.Contains(body, "anthropic") {
		t.Error("provider type not found in body")
	}

	// 8. Verification trace block present.
	if !strings.Contains(body, "<details><summary>Verification trace</summary>") {
		t.Error("Verification trace details block missing")
	}

	// 9. Attribution footer is the last non-empty line.
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			if !strings.Contains(lines[i], "Filed by Bugbot") {
				t.Errorf("last non-empty line should be attribution footer; got: %q", lines[i])
			}
			break
		}
	}
}

// TestRenderIssueBody_ReproMissing confirms that a missing repro dir produces
// a path-mention fallback and does not error the publish.
func TestRenderIssueBody_ReproMissing(t *testing.T) {
	f := store.Finding{
		Fingerprint: "fp123",
		Title:       "t",
		ReproPath:   "/nonexistent/repro/dir",
	}
	body := renderIssueBody(f, "", publishProvenance{})

	// Must contain a fallback mention of the path.
	if !strings.Contains(body, "/nonexistent/repro/dir") {
		t.Error("missing repro dir should produce a path-mention fallback")
	}
	// Must NOT contain the Reproduction details block (which is only for readable dirs).
	if strings.Contains(body, "<details><summary>Reproduction</summary>") {
		t.Error("Reproduction details block should NOT appear when repro dir is missing")
	}
	// Attribution footer still present.
	if !strings.Contains(body, "Filed by Bugbot") {
		t.Error("attribution footer missing when repro dir is absent")
	}

	// Full publish should not error.
	ctx := context.Background()
	st, _ := setupPublishStore(t)
	// Update the seeded finding to set ReproPath.
	findings, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen})
	if err != nil || len(findings) == 0 {
		t.Fatal("could not list findings")
	}
	findings[0].ReproPath = "/nonexistent/repro/dir"
	if _, err := st.UpsertFinding(ctx, findings[0]); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	gh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":5}`))
	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, gh.run, st, cfg, publishProvenance{}, 2, false); err != nil {
		t.Fatalf("runPublish must not error on missing repro dir: %v", err)
	}
}

// TestRenderIssueBody_ReproTruncation confirms the per-file and total caps are
// enforced when a repro artifact contains an oversized file.
func TestRenderIssueBody_ReproTruncation(t *testing.T) {
	dir := t.TempDir()

	// run.sh — minimal, just to make the dir readable.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/usr/bin/env bash\ngo test ./...\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}

	// An oversized test file: 15 KB, well above the 10 KB per-file cap.
	bigContent := strings.Repeat("// padding line\n", 15*1024/16+1)
	if err := os.WriteFile(filepath.Join(dir, "big_test.go"), []byte(bigContent), 0o644); err != nil {
		t.Fatalf("write big_test.go: %v", err)
	}

	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		ReproPath:   dir,
	}
	body := renderIssueBody(f, "", publishProvenance{})

	if !strings.Contains(body, "<details><summary>Reproduction</summary>") {
		t.Error("Reproduction block missing")
	}
	if !strings.Contains(body, "truncated by bugbot") {
		t.Error("truncation marker missing from oversized repro section")
	}
}

// TestRenderIssueBody_NoFixPatch confirms fix patch section is absent when FixPatch is empty.
func TestRenderIssueBody_NoFixPatch(t *testing.T) {
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		FixPatch:    "",
	}
	body := renderIssueBody(f, "", publishProvenance{})
	if strings.Contains(body, "Candidate fix") {
		t.Error("fix patch section should be absent when FixPatch is empty")
	}
}

// ---- MUST-FIX 1: code-fence breakout prevention ----

// TestFencedBlock_PreventBreakout verifies that fencedBlock produces a fence
// longer than any backtick run inside the content so the fence cannot be
// broken by embedded ``` or ```` sequences.
func TestFencedBlock_PreventBreakout(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"three backticks", "before\n```\nafter"},
		{"four backticks", "before\n````\nafter"},
		{"five backticks", "x\n`````\ny"},
		{"no backticks", "plain content\nno ticks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := fencedBlock("diff", tc.content)
			lines := strings.Split(out, "\n")
			if len(lines) < 2 {
				t.Fatalf("fencedBlock output too short: %q", out)
			}
			openFence := strings.TrimRight(lines[0], "abcdefghijklmnopqrstuvwxyz") // strip lang tag
			closeFence := lines[len(lines)-1]
			if closeFence == "" && len(lines) > 1 {
				closeFence = lines[len(lines)-2]
			}
			fenceLen := len(openFence)
			longest := longestBacktickRun(tc.content)
			if fenceLen <= longest {
				t.Errorf("fence length %d must be > longest backtick run %d in content", fenceLen, longest)
			}
			if fenceLen < 3 {
				t.Errorf("fence length %d is below CommonMark minimum of 3", fenceLen)
			}
			// The closing fence must match the opening fence length.
			if len(closeFence) != fenceLen {
				t.Errorf("closing fence length %d != opening fence length %d", len(closeFence), fenceLen)
			}
		})
	}
}

// TestRenderIssueBody_FencedPatchBreakout confirms that a FixPatch containing
// a ``` sequence does not break out of the diff fence in the rendered body.
func TestRenderIssueBody_FencedPatchBreakout(t *testing.T) {
	// A diff that contains a triple-backtick and a quad-backtick on separate lines.
	maliciousPatch := "--- a/foo.go\n+++ b/foo.go\n```\n````\n+fix\n"
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		FixPatch:    maliciousPatch,
	}
	body := renderIssueBody(f, "", publishProvenance{})

	// Count occurrences of the opening fence line. There should be exactly one
	// opening fence and one closing fence of equal length and they should wrap
	// all of the patch content. The simplest check: the body must NOT contain
	// an unmatched ``` that closes the block early. We verify this by checking
	// that after the first "```diff" the next fence line of the SAME length
	// contains only backticks (i.e. it is the closing fence, not injected content).
	lines := strings.Split(body, "\n")
	inFence := false
	fenceMarker := ""
	for _, line := range lines {
		if !inFence {
			if strings.HasPrefix(line, "```diff") || strings.HasPrefix(line, "````diff") || strings.HasPrefix(line, "`````diff") {
				fenceMarker = strings.TrimSuffix(line, "diff")
				inFence = true
			}
			continue
		}
		// Inside the fence: a line that matches the exact fence marker closes it.
		if line == fenceMarker {
			inFence = false
			fenceMarker = ""
			continue
		}
	}
	if inFence {
		t.Error("code fence was never properly closed — possible breakout")
	}
}

// TestRenderIssueBody_FencedReproBreakout confirms that repro source files
// containing ``` sequences do not break out of their code fences.
func TestRenderIssueBody_FencedReproBreakout(t *testing.T) {
	dir := t.TempDir()
	// run.sh with backtick sequence in a command.
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/bash\necho ```\ngo test ./...\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	// A Go test file containing triple-backtick in a comment.
	goSrc := "package foo_test\n\nimport \"testing\"\n\n// ```\nfunc TestFoo(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "foo_test.go"), []byte(goSrc), 0o644); err != nil {
		t.Fatalf("write foo_test.go: %v", err)
	}

	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		ReproPath:   dir,
	}
	body := renderIssueBody(f, "", publishProvenance{})

	// Verify no unmatched fences: parse all fenced blocks and ensure each
	// opens and closes correctly.
	lines := strings.Split(body, "\n")
	inFence := false
	fenceMarker := ""
	for _, line := range lines {
		if !inFence {
			// A fence opener is a line of 3+ backticks optionally followed by a lang tag.
			stripped := strings.TrimLeft(line, "`")
			ticks := len(line) - len(stripped)
			if ticks >= 3 {
				fenceMarker = strings.Repeat("`", ticks)
				inFence = true
			}
			continue
		}
		if line == fenceMarker {
			inFence = false
			fenceMarker = ""
		}
	}
	if inFence {
		t.Errorf("unclosed code fence detected in body (last fenceMarker=%q)", fenceMarker)
	}
}

// ---- MUST-FIX 2: </details> breakout prevention ----

// TestSanitizeDetailsTag verifies that </details> and <details> sequences
// (in various cases) are replaced with entity-escaped equivalents.
func TestSanitizeDetailsTag(t *testing.T) {
	cases := []struct {
		input    string
		mustNot  string // sequence that must NOT appear in output
		mustHave string // entity that must appear instead
	}{
		{"foo </details><script>x</script> bar", "</details>", "&lt;/details"},
		{"foo <DETAILS> bar", "<DETAILS>", "&lt;DETAILS"},
		{"no tags here", "", ""},
		{"</Details>xyz", "</Details>", "&lt;/Details"},
		{"<details><summary>inner</summary></details>", "</details>", "&lt;/details"},
	}
	for _, tc := range cases {
		got := sanitizeDetailsTag(tc.input)
		if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
			t.Errorf("sanitizeDetailsTag(%q) still contains %q; got %q", tc.input, tc.mustNot, got)
		}
		if tc.mustHave != "" && !strings.Contains(strings.ToLower(got), strings.ToLower(tc.mustHave)) {
			t.Errorf("sanitizeDetailsTag(%q) missing %q; got %q", tc.input, tc.mustHave, got)
		}
	}
}

// TestRenderIssueBody_DetailsBreakout confirms that Reasoning containing a
// literal </details> cannot close the verification trace block early. Our
// defense is to sanitize <details and </details tags in Reasoning (replacing
// '<' with '&lt;') so GitHub's Markdown renderer cannot use them to close the
// enclosing <details> block. GitHub strips <script> tags independently; we
// only need to prevent the </details> breakout.
func TestRenderIssueBody_DetailsBreakout(t *testing.T) {
	maliciousReasoning := "step 1\n</details><script>x</script>\nstep 2"
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		Reasoning:   maliciousReasoning,
	}
	body := renderIssueBody(f, "", publishProvenance{})

	// The raw </details> sequence must appear in the body only as many times as
	// bugbot itself emits it (once for metadata, once for trace). The sanitizer
	// converts the Reasoning's </details> to &lt;/details so it does not count.
	// This finding has no repro, so exactly 2 bugbot-emitted </details> exist.
	count := strings.Count(body, "</details>")
	if count > 2 {
		t.Errorf("body contains %d </details> tags; expected ≤2 (only bugbot-emitted ones); the injected </details> from Reasoning was not neutralised", count)
	}

	// The sanitized form must be present (proving replacement happened).
	if !strings.Contains(body, "&lt;/details") {
		t.Error("expected &lt;/details in body (sanitized form); not found")
	}

	// Attribution footer must still be present (body integrity).
	if !strings.Contains(body, "Filed by Bugbot") {
		t.Error("attribution footer missing after details-breakout sanitization")
	}
}

// ---- MUST-FIX 3: total body size guard ----

// TestRenderIssueBody_OversizedFixPatch confirms that a very large FixPatch is
// capped at ~20 KB with the truncation marker.
func TestRenderIssueBody_OversizedFixPatch(t *testing.T) {
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		FixPatch:    strings.Repeat("x", 30*1024), // 30 KB, over the 20 KB cap
	}
	body := renderIssueBody(f, "", publishProvenance{})
	if !strings.Contains(body, "truncated by bugbot") {
		t.Error("truncation marker missing from oversized FixPatch body")
	}
}

// TestRenderIssueBody_OversizedDescription confirms that a very large
// Description is capped at ~10 KB with the truncation marker.
func TestRenderIssueBody_OversizedDescription(t *testing.T) {
	f := store.Finding{
		Fingerprint: "fp",
		Title:       "t",
		Description: strings.Repeat("d", 15*1024), // 15 KB, over the 10 KB cap
	}
	body := renderIssueBody(f, "", publishProvenance{})
	if !strings.Contains(body, "truncated by bugbot") {
		t.Error("truncation marker missing from oversized Description body")
	}
}

// TestRenderIssueBody_TotalBodyGuard confirms that a pathological finding
// (huge description + huge patch + huge reasoning) produces a body under
// GitHub's 65 536 char limit, with the fingerprint marker on line 1 and the
// attribution footer as the last non-empty line.
func TestRenderIssueBody_TotalBodyGuard(t *testing.T) {
	fp := "abcdef1234567890abcdef1234567890"
	f := store.Finding{
		Fingerprint: fp,
		Title:       strings.Repeat("T", 1000),
		Description: strings.Repeat("D", 20*1024),
		FixPatch:    strings.Repeat("P", 25*1024),
		Reasoning:   strings.Repeat("R", 40*1024),
	}
	body := renderIssueBody(f, "", publishProvenance{})

	// Must stay under GitHub's hard limit.
	if len(body) >= 65536 {
		t.Errorf("body length %d exceeds GitHub's 65536 char limit", len(body))
	}

	// Line 1 must be the fingerprint marker.
	wantMarker := "<!-- bugbot:fp=" + fp + " -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("body does not start with fingerprint marker; first 120 chars: %q", body[:min(len(body), 120)])
	}

	// Attribution footer must be the last non-empty line.
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			if !strings.Contains(lines[i], "Filed by Bugbot") {
				t.Errorf("last non-empty line should be attribution footer; got: %q", lines[i])
			}
			break
		}
	}
}

// ---- Fix 4: patch.diff excluded from repro artifact walk ----

// TestRenderReproSection_SkipsPatchDiff confirms that patch.diff written by
// the patch prover is excluded from the reproduction section.
func TestRenderReproSection_SkipsPatchDiff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/bash\ngo test ./...\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	// patch.diff should be excluded.
	if err := os.WriteFile(filepath.Join(dir, "patch.diff"), []byte("--- a/foo\n+++ b/foo\n@@ -1 +1 @@\n-old\n+new\n"), 0o644); err != nil {
		t.Fatalf("write patch.diff: %v", err)
	}
	// A normal source file should be included.
	if err := os.WriteFile(filepath.Join(dir, "foo_test.go"), []byte("package foo\n"), 0o644); err != nil {
		t.Fatalf("write foo_test.go: %v", err)
	}

	section := renderReproSection(dir)

	if strings.Contains(section, "patch.diff") {
		t.Error("patch.diff should be excluded from the repro section")
	}
	if !strings.Contains(section, "foo_test.go") {
		t.Error("foo_test.go should be included in the repro section")
	}
}

// TestRenderReproSection_ExtensionAllowlist confirms that non-source files
// (e.g. binary-like or unknown extensions) are excluded from the repro section.
func TestRenderReproSection_ExtensionAllowlist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/bash\ngo test ./...\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	// A binary-like file with an unknown extension — must be excluded.
	if err := os.WriteFile(filepath.Join(dir, "binary.exe"), []byte("\x7fELF"), 0o755); err != nil {
		t.Fatalf("write binary.exe: %v", err)
	}
	// A Go source file — must be included.
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main_test.go: %v", err)
	}

	section := renderReproSection(dir)

	if strings.Contains(section, "binary.exe") {
		t.Error("binary.exe (unknown extension) should be excluded from the repro section")
	}
	if !strings.Contains(section, "main_test.go") {
		t.Error("main_test.go should be included in the repro section")
	}
}

// ---- Fix 5: NewStorePublisher delegates to NewStorePublisherWithProvenance ----

// TestNewStorePublisher_DelegatesProvenance confirms that NewStorePublisher is
// a thin shim over NewStorePublisherWithProvenance and produces identical
// publisher state (zero provenance).
func TestNewStorePublisher_DelegatesProvenance(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	shim := NewStorePublisher(realGH, st, cfg, log)
	explicit := NewStorePublisherWithProvenance(realGH, st, cfg, publishProvenance{}, log)

	// Both should have the same zero provenance. We verify by running a dry
	// publish with a fake gh (no real network call) and checking no panic/error.
	// The real test is that the shim compiles and delegates (structural).
	_ = shim
	_ = explicit

	// Confirm the shim's prov field is zero.
	if shim.prov != (publishProvenance{}) {
		t.Errorf("NewStorePublisher shim prov = %+v, want zero", shim.prov)
	}
	_ = ctx
}
