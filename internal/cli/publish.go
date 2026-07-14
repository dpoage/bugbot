package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/engine"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// publishGH is a package-level seam that tests can replace with a fakeGH.
// Production code leaves it nil and the command wires in engine.RealGH at run time.
var publishGH engine.GHRunner

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
  - Backsyncs GitHub -> local first: any published issue a human closed
    directly on GitHub is detected (one issues-list call, skipped entirely
    when nothing could have been closed) and pulled into the store -- the
    open finding is dismissed (so it is not re-reported) and its row is
    marked closed. This never posts a comment or PATCHes the issue; the
    human's close stands as-is.
  - Creates a new GitHub issue for every open finding with Tier <= tier_min
    that has not yet been filed.
  - Skips findings whose published issue is already up-to-date.
  - Updates the issue body if the finding was updated more recently than the
    last publish (UpdatedAt > published.updated_at).
  - Reopens the GitHub issue (state PATCH + fresh body, then a comment) for
    a finding that regressed after bugbot itself had closed it -- backsync
    above already ensures a still-open finding pointing at a closed row can
    only be a bugbot-closed regression, never a human close.
  - Closes the GitHub issue (and posts a comment) for findings that have been
    fixed or dismissed, when close_on_fixed is true.

Re-running files zero duplicates (idempotent via the published_issues table).
Requires the gh CLI to be installed and authenticated.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			cfg, st, err := cmdOpenStore(ctx, configPathFromCmd(cmd))
			if err != nil {
				return err
			}
			defer closeStore(st)

			// --tier-min flag overrides config if explicitly set.
			effective := cfg.Publish.TierMin
			if cmd.Flags().Changed("tier-min") {
				effective = tierMin
			}

			gh := publishGH
			if gh == nil {
				gh = engine.NewPacedGH(engine.RealGH)
			}

			prov := provenanceFromConfig(cfg)
			return runPublish(ctx, cmd.OutOrStdout(), gh, st, cfg.Publish, prov, effective, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without executing them")
	cmd.Flags().IntVar(&tierMin, "tier-min", 2, "maximum tier to publish (inclusive; overrides config)")
	return cmd
}

// publishProvenance carries the model and provider strings from the active
// config roles. It is populated at publish time from the full Config (no schema
// migration required) and threaded through to renderIssueBody for the metadata
// block. Fields may be empty when no config is available (tests, daemon paths).
type publishProvenance struct {
	FinderModel   string
	VerifierModel string
	ProviderType  string // type field from the finder's provider, e.g. "anthropic"
}

// provenanceFromConfig extracts model/provider strings from a loaded Config.
func provenanceFromConfig(cfg config.Config) publishProvenance {
	prov := publishProvenance{
		FinderModel:   cfg.Roles.Finder.Model,
		VerifierModel: cfg.Roles.Verifier.Model,
	}
	if p, ok := cfg.Providers[cfg.Roles.Finder.Provider]; ok {
		prov.ProviderType = string(p.Type)
	}
	return prov
}

