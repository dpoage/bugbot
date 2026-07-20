package tracker_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dpoage/bugbot/internal/tracker"
)

// stubTracker is a minimal in-test Tracker implementation.
type stubTracker struct {
	name string
}

var _ tracker.Tracker = (*stubTracker)(nil)

func (s *stubTracker) Name() string                   { return s.name }
func (s *stubTracker) RepoURL(context.Context) string { return "" }
func (s *stubTracker) CreateIssue(_ context.Context, _, _ string, _ []string) (tracker.IssueKey, error) {
	return "stub-1", nil
}
func (s *stubTracker) UpdateIssueBody(context.Context, tracker.IssueKey, string) error { return nil }
func (s *stubTracker) ReopenIssue(context.Context, tracker.IssueKey, string) error     { return nil }
func (s *stubTracker) CloseIssue(context.Context, tracker.IssueKey) error              { return nil }
func (s *stubTracker) Comment(context.Context, tracker.IssueKey, string) error         { return nil }
func (s *stubTracker) ListIssues(context.Context, string) ([]tracker.Issue, error) {
	return nil, nil
}
func (s *stubTracker) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Labels: true}
}
func (s *stubTracker) EnsureLabel(context.Context, tracker.Label) error            { return nil }
func (s *stubTracker) AddLabels(context.Context, tracker.IssueKey, []string) error { return nil }
func (s *stubTracker) RemoveLabel(context.Context, tracker.IssueKey, string) error { return nil }

func stubFactory(name string) func(tracker.Config) (tracker.Tracker, error) {
	return func(tracker.Config) (tracker.Tracker, error) {
		return &stubTracker{name: name}, nil
	}
}

// The registry is package-global and never unregisters, so each test
// registers under a process-unique name (keeps `go test -count=N` green).
var nameSeq atomic.Int64

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, nameSeq.Add(1))
}

// init registers the fixed "github" stub that the unknown-name test asserts
// against. Registration happens in init, not a test body, so -count>1 reruns
// cannot double-register.
func init() {
	tracker.Register("github", stubFactory("github"))
}

func TestNewRoundTripsRegisteredFactory(t *testing.T) {
	name := uniqueName("roundtrip")
	var got tracker.Config
	tracker.Register(name, func(cfg tracker.Config) (tracker.Tracker, error) {
		got = cfg
		return &stubTracker{name: name}, nil
	})

	want := tracker.Config{Labels: []string{"bugbot", "severity:high"}}
	tr, err := tracker.New(name, want)
	if err != nil {
		t.Fatalf("New(%q) error: %v", name, err)
	}
	if tr.Name() != name {
		t.Errorf("Name() = %q, want %q", tr.Name(), name)
	}
	if !slices.Equal(got.Labels, want.Labels) {
		t.Errorf("factory received Config.Labels = %v, want %v", got.Labels, want.Labels)
	}
}

func TestNewUnknownNameListsKnown(t *testing.T) {
	tr, err := tracker.New("gitlab", tracker.Config{})
	if err == nil {
		t.Fatal(`New("gitlab") = nil error, want unknown-tracker error`)
	}
	if tr != nil {
		t.Errorf(`New("gitlab") returned non-nil Tracker %v alongside error`, tr)
	}
	msg := err.Error()
	if !strings.Contains(msg, "gitlab") {
		t.Errorf(`error %q does not name the unknown tracker "gitlab"`, msg)
	}
	known := tracker.Known()
	if !slices.Contains(known, "github") {
		t.Fatalf(`precondition: "github" stub not registered; known = %v`, known)
	}
	for _, name := range known {
		if !strings.Contains(msg, name) {
			t.Errorf("error %q does not list registered tracker %q", msg, name)
		}
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	name := uniqueName("dup")
	tracker.Register(name, stubFactory(name))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("duplicate Register did not panic")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, name) {
			t.Errorf("panic message %q does not contain the duplicate name %q", msg, name)
		}
	}()
	tracker.Register(name, stubFactory(name))
}

func TestKnownSortedCopy(t *testing.T) {
	// Register out of lexical order; the counter suffix keeps names
	// process-unique, the prefixes force an unsorted insertion order.
	names := []string{
		uniqueName("known-zz"),
		uniqueName("known-aa"),
		uniqueName("known-mm"),
	}
	for _, n := range names {
		tracker.Register(n, stubFactory(n))
	}

	known := tracker.Known()
	if !slices.IsSorted(known) {
		t.Errorf("Known() not sorted: %v", known)
	}
	for _, n := range names {
		if !slices.Contains(known, n) {
			t.Errorf("Known() missing %q: %v", n, known)
		}
	}

	// Mutating the returned slice must not leak into the registry.
	known[0] = "mutated-sentinel"
	if slices.Contains(tracker.Known(), "mutated-sentinel") {
		t.Error("Known() returned a live reference, want a copy")
	}
}
