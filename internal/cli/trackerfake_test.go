package cli

import (
	"context"
	"fmt"

	"github.com/dpoage/bugbot/internal/tracker"
)

// trackerCall records one interface-level call on fakeTracker. Only the
// fields relevant to the op are set.
type trackerCall struct {
	op     string // create, update, reopen, close, comment, list, repoURL, ensureLabel, addLabels, removeLabel
	key    tracker.IssueKey
	title  string
	body   string        // create/update/reopen body
	text   string        // comment text
	state  string        // list state filter
	label  tracker.Label // ensureLabel
	labels []string      // create/addLabels label names; removeLabel uses labels[0]
}

// fakeTracker is a scripted tracker.Tracker double: canned returns per
// operation plus call recording at the INTERFACE level. The gh arg-level
// assertions that used to live in this package moved to
// internal/tracker/github's adapter tests; the applier suites here only care
// about WHICH tracker operations run, in what order, with what content.
//
// Error fields wrap the tracker sentinels exactly as a real adapter would
// (fmt.Errorf("...: %w", tracker.ErrX)), so the applier's errors.Is dispatch
// is exercised for real.
type fakeTracker struct {
	name    string
	repoURL string
	caps    tracker.Capabilities

	createKeys  []tracker.IssueKey // returned in order; exhausted -> error
	createErr   error
	updateErr   error
	reopenErr   error
	closeErr    error
	commentErr  error
	listErr     error
	ensureErr   error
	addErr      error
	removeErr   error
	listByState map[string][]tracker.Issue

	calls []trackerCall
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{
		name:        "github",
		repoURL:     "https://github.com/owner/repo",
		caps:        tracker.Capabilities{Labels: true},
		listByState: map[string][]tracker.Issue{},
	}
}

func (f *fakeTracker) Name() string                       { return f.name }
func (f *fakeTracker) Capabilities() tracker.Capabilities { return f.caps }

func (f *fakeTracker) RepoURL(context.Context) string {
	f.calls = append(f.calls, trackerCall{op: "repoURL"})
	return f.repoURL
}

func (f *fakeTracker) CreateIssue(_ context.Context, title, body string, labels []string) (tracker.IssueKey, error) {
	f.calls = append(f.calls, trackerCall{op: "create", title: title, body: body, labels: append([]string(nil), labels...)})
	if f.createErr != nil {
		return "", f.createErr
	}
	if len(f.createKeys) == 0 {
		return "", fmt.Errorf("fakeTracker: no scripted create key")
	}
	k := f.createKeys[0]
	f.createKeys = f.createKeys[1:]
	return k, nil
}

func (f *fakeTracker) UpdateIssueBody(_ context.Context, key tracker.IssueKey, body string) error {
	f.calls = append(f.calls, trackerCall{op: "update", key: key, body: body})
	return f.updateErr
}

func (f *fakeTracker) ReopenIssue(_ context.Context, key tracker.IssueKey, body string) error {
	f.calls = append(f.calls, trackerCall{op: "reopen", key: key, body: body})
	return f.reopenErr
}

func (f *fakeTracker) CloseIssue(_ context.Context, key tracker.IssueKey) error {
	f.calls = append(f.calls, trackerCall{op: "close", key: key})
	return f.closeErr
}

func (f *fakeTracker) Comment(_ context.Context, key tracker.IssueKey, text string) error {
	f.calls = append(f.calls, trackerCall{op: "comment", key: key, text: text})
	return f.commentErr
}

func (f *fakeTracker) ListIssues(_ context.Context, state string) ([]tracker.Issue, error) {
	f.calls = append(f.calls, trackerCall{op: "list", state: state})
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listByState[state], nil
}

func (f *fakeTracker) EnsureLabel(_ context.Context, l tracker.Label) error {
	f.calls = append(f.calls, trackerCall{op: "ensureLabel", label: l})
	return f.ensureErr
}

func (f *fakeTracker) AddLabels(_ context.Context, key tracker.IssueKey, labels []string) error {
	f.calls = append(f.calls, trackerCall{op: "addLabels", key: key, labels: append([]string(nil), labels...)})
	return f.addErr
}

func (f *fakeTracker) RemoveLabel(_ context.Context, key tracker.IssueKey, label string) error {
	f.calls = append(f.calls, trackerCall{op: "removeLabel", key: key, labels: []string{label}})
	return f.removeErr
}

var _ tracker.Tracker = (*fakeTracker)(nil)

// callsOf returns every recorded call with the given op, in order.
func (f *fakeTracker) callsOf(op string) []trackerCall {
	var out []trackerCall
	for _, c := range f.calls {
		if c.op == op {
			out = append(out, c)
		}
	}
	return out
}

// writes returns every recorded MUTATING call (everything except the
// read-only repoURL/list ops), for dry-run and convergence zero-write
// assertions.
func (f *fakeTracker) writes() []trackerCall {
	var out []trackerCall
	for _, c := range f.calls {
		switch c.op {
		case "repoURL", "list":
		default:
			out = append(out, c)
		}
	}
	return out
}

// indexOfOp returns the index in f.calls of the first call with op and, when
// key is non-empty, that key; -1 when absent. Used for ordering assertions
// (comment-before-close, close-before-comment, ensure-before-create).
func (f *fakeTracker) indexOfOp(op string, key tracker.IssueKey) int {
	for i, c := range f.calls {
		if c.op == op && (key == "" || c.key == key) {
			return i
		}
	}
	return -1
}

// Scripted sentinel errors, built exactly like a real adapter builds them.
func errTrackerGone(key tracker.IssueKey) error {
	return fmt.Errorf("github: update issue %s: %w: HTTP 410: This issue was deleted", key, tracker.ErrIssueGone)
}

func errTrackerRateLimited() error {
	return fmt.Errorf("github: %w: secondary rate limit exceeded", tracker.ErrRateLimited)
}

func errTrackerMissingPrereq() error {
	return fmt.Errorf("github: gh CLI is required but was not found on PATH; install gh from https://cli.github.com: %w", tracker.ErrMissingPrereq)
}
