// Package analyzer seeds the leads blackboard with hits from deterministic
// static analyzers run inside the existing sandbox.
//
// Analyzers are recall aids: they surface plausible targets for the finder
// agents that follow, never conclusions. Every failure mode degrades to a
// skip-with-note so the funnel always proceeds.
//
// # Design
//
// The registry mirrors internal/sandbox/deps.go's data-driven style: a table of
// analyzerSpec rows, each with a detect func, a command, a rule→lens mapping,
// and a per-analyzer timeout. Seed iterates the table, runs detected analyzers
// via the provided Sandbox, parses their SARIF stdout output, and posts each hit
// to the leads table via store.AddLead. The PosterLens field is set to
// "analyzer:<name>" so the funnel can attribute leads back to their source.
//
// # Failure semantics
//
// Binary absent (exit 125/126/127 or stderr "command not found"/"not found") →
// skip, note. Nonzero exit WITH parseable SARIF is NORMAL (analyzers exit
// nonzero when they find things). Nonzero exit WITHOUT parseable SARIF → skip,
// note. Timeout → skip, note. Store errors → propagated (infrastructure
// failure). All other analyzer problems → skip, note.
//
// # Rule → lens mapping
//
// Each analyzerSpec carries a ruleLens func that maps a ruleID to a lens name or
// "" to skip. Lens names are the exact Lens.Name strings from
// internal/funnel/lens.go; see lensForRule for the per-analyzer prefix tables.
// Style-only rules (staticcheck S1*/ST1*, ruff E*/W*) are skipped to keep the
// leads table signal-dense.
//
// # SARIF ingestion
//
// Only the fields bugbot uses are parsed: runs[].results[].ruleId,
// message.text, locations[0].physicalLocation.artifactLocation.uri, and
// region.startLine. Absent fields are tolerated (result skipped, counted).
// A per-analyzer cap (maxResultsPerAnalyzer) bounds parsed results so a
// pathological run cannot flood the leads table.
package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// maxResultsPerAnalyzer caps the number of SARIF results we parse and post per
// analyzer run. Analyzers can emit thousands of hits on large repos; without a
// cap a single pathological run could flood the leads table with noise and slow
// subsequent funnel runs. 200 is conservative: it still gives the funnel
// meaningful seed density while keeping the leads table bounded. If an analyzer
// consistently hits the cap on a healthy repo the cap can be raised; raising
// it is always safe (more leads, no breaking changes).
const maxResultsPerAnalyzer = 200

// Lens name constants from internal/funnel/lens.go. Defined here to document
// the coupling: these strings are stable across runs (they are part of the
// fingerprint and dedup key). If lens names ever change in lens.go these must
// be updated in lock-step. The tests exercise the mapping so a mismatch is
// caught immediately.
const (
	lensNilSafety   = "nil-safety/error-handling"
	lensConcurrency = "concurrency"
	lensResources   = "resource-leaks"
	lensBoundary    = "boundary-conditions"
	lensAPIContract = "api-contract-misuse"
	lensInjection   = "injection/input-validation"
)

// analyzerSpec describes one static analyzer in the registry.
//
// The registry mirrors internal/sandbox/deps.go's ecosystem table: data-driven,
// ordered, with identical comment density and field naming conventions.
type analyzerSpec struct {
	// name is the stable analyzer identifier. It is used as the source suffix
	// in the PosterLens field ("analyzer:<name>") and in skip notes.
	name string
	// detect reports whether the analyzer is applicable to repoDir. Fast and
	// side-effect-free. False means the analyzer is skipped entirely.
	detect func(repoDir string) bool
	// cmd is the full argv to execute inside the sandbox (working directory is
	// the repo root). The command must write SARIF to stdout.
	cmd []string
	// ruleLens maps a SARIF ruleId to a lens name, or returns "" to skip the
	// result. Called once per parsed result. Must be safe for concurrent use
	// (it is a pure function in all current implementations).
	ruleLens func(ruleID string) string
	// timeout bounds the sandbox execution. 0 falls back to defaultAnalyzerTimeout.
	timeout time.Duration
}

