package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/dpoage/bugbot/internal/config"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/tracker"

	// Registers the GitHub adapter with the tracker registry so the cobra
	// and daemon wiring can build whatever name config selects.
	_ "github.com/dpoage/bugbot/internal/tracker/github"
)

// publishTracker is a package-level seam that tests can replace with a fake
// (or a fake-backed adapter). Production code leaves it nil and the command
// builds the configured tracker from the registry at run time.
var publishTracker tracker.Tracker

// newPublishCmd implements `bugbot publish`: files open findings as issues
// on the configured tracker, idempotently, and closes linked issues when
// findings are auto-closed. The manual command always runs; the daemon hook
// is gated by cfg.Publish.Enabled.
func newPublishCmd() *cobra.Command {
	var (
		dryRun  bool
		tierMin int
	)

	cmd := &cobra.Command{
		Use:   "publish [flags]",
		Short: "File open findings as tracker issues (idempotent)",
		Long: `Publish open findings to the configured issue tracker.

The tracker is selected by publish.tracker in config. The default "github"
files GitHub issues through the GitHub CLI, which must be installed and
authenticated; an unknown tracker name fails with the list of known ones.

On each run it:
  - Backsyncs tracker -> local first: any published issue a human closed
    directly on the tracker is detected (one issues-list call, skipped
    entirely when nothing could have been closed) and pulled into the
    store -- the open finding is dismissed (so it is not re-reported) and
    its row is marked closed. This never posts a comment or pushes a body;
    the human's close stands as-is.
  - Creates a new tracker issue for every open finding with Tier <= tier_min
    that has not yet been filed.
  - Skips findings whose published issue is already up-to-date.
  - Updates the issue body if the finding was updated more recently than the
    last publish (UpdatedAt > published.updated_at).
  - Reopens the tracker issue (state flip + fresh body, then a comment) for
    a finding that regressed after bugbot itself had closed it -- backsync
    above already ensures a still-open finding pointing at a closed row can
    only be a bugbot-closed regression, never a human close.
  - Closes the tracker issue (and posts a comment) for findings that have
    been fixed or dismissed, when close_on_fixed is true.
  - Applies bugbot-managed labels on create alongside the configured base
    labels: severity:<critical|high|medium|low> when severity_labels is on,
    and bugbot:<fix-witnessed|reproduced|verified|suspected> (evidence tier)
    when tier_labels is on. Managed labels are reconciled on later runs as
    severity/tier change; human-added labels are never touched, and with
    both knobs off bugbot makes zero label calls.

Re-running files zero duplicates (idempotent via the published_issues table).`,
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

			tr := publishTracker
			if tr == nil {
				tr, err = tracker.New(cfg.Publish.Tracker, tracker.Config{Labels: cfg.Publish.Labels})
				if err != nil {
					return err
				}
			}

			return runPublish(ctx, cmd.OutOrStdout(), tr, st, cfg.Publish, effective, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print planned actions without executing them")
	cmd.Flags().IntVar(&tierMin, "tier-min", 2, "maximum tier to publish (inclusive; overrides config)")
	return cmd
}

// runPublish is the entry point for both the command and the daemon hook. It
// loads findings and published_issues, plans the reconcile, and applies it
// through the configured tracker. w receives the human-readable summary;
// pass cmd.OutOrStdout() from a cobra command or any io.Writer from the
// daemon hook.
func runPublish(ctx context.Context, w io.Writer, tr tracker.Tracker, st *store.Store, cfg config.Publish, tierMin int, dryRun bool) error {

	// Gather inputs for the pure planner.
	openFindings, err := st.ListFindings(ctx, domain.FindingFilter{Status: domain.StatusOpen})
	if err != nil {
		return fmt.Errorf("publish: list open findings: %w", err)
	}

	// Also load fixed+dismissed+superseded to drive close actions. Superseded
	// (backlog reconcile, bugbot-ezmx.4) is a merged-away duplicate, closed
	// exactly like fixed/dismissed so its tracker issue does not stay open
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

	// ---- Backsync: tracker -> local (bugbot-fchv) ----
	//
	// planPublish only ever reads local state; nothing above this point looks
	// at what the tracker itself currently thinks. Two local/remote-drift
	// bugs follow from that gap:
	//
	//  1. A human closes a bugbot-filed issue on the tracker as a triage
	//     signal. Without backsync the local finding stays "open" forever,
	//     and the next UpdatedAt bump plans a body push on the closed issue
	//     and upserts the row back to "open" -- the human's signal is
	//     silently discarded.
	//  2. store.ReopenAsRegression flips a fixed finding back to open while
	//     its published row is still "closed" (bugbot closed it, then the
	//     defect came back). Nothing reopens the tracker issue, so it stays
	//     closed even though we are actively re-tracking it.
	//
	// This step resolves (1): any published row still "open"/"closing" whose
	// tracker issue is now closed is backsynced -- the row is marked closed,
	// and if the local finding is still open we dismiss it (StatusDismissed
	// records suppression memory, store/findings.go:428-455 -- a human
	// closing our issue means "don't show me this again"). (2) is resolved
	// below by planPublish's own IssueStateClosed case, which only ever
	// fires for rows backsync did NOT touch (they were already closed before
	// this run), so a still-open finding pointing at one is unambiguously a
	// bugbot-closed regression, not a human close.
	//
	// Cost: one extra closed-issues listing call, and only when at least
	// one row is in a state that could have been closed on the tracker
	// (open/closing) -- a store with everything already closed costs zero
	// tracker calls.
	backsynced := 0
	needsBacksync := false
	for _, pi := range published {
		if pi.State == store.IssueStateOpen || pi.State == store.IssueStateClosing {
			needsBacksync = true
			break
		}
	}
	if needsBacksync {
		closedIssues, err := tr.ListIssues(ctx, "closed")
		if err != nil {
			return fmt.Errorf("publish: backsync: list closed issues: %w", err)
		}
		closedKeys := make(map[tracker.IssueKey]bool, len(closedIssues))
		for _, is := range closedIssues {
			// Some trackers share the listing with other work-item kinds
			// (GitHub issues and PRs share one number namespace); a
			// defensive check on State keeps a malformed/unexpected entry
			// from ever counting as "closed" here even though we only ever
			// match keys we ourselves recorded in published_issues.
			if is.State != "closed" {
				continue
			}
			closedKeys[is.Key] = true
		}

		findingByFP := make(map[string]domain.Finding, len(openFindings)+len(fixedFindings)+len(dismissedFindings)+len(supersededFindings))
		for _, group := range [][]domain.Finding{openFindings, fixedFindings, dismissedFindings, supersededFindings} {
			for _, f := range group {
				findingByFP[f.Fingerprint] = f
			}
		}

		backsyncActions := planBacksync(publishedMap, closedKeys, findingByFP)

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
					_, _ = fmt.Fprintf(w, "dry-run: backsync issue %s for %s (closed on tracker; would dismiss finding)\n", ba.issueKey, ba.fingerprint[:12])
				} else {
					_, _ = fmt.Fprintf(w, "dry-run: backsync issue %s for %s (closed on tracker; finding already %s)\n", ba.issueKey, ba.fingerprint[:12], status)
				}
				backsynced++
				continue
			}
			if ba.dismissFinding {
				reason := fmt.Sprintf("issue %s was closed manually on the tracker; dismissed by publish backsync", ba.issueKey)
				if err := st.UpdateStatus(ctx, ba.fingerprint, domain.StatusDismissed, reason); err != nil {
					return fmt.Errorf("publish: backsync dismiss %s: %w", ba.fingerprint[:12], err)
				}
				_, _ = fmt.Fprintf(w, "backsynced issue %s for %s (closed on tracker; finding dismissed)\n", ba.issueKey, ba.fingerprint[:12])
			} else {
				_, _ = fmt.Fprintf(w, "backsynced issue %s for %s (closed on tracker; finding already %s)\n", ba.issueKey, ba.fingerprint[:12], status)
			}
			if err := st.UpsertPublishedIssue(ctx, ba.fingerprint, tr.Name(), string(ba.issueKey), store.IssueStateClosed, ""); err != nil {
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
				IssueKey:    string(ba.issueKey),
				Tracker:     tr.Name(),
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
	repoURL := tr.RepoURL(ctx)

	created, updated, adopted, reopened, closed, skipped, stale, failed := 0, 0, 0, 0, 0, 0, 0, 0

	// printSummary writes the one-line apply-loop tally to w. Factored out of
	// the normal end-of-loop print because the rate-limit abort path below
	// must also emit it -- before returning the abort error -- so the
	// daemon's log always shows how far the plan got, not just that it
	// failed.
	printSummary := func() {
		_, _ = fmt.Fprintf(w, "publish: created=%d updated=%d adopted=%d reopened=%d closed=%d backsynced=%d skipped=%d stale=%d failed=%d\n",
			created, updated, adopted, reopened, closed, backsynced, skipped, stale, failed)
	}

	// applyErr classifies a tracker error hit while applying one planned
	// action and reports how the loop should react:
	//
	//   - non-nil return: the WHOLE run must stop. Either the tracker's
	//     client tooling is missing (tracker.ErrMissingPrereq -- nothing
	//     else in the plan can succeed either, so keeping going is
	//     pointless) or the tracker is rate limiting
	//     (tracker.ErrRateLimited -- the adapter has already exhausted
	//     whatever pacing/retry budget it owns by the time this surfaces,
	//     so retrying again here would only burn more quota). The caller
	//     must `return` the result immediately.
	//   - nil return: the failure is scoped to this one action (a
	//     validation rejection, a transient backend failure that outlived
	//     the adapter's retries, etc.). It has already been logged to w
	//     and counted in failed; the caller should `continue` to the next
	//     planned action instead of dropping the rest of the plan. This is
	//     safe because every composite action writes its pending/closing
	//     tombstone state to the store BEFORE the tracker call that can
	//     fail here, so a skipped action resumes correctly next cycle --
	//     the same tombstone design the ErrIssueGone stale-row paths below
	//     already rely on.
	//
	// Store errors are NOT routed through this helper: any st.* failure
	// still aborts the run directly at its call site (unchanged), because
	// a broken local db risks desyncing state no matter which action hit it.
	applyErr := func(action string, key tracker.IssueKey, fingerprint string, err error) error {
		if errors.Is(err, tracker.ErrMissingPrereq) {
			return fmt.Errorf("publish: %w", err)
		}
		if errors.Is(err, tracker.ErrRateLimited) {
			printSummary()
			return fmt.Errorf("publish: aborting remaining plan after tracker rate limit retries exhausted (%s for %s): %w", action, fingerprint[:12], err)
		}
		if key != "" {
			_, _ = fmt.Fprintf(w, "failed %s issue %s for %s: %v\n", action, key, fingerprint[:12], err)
		} else {
			_, _ = fmt.Fprintf(w, "failed %s for %s: %v\n", action, fingerprint[:12], err)
		}
		failed++
		return nil
	}

	// ensureManagedLabels lazily creates each bugbot-managed label in the
	// repo (color + description) before the first tracker call in this run
	// that applies it, memoized per run so each label costs at most one
	// create call per publish cycle. The adapter owns idempotency: ensuring
	// a label that already exists is success. Base cfg.Labels are never
	// ensured: those are user-managed, and creating them (or fighting over
	// their color) is not bugbot's business. Trackers without label support
	// (Capabilities().Labels false) skip label work entirely.
	ensured := make(map[string]bool, 4)
	ensureManagedLabels := func(labels []string) error {
		if !tr.Capabilities().Labels {
			return nil
		}
		for _, l := range labels {
			if ensured[l] {
				continue
			}
			def, ok := managedLabelDefs[l]
			if !ok {
				continue // not a bugbot-managed label; nothing to ensure
			}
			if err := tr.EnsureLabel(ctx, tracker.Label{Name: l, Color: def.color, Description: def.desc}); err != nil {
				return err
			}
			ensured[l] = true
		}
		return nil
	}

	// reconcileLabels converges the bugbot-managed labels on an already-
	// published issue toward managedLabels(f, cfg). Severity/tier changes
	// always change the rendered body too (the visible Severity line and the
	// bugbot:meta front-matter), so label drift normally rides an update
	// push; this reconcile exists for legacy backfill (rows created before
	// managed labels) and knob flips.
	//
	// Rules:
	//   - Both knobs off, or a tracker without label support: feature inert
	//     — zero label tracker calls, zero deletes.
	//   - desired vs. current (store.PublishedIssue.ManagedLabels) compared
	//     as sets (nil ≡ empty): converged rows cost zero tracker calls.
	//   - Additions land in ONE AddLabels call, after ensureManagedLabels;
	//     removals are per-label RemoveLabel calls. Never a full-array
	//     replace — that would clobber human-added labels. Removing an
	//     already-detached label is success (adapter contract).
	//   - Legacy rows (current == nil, the '' column sentinel) are additive
	//     only: we don't know what we applied historically, so we never
	//     delete.
	//   - Store bookkeeping is written only after tracker success, so a
	//     failed sync retries naturally on the next cycle's skip-path
	//     reconcile.
	//
	// The body work of the surrounding action has already succeeded by the
	// time this runs, so a label-sync failure must not abort the action: it
	// is routed through applyErr ("label-sync"), which logs, counts
	// failed++, and returns nil — except missing-prereq/rate-limit, which
	// abort the whole run exactly as every other action does. A non-nil
	// return from this closure therefore always means "stop the run".
	reconcileLabels := func(f domain.Finding, key tracker.IssueKey) error {
		if !cfg.SeverityLabels && !cfg.TierLabels {
			return nil
		}
		if !tr.Capabilities().Labels {
			return nil
		}
		desired := managedLabels(f, cfg)
		current := publishedMap[f.Fingerprint].ManagedLabels
		additions := labelSetDiff(desired, current)
		removals := labelSetDiff(current, desired)
		if current == nil {
			removals = nil // legacy row: additive only
		}
		if len(additions) == 0 && len(removals) == 0 {
			return nil // converged (nil ≡ empty)
		}
		if dryRun {
			_, _ = fmt.Fprintf(w, "dry-run: sync labels on issue %s for %s (+%d -%d)\n", key, f.Fingerprint[:12], len(additions), len(removals))
			return nil
		}
		if len(additions) > 0 {
			if err := ensureManagedLabels(additions); err != nil {
				return applyErr("label-sync", key, f.Fingerprint, err)
			}
			if err := tr.AddLabels(ctx, key, additions); err != nil {
				return applyErr("label-sync", key, f.Fingerprint, err)
			}
		}
		for _, l := range removals {
			if err := tr.RemoveLabel(ctx, key, l); err != nil {
				return applyErr("label-sync", key, f.Fingerprint, err)
			}
		}
		if err := st.SetPublishedManagedLabels(ctx, f.Fingerprint, desired); err != nil {
			return fmt.Errorf("publish: record managed labels for %s: %w", f.Fingerprint[:12], err)
		}
		return nil
	}

	for _, a := range plan {
		switch act := a.(type) {
		case publishCreate:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: create issue for %s (%s)\n", act.finding.Fingerprint[:12], act.finding.Title)
				created++
				continue
			}
			// A create spans two systems (the tracker + our store) with no
			// transaction. Record a "pending" row FIRST so a crash between
			// the create call and the store write leaves a tombstone: the
			// next run plans a recover (marker search) instead of blindly
			// creating a duplicate issue.
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), "", store.IssueStatePending, ""); err != nil {
				return fmt.Errorf("publish: record pending issue: %w", err)
			}
			body := renderIssueBody(act.finding, repoURL)
			managed := managedLabels(act.finding, cfg)
			if err := ensureManagedLabels(managed); err != nil {
				if aerr := applyErr("ensure-labels", "", act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			key, err := tr.CreateIssue(ctx, act.finding.Title, body, combinedLabels(cfg.Labels, managed))
			if err != nil {
				if aerr := applyErr("create", "", act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(key), store.IssueStateOpen, bodyHashHex(body)); err != nil {
				return fmt.Errorf("publish: record created issue: %w", err)
			}
			if err := st.SetPublishedManagedLabels(ctx, act.finding.Fingerprint, managed); err != nil {
				return fmt.Errorf("publish: record managed labels: %w", err)
			}
			_, _ = fmt.Fprintf(w, "created issue %s for %s (%s)\n", key, act.finding.Fingerprint[:12], act.finding.Title)
			created++

		case publishAdopt:
			// A re-discovered finding whose fingerprint drifted: adopt the existing
			// issue (record the new fingerprint -> issue mapping) rather than file a
			// duplicate. No tracker write; the next cycle's update/skip path takes
			// over. No body was pushed by this action, so body_hash stays "" -- the
			// eventual update/skip decision is made fresh next cycle.
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: adopt issue %s for %s (re-discovered; fingerprint drifted)\n", act.issueKey, act.finding.Fingerprint[:12])
				adopted++
				continue
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(act.issueKey), store.IssueStateOpen, ""); err != nil {
				return fmt.Errorf("publish: record adopted issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "adopted issue %s for %s (re-discovered; fingerprint drifted)\n", act.issueKey, act.finding.Fingerprint[:12])
			adopted++

		case publishRecover:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: recover pending publish for %s\n", act.finding.Fingerprint[:12])
				skipped++
				continue
			}
			// A prior run recorded "pending" but never recorded the issue
			// key — the create may or may not have happened. Search the
			// repo's bugbot issues for the fingerprint marker; adopt on hit,
			// create on miss.
			key, found, err := findIssueByMarker(ctx, tr, act.finding.Fingerprint)
			if err != nil {
				if aerr := applyErr("recover", "", act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			// bodyHash stays "" on the adopt-via-marker path: no body was
			// pushed by this run, so the next publishUpdate decides fresh.
			bodyHash := ""
			var managed []string // set only on the create arm: adopt-via-marker applies no labels, so the '' column stays legacy and the next cycle's reconcile backfills additively
			if !found {
				body := renderIssueBody(act.finding, repoURL)
				managed = managedLabels(act.finding, cfg)
				if err := ensureManagedLabels(managed); err != nil {
					if aerr := applyErr("ensure-labels", "", act.finding.Fingerprint, err); aerr != nil {
						return aerr
					}
					continue
				}
				key, err = tr.CreateIssue(ctx, act.finding.Title, body, combinedLabels(cfg.Labels, managed))
				if err != nil {
					if aerr := applyErr("recover-create", "", act.finding.Fingerprint, err); aerr != nil {
						return aerr
					}
					continue
				}
				bodyHash = bodyHashHex(body)
				created++
				_, _ = fmt.Fprintf(w, "created issue %s for %s (recovered pending; no existing issue found)\n", key, act.finding.Fingerprint[:12])
			} else {
				_, _ = fmt.Fprintf(w, "recovered issue %s for %s (adopted via fingerprint marker)\n", key, act.finding.Fingerprint[:12])
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(key), store.IssueStateOpen, bodyHash); err != nil {
				return fmt.Errorf("publish: record recovered issue: %w", err)
			}
			if !found {
				if err := st.SetPublishedManagedLabels(ctx, act.finding.Fingerprint, managed); err != nil {
					return fmt.Errorf("publish: record managed labels: %w", err)
				}
			}

		case publishUpdate:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: update issue %s for %s\n", act.issueKey, act.finding.Fingerprint[:12])
				updated++
				continue
			}
			// Render and hash once up front: both the no-op short-circuit
			// below and the actual push (and its stale-recreate fallback)
			// need the exact same body, and only one render/hash per action
			// keeps this cheap.
			body := renderIssueBody(act.finding, repoURL)
			h := bodyHashHex(body)
			if pi := publishedMap[act.finding.Fingerprint]; pi.BodyHash == h && h != "" {
				// The rendered body is byte-identical to what was last
				// pushed to the tracker -- this update was triggered by a
				// metadata-only finding touch (impact sweep,
				// AddCorroboratingLenses, AppendFindingSites all bump
				// findings.updated_at without changing anything
				// renderIssueBody reads), not a real content change. Skip
				// the push but still upsert so published_issues.updated_at
				// advances past findings.updated_at -- otherwise the
				// planner would replan this same no-op update every cycle
				// forever instead of converging to publishSkip.
				if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(act.issueKey), store.IssueStateOpen, h); err != nil {
					return fmt.Errorf("publish: record unchanged issue: %w", err)
				}
				_, _ = fmt.Fprintf(w, "unchanged issue %s for %s (body identical; no push)\n", act.issueKey, act.finding.Fingerprint[:12])
				skipped++
				if err := reconcileLabels(act.finding, act.issueKey); err != nil {
					return err
				}
				continue
			}
			if err := tr.UpdateIssueBody(ctx, act.issueKey, body); err != nil {
				if errors.Is(err, tracker.ErrIssueGone) {
					// Local row is stale: the issue is gone on the tracker
					// (deleted, transferred, or renamed). Drop the row,
					// create a fresh issue, and continue with the rest of
					// the plan.
					_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue %s gone: %v); re-creating\n", act.finding.Fingerprint[:12], act.issueKey, err)
					if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
						return fmt.Errorf("publish: delete stale published issue: %w", derr)
					}
					managed := managedLabels(act.finding, cfg)
					if err := ensureManagedLabels(managed); err != nil {
						if aerr := applyErr("ensure-labels", "", act.finding.Fingerprint, err); aerr != nil {
							return aerr
						}
						continue
					}
					key, cerr := tr.CreateIssue(ctx, act.finding.Title, body, combinedLabels(cfg.Labels, managed))
					if cerr != nil {
						if aerr := applyErr("update-recreate", "", act.finding.Fingerprint, cerr); aerr != nil {
							return aerr
						}
						continue
					}
					if uerr := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(key), store.IssueStateOpen, h); uerr != nil {
						return fmt.Errorf("publish: record recreated issue: %w", uerr)
					}
					if err := st.SetPublishedManagedLabels(ctx, act.finding.Fingerprint, managed); err != nil {
						return fmt.Errorf("publish: record managed labels: %w", err)
					}
					_, _ = fmt.Fprintf(w, "recreated issue %s for %s (replaced stale row)\n", key, act.finding.Fingerprint[:12])
					stale++
					created++
					continue
				}
				if aerr := applyErr("update", act.issueKey, act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(act.issueKey), store.IssueStateOpen, h); err != nil {
				return fmt.Errorf("publish: record updated issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "updated issue %s for %s\n", act.issueKey, act.finding.Fingerprint[:12])
			updated++
			if err := reconcileLabels(act.finding, act.issueKey); err != nil {
				return err
			}

		case publishReopen:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: reopen issue %s for %s (regression)\n", act.issueKey, act.finding.Fingerprint[:12])
				reopened++
				continue
			}
			// Backsync (above) already turned every human-closed row into
			// IssueStateClosed and dismissed the finding, so an OPEN finding
			// whose row is still closed at this point can only be a
			// bugbot-closed regression (store.ReopenAsRegression). One
			// ReopenIssue call both flips the state and refreshes the body,
			// so a reopened issue never shows stale content from before the
			// fix regressed.
			body := renderIssueBody(act.finding, repoURL)
			h := bodyHashHex(body)
			if err := tr.ReopenIssue(ctx, act.issueKey, body); err != nil {
				if errors.Is(err, tracker.ErrIssueGone) {
					// Same stale-row handling as publishUpdate: the issue is
					// gone, drop the row and file a fresh one.
					_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue %s gone on reopen: %v); re-creating\n", act.finding.Fingerprint[:12], act.issueKey, err)
					if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
						return fmt.Errorf("publish: delete stale published issue: %w", derr)
					}
					managed := managedLabels(act.finding, cfg)
					if err := ensureManagedLabels(managed); err != nil {
						if aerr := applyErr("ensure-labels", "", act.finding.Fingerprint, err); aerr != nil {
							return aerr
						}
						continue
					}
					key, cerr := tr.CreateIssue(ctx, act.finding.Title, body, combinedLabels(cfg.Labels, managed))
					if cerr != nil {
						if aerr := applyErr("reopen-recreate", "", act.finding.Fingerprint, cerr); aerr != nil {
							return aerr
						}
						continue
					}
					if uerr := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(key), store.IssueStateOpen, h); uerr != nil {
						return fmt.Errorf("publish: record recreated issue: %w", uerr)
					}
					if err := st.SetPublishedManagedLabels(ctx, act.finding.Fingerprint, managed); err != nil {
						return fmt.Errorf("publish: record managed labels: %w", err)
					}
					_, _ = fmt.Fprintf(w, "recreated issue %s for %s (replaced stale row)\n", key, act.finding.Fingerprint[:12])
					stale++
					created++
					continue
				}
				if aerr := applyErr("reopen", act.issueKey, act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(act.issueKey), store.IssueStateOpen, h); err != nil {
				return fmt.Errorf("publish: record reopened issue: %w", err)
			}
			// A comment failure here is benign: the row is already "open",
			// so the next cycle plans a plain body update rather than a
			// second reopen attempt. The regression note is lost but the
			// issue state is correct -- the same trade-off runPublish
			// already accepts at the closing-state boundary above.
			//
			// The mirror failure mode is a duplicate comment, not a lost one:
			// if the process crashes between the reopen above succeeding and
			// the UpsertPublishedIssue call succeeding, the row is still
			// recorded "closed" locally even though the tracker now shows it
			// open. The next run's planPublish sees that stale-closed row
			// again and replays the whole reopen (state flip + comment) --
			// idempotent for the state flip, but this comment is posted a
			// second time. Both directions (lost once, duplicated once) are
			// accepted: there is no cross-system transaction here, same as
			// every other two-write tracker+store sequence in this function.
			if err := tr.Comment(ctx, act.issueKey, "Reopened by bugbot: this finding was re-detected as a regression."); err != nil {
				if aerr := applyErr("reopen-comment", act.issueKey, act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			_, _ = fmt.Fprintf(w, "reopened issue %s for %s (regression)\n", act.issueKey, act.finding.Fingerprint[:12])
			reopened++
			// Label sync last: if the comment above failed we never get
			// here, and the next cycle's skip-path reconcile picks the
			// drift up instead.
			if err := reconcileLabels(act.finding, act.issueKey); err != nil {
				return err
			}

		case publishClose:
			if dryRun {
				_, _ = fmt.Fprintf(w, "dry-run: close issue %s for %s (status: %s)\n", act.issueKey, act.finding.Fingerprint[:12], act.finding.Status)
				closed++
				continue
			}
			// The close also spans two tracker writes (comment, then the
			// state change). Record "closing" once the comment lands so a
			// close failure does NOT re-post the comment on every subsequent
			// cycle — the resume path (skipComment) goes straight to the
			// close. Close never pushes a body, so body_hash is always "".
			if !act.skipComment {
				if err := tr.Comment(ctx, act.issueKey, autoCloseComment(act.finding)); err != nil {
					if errors.Is(err, tracker.ErrIssueGone) {
						// Issue is already gone — close is a no-op success.
						// Drop the stale row and move on; do not abort the run.
						_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue %s gone on close: %v); dropping row\n", act.finding.Fingerprint[:12], act.issueKey, err)
						if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
							return fmt.Errorf("publish: delete stale published issue: %w", derr)
						}
						stale++
						continue
					}
					if aerr := applyErr("close-comment", act.issueKey, act.finding.Fingerprint, err); aerr != nil {
						return aerr
					}
					continue
				}
				if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(act.issueKey), store.IssueStateClosing, ""); err != nil {
					return fmt.Errorf("publish: record closing issue: %w", err)
				}
			}
			if err := tr.CloseIssue(ctx, act.issueKey); err != nil {
				if errors.Is(err, tracker.ErrIssueGone) {
					// Same gone handling on the close itself: drop the row,
					// log it, and continue. The "closing" tombstone row from
					// above (if any) is also dropped, so the next cycle
					// starts clean.
					_, _ = fmt.Fprintf(w, "stale published_issues row for %s (issue %s gone on close: %v); dropping row\n", act.finding.Fingerprint[:12], act.issueKey, err)
					if derr := st.DeletePublishedIssue(ctx, act.finding.Fingerprint); derr != nil {
						return fmt.Errorf("publish: delete stale published issue: %w", derr)
					}
					stale++
					continue
				}
				if aerr := applyErr("close", act.issueKey, act.finding.Fingerprint, err); aerr != nil {
					return aerr
				}
				continue
			}
			if err := st.UpsertPublishedIssue(ctx, act.finding.Fingerprint, tr.Name(), string(act.issueKey), store.IssueStateClosed, ""); err != nil {
				return fmt.Errorf("publish: record closed issue: %w", err)
			}
			_, _ = fmt.Fprintf(w, "closed issue %s for %s (status: %s)\n", act.issueKey, act.finding.Fingerprint[:12], act.finding.Status)
			closed++

		case publishSkip:
			skipped++
			// The row is already up to date; the only work a skip can carry
			// is managed-label drift (legacy backfill, knob flip).
			if err := reconcileLabels(act.finding, act.issueKey); err != nil {
				return err
			}
		default:
			return fmt.Errorf("publish: unhandled action type %T", act)
		}
	}

	printSummary()
	if failed > 0 {
		return fmt.Errorf("publish: %d action(s) failed; see log above", failed)
	}
	return nil
}

