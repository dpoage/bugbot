package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/llm"
)

// DefaultRecordedDir is the conventional on-disk location of the committed
// real-model transcript corpus, relative to this package.
const DefaultRecordedDir = "testdata/recorded"

// RecordedManifest is the per-case metadata written alongside a recorded
// corpus. It captures WHAT was recorded (model, host, timestamp), the LIVE-run
// scores the recording was measured at, and the session counts per role so a
// replay divergence (e.g. a RunJSON repair round-trip consuming an extra
// session) is debuggable. It deliberately never stores the API key.
type RecordedManifest struct {
	// Case is the builtin case name this corpus replays.
	Case string `json:"case"`
	// Model is the recorded model id (e.g. "MiniMax-M3").
	Model string `json:"model"`
	// BaseURLHost is the HOST of the recording endpoint — never the full URL
	// with credentials, and never the key.
	BaseURLHost string `json:"base_url_host"`
	// RecordedAt is the RFC3339 UTC time the corpus was captured.
	RecordedAt string `json:"recorded_at"`

	// Scores are the LIVE run's measured scores. They are a MEASUREMENT of a real
	// model, not an invariant; the replay determinism test asserts these reproduce
	// exactly, which is the actual regression guard.
	Scores RecordedScores `json:"scores"`

	// FinderSessions / VerifierSessions count the recorded sessions per role.
	// These exist so a replay that exhausts a store (e.g. because a RunJSON repair
	// round-trip consumed an extra session at record time but not at replay time)
	// can be diagnosed against the recorded shape.
	FinderSessions   int `json:"finder_sessions"`
	VerifierSessions int `json:"verifier_sessions"`
}

// RecordedScores is the manifest's snapshot of a case's scored outcome from the
// live recording run.
type RecordedScores struct {
	TruePositives  int          `json:"tp"`
	FalsePositives int          `json:"fp"`
	FalseNegatives int          `json:"fn"`
	Precision      float64      `json:"precision"`
	Recall         float64      `json:"recall"`
	Stats          funnel.Stats `json:"stats"`
}

// builtinCaseByName returns a copy of the builtin case with the given name and
// reports whether it exists. Used to clone a builtin's fixture + seeded set when
// attaching recorded transcripts.
func builtinCaseByName(name string) (Case, bool) {
	for _, c := range BuiltinCases() {
		if c.Name == name {
			return c, true
		}
	}
	return Case{}, false
}

// LoadRecordedCases scans dir for subdirectories matching builtin case names and
// builds a replayable Case for each: it clones the builtin case (fixture +
// seeded set), loads the per-role JSONL recordings in filename order, attaches
// them as a RecordedCase, and drops any scripted behavior so the case can ONLY
// run in recorded mode.
//
// A missing dir returns (nil, nil): the corpus is optional, so its absence is
// not an error. A present-but-malformed corpus (unparseable transcript, a subdir
// that is not a builtin case, a role with zero sessions) returns an error, so a
// broken corpus fails loudly rather than silently scoring an empty run.
func LoadRecordedCases(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // optional corpus
		}
		return nil, fmt.Errorf("eval: read recorded corpus %q: %w", dir, err)
	}

	var cases []Case
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		base, ok := builtinCaseByName(name)
		if !ok {
			return nil, fmt.Errorf("eval: recorded corpus has subdir %q that is not a builtin case name", name)
		}

		caseDir := filepath.Join(dir, name)
		rc, err := loadRecordedCase(caseDir)
		if err != nil {
			return nil, fmt.Errorf("eval: load recorded case %q: %w", name, err)
		}

		// Clone the builtin's fixture + ground truth; replace scripted behavior
		// with the recordings.
		c := base
		c.Scripted = nil
		c.Recorded = rc
		cases = append(cases, c)
	}
	return cases, nil
}

// loadRecordedCase reads one case directory's finder-*.jsonl and verifier-*.jsonl
// recordings (each set in filename order) into per-role RoleTranscriptStores.
func loadRecordedCase(caseDir string) (*RecordedCase, error) {
	finder, err := loadRoleStore(caseDir, "finder")
	if err != nil {
		return nil, err
	}
	verifier, err := loadRoleStore(caseDir, "verifier")
	if err != nil {
		return nil, err
	}
	return &RecordedCase{Finder: finder, Verifier: verifier}, nil
}

// loadRoleStore loads "<role>-*.jsonl" files from caseDir in filename order
// (which is record order — see the timestamped naming) into a
// RoleTranscriptStore. A case with no finder sessions is an error (the funnel
// always hypothesizes), but zero verifier sessions is legitimate: a run whose
// finders report nothing (e.g. the clean-code canary) never reaches the verify
// stage, so nothing was recorded for that role.
func loadRoleStore(caseDir, role string) (*RoleTranscriptStore, error) {
	pattern := filepath.Join(caseDir, role+"-*.jsonl")
	names, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", pattern, err)
	}
	// Glob returns lexically sorted paths; the recorder writes them with a
	// zero-padded ordinal (finder-000.jsonl, finder-001.jsonl, ...) so lexical
	// order is record order.
	if len(names) == 0 && role == "finder" {
		return nil, fmt.Errorf("recorded corpus has no %s sessions in %q", role, caseDir)
	}

	var sessions []*agent.Transcript
	for _, p := range names {
		tr, err := loadTranscriptFile(p)
		if err != nil {
			return nil, fmt.Errorf("load %s transcript %q: %w", role, filepath.Base(p), err)
		}
		sessions = append(sessions, tr)
	}
	// Capabilities default to the zero value: the funnel's finder/verifier agents
	// do not branch on capability flags, and replay matching is structure-based.
	return NewRoleTranscriptStore(role, llm.Capabilities{}, sessions...), nil
}

// writeManifest writes a case directory's manifest.json, pretty-printed for a
// readable, reviewable diff in the committed corpus.
func writeManifest(caseDir string, m RecordedManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: marshal manifest for %q: %w", m.Case, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(caseDir, "manifest.json"), data, 0o644); err != nil {
		return fmt.Errorf("eval: write manifest in %q: %w", caseDir, err)
	}
	return nil
}

// LoadManifest reads a case directory's manifest.json.
func LoadManifest(caseDir string) (*RecordedManifest, error) {
	data, err := os.ReadFile(filepath.Join(caseDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("eval: read manifest in %q: %w", caseDir, err)
	}
	var m RecordedManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("eval: parse manifest in %q: %w", caseDir, err)
	}
	return &m, nil
}
