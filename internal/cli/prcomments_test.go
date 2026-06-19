package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// finding is a small constructor for test findings.
func finding(fp, file string, line int, tier domain.Tier) store.Finding {
	return store.Finding{
		Fingerprint: fp,
		Title:       "Title " + fp,
		Description: "Desc " + fp,
		Reasoning:   "Trace " + fp,
		Severity:    "high",
		Tier:        tier,
		File:        file,
		Line:        line,
	}
}

// commentableAt builds a commentableLines covering the given file:line pairs.
func commentableAt(pairs ...struct {
	file string
	line int
}) commentableLines {
	c := make(commentableLines)
	for _, p := range pairs {
		record(c, p.file, p.line)
	}
	return c
}

func result(findings ...store.Finding) *funnel.Result {
	return &funnel.Result{
		Commit:   "headSHA",
		Findings: findings,
		Stats:    funnel.Stats{FinderRuns: 5, FinderFailures: 0},
	}
}

// findThe action of a given op for a given fp (or summary when fp == "").
func actionFor(plan planResult, op syncOp, fp string) (syncAction, bool) {
	for _, a := range plan.actions {
		if a.op == op && a.fp == fp {
			return a, true
		}
	}
	return syncAction{}, false
}

// TestPlanSync_CreateOnFirstRun: a commentable T2 with no existing comment plans
// an inline create carrying the marker + body; the summary is created too.
func TestPlanSync_CreateOnFirstRun(t *testing.T) {
	f := finding("fp1", "foo.go", 10, 2)
	res := result(f)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})

	plan := planSync(res, commentable, existingState{byFingerprint: map[string]existingComment{}}, "headSHA", "summary")

	a, ok := actionFor(plan, opCreate, "fp1")
	if !ok {
		t.Fatalf("expected create action for fp1; actions=%+v", plan.actions)
	}
	if a.kind != kindReview {
		t.Errorf("inline finding should create a review comment")
	}
	if !strings.HasPrefix(a.body, "<!-- bugbot:fp=fp1 -->") {
		t.Errorf("inline body must lead with the fp marker:\n%s", a.body)
	}
	if !strings.Contains(a.body, "<details><summary>Verification trace</summary>") {
		t.Errorf("inline body should collapse the verification trace:\n%s", a.body)
	}
	if a.path != "foo.go" || a.line != 10 || a.commit != "headSHA" {
		t.Errorf("inline create must carry path/line/commit: %+v", a)
	}
	if _, ok := actionFor(plan, opCreate, ""); !ok {
		t.Errorf("summary create expected")
	}
	if !plan.newGateFingerprints["fp1"] {
		t.Errorf("new T2 should be a gate fingerprint")
	}
}

// TestPlanSync_IdempotentRerun: an existing comment whose body matches the
// rendered body is skipped (no update).
func TestPlanSync_IdempotentRerun(t *testing.T) {
	f := finding("fp1", "foo.go", 10, 2)
	res := result(f)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})

	body := renderInlineBody(f)
	existing := existingState{byFingerprint: map[string]existingComment{
		"fp1": {ID: 100, Body: body, Kind: kindReview},
	}}

	plan := planSync(res, commentable, existing, "headSHA", "summary")
	if _, ok := actionFor(plan, opUpdate, "fp1"); ok {
		t.Errorf("identical body must not produce an update")
	}
	if _, ok := actionFor(plan, opSkip, "fp1"); !ok {
		t.Errorf("identical body should be a skip (unchanged)")
	}
	// Pre-existing fingerprint must NOT trip the gate.
	if plan.newGateFingerprints["fp1"] {
		t.Errorf("re-post of an existing finding must not be a new gate fingerprint")
	}
}

// TestPlanSync_ChangedBodyUpdates: an existing comment with a different body is
// PATCHed.
func TestPlanSync_ChangedBodyUpdates(t *testing.T) {
	f := finding("fp1", "foo.go", 10, 2)
	res := result(f)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})

	existing := existingState{byFingerprint: map[string]existingComment{
		"fp1": {ID: 100, Body: "<!-- bugbot:fp=fp1 -->\n\nstale body", Kind: kindReview},
	}}

	plan := planSync(res, commentable, existing, "headSHA", "summary")
	a, ok := actionFor(plan, opUpdate, "fp1")
	if !ok {
		t.Fatalf("changed body should produce an update; actions=%+v", plan.actions)
	}
	if a.id != 100 {
		t.Errorf("update must target the existing comment id, got %d", a.id)
	}
}

