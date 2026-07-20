package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/tracker"
	githubtracker "github.com/dpoage/bugbot/internal/tracker/github"
)

// ---- planPublish unit tests (pure; no gh calls) ----

// makeOpenFinding builds a minimal open finding for plan tests.
func makeOpenFinding(fp string, tier domain.Tier, updatedAt time.Time) domain.Finding {
	return domain.Finding{
		ID:          fp,
		Fingerprint: fp,
		Title:       "title for " + fp,
		Tier:        tier,
		Status:      domain.StatusOpen,
		File:        "x.go",
		Line:        1,
		UpdatedAt:   updatedAt,
	}
}

func makeFixedFinding(fp string) domain.Finding {
	f := makeOpenFinding(fp, 2, time.Now())
	f.Status = domain.StatusFixed
	return f
}

func makeDismissedFinding(fp string) domain.Finding {
	f := makeOpenFinding(fp, 2, time.Now())
	f.Status = domain.StatusDismissed
	return f
}

func makeSupersededFinding(fp, canonicalFP string) domain.Finding {
	f := makeOpenFinding(fp, 2, time.Now())
	f.Status = domain.StatusSuperseded
	f.SupersededBy = canonicalFP
	f.SupersededReason = "backlog reconcile: merged into " + canonicalFP + " (dedup arbiter yes)"
	return f
}

func makePublishedIssue(fp, key string, state store.IssueState, updatedAt time.Time) store.PublishedIssue {
	return store.PublishedIssue{
		Fingerprint: fp,
		IssueKey:    key,
		Tracker:     "github",
		State:       state,
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
	}
}

// TestPlanPublish_Create: an open finding with no published row -> create.
func TestPlanPublish_Create(t *testing.T) {
	now := time.Now()
	open := []domain.Finding{makeOpenFinding("fp1", 2, now)}
	actions := planPublish(open, nil, nil, nil, nil, 2, true)

	if len(actions) != 1 {
		t.Errorf("expected 1 action, got %d: %+v", len(actions), actions)
	} else if _, ok := actions[0].(publishCreate); !ok {
		t.Errorf("expected publishCreate, got %T", actions[0])
	}
}

// TestPlanPublish_Skip: open finding with published row and UpdatedAt <= published.UpdatedAt -> skip.
func TestPlanPublish_Skip(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	open := []domain.Finding{makeOpenFinding("fp1", 2, t0)}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "10", store.IssueStateOpen, t0.Add(time.Second)),
	}
	actions := planPublish(open, nil, nil, nil, published, 2, true)

	if len(actions) != 1 {
		t.Errorf("expected 1 action, got %d: %+v", len(actions), actions)
	} else if _, ok := actions[0].(publishSkip); !ok {
		t.Errorf("expected publishSkip, got %T", actions[0])
	}
}

// TestPlanPublish_Update: open finding with published row but UpdatedAt newer -> update.
func TestPlanPublish_Update(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	t1 := t0.Add(2 * time.Hour) // finding updated after published.UpdatedAt
	open := []domain.Finding{makeOpenFinding("fp1", 2, t1)}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "10", store.IssueStateOpen, t0),
	}
	actions := planPublish(open, nil, nil, nil, published, 2, true)

	if len(actions) != 1 {
		t.Errorf("expected 1 action, got %d: %+v", len(actions), actions)
	} else if act, ok := actions[0].(publishUpdate); !ok {
		t.Errorf("expected publishUpdate, got %T", actions[0])
	} else if act.issueKey != "10" {
		t.Errorf("issueKey = %q, want 10", act.issueKey)
	}
}

// TestPlanPublish_TierFiltering: T3 finding excluded at tierMin=2; T0,T1,T2 included.
func TestPlanPublish_TierFiltering(t *testing.T) {
	now := time.Now()
	open := []domain.Finding{
		makeOpenFinding("fp0", 0, now),
		makeOpenFinding("fp1", 1, now),
		makeOpenFinding("fp2", 2, now),
		makeOpenFinding("fp3", 3, now), // should be excluded
	}
	actions := planPublish(open, nil, nil, nil, nil, 2, true)

	// Expect 3 creates (T0, T1, T2), not T3.
	creates := 0
	for _, a := range actions {
		if _, ok := a.(publishCreate); ok {
			creates++
		}
		if act, ok := a.(publishCreate); ok && act.finding.Tier == 3 {
			t.Error("T3 finding should not be created at tierMin=2")
		}
	}
	if creates != 3 {
		t.Errorf("expected 3 creates (T0/T1/T2), got %d", creates)
	}
}

// TestPlanPublish_Close: fixed finding with open published row -> close.
func TestPlanPublish_Close(t *testing.T) {
	fixed := []domain.Finding{makeFixedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "5", store.IssueStateOpen, time.Now()),
	}
	actions := planPublish(nil, fixed, nil, nil, published, 2, true)

	if len(actions) != 1 {
		t.Errorf("expected 1 action, got %d: %+v", len(actions), actions)
	} else if act, ok := actions[0].(publishClose); !ok {
		t.Errorf("expected publishClose, got %T", actions[0])
	} else if act.issueKey != "5" {
		t.Errorf("issueKey = %q, want 5", act.issueKey)
	}
}

// TestPlanPublish_DismissedClose: dismissed finding with open row and
// close_on_fixed=true -> close.
func TestPlanPublish_DismissedClose(t *testing.T) {
	dismissed := []domain.Finding{makeDismissedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "7", store.IssueStateOpen, time.Now()),
	}
	actions := planPublish(nil, nil, dismissed, nil, published, 2, true)

	if len(actions) != 1 {
		t.Errorf("expected 1 action, got %d: %+v", len(actions), actions)
	} else if _, ok := actions[0].(publishClose); !ok {
		t.Errorf("expected publishClose for dismissed, got %T", actions[0])
	}
}

// TestPlanPublish_SupersededClose: a superseded (backlog-reconcile
// merged-away duplicate) finding with an open published row -> close, with a
// duplicate-specific comment referencing the canonical fingerprint. Proves
// the ezmx.4 review's blocking finding (superseded findings never closed
// their GitHub issue) is fixed.
func TestPlanPublish_SupersededClose(t *testing.T) {
	const canonicalFP = "canonical-fp-abc123"
	superseded := []domain.Finding{makeSupersededFinding("fp1", canonicalFP)}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "9", store.IssueStateOpen, time.Now()),
	}
	actions := planPublish(nil, nil, nil, superseded, published, 2, true)

	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d: %+v", len(actions), actions)
	}
	act, ok := actions[0].(publishClose)
	if !ok {
		t.Fatalf("expected publishClose for superseded, got %T", actions[0])
	}
	if act.issueKey != "9" {
		t.Errorf("issueKey = %q, want 9", act.issueKey)
	}
	comment := autoCloseComment(act.finding)
	if !strings.Contains(comment, canonicalFP) {
		t.Errorf("autoCloseComment(%+v) = %q, want it to reference canonical fingerprint %q", act.finding, comment, canonicalFP)
	}
}

// TestAutoCloseComment_NonSupersededOmitsCanonicalFingerprint proves the
// duplicate-specific wording is superseded-only: a plain fixed/dismissed
// close comment never mentions SupersededBy (which is empty for those
// statuses in the first place).
func TestAutoCloseComment_NonSupersededOmitsCanonicalFingerprint(t *testing.T) {
	comment := autoCloseComment(makeFixedFinding("fp1"))
	if strings.Contains(comment, "canonical fingerprint") {
		t.Errorf("autoCloseComment for a fixed finding should not mention a canonical fingerprint, got %q", comment)
	}
}

// TestPlanPublish_CloseOnFixedFalse: fixed finding -> NO close action when close_on_fixed=false.
func TestPlanPublish_CloseOnFixedFalse(t *testing.T) {
	fixed := []domain.Finding{makeFixedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "5", store.IssueStateOpen, time.Now()),
	}
	actions := planPublish(nil, fixed, nil, nil, published, 2, false /* close_on_fixed=false */)

	for _, a := range actions {
		if _, ok := a.(publishClose); ok {
			t.Errorf("close action should not be planned when close_on_fixed=false")
		}
	}
}

// TestPlanPublish_AlreadyClosed: fixed finding with already-closed published row -> skip (no action).
func TestPlanPublish_AlreadyClosed(t *testing.T) {
	fixed := []domain.Finding{makeFixedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "5", store.IssueStateClosed, time.Now()),
	}
	actions := planPublish(nil, fixed, nil, nil, published, 2, true)

	for _, a := range actions {
		if _, ok := a.(publishClose); ok {
			t.Errorf("should not close an already-closed issue")
		}
	}
}

// ---- applyPublish with fakeTracker ----

// setupPublishStore opens a fresh store, seeds one open T2 finding, and
// returns the store, the finding, and the DB path for re-open.
func setupPublishStore(t *testing.T) (*store.Store, domain.Finding) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := domain.Finding{
		Fingerprint: domain.Fingerprint("race", "x.go", fmt.Sprintf("%d|%s", 7, "boom")),
		Title:       "boom",
		Description: "desc",
		Reasoning:   "trace",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
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

// TestApplyPublish_Create: fakeTracker records the create call, the returned
// key is persisted, and the body starts with the fingerprint marker.
func TestApplyPublish_Create(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	ft := newFakeTracker()
	ft.createKeys = []tracker.IssueKey{"42"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	// Verify the create call was made.
	creates := ft.callsOf("create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create call, got %d; all calls: %v", len(creates), ft.calls)
	}
	// Title must match.
	if creates[0].title != f.Title {
		t.Errorf("title = %q, want %q", creates[0].title, f.Title)
	}
	// Body must start with the fingerprint marker.
	body := creates[0].body
	wantMarker := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("body does not start with marker %q\nbody = %q", wantMarker, body[:min(len(body), 80)])
	}
	// The base label must be passed through to the create call.
	if !slices.Equal(creates[0].labels, []string{"bugbot"}) {
		t.Errorf("create labels = %v, want [bugbot]", creates[0].labels)
	}

	// The published_issues row must be written.
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.IssueKey != "42" {
		t.Errorf("issue_key = %q, want 42", pi.IssueKey)
	}
	if pi.Tracker != "github" {
		t.Errorf("tracker = %q, want github", pi.Tracker)
	}
	if pi.State != "open" {
		t.Errorf("state = %q, want open", pi.State)
	}
}