// managedLabels returns the bugbot-managed labels desired for f under cfg's
// publish knobs: "severity:<sev>" (severity_labels) for the four known
// severities and "bugbot:<slug>" (tier_labels) for tiers 0-3. An unknown
// severity or tier contributes no label. The result is sorted so it can be
// compared as a set against store.PublishedIssue.ManagedLabels (also stored
// sorted) and so tracker call args are deterministic. cfg.Labels is never part of
// the result: base labels are user-managed and only ever ADDITIVE alongside
// these (Labels[0] stays the backsync/recovery list filter anchor).
func managedLabels(f domain.Finding, cfg config.Publish) []string {
	var labels []string
	if cfg.SeverityLabels {
		switch f.Severity {
		case domain.SeverityCritical, domain.SeverityHigh, domain.SeverityMedium, domain.SeverityLow:
			labels = append(labels, "severity:"+string(f.Severity))
		}
	}
	if cfg.TierLabels {
		switch f.Tier {
		case domain.TierFixWitnessed:
			labels = append(labels, "bugbot:fix-witnessed")
		case domain.TierReproduced:
			labels = append(labels, "bugbot:reproduced")
		case domain.TierVerified:
			labels = append(labels, "bugbot:verified")
		case domain.TierSuspected:
			labels = append(labels, "bugbot:suspected")
		}
	}
	sort.Strings(labels)
	return labels
}