// runPublish is the entry point for both the command and the daemon hook. It
// loads findings and published_issues, plans the reconcile, and applies it.
// w receives the human-readable summary; pass cmd.OutOrStdout() from a cobra
// command or any io.Writer from the daemon hook.
func runPublish(ctx context.Context, w io.Writer, gh engine.GHRunner, st *store.Store, cfg config.Publish, prov publishProvenance, tierMin int, dryRun bool) error {

	// Gather inputs for the pure planner.
	openFindings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		return fmt.Errorf("publish: list open findings: %w", err)
	}

	// Also load fixed+dismissed+superseded to drive close actions. Superseded
	// (backlog reconcile, bugbot-ezmx.4) is a merged-away duplicate, closed
	// exactly like fixed/dismissed so its GitHub issue does not stay open
	// indefinitely after the underlying defect is now tracked under the
	// canonical finding.
	fixedFindings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusFixed})
	if err != nil {
		return fmt.Errorf("publish: list fixed findings: %w", err)
	}
	dismissedFindings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusDismissed})
	if err != nil {
		return fmt.Errorf("publish: list dismissed findings: %w", err)
	}
	supersededFindings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusSuperseded})
	if err != nil {
		return fmt.Errorf("publish: list superseded findings: %w", err)
	}

	published, err := st.ListPublishedIssues(ctx)
	if err != nil {
		return fmt.Errorf("publish: list published issues: %w", err)
	}
	publishedMap := make(map[string]store.PublishedIssue, len(published))
	for _, pi := range published {
		publishedMap[pi.Fingerprint] = pi
	}

	// ---- Backsync: GitHub -> local (bugbot-fchv) ----
	//
	// planPublish only ever reads local state; nothing above this point looks
	// at what GitHub itself currently thinks. Two local/remote-drift bugs
	// follow from that gap:
	//
	//  1. A human closes a bugbot-filed issue on GitHub as a triage signal.
	//     Without backsync the local finding stays "open" forever, and the
	//     next UpdatedAt bump plans a body PATCH on the closed issue and
	//     upserts the row back to "open" -- the human's signal is silently
	//     discarded.
	//  2. store.ReopenAsRegression flips a fixed finding back to open while
	//     its published row is still "closed" (bugbot closed it, then the
	//     defect came back). Nothing PATCHes state=open, so the GitHub issue
	//     stays closed even though we are actively re-tracking it.
	//
	// This step resolves (1): any published row still "open"/"closing" whose
	// GitHub issue is now closed is backsynced -- the row is marked closed,
	// and if the local finding is still open we dismiss it (StatusDismissed
	// records suppression memory, store/findings.go:428-455 -- a human
	// closing our issue means "don't show me this again"). (2) is resolved
	// below by planPublish's own IssueStateClosed case, which only ever
	// fires for rows backsync did NOT touch (they were already closed before
	// this run), so a still-open finding pointing at one is unambiguously a
	// bugbot-closed regression, not a human close.
	//
	// Cost: one extra `gh api issues?state=closed` list call, and only when
	// at least one row is in a state that could have been closed on GitHub
	// (open/closing) -- a store with everything already closed costs zero
	// gh calls.
	backsynced := 0
	needsBacksync := false
	for _, pi := range published {
		if pi.State == store.IssueStateOpen || pi.State == store.IssueStateClosing {
			needsBacksync = true
			break
		}
	}
	if needsBacksync {
		closedIssues, err := listBugbotIssues(ctx, gh, cfg.Labels, "closed")
		if err != nil {
			return fmt.Errorf("publish: backsync: list closed issues: %w", err)
		}
		closedNums := make(map[int]bool, len(closedIssues))
		for _, is := range closedIssues {
			// The listing endpoint is shared with pull requests (issues and
			// PRs share one number namespace on GitHub); a defensive check
			// on State keeps a malformed/unexpected entry from ever counting
			// as "closed" here even though we only ever match numbers we
			// ourselves recorded in published_issues.
			if is.State != "closed" {
				continue
			}
			closedNums[is.Number] = true
		}

		findingByFP := make(map[string]domain.Finding, len(openFindings)+len(fixedFindings)+len(dismissedFindings)+len(supersededFindings))
		for _, group := range [][]domain.Finding{openFindings, fixedFindings, dismissedFindings, supersededFindings} {
			for _, f := range group {
				findingByFP[f.Fingerprint] = f
			}
		}

		backsyncActions := planBacksync(publishedMap, closedNums, findingByFP)

		// Apply first (or print, under dry-run) -- but the two reconciliation
		// steps below (publishedMap, dismissedFPs) are ALWAYS derived from
		// backsyncActions itself, never from what the apply loop happened to
		// do. backsyncActions is identical in both modes, so dry-run and the
		// real run must reconcile identically; deriving dismissedFPs only
		// inside the non-dry-run branch (as an earlier version of this code
		// did) left dry-run's publishedMap forced to "closed" while
		// dismissedFPs stayed empty -- planPublish then saw an open finding
		// sitting on a "closed" row and planned a spurious reopen for the
		// very issue backsync had just decided to dismiss.
		for _, ba := range backsyncActions {
			status := "n/a"
			if f, ok := findingByFP[ba.fingerprint]; ok {
				status = string(f.Status)
			}
			if dryRun {
				if ba.dismissFinding {
					_, _ = fmt.Fprintf(w, "dry-run: backsync issue #%d for %s (closed on GitHub; would dismiss finding)\n", ba.issueNumber, ba.fingerprint[:12])
				} else {
					_, _ = fmt.Fprintf(w, "dry-run: backsync issue #%d for %s (closed on GitHub; finding already %s)\n", ba.issueNumber, ba.fingerprint[:12], status)
				}
				backsynced++
				continue
			}
			if ba.dismissFinding {
				reason := fmt.Sprintf("GitHub issue #%d was closed manually; dismissed by publish backsync", ba.issueNumber)
				if err := st.UpdateStatus(ctx, ba.fingerprint, domain.StatusDismissed, reason); err != nil {
					return fmt.Errorf("publish: backsync dismiss %s: %w", ba.fingerprint[:12], err)
				}
				_, _ = fmt.Fprintf(w, "backsynced issue #%d for %s (closed on GitHub; finding dismissed)\n", ba.issueNumber, ba.fingerprint[:12])
			} else {
				_, _ = fmt.Fprintf(w, "backsynced issue #%d for %s (closed on GitHub; finding already %s)\n", ba.issueNumber, ba.fingerprint[:12], status)
			}
			if err := st.UpsertPublishedIssue(ctx, ba.fingerprint, ba.issueNumber, store.IssueStateClosed); err != nil {
				return fmt.Errorf("publish: backsync record closed %s: %w", ba.fingerprint[:12], err)
			}
			backsynced++
		}

		// Keep the in-memory plan inputs consistent with what was just
		// applied (or would be, under dry-run -- the printed plan must match
		// what planPublish would do next) so planPublish never re-touches a
		// row this step already closed out. Derived from backsyncActions,
		// not from the apply loop above, for the reason in the comment there.
		dismissedFPs := make(map[string]bool, len(backsyncActions))
		for _, ba := range backsyncActions {
			publishedMap[ba.fingerprint] = store.PublishedIssue{
				Fingerprint: ba.fingerprint,
				IssueNumber: ba.issueNumber,
				State:       store.IssueStateClosed,
			}
			if ba.dismissFinding {
				dismissedFPs[ba.fingerprint] = true
			}
		}
		if len(dismissedFPs) > 0 {
			kept := openFindings[:0]
			for _, f := range openFindings {
				if !dismissedFPs[f.Fingerprint] {
					kept = append(kept, f)
				}
			}
			openFindings = kept
		}
	}

	plan := planPublish(openFindings, fixedFindings, dismissedFindings, supersededFindings, publishedMap, tierMin, cfg.CloseOnFixed)

	// Resolve the repo URL once; tolerate failure (degrade: no permalinks).
	repoURL := resolveRepoURL(ctx, gh)

	created, updated, adopted, reopened, closed, skipped, stale := 0, 0, 0, 0, 0, 0, 0

	for _, a := range plan {
		switch act := a.(type) {
		case publishCreate:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: create issue for %s (%s)\n", act.finding.Fingerprint[:12], act.finding.Title)
				created++
				continue
			}
			// A create spans two systems (GitHub + our store) with no
			// transaction. Record a "pending" row FIRST so a crash between the
			// gh create and the store write leaves a tombstone: the next run
			// plans a recover (marker search) instead of blindly creating a
			// duplicate issue.
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, 0, store.IssueStatePending); err != nil {
				return fmt.Errorf("publish: record pending issue: %w", err)
			}
			n, err := ghCreateIssue(ctx, gh, act.finding.Title, renderIssueBody(act.finding, repoURL, prov), cfg.Labels)
			if err != nil {
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, n, store.IssueStateOpen); err != nil {
				return fmt.Errorf("publish: record created issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "created issue #%d for %s (%s)\n", n, act.finding.Fingerprint[:12], act.finding.Title)
			created++

		case publishAdopt:
			// A re-discovered finding whose fingerprint drifted: adopt the existing
			// issue (record the new fingerprint -> issue mapping) rather than file a
			// duplicate. No gh write; the next cycle's update/skip path takes over.
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: adopt issue #%d for %s (re-discovered; fingerprint drifted)\n", act.issueNumber, act.finding.Fingerprint[:12])
				adopted++
				continue
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, act.issueNumber, store.IssueStateOpen); err != nil {
				return fmt.Errorf("publish: record adopted issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "adopted issue #%d for %s (re-discovered; fingerprint drifted)\n", act.issueNumber, act.finding.Fingerprint[:12])
			adopted++

		case publishRecover:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: recover pending publish for %s\n", act.finding.Fingerprint[:12])
				skipped++
				continue
			}
			// A prior run recorded "pending" but never recorded the issue
			// number — the gh create may or may not have happened. Search the
			// repo's bugbot issues for the fingerprint marker; adopt on hit,
			// create on miss.
			n, found, err := findIssueByMarker(ctx, gh, cfg.Labels, act.finding.Fingerprint)
			if err != nil {
				return fmt.Errorf("publish: recover pending issue: %w", err)
			}
			if !found {
				n, err = ghCreateIssue(ctx, gh, act.finding.Title, renderIssueBody(act.finding, repoURL, prov), cfg.Labels)
				if err != nil {
					return err
				}
				created++
				_, _ = fmt.Fprintf(w, "created issue #%d for %s (recovered pending; no existing issue found)\n", n, act.finding.Fingerprint[:12])
			} else {
				_, _ = fmt.Fprintf(w, "recovered issue #%d for %s (adopted via fingerprint marker)\n", n, act.finding.Fingerprint[:12])
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, n, store.IssueStateOpen); err != nil {
				return fmt.Errorf("publish: record recovered issue: %w", err)
			}

		case publishUpdate:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: update issue #%d for %s\n", act.issueNumber, act.finding.Fingerprint[:12])
				updated++
				continue
			}
			if err := ghUpdateIssue(ctx, gh, act.issueNumber, renderIssueBody(act.finding, repoURL, prov)); err != nil {
				if isGHGoneOrNotFound(err) {
					// Local row is stale: the issue was deleted (410) or
					// transferred/renamed (404) on GitHub. Drop the row, create
					// a fresh issue, and continue with the rest of the plan.
					_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue #%d gone: %v); re-creating\n", act.finding.Fingerprint[:12], act.issueNumber, err)
					if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
						return fmt.Errorf("publish: delete stale published issue: %w", derr)
					}
					n, cerr := ghCreateIssue(ctx, gh, act.finding.Title, renderIssueBody(act.finding, repoURL, prov), cfg.Labels)
					if cerr != nil {
						return fmt.Errorf("publish: recreate issue after stale: %w", cerr)
					}
					if uerr := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, n, store.IssueStateOpen); uerr != nil {
						return fmt.Errorf("publish: record recreated issue: %w", uerr)
					}
					_, _ = fmt.Fprintf(w, "recreated issue #%d for %s (replaced stale row)\n", n, act.finding.Fingerprint[:12])
					stale++
					created++
					continue
				}
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, act.issueNumber, store.IssueStateOpen); err != nil {
				return fmt.Errorf("publish: record updated issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "updated issue #%d for %s\n", act.issueNumber, act.finding.Fingerprint[:12])
			updated++

		case publishReopen:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: reopen issue #%d for %s (regression)\n", act.issueNumber, act.finding.Fingerprint[:12])
				reopened++
				continue
			}
			// Backsync (above) already turned every human-closed row into
			// IssueStateClosed and dismissed the finding, so an OPEN finding
			// whose row is still closed at this point can only be a
			// bugbot-closed regression (store.ReopenAsRegression). One PATCH
			// both flips state=open and refreshes the body, so a reopened
			// issue never shows stale content from before the fix regressed.
			if err := ghReopenIssue(ctx, gh, act.issueNumber, renderIssueBody(act.finding, repoURL, prov)); err != nil {
				if isGHGoneOrNotFound(err) {
					// Same stale-row handling as publishUpdate: the issue is
					// gone, drop the row and file a fresh one.
					_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue #%d gone on reopen: %v); re-creating\n", act.finding.Fingerprint[:12], act.issueNumber, err)
					if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
						return fmt.Errorf("publish: delete stale published issue: %w", derr)
					}
					n, cerr := ghCreateIssue(ctx, gh, act.finding.Title, renderIssueBody(act.finding, repoURL, prov), cfg.Labels)
					if cerr != nil {
						return fmt.Errorf("publish: recreate issue after stale reopen: %w", cerr)
					}
					if uerr := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, n, store.IssueStateOpen); uerr != nil {
						return fmt.Errorf("publish: record recreated issue: %w", uerr)
					}
					_, _ = fmt.Fprintf(w, "recreated issue #%d for %s (replaced stale row)\n", n, act.finding.Fingerprint[:12])
					stale++
					created++
					continue
				}
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, act.issueNumber, store.IssueStateOpen); err != nil {
				return fmt.Errorf("publish: record reopened issue: %w", err)
			}
			// A comment failure here is benign and deliberately left
			// unrecovered: the row is already "open", so the next cycle
			// plans a plain body update rather than a second reopen attempt.
			// The regression note is lost but the issue state is correct --
			// the same trade-off runPublish already accepts at the
			// closing-state boundary above (publish.go:264-267 equivalent).
			//
			// The mirror failure mode is a duplicate comment, not a lost one:
			// if the process crashes between the PATCH above succeeding and
			// the UpsertPublishedIssue call succeeding, the row is still
			// recorded "closed" locally even though GitHub now shows it
			// open. The next run's planPublish sees that stale-closed row
			// again and replays the whole reopen (PATCH + comment) --
			// idempotent for the PATCH, but this comment is posted a second
			// time. Both directions (lost once, duplicated once) are
			// accepted: there is no cross-system transaction here, same as
			// every other two-write gh+store sequence in this function.
			if err := ghCommentIssue(ctx, gh, act.issueNumber, "Reopened by bugbot: this finding was re-detected as a regression."); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "reopened issue #%d for %s (regression)\n", act.issueNumber, act.finding.Fingerprint[:12])
			reopened++

		case publishClose:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: close issue #%d for %s (status: %s)\n", act.issueNumber, act.finding.Fingerprint[:12], act.finding.Status)
				closed++
				continue
			}
			// The close also spans two gh writes (comment, then state PATCH).
			// Record "closing" once the comment lands so a PATCH failure does
			// NOT re-post the comment on every subsequent cycle — the resume
			// path (skipComment) goes straight to the PATCH.
			if !act.skipComment {
				if err := ghCommentIssue(ctx, gh, act.issueNumber, autoCloseComment(act.finding)); err != nil {
					if isGHGoneOrNotFound(err) {
						// Issue is already gone — close is a no-op success.
						// Drop the stale row and move on; do not abort the run.
						_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue #%d gone on close: %v); dropping row\n", act.finding.Fingerprint[:12], act.issueNumber, err)
						if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
							return fmt.Errorf("publish: delete stale published issue: %w", derr)
						}
						stale++
						continue
					}
					return err
				}
				if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, act.issueNumber, store.IssueStateClosing); err != nil {
					return fmt.Errorf("publish: record closing issue: %w", err)
				}
			}
			if err := ghPatchIssueClosed(ctx, gh, act.issueNumber); err != nil {
				if isGHGoneOrNotFound(err) {
					// Same 410/404 handling on the PATCH: drop the row, log
					// it, and continue. The "closing" tombstone row from
					// above (if any) is also dropped, so the next cycle
					// starts clean.
					_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue #%d gone on PATCH: %v); dropping row\n", act.finding.Fingerprint[:12], act.issueNumber, err)
					if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
						return fmt.Errorf("publish: delete stale published issue: %w", derr)
					}
					stale++
					continue
				}
				return err
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, act.issueNumber, store.IssueStateClosed); err != nil {
				return fmt.Errorf("publish: record closed issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "closed issue #%d for %s (status: %s)\n", act.issueNumber, act.finding.Fingerprint[:12], act.finding.Status)
			closed++

		case publishSkip:
			skipped++
		default:
			return fmt.Errorf("publish: unhandled action type %T", act)
		}
	}

	_, _ = fmt.Fprintf(w, "publish: created=%d updated=%d adopted=%d reopened=%d closed=%d backsynced=%d skipped=%d stale=%d\n", created, updated, adopted, reopened, closed, backsynced, skipped, stale)
	return nil
}