// defaultAnalyzerTimeout is the fallback execution bound for analyzers that
// do not set their own timeout. Five minutes is generous for most repo sizes
// while keeping the seed step from blocking an interactive scan indefinitely.
const defaultAnalyzerTimeout = 5 * time.Minute

// registry is the ordered table of analyzers. Seed iterates it and runs every
// entry whose detect returns true for repoDir. To add a new analyzer, append a
// new analyzerSpec row here; the iteration semantics in Seed handle the rest.
//
// Current v1 entries:
//   - staticcheck: Go linter emitting SARIF via `-f sarif ./...`. Chosen over
//     go vet because go vet cannot emit SARIF directly. Detected by go.mod.
//   - ruff: Python linter emitting SARIF via `--output-format=sarif`. Detected
//     by requirements.txt or pyproject.toml.
//   - gosec: Go security linter emitting SARIF via `-fmt=sarif`. Detected by
//     go.mod. Covers injection, weak crypto, unsafe permissions, and bounds.
var registry = []analyzerSpec{
	staticcheckSpec,
	ruffSpec,
	gosecSpec,
}

// staticcheckSpec is the staticcheck Go analyzer entry.
//
// staticcheck is run as `staticcheck -f sarif ./...` from the repo root.
// It writes SARIF to stdout regardless of exit code; a nonzero exit means
// it found issues (which is normal and expected).
//
// Rule prefix → lens mapping (staticcheck rule families):
//
//	SA1* (incorrect API use), SA4* (unnecessary code), SA5* (correctness) →
//	  correctness / api-contract-misuse (SA1*) or nil-safety/error-handling (SA5* nil/err)
//	SA2* (concurrency) → concurrency
//	SA9* (dubious code) → correctness → nil-safety/error-handling
//	S1*, ST1* → skip (style; noise, not leads)
//	default → nil-safety/error-handling (the most common staticcheck category)
var staticcheckSpec = analyzerSpec{
	name:     "staticcheck",
	detect:   hasGoModule,
	cmd:      []string{"staticcheck", "-f", "sarif", "./..."},
	ruleLens: staticcheckRuleLens,
	timeout:  defaultAnalyzerTimeout,
}

// staticcheckRuleLens maps a staticcheck rule ID to a lens name.
// See: https://staticcheck.dev/docs/checks for the full rule taxonomy.
func staticcheckRuleLens(ruleID string) string {
	switch {
	// SA2* — concurrency issues (channel misuse, mutex mistakes, etc.)
	case strings.HasPrefix(ruleID, "SA2"):
		return lensConcurrency

	// SA5* — correctness: nil dereferences, incorrect API use, etc.
	// These map to the nil-safety lens because SA5 covers the same failure
	// modes: unchecked errors, nil interfaces, invalid conversions.
	case strings.HasPrefix(ruleID, "SA5"):
		return lensNilSafety

	// SA1* — incorrect standard library / API use → api-contract-misuse
	case strings.HasPrefix(ruleID, "SA1"):
		return lensAPIContract

	// SA4* — unnecessary / unreachable code → correctness, nil-safety proxy
	case strings.HasPrefix(ruleID, "SA4"):
		return lensNilSafety

	// SA9* — dubious constructs that may indicate deeper bugs → nil-safety
	case strings.HasPrefix(ruleID, "SA9"):
		return lensNilSafety

	// SA3* — test-related issues → api-contract-misuse (test API misuse)
	case strings.HasPrefix(ruleID, "SA3"):
		return lensAPIContract

	// SA6* — performance issues; not a high-value lead for bug-finding lenses
	case strings.HasPrefix(ruleID, "SA6"):
		return lensAPIContract

	// S1*, ST1* — code simplification / style. These are pure style signals
	// and are explicitly excluded: they generate noise in the leads table
	// without pointing at real defects.
	case strings.HasPrefix(ruleID, "S1"), strings.HasPrefix(ruleID, "ST1"):
		return "" // skip: style only

	// QF* — quickfix suggestions, also style-adjacent → skip
	case strings.HasPrefix(ruleID, "QF"):
		return "" // skip: style / refactor only

	// Default: unmapped staticcheck rule → treat as correctness
	default:
		return lensNilSafety
	}
}

