package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/progress"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// reachClass categorises why a finding was or was not downranked.
type reachClass int

const (
	reachKnownDownrank reachClass = iota // deterministically dead/platform/test → low
	reachKnownKeep                       // deterministically reachable → keep severity
	reachAmbiguous                       // unclear; route to LLM adjudication
)

// reachResult is the output of classifyReachability.
type reachResult struct {
	class       reachClass
	severity    domain.Severity
	rationale   string // verdict_detail / human rationale for keep/downrank
	callerFacts string // LLM prompt facts for ambiguous cases (empty when class != reachAmbiguous)
}

// ambiguousEntry pairs an index into the findings slice with the finding value
// for LLM adjudication, plus a human-readable description of why the case is
// ambiguous (used in the LLM prompt).
type ambiguousEntry struct {
	idx         int
	fi          domain.Finding
	callerFacts string
}

// adjResult carries the LLM-resolved severity and rationale for one finding.
type adjResult struct {
	idx       int
	severity  string
	rationale string
}

// impactSweep is the Stage F end-of-scan reachability/impact re-ranker.
// It classifies each surviving finding's reachability deterministically first,
// then batches any ambiguous cases into a single LLM call (zero calls when
// everything is classified deterministically or the client is nil).
//
// A sweep failure never aborts the run; errors are recorded via f.note.
func (f *Funnel) impactSweep(
	ctx context.Context,
	findings []domain.Finding,
	repoRoot string,
	verifierClient llm.Client,
	budgetStopped bool,
	result *Result,
) {
	if len(findings) == 0 {
		return
	}

	ts := treesitter.New(repoRoot)

	// Phase 1: deterministic classification.
	var ambiguous []ambiguousEntry

	for i := range findings {
		fi := &findings[i]
		r := classifyReachability(fi, repoRoot, ts)
		switch r.class {
		case reachKnownDownrank, reachKnownKeep:
			fi.Severity = r.severity
			fi.VerdictDetail = r.rationale
			if err := f.store.UpdateFindingSeverity(ctx, fi.ID, r.severity, r.rationale); err != nil {
				f.note(result, fmt.Sprintf("impact_sweep: UpdateFindingSeverity id=%s: %v", fi.ID, err))
			}
		case reachAmbiguous:
			ambiguous = append(ambiguous, ambiguousEntry{idx: i, fi: *fi, callerFacts: r.callerFacts})
		}
	}

	// Phase 2: optional batched LLM adjudication for ambiguous findings.
	// Skip the LLM call when the budget is already stopped (no headroom for
	// additional spend) or when no client is available.
	if len(ambiguous) == 0 {
		return
	}
	if verifierClient == nil {
		f.note(result, fmt.Sprintf("impact_sweep: %d ambiguous finding(s) not adjudicated (no LLM client)", len(ambiguous)))
		return
	}
	if budgetStopped {
		f.note(result, fmt.Sprintf("impact_sweep: %d ambiguous finding(s) not adjudicated (budget stopped)", len(ambiguous)))
		return
	}

	llmResults, err := adjudicateImpact(ctx, verifierClient, ambiguous, f.opts.Progress)
	if err != nil {
		f.note(result, fmt.Sprintf("impact_sweep: LLM adjudication failed: %v", err))
		return
	}

	for _, adj := range llmResults {
		if adj.idx < 0 || adj.idx >= len(findings) {
			continue
		}
		fi := &findings[adj.idx]
		sev, ok := domain.ParseSeverity(adj.severity)
		if !ok {
			sev = fi.Severity // keep original on bad parse
		}
		rationale := adj.rationale
		if rationale == "" {
			rationale = fmt.Sprintf("LLM impact adjudication: %s", sev)
		}
		fi.Severity = sev
		fi.VerdictDetail = rationale
		if err := f.store.UpdateFindingSeverity(ctx, fi.ID, sev, rationale); err != nil {
			f.note(result, fmt.Sprintf("impact_sweep: UpdateFindingSeverity id=%s: %v", fi.ID, err))
		}
	}
}

