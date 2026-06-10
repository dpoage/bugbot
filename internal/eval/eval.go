// Package eval is Bugbot's offline evaluation harness: it measures the
// detection funnel's precision and recall against seeded-bug fixtures without
// spending a single API call.
//
// # Why this exists
//
// Bugbot's defining constraint is precision over recall (see ARCHITECTURE.md):
// it is better to surface three real bugs than ten probable ones. That
// constraint is only meaningful if it can be measured. This package is the
// ground truth: it runs the real [funnel] over fixture repositories whose bugs
// are known by construction, then scores the persisted findings against that
// ground truth — counting both whether seeded bugs were found (recall) AND
// whether anything was reported in clean code (the false-positive rate that
// precision turns on).
//
// The harness generalizes, by hand, exactly what internal/funnel's own tests
// do: it materializes a real git repo from an in-memory file map, injects
// deterministic finder/verifier behavior, runs Sweep, and asserts on the
// outcome. Where the funnel tests bake one scenario into Go test code, this
// package turns the scenario into data (a [Case]) so a whole benchmark suite
// can be scored uniformly and compared across prompt/pipeline changes.
//
// # Two offline modes
//
//   - [ModeScripted] drives the funnel with a [ScriptedClient] whose responses
//     are routed by request content (system prompt / task text). This exercises
//     the PIPELINE's discrimination machinery — triage, adversarial verify,
//     dedup, suppression, scoring — under fully controlled inputs. Because the
//     inputs are exact, scripted-mode regression assertions are exact, not
//     flaky thresholds.
//   - [ModeRecorded] replays captured [agent.Transcript] JSONL recordings
//     through a [RoleTranscriptStore], so a real model session can be recorded
//     once and then re-run deterministically against modified harness code with
//     no API calls. See the package README for the recording workflow.
//
// # The command
//
// Two equivalent entrypoints run the built-in suite in scripted mode and share
// a single gate ([Gate]):
//
//	bugbot eval                                          # the CLI command
//	go test ./internal/eval/ -run TestBenchmarkSuite -v  # the CI regression test
//
// Both run every built-in [Case] (see [BuiltinCases]) in scripted mode and fail
// (non-zero exit / red test) if any clean-code case reports a false positive, or
// if aggregate precision drops below 1.0. Because they call the same [Gate], the
// CLI and the test can never disagree on what "passing" means.
package eval

import (
	"context"
	"fmt"

	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/llm"
	"github.com/dpoage/bugbot/internal/store"
)

// Mode selects how the funnel's LLM clients are supplied for a run.
type Mode int

const (
	// ModeScripted drives the funnel with per-case ScriptedClients (content
	// routed). Fully deterministic; tests the pipeline's discrimination logic.
	ModeScripted Mode = iota
	// ModeRecorded replays per-case agent.Transcript recordings through a
	// RoleTranscriptStore. Deterministic given fixed recordings; tests the same
	// pipeline against captured real-model behavior.
	ModeRecorded
)