// publishAction is the sum type for one unit of planned publish work. The
// concrete types are publishCreate, publishRecover, publishUpdate,
// publishReopen, publishClose, publishSkip, and publishAdopt; each carries
// only the fields valid for its op so invalid combinations are
// unrepresentable.
type publishAction interface{ publishAction() }

// publishCreate plans a new GitHub issue for a finding with no published row.
type publishCreate struct{ finding domain.Finding }

// publishRecover plans a marker search + adopt-or-create for a finding whose
// published row is stuck in "pending" (interrupted create).
type publishRecover struct{ finding domain.Finding }

// publishUpdate plans a body re-push for a finding updated after its last
// publish. issueNumber is the existing GitHub issue to PATCH.
type publishUpdate struct {
	finding     domain.Finding
	issueNumber int
}

// publishReopen plans reopening a GitHub issue whose published row is
// closed but the finding is open again -- a regression re-detected after
// store.ReopenAsRegression flipped it back to StatusOpen. issueNumber is
// the existing GitHub issue to reopen and refresh.
type publishReopen struct {
	finding     domain.Finding
	issueNumber int
}

// publishClose plans an issue close (comment then state PATCH). skipComment
// resumes an interrupted close: the auto-close comment already landed, only
// the state PATCH remains.
type publishClose struct {
	finding     domain.Finding
	issueNumber int
	skipComment bool
}