// classifyReachability deterministically classifies a finding's reachability.
//
// Symbol derivation: we derive the implicated symbol from fi.File + fi.Line via
// tree-sitter's enclosing-definition query (EnclosingDefinition), NOT by
// string-splitting the prose Title. Real finder titles are prose ("nil deref of
// cfg in Greeting"), not "Symbol: description", so title-based extraction is
// inert on production data.
//
// Decision ladder:
//  1. Platform-only file (_windows.go, #ifdef _WIN32) on Linux → low.
//  2. Test-only file → low.
//  3. Zero non-test callers of the innermost enclosing definition (derived from
//     File+Line via EnclosingDefinition) → low (unless an entrypoint exception applies).
//     Same-name collision risk: when references span many distinct files (>5)
//     the name is too generic to trust — route to adjudication.
//  4. One or more non-test callers found → keep severity (reachable).
//  5. No enclosing definition found / unsupported file type → ambiguous (LLM).
//  6. Exported Go symbol with zero in-repo callers → deterministic keep-with-rationale.
//     A public Go API may have external importers the in-repo scan cannot see;
//     the verdict_detail records the zero-caller evidence for human review.
//  7. C/C++ header declaration with zero callers → ambiguous (LLM). Headers are
//     the primary dead-code locus in the corpus; the LLM can distinguish a
//     genuinely-dead application header member from an intended public-library API.
func classifyReachability(fi *domain.Finding, repoRoot string, ts *treesitter.Backend) reachResult {
	keep := func(rat string) reachResult {
		return reachResult{class: reachKnownKeep, severity: fi.Severity, rationale: rat}
	}
	down := func(rat string) reachResult {
		return reachResult{class: reachKnownDownrank, severity: domain.SeverityLow, rationale: rat}
	}
	ambig := func(facts string) reachResult {
		return reachResult{class: reachAmbiguous, severity: fi.Severity, callerFacts: facts}
	}

	// Guard 1: platform-gated code not compiled on Linux.
	if isWindowsOrMacOnly(fi.File, repoRoot) {
		return down(fmt.Sprintf("platform-only path (%s) not compiled on Linux target", filepath.Base(fi.File)))
	}

	// Guard 2: test-only file → not reachable from production.
	if isTestFile(fi.File) {
		return down(fmt.Sprintf("finding in test file %s; not reachable from production code", filepath.Base(fi.File)))
	}

	// Guard 3-7: caller-count analysis via tree-sitter.
	absPath := filepath.Join(repoRoot, fi.File)
	if !ts.Supports(absPath) {
		// No grammar for this language; can't do caller analysis.
		return ambig("no tree-sitter grammar for this file type; caller count unknown")
	}

	// Derive the implicated symbol from the enclosing definition at the reported
	// line. fi.Line is 1-based (as stored by the finder), so convert to 0-based.
	lineNum := fi.Line - 1
	if lineNum < 0 {
		lineNum = 0
	}
	symbol, found := ts.EnclosingDefinition(absPath, lineNum)
	if !found || symbol == "" {
		// No enclosing definition at this line — file-level code or parse gap.
		return ambig("no enclosing definition found at the reported line; reachability unknown")
	}

	// Handle qualified C++ names in the symbol (should not happen via
	// EnclosingDefinition since it returns the @name capture, but be defensive):
	// if the found symbol contains "::", use the last segment (the method).
	if idx := strings.LastIndex(symbol, "::"); idx >= 0 {
		symbol = symbol[idx+2:]
	}

	// Count non-test callers across the repo.
	refs, err := ts.References(absPath, symbol)
	if err != nil {
		// Tree-sitter error (grammar compile failure, etc.) — route to adjudication.
		return ambig(fmt.Sprintf("tree-sitter reference query for %q failed: %v", symbol, err))
	}

	ext := strings.ToLower(filepath.Ext(fi.File))

	// Same-name collision guard: when references span more than 5 distinct
	// non-test files the bare symbol name is too generic to trust (e.g. "write",
	// "read", "bind" appear on every receiver type). Route to LLM adjudication
	// rather than trusting the raw count. Threshold of 5 is conservative: a dead
	// method whose name collides in 2-5 files may still be falsely kept, but
	// raising the threshold risks flooding the LLM with noisy inputs.
	nDistinct := distinctFiles(refs, repoRoot)
	if nDistinct > 5 {
		return ambig(fmt.Sprintf("symbol %q referenced in %d distinct non-test files — name collision likely; raw caller count unreliable", symbol, nDistinct))
	}

	callerCount := countNonTestCallers(refs, repoRoot)
	if callerCount > 0 {
		return keep(fmt.Sprintf("%d non-test caller(s) of %q found repo-wide; severity unchanged", callerCount, symbol))
	}

	// Zero callers. Whether we downrank deterministically depends on whether
	// external callers could plausibly exist.

	// Exported Go symbol: deterministic keep-with-rationale. A public Go API
	// may have external importers the in-repo scan cannot see. The verdict_detail
	// records the zero-caller evidence for human review without triggering an
	// LLM call (preventing unexpected LLM spend in small-budget runs).
	if ext == ".go" && len(symbol) > 0 && symbol[0] >= 'A' && symbol[0] <= 'Z' {
		return keep(fmt.Sprintf("exported Go symbol %q: zero in-repo callers found; possible dead API surface but external importers cannot be ruled out — severity preserved", symbol))
	}

	// C/C++ header declaration: route to LLM. Headers are the primary dead-code
	// locus in the corpus (Surface ctor, JSON::write, RegisterableFuncType::bind)
	// but may also be public library APIs. The LLM can distinguish a genuinely-
	// dead application header member from an intended external entrypoint.
	if ext == ".h" || ext == ".hpp" || ext == ".hh" || ext == ".hxx" {
		return ambig(fmt.Sprintf("C/C++ header symbol %q: zero non-test callers in this repo; may be a public API or dead application surface", symbol))
	}

	// Unexported symbol in a non-header, grammar-supported file, zero callers
	// → dead code → downrank.
	return down(fmt.Sprintf("zero non-test callers of %q found repo-wide by AST reference scan", symbol))
}

