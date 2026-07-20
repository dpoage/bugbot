// Package github adapts GitHub Issues to the tracker.Tracker interface by
// shelling out to the `gh` CLI through an injected engine.GHRunner. The
// production factory (registered under "github" from init) wires
// engine.NewPacedGH(engine.RealGH) so mutating calls are paced and retried
// against GitHub's secondary rate limit exactly as the publish pipeline did
// before the tracker seam existed; tests inject a fake runner via New.
//
// Issue keys are the decimal GitHub issue number ("42"). Bodies pass through
// verbatim in both directions: GitHub renders HTML comments invisibly, so the
// canonical body contract (fingerprint marker + meta front-matter, see the
// tracker package doc) needs no transformation here.
//
// Error classification (see the tracker package doc for the caller-side
// decision table):
//
//   - gh binary missing from PATH -> tracker.ErrMissingPrereq, preserving the
//     install hint in the message.
//   - GitHub rate limiting after the paced runner's retry budget is exhausted
//     (engine.IsGHRateLimited) -> tracker.ErrRateLimited.
//   - HTTP 404/410 on an operation addressed at an existing issue key -> the
//     issue is deleted, transferred, or renamed -> tracker.ErrIssueGone.
//   - Everything else is returned unclassified: permanent GitHub-side
//     failures (422 validation) and transient 5xx alike are per-action
//     failures for the publish applier.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/tracker"
)

func init() {
	tracker.Register("github", func(cfg tracker.Config) (tracker.Tracker, error) {
		return New(engine.NewPacedGH(engine.RealGH), cfg), nil
	})
}

// New constructs the GitHub adapter over an explicit gh runner. Production
// code goes through the registry (tracker.New("github", cfg)), which wires
// the paced real runner; New exists so tests can inject a fake.
func New(gh engine.GHRunner, cfg tracker.Config) tracker.Tracker {
	return &ghTracker{gh: gh, cfg: cfg}
}

// ghTracker is the concrete adapter. It is stateless beyond its wiring: all
// idempotency guarantees ride on the GitHub API semantics of the individual
// calls (PATCHing a closed issue closed again succeeds, etc.).
type ghTracker struct {
	gh  engine.GHRunner
	cfg tracker.Config
}

func (t *ghTracker) Name() string { return "github" }

func (t *ghTracker) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{Labels: true}
}