// ruffSpec is the ruff Python linter entry.
//
// ruff is run as `ruff check --output-format=sarif .` from the repo root.
// Like staticcheck, it writes SARIF to stdout and exits nonzero when it finds
// issues; a nonzero exit with parseable SARIF is normal.
//
// Rule family → lens mapping (ruff rule families):
//
//	B* (flake8-bugbear) → correctness; these are real potential-bug rules
//	  (e.g. B006 mutable default, B008 function call in default, B023
//	  loop variable capture). Map to the most semantically-matching lens.
//	E1*/W1* (indentation) → skip (style)
//	E2*/W2* (whitespace)  → skip (style)
//	E3*/W3* (blank lines) → skip (style)
//	E4*/W4* (imports)     → skip (style)
//	E5*/W5* (line length) → skip (style)
//	E7*     (statements)  → skip (style)
//	E9*     (runtime errors) → nil-safety/error-handling
//	F8*     (undefined names, unused vars) → nil-safety (F821 undefined name is a real bug)
//	F4*     (import issues) → api-contract-misuse
//	S* (bandit security)  → injection/input-validation
//	default               → nil-safety/error-handling
var ruffSpec = analyzerSpec{
	name:     "ruff",
	detect:   hasPythonProject,
	cmd:      []string{"ruff", "check", "--output-format=sarif", "."},
	ruleLens: ruffRuleLens,
	timeout:  defaultAnalyzerTimeout,
}

// ruffRuleLens maps a ruff rule ID to a lens name.
// See: https://docs.astral.sh/ruff/rules/ for the full rule taxonomy.
func ruffRuleLens(ruleID string) string {
	switch {
	// E1*/E2*/E3*/E4*/E5*/E7* and W* — pycodestyle style rules. These are
	// pure formatting/style signals; excluded to keep leads signal-dense.
	case strings.HasPrefix(ruleID, "E1"),
		strings.HasPrefix(ruleID, "E2"),
		strings.HasPrefix(ruleID, "E3"),
		strings.HasPrefix(ruleID, "E4"),
		strings.HasPrefix(ruleID, "E5"),
		strings.HasPrefix(ruleID, "E7"),
		strings.HasPrefix(ruleID, "W"):
		return "" // skip: pycodestyle style only

	// E9* — runtime errors (SyntaxError, IOError, etc.) → real defects
	case strings.HasPrefix(ruleID, "E9"):
		return lensNilSafety

	// B* — flake8-bugbear: actual potential bugs, not just style.
	// Map by sub-family where semantics differ:
	//   B0xx (general) → nil-safety (e.g. B006 mutable defaults, B007 unused loop var)
	//   B023 (loop var capture) → concurrency (analogous to Go's loop capture)
	case ruleID == "B023":
		return lensConcurrency
	case strings.HasPrefix(ruleID, "B"):
		return lensNilSafety

	// F4* — import issues (undefined names from star imports etc.) →
	//   api-contract-misuse
	case strings.HasPrefix(ruleID, "F4"):
		return lensAPIContract

	// F8* — undefined names / unused variables. F821 (undefined name) is a
	//   real correctness bug; others are less critical but still correctness.
	case strings.HasPrefix(ruleID, "F8"):
		return lensNilSafety

	// S* — flake8-bandit security rules: injection, path traversal, exec, etc.
	case strings.HasPrefix(ruleID, "S"):
		return lensInjection

	// C9* — McCabe complexity: not a lead on its own → skip
	case strings.HasPrefix(ruleID, "C9"):
		return "" // skip: complexity metric only

	// I* (isort), D* (pydocstyle), N* (naming) — all style/convention → skip
	case strings.HasPrefix(ruleID, "I"),
		strings.HasPrefix(ruleID, "D"),
		strings.HasPrefix(ruleID, "N"):
		return "" // skip: style / convention

	// Default: unmapped rule → treat as correctness
	default:
		return lensNilSafety
	}
}