// publishSkip records that a finding already has an up-to-date published row.
// issueNumber is carried for logging.
type publishSkip struct {
	finding     domain.Finding
	issueNumber int
}

// publishAdopt plans adopting an existing open issue for a re-discovered finding
// whose fingerprint drifted: instead of creating a duplicate, it records a
// published_issues row mapping the new fingerprint to the existing issue number.
type publishAdopt struct {
	finding     domain.Finding
	issueNumber int
}

func (publishCreate) publishAction()  {}
func (publishRecover) publishAction() {}
func (publishUpdate) publishAction()  {}
func (publishReopen) publishAction()  {}
func (publishClose) publishAction()   {}
func (publishSkip) publishAction()    {}
func (publishAdopt) publishAction()   {}

// pubAnchor is an open finding that already owns a published issue; adoptAnchor
// matches a fingerprint-drifted re-discovery to one.
type pubAnchor struct {
	finding domain.Finding
	issue   int
}

// adoptAnchor returns the first already-published open finding that is the same
// defect as f under the cross-scan similarity rule (same file, nearby line,
// similar description), so the re-discovery adopts that issue instead of filing a
// duplicate. Identical fingerprints are skipped: those are handled by the
// published lookup, not by similarity.
func adoptAnchor(f domain.Finding, anchors []pubAnchor) (pubAnchor, bool) {
	for _, a := range anchors {
		if a.finding.Fingerprint == f.Fingerprint {
			continue
		}
		if funnel.SimilarFinding(a.finding.File, a.finding.Line, a.finding.Description,
			f.File, f.Line, f.Description) {
			return a, true
		}
	}
	return pubAnchor{}, false
}

// planPublish is the pure reconciler: given open/fixed/dismissed/superseded
// findings and the current published_issues map, it decides what to do with
// each finding.
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
// Close rule: if close_on_fixed is true, any finding with status fixed,
// dismissed, or superseded (backlog reconcile, bugbot-ezmx.4 — a merged-away
// duplicate) whose published row state is "open" gets a close action.
//
// Reopen rule: an OPEN finding whose published row is already "closed"
// (IssueStateClosed) is a regression -- store.ReopenAsRegression flipped a
// fixed/dismissed finding back to open while its GitHub issue stayed
// closed. This case can only reach planPublish already disambiguated from
// a human-closed issue: the caller (runPublish) runs the GitHub->local
// backsync step first, which reclassifies every human-closed row as
// closed *and* dismisses its finding (bugbot-fchv). So by the time
// planPublish sees an open finding pointing at a closed row, the close
// must have been ours, and it plans a reopen (state PATCH + body refresh)
// rather than a plain body update.
func planPublish(
	open, fixed, dismissed, superseded []domain.Finding,
	published map[string]store.PublishedIssue,
	tierMin int,
	closeOnFixed bool,
) []publishAction {
	var actions []publishAction

	// anchors are open findings that already own an open/closing published issue.
	// A re-discovered finding whose fingerprint drifted (symbol rename, or the
	// one-time v1->v2 scheme change) adopts the matching anchor's issue instead of
	// filing a duplicate. Built from the same `open` set, so it is order-stable.
	var anchors []pubAnchor
	for _, f := range open {
		if pi, ok := published[f.Fingerprint]; ok &&
			(pi.State == store.IssueStateOpen || pi.State == store.IssueStateClosing) {
			anchors = append(anchors, pubAnchor{finding: f, issue: pi.IssueNumber})
		}
	}

	// Create/recover/update/skip for open findings within tier.
	for _, f := range open {
		if f.Tier > domain.Tier(tierMin) {
			continue // outside publication window
		}
		pi, found := published[f.Fingerprint]
		switch {
		case !found:
			if a, ok := adoptAnchor(f, anchors); ok {
				actions = append(actions, publishAdopt{finding: f, issueNumber: a.issue})
			} else {
				actions = append(actions, publishCreate{finding: f})
			}
		case pi.State == store.IssueStatePending:
			// An earlier create was interrupted between the gh call and the
			// store write; the issue may or may not exist on GitHub.
			actions = append(actions, publishRecover{finding: f})
		case pi.State == store.IssueStateClosed:
			// See the Reopen rule above: an open finding with a closed row
			// is a bugbot-closed regression, not a human close (backsync
			// already dismissed and skipped those).
			actions = append(actions, publishReopen{finding: f, issueNumber: pi.IssueNumber})
		case f.UpdatedAt.After(pi.UpdatedAt):
			// Published row exists ("open", or "closing" from a reintroduced
			// finding — the body re-push is correct either way). If the finding
			// was updated after our last publish, re-push the body.
			actions = append(actions, publishUpdate{finding: f, issueNumber: pi.IssueNumber})
		default:
			actions = append(actions, publishSkip{finding: f, issueNumber: pi.IssueNumber})
		}
	}

	if !closeOnFixed {
		return actions
	}

	// Close actions for fixed/dismissed/superseded findings with published
	// rows that haven't completed a close. "closing" rows resume without
	// re-posting the auto-close comment (it already landed). "pending" rows
	// are skipped: there is no known issue number to close, and the finding
	// is gone — the rare interrupted-create-then-closed overlap is left for a
	// future open cycle. autoCloseComment renders a duplicate-specific note
	// for superseded findings, referencing the canonical fingerprint.
	closeCandidates := make([]domain.Finding, 0, len(fixed)+len(dismissed)+len(superseded))
	closeCandidates = append(closeCandidates, fixed...)
	closeCandidates = append(closeCandidates, dismissed...)
	closeCandidates = append(closeCandidates, superseded...)
	for _, f := range closeCandidates {
		pi, found := published[f.Fingerprint]
		if !found || pi.State == store.IssueStateClosed || pi.State == store.IssueStatePending {
			continue
		}
		actions = append(actions, publishClose{
			finding:     f,
			issueNumber: pi.IssueNumber,
			skipComment: pi.State == store.IssueStateClosing,
		})
	}

	return actions
}

