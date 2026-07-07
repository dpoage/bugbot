package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/util"
)

// markerFP is the hidden HTML-comment marker that keys a per-finding bugbot
// comment to its store fingerprint. It is ALWAYS the first line of the body so
// lifecycle sync can match an existing comment back to a finding without any
// server-side state. Bodies are otherwise deterministic for a given finding, so
// an unchanged finding renders byte-identical and the sync skips it.
const markerSummary = "<!-- bugbot:summary -->"

// markerResolved tags a per-finding comment whose finding is no longer
// reported. It is detected structurally (not by prose matching) so the CI gate
// can treat a resolved fingerprint as absent: if the finding reappears on a
// later push it counts as NEW again and trips the gate.
const markerResolved = "<!-- bugbot:resolved -->"

// fpMarkerRE extracts the fingerprint from a per-finding marker line.
var fpMarkerRE = regexp.MustCompile(`<!-- bugbot:fp=([^ ]+) -->`)

// markerFor renders the hidden fingerprint marker line for a finding.
func markerFor(fp string) string {
	return fmt.Sprintf("<!-- bugbot:fp=%s -->", fp)
}

// extractMarkerFP returns the fingerprint embedded in a comment body, or "" if
// the body is not a bugbot per-finding comment.
func extractMarkerFP(body string) string {
	if m := fpMarkerRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// isSummaryBody reports whether a comment body is the bugbot summary comment.
func isSummaryBody(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), markerSummary)
}

// renderInlineBody renders the deterministic body for an inline review comment
// anchored to a finding's file:line. The fingerprint marker leads so the comment
// is recoverable on re-run; the location is implicit (GitHub renders it from the
// anchor) so it is omitted from the prose.
func renderInlineBody(f domain.Finding) string {
	var b strings.Builder
	b.WriteString(markerFor(f.Fingerprint))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "**[%s] %s — %s**\n\n", f.Tier.Label(), severityLabel(f.Severity), titleOrUnknown(f.Title))

	if f.Description != "" {
		b.WriteString(f.Description)
		b.WriteString("\n\n")
	}

	if len(f.CorroboratingLenses) > 0 {
		fmt.Fprintf(&b, "Corroborated by: %s\n\n", strings.Join(f.CorroboratingLenses, ", "))
	}

	if f.Reasoning != "" {
		b.WriteString("<details><summary>Verification trace</summary>\n\n")
		b.WriteString(f.Reasoning)
		b.WriteString("\n\n</details>")
	}

	return strings.TrimRight(b.String(), "\n")
}

// resolvedNotice is the body an inline comment is PATCHed to when its finding
// is no longer reported. It keeps the fingerprint marker (so the comment stays
// matchable) plus the resolved marker (so the state is detectable). The body is
// deliberately SHA-free: it must be byte-stable across subsequent pushes or
// every resolved comment would be re-PATCHed on every new head commit.
func resolvedNotice(fp string) string {
	return fmt.Sprintf("%s\n%s\n\n✅ No longer reported.", markerFor(fp), markerResolved)
}

// isResolvedBody reports whether an existing comment body is a resolved notice.
func isResolvedBody(body string) bool {
	return strings.Contains(body, markerResolved)
}

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

// existingComment is a bugbot-authored comment already on the PR, recovered by
// listing review comments and issue comments and matching the marker. kind
// distinguishes the two GitHub comment APIs because updates target different
// endpoints (pulls/comments/<id> vs issues/comments/<id>).
type existingComment struct {
	ID   int64
	Body string
	Kind commentKind
}

type commentKind int

const (
	kindReview commentKind = iota // inline PR review comment
	kindIssue                     // PR conversation (issue) comment — used for the summary
)

// existingState is the set of pre-existing bugbot comments on a PR, indexed for
// lifecycle sync: per-finding comments by fingerprint, plus the single summary.
type existingState struct {
	byFingerprint map[string]existingComment
	summary       *existingComment
}

