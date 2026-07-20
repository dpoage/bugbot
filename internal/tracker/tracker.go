// Package tracker abstracts the issue tracker that bugbot publishes
// findings to. GitHub is the first adapter; Jira, GitLab, and OpenProject
// plug in behind the same interface. The publish pipeline talks only to this
// package: adapters self-register from their package init via [Register],
// and callers construct one with [New].
//
// # Issue keys
//
// [IssueKey] is the tracker-native identifier of an issue: "17" for a GitHub
// issue number, "PROJ-42" for a Jira key. A key is opaque everywhere outside
// the adapter that minted it. Callers store keys, compare them for equality,
// and hand them back to the same adapter — they never parse a key, derive a
// URL from one, or assume it is numeric.
//
// # Body contract
//
// Issue bodies are CommonMark. The publish pipeline renders every body in a
// canonical form whose first two lines are machine-readable HTML comments:
//
//	<!-- bugbot:fp=<fingerprint> -->
//	<!-- bugbot:meta <json> -->
//
// Both lines are load-bearing: the line-1 fingerprint marker is how issues
// are matched back to findings during recovery, and the line-2 meta comment
// is what external tooling parses instead of scraping the Markdown. Adapters
// for trackers that reject or visibly render HTML comments own the
// transformation in both directions: they accept canonical bodies on every
// write and return bodies restored to canonical form from
// [Tracker.ListIssues].
//
// # Error taxonomy
//
// Adapters classify failures by wrapping the package sentinels with %w, so
// callers dispatch with errors.Is while the message keeps tracker-specific
// detail:
//
//   - [ErrIssueGone]: the key no longer resolves (issue deleted, moved, or
//     converted). The caller treats the issue as lost and may re-create it.
//   - [ErrRateLimited]: the tracker is throttling. Retry after backing off.
//   - [ErrUnavailable]: transient transport or server failure. Retryable.
//   - [ErrUnsupported]: the tracker cannot perform the operation at all — a
//     permanent condition, not a transient one. Label methods return it when
//     Capabilities().Labels is false.
//
// Failures that match no sentinel are permanent adapter-specific errors.
//
// # Idempotency
//
// The publish applier re-runs plans after partial failure, so
// implementations owe these repeat-safety guarantees:
//
//   - CreateIssue and Comment are NOT idempotent: every call creates a new
//     issue or comment. The caller dedupes via its store and the line-1
//     fingerprint marker before calling.
//   - UpdateIssueBody, ReopenIssue, CloseIssue, EnsureLabel, AddLabels, and
//     RemoveLabel are idempotent: repeating a successful call succeeds again
//     (closing a closed issue, reopening an open one, ensuring an existing
//     label, adding a present label, and removing an absent one are all
//     no-op successes).
package tracker

import (
	"context"
	"errors"
)

// IssueKey is the tracker-native identifier of one issue. See the package
// documentation: keys are opaque outside the adapter that minted them.
type IssueKey string

// Issue is one issue as returned by [Tracker.ListIssues], used for backsync
// and recovery listings. State is normalized by the adapter to "open" or
// "closed" regardless of the tracker's native workflow vocabulary. Body is
// the issue body restored to the canonical form described in the package
// documentation.
type Issue struct {
	Key   IssueKey
	State string
	Body  string
}

// Label describes one issue label. Color is a hex RGB string without a
// leading '#' (e.g. "d73a4a"); adapters for trackers without label colors or
// descriptions ignore the fields they cannot represent.
type Label struct {
	Name        string
	Color       string
	Description string
}

// Capabilities reports what a tracker supports. When Labels is false the
// label methods (EnsureLabel, AddLabels, RemoveLabel) return
// [ErrUnsupported] and callers skip label reconciliation entirely.
type Capabilities struct {
	Labels bool
}

// Config carries the tracker-local settings [New] hands to an adapter
// factory. It is deliberately independent of internal/config: the cli layer
// maps its own configuration onto this struct so adapters never import it.
type Config struct {
	// Labels lists the label names the publish pipeline manages on the
	// issues it creates.
	Labels []string
}

// Sentinel errors adapters wrap with %w. See the package documentation for
// when each applies.
var (
	ErrIssueGone   = errors.New("issue gone")
	ErrRateLimited = errors.New("rate limited")
	ErrUnavailable = errors.New("tracker unavailable")
	ErrUnsupported = errors.New("operation unsupported by tracker")
)

// Tracker is one issue-tracker backend. Implementations owe the idempotency,
// error-taxonomy, and body-contract guarantees in the package documentation.
type Tracker interface {
	// Name returns the registry name the adapter was registered under
	// (e.g. "github").
	Name() string

	// RepoURL returns the canonical browse URL of the repository the
	// tracker publishes for, or "" when it cannot be resolved. It is
	// used to build source permalinks in issue bodies.
	RepoURL(ctx context.Context) string

	// CreateIssue creates a new issue and returns its key. labels lists
	// label names to apply at creation; trackers without label support
	// ignore it.
	CreateIssue(ctx context.Context, title, body string, labels []string) (IssueKey, error)

	// UpdateIssueBody replaces the entire body of the issue.
	UpdateIssueBody(ctx context.Context, key IssueKey, body string) error

	// ReopenIssue reopens a closed issue and replaces its body.
	// Reopening an already-open issue succeeds.
	ReopenIssue(ctx context.Context, key IssueKey, body string) error

	// CloseIssue closes the issue. Closing an already-closed issue
	// succeeds.
	CloseIssue(ctx context.Context, key IssueKey) error

	// Comment appends a new comment to the issue.
	Comment(ctx context.Context, key IssueKey, text string) error

	// ListIssues lists issues in the adapter's configured project or
	// repository. state is "closed" (closed issues only) or "all" (open
	// and closed); adapters map these two values onto the tracker's
	// native filters.
	ListIssues(ctx context.Context, state string) ([]Issue, error)

	// Capabilities reports what this tracker supports.
	Capabilities() Capabilities

	// EnsureLabel creates the label or updates an existing label of the
	// same name to match l.
	EnsureLabel(ctx context.Context, l Label) error

	// AddLabels adds the named labels to the issue. Labels already
	// present are no-ops.
	AddLabels(ctx context.Context, key IssueKey, labels []string) error

	// RemoveLabel removes the named label from the issue. Removing an
	// absent label succeeds.
	RemoveLabel(ctx context.Context, key IssueKey, label string) error
}
