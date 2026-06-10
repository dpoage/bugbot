package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/store"
)

// publishGH is a package-level seam that tests can replace with a fakeGH.
// Production code leaves it nil and the command wires in realGH at run time.
var publishGH ghRunner

// newPublishCmd implements `bugbot publish`: files open findings as GitHub
// issues via gh, idempotently, and closes linked issues when findings are
// auto-closed. The manual command always runs; the daemon hook is gated by
// cfg.Publish.Enabled.
func newPublishCmd() *cobra.Command {
	var (
		dryRun  bool
		tierMin int
	)

	cmd := &cobra.Command{
		Use:   "publish [flags]",
		Short: "File open findings as GitHub issues (idempotent)",
		Long: `Publish open findings to GitHub Issues via the gh CLI.

On each run it:
  - Creates a new GitHub issue for every open finding with Tier <= tier_min
    that has not yet been filed.
  - Skips findings whose published issue is already up-to-date.
  - Updates the issue body if the finding was updated more recently than the
    last publish (UpdatedAt > published.updated_at).
  - Closes the GitHub issue (and posts a comment) for findings that have been
    fixed or dismissed, when close_on_fixed is true.

Re-running files zero duplicates (idempotent via the published_issues table).
Requires the gh CLI to be installed and authenticated.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, st, err := openStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			// --tier-min flag overrides config if explicitly set.
			effective := cfg.Publish.TierMin
			if cmd.Flags().Changed("tier-min") {
				effective = tierMin
			}

			gh := publishGH
			if gh == nil {
				gh = realGH
			}

			return runPublish(ctx, cmd.OutOrStdout(), gh, st, cfg.Publish, effective, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without executing them")
	cmd.Flags().IntVar(&tierMin, "tier-min", 2, "maximum tier to publish (inclusive; overrides config)")
	return cmd
}

// runPublish is the entry point for both the command and the daemon hook. It
// loads findings and published_issues, plans the reconcile, and applies it.
// w receives the human-readable summary; pass cmd.OutOrStdout() from a cobra
// command or any io.Writer from the daemon hook.
func runPublish(ctx context.Context, w io.Writer, gh ghRunner, st *store.Store, cfg config.Publish, tierMin int, dryRun bool) error {

	// Gather inputs for the pure planner.
	openFindings, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen})
	if err != nil {
		return fmt.Errorf("publish: list open findings: %w", err)
	}

	// Also load fixed+dismissed to drive close actions.
	fixedFindings, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusFixed})
	if err != nil {
		return fmt.Errorf("publish: list fixed findings: %w", err)
	}
	dismissedFindings, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusDismissed})
	if err != nil {
		return fmt.Errorf("publish: list dismissed findings: %w", err)
	}

	published, err := st.ListPublishedIssues(ctx)
	if err != nil {
		return fmt.Errorf("publish: list published issues: %w", err)
	}
	publishedMap := make(map[string]store.PublishedIssue, len(published))
	for _, pi := range published {
		publishedMap[pi.Fingerprint] = pi
	}

	plan := planPublish(openFindings, fixedFindings, dismissedFindings, publishedMap, tierMin, cfg.CloseOnFixed)

	// Resolve the repo URL once; tolerate failure (degrade: no permalinks).
	repoURL := resolveRepoURL(ctx, gh)

	created, updated, closed, skipped := 0, 0, 0, 0

	for _, a := range plan {
		switch a.op {
		case publishOpCreate:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: create issue for %s (%s)\n", a.finding.Fingerprint[:12], a.finding.Title)
				created++
				continue
			}
			// A create spans two systems (GitHub + our store) with no
			// transaction. Record a "pending" row FIRST so a crash between the
			// gh create and the store write leaves a tombstone: the next run
			// plans a recover (marker search) instead of blindly creating a
			// duplicate issue.
			if err := st.UpsertPublishedIssue(ctx, a.finding.Fingerprint, 0, "pending"); err != nil {
				return fmt.Errorf("publish: record pending issue: %w", err)
			}
			n, err := ghCreateIssue(ctx, gh, a.finding.Title, renderIssueBody(a.finding, repoURL), cfg.Labels)
			if err != nil {
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, a.finding.Fingerprint, n, "open"); err != nil {
				return fmt.Errorf("publish: record created issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "created issue #%d for %s (%s)\n", n, a.finding.Fingerprint[:12], a.finding.Title)
			created++

		case publishOpRecover:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: recover pending publish for %s\n", a.finding.Fingerprint[:12])
				skipped++
				continue
			}
			// A prior run recorded "pending" but never recorded the issue
			// number — the gh create may or may not have happened. Search the
			// repo's bugbot issues for the fingerprint marker; adopt on hit,
			// create on miss.
			n, found, err := findIssueByMarker(ctx, gh, cfg.Labels, a.finding.Fingerprint)
			if err != nil {
				return fmt.Errorf("publish: recover pending issue: %w", err)
			}
			if !found {
				n, err = ghCreateIssue(ctx, gh, a.finding.Title, renderIssueBody(a.finding, repoURL), cfg.Labels)
				if err != nil {
					return err
				}
				created++
				_, _ = fmt.Fprintf(w, "created issue #%d for %s (recovered pending; no existing issue found)\n", n, a.finding.Fingerprint[:12])
			} else {
				_, _ = fmt.Fprintf(w, "recovered issue #%d for %s (adopted via fingerprint marker)\n", n, a.finding.Fingerprint[:12])
			}
			if err := st.UpsertPublishedIssue(ctx, a.finding.Fingerprint, n, "open"); err != nil {
				return fmt.Errorf("publish: record recovered issue: %w", err)
			}

		case publishOpUpdate:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: update issue #%d for %s\n", a.issueNumber, a.finding.Fingerprint[:12])
				updated++
				continue
			}
			if err := ghUpdateIssue(ctx, gh, a.issueNumber, renderIssueBody(a.finding, repoURL)); err != nil {
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, a.finding.Fingerprint, a.issueNumber, "open"); err != nil {
				return fmt.Errorf("publish: record updated issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "updated issue #%d for %s\n", a.issueNumber, a.finding.Fingerprint[:12])
			updated++

		case publishOpClose:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: close issue #%d for %s (status: %s)\n", a.issueNumber, a.finding.Fingerprint[:12], a.finding.Status)
				closed++
				continue
			}
			// The close also spans two gh writes (comment, then state PATCH).
			// Record "closing" once the comment lands so a PATCH failure does
			// NOT re-post the comment on every subsequent cycle — the resume
			// path (skipComment) goes straight to the PATCH.
			if !a.skipComment {
				if err := ghCommentIssue(ctx, gh, a.issueNumber, autoCloseComment(string(a.finding.Status))); err != nil {
					return err
				}
				if err := st.UpsertPublishedIssue(ctx, a.finding.Fingerprint, a.issueNumber, "closing"); err != nil {
					return fmt.Errorf("publish: record closing issue: %w", err)
				}
			}
			if err := ghPatchIssueClosed(ctx, gh, a.issueNumber); err != nil {
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, a.finding.Fingerprint, a.issueNumber, "closed"); err != nil {
				return fmt.Errorf("publish: record closed issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "closed issue #%d for %s (status: %s)\n", a.issueNumber, a.finding.Fingerprint[:12], a.finding.Status)
			closed++

		case publishOpSkip:
			skipped++
		}
	}

	_, _ = fmt.Fprintf(w, "publish: created=%d updated=%d closed=%d skipped=%d\n", created, updated, closed, skipped)
	return nil
}

// publishOp is a planned action in the publish reconcile cycle.
type publishOp int

const (
	publishOpCreate  publishOp = iota
	publishOpRecover           // pending row from an interrupted create: search-then-adopt-or-create
	publishOpUpdate
	publishOpClose
	publishOpSkip
)

// publishAction is one unit of planned publish work.
type publishAction struct {
	op          publishOp
	finding     store.Finding
	issueNumber int // set for update/close/skip
	// skipComment resumes an interrupted close (state "closing"): the auto-close
	// comment already landed, only the state PATCH remains.
	skipComment bool
}

// planPublish is the pure reconciler: given open/fixed/dismissed findings and
// the current published_issues map, it decides what to do with each finding.
//
// Inclusion rule: a finding is considered for publication when
// Tier <= tierMin (lower = stronger: 0=fix-witnessed, 1=reproduced, 2=verified,
// 3=suspected). tier_min=0 therefore publishes only fix-witnessed findings.
//
// Update heuristic: rather than fetching the current issue body (which would
// require a gh read per issue), we use finding.UpdatedAt > published.UpdatedAt
// as a proxy. If the finding was updated after the last publish, we re-push
// the body. This is cheap (no gh reads) at the cost of a no-op PATCH on
// every finding whose metadata was touched. Document the trade-off and accept
// it; a true body-diff would require a gh read per issue.
//
// Close rule: if close_on_fixed is true, any finding with status fixed or
// dismissed whose published row state is "open" gets a close action.
func planPublish(
	open, fixed, dismissed []store.Finding,
	published map[string]store.PublishedIssue,
	tierMin int,
	closeOnFixed bool,
) []publishAction {
	var actions []publishAction

	// Create/recover/update/skip for open findings within tier.
	for _, f := range open {
		if f.Tier > tierMin {
			continue // outside publication window
		}
		pi, found := published[f.Fingerprint]
		switch {
		case !found:
			actions = append(actions, publishAction{op: publishOpCreate, finding: f})
		case pi.State == "pending":
			// An earlier create was interrupted between the gh call and the
			// store write; the issue may or may not exist on GitHub.
			actions = append(actions, publishAction{op: publishOpRecover, finding: f})
		case f.UpdatedAt.After(pi.UpdatedAt):
			// Published row exists ("open", or "closing" from a reintroduced
			// finding — the body re-push is correct either way). If the finding
			// was updated after our last publish, re-push the body.
			actions = append(actions, publishAction{op: publishOpUpdate, finding: f, issueNumber: pi.IssueNumber})
		default:
			actions = append(actions, publishAction{op: publishOpSkip, finding: f, issueNumber: pi.IssueNumber})
		}
	}

	if !closeOnFixed {
		return actions
	}

	// Close actions for fixed/dismissed findings with published rows that
	// haven't completed a close. "closing" rows resume without re-posting the
	// auto-close comment (it already landed). "pending" rows are skipped: there
	// is no known issue number to close, and the finding is gone — the rare
	// interrupted-create-then-fixed overlap is left for a future open cycle.
	for _, f := range append(fixed, dismissed...) {
		pi, found := published[f.Fingerprint]
		if !found || pi.State == "closed" || pi.State == "pending" {
			continue
		}
		actions = append(actions, publishAction{
			op: publishOpClose, finding: f, issueNumber: pi.IssueNumber,
			skipComment: pi.State == "closing",
		})
	}

	return actions
}

// findIssueByMarker lists the repo's bugbot issues (filtered by the first
// configured label when present) and returns the number of the issue whose
// body carries the fingerprint marker. Used only on the rare recover path.
func findIssueByMarker(ctx context.Context, gh ghRunner, labels []string, fingerprint string) (int, bool, error) {
	path := "repos/{owner}/{repo}/issues?state=all&per_page=100"
	if len(labels) > 0 {
		path += "&labels=" + labels[0]
	}
	raw, err := gh(ctx, "api", "--paginate", path)
	if err != nil {
		if isGHMissing(err) {
			return 0, false, errGHRequired()
		}
		return 0, false, fmt.Errorf("list issues: %w", err)
	}
	issues, err := parsePublishIssues(raw)
	if err != nil {
		return 0, false, fmt.Errorf("parse issues list: %w", err)
	}
	marker := "<!-- bugbot:fp=" + fingerprint + " -->"
	for _, is := range issues {
		if strings.Contains(is.Body, marker) {
			return is.Number, true, nil
		}
	}
	return 0, false, nil
}

// parsePublishIssues decodes the (possibly concatenated, via --paginate) JSON
// arrays of the issues list endpoint.
func parsePublishIssues(raw []byte) ([]publishIssue, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var out []publishIssue
	for dec.More() {
		var page []publishIssue
		if err := dec.Decode(&page); err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

type publishIssue struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

// renderIssueBody renders the deterministic issue body for a finding. The
// fingerprint marker is always the first line so the body is recoverable
// without server-side state. When repoURL and CommitSHA are both non-empty a
// permalink to the exact location is appended.
func renderIssueBody(f store.Finding, repoURL string) string {
	var b strings.Builder

	// First line: hidden fingerprint marker (matches the PR-comment pattern).
	fmt.Fprintf(&b, "<!-- bugbot:fp=%s -->\n\n", f.Fingerprint)

	// Title as heading.
	fmt.Fprintf(&b, "## %s\n\n", titleOrUnknown(f.Title))

	// Meta block.
	fmt.Fprintf(&b, "**Tier:** %s  \n", tierLabel(f.Tier))
	fmt.Fprintf(&b, "**Severity:** %s  \n", severityLabel(f.Severity))
	fmt.Fprintf(&b, "**Lens:** %s  \n", f.Lens)
	if len(f.CorroboratingLenses) > 0 {
		fmt.Fprintf(&b, "**Also reported by:** %s  \n", strings.Join(f.CorroboratingLenses, ", "))
	}
	fmt.Fprintf(&b, "**Location:** `%s:%d`  \n\n", f.File, f.Line)

	if f.Description != "" {
		b.WriteString(f.Description)
		b.WriteString("\n\n")
	}

	if f.Reasoning != "" {
		// GitHub rejects issue bodies over 65536 chars (HTTP 422). The
		// reasoning trace is the only unbounded field; cap it well under the
		// limit so a long trace degrades instead of failing the whole run.
		reasoning := f.Reasoning
		const maxReasoning = 30 * 1024
		if len(reasoning) > maxReasoning {
			reasoning = reasoning[:maxReasoning] + "\n\n[... truncated by bugbot: full trace exceeds GitHub's body limit ...]"
		}
		b.WriteString("<details><summary>Verification trace</summary>\n\n")
		b.WriteString(reasoning)
		b.WriteString("\n\n</details>\n\n")
	}

	if f.ReproPath != "" {
		fmt.Fprintf(&b, "_Reproduction script: `%s`_\n\n", f.ReproPath)
	}

	// Permalink: only when both repo URL and commit SHA are available.
	if repoURL != "" && f.CommitSHA != "" && f.File != "" {
		fmt.Fprintf(&b, "[View in source](%s/blob/%s/%s#L%d)\n", repoURL, f.CommitSHA, f.File, f.Line)
	}

	return strings.TrimRight(b.String(), "\n")
}

// resolveRepoURL fetches the GitHub repo URL via `gh repo view --json url -q
// .url`. Errors are tolerated: the function returns "" to signal that
// permalinks should be omitted rather than aborting the publish run.
func resolveRepoURL(ctx context.Context, gh ghRunner) string {
	raw, err := gh(ctx, "repo", "view", "--json", "url", "-q", ".url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ghCreateIssue posts a new GitHub issue and returns the issue number.
func ghCreateIssue(ctx context.Context, gh ghRunner, title, body string, labels []string) (int, error) {
	args := []string{
		"api", "repos/{owner}/{repo}/issues",
		"-X", "POST",
		"-f", "title=" + title,
		"-f", "body=" + body,
	}
	for _, l := range labels {
		args = append(args, "-f", "labels[]="+l)
	}

	raw, err := gh(ctx, args...)
	if err != nil {
		// Detect gh-not-found: exec.ErrNotFound semantics surfaced in the error string.
		if isGHMissing(err) {
			return 0, errGHRequired()
		}
		return 0, fmt.Errorf("publish: create issue: %w", err)
	}

	var payload struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("publish: parse create-issue response: %w", err)
	}
	if payload.Number == 0 {
		return 0, fmt.Errorf("publish: create-issue response missing 'number' field")
	}
	return payload.Number, nil
}

// ghUpdateIssue patches the body of an existing GitHub issue.
func ghUpdateIssue(ctx context.Context, gh ghRunner, number int, body string) error {
	_, err := gh(ctx,
		"api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number),
		"-X", "PATCH",
		"-f", "body="+body,
	)
	if err != nil {
		if isGHMissing(err) {
			return errGHRequired()
		}
		return fmt.Errorf("publish: update issue #%d: %w", number, err)
	}
	return nil
}

// autoCloseComment renders the timeline comment posted before closing.
func autoCloseComment(status string) string {
	return fmt.Sprintf(
		"Auto-closed by bugbot: this finding is no longer reported (status: %s).",
		status,
	)
}

// ghCommentIssue posts a comment on the issue. The caller records the
// "closing" state between this and the state PATCH so an interrupted close
// never re-posts the comment.
func ghCommentIssue(ctx context.Context, gh ghRunner, number int, comment string) error {
	_, err := gh(ctx,
		"api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", number),
		"-X", "POST",
		"-f", "body="+comment,
	)
	if err != nil {
		if isGHMissing(err) {
			return errGHRequired()
		}
		return fmt.Errorf("publish: post close comment on issue #%d: %w", number, err)
	}
	return nil
}

// ghPatchIssueClosed patches the issue state to closed.
func ghPatchIssueClosed(ctx context.Context, gh ghRunner, number int) error {
	_, err := gh(ctx,
		"api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number),
		"-X", "PATCH",
		"-f", "state=closed",
	)
	if err != nil {
		if isGHMissing(err) {
			return errGHRequired()
		}
		return fmt.Errorf("publish: close issue #%d: %w", number, err)
	}
	return nil
}

// errGHMissing is the sentinel for a missing gh binary, so callers up the
// stack (the daemon publisher's warn-once latch) can detect the condition with
// errors.Is even after the message has been made user-friendly.
var errGHMissing = errors.New("gh CLI not found on PATH")

// errGHRequired is the uniform user-facing error for a missing gh binary.
func errGHRequired() error {
	return fmt.Errorf("publish: gh CLI is required but was not found on PATH; install gh from https://cli.github.com: %w", errGHMissing)
}

// isGHMissing reports whether the error indicates gh was not found on PATH.
// The realGH runner wraps the exec error via %w so errors.As finds it; we
// also check the string for fake runners that return a plain error.
func isGHMissing(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, errGHMissing) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found")
}