// RepoURL fetches the repository browse URL via `gh repo view --json url -q
// .url`. Errors are tolerated: the empty string signals that permalinks
// should be omitted rather than aborting a publish run.
func (t *ghTracker) RepoURL(ctx context.Context) string {
	raw, err := t.gh(ctx, "repo", "view", "--json", "url", "-q", ".url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// CreateIssue posts a new GitHub issue and returns its number as the key.
// The title is sanitized here (not by the caller) because the NUL-stripping
// exists for exec's sake: a NUL in an argv element makes the gh fork fail
// with EINVAL, which is a property of this adapter's transport, not of the
// canonical content. The body arrives already sanitized by the renderer.
func (t *ghTracker) CreateIssue(ctx context.Context, title, body string, labels []string) (tracker.IssueKey, error) {
	title = sanitizeControlChars(title)
	args := []string{
		"api", "repos/{owner}/{repo}/issues",
		"-X", "POST",
		"-f", "title=" + title,
		"-f", "body=" + body,
	}
	for _, l := range labels {
		args = append(args, "-f", "labels[]="+l)
	}

	raw, err := t.gh(ctx, args...)
	if err != nil {
		return "", wrapErr("create issue", err)
	}

	var payload struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("github: parse create-issue response: %w", err)
	}
	if payload.Number == 0 {
		return "", errors.New("github: create-issue response missing 'number' field")
	}
	return tracker.IssueKey(strconv.Itoa(payload.Number)), nil
}

// UpdateIssueBody patches the body of an existing issue.
func (t *ghTracker) UpdateIssueBody(ctx context.Context, key tracker.IssueKey, body string) error {
	_, err := t.gh(ctx,
		"api", "repos/{owner}/{repo}/issues/"+string(key),
		"-X", "PATCH",
		"-f", "body="+body,
	)
	if err != nil {
		return wrapIssueErr("update issue", key, err)
	}
	return nil
}

// ReopenIssue reopens a closed issue and refreshes its body in a single
// PATCH (state=open, body=...). One mutating call does double duty so a
// reopened issue never shows the stale content it had when it was closed;
// reopening an already-open issue succeeds (GitHub treats it as a no-op).
func (t *ghTracker) ReopenIssue(ctx context.Context, key tracker.IssueKey, body string) error {
	_, err := t.gh(ctx,
		"api", "repos/{owner}/{repo}/issues/"+string(key),
		"-X", "PATCH",
		"-f", "state=open",
		"-f", "body="+body,
	)
	if err != nil {
		return wrapIssueErr("reopen issue", key, err)
	}
	return nil
}

// CloseIssue patches the issue state to closed. Closing an already-closed
// issue succeeds (GitHub treats it as a no-op).
func (t *ghTracker) CloseIssue(ctx context.Context, key tracker.IssueKey) error {
	_, err := t.gh(ctx,
		"api", "repos/{owner}/{repo}/issues/"+string(key),
		"-X", "PATCH",
		"-f", "state=closed",
	)
	if err != nil {
		return wrapIssueErr("close issue", key, err)
	}
	return nil
}

// Comment posts a new comment on the issue.
func (t *ghTracker) Comment(ctx context.Context, key tracker.IssueKey, text string) error {
	_, err := t.gh(ctx,
		"api", "repos/{owner}/{repo}/issues/"+string(key)+"/comments",
		"-X", "POST",
		"-f", "body="+text,
	)
	if err != nil {
		return wrapIssueErr("comment on issue", key, err)
	}
	return nil
}

// ListIssues lists the repository's issues in the given state ("closed" or
// "all"), filtered by the anchor label (cfg.Labels[0]) when one is
// configured. An empty cfg.Labels lists UNFILTERED — the pre-seam publish
// pipeline behaved exactly this way (no labels configured means no list
// filter, not an empty result), and backsync/recovery correctness depends on
// the listing actually returning the repo's issues.
func (t *ghTracker) ListIssues(ctx context.Context, state string) ([]tracker.Issue, error) {
	path := "repos/{owner}/{repo}/issues?state=" + state + "&per_page=100"
	if len(t.cfg.Labels) > 0 {
		path += "&labels=" + t.cfg.Labels[0]
	}
	raw, err := t.gh(ctx, "api", "--paginate", path)
	if err != nil {
		return nil, wrapErr("list issues", err)
	}
	pages, err := parseIssuePages(raw)
	if err != nil {
		return nil, fmt.Errorf("github: parse issues list: %w", err)
	}
	out := make([]tracker.Issue, 0, len(pages))
	for _, is := range pages {
		out = append(out, tracker.Issue{
			Key:   tracker.IssueKey(strconv.Itoa(is.Number)),
			State: is.State,
			Body:  is.Body,
		})
	}
	return out, nil
}

// EnsureLabel creates the repo-level label if it does not exist. GitHub
// surfaces "already exists" as an HTTP 422 with the "already_exists"
// validation code; any 422 on this endpoint means the label is there, which
// is success for the create-only contract (an existing label's color and
// description are deliberately left alone).
func (t *ghTracker) EnsureLabel(ctx context.Context, l tracker.Label) error {
	_, err := t.gh(ctx, "api", "repos/{owner}/{repo}/labels", "-X", "POST",
		"-f", "name="+l.Name, "-f", "color="+l.Color, "-f", "description="+l.Description)
	if err != nil && !isLabelAlreadyExists(err) {
		return wrapErr("ensure label "+l.Name, err)
	}
	return nil
}

// AddLabels adds the named labels to the issue in ONE POST (a labels[] pair
// per label). Labels already present are no-ops on the GitHub side.
func (t *ghTracker) AddLabels(ctx context.Context, key tracker.IssueKey, labels []string) error {
	args := []string{"api", "repos/{owner}/{repo}/issues/" + string(key) + "/labels", "-X", "POST"}
	for _, l := range labels {
		args = append(args, "-f", "labels[]="+l)
	}
	if _, err := t.gh(ctx, args...); err != nil {
		return wrapIssueErr("add labels to issue", key, err)
	}
	return nil
}

// RemoveLabel removes one label from the issue with a path-escaped DELETE. A
// 404/410 is success: the label is already detached (or the issue itself is
// gone, in which case there is nothing to detach) — removing an absent label
// succeeds per the interface contract.
func (t *ghTracker) RemoveLabel(ctx context.Context, key tracker.IssueKey, label string) error {
	_, err := t.gh(ctx, "api", "repos/{owner}/{repo}/issues/"+string(key)+"/labels/"+url.PathEscape(label), "-X", "DELETE")
	if err != nil && !isGHGoneOrNotFound(err) {
		return wrapIssueErr("remove label from issue", key, err)
	}
	return nil
}

// issuePage is one entry of the issues list endpoint's JSON.
type issuePage struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

// parseIssuePages decodes the (possibly concatenated, via --paginate) JSON
// arrays of the issues list endpoint.
func parseIssuePages(raw []byte) ([]issuePage, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var out []issuePage
	for dec.More() {
		var page []issuePage
		if err := dec.Decode(&page); err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

// wrapErr classifies a gh failure for operations that are not addressed at
// an existing issue key (create, list, ensure-label): the missing-binary and
// rate-limit sentinels apply; a 404 here is NOT "issue gone" (there is no
// issue key it could refer to) and passes through unclassified.
func wrapErr(op string, err error) error {
	if isGHMissing(err) {
		return errGHRequired()
	}
	if engine.IsGHRateLimited(err) {
		return fmt.Errorf("github: %s: %w: %w", op, tracker.ErrRateLimited, err)
	}
	return fmt.Errorf("github: %s: %w", op, err)
}

// wrapIssueErr classifies a gh failure for operations addressed at an
// existing issue key: like wrapErr, plus 404/410 -> tracker.ErrIssueGone
// (the issue was deleted, transferred, or renamed and the caller's row is
// stale).
func wrapIssueErr(op string, key tracker.IssueKey, err error) error {
	if isGHMissing(err) {
		return errGHRequired()
	}
	if engine.IsGHRateLimited(err) {
		return fmt.Errorf("github: %s %s: %w: %w", op, key, tracker.ErrRateLimited, err)
	}
	if isGHGoneOrNotFound(err) {
		return fmt.Errorf("github: %s %s: %w: %w", op, key, tracker.ErrIssueGone, err)
	}
	return fmt.Errorf("github: %s %s: %w", op, key, err)
}

// errGHRequired is the uniform user-facing error for a missing gh binary,
// wrapping tracker.ErrMissingPrereq so callers can latch publishing off for
// the process lifetime while the message keeps the install hint.
func errGHRequired() error {
	return fmt.Errorf("github: gh CLI is required but was not found on PATH; install gh from https://cli.github.com: %w", tracker.ErrMissingPrereq)
}

// isGHMissing reports whether the error indicates gh was not found on PATH.
// engine.RealGH wraps the exec error via %w so errors.Is finds it; the
// string check covers fake runners that return a plain error.
func isGHMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found")
}

// isGHGoneOrNotFound reports whether the error indicates the GitHub issue no
// longer exists at the recorded number. We detect the two HTTP statuses that
// mean "this issue is gone, the caller's row is stale":
//   - 410 Gone: issue was deleted
//   - 404 Not Found: issue was deleted, transferred, or renamed
//
// gh surfaces these in the stderr text it returns; the wording varies across
// gh versions ("was deleted", "Gone", "Not Found", the raw status code). We
// match gh's HTTP status token, not a bare "404"/"410" substring: the
// wrapped error always embeds the issue number and API path, so a bare match
// misfires on issues numbered like #404/#410 for any unrelated failure
// (rate-limit, 5xx, validation). The human-readable "was deleted" phrase is
// the fallback.
func isGHGoneOrNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "HTTP 410") {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "was deleted")
}

