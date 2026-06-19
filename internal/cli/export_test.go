package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExportSARIF_ToFile runs `bugbot export --format sarif --output FILE` and
// asserts that the written file is valid SARIF with the expected structure.
func TestExportSARIF_ToFile(t *testing.T) {
	cfgPath, _, f := setup(t)

	outFile := filepath.Join(t.TempDir(), "out.sarif.json")
	_, err := run(t, cfgPath, "export", "--format", "sarif", "--output", outFile)
	if err != nil {
		t.Fatalf("export command failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Top-level required SARIF fields.
	if v, _ := doc["version"].(string); v != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", v)
	}
	if s, _ := doc["$schema"].(string); s == "" {
		t.Error("$schema must not be empty")
	}

	runs, _ := doc["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(runs))
	}

	run0 := runs[0].(map[string]any)
	tool := run0["tool"].(map[string]any)
	driver := tool["driver"].(map[string]any)
	if name, _ := driver["name"].(string); name != "bugbot" {
		t.Errorf("driver.name = %q, want bugbot", name)
	}

	results, _ := run0["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (one seeded finding)", len(results))
	}

	r := results[0].(map[string]any)
	if ruleID, _ := r["ruleId"].(string); ruleID != f.Lens {
		t.Errorf("ruleId = %q, want %q", ruleID, f.Lens)
	}
	if level, _ := r["level"].(string); level != "warning" {
		// setup() seeds a Tier=2 finding -> warning
		t.Errorf("level = %q, want warning (Tier 2)", level)
	}
	msg := r["message"].(map[string]any)
	if txt, _ := msg["text"].(string); txt == "" {
		t.Error("message.text must not be empty")
	}

	locs, _ := r["locations"].([]any)
	if len(locs) != 1 {
		t.Fatalf("len(locations) = %d, want 1", len(locs))
	}
	pl := locs[0].(map[string]any)["physicalLocation"].(map[string]any)
	uri := pl["artifactLocation"].(map[string]any)["uri"].(string)
	if uri != f.File {
		t.Errorf("artifactLocation.uri = %q, want %q", uri, f.File)
	}
	startLine := pl["region"].(map[string]any)["startLine"].(float64)
	if int(startLine) != f.Line {
		t.Errorf("region.startLine = %d, want %d", int(startLine), f.Line)
	}

	fps, _ := r["partialFingerprints"].(map[string]any)
	if fp, _ := fps["bugbotFingerprint/v1"].(string); fp != f.Fingerprint {
		t.Errorf("partialFingerprints[bugbotFingerprint/v1] = %q, want %q", fp, f.Fingerprint)
	}
}

// TestExportSARIF_Stdout runs export without --output and checks stdout.
func TestExportSARIF_Stdout(t *testing.T) {
	cfgPath, _, _ := setup(t)

	out, err := run(t, cfgPath, "export", "--format", "sarif")
	if err != nil {
		t.Fatalf("export command failed: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\noutput: %s", err, out)
	}
	if v, _ := doc["version"].(string); v != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", v)
	}
}

// TestExportUnsupportedFormat checks that an unknown --format returns an error.
func TestExportUnsupportedFormat(t *testing.T) {
	cfgPath, _, _ := setup(t)

	_, err := run(t, cfgPath, "export", "--format", "junit")
	if err == nil {
		t.Error("expected error for unsupported format, got nil")
	}
}