// loadExisting lists the PR's review comments and issue comments, extracts the
// bugbot markers, and builds the fingerprint→comment and summary indexes used by
// sync. Both lists are paginated. gh reads here are safe in dry-run.
func loadExisting(ctx context.Context, gh GHRunner, pr int) (existingState, error) {
	st := existingState{byFingerprint: map[string]existingComment{}}

	reviewRaw, err := gh(ctx, "api", "--paginate", fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", pr))
	if err != nil {
		return existingState{}, fmt.Errorf("list review comments: %w", err)
	}
	reviewComments, err := parseComments(reviewRaw)
	if err != nil {
		return existingState{}, fmt.Errorf("parse review comments: %w", err)
	}
	for _, c := range reviewComments {
		if fp := extractMarkerFP(c.Body); fp != "" {
			st.byFingerprint[fp] = existingComment{ID: c.ID, Body: c.Body, Kind: kindReview}
		}
	}

	issueRaw, err := gh(ctx, "api", "--paginate", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", pr))
	if err != nil {
		return existingState{}, fmt.Errorf("list issue comments: %w", err)
	}
	issueComments, err := parseComments(issueRaw)
	if err != nil {
		return existingState{}, fmt.Errorf("parse issue comments: %w", err)
	}
	for _, c := range issueComments {
		if isSummaryBody(c.Body) {
			s := existingComment{ID: c.ID, Body: c.Body, Kind: kindIssue}
			st.summary = &s
			continue
		}
		if fp := extractMarkerFP(c.Body); fp != "" {
			// A per-finding finding that fell back to an issue comment in some prior
			// version; index it so it stays matchable.
			st.byFingerprint[fp] = existingComment{ID: c.ID, Body: c.Body, Kind: kindIssue}
		}
	}

	return st, nil
}

// apiComment is the subset of a GitHub comment payload we read. The --paginate
// flag may concatenate multiple JSON arrays; parseComments handles that.
type apiComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// parseComments decodes the (possibly concatenated) JSON arrays gh --paginate
// emits. A single page is a JSON array; --paginate concatenates pages back to
// back, so we stream-decode arrays until the input is exhausted.
func parseComments(raw []byte) ([]apiComment, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var out []apiComment
	for dec.More() {
		var page []apiComment
		if err := dec.Decode(&page); err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

// syncAction is one decided lifecycle operation, recorded for the dry-run/stats
// output and applied (unless dry-run) by applyAction.
type syncAction struct {
	op   syncOp
	kind commentKind
	id   int64  // target comment id for update/resolve; 0 for create
	body string // rendered body to post/patch
	// path/line/commit are set only for inline create.
	path   string
	line   int
	commit string
	fp     string // fingerprint this action concerns ("" for summary)
}

type syncOp int

const (
	opCreate syncOp = iota
	opUpdate
	opSkip // identical body, nothing to do (counted as unchanged)
	opResolve
)

func (o syncOp) String() string {
	switch o {
	case opCreate:
		return "create"
	case opUpdate:
		return "update"
	case opSkip:
		return "skip"
	case opResolve:
		return "resolve"
	default:
		return "unknown"
	}
}

// planResult holds the decided sync plan and the gate inputs derived while
// planning (which findings are NEW vs already present on the PR).
type planResult struct {
	actions []syncAction
	// newGateFingerprints are tier<=2 findings whose fingerprint was NOT already
	// present on the PR before this run — the set the CI exit gate keys on.
	newGateFingerprints map[string]bool
}

// renderedFinding pairs a finding with whether it is commentable inline.
type renderedFinding struct {
	finding     domain.Finding
	commentable bool
}

// planSync decides the full lifecycle plan for a run: which per-finding comments
// to create/update/skip, which stale comments to resolve, and the summary
// create-or-update. It is pure over its inputs (findings, commentable lines,
// existing state, head SHA, config) so it is fully unit-testable without gh.
func planSync(
	res *funnel.Result,
	commentable commentableLines,
	existing existingState,
	headSHA string,
	suspectedMode string,
) planResult {
	plan := planResult{newGateFingerprints: map[string]bool{}}
	headShort := util.ShortSHA(headSHA)

	// Partition findings: inline-eligible (tier<=2, commentable line) vs
	// summary-only (everything else surfaced).
	var inline []renderedFinding
	var summaryFindings []domain.Finding
	current := map[string]bool{} // fingerprints reported this run

	for _, f := range res.Findings {
		// T3 may be withheld entirely.
		if f.Tier >= domain.TierSuspected && suspectedMode == "withhold" {
			continue
		}
		current[f.Fingerprint] = true

		isInline := f.Tier <= domain.TierVerified && commentable.has(f.File, f.Line)
		if isInline {
			inline = append(inline, renderedFinding{finding: f, commentable: true})
		}
		summaryFindings = append(summaryFindings, f)

		// Gate: a NEW tier<=2 finding (fingerprint not already on the PR) trips
		// CI. A resolved notice does NOT count as "already on the PR": a finding
		// that cleared on an earlier push and reappears (fix-then-revert) must
		// trip the gate again, not slide through on its tombstone.
		if f.Tier <= domain.TierVerified {
			prev, seen := existing.byFingerprint[f.Fingerprint]
			if !seen || isResolvedBody(prev.Body) {
				plan.newGateFingerprints[f.Fingerprint] = true
			}
		}
	}

	// Per-finding inline comments: create / update / skip.
	for _, rf := range inline {
		f := rf.finding
		body := renderInlineBody(f)
		if prev, ok := existing.byFingerprint[f.Fingerprint]; ok {
			if prev.Body == body {
				plan.actions = append(plan.actions, syncAction{op: opSkip, kind: prev.Kind, id: prev.ID, fp: f.Fingerprint, body: body})
			} else {
				plan.actions = append(plan.actions, syncAction{op: opUpdate, kind: prev.Kind, id: prev.ID, fp: f.Fingerprint, body: body})
			}
			continue
		}
		plan.actions = append(plan.actions, syncAction{
			op: opCreate, kind: kindReview, fp: f.Fingerprint, body: body,
			path: f.File, line: f.Line, commit: headSHA,
		})
	}

	// Stale resolution: any existing per-finding comment whose fingerprint is NOT
	// in the current run gets its body PATCHed to a resolved notice (never
	// deleted). Skip ones already showing the notice to keep re-runs idempotent.
	var staleFPs []string
	for fp := range existing.byFingerprint {
		if !current[fp] {
			staleFPs = append(staleFPs, fp)
		}
	}
	sort.Strings(staleFPs)
	for _, fp := range staleFPs {
		prev := existing.byFingerprint[fp]
		// Already-resolved comments are detected by marker, not exact body, so a
		// notice from any prior run stays untouched on subsequent pushes.
		if isResolvedBody(prev.Body) {
			plan.actions = append(plan.actions, syncAction{op: opSkip, kind: prev.Kind, id: prev.ID, fp: fp, body: prev.Body})
			continue
		}
		plan.actions = append(plan.actions, syncAction{op: opResolve, kind: prev.Kind, id: prev.ID, fp: fp, body: resolvedNotice(fp)})
	}

	// Summary comment: always create-or-update (the re-run signal). Mark which
	// summary findings were posted inline so the summary can keep them brief.
	inlineFP := map[string]bool{}
	for _, rf := range inline {
		inlineFP[rf.finding.Fingerprint] = true
	}
	summaryBody := renderSummaryBody(res, summaryFindings, inlineFP, headShort, suspectedMode)
	if existing.summary != nil {
		if existing.summary.Body == summaryBody {
			plan.actions = append(plan.actions, syncAction{op: opSkip, kind: kindIssue, id: existing.summary.ID, body: summaryBody})
		} else {
			plan.actions = append(plan.actions, syncAction{op: opUpdate, kind: kindIssue, id: existing.summary.ID, body: summaryBody})
		}
	} else {
		plan.actions = append(plan.actions, syncAction{op: opCreate, kind: kindIssue, body: summaryBody})
	}

	return plan
}

// renderSummaryBody renders the deterministic summary comment: a verdict line, a
// table of all surfaced findings (file:line, tier, severity, inline marker), the
// degraded/skip warnings, and a stats footer with the same numbers scan prints.
func renderSummaryBody(res *funnel.Result, findings []domain.Finding, inlineFP map[string]bool, headShort, suspectedMode string) string {
	var b strings.Builder
	b.WriteString(markerSummary)
	b.WriteString("\n\n## Bugbot review\n\n")

	var verified, suspected int
	for _, f := range findings {
		switch {
		case f.Tier <= domain.TierVerified:
			verified++
		case f.Tier >= domain.TierSuspected:
			suspected++
		}
	}

	fmt.Fprintf(&b, "**Verdict:** %d verified, %d suspected (head %s)\n\n", verified, suspected, headShort)

	if len(findings) == 0 {
		if res.Stats.FinderReliable() {
			b.WriteString("No verified findings in the changed code.\n\n")
		} else {
			b.WriteString("No findings were recovered, but this run is NOT a clean bill of health (see reliability warning below).\n\n")
		}
	} else {
		b.WriteString("| Tier | Severity | Location | Finding | Inline |\n")
		b.WriteString("| --- | --- | --- | --- | --- |\n")
		for _, f := range findings {
			inline := ""
			if inlineFP[f.Fingerprint] {
				inline = "✅"
			}
			fmt.Fprintf(&b, "| T%d | %s | `%s:%d` | %s | %s |\n",
				f.Tier, severityLabel(f.Severity), f.File, f.Line, titleOrUnknown(f.Title), inline)
		}
		b.WriteString("\n")
	}

	if suspectedMode == "withhold" {
		b.WriteString("_Suspected (T3) findings are withheld by configuration._\n\n")
	}

	writeReviewWarnings(&b, res)
	writeStatsFooter(&b, res.Stats)

	return strings.TrimRight(b.String(), "\n")
}

// writeReviewWarnings renders degradation/skip/reliability notes into the summary.
func writeReviewWarnings(b *strings.Builder, res *funnel.Result) {
	if res.Degraded || res.Stopped {
		fmt.Fprintf(b, "> Budget: degraded=%v stopped=%v\n\n", res.Degraded, res.Stopped)
	}
	for _, note := range res.Skipped {
		fmt.Fprintf(b, "> Skipped: %s\n\n", note)
	}
	if !res.Stats.FinderReliable() {
		s := res.Stats
		fmt.Fprintf(b, "> ⚠️ Reliability: %d of %d finder agents produced no parseable output; coverage is incomplete.\n\n",
			s.FinderFailures, s.FinderRuns)
	}
}

// writeStatsFooter renders the same spend/agent numbers scan.go prints, in a
// collapsed details block to keep the summary compact.
func writeStatsFooter(b *strings.Builder, s funnel.Stats) {
	b.WriteString("<details><summary>Run stats</summary>\n\n")
	fmt.Fprintf(b, "- Stages: hypothesized=%d triaged=%d verified=%d killed=%d\n",
		s.Hypothesized, s.Triaged, s.Verified, s.Killed)
	fmt.Fprintf(b, "- Agents: finders=%d/%d failed, verifiers=%d/%d failed\n",
		s.FinderFailures, s.FinderRuns, s.VerifierFailures, s.VerifierRuns)
	if s.MergedWithinLens > 0 || s.MergedCrossLens > 0 || s.MergedRootCause > 0 {
		fmt.Fprintf(b, "- Location merges: within_lens=%d cross_lens=%d root_cause=%d\n", s.MergedWithinLens, s.MergedCrossLens, s.MergedRootCause)
	}
	fmt.Fprintf(b, "- Tokens: input=%d output=%d total=%d\n", s.InputTokens, s.OutputTokens, s.InputTokens+s.OutputTokens)
	if s.CacheReadTokens > 0 || s.CacheCreationTokens > 0 {
		fmt.Fprintf(b, "- Cache: read=%d created=%d\n", s.CacheReadTokens, s.CacheCreationTokens)
	}
	if s.SandboxExecs > 0 {
		fmt.Fprintf(b, "- Sandbox: execs=%d total_ms=%d\n", s.SandboxExecs, s.SandboxExecMillis)
	}
	if s.LeadsPosted > 0 || s.LeadsConsumed > 0 {
		fmt.Fprintf(b, "- Leads: posted=%d consumed=%d\n", s.LeadsPosted, s.LeadsConsumed)
	}
	b.WriteString("\n</details>")
}