// managedLabelDefs fixes the repo-level color and description used when
// ensureManagedLabels creates a managed label. Keyed by the exact label
// names managedLabels emits; anything not in this map is never ensured.
var managedLabelDefs = map[string]struct{ color, desc string }{
	"severity:critical":    {"b60205", "Bugbot: critical severity finding"},
	"severity:high":        {"d93f0b", "Bugbot: high severity finding"},
	"severity:medium":      {"fbca04", "Bugbot: medium severity finding"},
	"severity:low":         {"c2e0c6", "Bugbot: low severity finding"},
	"bugbot:fix-witnessed": {"0e8a16", "Bugbot T0: a generated fix made a failing test pass"},
	"bugbot:reproduced":    {"1d76db", "Bugbot T1: a sandboxed failing test was produced"},
	"bugbot:verified":      {"5319e7", "Bugbot T2: a refuter panel failed to disprove it"},
	"bugbot:suspected":     {"bfdadc", "Bugbot T3: reported by a finder, not yet verified"},
}

// combinedLabels returns base+managed for an issue create call without
// mutating base (cfg.Labels is shared across the whole run). base order is
// preserved: Labels[0] is the list filter anchor used by backsync/recovery.
func combinedLabels(base, managed []string) []string {
	if len(managed) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(managed))
	out = append(out, base...)
	return append(out, managed...)
}

