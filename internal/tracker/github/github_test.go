package github

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/tracker"
)

// fakeGH is a test engine.GHRunner that routes on the joined args and
// records every invocation, so the adapter tests can assert the exact gh
// argv this package produces without touching the network.
//
// This is a deliberate duplicate of internal/cli's and internal/engine's own
// fakeGH doubles: all three packages need the same tiny fake, but it is
// test-only, and sharing a test helper across package boundaries would mean
// exporting a fake solely for tests, which is worse than the ~30 lines of
// duplication.
type fakeGH struct {
	keys      []string
	responses map[string][]byte
	errs      map[string]error
	calls     [][]string
}

func newFakeGH() *fakeGH {
	return &fakeGH{responses: map[string][]byte{}, errs: map[string]error{}}
}

// on registers a canned JSON response for invocations whose joined args
// contain substr. Routes are checked in registration order.
func (f *fakeGH) on(substr string, resp []byte) *fakeGH {
	f.keys = append(f.keys, substr)
	f.responses[substr] = resp
	return f
}

// run is the engine.GHRunner the adapter under test calls.
func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	for _, k := range f.keys {
		if strings.Contains(joined, k) {
			if err, ok := f.errs[k]; ok {
				return nil, err
			}
			return f.responses[k], nil
		}
	}
	return nil, fmt.Errorf("fakeGH: no route for: %s", joined)
}

// newAdapter wires the adapter under test over a fakeGH with the given base
// labels.
func newAdapter(f *fakeGH, labels ...string) tracker.Tracker {
	return New(f.run, tracker.Config{Labels: labels})
}

func TestNameAndCapabilities(t *testing.T) {
	tr := newAdapter(newFakeGH())
	if tr.Name() != "github" {
		t.Errorf("Name() = %q, want github", tr.Name())
	}
	if !tr.Capabilities().Labels {
		t.Error("Capabilities().Labels = false, want true")
	}
}

// TestRegistryFactory pins the init() registration: the production factory
// is reachable under "github" and constructs without touching the network.
func TestRegistryFactory(t *testing.T) {
	if known := tracker.Known(); !slices.Contains(known, "github") {
		t.Fatalf("tracker.Known() = %v, want it to contain github", known)
	}
	tr, err := tracker.New("github", tracker.Config{Labels: []string{"bugbot"}})
	if err != nil {
		t.Fatalf("tracker.New(github): %v", err)
	}
	if tr.Name() != "github" {
		t.Errorf("registry-built Name() = %q, want github", tr.Name())
	}
}