func (m Mode) String() string {
	switch m {
	case ModeScripted:
		return "scripted"
	case ModeRecorded:
		return "recorded"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// SeededBug is one bug known by construction to exist in a fixture, used as
// ground truth when scoring findings. An empty Seeded slice on a Case marks it
// as a clean-code case, where ANY finding is a false positive.
type SeededBug struct {
	// File is the repo-relative path the bug lives in.
	File string
	// Line is the approximate 1-based line of the defect. Matching tolerates
	// LineTolerance lines of drift, since finders (and humans) disagree on the
	// exact line of a multi-line defect.
	Line int
	// LineTolerance is the maximum absolute line distance for a match. Zero means
	// the finding must land on exactly Line. Use a small positive value (1–3) for
	// realistic tolerance.
	LineTolerance int
	// Kind is a human label for the defect class (e.g. "nil-deref"). It maps
	// loosely to a funnel lens and appears in per-lens breakdowns; it is NOT used
	// for match gating (only file+line are), so a finding from any lens can match
	// a seeded bug at the right location.
	Kind string
}

// Case is one evaluation scenario: a fixture repository plus the ground-truth
// bugs seeded into it and the scripted/recorded behavior that drives the funnel
// over it.
type Case struct {
	// Name identifies the case in reports and test output. Must be unique within
	// a suite.
	Name string
	// Repo describes the fixture repository's files (and optional on-disk base).
	Repo FixtureSpec
	// Seeded is the ground-truth bug set. Empty => clean-code case (FP canary).
	Seeded []SeededBug

	// Options tunes the funnel run for this case (lenses, refuters, budget). The
	// zero value is valid. Note: scripted/recorded determinism is most robust
	// with MaxParallel:1, which the harness sets by default when this leaves it
	// zero (see RunCase).
	Options funnel.Options

	// Scripted supplies the finder/verifier behavior for ModeScripted. Required
	// in scripted mode; ignored in recorded mode.
	Scripted *ScriptedCase
	// Recorded supplies the transcript recordings for ModeRecorded. Required in
	// recorded mode; ignored in scripted mode.
	Recorded *RecordedCase

	// Suppress, when non-empty, pre-suppresses these fingerprints in the store
	// before the run (the suppressed-finding scenario). Each entry is built with
	// store.Fingerprint(lens, file, line, title).
	Suppress []Suppression
}

// Suppression is a fingerprint to pre-load into the store's suppression memory
// before a case runs, modeling a maintainer-dismissed finding.
type Suppression struct {
	Lens  string
	File  string
	Line  int
	Title string
	// Reason is the human-readable dismissal note recorded with the suppression.
	Reason string
}

func (s Suppression) fingerprint() string {
	return store.Fingerprint(s.Lens, s.File, s.Line, s.Title)
}

// ScriptedCase carries the per-case scripted client behavior. The builder
// functions receive a fresh ScriptedClient and register routes on it.
type ScriptedCase struct {
	// Finder configures the finder client's routes (typically lens → candidate
	// JSON). If nil, the finder returns the empty-candidate list for every
	// request (a clean-code default).
	Finder func(*ScriptedClient)
	// Verifier configures the verifier client's routes (typically candidate
	// title → refuter verdict). If nil, the verifier never refutes anything (the
	// precision-conservative default: a missing route means "could not refute").
	Verifier func(*ScriptedClient)
}

// RecordedCase carries the per-case transcript recordings for replay. Finder
// and Verifier each hold an ordered list of recorded sessions (one per
// RunJSON-driven agent run for that role).
type RecordedCase struct {
	// Finder is the ordered finder-role recordings, one per finder agent run.
	Finder *RoleTranscriptStore
	// Verifier is the ordered verifier-role recordings, one per refuter run.
	Verifier *RoleTranscriptStore
}

// RunCase materializes the case's fixture, opens a fresh store, wires the
// mode-appropriate clients, runs a full Sweep, and scores the persisted
// findings against the case's seeded bugs. It returns a CaseResult; an error is
// returned only for harness failures (fixture/store/funnel wiring), never for a
// "bad" detection score — a missed bug or a false positive is data, not an
// error.
func RunCase(ctx context.Context, c Case) (*CaseResult, error) {
	clients, err := buildClients(c)
	if err != nil {
		return nil, fmt.Errorf("eval: build clients for %q: %w", c.Name, err)
	}
	return runWithClients(ctx, c, clients)
}

// runWithClients is the funnel-invocation core shared by RunCase (which builds
// scripted/recorded clients from the case) and the real-model recorder (which
// injects live RoleClients). It materializes the fixture, opens a fresh store,
// applies suppressions, runs the Sweep, and scores the result. Splitting this
// out is the small seam that lets the recorder drive the EXACT same pipeline
// path as RunCase with real clients, rather than duplicating the wiring.
//
// The caller may pre-set c.Options (e.g. TranscriptDir, FinderLimits); this
// function only forces serial execution when MaxParallel is left at zero, which
// is required for deterministic recording/replay ordering.
func runWithClients(ctx context.Context, c Case, clients funnel.RoleClients) (*CaseResult, error) {
	dir, err := materialize(c.Repo)
	if err != nil {
		return nil, fmt.Errorf("eval: materialize %q: %w", c.Name, err)
	}
	defer cleanup(dir)

	st, err := store.Open(ctx, fixtureDBPath(dir))
	if err != nil {
		return nil, fmt.Errorf("eval: open store for %q: %w", c.Name, err)
	}
	defer func() { _ = st.Close() }()

	for _, s := range c.Suppress {
		if err := st.AddSuppression(ctx, s.fingerprint(), s.Reason); err != nil {
			return nil, fmt.Errorf("eval: pre-suppress %q in %q: %w", s.Title, c.Name, err)
		}
	}

	repo, err := ingest.Open(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("eval: open repo for %q: %w", c.Name, err)
	}

	opts := c.Options
	// Determinism: parallel agent runs make response ordering nondeterministic,
	// which matters for recorded replay (sequence-matched) and for stable
	// scripted call counts. Default to serial unless the case opts out.
	if opts.MaxParallel == 0 {
		opts.MaxParallel = 1
	}

	f, err := funnel.New(clients, st, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("eval: construct funnel for %q: %w", c.Name, err)
	}

	res, err := f.Sweep(ctx)
	if err != nil {
		return nil, fmt.Errorf("eval: sweep %q: %w", c.Name, err)
	}

	return score(c, res), nil
}

// buildClients resolves the RoleClients for the case's mode.
func buildClients(c Case) (funnel.RoleClients, error) {
	switch {
	case c.Recorded != nil:
		if c.Recorded.Finder == nil || c.Recorded.Verifier == nil {
			return funnel.RoleClients{}, fmt.Errorf("recorded case %q missing a role store", c.Name)
		}
		return funnel.RoleClients{
			Finder:   newReplayRoleClient(c.Recorded.Finder),
			Verifier: newReplayRoleClient(c.Recorded.Verifier),
		}, nil
	default:
		finder := NewScriptedClient()
		verifier := NewScriptedClient()
		if c.Scripted != nil {
			if c.Scripted.Finder != nil {
				c.Scripted.Finder(finder)
			}
			if c.Scripted.Verifier != nil {
				c.Scripted.Verifier(verifier)
			}
		}
		return funnel.RoleClients{Finder: finder, Verifier: verifier}, nil
	}
}

var _ llm.Client = (*ScriptedClient)(nil)