// TestPlanSync_StaleResolves: a marker present on the PR but absent from this
// run is PATCHed to a resolved notice (not deleted, not skipped).
func TestPlanSync_StaleResolves(t *testing.T) {
	// This run reports nothing; fp_old is stale.
	res := result()
	existing := existingState{byFingerprint: map[string]existingComment{
		"fp_old": {ID: 200, Body: "<!-- bugbot:fp=fp_old -->\n\nold body", Kind: kindReview},
	}}

	plan := planSync(res, commentableLines{}, existing, "headSHA", "summary")
	a, ok := actionFor(plan, opResolve, "fp_old")
	if !ok {
		t.Fatalf("stale fingerprint should resolve; actions=%+v", plan.actions)
	}
	if a.id != 200 {
		t.Errorf("resolve must target the stale comment id")
	}
	if !strings.Contains(a.body, "No longer reported") {
		t.Errorf("resolve body should be the resolved notice:\n%s", a.body)
	}
	if !strings.HasPrefix(a.body, "<!-- bugbot:fp=fp_old -->") {
		t.Errorf("resolve body must keep the marker:\n%s", a.body)
	}
}

// TestPlanSync_StaleAlreadyResolvedSkips: re-running over an already-resolved
// stale comment is idempotent (skip, no second PATCH) — including on a LATER
// head commit, since the notice body is SHA-free and detected by marker.
func TestPlanSync_StaleAlreadyResolvedSkips(t *testing.T) {
	res := result()
	notice := resolvedNotice("fp_old")
	existing := existingState{byFingerprint: map[string]existingComment{
		"fp_old": {ID: 200, Body: notice, Kind: kindReview},
	}}
	for _, head := range []string{"headSHA", "laterHeadSHA"} {
		plan := planSync(res, commentableLines{}, existing, head, "summary")
		if _, ok := actionFor(plan, opResolve, "fp_old"); ok {
			t.Errorf("head %s: already-resolved stale comment must not be resolved again", head)
		}
		if _, ok := actionFor(plan, opSkip, "fp_old"); !ok {
			t.Errorf("head %s: already-resolved stale comment should be a skip", head)
		}
	}
}

// TestPlanSync_ReappearingResolvedTripsGateAndUnresolves: a finding that was
// resolved on an earlier push and reappears (fix-then-revert) must count as NEW
// for the CI gate — its tombstone does not vouch for it — and its comment must
// be un-resolved back to the full inline body.
func TestPlanSync_ReappearingResolvedTripsGateAndUnresolves(t *testing.T) {
	f := finding("fp1", "foo.go", 10, 2)
	res := result(f)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})

	existing := existingState{byFingerprint: map[string]existingComment{
		"fp1": {ID: 100, Body: resolvedNotice("fp1"), Kind: kindReview},
	}}

	plan := planSync(res, commentable, existing, "headSHA", "summary")
	if !plan.newGateFingerprints["fp1"] {
		t.Errorf("reappearing resolved finding must trip the gate as new")
	}
	a, ok := actionFor(plan, opUpdate, "fp1")
	if !ok {
		t.Fatalf("reappearing finding should update the resolved comment back to the inline body; actions=%+v", plan.actions)
	}
	if a.id != 100 {
		t.Errorf("un-resolve must target the existing comment id, got %d", a.id)
	}
	if isResolvedBody(a.body) {
		t.Errorf("un-resolved body must not carry the resolved marker:\n%s", a.body)
	}
	if a.body != renderInlineBody(f) {
		t.Errorf("un-resolved body should be the full inline body:\n%s", a.body)
	}
}

// TestPlanSync_SummaryUpdateInPlace: an existing summary with a different body is
// updated in place, not duplicated.
func TestPlanSync_SummaryUpdateInPlace(t *testing.T) {
	res := result(finding("fp1", "foo.go", 10, 2))
	existing := existingState{
		byFingerprint: map[string]existingComment{},
		summary:       &existingComment{ID: 999, Body: markerSummary + "\n\nold summary", Kind: kindIssue},
	}
	plan := planSync(res, commentableLines{}, existing, "headSHA", "summary")
	// No commentable line, so fp1 is summary-only; summary must update.
	a, ok := actionFor(plan, opUpdate, "")
	if !ok {
		t.Fatalf("existing summary with changed body should update; actions=%+v", plan.actions)
	}
	if a.kind != kindIssue || a.id != 999 {
		t.Errorf("summary update must target the issue comment id 999, got %+v", a)
	}
	// No second summary create.
	if _, ok := actionFor(plan, opCreate, ""); ok {
		t.Errorf("must not create a second summary when one exists")
	}
}