// distinctFiles counts how many distinct non-test files appear in refs.
func distinctFiles(refs treesitter.Result, repoRoot string) int {
	seen := make(map[string]bool, len(refs.Locations))
	for _, loc := range refs.Locations {
		path := strings.TrimPrefix(string(loc.URI), "file://")
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			rel = path
		}
		if !isTestFile(rel) {
			seen[path] = true
		}
	}
	return len(seen)
}

// isWindowsOrMacOnly reports whether the file is gated to a non-Linux platform.
// Checks file-path patterns (OS-specific filename suffixes and well-known
// subdirectory names) as well as #ifdef guards read from the first few lines.
func isWindowsOrMacOnly(relPath, repoRoot string) bool {
	lower := strings.ToLower(relPath)

	// Filename suffix conventions: _windows.go, _darwin.go, _windows_amd64.go.
	base := strings.ToLower(filepath.Base(lower))
	noExt := strings.TrimSuffix(base, filepath.Ext(base))
	parts := strings.Split(noExt, "_")
	for _, p := range parts[1:] {
		switch p {
		case "windows", "win32", "darwin", "macos", "osx":
			return true
		}
	}

	// Well-known directory-level platform segregation.
	for _, dir := range []string{"/win32/", "/windows/", "/darwin/", "/macos/", "/osx/"} {
		if strings.Contains(lower, dir) {
			return true
		}
	}

	// Inspect the first few KB for a top-level #ifdef guard.
	absPath := filepath.Join(repoRoot, relPath)
	return hasWindowsMacTopGuard(absPath)
}

// hasWindowsMacTopGuard checks whether the file starts with a platform guard
// (#ifdef _WIN32 / #ifdef __APPLE__) that wraps the whole file.
func hasWindowsMacTopGuard(absPath string) bool {
	ext := strings.ToLower(filepath.Ext(absPath))
	switch ext {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".hh", ".hxx":
	default:
		return false
	}
	raw, err := readFilePrefix(absPath, 4096)
	if err != nil {
		return false
	}
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		lowerLine := strings.ToLower(trimmed)
		if strings.Contains(lowerLine, "_win32") || strings.Contains(lowerLine, "__apple__") ||
			strings.Contains(lowerLine, "macos") || strings.Contains(lowerLine, "darwin") {
			if strings.HasPrefix(trimmed, "#ifdef") || strings.HasPrefix(trimmed, "#if ") || strings.HasPrefix(trimmed, "#if\t") {
				return true
			}
		}
		break
	}
	return false
}