// TestApplyPublish_Close: a fixed finding with an open published row triggers
// the auto-close comment, then the close.
func TestApplyPublish_Close(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	// Pre-record the published issue.
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "77", "open", ""); err != nil {
		t.Fatalf("seed published: %v", err)
	}
	// Mark the finding fixed.
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	// Should have posted the comment.
	comments := ft.callsOf("comment")
	if len(comments) != 1 || comments[0].key != "77" {
		t.Errorf("expected 1 comment call on issue 77, got %v", comments)
	}
	// Then closed the issue.
	closes := ft.callsOf("close")
	if len(closes) != 1 || closes[0].key != "77" {
		t.Errorf("expected 1 close call on issue 77, got %v", closes)
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

// TestApplyPublish_DryRun: dry-run makes zero tracker writes.
func TestApplyPublish_DryRun(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, true /* dry-run */); err != nil {
		t.Fatalf("runPublish dry-run: %v", err)
	}

	// Only reads (repoURL, list) are allowed; no create/update/close/etc.
	if writes := ft.writes(); len(writes) != 0 {
		t.Errorf("dry-run should not make write calls; got: %v", writes)
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("dry-run output should contain 'dry-run': %s", buf.String())
	}
}

// TestApplyPublish_RepoURLFailureDegrades: if the tracker cannot resolve the
// repo URL (RepoURL returns ""), the body has no permalink but the command
// succeeds.
func TestApplyPublish_RepoURLFailureDegrades(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	ft := newFakeTracker()
	ft.repoURL = "" // RepoURL degrades to "" on failure per the interface
	ft.createKeys = []tracker.IssueKey{"1"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish should succeed even without repo URL: %v", err)
	}

	// Body should not contain a permalink link.
	creates := ft.callsOf("create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create call")
	}
	body := creates[0].body
	if strings.Contains(body, "blob/") {
		t.Errorf("body contains permalink despite missing repo URL; body=%q", body)
	}

	// But fingerprint marker still first.
	wantMarker := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("body does not start with marker even without permalink")
	}
}

// TestApplyPublish_MissingPrereqAborts: a tracker error wrapping
// tracker.ErrMissingPrereq aborts the WHOLE run at the first action — later
// actions are never attempted, so at most ONE pending tombstone exists (not
// one per planned action) — and the error surfaces the adapter's install
// hint.
func TestApplyPublish_MissingPrereqAborts(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)
	seedSecondOpenFinding(t, ctx, st) // second create in the same plan

	ft := newFakeTracker()
	ft.createErr = errTrackerMissingPrereq()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	err := runPublish(ctx, &buf, ft, st, cfg, 2, false)
	if err == nil {
		t.Fatal("expected error for missing tracker prerequisite")
	}
	if !errors.Is(err, tracker.ErrMissingPrereq) {
		t.Errorf("error should wrap tracker.ErrMissingPrereq; got: %v", err)
	}
	if !strings.Contains(err.Error(), "gh CLI is required") {
		t.Errorf("error should surface the adapter's install hint; got: %v", err)
	}

	// Abort on the FIRST action: exactly one create attempt, exactly one
	// pending tombstone.
	if creates := ft.callsOf("create"); len(creates) != 1 {
		t.Errorf("expected exactly 1 create attempt before the abort, got %d", len(creates))
	}
	rows, lerr := st.ListPublishedIssues(ctx)
	if lerr != nil {
		t.Fatalf("list published: %v", lerr)
	}
	if len(rows) != 1 || rows[0].State != store.IssueStatePending {
		t.Errorf("expected exactly 1 pending tombstone after abort, got %+v", rows)
	}
}

// TestApplyPublish_Idempotence: running publish twice creates only one issue.
func TestApplyPublish_Idempotence(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}

	// First run: creates the issue.
	ft1 := newFakeTracker()
	ft1.createKeys = []tracker.IssueKey{"55"}
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, ft1, st, cfg, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if n := len(ft1.callsOf("create")); n != 1 {
		t.Errorf("first run: expected 1 create, got %d", n)
	}

	// Second run: the finding's UpdatedAt has not changed, so it should be skipped.
	ft2 := newFakeTracker()
	var buf2 strings.Builder
	if err := runPublish(ctx, &buf2, ft2, st, cfg, 2, false); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// No new writes on second run (idempotent).
	if writes := ft2.writes(); len(writes) != 0 {
		t.Errorf("second run should not write (idempotent); got %v", writes)
	}
	if !strings.Contains(buf2.String(), "skipped=1") {
		t.Errorf("second run should report skipped=1; got: %s", buf2.String())
	}
}

// ---- Command-level integration test ----

// TestPublishCmd_Via_Setup uses the report_test.go setup() pattern: a real
// on-disk store, seeded via setup(), and the publish command invoked via
// run(). The injected seam is the REAL GitHub adapter over a fakeGH runner,
// preserving one true end-to-end path cli -> adapter -> gh argv.
func TestPublishCmd_Via_Setup(t *testing.T) {
	cfgPath, _, f := setup(t)

	// Inject the adapter-backed tracker before the test runs. Restore after.
	old := publishTracker
	defer func() { publishTracker = old }()
	fgh := newFakeGH().
		on("repo view", []byte("https://github.com/owner/repo\n")).
		on("repos/{owner}/{repo}/labels -X POST", []byte(`{}`)).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":100}`))
	publishTracker = githubtracker.New(fgh.run, tracker.Config{Labels: []string{"bugbot"}})

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
		on("issues?state=closed", []byte("[]")).
		on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":101}`))
	publishTracker = githubtracker.New(fgh2.run, tracker.Config{Labels: []string{"bugbot"}})

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

// TestPublishCmd_UnknownTracker: a config selecting an unregistered tracker
// fails the publish command with the registry error naming the known
// trackers.
func TestPublishCmd_UnknownTracker(t *testing.T) {
	cfgPath, _, _ := setup(t)

	// Append the publish tracker selection to the setup() config.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	raw = append(raw, []byte("publish:\n  tracker: gitlab\n")...)
	if err := os.WriteFile(cfgPath, raw, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// The seam must stay nil so the command goes through the registry.
	old := publishTracker
	publishTracker = nil
	defer func() { publishTracker = old }()

	out, err := run(t, cfgPath, "publish")
	if err == nil {
		t.Fatalf("expected unknown-tracker error, got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), `unknown tracker "gitlab"`) {
		t.Errorf("error should name the unknown tracker; got: %v", err)
	}
	if !strings.Contains(err.Error(), "github") {
		t.Errorf("error should list known trackers (github); got: %v", err)
	}
}

// min is a helper used in test assertions.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestApplyPublish_CloseOrdering pins comment-before-close: the timeline
// comment explaining the close must land before the state change.
func TestApplyPublish_CloseOrdering(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "77", "open", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatal(err)
	}

	ft := newFakeTracker()

	var buf strings.Builder
	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	commentIdx := ft.indexOfOp("comment", "77")
	closeIdx := ft.indexOfOp("close", "77")
	if commentIdx == -1 || closeIdx == -1 || commentIdx > closeIdx {
		t.Errorf("comment must precede close: commentIdx=%d closeIdx=%d calls=%v", commentIdx, closeIdx, ft.calls)
	}
}

// TestApplyPublish_ClosingResume pins the interrupted-close recovery: a row in
// state "closing" means the auto-close comment already landed, so the resume
// run must close only — never a second comment.
func TestApplyPublish_ClosingResume(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "77", "closing", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatal(err)
	}

	ft := newFakeTracker()

	var buf strings.Builder
	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish (resume) should not re-comment: %v", err)
	}
	if n := len(ft.callsOf("comment")); n != 0 {
		t.Errorf("resume must not post a second auto-close comment, posted %d", n)
	}
	if n := len(ft.callsOf("close")); n != 1 {
		t.Errorf("resume must close exactly once, got %d", n)
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
// recovery: marker found on the tracker -> adopt the issue key without
// creating; marker absent -> create normally. Either way the row must land
// in "open".
func TestApplyPublish_PendingRecovery(t *testing.T) {
	t.Run("adopts existing issue by marker", func(t *testing.T) {
		ctx := context.Background()
		st, f := setupPublishStore(t)
		if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "", "pending", ""); err != nil {
			t.Fatal(err)
		}

		ft := newFakeTracker()
		ft.listByState["all"] = []tracker.Issue{
			{Key: "99", State: "open", Body: "<!-- bugbot:fp=" + f.Fingerprint + " -->\n\nbody"},
		}
		// NOTE: no create key scripted — a create attempt would error.

		var buf strings.Builder
		cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
		if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
			t.Fatalf("runPublish (recover-adopt): %v", err)
		}
		if n := len(ft.callsOf("create")); n != 0 {
			t.Errorf("adopt-via-marker must not create, got %d creates", n)
		}
		pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if pi.IssueKey != "99" || pi.State != "open" {
			t.Errorf("recovered row = %q state=%q, want 99 open", pi.IssueKey, pi.State)
		}
	})

	t.Run("creates when no marker found", func(t *testing.T) {
		ctx := context.Background()
		st, f := setupPublishStore(t)
		if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "", "pending", ""); err != nil {
			t.Fatal(err)
		}

		ft := newFakeTracker()
		ft.listByState["all"] = nil
		ft.createKeys = []tracker.IssueKey{"43"}

		var buf strings.Builder
		cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
		if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
			t.Fatalf("runPublish (recover-create): %v", err)
		}
		pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if pi.IssueKey != "43" || pi.State != "open" {
			t.Errorf("recovered row = %q state=%q, want 43 open", pi.IssueKey, pi.State)
		}
	})
}