// TestPlanSync_T3RoutingSummaryVsWithhold covers the suspected mode.
func TestPlanSync_T3RoutingSummaryVsWithhold(t *testing.T) {
	t3 := finding("fp_t3", "foo.go", 10, 3)
	res := result(t3)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})

	// summary: T3 is surfaced in the summary, never inline (tier>2).
	plan := planSync(res, commentable, existingState{byFingerprint: map[string]existingComment{}}, "headSHA", "summary")
	if _, ok := actionFor(plan, opCreate, "fp_t3"); ok {
		t.Errorf("T3 must never be posted inline")
	}
	summary, _ := actionFor(plan, opCreate, "")
	if !strings.Contains(summary.body, "fp_t3") && !strings.Contains(summary.body, "T3") {
		t.Errorf("T3 should appear in the summary:\n%s", summary.body)
	}
	if plan.newGateFingerprints["fp_t3"] {
		t.Errorf("T3 must not trip the verified gate")
	}

	// withhold: T3 omitted entirely.
	planW := planSync(res, commentable, existingState{byFingerprint: map[string]existingComment{}}, "headSHA", "withhold")
	sw, _ := actionFor(planW, opCreate, "")
	if strings.Contains(sw.body, "| T3 |") {
		t.Errorf("withheld T3 must not appear in the summary table:\n%s", sw.body)
	}
	if !strings.Contains(sw.body, "withheld by configuration") {
		t.Errorf("withhold mode should note suppression:\n%s", sw.body)
	}
}

// TestPlanSync_NonCommentableT2GoesToSummary: a verified finding outside the diff
// (no commentable line) is not posted inline; it appears only in the summary, but
// still trips the gate.
func TestPlanSync_NonCommentableT2GoesToSummary(t *testing.T) {
	f := finding("fp_far", "blast.go", 42, 2)
	res := result(f)
	plan := planSync(res, commentableLines{}, existingState{byFingerprint: map[string]existingComment{}}, "headSHA", "summary")
	if _, ok := actionFor(plan, opCreate, "fp_far"); ok {
		t.Errorf("non-commentable T2 must not be posted inline")
	}
	if !plan.newGateFingerprints["fp_far"] {
		t.Errorf("non-commentable NEW T2 must still trip the gate")
	}
	summary, _ := actionFor(plan, opCreate, "")
	if !strings.Contains(summary.body, "blast.go:42") {
		t.Errorf("non-commentable T2 should be listed in the summary:\n%s", summary.body)
	}
}

// TestApplyPlan_CreateInlineCall asserts the exact gh api call for an inline
// create, including side=RIGHT and commit_id.
func TestApplyPlan_CreateInlineCall(t *testing.T) {
	f := finding("fp1", "foo.go", 10, 2)
	res := result(f)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})
	plan := planSync(res, commentable, existingState{byFingerprint: map[string]existingComment{}}, "headSHA", "summary")

	gh := newFakeGH().
		on("pulls/3/comments", []byte(`{"id":1}`)).
		on("issues/3/comments", []byte(`{"id":2}`))

	var sink strings.Builder
	if err := applyPlan(context.Background(), gh.run, 3, "headSHA", plan, false, &sink); err != nil {
		t.Fatalf("applyPlan: %v", err)
	}

	posts := gh.callsContaining("pulls/3/comments")
	if len(posts) != 1 {
		t.Fatalf("expected one inline POST, got %d: %v", len(posts), gh.calls)
	}
	call := posts[0]
	if v, _ := argValue(call, "path"); v != "foo.go" {
		t.Errorf("path arg = %q", v)
	}
	if v, _ := argValue(call, "side"); v != "RIGHT" {
		t.Errorf("side arg = %q, want RIGHT", v)
	}
	if v, _ := argValue(call, "commit_id"); v != "headSHA" {
		t.Errorf("commit_id arg = %q", v)
	}
	if v, _ := argValue(call, "line"); v != "10" {
		t.Errorf("line arg = %q", v)
	}
	body, _ := argValue(call, "body")
	if !strings.HasPrefix(body, "<!-- bugbot:fp=fp1 -->") {
		t.Errorf("posted body must carry the marker:\n%s", body)
	}
}