// readFilePrefix reads at most maxBytes from a file.
func readFilePrefix(path string, maxBytes int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxBytes)
	n, _ := f.Read(buf)
	return buf[:n], nil
}

// isTestFile reports whether the file is a test file by name convention.
func isTestFile(relPath string) bool {
	base := filepath.Base(relPath)
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, "_test.go") {
		return true
	}
	if strings.HasPrefix(lower, "test_") || strings.HasSuffix(lower, "_test.cpp") ||
		strings.HasSuffix(lower, "_test.cc") || strings.HasSuffix(lower, "_test.py") ||
		strings.HasSuffix(lower, ".test.ts") || strings.HasSuffix(lower, ".test.js") ||
		strings.HasSuffix(lower, ".spec.ts") || strings.HasSuffix(lower, ".spec.js") {
		return true
	}
	dir := strings.ToLower(filepath.Dir(relPath))
	for _, seg := range strings.Split(dir, string(filepath.Separator)) {
		if seg == "test" || seg == "tests" || seg == "testdata" || seg == "__tests__" {
			return true
		}
	}
	return false
}

// countNonTestCallers counts how many of the reference locations are in
// non-test files.
func countNonTestCallers(refs treesitter.Result, repoRoot string) int {
	n := 0
	for _, loc := range refs.Locations {
		path := strings.TrimPrefix(string(loc.URI), "file://")
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			rel = path
		}
		if !isTestFile(rel) {
			n++
		}
	}
	return n
}

// ---- LLM adjudication schema ----

// impactAdjEntry is the per-finding input tuple sent to the LLM.
type impactAdjEntry struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	File        string `json:"file"`
	Severity    string `json:"severity"`
	CallerFacts string `json:"caller_facts"`
}

// impactAdjResult is one per-finding output from the LLM.
type impactAdjResult struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Rationale string `json:"rationale"`
}

// impactAdjResponse is the full LLM response envelope.
type impactAdjResponse struct {
	Results []impactAdjResult `json:"results"`
}

// impactAdjSchema is the JSON Schema for impactAdjResponse.
var impactAdjSchema = json.RawMessage(`{
  "type": "object",
  "required": ["results"],
  "additionalProperties": false,
  "properties": {
    "results": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "severity", "rationale"],
        "additionalProperties": false,
        "properties": {
          "id":        {"type": "string"},
          "severity":  {"type": "string", "enum": ["critical","high","medium","low"]},
          "rationale": {"type": "string", "minLength": 1}
        }
      }
    }
  }
}`)

const impactAdjSystemPrompt = `You are a security-focused code reviewer re-assessing finding severity based on reachability and impact.

For each finding in the input JSON array, decide whether the ORIGINAL severity is appropriate.
The caller_facts field describes what is known about callers and why this case is ambiguous.

Rules:
- If the code is in a header/exported symbol with no known callers, and appears to be dead API surface, downrank to "low".
- If the code is called from startup sequences, request handlers, or other externally-controlled input paths, keep or raise severity.
- Be CONSERVATIVE: only downrank when reachability evidence is clear; when uncertain, preserve the original severity.
- Return ONLY valid JSON matching the schema; no prose.`