// backsyncAction is one unit of GitHub->local reconciliation work: a
// published row whose GitHub issue closed without our involvement.
// dismissFinding is true only when the local finding still exists and is
// StatusOpen -- a human closing our issue is a triage signal to stop
// reporting the finding (StatusDismissed records that as suppression
// memory). It is false when the local finding is already fixed/dismissed/
// superseded or gone: there the row was simply lagging the true state, and
// only the row needs to catch up.
type backsyncAction struct {
	fingerprint    string
	issueNumber    int
	dismissFinding bool
}

// planBacksync is the pure reconciler for the GitHub->local direction
// (bugbot-fchv): given the published_issues rows, the set of issue numbers
// that are closed on GitHub right now, and the local findings keyed by
// fingerprint, it decides which rows need to be pulled into sync.
//
// Only rows still recorded "open" or "closing" are candidates -- rows
// already "closed" locally agree with GitHub already, and "pending" rows
// have no confirmed issue number to check. Results are sorted by
// fingerprint for deterministic output (map iteration order is not).
func planBacksync(published map[string]store.PublishedIssue, closedNums map[int]bool, findingByFP map[string]domain.Finding) []backsyncAction {
	var actions []backsyncAction
	for fp, pi := range published {
		if pi.State != store.IssueStateOpen && pi.State != store.IssueStateClosing {
			continue
		}
		if !closedNums[pi.IssueNumber] {
			continue
		}
		dismiss := false
		if f, ok := findingByFP[fp]; ok && f.Status == domain.StatusOpen {
			dismiss = true
		}
		actions = append(actions, backsyncAction{fingerprint: fp, issueNumber: pi.IssueNumber, dismissFinding: dismiss})
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].fingerprint < actions[j].fingerprint })
	return actions
}

// listBugbotIssues lists the repo's bugbot issues in the given GitHub
// `state` filter value ("open", "closed", or "all"), filtered by the first
// configured label when present. Shared by findIssueByMarker (state=all,
// recover path) and the backsync step in runPublish (state=closed).
func listBugbotIssues(ctx context.Context, gh engine.GHRunner, labels []string, state string) ([]publishIssue, error) {
	path := "repos/{owner}/{repo}/issues?state=" + state + "&per_page=100"
	if len(labels) > 0 {
		path += "&labels=" + labels[0]
	}
	raw, err := gh(ctx, "api", "--paginate", path)
	if err != nil {
		if isGHMissing(err) {
			return nil, errGHRequired()
		}
		return nil, fmt.Errorf("list issues: %w", err)
	}
	issues, err := parsePublishIssues(raw)
	if err != nil {
		return nil, fmt.Errorf("parse issues list: %w", err)
	}
	return issues, nil
}

// findIssueByMarker lists the repo's bugbot issues (filtered by the first
// configured label when present) and returns the number of the issue whose
// body carries the fingerprint marker. Used only on the rare recover path.
func findIssueByMarker(ctx context.Context, gh engine.GHRunner, labels []string, fingerprint string) (int, bool, error) {
	issues, err := listBugbotIssues(ctx, gh, labels, "all")
	if err != nil {
		return 0, false, err
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
	State  string `json:"state"`
}

// truncateUTF8 returns s sliced to at most max bytes, walking back to a valid
// UTF-8 rune boundary so the result is always valid UTF-8. When s is shorter
// than max, it is returned unchanged. The walk-back is bounded by 3 bytes
// (the longest UTF-8 continuation sequence length), so the result is <= max
// bytes in every case and never more than 3 bytes shorter than the cap.
//
// Used by every byte-cap truncation site in renderIssueBody and the repro
// section so model-authored content never produces a string that fails
// utf8.ValidString. The body-budget accounting is unchanged: callers that
// sum len() of the returned string still observe <= max.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// longestBacktickRun returns the length of the longest run of consecutive
// backtick characters found in s. Used by fencedBlock to compute a safe fence.
func longestBacktickRun(s string) int {
	max, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > max {
				max = cur
			}
		} else {
			cur = 0
		}
	}
	return max
}

// fencedBlock wraps content in a CommonMark fenced code block whose fence is
// always at least one backtick longer than the longest run inside content.
// This prevents any ``` sequence inside content (e.g. a raw git diff or a
// model-authored source file) from breaking out of the fence.
//
// The minimum fence length is 3 backticks (CommonMark minimum). When content
// contains a run of N backticks the fence uses N+1 backticks.
func fencedBlock(lang, content string) string {
	fenceLen := longestBacktickRun(content) + 1
	if fenceLen < 3 {
		fenceLen = 3
	}
	fence := strings.Repeat("`", fenceLen)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return fence + lang + "\n" + content + fence + "\n"
}