// gosecSpec is the gosec Go security analyzer entry.
//
// gosec is run as `gosec -fmt=sarif -quiet -no-fail ./...` from the repo root.
// -fmt=sarif:   emit SARIF to stdout.
// -quiet:       suppress the banner/progress lines that go to stderr.
// -no-fail:     always exit 0; without this, gosec exits nonzero when it finds
//
//	issues, which the analyzer framework treats as a potential error
//	only when SARIF cannot be parsed. -no-fail makes the exit code
//	unambiguous: nonzero always means execution failure, not findings.
//
// Rule group → lens mapping (gosec rule families):
//
//	G1xx (credential/audit), G2xx (injection: SQL/template/command/SSRF),
//	G4xx (weak crypto), G5xx (blocklisted imports) → injection/input-validation
//	G3xx (file perms / tempfile / path traversal):
//	  G301-G306 (file/dir perms) → resource-leaks (improper ACL = resource exposure)
//	  G304, G310 (path traversal, symlink) → injection/input-validation
//	  G307 (deferred close / tempfile cleanup) → resource-leaks
//	G6xx (memory safety):
//	  G601 (implicit memory aliasing in for-range) → boundary-conditions
//	  G602 (slice access out of bounds) → boundary-conditions
//	  G115 (integer overflow in conversion) → boundary-conditions
//	default → nil-safety/error-handling
var gosecSpec = analyzerSpec{
	name:     "gosec",
	detect:   hasGoModule,
	cmd:      []string{"gosec", "-fmt=sarif", "-quiet", "-no-fail", "./..."},
	ruleLens: gosecRuleLens,
	timeout:  defaultAnalyzerTimeout,
}

// gosecRuleLens maps a gosec rule ID to a lens name.
// See: https://github.com/securego/gosec#available-rules for the full taxonomy.
//
// Precision note: gosec G2xx/G4xx/G5xx rules are high-signal security rules
// that map directly to the injection lens. G1xx credential rules are also
// injection-class (hardcoded credentials feed injection paths). G3xx is split
// by sub-rule: path-traversal rules (G304, G310) are injection-class because
// the attacker-controlled path is the injection; permission bits (G301-G306,
// G307) are resource-class because they represent improper access control on
// resources. G6xx rules are boundary conditions (memory safety). The default
// maps to nil-safety to match the staticcheck convention.
func gosecRuleLens(ruleID string) string {
	switch {
	// G1xx — hardcoded credentials, insecure random, audit markers → injection
	// G101: hardcoded credentials; G102: network binding; G106: SSH InsecureIgnoreHostKey;
	// G107: URL from variable (SSRF precursor); G108-G115: various audit findings.
	case strings.HasPrefix(ruleID, "G1"):
		return lensInjection

	// G2xx — injection rules: SQL injection (G201/G202), template injection
	// (G203), command injection (G204), SSRF (G107 is G1 actually; G2 covers
	// the injection sinks). All are injection/input-validation.
	case strings.HasPrefix(ruleID, "G2"):
		return lensInjection

	// G3xx — file system / permission rules. Split by sub-rule:
	//   G304 (file path from variable = path traversal) → injection
	//   G310 (symlink follow = path traversal) → injection
	//   G301-G303, G305-G309 (permission bits, tempfile, deferred close) → resources
	case ruleID == "G304", ruleID == "G310":
		return lensInjection
	case strings.HasPrefix(ruleID, "G3"):
		return lensResources

	// G4xx — weak cryptographic primitives (weak rand, MD5/SHA1, DES, RC4,
	// weak RSA, ECB mode). These feed injection-class exploitability.
	case strings.HasPrefix(ruleID, "G4"):
		return lensInjection

	// G5xx — blocklisted imports (unsafe, CGO, net/http/cgi, etc.) → injection
	case strings.HasPrefix(ruleID, "G5"):
		return lensInjection

	// G6xx — memory safety:
	//   G601: implicit memory aliasing (loop variable address) → boundary
	//   G602: slice access out of bounds → boundary
	// Other G6xx rules default to boundary as well since they are all
	// memory-safety / integer-safety concerns.
	case strings.HasPrefix(ruleID, "G6"):
		return lensBoundary

	// Default: unknown gosec rule → treat as correctness (nil-safety proxy).
	default:
		return lensNilSafety
	}
}

// hasGoModule reports whether repoDir contains a root go.mod, indicating a Go
// project. Identical to the check in internal/sandbox/deps.go; reproduced here
// to keep the analyzer package self-contained.
func hasGoModule(repoDir string) bool {
	st, err := os.Stat(filepath.Join(repoDir, "go.mod"))
	return err == nil && !st.IsDir()
}