// adjudicateImpact sends a single batched LLM call for ambiguous findings and
// returns per-finding severity + rationale. At most one LLM call is made
// regardless of input size. sink (the funnel's progress sink, possibly nil)
// brackets the call as a "severity" agent so the re-assessment surfaces in
// `bugbot status` and the live pane via the shared AgentScope seam.
func adjudicateImpact(
	ctx context.Context,
	client llm.Client,
	ambiguous []ambiguousEntry,
	sink progress.EventSink,
) ([]adjResult, error) {
	entries := make([]impactAdjEntry, 0, len(ambiguous))
	idxByID := make(map[string]int, len(ambiguous))
	for _, a := range ambiguous {
		e := impactAdjEntry{
			ID:          a.fi.ID,
			Title:       a.fi.Title,
			File:        a.fi.File,
			Severity:    string(a.fi.Severity),
			CallerFacts: a.callerFacts, // set per-case by classifyReachability
		}
		entries = append(entries, e)
		idxByID[a.fi.ID] = a.idx
	}

	inputJSON, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("impact_sweep: marshal input: %w", err)
	}

	task := fmt.Sprintf("Re-assess the severity of these %d findings by reachability and impact.\n\nFindings:\n%s",
		len(entries), string(inputJSON))

	// One tool-less batched completion; the started/finished bracket is the
	// observability (no per-turn tool-call activity to report).
	label := fmt.Sprintf("%d findings", len(entries))
	progress.NewAgentScope(sink, progress.RoleSeverity, label).Start()
	runner := agent.NewRunner(client, nil, impactAdjSystemPrompt)
	var resp impactAdjResponse
	start := time.Now()
	outcome, rerr := runner.RunJSON(ctx, task, impactAdjSchema, &resp)
	emitAgentFinished(sink, progress.RoleSeverity, label, outcome, start, rerr)
	if rerr != nil {
		return nil, fmt.Errorf("impact_sweep: LLM adjudication: %w", rerr)
	}

	results := make([]adjResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		idx, ok := idxByID[r.ID]
		if !ok {
			continue
		}
		results = append(results, adjResult{idx: idx, severity: r.Severity, rationale: r.Rationale})
	}
	return results, nil
}

// validateSeverityInline classifies one survivor's reachability/impact at
// persist time using the same deterministic ladder (and optional single-finding
// LLM adjudication) as impactSweep. Returns (severity, rationale, swept):
// swept=true means a verdict was applied (deterministic OR adjudicated) and
// the caller should stamp SweptAt=now on the persisted finding; swept=false
// means defer to the bulk SweepDrain (leave SweptAt zero / NULL).
//
// This is the inline counterpart of impactSweep's bulk pass: it runs on the
// verify-and-persist path so a T2 survivor is stored with its validated
// severity and a swept_at stamp, instead of carrying the raw finder severity
// until the post-scan drainToFixpoint pass. SweepDrain then only needs to
// reconcile stranded/interrupted findings.
func (f *Funnel) validateSeverityInline(
	ctx context.Context,
	c Candidate,
	verifier llm.Client,
	budget *budgetState,
	result *Result,
) (domain.Severity, string, bool) {
	repoRoot := f.repo.Root()
	fi := domain.Finding{
		File:     c.File,
		Line:     c.Line,
		Severity: c.Severity,
		Lens:     c.Lens,
	}
	ts := treesitter.New(repoRoot)
	r := classifyReachability(&fi, repoRoot, ts)
	switch r.class {
	case reachKnownDownrank, reachKnownKeep:
		return r.severity, r.rationale, true
	case reachAmbiguous:
		if verifier != nil && budget != nil && !budget.stopped.Load() {
			llmResults, err := adjudicateImpact(ctx, verifier,
				[]ambiguousEntry{{idx: 0, fi: fi, callerFacts: r.callerFacts}},
				f.opts.Progress)
			if err != nil {
				f.note(result, fmt.Sprintf("impact_sweep: inline LLM adjudication failed: %v", err))
				return c.Severity, "", false
			}
			if len(llmResults) == 0 {
				return c.Severity, "", false
			}
			adj := llmResults[0]
			sev, ok := domain.ParseSeverity(adj.severity)
			if !ok {
				sev = c.Severity // keep original on bad parse
			}
			rationale := adj.rationale
			if rationale == "" {
				rationale = fmt.Sprintf("LLM impact adjudication: %s", sev)
			}
			return sev, rationale, true
		}
		return c.Severity, "", false
	default:
		return c.Severity, "", false
	}
}