// sanitizeDetailsTag replaces the opening bytes of any <details or </details
// HTML tag (case-insensitive) with their HTML entity equivalents so that
// model-authored text placed inside a GitHub <details> block cannot close the
// block early or inject arbitrary HTML. Content already inside a code fence
// produced by fencedBlock is inert and does not need this treatment.
//
// Strategy: replace `<details` (including `</details`) with `&lt;details` so
// the tag is rendered as literal text by GitHub's Markdown renderer.
func sanitizeDetailsTag(s string) string {
	// We need a case-insensitive replace of `</details` and `<details`.
	// Process rune-by-rune to avoid regexp import overhead. Since the common
	// case is no match at all, use a simple strings.ContainsFold-equivalent
	// check first.
	lower := strings.ToLower(s)
	if !strings.Contains(lower, "<details") && !strings.Contains(lower, "</details") {
		return s
	}
	// Walk and replace. We look for '<' followed by optional '/' then "details"
	// (case-insensitive). Replace '<' with '&lt;'.
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '<' {
			rest := lower[i:]
			if strings.HasPrefix(rest, "</details") || strings.HasPrefix(rest, "<details") {
				out.WriteString("&lt;")
				i++ // skip the '<', emit the rest as-is
				continue
			}
		}
		// Emit one rune.
		_, size := utf8.DecodeRuneInString(s[i:])
		out.WriteString(s[i : i+size])
		i += size
	}
	return out.String()
}

// sanitizeControlChars strips C0 control characters (U+0000–U+001F) except tab,
// newline, and carriage return. A NUL in particular makes exec reject the gh
// argument with EINVAL ("invalid argument"); the rest have no place in a
// Markdown issue body and render as replacement glyphs on GitHub. The sources
// are model-authored fields (description, verification trace, title) and repro
// source files. The no-control-char common case returns s unchanged with no
// allocation.
//
// The scan is byte-wise: every C0 control byte is < 0x80, so it can never be
// part of a multi-byte UTF-8 sequence, and stripping it can never split a rune.
// All other bytes (including multi-byte runes and any invalid UTF-8) are copied
// through verbatim — so this never normalizes or rewrites valid content.
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
		if c := s[i]; isC0Control(c) {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isC0Control reports whether c is a C0 control byte that should be removed from
// issue text. Tab, newline, and carriage return are preserved because they are
// meaningful Markdown whitespace.
func isC0Control(c byte) bool {
	return c < 0x20 && c != '\t' && c != '\n' && c != '\r'
}

// severityLabel and titleOrUnknown are pure formatting helpers duplicated
// from internal/engine's own copies (used by planSync/renderSummaryBody):
// both packages format the same domain.Finding fields, but the gh
// comment-sync orchestration that owns the review-side copies now lives in
// internal/engine (bugbot-2p8z.5), and pulling in an engine dependency here
// just for two one-line string helpers would be the wrong direction — cli
// already depends on engine, not the other way around, and neither format
// is public API, so a two-line duplication is the correct trade-off.
func severityLabel(s domain.Severity) string {
	if string(s) == "" {
		return "unspecified"
	}
	return string(s)
}

func titleOrUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(untitled finding)"
	}
	return s
}

