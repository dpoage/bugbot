// cartographer_regen.go holds the summary normalization, per-package summarization, and out-of-band refresh extracted from cartographer.go for readability.
// Pure code motion: no logic changes.

package funnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/util"
)

// cartographySummarySchema constrains the package-summary completion to a
// single {"summary": string} object. Routing the summary through RunJSON with
// this schema (instead of a bare client.Complete) gives the cartographer the
// same guarantees every other agent has: shape validation, a one-shot repair
// round-trip when the first completion is malformed (so a bad summary is fixed,
// not silently dropped), and think-block stripping via stripBody.
var cartographySummarySchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","minLength":1,"description":"<=120 word package summary"}},"required":["summary"],"additionalProperties":false}`)

// cartographySummaryMaxWords is the post-processed word cap applied to every
// package summary before it is persisted or returned. It matches the 120-word
// limit the system prompt requests. The normalizer enforces it
// deterministically (truncating to the cap and appending a single ellipsis
// character when the model overshoots), independent of model behavior.
const cartographySummaryMaxWords = 120

// cartographySummaryHeadingRE matches ONE leading markdown heading line and any
// optional label it carries, e.g. "# Package Summary", "## Summary", "#
// Package: <name>", "### Anything". normalizeSummary applies it in a loop to
// strip a stacked heading block. Anchored at the start (no multiline flag) so
// it only ever strips the leading line, never a mid-text '#'. A summary that is
// nothing but a hash line collapses to "" and the package is dropped from the
// regen batch — acceptable since the prompt forbids markdown headings.
var cartographySummaryHeadingRE = regexp.MustCompile(`^\s*(?:#+\s*).*?(?:\n|$)`)

// cartographySummaryLabelRE matches a leading bold-label preamble, e.g.
// "**Purpose:**", "**Package Purpose:**", "**Purpose**:", "**Goal** -",
// "**Overview**:". The label word(s) and the optional colon (inside or
// outside the bold) are stripped; the remainder of the line is discarded so
// the first real sentence stands on its own.
var cartographySummaryLabelRE = regexp.MustCompile(`(?ims)^\s*\*\*[^*\n]*\*\*\s*[:\-]?\s*\n?`)

// normalizeSummary deterministically cleans a model-produced package summary
// into a single bounded paragraph regardless of the model's output shape.
// Steps, in order:
//
//  1. Strip every leading markdown heading line (a stacked "# X\n## Y" block
//     included), each with any optional label like "Package Summary" or
//     "Package: <name>". A heading must have whitespace after the hashes.
//  2. Strip a leading bold-label preamble ("**Purpose:**",
//     "**Package Purpose:**", "**Purpose**:", case-insensitive, with the
//     colon either inside or outside the bold).
//  3. Collapse every run of whitespace (spaces, tabs, newlines) into a
//     single space and trim the result.
//  4. Enforce a word cap (cartographySummaryMaxWords). If the model
//     overshoots, keep the first cap words and append a single ellipsis
//     character. Words are counted with strings.Fields, which already
//     operates on runes (so multibyte CJK / accented text is never split
//     mid-character and the resulting count matches what a human reader
//     would call "words").
//
// The two leading-shape regexes are precompiled at package scope: this
// function is called once per package per regen pass, so the cost is
// negligible, and a regex is both clearer and safer than hand-rolled byte
// scanning for the multi-character heading/label shapes we tolerate. All
// other work is plain strings (TrimSpace, Fields, Join) — no extra
// allocations from scanning once the regexes have matched.
func normalizeSummary(s string) string {
	// 1. Strip every leading markdown heading line. Looped because the regex
	//    is start-anchored: one ReplaceAllString removes only the first
	//    heading; re-running re-anchors ^ at the reduced string so a stacked
	//    "# Title\n## Subtitle" block is fully removed.
	for cartographySummaryHeadingRE.MatchString(s) {
		s = cartographySummaryHeadingRE.ReplaceAllString(s, "")
	}
	// 2. Bold-label preamble.
	s = cartographySummaryLabelRE.ReplaceAllString(s, "")
	// 3. Collapse whitespace. fields + Join is the idiomatic,
	//    allocation-light "split on any unicode whitespace, rejoin with
	//    single spaces" combo used elsewhere in this package (e.g.
	//    prompt.go, strategy.go).
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return ""
	}
	// 4. Word cap.
	words := strings.Fields(s)
	if len(words) > cartographySummaryMaxWords {
		words = words[:cartographySummaryMaxWords]
		return strings.Join(words, " ") + " …"
	}
	return s
}