// isLabelAlreadyExists reports whether a label-create POST failed only
// because the label is already there. gh surfaces the API's
// "already_exists" validation code (HTTP 422) in the error text; any 422 on
// this endpoint means the label exists, so EnsureLabel treats it as success
// (idempotent).
func isLabelAlreadyExists(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "already_exists") || strings.Contains(msg, "422")
}

// sanitizeControlChars strips C0 control characters (U+0000–U+001F) except
// tab, newline, and carriage return. A NUL in particular makes exec reject
// the gh argument with EINVAL ("invalid argument"); the rest have no place
// in issue text and render as replacement glyphs on GitHub. Deliberately
// duplicated from internal/cli's copy (which owns whole-body sanitization at
// render time): the title strip lives here because it protects this
// adapter's exec transport, and importing cli from an adapter would invert
// the dependency direction.
//
// The scan is byte-wise: every C0 control byte is < 0x80, so it can never be
// part of a multi-byte UTF-8 sequence, and stripping it can never split a
// rune. All other bytes (including multi-byte runes and any invalid UTF-8)
// are copied through verbatim — this never normalizes or rewrites valid
// content, and the no-control-char common case returns s unchanged with no
// allocation.
func sanitizeControlChars(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if isC0Control(s[i]) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if !isC0Control(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isC0Control reports whether c is a C0 control byte that should be removed
// from issue text. Tab, newline, and carriage return are preserved because
// they are meaningful Markdown whitespace.
func isC0Control(c byte) bool {
	return c < 0x20 && c != '\t' && c != '\n' && c != '\r'
}