// labelSetDiff returns the elements of a not present in b, preserving a's
// order (both inputs are sorted label sets of at most a few entries, so the
// linear scan beats building a map).
func labelSetDiff(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// publishAction is the sum type for one unit of planned publish work. The
// concrete types are publishCreate, publishRecover, publishUpdate,
// publishReopen, publishClose, publishSkip, and publishAdopt; each carries
// only the fields valid for its op so invalid combinations are
// unrepresentable.
type publishAction interface{ publishAction() }

// publishCreate plans a new tracker issue for a finding with no published row.
type publishCreate struct{ finding domain.Finding }

// publishRecover plans a marker search + adopt-or-create for a finding whose
// published row is stuck in "pending" (interrupted create).
type publishRecover struct{ finding domain.Finding }

// publishUpdate plans a body re-push for a finding updated after its last
// publish. issueKey is the existing tracker issue to update.
type publishUpdate struct {
	finding  domain.Finding
	issueKey tracker.IssueKey
}

// publishReopen plans reopening a tracker issue whose published row is
// closed but the finding is open again -- a regression re-detected after
// store.ReopenAsRegression flipped it back to StatusOpen. issueKey is
// the existing tracker issue to reopen and refresh.
type publishReopen struct {
	finding  domain.Finding
	issueKey tracker.IssueKey
}

// publishClose plans an issue close (comment then state change). skipComment
// resumes an interrupted close: the auto-close comment already landed, only
// the state change remains.
type publishClose struct {
	finding     domain.Finding
	issueKey    tracker.IssueKey
	skipComment bool
}

// publishSkip records that a finding already has an up-to-date published row.
// issueKey is carried for logging.
type publishSkip struct {
	finding  domain.Finding
	issueKey tracker.IssueKey
}

// publishAdopt plans adopting an existing open issue for a re-discovered finding
// whose fingerprint drifted: instead of creating a duplicate, it records a
// published_issues row mapping the new fingerprint to the existing issue key.
type publishAdopt struct {
	finding  domain.Finding
	issueKey tracker.IssueKey
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
	issue   tracker.IssueKey
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
// require a tracker read per issue), we use finding.UpdatedAt > published.UpdatedAt
// as a proxy. If the finding was updated after the last publish, we plan a
// publishUpdate -- but an updated_at bump is a superset of an actual body
// change: impact sweep, AddCorroboratingLenses, and AppendFindingSites all
// touch findings.updated_at without changing anything renderIssueBody reads.
// This planner stays cheap (no tracker reads) by over-selecting; the no-op
// body push this would otherwise cost is closed at apply time instead
// (bugbot-klaj): runPublish renders the body once, hashes it, and compares
// against published_issues.body_hash before calling the tracker -- a hash
// match skips the push entirely (still upserting so updated_at advances and
// the planner converges to publishSkip next cycle).
//
// Close rule: if close_on_fixed is true, any finding with status fixed,
// dismissed, or superseded (backlog reconcile, bugbot-ezmx.4 — a merged-away
// duplicate) whose published row state is "open" gets a close action.
//
// Reopen rule: an OPEN finding whose published row is already "closed"
// (IssueStateClosed) is a regression -- store.ReopenAsRegression flipped a
// fixed/dismissed finding back to open while its tracker issue stayed
// closed. This case can only reach planPublish already disambiguated from
// a human-closed issue: the caller (runPublish) runs the tracker->local
// backsync step first, which reclassifies every human-closed row as
// closed *and* dismisses its finding (bugbot-fchv). So by the time
// planPublish sees an open finding pointing at a closed row, the close
// must have been ours, and it plans a reopen (state flip + body refresh)
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
			anchors = append(anchors, pubAnchor{finding: f, issue: tracker.IssueKey(pi.IssueKey)})
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
				actions = append(actions, publishAdopt{finding: f, issueKey: a.issue})
			} else {
				actions = append(actions, publishCreate{finding: f})
			}
		case pi.State == store.IssueStatePending:
			// An earlier create was interrupted between the tracker call and
			// the store write; the issue may or may not exist on the tracker.
			actions = append(actions, publishRecover{finding: f})
		case pi.State == store.IssueStateClosed:
			// See the Reopen rule above: an open finding with a closed row
			// is a bugbot-closed regression, not a human close (backsync
			// already dismissed and skipped those).
			actions = append(actions, publishReopen{finding: f, issueKey: tracker.IssueKey(pi.IssueKey)})
		case f.UpdatedAt.After(pi.UpdatedAt):
			// Published row exists ("open", or "closing" from a reintroduced
			// finding — the body re-push is correct either way). If the finding
			// was updated after our last publish, re-push the body.
			actions = append(actions, publishUpdate{finding: f, issueKey: tracker.IssueKey(pi.IssueKey)})
		default:
			actions = append(actions, publishSkip{finding: f, issueKey: tracker.IssueKey(pi.IssueKey)})
		}
	}

	if !closeOnFixed {
		return actions
	}

	// Close actions for fixed/dismissed/superseded findings with published
	// rows that haven't completed a close. "closing" rows resume without
	// re-posting the auto-close comment (it already landed). "pending" rows
	// are skipped: there is no known issue key to close, and the finding
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
			issueKey:    tracker.IssueKey(pi.IssueKey),
			skipComment: pi.State == store.IssueStateClosing,
		})
	}

	return actions
}