// summarizePackage builds the bounded input for one package's summary and runs
// a zero-tool agent.Runner via RunJSON to produce it. The input is the
// package's member files head-truncated to DefaultCartographerHeadLines, the
// whole package capped at DefaultCartographerInputBytes. The runner shares the
// finder budget pool (via budget.finderRunnerLimits) so an in-flight summary
// respects the run-wide token budget; budget may be nil (no pool gating). The
// output is the schema's "summary" field, deterministically normalized to a
// single bounded paragraph (see normalizeSummary) before being returned; an
// empty result is reported as an error so the caller drops the package from
// the regen batch.
func (f *Funnel) summarizePackage(ctx context.Context, client llm.Client, budget *budgetState, pkg string, members []string, fps map[string]string) (string, error) {
	if len(members) == 0 {
		return "", errors.New("cartograph: empty members for package")
	}
	root := f.repo.Root()

	// Bound the member set: at most DefaultCartographerMaxFiles. Pick the
	// first N (members are already deterministic-sorted by
	// packagesSpanned) so the chosen set is reproducible.
	if len(members) > DefaultCartographerMaxFiles {
		members = members[:DefaultCartographerMaxFiles]
	}

	// Compose the user message: a brief preamble then a per-file block.
	var body strings.Builder
	body.WriteString("Package: ")
	body.WriteString(pkg)
	body.WriteString("\n\nFiles:\n")
	const perFileHead = DefaultCartographerInputBytes / 4 // soft budget: each file gets a quarter of the cap
	for _, rel := range members {
		if body.Len() >= DefaultCartographerInputBytes {
			body.WriteString("  [additional files omitted to fit budget]\n")
			break
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		content, err := readFileHead(abs, DefaultCartographerHeadLines, perFileHead)
		if err != nil {
			// Unreadable file (deleted, race): skip with a one-liner so
			// the model knows the file was once here.
			fmt.Fprintf(&body, "--- %s ---\n  (unreadable: %v)\n", rel, err)
			continue
		}
		// Stop writing more files once the running total exceeds the
		// cap. The per-file head was sized so a typical file fits
		// comfortably, but a single oversized file is truncated at its
		// own cap rather than spilling the rest.
		projected := body.Len() + len(content) + len(rel) + 8
		if projected > DefaultCartographerInputBytes {
			body.WriteString("  [additional files omitted to fit budget]\n")
			break
		}
		body.WriteString("--- ")
		body.WriteString(rel)
		body.WriteString(" ---\n")
		body.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			body.WriteString("\n")
		}
	}

	limits := f.opts.Limits.FinderLimits
	if budget != nil {
		// The cartographer shares the finder pool, so its per-run limits and
		// per-turn budget check come from finderRunnerLimits — the same hook
		// finders use to stop mid-run when the shared budget is exhausted.
		limits = budget.finderRunnerLimits(f.opts.Limits.FinderLimits)
	}
	// Surface this per-package summary as an in-flight agent in `bugbot status`
	// and the live pane via the shared AgentScope seam. The cartographer drives
	// a single tool-less completion, so there is no per-turn tool-call activity;
	// the started/finished bracket is what shows the operator the cartograph
	// stage is doing work (and on which package).
	progress.NewAgentScope(f.opts.Progress, progress.RoleCartographer, pkg).Start()
	runner := f.newAgentRunner(client, nil, cartographySystemPrompt, limits)
	var out struct {
		Summary string `json:"summary"`
	}
	start := time.Now()
	outcome, err := runner.RunJSON(ctx, body.String(), cartographySummarySchema, &out)
	emitAgentFinished(f.opts.Progress, progress.RoleCartographer, pkg, outcome, start, err)
	if err != nil {
		return "", err
	}
	// Deterministic post-process: strip any leading heading/label the
	// model added, collapse to one paragraph, enforce the word cap. This
	// is the byte-uniform guarantee on the summary regardless of model
	// behavior; the system prompt forbids the same shapes as cheap
	// belt-and-suspenders but cannot enforce them.
	summary := normalizeSummary(out.Summary)
	if summary == "" {
		return "", errors.New("cartograph: empty summary from LLM")
	}
	return summary, nil
}

// readFileHead returns the first maxLines lines of abs, capped at
// maxBytes. Used by the cartographer to bound each member file's
// contribution to the summary input without reading it whole.
//
// Lines are read with bufio.Scanner, which counts newlines correctly on
// every supported OS. The byte cap is a soft second constraint applied
// AFTER line-capping: a file with 5 long lines stops at the first newline
// that pushes the running total over maxBytes.
func readFileHead(abs string, maxLines, maxBytes int) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	// Allow long lines (e.g. minified blobs) without breaking the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var b strings.Builder
	line := 0
	for sc.Scan() {
		b.Write(sc.Bytes())
		b.WriteByte('\n')
		line++
		if line >= maxLines || b.Len() >= maxBytes {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return b.String(), err
	}
	return b.String(), nil
}

// packagesSpanned groups targets by their parent package directory. The
// returned map is keyed by package (repo-relative dir) and its value is the
// sorted list of member files. Repo-root files (path.Dir == ".") are SKIPPED:
// the root holds build/config/doc files rather than a coherent code package,
// its empty key cannot be persisted (UpsertPackageSummaries rejects an empty
// Pkg, and as one transaction a single such row would poison the whole batch),
// and contextFor never injects it. Sort is by path so the fingerprint and the
// DefaultCartographerMaxFiles subset are deterministic.
func packagesSpanned(targets []string) map[string][]string {
	pkgs := make(map[string][]string)
	for _, t := range targets {
		d := path.Dir(t)
		if d == "." {
			continue // repo-root file: not a storable package (see doc)
		}
		pkgs[d] = append(pkgs[d], t)
	}
	for d := range pkgs {
		sort.Strings(pkgs[d])
	}
	return pkgs
}