// TestRenderIssueBody_ReasoningCapped pins the GitHub 65536-char body limit
// mitigation: an oversized reasoning trace is truncated with a marker.
func TestRenderIssueBody_ReasoningCapped(t *testing.T) {
	f := domain.Finding{
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

// TestStorePublisher_WarnsOnceOnMissingPrereq pins the daemon latch: the
// first tracker.ErrMissingPrereq error warns and disables; subsequent cycles
// are silent no-ops that never touch the tracker again.
func TestStorePublisher_WarnsOnceOnMissingPrereq(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	ft := newFakeTracker()
	ft.createErr = errTrackerMissingPrereq()

	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := NewStorePublisher(ft, st, config.Publish{TierMin: 2, CloseOnFixed: true}, log)

	if err := p.Publish(ctx); err != nil {
		t.Fatalf("first cycle: publish must swallow missing-prereq, got %v", err)
	}
	afterFirst := len(ft.calls) // a single run may make several tracker calls before failing

	for i := 1; i < 3; i++ {
		if err := p.Publish(ctx); err != nil {
			t.Fatalf("cycle %d: publish must swallow missing-prereq, got %v", i, err)
		}
	}

	if got := strings.Count(logBuf.String(), "publish disabled"); got != 1 {
		t.Errorf("warn count = %d, want exactly 1\nlog:\n%s", got, logBuf.String())
	}
	if len(ft.calls) != afterFirst {
		t.Errorf("tracker invoked %d more times after the latch; later cycles must be no-ops", len(ft.calls)-afterFirst)
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
// in the documented order, the machine front-matter is parseable on line 2,
// and agent-internal bookkeeping (lens, tier, models) is absent from the body.
func TestRenderIssueBody_Structure(t *testing.T) {
	reproDir := makeReproDir(t)

	f := domain.Finding{
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

	body := renderIssueBody(f, "https://github.com/owner/repo")

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

	// 7. Machine front-matter MUST be line 2 and parse as JSON with the
	// finding's coordinates.
	bodyLines := strings.Split(body, "\n")
	if len(bodyLines) < 2 {
		t.Fatal("body has fewer than 2 lines")
	}
	metaRe := regexp.MustCompile(`^<!-- bugbot:meta (.+) -->$`)
	m := metaRe.FindStringSubmatch(bodyLines[1])
	if m == nil {
		t.Fatalf("line 2 is not a bugbot:meta comment: %q", bodyLines[1])
	}
	var meta struct {
		Severity string `json:"severity"`
		Tier     int    `json:"tier"`
		Lens     string `json:"lens"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Commit   string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(m[1]), &meta); err != nil {
		t.Fatalf("bugbot:meta payload is not valid JSON: %v\npayload = %q", err, m[1])
	}
	if meta.Severity != "high" || meta.Tier != 1 || meta.Lens != "race" ||
		meta.File != "internal/store/cache.go" || meta.Line != 42 || meta.Commit != "deadbeef" {
		t.Errorf("bugbot:meta fields = %+v, want severity=high tier=1 lens=race file=internal/store/cache.go line=42 commit=deadbeef", meta)
	}

	// The agent-internal metadata block is gone: no details table, no
	// model/provider strings, no lens/tier rows anywhere in the body.
	for _, needle := range []string{
		"Bugbot metadata",
		"| Lens |",
		"| Tier |",
		"T1 Reproduced",
		"claude-sonnet-4-6",
		"claude-opus-4",
		"anthropic",
	} {
		if strings.Contains(body, needle) {
			t.Errorf("agent-internal metadata %q must not appear in the body", needle)
		}
	}

	// The fingerprint appears exactly once: in the hidden marker comment.
	// (The old metadata table repeated it as a human-visible cell.)
	if n := strings.Count(body, f.Fingerprint); n != 1 {
		t.Errorf("fingerprint appears %d times, want exactly 1 (the line-1 marker)", n)
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
	f := domain.Finding{
		Fingerprint: "fp123",
		Title:       "t",
		ReproPath:   "/nonexistent/repro/dir",
	}
	body := renderIssueBody(f, "")

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
	findings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil || len(findings) == 0 {
		t.Fatal("could not list findings")
	}
	findings[0].ReproPath = "/nonexistent/repro/dir"
	if _, err := st.UpsertFinding(ctx, findings[0]); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	ft := newFakeTracker()
	ft.createKeys = []tracker.IssueKey{"5"}
	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
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

	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		ReproPath:   dir,
	}
	body := renderIssueBody(f, "")

	if !strings.Contains(body, "<details><summary>Reproduction</summary>") {
		t.Error("Reproduction block missing")
	}
	if !strings.Contains(body, "truncated by bugbot") {
		t.Error("truncation marker missing from oversized repro section")
	}
}

// TestRenderIssueBody_NoFixPatch confirms fix patch section is absent when FixPatch is empty.
func TestRenderIssueBody_NoFixPatch(t *testing.T) {
	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		FixPatch:    "",
	}
	body := renderIssueBody(f, "")
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
	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		FixPatch:    maliciousPatch,
	}
	body := renderIssueBody(f, "")

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

	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		ReproPath:   dir,
	}
	body := renderIssueBody(f, "")

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
	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		Reasoning:   maliciousReasoning,
	}
	body := renderIssueBody(f, "")

	// The raw </details> sequence must appear in the body only as many times
	// as bugbot itself emits it. The sanitizer converts the Reasoning's
	// </details> to &lt;/details so it does not count. This finding has no
	// repro, so the verification trace block is the only bugbot-emitted
	// </details>.
	count := strings.Count(body, "</details>")
	if count > 1 {
		t.Errorf("body contains %d </details> tags; expected ≤1 (only bugbot-emitted ones); the injected </details> from Reasoning was not neutralised", count)
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
	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		FixPatch:    strings.Repeat("x", 30*1024), // 30 KB, over the 20 KB cap
	}
	body := renderIssueBody(f, "")
	if !strings.Contains(body, "truncated by bugbot") {
		t.Error("truncation marker missing from oversized FixPatch body")
	}
}

// TestRenderIssueBody_OversizedDescription confirms that a very large
// Description is capped at ~10 KB with the truncation marker.
func TestRenderIssueBody_OversizedDescription(t *testing.T) {
	f := domain.Finding{
		Fingerprint: "fp",
		Title:       "t",
		Description: strings.Repeat("d", 15*1024), // 15 KB, over the 10 KB cap
	}
	body := renderIssueBody(f, "")
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
	f := domain.Finding{
		Fingerprint: fp,
		Title:       strings.Repeat("T", 1000),
		Description: strings.Repeat("D", 20*1024),
		FixPatch:    strings.Repeat("P", 25*1024),
		Reasoning:   strings.Repeat("R", 40*1024),
	}
	body := renderIssueBody(f, "")

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

// TestTruncateUTF8 pins the rune-boundary walk-back: when s is split mid-rune
// at byte offset `max`, the helper walks back to the start of the rune, so the
// returned slice is always valid UTF-8. The behavior is the precondition for
// every byte-cap truncation site in renderIssueBody and renderReproSection
// (description, fix patch, reasoning, repro per-file, repro total, plus the
// belt-and-braces body guard) — model-authored content cut at a raw byte
// offset would otherwise land in the issue body as a U+FFFD replacement.
func TestTruncateUTF8(t *testing.T) {
	t.Run("ascii-shorter-than-max", func(t *testing.T) {
		if got := truncateUTF8("hello", 10); got != "hello" {
			t.Errorf("truncateUTF8(%q, 10) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("ascii-truncated-exact", func(t *testing.T) {
		if got := truncateUTF8("hello", 5); got != "hello" {
			t.Errorf("truncateUTF8(%q, 5) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("multibyte-split-mid-rune", func(t *testing.T) {
		// Each 锁 is 3 bytes. 50 runes (150 bytes) truncated to 10 bytes
		// would split a rune in half without the walk-back, producing
		// invalid UTF-8. The helper walks back to byte 9 (the last byte
		// boundary inside the third rune) and returns 3 complete runes.
		in := strings.Repeat("锁", 50)
		got := truncateUTF8(in, 10)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateUTF8 produced invalid UTF-8: %q (bytes %v)", got, []byte(got))
		}
		if len(got) > 10 {
			t.Errorf("truncateUTF8 result length = %d, want <= 10", len(got))
		}
		// 3 runes * 3 bytes/rune = 9 bytes (the rune start at byte 9 is
		// the 4th rune, so we keep 3 complete runes).
		wantRunes := 3
		if r := []rune(got); len(r) != wantRunes {
			t.Errorf("rune count = %d, want %d (3 complete 锁 runes)", len(r), wantRunes)
		}
	})

	t.Run("multibyte-exact-boundary", func(t *testing.T) {
		// 30 runes * 3 bytes = 90 bytes; cap at 90 should not walk back.
		in := strings.Repeat("锁", 30)
		got := truncateUTF8(in, 90)
		if got != in {
			t.Errorf("truncateUTF8 exact-boundary = %q, want unchanged", got)
		}
	})

	t.Run("empty-string", func(t *testing.T) {
		if got := truncateUTF8("", 5); got != "" {
			t.Errorf("truncateUTF8 empty = %q, want empty", got)
		}
	})

	t.Run("zero-max", func(t *testing.T) {
		if got := truncateUTF8("hello", 0); got != "" {
			t.Errorf("truncateUTF8 zero-max = %q, want empty", got)
		}
	})
}

// TestApplyPublish_UpdateStaleRecreate pins the bugbot-09m resilience path:
// when the update for an existing issue fails with tracker.ErrIssueGone, the
// local published_issues row is stale. The reconciler must log it, delete
// the stale row, create a fresh issue, record the new key, and CONTINUE
// with the rest of the plan — never abort the run on a deleted/transferred
// issue.
func TestApplyPublish_UpdateStaleRecreate(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	// Pre-record an open published row pointing at a now-deleted issue.
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "50", "open", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Bump the finding's UpdatedAt so the planner produces an update action
	// (it only updates when the finding is newer than the published row).
	findings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil || len(findings) == 0 {
		t.Fatalf("list: %v", err)
	}
	findings[0].UpdatedAt = time.Now().Add(time.Hour)
	if _, err := st.UpsertFinding(ctx, findings[0]); err != nil {
		t.Fatalf("bump: %v", err)
	}

	// The update on the stale key 50 fails with ErrIssueGone; the recreate
	// lands on a fresh issue 123.
	ft := newFakeTracker()
	ft.updateErr = errTrackerGone("50")
	ft.createKeys = []tracker.IssueKey{"123"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish must NOT abort on ErrIssueGone: %v", err)
	}

	// The stale row must have been deleted and a new row with the new key
	// recorded.
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published: %v", err)
	}
	if pi.IssueKey != "123" {
		t.Errorf("issue_key = %q, want 123 (recreated)", pi.IssueKey)
	}
	if pi.State != "open" {
		t.Errorf("state = %q, want open", pi.State)
	}

	// The update on the stale 50 and a create for the new 123 must have
	// both been attempted.
	if n := len(ft.callsOf("update")); n != 1 {
		t.Errorf("expected 1 update attempt on stale 50, got %d", n)
	}
	if n := len(ft.callsOf("create")); n != 1 {
		t.Errorf("expected 1 recreate, got %d", n)
	}

	// Summary must report stale=1 and created=1.
	out := buf.String()
	if !strings.Contains(out, "stale=1") {
		t.Errorf("summary must include stale=1; got: %s", out)
	}
	if !strings.Contains(out, "created=1") {
		t.Errorf("summary must include created=1; got: %s", out)
	}
}

// TestApplyPublish_UpdateStaleRecreate_404 covers the transferred/renamed
// variant: the adapter classifies an HTTP 404 identically to a 410
// (tracker.ErrIssueGone), and the reconciler must recover identically.
func TestApplyPublish_UpdateStaleRecreate_404(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "77", "open", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	findings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil || len(findings) == 0 {
		t.Fatalf("list: %v", err)
	}
	findings[0].UpdatedAt = time.Now().Add(time.Hour)
	if _, err := st.UpsertFinding(ctx, findings[0]); err != nil {
		t.Fatalf("bump: %v", err)
	}
	ft := newFakeTracker()
	ft.updateErr = fmt.Errorf("github: update issue 77: %w: HTTP 404 Not Found", tracker.ErrIssueGone)
	ft.createKeys = []tracker.IssueKey{"456"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish must NOT abort on ErrIssueGone: %v", err)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pi.IssueKey != "456" {
		t.Errorf("issue_key = %q, want 456 (recreated)", pi.IssueKey)
	}
	if !strings.Contains(buf.String(), "stale=1") {
		t.Errorf("summary must include stale=1; got: %s", buf.String())
	}
}

// TestApplyPublish_CloseStaleDrop pins the close path: when the close fails
// with tracker.ErrIssueGone, the issue is already gone. The reconciler must
// log it, drop the row, and continue (no error, no abort).
func TestApplyPublish_CloseStaleDrop(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "88", "open", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}

	ft := newFakeTracker()
	ft.closeErr = fmt.Errorf("github: close issue 88: %w: HTTP 410 Gone", tracker.ErrIssueGone)

	cfg := config.Publish{TierMin: 2, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish must NOT abort on close ErrIssueGone: %v", err)
	}
	// The comment landed first (close ordering), then the close hit the
	// gone issue.
	if n := len(ft.callsOf("comment")); n != 1 {
		t.Errorf("expected the auto-close comment before the close, got %d", n)
	}
	// Row must be gone (close is a no-op success on a gone issue).
	if _, err := st.GetPublishedIssue(ctx, f.Fingerprint); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row must be deleted after stale close; got err = %v", err)
	}
	if !strings.Contains(buf.String(), "stale=1") {
		t.Errorf("summary must include stale=1; got: %s", buf.String())
	}
}

// TestApplyPublish_UpdateRecordsSuccess pins the bugbot-09m success path: a
// successful update must record the published row (UpsertPublishedIssue) and
// count updated++, so the planner converges instead of re-pushing the same
// issue every cycle. The merge briefly dropped these lines.
func TestApplyPublish_UpdateRecordsSuccess(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "60", "open", ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Make the finding newer than the published row so the planner updates.
	findings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil || len(findings) == 0 {
		t.Fatalf("list: %v", err)
	}
	findings[0].UpdatedAt = time.Now().Add(time.Hour)
	if _, err := st.UpsertFinding(ctx, findings[0]); err != nil {
		t.Fatalf("bump: %v", err)
	}
	ft := newFakeTracker()
	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}
	updates := ft.callsOf("update")
	if len(updates) != 1 || updates[0].key != "60" {
		t.Errorf("expected 1 update on issue 60, got %v", updates)
	}
	if out := buf.String(); !strings.Contains(out, "updated=1") {
		t.Errorf("summary must report updated=1 (success path records the update); got: %s", out)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published: %v", err)
	}
	if pi.IssueKey != "60" || pi.State != "open" {
		t.Errorf("published row = %q %q, want 60 open", pi.IssueKey, pi.State)
	}
}

// TestPlanPublish_AdoptsDriftedRediscovery verifies the cross-scan fuzzy dedup:
// an open finding with no published row that is the same defect as an already-
// published finding (same file, nearby line, similar description) adopts the
// existing issue instead of creating a duplicate. Findings in a different file,
// beyond the line window, or with an unrelated description still create.
func TestPlanPublish_AdoptsDriftedRediscovery(t *testing.T) {
	mkF := func(fp, file string, line int, desc string) domain.Finding {
		return domain.Finding{
			Fingerprint: fp, Title: "t-" + fp, Description: desc,
			File: file, Line: line, Tier: domain.TierVerified, Status: domain.StatusOpen,
		}
	}
	const sharedDesc = "cfg pointer may be nil and is dereferenced without a guard in handler"
	anchor := mkF("fp_anchor", "a.go", 10, sharedDesc)
	drifted := mkF("fp_drift", "a.go", 12, "the cfg pointer is dereferenced without a nil guard in handler")
	otherFile := mkF("fp_otherfile", "b.go", 10, sharedDesc)
	farLine := mkF("fp_far", "a.go", 100, sharedDesc)
	unrelated := mkF("fp_unrel", "a.go", 11, "memory leak when the parser fails to release the buffer handle")

	published := map[string]store.PublishedIssue{
		"fp_anchor": {Fingerprint: "fp_anchor", IssueKey: "7", Tracker: "github", State: store.IssueStateOpen},
	}
	open := []domain.Finding{anchor, drifted, otherFile, farLine, unrelated}

	act := map[string]publishAction{}
	for _, a := range planPublish(open, nil, nil, nil, published, 2, false) {
		switch v := a.(type) {
		case publishAdopt:
			act[v.finding.Fingerprint] = v
		case publishCreate:
			act[v.finding.Fingerprint] = v
		case publishSkip:
			act[v.finding.Fingerprint] = v
		case publishUpdate:
			act[v.finding.Fingerprint] = v
		}
	}

	ad, ok := act["fp_drift"].(publishAdopt)
	if !ok {
		t.Fatalf("fp_drift: want publishAdopt, got %T", act["fp_drift"])
	}
	if ad.issueKey != "7" {
		t.Errorf("adopt issue = %q, want 7", ad.issueKey)
	}

	for _, fp := range []string{"fp_otherfile", "fp_far", "fp_unrel"} {
		if _, ok := act[fp].(publishCreate); !ok {
			t.Errorf("%s: want publishCreate (no adopt), got %T", fp, act[fp])
		}
	}

	if _, ok := act["fp_anchor"].(publishSkip); !ok {
		t.Errorf("fp_anchor: want publishSkip, got %T", act["fp_anchor"])
	}
}

// TestSanitizeControlChars verifies C0 control characters are stripped except
// the meaningful Markdown whitespace (tab, newline, carriage return), and that
// clean input is returned unchanged.
func TestSanitizeControlChars(t *testing.T) {
	clean := "hello\tworld\nsecond line\r\nthird"
	if got := sanitizeControlChars(clean); got != clean {
		t.Errorf("clean text mutated: got %q want %q", got, clean)
	}

	// NUL plus other C0 controls (bell, ESC, vertical tab, form feed) removed.
	if got := sanitizeControlChars("a\x00b\x07c\x1bd\x0be\x0cf"); got != "abcdef" {
		t.Errorf("control strip: got %q want %q", got, "abcdef")
	}

	// Whitespace controls survive even when other controls are present.
	if got := sanitizeControlChars("x\x00\ty\n\rz"); got != "x\ty\n\rz" {
		t.Errorf("whitespace not preserved: got %q", got)
	}
}

// TestRenderIssueBody_StripsControlChars verifies model-authored content with a
// NUL byte (the exec-killing case) and other C0 controls produces a body with
// no such bytes — so the body can never crash gh's forkExec with EINVAL.
func TestRenderIssueBody_StripsControlChars(t *testing.T) {
	f := domain.Finding{
		Fingerprint: "abc123",
		Title:       "title",
		Description: "desc with NUL\x00 and bell\x07 here",
		Reasoning:   "trace\x00line",
		Severity:    "low",
		Tier:        2,
		Lens:        "x",
		File:        "a.go",
		Line:        1,
	}
	body := renderIssueBody(f, "")
	if strings.IndexByte(body, 0) >= 0 {
		t.Fatalf("body contains NUL after render")
	}
	for _, r := range body {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			t.Fatalf("body contains C0 control %#x after render", r)
		}
	}
	if !strings.Contains(body, "desc with NUL and bell here") {
		t.Errorf("description text not preserved: %q", body)
	}
}

// TestApplyPublish_StripsNULFromModelText is the regression test for the
// fork/exec EINVAL crash: a finding whose model-authored fields carry a NUL
// byte must publish, and the rendered body handed to the tracker must
// contain no NUL. (The TITLE strip now lives in the GitHub adapter, whose
// exec transport is the thing a NUL crashes — see internal/tracker/github's
// TestCreateIssue_SanitizesTitle for that half.)
func TestApplyPublish_StripsNULFromModelText(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f := domain.Finding{
		Fingerprint: domain.Fingerprint("race", "x.go", "7|boom"),
		Title:       "boom\x00 title",
		Description: "desc\x00ription",
		Reasoning:   "trace with NUL\x00 byte",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "race",
		File:        "x.go",
		Line:        7,
		CommitSHA:   "c1abc",
	}
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	ft := newFakeTracker()
	ft.createKeys = []tracker.IssueKey{"42"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	creates := ft.callsOf("create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(creates))
	}
	if strings.IndexByte(creates[0].body, 0) >= 0 {
		t.Errorf("rendered body contains NUL (renderIssueBody must sanitize)")
	}
	if !strings.Contains(creates[0].body, "description") {
		t.Errorf("description text not preserved in body")
	}
}

// TestRenderIssueBody_SanitizeAndTruncateJointly exercises the subtle
// interaction between sanitizeControlChars (whole-body) and the maxBody
// belt-and-braces truncation: an oversized body (> maxBody) that also contains
// C0 control bytes must still (a) lose every control byte, (b) preserve the
// line-1 fingerprint marker and the attribution footer, and (c) stay under
// GitHub's 65 536-char hard limit. Sparse control bytes near the start of each
// model field survive the per-field caps but are stripped by the whole-body
// sanitize; enough plain content remains to keep the body over maxBody so the
// truncation path runs jointly with the sanitize.
func TestRenderIssueBody_SanitizeAndTruncateJointly(t *testing.T) {
	f := domain.Finding{
		Fingerprint: "fp1234567890abcdef",
		Title:       "oversized",
		Description: "x\x00y\x07z" + strings.Repeat("d", 11000),
		FixPatch:    "p\x00q" + strings.Repeat("P", 21000),
		Reasoning:   "r\x00s\x1bt" + strings.Repeat("R", 31000),
		Severity:    "low",
		Tier:        2,
		Lens:        "x",
		File:        "a.go",
		Line:        1,
	}
	body := renderIssueBody(f, "")

	// Truncation must have run (oversized even after sanitize).
	if !strings.Contains(body, "body truncated by bugbot") {
		t.Fatalf("expected belt-and-braces truncation to run; body len=%d", len(body))
	}
	// No control bytes survive (the joint invariant).
	for _, r := range body {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			t.Fatalf("body contains C0 control %#x after sanitize+truncate", r)
		}
	}
	// Line-1 fingerprint marker preserved (load-bearing for recovery).
	wantMarker := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	if !strings.HasPrefix(body, wantMarker) {
		t.Errorf("fingerprint marker not preserved after truncation")
	}
	// Attribution footer preserved.
	if !strings.HasSuffix(body, "Filed by Bugbot — automated finding; verify before acting.") {
		t.Errorf("attribution footer not preserved after truncation")
	}
	// Under GitHub's hard limit.
	if len(body) >= 65536 {
		t.Errorf("body length %d exceeds GitHub 65536 hard limit", len(body))
	}
}

// ---- Backsync + Reopen tests (bugbot-fchv) ----

// TestBacksync_HumanClosedDismissesFinding: an issue closed manually on the
// tracker must dismiss the local open finding (suppression memory), mark the
// published row closed, and must NOT push a body or post a comment in the
// same run -- the human's close stands as-is.
func TestBacksync_HumanClosedDismissesFinding(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "77", "open", ""); err != nil {
		t.Fatalf("seed published: %v", err)
	}

	ft := newFakeTracker()
	ft.listByState["closed"] = []tracker.Issue{{Key: "77", State: "closed"}}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	// No body push, close, or comment must have been issued for issue 77.
	for _, op := range []string{"update", "reopen", "close", "comment"} {
		if n := len(ft.callsOf(op)); n != 0 {
			t.Errorf("expected zero %s calls on a human-closed issue, got %d", op, n)
		}
	}

	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if got.Status != domain.StatusDismissed {
		t.Errorf("finding status = %q, want dismissed", got.Status)
	}

	// Re-upserting the same finding must not resurrect it as open: the
	// suppression row backsync created makes UpsertFinding force it back to
	// dismissed (store/findings.go:164-170).
	reupserted, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: f.Fingerprint,
		Title:       f.Title,
		Severity:    f.Severity,
		Tier:        f.Tier,
		Status:      domain.StatusOpen,
		Lens:        f.Lens,
		File:        f.File,
		Line:        f.Line,
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if reupserted.Status != domain.StatusDismissed {
		t.Errorf("re-upsert status = %q, want dismissed (suppression must stick)", reupserted.Status)
	}

	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.State != store.IssueStateClosed {
		t.Errorf("published state = %q, want closed", pi.State)
	}
	if !strings.Contains(buf.String(), "backsynced issue 77") {
		t.Errorf("expected backsync summary line, got: %s", buf.String())
	}
}

// TestBacksync_Idempotent: a second run over an already-backsynced store
// makes zero backsync listing calls (needsBacksync sees only closed rows)
// and zero further mutations.
func TestBacksync_Idempotent(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "77", "open", ""); err != nil {
		t.Fatalf("seed published: %v", err)
	}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}

	ft1 := newFakeTracker()
	ft1.listByState["closed"] = []tracker.Issue{{Key: "77", State: "closed"}}
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, ft1, st, cfg, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Second run: only the repo-URL read is expected. If backsync tried to
	// list closed issues again, or mutate anything, the call log would show
	// it.
	ft2 := newFakeTracker()
	var buf2 strings.Builder
	if err := runPublish(ctx, &buf2, ft2, st, cfg, 2, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(ft2.calls) != 1 || ft2.calls[0].op != "repoURL" {
		t.Errorf("second run should make exactly 1 tracker call (repoURL), got %v", ft2.calls)
	}
	if strings.Contains(buf2.String(), "backsynced issue") {
		t.Errorf("second run should not backsync again: %s", buf2.String())
	}

	got, err := st.GetFindingByFingerprint(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if got.Status != domain.StatusDismissed {
		t.Errorf("finding status = %q, want dismissed (unchanged)", got.Status)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.State != store.IssueStateClosed {
		t.Errorf("published state = %q, want closed (unchanged)", pi.State)
	}
}

// TestBacksync_FixedFindingHumanClosed: a finding that was already fixed
// locally, whose issue is closed manually on the tracker before bugbot's
// own close runs, must land with a closed row and no comment/close call --
// backsync beats planPublish's close action to the row.
func TestBacksync_FixedFindingHumanClosed(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "90", "open", ""); err != nil {
		t.Fatalf("seed published: %v", err)
	}
	if err := st.MarkFixed(ctx, f.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}

	ft := newFakeTracker()
	ft.listByState["closed"] = []tracker.Issue{{Key: "90", State: "closed"}}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	if n := len(ft.callsOf("close")); n != 0 {
		t.Errorf("expected zero close calls, got %d", n)
	}
	if n := len(ft.callsOf("comment")); n != 0 {
		t.Errorf("expected zero comment calls, got %d", n)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.State != store.IssueStateClosed {
		t.Errorf("published state = %q, want closed", pi.State)
	}
	if !strings.Contains(buf.String(), "finding already fixed") {
		t.Errorf("expected 'finding already fixed' in output, got: %s", buf.String())
	}
}

// TestPlanPublish_DismissedNeverReopened pins that dismissal wins: a
// dismissed finding whose published row is closed must never produce a
// publishReopen (or any) action -- planPublish's open-findings loop only
// ever iterates the `open` slice, and a backsync-dismissed finding is no
// longer in it.
func TestPlanPublish_DismissedNeverReopened(t *testing.T) {
	dismissed := []domain.Finding{makeDismissedFinding("fp1")}
	published := map[string]store.PublishedIssue{
		"fp1": makePublishedIssue("fp1", "77", store.IssueStateClosed, time.Now()),
	}
	actions := planPublish(nil, nil, dismissed, nil, published, 2, true)
	for _, a := range actions {
		if _, ok := a.(publishReopen); ok {
			t.Fatalf("dismissed finding must never be reopened, got actions: %#v", actions)
		}
	}
	if len(actions) != 0 {
		t.Errorf("expected zero actions for a dismissed finding with an already-closed row, got %#v", actions)
	}
}

// TestApplyPublish_RegressionReopen: an open finding (re-detected regression)
// whose published row is closed (bugbot closed it previously) must produce
// exactly one reopen carrying a fresh body, followed by a comment, and the
// row must flip to open.
func TestApplyPublish_RegressionReopen(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "65", "closed", ""); err != nil {
		t.Fatalf("seed published: %v", err)
	}

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	reopens := ft.callsOf("reopen")
	if len(reopens) != 1 {
		t.Fatalf("expected exactly 1 reopen call, got %d: %v", len(reopens), ft.calls)
	}
	if reopens[0].key != "65" {
		t.Errorf("reopen key = %q, want 65", reopens[0].key)
	}
	if reopens[0].body == "" {
		t.Errorf("reopen must carry a non-empty body")
	}
	comments := ft.callsOf("comment")
	if len(comments) != 1 {
		t.Errorf("expected exactly 1 comment call, got %d", len(comments))
	}
	// Comment must follow the reopen.
	reopenIdx := ft.indexOfOp("reopen", "65")
	commentIdx := ft.indexOfOp("comment", "65")
	if reopenIdx == -1 || commentIdx == -1 || reopenIdx > commentIdx {
		t.Errorf("reopen must precede the comment: reopenIdx=%d commentIdx=%d", reopenIdx, commentIdx)
	}

	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.State != store.IssueStateOpen {
		t.Errorf("published state = %q, want open", pi.State)
	}
	if !strings.Contains(buf.String(), "reopened=1") {
		t.Errorf("expected reopened=1 in summary, got: %s", buf.String())
	}
}

// TestApplyPublish_RegressionReopen_StaleRecreate: ErrIssueGone on the
// reopen means the issue is gone; the stale row must be dropped and a fresh
// issue created (mirroring publishUpdate's stale path).
func TestApplyPublish_RegressionReopen_StaleRecreate(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "65", "closed", ""); err != nil {
		t.Fatalf("seed published: %v", err)
	}

	ft := newFakeTracker()
	ft.reopenErr = fmt.Errorf("github: reopen issue 65: %w: HTTP 404 Not Found", tracker.ErrIssueGone)
	ft.createKeys = []tracker.IssueKey{"200"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish must NOT abort on ErrIssueGone: %v", err)
	}

	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if pi.IssueKey != "200" {
		t.Errorf("issue_key = %q, want 200 (recreated)", pi.IssueKey)
	}
	if pi.State != store.IssueStateOpen {
		t.Errorf("published state = %q, want open", pi.State)
	}
	if !strings.Contains(buf.String(), "stale=1") {
		t.Errorf("summary must include stale=1; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "created=1") {
		t.Errorf("summary must include created=1; got: %s", buf.String())
	}
}

// TestApplyPublish_BacksyncReopenDryRun: dry-run prints backsync and reopen
// intents but performs zero tracker writes and zero store mutations.
func TestApplyPublish_BacksyncReopenDryRun(t *testing.T) {
	ctx := context.Background()
	st, f1 := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f1.Fingerprint, "github", "77", "open", ""); err != nil {
		t.Fatalf("seed published f1: %v", err)
	}

	f2, err := st.UpsertFinding(ctx, domain.Finding{
		Fingerprint: domain.Fingerprint("leak", "y.go", "3|leak"),
		Title:       "leak",
		Severity:    "medium",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "leak",
		File:        "y.go",
		Line:        3,
	})
	if err != nil {
		t.Fatalf("seed f2: %v", err)
	}
	if err := st.UpsertPublishedIssue(ctx, f2.Fingerprint, "github", "65", "closed", ""); err != nil {
		t.Fatalf("seed published f2: %v", err)
	}

	ft := newFakeTracker()
	ft.listByState["closed"] = []tracker.Issue{{Key: "77", State: "closed"}}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, true /* dry-run */); err != nil {
		t.Fatalf("runPublish dry-run: %v", err)
	}

	if writes := ft.writes(); len(writes) != 0 {
		t.Errorf("dry-run should not make write calls; got: %v", writes)
	}
	out := buf.String()
	if !strings.Contains(out, "dry-run: backsync issue 77") {
		t.Errorf("expected dry-run backsync intent for 77, got: %s", out)
	}
	if !strings.Contains(out, "dry-run: reopen issue 65") {
		t.Errorf("expected dry-run reopen intent for 65, got: %s", out)
	}
	// Negative: issue 77 was just backsync-dismissed in the same pass; it
	// must never also be planned for reopen (that would mean planPublish's
	// open loop still saw the dismissed finding sitting on a "closed" row
	// -- the dry-run/real-run reconciliation divergence bug).
	if strings.Contains(out, "reopen issue 77") {
		t.Errorf("issue 77 was backsync-dismissed and must never also be reopened, got: %s", out)
	}
	if !strings.Contains(out, "reopened=1") {
		t.Errorf("expected exactly reopened=1 (issue 65 only), got: %s", out)
	}

	// Zero store mutations: both findings still open, both rows unchanged.
	got1, err := st.GetFindingByFingerprint(ctx, f1.Fingerprint)
	if err != nil {
		t.Fatalf("get f1: %v", err)
	}
	if got1.Status != domain.StatusOpen {
		t.Errorf("f1 status = %q, want open (dry-run must not mutate)", got1.Status)
	}
	pi1, err := st.GetPublishedIssue(ctx, f1.Fingerprint)
	if err != nil {
		t.Fatalf("get pi1: %v", err)
	}
	if pi1.State != store.IssueStateOpen {
		t.Errorf("pi1 state = %q, want open (dry-run must not mutate)", pi1.State)
	}
	pi2, err := st.GetPublishedIssue(ctx, f2.Fingerprint)
	if err != nil {
		t.Fatalf("get pi2: %v", err)
	}
	if pi2.State != store.IssueStateClosed {
		t.Errorf("pi2 state = %q, want closed (dry-run must not mutate)", pi2.State)
	}
}

// TestApplyPublish_NoBacksyncWhenNoOpenRows: when no published row is open
// or closing, backsync must make zero tracker calls -- in particular it
// must never call the closed-issues listing.
func TestApplyPublish_NoBacksyncWhenNoOpenRows(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)

	// No published rows at all: the seeded finding has no row yet, so
	// planPublish will plan a create -- but backsync itself must not list,
	// since there is nothing to check.
	ft := newFakeTracker()
	ft.createKeys = []tracker.IssueKey{"10"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}
	if !strings.Contains(buf.String(), "created=1") {
		t.Errorf("expected created=1, got: %s", buf.String())
	}
	if n := len(ft.callsOf("list")); n != 0 {
		t.Errorf("backsync must not list issues when no row is open/closing; got %d list calls", n)
	}
}

// ---- Per-action error resilience + body_hash no-op PATCH elimination (bugbot-klaj) ----

// seedSecondOpenFinding adds a second open T2 finding (no published row) to
// st, distinct from the one setupPublishStore seeds, so a test can exercise
// two independent plan actions (e.g. a create alongside a close) in one run.
func seedSecondOpenFinding(t *testing.T, ctx context.Context, st *store.Store) domain.Finding {
	t.Helper()
	f := domain.Finding{
		Fingerprint: domain.Fingerprint("race", "y.go", fmt.Sprintf("%d|%s", 9, "kaboom")),
		Title:       "kaboom",
		Description: "desc-a",
		Reasoning:   "trace-a",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "race",
		File:        "y.go",
		Line:        9,
		CommitSHA:   "c2def",
	}
	f, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("seed second finding: %v", err)
	}
	return f
}

// TestApplyPublish_ActionFailureDoesNotBlockRestOfPlan pins bugbot-klaj's
// per-action error resilience: a validation failure on creating one issue
// must not drop the rest of the plan. The subsequent close for a second,
// unrelated finding must still apply, and the run must return an aggregate
// error naming the one failure.
func TestApplyPublish_ActionFailureDoesNotBlockRestOfPlan(t *testing.T) {
	ctx := context.Background()
	st, fClose := setupPublishStore(t)

	// fClose becomes the close action: mark it fixed with an existing open row.
	if err := st.UpsertPublishedIssue(ctx, fClose.Fingerprint, "github", "77", store.IssueStateOpen, "seed-hash"); err != nil {
		t.Fatalf("seed published close target: %v", err)
	}
	if err := st.MarkFixed(ctx, fClose.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}

	// fCreate is a second, still-open finding with no published row -> the
	// planner emits a publishCreate action for it, processed BEFORE the
	// close (planPublish orders create/update/skip actions for `open`
	// findings first, then close actions for fixed/dismissed/superseded).
	seedSecondOpenFinding(t, ctx, st)

	ft := newFakeTracker()
	ft.createErr = errors.New("github: create issue: HTTP 422: Validation Failed")

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	err := runPublish(ctx, &buf, ft, st, cfg, 2, false)
	if err == nil {
		t.Fatal("expected an aggregate error naming the create failure")
	}
	if !strings.Contains(err.Error(), "1 action(s) failed") {
		t.Errorf("error should name 1 failure; got: %v", err)
	}

	// The close must still have applied despite the create failure.
	if closes := ft.callsOf("close"); len(closes) != 1 || closes[0].key != "77" {
		t.Fatalf("expected the close to still apply, got %v", closes)
	}
	pi, gerr := st.GetPublishedIssue(ctx, fClose.Fingerprint)
	if gerr != nil {
		t.Fatalf("get published close target: %v", gerr)
	}
	if pi.State != store.IssueStateClosed {
		t.Errorf("close target state = %q, want closed", pi.State)
	}

	out := buf.String()
	if !strings.Contains(out, "failed=1") {
		t.Errorf("summary must include failed=1; got: %s", out)
	}
	if !strings.Contains(out, "closed=1") {
		t.Errorf("summary must include closed=1; got: %s", out)
	}
	if !strings.Contains(out, "failed create for") {
		t.Errorf("expected a 'failed create for ...' log line; got: %s", out)
	}
}

// TestApplyPublish_RateLimitAbortsRemainingPlan pins bugbot-klaj's other
// classification branch: an error wrapping tracker.ErrRateLimited must
// abort the rest of the plan (the adapter has already exhausted its retry
// budget by the time this surfaces) rather than being treated as an
// action-scoped failure like a validation error — so at most ONE pending
// tombstone exists, not one per remaining action.
func TestApplyPublish_RateLimitAbortsRemainingPlan(t *testing.T) {
	ctx := context.Background()
	st, fClose := setupPublishStore(t)

	if err := st.UpsertPublishedIssue(ctx, fClose.Fingerprint, "github", "77", store.IssueStateOpen, "seed-hash"); err != nil {
		t.Fatalf("seed published close target: %v", err)
	}
	if err := st.MarkFixed(ctx, fClose.Fingerprint); err != nil {
		t.Fatalf("mark fixed: %v", err)
	}

	// Processed before the close, same ordering as the resilience test above.
	seedSecondOpenFinding(t, ctx, st)

	ft := newFakeTracker()
	ft.createErr = errTrackerRateLimited()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	err := runPublish(ctx, &buf, ft, st, cfg, 2, false)
	if err == nil {
		t.Fatal("expected an abort error on rate limit")
	}
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Errorf("returned error should wrap tracker.ErrRateLimited; got: %v", err)
	}

	// The close for the fixed finding must NOT have been attempted -- it's
	// later in the plan than the rate-limited create.
	if n := len(ft.callsOf("close")) + len(ft.callsOf("comment")); n != 0 {
		t.Errorf("close must not be attempted after a rate-limit abort; calls: %v", ft.calls)
	}
	pi, gerr := st.GetPublishedIssue(ctx, fClose.Fingerprint)
	if gerr != nil {
		t.Fatalf("get published close target: %v", gerr)
	}
	if pi.State != store.IssueStateOpen {
		t.Errorf("close target state = %q, want still open (close never attempted)", pi.State)
	}
	// The abort fired on the FIRST action: exactly one create attempt.
	if creates := ft.callsOf("create"); len(creates) != 1 {
		t.Errorf("expected exactly 1 create attempt before the abort, got %d", len(creates))
	}
}

// TestApplyPublish_BodyHash_MetadataTouchSkipsPatch pins bugbot-klaj's no-op
// push elimination: a metadata-only finding touch (any UpsertFinding call
// that leaves the rendered body unchanged -- the impact sweep,
// AddCorroboratingLenses, AppendFindingSites analogue exercised here is a
// bare re-upsert of the same finding content) must not cost a body push. The
// published row's updated_at still advances so the planner converges to
// publishSkip on the next cycle instead of replanning the same no-op update
// forever.
func TestApplyPublish_BodyHash_MetadataTouchSkipsPatch(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}

	ft1 := newFakeTracker()
	ft1.createKeys = []tracker.IssueKey{"42"}
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, ft1, st, cfg, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	pi1, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published after create: %v", err)
	}
	if pi1.BodyHash == "" {
		t.Fatal("BodyHash must be recorded after create")
	}

	// Metadata-only touch: re-upsert the SAME finding content. UpsertFinding
	// always stamps updated_at=now on the update path, so this alone makes
	// finding.UpdatedAt > published.UpdatedAt and plans a publishUpdate --
	// but the rendered body is byte-identical.
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("metadata touch: %v", err)
	}

	ft2 := newFakeTracker()
	var buf2 strings.Builder
	if err := runPublish(ctx, &buf2, ft2, st, cfg, 2, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n := len(ft2.callsOf("update")); n != 0 {
		t.Errorf("expected zero update calls on a metadata-only touch, got %d", n)
	}
	if !strings.Contains(buf2.String(), "unchanged issue 42") {
		t.Errorf("expected an 'unchanged issue 42' log line; got: %s", buf2.String())
	}
	if !strings.Contains(buf2.String(), "skipped=1") {
		t.Errorf("second run must count the no-op as skipped, not updated; got: %s", buf2.String())
	}

	pi2, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published after second run: %v", err)
	}
	if !pi2.UpdatedAt.After(pi1.UpdatedAt) {
		t.Errorf("published_issues.updated_at must advance even without a push: %s -> %s", pi1.UpdatedAt, pi2.UpdatedAt)
	}
	if pi2.BodyHash != pi1.BodyHash {
		t.Errorf("BodyHash must stay the same when the body did not change: %q -> %q", pi1.BodyHash, pi2.BodyHash)
	}

	// Third run: published.updated_at is now newer than the finding's
	// updated_at, so the planner converges to publishSkip with zero writes.
	ft3 := newFakeTracker()
	var buf3 strings.Builder
	if err := runPublish(ctx, &buf3, ft3, st, cfg, 2, false); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if !strings.Contains(buf3.String(), "skipped=1") {
		t.Errorf("third run should plan publishSkip; got: %s", buf3.String())
	}
	if writes := ft3.writes(); len(writes) != 0 {
		t.Errorf("third run should make zero write calls (pure skip); got: %v", writes)
	}
}

// TestApplyPublish_BodyHash_RealChangeStillPatches pins the other half of
// bugbot-klaj's no-op push elimination: a genuine content change
// (Description, which renderIssueBody reads) must still produce exactly one
// body push, and the stored hash must be refreshed to match the new body.
func TestApplyPublish_BodyHash_RealChangeStillPatches(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}

	ft1 := newFakeTracker()
	ft1.createKeys = []tracker.IssueKey{"42"}
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, ft1, st, cfg, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	pi1, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published after create: %v", err)
	}

	// Real content change: Description drives renderIssueBody's output.
	f.Description = "a completely different description"
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("content change: %v", err)
	}

	ft2 := newFakeTracker()
	var buf2 strings.Builder
	if err := runPublish(ctx, &buf2, ft2, st, cfg, 2, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	updates := ft2.callsOf("update")
	if len(updates) != 1 {
		t.Fatalf("expected exactly 1 update for a real content change, got %d", len(updates))
	}
	body := updates[0].body
	if !strings.Contains(body, "a completely different description") {
		t.Errorf("update body should carry the new description; got: %q", body[:min(len(body), 200)])
	}
	if !strings.Contains(buf2.String(), "updated=1") {
		t.Errorf("second run should count updated=1; got: %s", buf2.String())
	}

	pi2, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published after second run: %v", err)
	}
	if pi2.BodyHash == pi1.BodyHash {
		t.Error("BodyHash must be refreshed after a real content change")
	}
	if pi2.BodyHash != bodyHashHex(body) {
		t.Errorf("stored BodyHash = %q, want sha256 of the pushed body = %q", pi2.BodyHash, bodyHashHex(body))
	}
}

// ---- bugbot-dnqf.3: lean issue body + managed labels ----

// TestManagedLabels pins the desired-label helper: knob gating, the
// severity/tier vocabularies, no label for unknown values, and sorted
// deterministic output.
func TestManagedLabels(t *testing.T) {
	both := config.Publish{SeverityLabels: true, TierLabels: true}
	cases := []struct {
		name string
		f    domain.Finding
		cfg  config.Publish
		want []string
	}{
		{"both knobs", domain.Finding{Severity: "high", Tier: 1}, both, []string{"bugbot:reproduced", "severity:high"}},
		{"severity only", domain.Finding{Severity: "high", Tier: 1}, config.Publish{SeverityLabels: true}, []string{"severity:high"}},
		{"tier only", domain.Finding{Severity: "high", Tier: 1}, config.Publish{TierLabels: true}, []string{"bugbot:reproduced"}},
		{"both off", domain.Finding{Severity: "high", Tier: 1}, config.Publish{}, nil},
		{"unknown severity drops severity label", domain.Finding{Severity: "blocker", Tier: 0}, both, []string{"bugbot:fix-witnessed"}},
		{"unknown tier drops tier label", domain.Finding{Severity: "low", Tier: 9}, both, []string{"severity:low"}},
		{"critical suspected", domain.Finding{Severity: "critical", Tier: 3}, both, []string{"bugbot:suspected", "severity:critical"}},
		{"medium verified", domain.Finding{Severity: "medium", Tier: 2}, both, []string{"bugbot:verified", "severity:medium"}},
	}
	for _, c := range cases {
		if got := managedLabels(c.f, c.cfg); !slices.Equal(got, c.want) {
			t.Errorf("%s: managedLabels = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRenderIssueBody_MetaSurvivesTruncation pins the belt-and-braces guard's
// preserved prefix: a pathological oversized body must keep BOTH machine
// comments — the fingerprint marker on line 1 and the bugbot:meta
// front-matter on line 2 — while staying under GitHub's hard limit.
func TestRenderIssueBody_MetaSurvivesTruncation(t *testing.T) {
	fp := "abcdef1234567890abcdef1234567890"
	f := domain.Finding{
		Fingerprint: fp,
		Title:       strings.Repeat("T", 1000),
		Description: strings.Repeat("D", 20*1024),
		FixPatch:    strings.Repeat("P", 25*1024),
		Reasoning:   strings.Repeat("R", 40*1024),
		Severity:    "high",
		Tier:        1,
		Lens:        "race",
		File:        "x.go",
		Line:        7,
		CommitSHA:   "deadbeef",
	}
	body := renderIssueBody(f, "")

	// The truncation path must actually have run for this test to prove anything.
	if !strings.Contains(body, "body truncated by bugbot") {
		t.Fatal("expected the belt-and-braces truncation to trigger")
	}
	if len(body) >= 65536 {
		t.Errorf("body length %d exceeds GitHub's 65536 char limit", len(body))
	}

	lines := strings.Split(body, "\n")
	if len(lines) < 2 {
		t.Fatal("body has fewer than 2 lines")
	}
	if want := "<!-- bugbot:fp=" + fp + " -->"; lines[0] != want {
		t.Errorf("line 1 = %q, want fingerprint marker %q", lines[0], want)
	}
	m := regexp.MustCompile(`^<!-- bugbot:meta (.+) -->$`).FindStringSubmatch(lines[1])
	if m == nil {
		t.Fatalf("line 2 is not a bugbot:meta comment after truncation: %q", lines[1])
	}
	var meta struct {
		Severity string `json:"severity"`
		Tier     int    `json:"tier"`
		Commit   string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(m[1]), &meta); err != nil {
		t.Fatalf("bugbot:meta payload is not valid JSON after truncation: %v", err)
	}
	if meta.Severity != "high" || meta.Tier != 1 || meta.Commit != "deadbeef" {
		t.Errorf("bugbot:meta fields = %+v, want severity=high tier=1 commit=deadbeef", meta)
	}

	// Attribution footer must still be the last non-empty line.
	trimmed := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if last := trimmed[len(trimmed)-1]; !strings.Contains(last, "Filed by Bugbot") {
		t.Errorf("last non-empty line should be attribution footer; got: %q", last)
	}
}

// TestApplyPublish_CreateAppliesManagedLabels: with both knobs on, the create
// call carries base + severity + tier labels (base first, managed sorted),
// each managed label is ensured (name + pinned color + description) BEFORE
// the create, and the applied managed set is recorded in the store.
func TestApplyPublish_CreateAppliesManagedLabels(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t) // severity high, tier 2

	ft := newFakeTracker()
	ft.createKeys = []tracker.IssueKey{"42"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	creates := ft.callsOf("create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 create call, got %d; all calls: %v", len(creates), ft.calls)
	}
	wantLabels := []string{"bugbot", "bugbot:verified", "severity:high"}
	if !slices.Equal(creates[0].labels, wantLabels) {
		t.Errorf("create labels = %v, want %v (base first, managed sorted)", creates[0].labels, wantLabels)
	}

	// Both managed labels are ensured, with the pinned colors, before the create.
	ensures := ft.callsOf("ensureLabel")
	if len(ensures) != 2 {
		t.Fatalf("expected 2 ensure calls (one per managed label), got %d", len(ensures))
	}
	gotColors := map[string]string{}
	for _, c := range ensures {
		gotColors[c.label.Name] = c.label.Color
	}
	if gotColors["bugbot:verified"] != "5319e7" || gotColors["severity:high"] != "d93f0b" {
		t.Errorf("ensure calls carried wrong names/colors: %v", gotColors)
	}
	createIdx := ft.indexOfOp("create", "")
	for i, c := range ft.calls {
		if c.op == "ensureLabel" && i > createIdx {
			t.Error("ensure call recorded AFTER the create call; labels must be ensured first")
		}
	}

	// The applied managed set is recorded for future reconciles.
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if want := []string{"bugbot:verified", "severity:high"}; !slices.Equal(pi.ManagedLabels, want) {
		t.Errorf("stored ManagedLabels = %v, want %v", pi.ManagedLabels, want)
	}
}

// TestApplyPublish_EnsureLabelsOncePerRun: the per-run ensure memo means
// each managed label costs at most one EnsureLabel call per run even across
// multiple creates. (Tolerating the tracker-side "already exists" response
// is the adapter's job now — see internal/tracker/github's
// TestEnsureLabel_ExactArgsAnd422Tolerated.)
func TestApplyPublish_EnsureLabelsOncePerRun(t *testing.T) {
	ctx := context.Background()
	st, f1 := setupPublishStore(t)
	f2 := seedSecondOpenFinding(t, ctx, st) // same severity/tier as f1

	ft := newFakeTracker()
	ft.createKeys = []tracker.IssueKey{"42", "43"}

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	if n := len(ft.callsOf("create")); n != 2 {
		t.Errorf("expected 2 creates, got %d", n)
	}
	// 2 managed labels, 2 findings -> still exactly 2 ensure calls (memoized).
	if n := len(ft.callsOf("ensureLabel")); n != 2 {
		t.Errorf("expected 2 ensure calls total across the run, got %d", n)
	}
	for _, fp := range []string{f1.Fingerprint, f2.Fingerprint} {
		pi, err := st.GetPublishedIssue(ctx, fp)
		if err != nil {
			t.Fatalf("get published issue: %v", err)
		}
		if want := []string{"bugbot:verified", "severity:high"}; !slices.Equal(pi.ManagedLabels, want) {
			t.Errorf("stored ManagedLabels for %s = %v, want %v", fp[:12], pi.ManagedLabels, want)
		}
	}
}

// TestApplyPublish_LabelReconcileConverges: after a create applied and
// recorded the managed set, a second run with no changes must make ZERO
// label tracker calls (desired == current, nil handling included).
func TestApplyPublish_LabelReconcileConverges(t *testing.T) {
	ctx := context.Background()
	st, _ := setupPublishStore(t)
	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}

	ft1 := newFakeTracker()
	ft1.createKeys = []tracker.IssueKey{"42"}
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, ft1, st, cfg, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	ft2 := newFakeTracker()
	var buf2 strings.Builder
	if err := runPublish(ctx, &buf2, ft2, st, cfg, 2, false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(buf2.String(), "skipped=1") {
		t.Errorf("second run should skip; got: %s", buf2.String())
	}
	if writes := ft2.writes(); len(writes) != 0 {
		t.Errorf("converged run must make zero write calls; got: %v", writes)
	}
}

// TestApplyPublish_LegacyRowAdditiveBackfill: a pre-feature row (empty
// managed_labels column -> ManagedLabels nil) gets its managed labels
// backfilled additively on the skip path -- one AddLabels call with every
// missing label, and NEVER a RemoveLabel (we don't know what was applied
// historically).
func TestApplyPublish_LegacyRowAdditiveBackfill(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	// Row exists (upserted after the finding, so updated_at is newer ->
	// publishSkip) but predates managed labels: column stays ''.
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "13", store.IssueStateOpen, ""); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	adds := ft.callsOf("addLabels")
	if len(adds) != 1 {
		t.Fatalf("expected exactly 1 additions call, got %d; calls: %v", len(adds), ft.calls)
	}
	if want := []string{"bugbot:verified", "severity:high"}; !slices.Equal(adds[0].labels, want) {
		t.Errorf("additions must carry both managed labels; got: %v", adds[0].labels)
	}
	if adds[0].key != "13" {
		t.Errorf("additions key = %q, want 13", adds[0].key)
	}
	if n := len(ft.callsOf("removeLabel")); n != 0 {
		t.Errorf("legacy backfill must never remove labels; got %d removals", n)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if want := []string{"bugbot:verified", "severity:high"}; !slices.Equal(pi.ManagedLabels, want) {
		t.Errorf("stored ManagedLabels = %v, want %v", pi.ManagedLabels, want)
	}
}

// TestApplyPublish_KnobsOffZeroLabelCalls: with both knobs off the feature
// is inert -- zero label tracker calls and zero removals, even when the
// store carries stale managed labels from a time the knobs were on.
func TestApplyPublish_KnobsOffZeroLabelCalls(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "13", store.IssueStateOpen, ""); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := st.SetPublishedManagedLabels(ctx, f.Fingerprint, []string{"severity:high"}); err != nil {
		t.Fatalf("seed stale labels: %v", err)
	}

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}
	if writes := ft.writes(); len(writes) != 0 {
		t.Errorf("knobs off must make zero write calls; got: %v", writes)
	}
	// The stale bookkeeping is left alone: nothing was synced.
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if want := []string{"severity:high"}; !slices.Equal(pi.ManagedLabels, want) {
		t.Errorf("stored ManagedLabels = %v, want %v (untouched)", pi.ManagedLabels, want)
	}
}

// TestApplyPublish_LabelRemovalPath: a recorded managed set that drifted
// from desired gets exactly current−desired removed (one RemoveLabel per
// label) and desired−current added in one AddLabels call -- never a
// full-array replace, and the shared label (severity:high) is not touched
// in either direction.
func TestApplyPublish_LabelRemovalPath(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t) // desired: bugbot:verified + severity:high
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "13", store.IssueStateOpen, ""); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	// Recorded as applied back when the finding was still T3.
	if err := st.SetPublishedManagedLabels(ctx, f.Fingerprint, []string{"bugbot:suspected", "severity:high"}); err != nil {
		t.Fatalf("seed recorded labels: %v", err)
	}

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, false); err != nil {
		t.Fatalf("runPublish: %v", err)
	}

	adds := ft.callsOf("addLabels")
	if len(adds) != 1 {
		t.Fatalf("expected exactly 1 additions call, got %d", len(adds))
	}
	if want := []string{"bugbot:verified"}; !slices.Equal(adds[0].labels, want) {
		t.Errorf("additions = %v, want %v (severity:high already applied)", adds[0].labels, want)
	}

	removes := ft.callsOf("removeLabel")
	if len(removes) != 1 {
		t.Fatalf("expected exactly 1 removal (current−desired), got %d: %v", len(removes), removes)
	}
	if removes[0].key != "13" || removes[0].labels[0] != "bugbot:suspected" {
		t.Errorf("removal = %v, want bugbot:suspected on issue 13", removes[0])
	}

	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if want := []string{"bugbot:verified", "severity:high"}; !slices.Equal(pi.ManagedLabels, want) {
		t.Errorf("stored ManagedLabels = %v, want %v", pi.ManagedLabels, want)
	}
}

// TestApplyPublish_LabelSyncFailureContinues: a label-sync tracker failure
// after a successful body push is action-scoped -- logged, counted in
// failed, run continues, body bookkeeping intact -- and the managed-label
// bookkeeping is NOT advanced, so the next cycle retries the sync naturally.
func TestApplyPublish_LabelSyncFailureContinues(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)

	// First run with knobs OFF: creates the issue, records no managed labels.
	ft1 := newFakeTracker()
	ft1.createKeys = []tracker.IssueKey{"42"}
	var buf1 strings.Builder
	if err := runPublish(ctx, &buf1, ft1, st, config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true}, 2, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Real content change so the second run takes the update path.
	f.Description = "a completely different description"
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("content change: %v", err)
	}

	ft2 := newFakeTracker()
	ft2.addErr = errors.New("github: add labels to issue 42: HTTP 500 boom")

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}
	var buf2 strings.Builder
	err := runPublish(ctx, &buf2, ft2, st, cfg, 2, false)
	if err == nil || !strings.Contains(err.Error(), "1 action(s) failed") {
		t.Fatalf("expected aggregate one-failure error, got: %v", err)
	}
	out := buf2.String()
	if !strings.Contains(out, "failed label-sync issue 42") {
		t.Errorf("expected a 'failed label-sync issue 42' log line; got: %s", out)
	}
	if !strings.Contains(out, "updated=1") || !strings.Contains(out, "failed=1") {
		t.Errorf("run must complete with updated=1 failed=1; got: %s", out)
	}

	// Body work is intact: the push landed and its hash was recorded.
	updates := ft2.callsOf("update")
	if len(updates) != 1 {
		t.Fatalf("expected exactly 1 body push, got %d", len(updates))
	}
	pi, gerr := st.GetPublishedIssue(ctx, f.Fingerprint)
	if gerr != nil {
		t.Fatalf("get published issue: %v", gerr)
	}
	if pi.BodyHash != bodyHashHex(updates[0].body) {
		t.Errorf("BodyHash must reflect the successful push despite the label failure")
	}
	// Label bookkeeping must NOT advance past the failed tracker call.
	if len(pi.ManagedLabels) != 0 {
		t.Errorf("ManagedLabels = %v, want empty (sync failed; retry next cycle)", pi.ManagedLabels)
	}
}

// TestApplyPublish_DryRunLabelSync: dry-run prints the sync intent for a
// drifted row and performs zero tracker writes (no ensure calls either) and
// zero store writes.
func TestApplyPublish_DryRunLabelSync(t *testing.T) {
	ctx := context.Background()
	st, f := setupPublishStore(t)
	if err := st.UpsertPublishedIssue(ctx, f.Fingerprint, "github", "13", store.IssueStateOpen, ""); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	ft := newFakeTracker()

	cfg := config.Publish{TierMin: 2, Labels: []string{"bugbot"}, CloseOnFixed: true, SeverityLabels: true, TierLabels: true}
	var buf strings.Builder
	if err := runPublish(ctx, &buf, ft, st, cfg, 2, true /* dry-run */); err != nil {
		t.Fatalf("runPublish dry-run: %v", err)
	}

	want := fmt.Sprintf("dry-run: sync labels on issue 13 for %s (+2 -0)", f.Fingerprint[:12])
	if !strings.Contains(buf.String(), want) {
		t.Errorf("expected %q in dry-run output; got: %s", want, buf.String())
	}
	if writes := ft.writes(); len(writes) != 0 {
		t.Errorf("dry-run must make zero write calls; got: %v", writes)
	}
	pi, err := st.GetPublishedIssue(ctx, f.Fingerprint)
	if err != nil {
		t.Fatalf("get published issue: %v", err)
	}
	if len(pi.ManagedLabels) != 0 {
		t.Errorf("dry-run must not write label bookkeeping; got %v", pi.ManagedLabels)
	}
}