// backsyncAction is one unit of tracker->local reconciliation work: a
// published row whose tracker issue closed without our involvement.
// dismissFinding is true only when the local finding still exists and is
// StatusOpen -- a human closing our issue is a triage signal to stop
// reporting the finding (StatusDismissed records that as suppression
// memory). It is false when the local finding is already fixed/dismissed/
// superseded or gone: there the row was simply lagging the true state, and
// only the row needs to catch up.
type backsyncAction struct {
	fingerprint    string
	issueKey       tracker.IssueKey
	dismissFinding bool
}

// planBacksync is the pure reconciler for the tracker->local direction
// (bugbot-fchv): given the published_issues rows, the set of issue keys
// that are closed on the tracker right now, and the local findings keyed by
// fingerprint, it decides which rows need to be pulled into sync.
//
// Only rows still recorded "open" or "closing" are candidates -- rows
// already "closed" locally agree with the tracker already, and "pending"
// rows have no confirmed issue key to check. Results are sorted by
// fingerprint for deterministic output (map iteration order is not).
func planBacksync(published map[string]store.PublishedIssue, closedKeys map[tracker.IssueKey]bool, findingByFP map[string]domain.Finding) []backsyncAction {
	var actions []backsyncAction
	for fp, pi := range published {
		if pi.State != store.IssueStateOpen && pi.State != store.IssueStateClosing {
			continue
		}
		if !closedKeys[tracker.IssueKey(pi.IssueKey)] {
			continue
		}
		dismiss := false
		if f, ok := findingByFP[fp]; ok && f.Status == domain.StatusOpen {
			dismiss = true
		}
		actions = append(actions, backsyncAction{fingerprint: fp, issueKey: tracker.IssueKey(pi.IssueKey), dismissFinding: dismiss})
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].fingerprint < actions[j].fingerprint })
	return actions
}