// packageFingerprint is the deterministic package fingerprint used as the
// cache key. The recipe (per the contract): for each member p in sorted
// order, append p + NUL + fps[p] + LF; feed the result to
// ingest.HashBytes. Empty members or empty fps[p] are tolerated (the
// resulting fingerprint still changes when content does), but callers
// that want a strict "must have content" guarantee should pre-filter.
func packageFingerprint(pkg string, members []string, fps map[string]string) string {
	var b strings.Builder
	for _, m := range members {
		b.WriteString(m)
		b.WriteByte(0)
		b.WriteString(fps[m])
		b.WriteByte('\n')
	}
	_ = pkg // pkg name is implicit in the members (their path.Dir matches it)
	return ingest.HashBytes([]byte(b.String()))
}

// CartographyReport summarizes an out-of-band cartographer refresh.
type CartographyReport struct {
	ScanRunID    string // the scan_run (kind "cartography") this refresh ledgered to
	Packages     int    // packages spanned by the repo snapshot
	Summarized   int    // packages (re)generated and persisted this run
	Reused       int    // packages whose cached summary fingerprint still matched
	Failed       int    // packages whose summary generation returned empty/error
	InputTokens  int64  // total input tokens billed by the refresh
	OutputTokens int64  // total output tokens billed by the refresh
}

// RefreshCartography runs the cartographer pass out-of-band — no finder or
// verify stages — over the whole repo snapshot: it (re)summarizes every package
// whose content fingerprint changed and persists the results, exactly the
// fingerprint-cached summaries a scan's cartographer pass produces. Spend is
// ledgered to a fresh scan_run of kind "cartography" (so it shows in the
// metrics series, classified as a cartographer run). client is the unwrapped
// cartographer LLM client; it is recorder-wrapped internally. Unlike the
// in-scan pass this does NOT gate on a finder budget — a manual refresh runs to
// completion — and it returns counts so the caller can report what happened.
func (f *Funnel) RefreshCartography(ctx context.Context, client llm.Client) (CartographyReport, error) {
	var rep CartographyReport
	if client == nil {
		return rep, errors.New("cartographer: nil client")
	}
	snap, err := f.repo.Snapshot(ctx, f.opts.Discovery.Filter)
	if err != nil {
		return rep, fmt.Errorf("cartographer: snapshot: %w", err)
	}
	fps, err := f.repo.Fingerprints(ctx, snap)
	if err != nil {
		return rep, fmt.Errorf("cartographer: fingerprints: %w", err)
	}
	targets := make([]string, len(snap.Files))
	for i, file := range snap.Files {
		targets[i] = file.Path
	}
	packages := packagesSpanned(targets)
	rep.Packages = len(packages)
	if len(packages) == 0 {
		return rep, nil
	}

	runID, err := f.store.BeginScanRun(ctx, store.ScanCartography, snap.Commit)
	if err != nil {
		return rep, fmt.Errorf("cartographer: begin run: %w", err)
	}
	rep.ScanRunID = runID
	rec := &spendRecorder{ctx: ctx, store: f.store, scanRunID: runID}
	cc := llm.WithRecorder(client, rec, roleCartographer, "", "")

	pkgFingerprints := make(map[string]string, len(packages))
	for pkg, members := range packages {
		pkgFingerprints[pkg] = packageFingerprint(pkg, members, fps)
	}
	keys := util.SortedKeys(pkgFingerprints)
	cached, cErr := f.store.GetPackageSummaries(ctx, keys)
	if cErr != nil {
		cached = nil // degrade: regenerate everything
	}
	// Reused = cache hits; everything else goes on the regen list. keys is
	// already sorted (sortedKeys), so the launch order is reproducible.
	var toRegen []string
	for _, pkg := range keys {
		fp := pkgFingerprints[pkg]
		if row, ok := cached[pkg]; ok && row.Fingerprint == fp && row.Summary != "" {
			rep.Reused++
			continue
		}
		toRegen = append(toRegen, pkg)
	}

	// Summarize and persist each uncached package concurrently and on the fly,
	// so a manual refresh interrupted partway keeps every summary already
	// written. No finder-budget gating: a manual refresh runs to completion.
	var persistErr error
	f.regenSummaries(ctx, cc, packages, pkgFingerprints, fps, toRegen, nil,
		func(r regenResult) {
			switch {
			case r.err == nil:
				rep.Summarized++
			case r.stage == "persist":
				rep.Failed++
				persistErr = r.err
			default: // summarize failure
				rep.Failed++
			}
		})

	rep.InputTokens, rep.OutputTokens, _, _ = rec.totals()
	statsBlob, _ := json.Marshal(Stats{
		CartographerEnabled: true,
		InputTokens:         rep.InputTokens,
		OutputTokens:        rep.OutputTokens,
	})
	_ = f.store.FinishScanRun(ctx, runID, string(statsBlob))
	if persistErr != nil {
		return rep, fmt.Errorf("cartographer: persist: %w", persistErr)
	}
	return rep, nil
}
