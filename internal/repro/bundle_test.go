package repro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestWriteArtifacts_ManifestRoundTrip is acceptance criterion 1 for
// bugbot-ecm8: writeArtifacts emits manifest.json alongside README.md/run.sh
// with the documented fields, and LoadBundle reads a written bundle back
// into an equivalent Bundle/Plan.
func TestWriteArtifacts_ManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	finding := domain.Finding{
		ID:          "finding-123",
		Fingerprint: "fp-abc",
		File:        "pkg/calc.go",
		Line:        42,
		CommitSHA:   "deadbeef",
	}
	plan := &Plan{
		Files: map[string]string{
			"bug_test.go": "package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n",
		},
		Cmd:    []string{"go", "test", "-run", "TestBug", "./..."},
		Expect: "TestBug fails",
	}
	res := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nFAIL"}

	bundleDir, err := writeArtifacts(dir, finding, plan, res, res, "docker.io/library/golang:1.21", "none")
	if err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	if bundleDir != filepath.Join(dir, finding.ID) {
		t.Fatalf("bundleDir = %q, want %q", bundleDir, filepath.Join(dir, finding.ID))
	}

	b, err := LoadBundle(bundleDir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}

	wantFinding := ManifestFinding{
		ID: "finding-123", Fingerprint: "fp-abc", File: "pkg/calc.go", Line: 42, CommitSHA: "deadbeef",
	}
	if b.Manifest.Finding != wantFinding {
		t.Errorf("Manifest.Finding = %+v, want %+v", b.Manifest.Finding, wantFinding)
	}
	if !reflect.DeepEqual(b.Manifest.Plan.Cmd, plan.Cmd) {
		t.Errorf("Manifest.Plan.Cmd = %v, want %v", b.Manifest.Plan.Cmd, plan.Cmd)
	}
	if !reflect.DeepEqual(b.Manifest.Plan.Files, []string{"bug_test.go"}) {
		t.Errorf("Manifest.Plan.Files = %v, want [bug_test.go]", b.Manifest.Plan.Files)
	}
	wantSandbox := ManifestSandbox{
		Image: "docker.io/library/golang:1.21", Ecosystem: sandbox.EcosystemGo, Network: "none",
	}
	if b.Manifest.Sandbox != wantSandbox {
		t.Errorf("Manifest.Sandbox = %+v, want %+v", b.Manifest.Sandbox, wantSandbox)
	}
	if b.Manifest.Result.ExitCode != 1 {
		t.Errorf("Manifest.Result.ExitCode = %d, want 1", b.Manifest.Result.ExitCode)
	}
	if b.Manifest.BugbotVersion == "" {
		t.Error("Manifest.BugbotVersion is empty")
	}

	gotPlan := b.Plan()
	if !reflect.DeepEqual(gotPlan.Files, plan.Files) {
		t.Errorf("round-tripped Plan.Files = %v, want %v", gotPlan.Files, plan.Files)
	}
	if !reflect.DeepEqual(gotPlan.Cmd, plan.Cmd) {
		t.Errorf("round-tripped Plan.Cmd = %v, want %v", gotPlan.Cmd, plan.Cmd)
	}
}

// TestWriteArtifacts_SentinelSeen asserts Result.SentinelSeen reflects the
// reproduction sentinel marker, independent of the ecosystem's own
// ran-evidence markers.
func TestWriteArtifacts_SentinelSeen(t *testing.T) {
	dir := t.TempDir()
	finding := domain.Finding{ID: "f1"}
	plan := &Plan{Cmd: []string{"python3", "repro.py"}, Files: map[string]string{"repro.py": "print('x')\n"}}
	res := sandbox.Result{ExitCode: 1, Stdout: reproSentinelDemonstrated}

	bundleDir, err := writeArtifacts(dir, finding, plan, res, res, "python:3.12", "none")
	if err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	b, err := LoadBundle(bundleDir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if !b.Manifest.Result.SentinelSeen {
		t.Error("Manifest.Result.SentinelSeen = false, want true")
	}
}

// TestWriteArtifacts_ReadmeRecordsBothRuns is bugbot-c49s's artifact
// acceptance: the determinism gate pays for a confirmation run before
// promoting, and the README must show a human BOTH outcomes, not just the
// official run — otherwise the second run's cost buys no visible trust
// signal in the bundle itself.
func TestWriteArtifacts_ReadmeRecordsBothRuns(t *testing.T) {
	dir := t.TempDir()
	finding := domain.Finding{ID: "f-readme", Title: "racy read", File: "pkg/race.go", Line: 7}
	plan := &Plan{
		Files:  map[string]string{"bug_test.go": "package bug\n\nimport \"testing\"\n\nfunc TestBug(t *testing.T){ t.Fatal(\"boom\") }\n"},
		Cmd:    []string{"go", "test", "-run", "TestBug", "./..."},
		Expect: "TestBug fails",
	}
	res := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nofficial run\nFAIL"}
	confirmRes := sandbox.Result{ExitCode: 1, Stdout: "--- FAIL: TestBug\nconfirmation run\nFAIL"}

	bundleDir, err := writeArtifacts(dir, finding, plan, res, confirmRes, "docker.io/library/golang:1.21", "none")
	if err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	readmeBytes, err := os.ReadFile(filepath.Join(bundleDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(readmeBytes)
	for _, want := range []string{"official run", "confirmation run", "Determinism"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README missing %q; got:\n%s", want, readme)
		}
	}
}

// TestLoadBundle_RejectsUnsafeFilePath ensures a manifest naming an
// escaping path is rejected rather than read as an arbitrary host file.
func TestLoadBundle_RejectsUnsafeFilePath(t *testing.T) {
	dir := t.TempDir()
	writeManifestFixture(t, dir, Manifest{
		Plan: ManifestPlan{Cmd: []string{"go", "test"}, Files: []string{"../../etc/passwd"}},
	})
	if _, err := LoadBundle(dir); err == nil {
		t.Fatal("LoadBundle: want error for unsafe manifest file path, got nil")
	}
}

// TestLoadBundle_MissingManifest ensures a directory with no manifest.json
// fails clearly instead of loading a zero-value Bundle.
func TestLoadBundle_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadBundle(dir); err == nil {
		t.Fatal("LoadBundle: want error for missing manifest.json, got nil")
	}
}

// writeManifestFixture writes a bare manifest.json (no README/run.sh) into
// dir, for tests exercising LoadBundle's own validation independent of
// writeArtifacts.
func writeManifestFixture(t *testing.T, dir string, m Manifest) {
	t.Helper()
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), raw, 0o644); err != nil {
		t.Fatalf("write manifest fixture: %v", err)
	}
}