// hasPythonProject reports whether repoDir contains a Python project marker:
// requirements.txt or pyproject.toml. Either is sufficient to warrant running
// ruff; the former is the common pip-managed case, the latter the modern
// packaging case (poetry, flit, hatch, uv, etc.).
func hasPythonProject(repoDir string) bool {
	for _, name := range []string{"requirements.txt", "pyproject.toml"} {
		st, err := os.Stat(filepath.Join(repoDir, name))
		if err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// AnalyzerSummary is the per-analyzer outcome within a Seed run.
type AnalyzerSummary struct {
	// Name is the analyzer identifier (e.g. "staticcheck", "ruff").
	Name string
	// Ran reports whether the analyzer was detected and executed (as opposed
	// to skipped because detect returned false or the binary was absent).
	Ran bool
	// Hits is the number of SARIF results successfully parsed (before capping).
	Hits int
	// Posted is the number of leads posted to the store. Posted <= Hits (some
	// results are skipped by the rule→lens mapping or cap enforcement).
	Posted int
	// SkippedReason is non-empty when the analyzer was not run or its output
	// was not usable, and contains the human-readable reason.
	SkippedReason string
	// Duration is the wall-clock time of the sandbox execution.
	Duration time.Duration
}

// Summary is the aggregate outcome of a Seed call: one AnalyzerSummary per
// registry entry, plus a total lead count for convenience.
type Summary struct {
	// Analyzers holds one entry per registry row, in registry order.
	Analyzers []AnalyzerSummary
	// TotalPosted is the sum of Posted across all analyzers.
	TotalPosted int
}

// Seed runs every applicable static analyzer from the registry against repoDir
// inside the provided sandbox, parses their SARIF output, and posts hits to the
// leads table in st.
//
// The image parameter is passed verbatim to the sandbox (use the same image as
// the main analysis run so the container environment is consistent).
//
// Seed returns (Summary, nil) on success. Infrastructure failures (store
// writes) are returned as errors; analyzer failures (binary absent, nonzero
// exit, timeout, unparseable SARIF) are captured as SkippedReason in the
// per-analyzer Summary and never returned as errors. The Summary is always
// populated, even on error.
func Seed(ctx context.Context, sb sandbox.Sandbox, repoDir string, st *store.Store, image string) (Summary, error) {
	var sum Summary
	sum.Analyzers = make([]AnalyzerSummary, 0, len(registry))

	for _, spec := range registry {
		asum := runAnalyzer(ctx, spec, sb, repoDir, image)
		if asum.Ran && asum.SkippedReason == "" {
			// Post leads for the parsed results.
			posted, err := postLeads(ctx, asum.results, spec.name, st)
			if err != nil {
				// Infrastructure failure: stop seeding, return error.
				asum.Posted = posted
				sum.TotalPosted += posted
				sum.Analyzers = append(sum.Analyzers, asum.AnalyzerSummary)
				return sum, fmt.Errorf("analyzer: post leads for %s: %w", spec.name, err)
			}
			asum.Posted = posted
			sum.TotalPosted += posted
		}
		sum.Analyzers = append(sum.Analyzers, asum.AnalyzerSummary)
	}

	return sum, nil
}

// analyzerRun is an internal extension of AnalyzerSummary that also carries
// the parsed results before posting (so Seed can separate parsing from posting
// without passing multiple slices around).
type analyzerRun struct {
	AnalyzerSummary
	results []parsedResult
}

// parsedResult holds the minimal fields extracted from one SARIF result entry.
type parsedResult struct {
	ruleID  string
	message string
	file    string // repo-relative path
	line    int
}

// runAnalyzer executes one analyzer spec and parses its SARIF output. It never
// returns a Go error; failures are captured in the returned analyzerRun.
func runAnalyzer(ctx context.Context, spec analyzerSpec, sb sandbox.Sandbox, repoDir, image string) analyzerRun {
	// Skip if the ecosystem is not present in repoDir.
	if !spec.detect(repoDir) {
		return analyzerRun{
			AnalyzerSummary: AnalyzerSummary{
				Name:          spec.name,
				SkippedReason: "not applicable (project type not detected)",
			},
		}
	}

	timeout := spec.timeout
	if timeout <= 0 {
		timeout = defaultAnalyzerTimeout
	}

	spec2 := sandbox.Spec{
		RepoDir: repoDir,
		Cmd:     spec.cmd,
		Network: "none",
		Timeout: timeout,
		Image:   image,
	}

	start := time.Now()
	res, err := sb.Exec(ctx, spec2)
	dur := time.Since(start)

	arun := analyzerRun{
		AnalyzerSummary: AnalyzerSummary{
			Name:     spec.name,
			Ran:      true,
			Duration: dur,
		},
	}

	// Infrastructure failure: sandbox could not launch at all.
	if err != nil {
		arun.SkippedReason = fmt.Sprintf("sandbox exec error: %s", err)
		return arun
	}

	// Timeout: treat as skip.
	if res.TimedOut {
		arun.SkippedReason = fmt.Sprintf("analyzer timed out after %s", timeout)
		return arun
	}

	// Binary absent: exit 125/126/127 are reserved by the POSIX shell for
	// "command not found", "permission denied (not executable)", and "command
	// found but exec failed". We also check stderr for common "not found" text.
	if isBinaryAbsent(res) {
		arun.SkippedReason = fmt.Sprintf("%s binary not found in container image", spec.name)
		arun.Ran = false // binary absent means effectively not run
		return arun
	}

	// Nonzero exit is NORMAL for analyzers that found issues. Attempt to parse
	// SARIF from stdout regardless of exit code. Only skip if stdout has no
	// parseable SARIF AND exit was nonzero (genuine tool failure, not findings).
	results, parseErr := parseSARIF(res.Stdout, spec.ruleLens)
	if parseErr != nil && res.ExitCode != 0 {
		arun.SkippedReason = fmt.Sprintf("exit code %d, SARIF parse failed: %s", res.ExitCode, parseErr)
		return arun
	}
	if parseErr != nil {
		// Nonzero exit but we have no SARIF; may be a config error.
		arun.SkippedReason = fmt.Sprintf("SARIF parse failed: %s", parseErr)
		return arun
	}

	arun.Hits = len(results)
	arun.results = results
	return arun
}

// isBinaryAbsent reports whether a sandbox result indicates that the analyzer
// binary was not found in the container image. This covers three cases:
//   - Exit 125: POSIX "command not found" via /bin/sh -c
//   - Exit 126: permission denied (not executable)
//   - Exit 127: command not found (bash/sh standard)
//   - stderr contains "command not found" or "not found" — some images use a
//     minimal sh whose exit codes differ.
func isBinaryAbsent(res sandbox.Result) bool {
	if res.ExitCode == 125 || res.ExitCode == 126 || res.ExitCode == 127 {
		return true
	}
	lower := strings.ToLower(res.Stderr)
	return strings.Contains(lower, "command not found") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no such file or directory")
}

// postLeads writes each parsed result to the leads table, skipping results
// whose rule→lens mapping returns "". It returns the number of leads
// successfully posted and the first store error encountered.
func postLeads(ctx context.Context, results []parsedResult, analyzerName string, st *store.Store) (int, error) {
	poster := "analyzer:" + analyzerName
	posted := 0
	for _, r := range results {
		if r.file == "" || r.line <= 0 {
			continue // cannot address without a location
		}
		if err := st.AddLead(ctx, store.Lead{
			PosterLens: poster,
			TargetLens: r.ruleID, // set by parseSARIF to the lens name
			File:       r.file,
			Line:       r.line,
			Note:       r.message,
		}); err != nil {
			return posted, err
		}
		posted++
	}
	return posted, nil
}

// sarifMinimal is a minimal SARIF envelope for ingestion. We parse only the
// fields needed by Seed; the full SARIF 2.1.0 schema has many more. Using a
// separate minimal struct (rather than importing report.SARIFLog) keeps the
// package self-contained and the struct fields exactly scoped to what we need.
// The report package's types are the OUTPUT side; this is the INPUT side.
type sarifMinimal struct {
	Runs []sarifRunMin `json:"runs"`
}

type sarifRunMin struct {
	Results []sarifResultMin `json:"results"`
}

type sarifResultMin struct {
	RuleID    string             `json:"ruleId"`
	Message   sarifMsgMin        `json:"message"`
	Locations []sarifLocationMin `json:"locations"`
}

type sarifMsgMin struct {
	Text string `json:"text"`
}

type sarifLocationMin struct {
	PhysicalLocation sarifPhysMin `json:"physicalLocation"`
}

type sarifPhysMin struct {
	ArtifactLocation sarifArtMin  `json:"artifactLocation"`
	Region           *sarifRegMin `json:"region"`
}

type sarifArtMin struct {
	URI string `json:"uri"`
}

type sarifRegMin struct {
	StartLine int `json:"startLine"`
}

// parseSARIF parses SARIF JSON from stdout, applying the rule→lens mapping.
// It returns a (potentially empty) slice of parsedResults on success.
// Absent fields are tolerated (result skipped). Results are capped at
// maxResultsPerAnalyzer so a pathological run cannot flood the leads table.
//
// parsedResult.ruleID is set to the MAPPED LENS NAME (not the original SARIF
// ruleId), so postLeads can use it directly as the TargetLens field.
func parseSARIF(stdout string, ruleLens func(string) string) ([]parsedResult, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, fmt.Errorf("empty stdout")
	}

	var doc sarifMinimal
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		// Provide a clipped error context so skip notes are readable.
		preview := stdout
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("JSON decode: %w (stdout prefix: %s)", err, preview)
	}

	var out []parsedResult
	for _, run := range doc.Runs {
		for _, r := range run.Results {
			if len(out) >= maxResultsPerAnalyzer {
				break
			}
			// Rule→lens: skip unmapped rules entirely.
			lens := ruleLens(r.RuleID)
			if lens == "" {
				continue
			}
			// Extract location (first location entry only).
			var file string
			var line int
			if len(r.Locations) > 0 {
				loc := r.Locations[0].PhysicalLocation
				file = normalizeURI(loc.ArtifactLocation.URI)
				if loc.Region != nil {
					line = loc.Region.StartLine
				}
			}
			// Skip results without addressable location.
			if file == "" || line <= 0 {
				continue
			}
			out = append(out, parsedResult{
				ruleID:  lens, // store the lens name, not the raw ruleID
				message: r.Message.Text,
				file:    file,
				line:    line,
			})
		}
	}
	return out, nil
}