// TestCreateIssue_ExactArgs pins the pre-refactor create invocation
// byte-for-byte: api path, -X POST, title/body -f pairs, then one labels[]
// pair per label preserving the caller's order (base labels first, managed
// labels sorted — the caller owns that ordering; the adapter must not
// reorder).
func TestCreateIssue_ExactArgs(t *testing.T) {
	fgh := newFakeGH().on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":42}`))
	tr := newAdapter(fgh, "bugbot")

	key, err := tr.CreateIssue(context.Background(), "boom", "the body",
		[]string{"bugbot", "bugbot:verified", "severity:high"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if key != "42" {
		t.Errorf("key = %q, want 42", key)
	}

	want := []string{
		"api", "repos/{owner}/{repo}/issues",
		"-X", "POST",
		"-f", "title=boom",
		"-f", "body=the body",
		"-f", "labels[]=bugbot",
		"-f", "labels[]=bugbot:verified",
		"-f", "labels[]=severity:high",
	}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("create argv:\n got  %q\n want %q", fgh.calls, want)
	}
}

// TestCreateIssue_NoLabels: an empty label list adds zero labels[] pairs.
func TestCreateIssue_NoLabels(t *testing.T) {
	fgh := newFakeGH().on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":7}`))
	tr := newAdapter(fgh)

	if _, err := tr.CreateIssue(context.Background(), "t", "b", nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	want := []string{"api", "repos/{owner}/{repo}/issues", "-X", "POST", "-f", "title=t", "-f", "body=b"}
	if !slices.Equal(fgh.calls[0], want) {
		t.Errorf("create argv = %q, want %q", fgh.calls[0], want)
	}
}

// TestCreateIssue_SanitizesTitle pins the exec-safety strip: a NUL (or any
// C0 control except tab/newline/CR) in the model-authored title would crash
// the gh fork with EINVAL, so the adapter strips it before building argv.
func TestCreateIssue_SanitizesTitle(t *testing.T) {
	fgh := newFakeGH().on("repos/{owner}/{repo}/issues -X POST", []byte(`{"number":42}`))
	tr := newAdapter(fgh, "bugbot")

	if _, err := tr.CreateIssue(context.Background(), "boom\x00 title", "body", nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	call := fgh.calls[0]
	for _, arg := range call {
		if strings.IndexByte(arg, 0) >= 0 {
			t.Errorf("create arg contains NUL (would crash forkExec): %q", arg)
		}
	}
	if want := "title=boom title"; !slices.Contains(call, want) {
		t.Errorf("sanitized title pair %q missing from argv %q", want, call)
	}
}

// TestCreateIssue_BadResponse: a response without a number (or non-JSON) is
// an error, never a zero key.
func TestCreateIssue_BadResponse(t *testing.T) {
	for name, resp := range map[string][]byte{
		"missing-number": []byte(`{}`),
		"not-json":       []byte(`nope`),
	} {
		t.Run(name, func(t *testing.T) {
			fgh := newFakeGH().on("repos/{owner}/{repo}/issues -X POST", resp)
			tr := newAdapter(fgh)
			if _, err := tr.CreateIssue(context.Background(), "t", "b", nil); err == nil {
				t.Error("expected an error for a create response without an issue number")
			}
		})
	}
}

// TestUpdateIssueBody_ExactArgs pins the body-only PATCH.
func TestUpdateIssueBody_ExactArgs(t *testing.T) {
	fgh := newFakeGH().on("issues/50 -X PATCH", []byte(``))
	tr := newAdapter(fgh, "bugbot")

	if err := tr.UpdateIssueBody(context.Background(), "50", "new body"); err != nil {
		t.Fatalf("UpdateIssueBody: %v", err)
	}
	want := []string{"api", "repos/{owner}/{repo}/issues/50", "-X", "PATCH", "-f", "body=new body"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("update argv:\n got  %q\n want %q", fgh.calls, want)
	}
}

// TestReopenIssue_ExactArgs pins the combined state+body PATCH: state=open
// first, then the fresh body, in one mutating call.
func TestReopenIssue_ExactArgs(t *testing.T) {
	fgh := newFakeGH().on("issues/65 -X PATCH", []byte(`{"number":65}`))
	tr := newAdapter(fgh, "bugbot")

	if err := tr.ReopenIssue(context.Background(), "65", "fresh body"); err != nil {
		t.Fatalf("ReopenIssue: %v", err)
	}
	want := []string{"api", "repos/{owner}/{repo}/issues/65", "-X", "PATCH", "-f", "state=open", "-f", "body=fresh body"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("reopen argv:\n got  %q\n want %q", fgh.calls, want)
	}
}

// TestCloseIssue_ExactArgs pins the state-only PATCH.
func TestCloseIssue_ExactArgs(t *testing.T) {
	fgh := newFakeGH().on("issues/77 -X PATCH", []byte(`{"number":77}`))
	tr := newAdapter(fgh, "bugbot")

	if err := tr.CloseIssue(context.Background(), "77"); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	want := []string{"api", "repos/{owner}/{repo}/issues/77", "-X", "PATCH", "-f", "state=closed"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("close argv:\n got  %q\n want %q", fgh.calls, want)
	}
}

// TestComment_ExactArgs pins the comment POST.
func TestComment_ExactArgs(t *testing.T) {
	fgh := newFakeGH().on("issues/77/comments", []byte(`{"id":1}`))
	tr := newAdapter(fgh, "bugbot")

	if err := tr.Comment(context.Background(), "77", "a note"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	want := []string{"api", "repos/{owner}/{repo}/issues/77/comments", "-X", "POST", "-f", "body=a note"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("comment argv:\n got  %q\n want %q", fgh.calls, want)
	}
}

// TestListIssues_ExactArgsAndParsing pins the backsync/recovery listing: the
// --paginate flag, the state + per_page + anchor-label query, and the
// decoding of concatenated JSON pages into key/state/body.
func TestListIssues_ExactArgsAndParsing(t *testing.T) {
	pages := `[{"number":77,"body":"b77","state":"closed"},{"number":78,"body":"b78","state":"closed"}][{"number":90,"body":"b90","state":"closed"}]`
	fgh := newFakeGH().on("issues?state=closed", []byte(pages))
	tr := newAdapter(fgh, "bugbot", "second-label-ignored")

	issues, err := tr.ListIssues(context.Background(), "closed")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []string{"api", "--paginate", "repos/{owner}/{repo}/issues?state=closed&per_page=100&labels=bugbot"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Fatalf("list argv:\n got  %q\n want %q", fgh.calls, want)
	}

	wantIssues := []tracker.Issue{
		{Key: "77", State: "closed", Body: "b77"},
		{Key: "78", State: "closed", Body: "b78"},
		{Key: "90", State: "closed", Body: "b90"},
	}
	if !slices.Equal(issues, wantIssues) {
		t.Errorf("issues = %+v, want %+v", issues, wantIssues)
	}
}

// TestListIssues_NoLabelsListsUnfiltered pins the empty-cfg.Labels edge: no
// anchor label means NO labels= filter at all (the pre-seam pipeline listed
// the whole repo), never an empty filter or an empty result.
func TestListIssues_NoLabelsListsUnfiltered(t *testing.T) {
	fgh := newFakeGH().on("issues?state=all", []byte(`[{"number":5,"body":"x","state":"open"}]`))
	tr := newAdapter(fgh)

	issues, err := tr.ListIssues(context.Background(), "all")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []string{"api", "--paginate", "repos/{owner}/{repo}/issues?state=all&per_page=100"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Fatalf("list argv:\n got  %q\n want %q", fgh.calls, want)
	}
	if len(issues) != 1 || issues[0].Key != "5" || issues[0].State != "open" {
		t.Errorf("issues = %+v, want one open issue with key 5", issues)
	}
}

// TestRepoURL pins the resolve call and its tolerant degradation: errors
// yield "" (permalinks omitted) rather than failing the publish run.
func TestRepoURL(t *testing.T) {
	fgh := newFakeGH().on("repo view", []byte("https://github.com/owner/repo\n"))
	tr := newAdapter(fgh, "bugbot")

	if got := tr.RepoURL(context.Background()); got != "https://github.com/owner/repo" {
		t.Errorf("RepoURL = %q, want trimmed URL", got)
	}
	want := []string{"repo", "view", "--json", "url", "-q", ".url"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("repo view argv:\n got  %q\n want %q", fgh.calls, want)
	}

	// No route -> error -> "".
	if got := newAdapter(newFakeGH()).RepoURL(context.Background()); got != "" {
		t.Errorf("RepoURL on failure = %q, want empty", got)
	}
}

// TestEnsureLabel_ExactArgsAnd422Tolerated pins the create-only single-label
// POST (name + pinned color + description) and its idempotency: a
// 422/already_exists response is success, any other failure propagates.
func TestEnsureLabel_ExactArgsAnd422Tolerated(t *testing.T) {
	ctx := context.Background()
	l := tracker.Label{Name: "severity:high", Color: "d93f0b", Description: "Bugbot: high severity finding"}

	fgh := newFakeGH().on("repos/{owner}/{repo}/labels -X POST", []byte(`{}`))
	tr := newAdapter(fgh, "bugbot")
	if err := tr.EnsureLabel(ctx, l); err != nil {
		t.Fatalf("EnsureLabel: %v", err)
	}
	want := []string{
		"api", "repos/{owner}/{repo}/labels", "-X", "POST",
		"-f", "name=severity:high",
		"-f", "color=d93f0b",
		"-f", "description=Bugbot: high severity finding",
	}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("ensure argv:\n got  %q\n want %q", fgh.calls, want)
	}

	for name, ghErr := range map[string]error{
		"already-exists": errors.New("gh: HTTP 422: Validation Failed (already_exists)"),
		"bare-422":       errors.New("gh: HTTP 422: Validation Failed"),
	} {
		t.Run(name, func(t *testing.T) {
			fgh := newFakeGH().on("repos/{owner}/{repo}/labels -X POST", nil)
			fgh.errs["repos/{owner}/{repo}/labels -X POST"] = ghErr
			if err := newAdapter(fgh).EnsureLabel(ctx, l); err != nil {
				t.Errorf("EnsureLabel must treat %v as success, got %v", ghErr, err)
			}
		})
	}

	t.Run("other-error-propagates", func(t *testing.T) {
		fgh := newFakeGH().on("repos/{owner}/{repo}/labels -X POST", nil)
		fgh.errs["repos/{owner}/{repo}/labels -X POST"] = errors.New("gh: HTTP 500 boom")
		if err := newAdapter(fgh).EnsureLabel(ctx, l); err == nil {
			t.Error("EnsureLabel must propagate a non-422 failure")
		}
	})
}

// TestAddLabels_ExactArgs pins the one-POST additions call: a labels[] pair
// per label, order preserved, never a full-array PATCH.
func TestAddLabels_ExactArgs(t *testing.T) {
	fgh := newFakeGH().on("issues/13/labels -X POST", []byte(`[]`))
	tr := newAdapter(fgh, "bugbot")

	if err := tr.AddLabels(context.Background(), "13", []string{"bugbot:verified", "severity:high"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	want := []string{
		"api", "repos/{owner}/{repo}/issues/13/labels", "-X", "POST",
		"-f", "labels[]=bugbot:verified",
		"-f", "labels[]=severity:high",
	}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("add-labels argv:\n got  %q\n want %q", fgh.calls, want)
	}
}

// TestRemoveLabel_ExactArgsAndEscaping pins the per-label DELETE with the
// label path-escaped, and the 404-is-success idempotency (label already
// detached, or the issue itself gone).
func TestRemoveLabel_ExactArgsAndEscaping(t *testing.T) {
	ctx := context.Background()

	fgh := newFakeGH().on("issues/13/labels/bugbot:suspected", []byte(`{}`))
	tr := newAdapter(fgh, "bugbot")
	if err := tr.RemoveLabel(ctx, "13", "bugbot:suspected"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	// ':' is a valid path-segment byte, so the historical argv is unescaped.
	want := []string{"api", "repos/{owner}/{repo}/issues/13/labels/bugbot:suspected", "-X", "DELETE"}
	if len(fgh.calls) != 1 || !slices.Equal(fgh.calls[0], want) {
		t.Errorf("delete argv:\n got  %q\n want %q", fgh.calls, want)
	}

	t.Run("escapes-reserved-bytes", func(t *testing.T) {
		fgh := newFakeGH().on("issues/13/labels/", []byte(`{}`))
		if err := newAdapter(fgh).RemoveLabel(ctx, "13", "help wanted/x"); err != nil {
			t.Fatalf("RemoveLabel: %v", err)
		}
		if got, want := fgh.calls[0][1], "repos/{owner}/{repo}/issues/13/labels/help%20wanted%2Fx"; got != want {
			t.Errorf("escaped DELETE path = %q, want %q", got, want)
		}
	})

	t.Run("404-is-success", func(t *testing.T) {
		fgh := newFakeGH().on("issues/13/labels/", nil)
		fgh.errs["issues/13/labels/"] = errors.New("gh api: HTTP 404 Not Found")
		if err := newAdapter(fgh).RemoveLabel(ctx, "13", "bugbot:suspected"); err != nil {
			t.Errorf("RemoveLabel must treat 404 as already-detached success, got %v", err)
		}
	})
}

// failingGH returns an engine.GHRunner that fails every call with err.
func failingGH(err error) engine.GHRunner {
	return func(context.Context, ...string) ([]byte, error) { return nil, err }
}

// TestSentinelMapping_MissingPrereq: a missing gh binary — whether surfaced
// as a wrapped exec.ErrNotFound (engine.RealGH) or as the plain "executable
// file not found" string (fake runners) — maps to tracker.ErrMissingPrereq
// on every operation, with the install hint preserved in the message.
func TestSentinelMapping_MissingPrereq(t *testing.T) {
	ctx := context.Background()
	for name, ghErr := range map[string]error{
		"wrapped-exec-errnotfound": fmt.Errorf("gh api: exec: %w", exec.ErrNotFound),
		"plain-string":             errors.New("executable file not found in $PATH"),
	} {
		t.Run(name, func(t *testing.T) {
			tr := New(failingGH(ghErr), tracker.Config{Labels: []string{"bugbot"}})
			ops := map[string]func() error{
				"create":      func() error { _, err := tr.CreateIssue(ctx, "t", "b", nil); return err },
				"update":      func() error { return tr.UpdateIssueBody(ctx, "1", "b") },
				"reopen":      func() error { return tr.ReopenIssue(ctx, "1", "b") },
				"close":       func() error { return tr.CloseIssue(ctx, "1") },
				"comment":     func() error { return tr.Comment(ctx, "1", "c") },
				"list":        func() error { _, err := tr.ListIssues(ctx, "all"); return err },
				"ensureLabel": func() error { return tr.EnsureLabel(ctx, tracker.Label{Name: "x"}) },
				"addLabels":   func() error { return tr.AddLabels(ctx, "1", []string{"x"}) },
			}
			for op, call := range ops {
				err := call()
				if !errors.Is(err, tracker.ErrMissingPrereq) {
					t.Errorf("%s: err = %v, want ErrMissingPrereq", op, err)
					continue
				}
				if !strings.Contains(err.Error(), "gh CLI is required") || !strings.Contains(err.Error(), "https://cli.github.com") {
					t.Errorf("%s: install hint lost: %v", op, err)
				}
			}
		})
	}
}

// TestSentinelMapping_RateLimited: an error the paced runner classifies as a
// rate limit maps to tracker.ErrRateLimited.
func TestSentinelMapping_RateLimited(t *testing.T) {
	ctx := context.Background()
	ghErr := fmt.Errorf("gh api: secondary rate limit exceeded: %w", engine.ErrGHRateLimited)
	tr := New(failingGH(ghErr), tracker.Config{})

	if _, err := tr.CreateIssue(ctx, "t", "b", nil); !errors.Is(err, tracker.ErrRateLimited) {
		t.Errorf("create: err = %v, want ErrRateLimited", err)
	}
	if err := tr.UpdateIssueBody(ctx, "1", "b"); !errors.Is(err, tracker.ErrRateLimited) {
		t.Errorf("update: err = %v, want ErrRateLimited", err)
	}
	if _, err := tr.ListIssues(ctx, "closed"); !errors.Is(err, tracker.ErrRateLimited) {
		t.Errorf("list: err = %v, want ErrRateLimited", err)
	}
}

// TestSentinelMapping_IssueGone: 404/410 on a key-addressed operation maps
// to tracker.ErrIssueGone; the same status on create does NOT (there is no
// issue key it could refer to), and non-gone failures on key-addressed
// operations stay unclassified.
func TestSentinelMapping_IssueGone(t *testing.T) {
	ctx := context.Background()
	goneErr := errors.New("gh api: HTTP 410: This issue was deleted")

	tr := New(failingGH(goneErr), tracker.Config{})
	keyOps := map[string]func() error{
		"update":    func() error { return tr.UpdateIssueBody(ctx, "50", "b") },
		"reopen":    func() error { return tr.ReopenIssue(ctx, "50", "b") },
		"close":     func() error { return tr.CloseIssue(ctx, "50") },
		"comment":   func() error { return tr.Comment(ctx, "50", "c") },
		"addLabels": func() error { return tr.AddLabels(ctx, "50", []string{"x"}) },
	}
	for op, call := range keyOps {
		if err := call(); !errors.Is(err, tracker.ErrIssueGone) {
			t.Errorf("%s: err = %v, want ErrIssueGone", op, err)
		}
	}

	if _, err := tr.CreateIssue(ctx, "t", "b", nil); errors.Is(err, tracker.ErrIssueGone) {
		t.Errorf("create: a 404/410 must not classify as ErrIssueGone, got %v", err)
	}

	plain := errors.New("gh api: HTTP 422: Validation Failed")
	tr2 := New(failingGH(plain), tracker.Config{})
	err := tr2.UpdateIssueBody(ctx, "50", "b")
	for _, sentinel := range []error{tracker.ErrIssueGone, tracker.ErrRateLimited, tracker.ErrMissingPrereq, tracker.ErrUnavailable, tracker.ErrUnsupported} {
		if errors.Is(err, sentinel) {
			t.Errorf("422 must stay unclassified, matched %v", sentinel)
		}
	}
}

// TestIsGHGoneOrNotFound guards the stale-detection predicate (moved here
// from internal/cli with the calls that consume it). It must key off gh's
// HTTP status token ("HTTP 404"/"HTTP 410") or the explicit "was deleted"
// phrase — never a bare "404"/"410" substring, which the wrapped error
// embeds via the issue number and API path. A real transient failure on an
// issue numbered like #404 must NOT be read as "gone" (that would delete
// the caller's row and create a duplicate).
func TestIsGHGoneOrNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"410-deleted", fmt.Errorf("publish: update issue #50: gh api repos/{owner}/{repo}/issues/50: HTTP 410: This issue was deleted"), true},
		{"404-not-found", fmt.Errorf("publish: update issue #77: gh api repos/{owner}/{repo}/issues/77: HTTP 404 Not Found"), true},
		{"410-status", fmt.Errorf("HTTP 410 Gone"), true},
		{"404-status", fmt.Errorf("HTTP 404 Not Found"), true},
		{"was-deleted", fmt.Errorf("This issue was deleted"), true},
		{"403-on-issue-404", fmt.Errorf("publish: update issue #404: gh api repos/{owner}/{repo}/issues/404: HTTP 403 rate limited"), false},
		{"422-on-issue-410", fmt.Errorf("publish: update issue #410: gh api repos/{owner}/{repo}/issues/410: HTTP 422 validation failed"), false},
		{"unrelated-auth", fmt.Errorf("gh: HTTP 401 Unauthorized"), false},
		{"unrelated-network", fmt.Errorf("dial tcp: connection refused"), false},
		{"generic", fmt.Errorf("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGHGoneOrNotFound(tc.err); got != tc.want {
				t.Errorf("isGHGoneOrNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSanitizeControlChars pins the adapter-local copy of the strip helper.
func TestSanitizeControlChars(t *testing.T) {
	clean := "hello\tworld\nsecond line\r\nthird"
	if got := sanitizeControlChars(clean); got != clean {
		t.Errorf("clean text mutated: got %q want %q", got, clean)
	}
	if got := sanitizeControlChars("a\x00b\x07c\x1bd\x0be\x0cf"); got != "abcdef" {
		t.Errorf("control strip: got %q want %q", got, "abcdef")
	}
	if got := sanitizeControlChars("x\x00\ty\n\rz"); got != "x\ty\n\rz" {
		t.Errorf("whitespace not preserved: got %q", got)
	}
}