// renderIssueBody renders the deterministic issue body for a finding.
//
// Section order:
//  1. Hidden fingerprint marker — MUST remain the first line (findIssueByMarker recovery).
//  2. ## title
//  3. Human meta: Severity + Location (file:line) with optional source permalink.
//  4. Description (capped at 10 KB).
//  5. Candidate fix diff (when FixPatch != "", capped at 20 KB).
//  6. Inline reproduction <details> block (when ReproPath is set and readable).
//  7. Bugbot metadata <details> block: Lens, Tier, Fingerprint, models, commit, scan time.
//  8. Verification trace <details> block with 30 KB cap.
//  9. Attribution footer.
//
// Body size budget (GitHub hard limit: 65 536 chars):
//   - Description cap:  10 KB  (~10 240 chars)
//   - FixPatch cap:     20 KB  (~20 480 chars)
//   - Reasoning cap:    30 KB  (~30 720 chars)
//   - Repro cap:        25 KB  (~25 600 chars)
//   - Belt-and-braces:  if assembled body still exceeds ~60 000 chars it is
//     truncated at a safe point preserving line 1 (fingerprint marker) and the
//     attribution footer so recovery and attribution always survive.
//
// Security invariants:
//   - All fenced blocks use fencedBlock(), which auto-sizes the fence to be
//     longer than any backtick run inside the content (CommonMark rule), so
//     model-authored content cannot break out of the fence.
//   - Non-fenced model content placed inside <details> blocks (Reasoning) is
//     passed through sanitizeDetailsTag() so a literal </details> sequence
//     cannot close the block early.
func renderIssueBody(f domain.Finding, repoURL string, prov publishProvenance) string {
	const (
		maxDescription = 10 * 1024 // 10 KB cap on model-authored description
		maxFixPatch    = 20 * 1024 // 20 KB cap on model-authored patch
		maxReasoning   = 30 * 1024 // 30 KB cap on verification trace
		maxBody        = 60_000    // belt-and-braces: stay under GitHub's 65 536 limit
	)

	var b strings.Builder

	// 1. Hidden fingerprint marker — load-bearing for recovery; must stay first.
	firstLine := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	fmt.Fprintf(&b, "%s\n\n", firstLine)

	// 2. Title heading.
	fmt.Fprintf(&b, "## %s\n\n", titleOrUnknown(f.Title))

	// 3. Human-facing meta: only Severity and Location.
	fmt.Fprintf(&b, "**Severity:** %s  \n", severityLabel(f.Severity))
	if repoURL != "" && f.CommitSHA != "" && f.File != "" {
		fmt.Fprintf(&b, "**Location:** [`%s:%d`](%s/blob/%s/%s#L%d)  \n\n",
			f.File, f.Line, repoURL, f.CommitSHA, f.File, f.Line)
	} else {
		fmt.Fprintf(&b, "**Location:** `%s:%d`  \n\n", f.File, f.Line)
	}

	// 4. Description — capped to prevent oversized model output from consuming
	// the whole body budget. A truncated description is still readable.
	if f.Description != "" {
		desc := f.Description
		if len(desc) > maxDescription {
			desc = truncateUTF8(desc, maxDescription) + "\n\n[... truncated by bugbot ...]"
		}
		b.WriteString(desc)
		b.WriteString("\n\n")
	}

	// 5. Candidate fix diff — fencedBlock auto-sizes the fence so a ``` run
	// inside the diff cannot break out. Also cap large patches: a truncated
	// diff is still a useful witness.
	if f.FixPatch != "" {
		patch := f.FixPatch
		if len(patch) > maxFixPatch {
			patch = truncateUTF8(patch, maxFixPatch) + "\n[... truncated by bugbot ...]"
		}
		b.WriteString("**Candidate fix (witness — starting point only, NOT reviewed):**\n\n")
		b.WriteString(fencedBlock("diff", patch))
		b.WriteString("\n")
	}

	// 6. Inline reproduction block.
	b.WriteString(renderReproSection(f.ReproPath))

	// 7. Bugbot metadata <details> block.
	// Metadata field values (Lens, fingerprint, model names, provider type,
	// CommitSHA, scan time) are all bugbot-generated — not model-authored text —
	// so no sanitization is needed here.
	b.WriteString("<details><summary>Bugbot metadata</summary>\n\n")
	b.WriteString("| Field | Value |\n")
	b.WriteString("|---|---|\n")
	fmt.Fprintf(&b, "| Lens | %s |\n", f.Lens)
	if len(f.CorroboratingLenses) > 0 {
		fmt.Fprintf(&b, "| Corroborating lenses | %s |\n", strings.Join(f.CorroboratingLenses, ", "))
	}
	fmt.Fprintf(&b, "| Tier | %s |\n", f.Tier.Label())
	fmt.Fprintf(&b, "| Fingerprint | `%s` |\n", f.Fingerprint)
	if prov.FinderModel != "" || prov.VerifierModel != "" {
		fmt.Fprintf(&b, "| Model(s) | finder: %s · verifier: %s |\n", prov.FinderModel, prov.VerifierModel)
	}
	if prov.ProviderType != "" {
		fmt.Fprintf(&b, "| Provider | %s |\n", prov.ProviderType)
	}
	if f.CommitSHA != "" {
		fmt.Fprintf(&b, "| Commit scanned | `%s` |\n", f.CommitSHA)
	}
	if !f.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "| Scan time | %s |\n", f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	b.WriteString("\n</details>\n\n")

	// 8. Verification trace with 30 KB cap.
	// Reasoning is model-authored text placed directly inside a <details> block
	// (not inside a code fence), so we must sanitize details tags to prevent
	// a literal </details> from closing the block early.
	if f.Reasoning != "" {
		reasoning := f.Reasoning
		if len(reasoning) > maxReasoning {
			reasoning = truncateUTF8(reasoning, maxReasoning) + "\n\n[... truncated by bugbot: full trace exceeds GitHub's body limit ...]"
		}
		reasoning = sanitizeDetailsTag(reasoning)
		b.WriteString("<details><summary>Verification trace</summary>\n\n")
		b.WriteString(reasoning)
		b.WriteString("\n\n</details>\n\n")
	}

	// 9. Attribution footer — always last.
	footer := "🤖 Filed by Bugbot — automated finding; verify before acting."
	b.WriteString(footer)

	// Belt-and-braces: if the assembled body still exceeds the safe threshold,
	// truncate at a safe byte boundary while preserving line 1 (fingerprint
	// marker — load-bearing for issue recovery) and the attribution footer.
	// sanitizeControlChars runs on the whole body before the size check. It is
	// safe to slice the sanitized body at len(firstLine+"\n\n") below: firstLine
	// is the fingerprint marker (literal text + sha256 hex, no control bytes)
	// and "\n\n" is preserved whitespace, so the marker prefix is byte-identical
	// after sanitizing and len(afterFirst) still names the exact post-marker
	// offset.
	body := sanitizeControlChars(b.String())
	if len(body) > maxBody {
		truncNote := "\n\n[... body truncated by bugbot: content exceeds GitHub's issue size limit ...]\n\n"
		available := maxBody - len(firstLine) - len("\n\n") - len(truncNote) - len("\n") - len(footer)
		if available < 0 {
			available = 0
		}
		// truncateUTF8 walks back to a valid UTF-8 rune boundary, so the
		// truncated slice never breaks a multi-byte rune.
		afterFirst := firstLine + "\n\n"
		mid := body[len(afterFirst):]
		if len(mid) > available {
			mid = truncateUTF8(mid, available)
		}
		body = afterFirst + mid + truncNote + footer
	}

	return body
}

// reproSourceExtensions is the allowlist of file extensions that are inlined
// as source/test files in the reproduction <details> block. Only recognisable
// source and script files are included; binary, compiled, and diff artifacts
// are excluded. patch.diff is also explicitly excluded below regardless of
// extension because the patch prover writes it and it is not a repro source.
var reproSourceExtensions = map[string]bool{
	"go":   true,
	"py":   true,
	"js":   true,
	"ts":   true,
	"rs":   true,
	"c":    true,
	"cc":   true,
	"cpp":  true,
	"h":    true,
	"java": true,
	"rb":   true,
	"sh":   true,
	"bash": true,
	"zsh":  true,
	"fish": true,
	"toml": true,
	"yaml": true,
	"yml":  true,
	"json": true,
	"txt":  true,
	"text": true,
	"mod":  true, // go.mod
	"sum":  true, // go.sum
}