// TestApplyPlan_UpdateAndResolveUsePatch asserts update and resolve actions
// issue -X PATCH (not POST) against the kind-correct endpoint: review comments
// via pulls/comments/<id>, issue comments via issues/comments/<id>.
func TestApplyPlan_UpdateAndResolveUsePatch(t *testing.T) {
	plan := planResult{actions: []syncAction{
		{op: opUpdate, kind: kindReview, id: 100, fp: "fp1", body: "updated body"},
		{op: opResolve, kind: kindIssue, id: 200, fp: "fp2", body: resolvedNotice("fp2")},
	}}

	gh := newFakeGH().
		on("pulls/comments/100", []byte(`{}`)).
		on("issues/comments/200", []byte(`{}`))

	var sink strings.Builder
	if err := applyPlan(context.Background(), gh.run, 3, "headSHA", plan, false, &sink); err != nil {
		t.Fatalf("applyPlan: %v", err)
	}

	updates := gh.callsContaining("pulls/comments/100")
	if len(updates) != 1 {
		t.Fatalf("expected one update call, got %d: %v", len(updates), gh.calls)
	}
	if v, _ := flagValue(updates[0], "-X"); v != "PATCH" {
		t.Errorf("update must use -X PATCH, got %q in %v", v, updates[0])
	}

	resolves := gh.callsContaining("issues/comments/200")
	if len(resolves) != 1 {
		t.Fatalf("expected one resolve call, got %d: %v", len(resolves), gh.calls)
	}
	if v, _ := flagValue(resolves[0], "-X"); v != "PATCH" {
		t.Errorf("resolve must use -X PATCH, got %q in %v", v, resolves[0])
	}
	if body, _ := argValue(resolves[0], "body"); !isResolvedBody(body) {
		t.Errorf("resolve body must carry the resolved marker:\n%s", body)
	}
}

// TestApplyPlan_DryRunMakesNoWrites confirms dry-run performs zero gh write
// calls.
func TestApplyPlan_DryRunMakesNoWrites(t *testing.T) {
	f := finding("fp1", "foo.go", 10, 2)
	res := result(f)
	commentable := commentableAt(struct {
		file string
		line int
	}{"foo.go", 10})
	plan := planSync(res, commentable, existingState{byFingerprint: map[string]existingComment{}}, "headSHA", "summary")

	gh := newFakeGH() // no routes: any call would error
	var sink strings.Builder
	if err := applyPlan(context.Background(), gh.run, 3, "headSHA", plan, true, &sink); err != nil {
		t.Fatalf("dry-run applyPlan should not error: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("dry-run must make no gh calls, made %d: %v", len(gh.calls), gh.calls)
	}
	if !strings.Contains(sink.String(), "[dry-run] would create") {
		t.Errorf("dry-run should describe planned actions:\n%s", sink.String())
	}
}

// TestParseComments_Paginated confirms concatenated JSON arrays (gh --paginate)
// are decoded into a single comment list.
func TestParseComments_Paginated(t *testing.T) {
	raw := []byte(`[{"id":1,"body":"a"},{"id":2,"body":"b"}][{"id":3,"body":"c"}]`)
	got, err := parseComments(raw)
	if err != nil {
		t.Fatalf("parseComments: %v", err)
	}
	if len(got) != 3 || got[2].ID != 3 {
		t.Errorf("expected 3 comments across two pages, got %+v", got)
	}
}

// TestLoadExisting_IndexesMarkers confirms loadExisting builds the
// fingerprint→comment and summary indexes from review + issue comments.
func TestLoadExisting_IndexesMarkers(t *testing.T) {
	gh := newFakeGH().
		on("pulls/5/comments", []byte(`[{"id":11,"body":"<!-- bugbot:fp=abc -->\n\nbody"},{"id":12,"body":"not ours"}]`)).
		on("issues/5/comments", []byte(`[{"id":21,"body":"`+markerSummary+`\n\nsummary"}]`))

	st, err := loadExisting(context.Background(), gh.run, 5)
	if err != nil {
		t.Fatalf("loadExisting: %v", err)
	}
	if c, ok := st.byFingerprint["abc"]; !ok || c.ID != 11 || c.Kind != kindReview {
		t.Errorf("expected fp abc -> review comment 11, got %+v", st.byFingerprint)
	}
	if st.summary == nil || st.summary.ID != 21 {
		t.Errorf("expected summary comment 21, got %+v", st.summary)
	}
}