// findIssueByMarker lists the repo's bugbot issues through the tracker (the
// adapter applies the anchor-label filter from its own config) and returns
// the key of the issue whose body carries the fingerprint marker. Used only
// on the rare recover path. The marker format is part of the canonical body
// contract shared with renderIssueBody (see the tracker package doc), so the
// scan lives here beside the renderer that writes it, not in any adapter.
func findIssueByMarker(ctx context.Context, tr tracker.Tracker, fingerprint string) (tracker.IssueKey, bool, error) {
	issues, err := tr.ListIssues(ctx, "all")
	if err != nil {
		return "", false, err
	}
	marker := "<!-- bugbot:fp=" + fingerprint + " -->"
	for _, is := range issues {
		if strings.Contains(is.Body, marker) {
			return is.Key, true, nil
		}
	}
	return "", false, nil
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
// newline, and carriage return. A NUL in particular would make an exec-based
// tracker transport reject the argument with EINVAL ("invalid argument"); the
// rest have no place in a Markdown issue body and render as replacement
// glyphs when the tracker displays them. The sources
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
// both packages format the same domain.Finding fields, but the review
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

// bugbotMeta is the machine-readable front-matter embedded as an HTML comment
// on line 2 of every issue body (line 1 is the fingerprint marker):
//
//	<!-- bugbot:meta {"severity":"high","tier":1,"lens":"race",...} -->
//
// External tooling parses this instead of scraping the human-facing Markdown.
// Severity, tier, lens, file, and line are always present; commit is omitted
// when the finding has no recorded commit SHA.
type bugbotMeta struct {
	Severity string `json:"severity"`
	Tier     int    `json:"tier"`
	Lens     string `json:"lens"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Commit   string `json:"commit,omitempty"`
}

// renderIssueBody renders the deterministic issue body for a finding.
//
// Section order:
//  1. Hidden fingerprint marker — MUST remain the first line (findIssueByMarker recovery).
//  2. Machine front-matter: <!-- bugbot:meta {json} --> on line 2 (see bugbotMeta).
//  3. ## title
//  4. Human meta: Severity + Location (file:line) with optional source permalink.
//  5. Description (capped at 10 KB).
//  6. Candidate fix diff (when FixPatch != "", capped at 20 KB).
//  7. Inline reproduction <details> block (when ReproPath is set and readable).
//  8. Verification trace <details> block with 30 KB cap.
//  9. Attribution footer.
//
// Body size budget (GitHub hard limit: 65 536 chars):
//   - Description cap:  10 KB  (~10 240 chars)
//   - FixPatch cap:     20 KB  (~20 480 chars)
//   - Reasoning cap:    30 KB  (~30 720 chars)
//   - Repro cap:        25 KB  (~25 600 chars)
//   - Belt-and-braces:  if assembled body still exceeds ~60 000 chars it is
//     truncated at a safe point preserving lines 1-2 (fingerprint marker +
//     machine front-matter) and the attribution footer so recovery, tooling,
//     and attribution always survive.
//
// Security invariants:
//   - All fenced blocks use fencedBlock(), which auto-sizes the fence to be
//     longer than any backtick run inside the content (CommonMark rule), so
//     model-authored content cannot break out of the fence.
//   - Non-fenced model content placed inside <details> blocks (Reasoning) is
//     passed through sanitizeDetailsTag() so a literal </details> sequence
//     cannot close the block early.
func renderIssueBody(f domain.Finding, repoURL string) string {
	const (
		maxDescription = 10 * 1024 // 10 KB cap on model-authored description
		maxFixPatch    = 20 * 1024 // 20 KB cap on model-authored patch
		maxReasoning   = 30 * 1024 // 30 KB cap on verification trace
		maxBody        = 60_000    // belt-and-braces: stay under GitHub's 65 536 limit
	)

	var b strings.Builder

	// 1+2. Hidden fingerprint marker (line 1 — load-bearing for recovery;
	// must stay first) and the machine front-matter comment (line 2).
	// json.Marshal HTML-escapes '>' as \u003e inside string values, so no
	// field value can ever produce a literal "-->" that would close the
	// comment early. The marshal cannot fail: bugbotMeta is strings and ints.
	firstLine := "<!-- bugbot:fp=" + f.Fingerprint + " -->"
	metaJSON, _ := json.Marshal(bugbotMeta{
		Severity: string(f.Severity),
		Tier:     int(f.Tier),
		Lens:     f.Lens,
		File:     f.File,
		Line:     f.Line,
		Commit:   f.CommitSHA,
	})
	bodyPrefix := firstLine + "\n<!-- bugbot:meta " + string(metaJSON) + " -->\n\n"
	b.WriteString(bodyPrefix)

	// 3. Title heading.
	fmt.Fprintf(&b, "## %s\n\n", titleOrUnknown(f.Title))

	// 4. Human-facing meta: only Severity and Location.
	fmt.Fprintf(&b, "**Severity:** %s  \n", severityLabel(f.Severity))
	if repoURL != "" && f.CommitSHA != "" && f.File != "" {
		fmt.Fprintf(&b, "**Location:** [`%s:%d`](%s/blob/%s/%s#L%d)  \n\n",
			f.File, f.Line, repoURL, f.CommitSHA, f.File, f.Line)
	} else {
		fmt.Fprintf(&b, "**Location:** `%s:%d`  \n\n", f.File, f.Line)
	}

	// 5. Description — capped to prevent oversized model output from consuming
	// the whole body budget. A truncated description is still readable.
	if f.Description != "" {
		desc := f.Description
		if len(desc) > maxDescription {
			desc = truncateUTF8(desc, maxDescription) + "\n\n[... truncated by bugbot ...]"
		}
		b.WriteString(desc)
		b.WriteString("\n\n")
	}

	// 6. Candidate fix diff — fencedBlock auto-sizes the fence so a ``` run
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

	// 7. Inline reproduction block.
	b.WriteString(renderReproSection(f.ReproPath))

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
	// truncate at a safe byte boundary while preserving lines 1-2 (fingerprint
	// marker + bugbot:meta front-matter — load-bearing for issue recovery and
	// tooling) and the attribution footer.
	// sanitizeControlChars runs on the whole body before the size check. It is
	// safe to slice the sanitized body at len(bodyPrefix) below: the marker is
	// literal text + sha256 hex, the meta comment is JSON (Marshal escapes
	// control characters as \uXXXX), and "\n\n" is preserved whitespace, so
	// the prefix is byte-identical after sanitizing and len(bodyPrefix) still
	// names the exact post-prefix offset.
	body := sanitizeControlChars(b.String())
	if len(body) > maxBody {
		truncNote := "\n\n[... body truncated by bugbot: content exceeds GitHub's issue size limit ...]\n\n"
		available := maxBody - len(bodyPrefix) - len(truncNote) - len("\n") - len(footer)
		if available < 0 {
			available = 0
		}
		// truncateUTF8 walks back to a valid UTF-8 rune boundary, so the
		// truncated slice never breaks a multi-byte rune.
		mid := body[len(bodyPrefix):]
		if len(mid) > available {
			mid = truncateUTF8(mid, available)
		}
		body = bodyPrefix + mid + truncNote + footer
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

// bodyHashHex returns the sha256 hex digest of body. Stored as
// published_issues.body_hash so the apply loop's publishUpdate case can
// detect a byte-identical re-render (a metadata-only finding touch, not an
// actual content change) and skip the body push -- see planPublish's Update
// heuristic doc comment for why the planner over-selects and leaves this
// check to apply time.
func bodyHashHex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
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