// containerWorkspacePrefix is the SARIF path prefix produced when an analyzer
// emits absolute container paths under the sandbox's /workspace mount. After
// stripping "file:///", a URI for "app.py" in the workspace becomes
// "workspace/app.py". We strip this prefix to produce a repo-relative path.
const containerWorkspacePrefix = "workspace/"

// normalizeURI strips common SARIF URI prefixes and normalizes the path to a
// clean repo-relative form. SARIF uris can be bare relative paths ("main.go"),
// "file:///abs/path" absolute URIs, or relative URIs with "./" prefixes
// ("./main.go"). We want repo-relative clean paths matching how the leads table
// and fingerprints address files (e.g. "internal/foo/bar.go").
//
// Analyzers running inside the sandbox emit file:/// URIs rooted at the
// container workspace (/workspace). After stripping the scheme, these become
// "workspace/foo.go". We strip the leading "workspace/" prefix so the resulting
// path matches the snapshot's repo-relative addressing.
func normalizeURI(uri string) string {
	// Strip "file://" or "file:///" scheme.
	if strings.HasPrefix(uri, "file:///") {
		uri = uri[len("file:///"):]
	} else if strings.HasPrefix(uri, "file://") {
		uri = uri[len("file://"):]
	}
	// Normalize forward slashes (SARIF always uses "/").
	uri = filepath.ToSlash(uri)
	// Strip the sandbox container workspace prefix so absolute in-container
	// paths become repo-relative (e.g. "workspace/pkg/foo.go" → "pkg/foo.go").
	uri = strings.TrimPrefix(uri, containerWorkspacePrefix)
	// Strip leading "./" so paths are uniformly relative.
	uri = strings.TrimPrefix(uri, "./")
	return uri
}