// renderReproSection inlines the reproduction artifact as a <details> block.
// It reads run.sh (stripping shebang/comment lines) and each source file found
// under reproDir, applying a 10 KB per-file cap and a 25 KB total cap.
//
// File selection: only files whose extension appears in reproSourceExtensions
// are included. patch.diff (written by the patch prover), README.md, and
// run.sh are always skipped. This prevents accidental inclusion of binary
// build artifacts or compiled objects.
//
// Code-fence safety: all fenced blocks are produced by fencedBlock(), which
// auto-sizes the fence to be longer than any backtick run inside the content
// (CommonMark rule), so model-authored source cannot break out of the fence.
//
// When the artifact dir is missing or unreadable the function falls back to a
// single-line path mention so publish never errors on a missing artifact.
// Returns an empty string when reproDir is "".
func renderReproSection(reproDir string) string {
	if reproDir == "" {
		return ""
	}

	var b strings.Builder

	// Per-file and total byte caps for the repro section.
	const maxPerFile = 10 * 1024    // 10 KB
	const maxReproTotal = 25 * 1024 // 25 KB

	// Try to read run.sh.
	runShPath := filepath.Join(reproDir, "run.sh")
	runShBytes, err := os.ReadFile(runShPath)
	if err != nil {
		// Artifact dir missing or unreadable — fall back to path mention.
		fmt.Fprintf(&b, "_Reproduction script: `%s`_\n\n", reproDir)
		return b.String()
	}

	// Parse run.sh: skip shebang and comment lines, collect meaningful command lines.
	runCmd := extractRunCommands(string(runShBytes))

	b.WriteString("<details><summary>Reproduction</summary>\n\n")

	if runCmd != "" {
		// fencedBlock auto-sizes fence to prevent ``` breakout from run commands.
		b.WriteString(fencedBlock("sh", runCmd))
		b.WriteString("\n")
	}

	// Walk for source files. Skipped files:
	//   - README.md   — documentation, not a repro source
	//   - run.sh      — already rendered above
	//   - patch.diff  — written by the patch prover; not a repro source
	//   - any file whose extension is not in reproSourceExtensions
	total := 0
	truncated := false
	_ = filepath.WalkDir(reproDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == "README.md" || name == "run.sh" || name == "patch.diff" {
			return nil
		}

		// Extension allowlist: skip non-source/test files (binaries, objects, etc.).
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		if ext == "" || !reproSourceExtensions[ext] {
			return nil
		}

		if truncated {
			return nil
		}

		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}

		// Per-file cap (truncateUTF8 keeps the result valid UTF-8 so a
		// file split mid-rune never lands in the issue body as U+FFFD).
		fileTruncated := false
		if len(content) > maxPerFile {
			content = []byte(truncateUTF8(string(content), maxPerFile))
			fileTruncated = true
		}

		// Total cap.
		remaining := maxReproTotal - total
		if remaining <= 0 {
			truncated = true
			return nil
		}
		if len(content) > remaining {
			content = []byte(truncateUTF8(string(content), remaining))
			fileTruncated = true
			truncated = true
		}
		total += len(content)

		fileContent := string(content)
		if fileTruncated {
			fileContent += "\n// ... truncated by bugbot"
		}

		rel, _ := filepath.Rel(reproDir, path)
		fmt.Fprintf(&b, "**`%s`**\n\n", rel)
		// fencedBlock auto-sizes fence to prevent ``` breakout from source content.
		b.WriteString(fencedBlock(ext, fileContent))
		b.WriteString("\n")
		return nil
	})

	if truncated {
		b.WriteString("_… truncated by bugbot: reproduction section exceeds inline size limit._\n\n")
	}

	b.WriteString("</details>\n\n")
	return b.String()
}

// extractRunCommands parses the content of run.sh and returns the meaningful
// command lines (non-shebang, non-comment, non-empty, non-set-option lines).
func extractRunCommands(content string) string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip shebang.
		if strings.HasPrefix(trimmed, "#!") {
			continue
		}
		// Skip shell comments.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Skip set -e / set -euo pipefail style lines.
		if strings.HasPrefix(trimmed, "set ") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// resolveRepoURL fetches the GitHub repo URL via `gh repo view --json url -q
// .url`. Errors are tolerated: the function returns "" to signal that
// permalinks should be omitted rather than aborting the publish run.
func resolveRepoURL(ctx context.Context, gh engine.GHRunner) string {
	raw, err := gh(ctx, "repo", "view", "--json", "url", "-q", ".url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ghCreateIssue posts a new GitHub issue and returns the issue number.
func ghCreateIssue(ctx context.Context, gh engine.GHRunner, title, body string, labels []string) (int, error) {
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
func ghUpdateIssue(ctx context.Context, gh engine.GHRunner, number int, body string) error {
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

// autoCloseComment renders the timeline comment posted before closing. A
// finding closed as StatusSuperseded (backlog reconcile, bugbot-ezmx.4)
// gets a duplicate-specific note referencing the canonical fingerprint --
// prose provenance for the operator only; the close DECISION was already
// made by planPublish keying on Status, never on this comment text (repo
// invariant: machine decisions never key on prose).
func autoCloseComment(f domain.Finding) string {
	if f.Status == domain.StatusSuperseded && f.SupersededBy != "" {
		return fmt.Sprintf(
			"Auto-closed by bugbot: this finding was merged as a duplicate of another finding (canonical fingerprint: %s).",
			f.SupersededBy,
		)
	}
	return fmt.Sprintf(
		"Auto-closed by bugbot: this finding is no longer reported (status: %s).",
		f.Status,
	)
}

// ghCommentIssue posts a comment on the issue. The caller records the
// "closing" state between this and the state PATCH so an interrupted close
// never re-posts the comment.
func ghCommentIssue(ctx context.Context, gh engine.GHRunner, number int, comment string) error {
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
func ghPatchIssueClosed(ctx context.Context, gh engine.GHRunner, number int) error {
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

// ghReopenIssue reopens a closed GitHub issue and refreshes its body in a
// single PATCH (state=open, body=...). Unlike ghUpdateIssue (body-only) and
// ghPatchIssueClosed (state-only), a regression reopen needs both the state
// flip and a fresh body -- one mutating call does double duty so a reopened
// issue never shows the stale content it had when it was closed.
func ghReopenIssue(ctx context.Context, gh engine.GHRunner, number int, body string) error {
	_, err := gh(ctx,
		"api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number),
		"-X", "PATCH",
		"-f", "state=open",
		"-f", "body="+body,
	)
	if err != nil {
		if isGHMissing(err) {
			return errGHRequired()
		}
		return fmt.Errorf("publish: reopen issue #%d: %w", number, err)
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

// isGHGoneOrNotFound reports whether the error indicates the GitHub issue no
// longer exists at the recorded number. We detect the two HTTP statuses that
// mean "this issue is gone, the local row is stale":
//   - 410 Gone: issue was deleted
//   - 404 Not Found: issue was deleted, transferred, or renamed
//
// gh surfaces these in the stderr text it returns; the wording varies across
// gh versions ("was deleted", "Gone", "Not Found", the raw status code). We
// look for the most robust signals first (the literal "410" and "404" status
// codes) and fall back to the human-readable phrases gh typically emits.
func isGHGoneOrNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Match gh's HTTP status token, not a bare "404"/"410" substring: the
	// wrapped error always embeds the issue number and API path, so a bare
	// match misfires on issues numbered like #404/#410 for any unrelated
	// failure (rate-limit, 5xx, validation).
	if strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "HTTP 410") {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "was deleted")
}
