package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
	"github.com/dpoage/bugbot/internal/store"
)

// --------------------------------------------------------------------------
// SARIF parser table tests
// --------------------------------------------------------------------------

func TestParseSARIF_valid(t *testing.T) {
	sarif := `{
		"runs": [{
			"results": [
				{
					"ruleId": "SA2001",
					"message": {"text": "empty critical section"},
					"locations": [{
						"physicalLocation": {
							"artifactLocation": {"uri": "pkg/foo/bar.go"},
							"region": {"startLine": 42}
						}
					}]
				}
			]
		}]
	}`

	results, err := parseSARIF(sarif, staticcheckRuleLens)
	if err != nil {
		t.Fatalf("parseSARIF returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.ruleID != lensConcurrency {
		t.Errorf("ruleID (lens) = %q, want %q", r.ruleID, lensConcurrency)
	}
	if r.file != "pkg/foo/bar.go" {
		t.Errorf("file = %q, want pkg/foo/bar.go", r.file)
	}
	if r.line != 42 {
		t.Errorf("line = %d, want 42", r.line)
	}
	if r.message != "empty critical section" {
		t.Errorf("message = %q, want 'empty critical section'", r.message)
	}
}

func TestParseSARIF_missingFields(t *testing.T) {
	tests := []struct {
		name      string
		sarif     string
		wantCount int
	}{
		{
			name:      "missing region skips result",
			sarif:     `{"runs": [{"results": [{"ruleId":"SA2001","message":{"text":"x"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"foo.go"}}}]}]}]}`,
			wantCount: 0, // no region → no line → skip
		},
		{
			name:      "missing locations skips result",
			sarif:     `{"runs": [{"results": [{"ruleId":"SA2001","message":{"text":"x"}}]}]}`,
			wantCount: 0,
		},
		{
			name:      "empty uri skips result",
			sarif:     `{"runs": [{"results": [{"ruleId":"SA2001","message":{"text":"x"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":""},"region":{"startLine":1}}}]}]}]}`,
			wantCount: 0,
		},
		{
			name:      "zero startLine skips result",
			sarif:     `{"runs": [{"results": [{"ruleId":"SA2001","message":{"text":"x"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"foo.go"},"region":{"startLine":0}}}]}]}]}`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results, err := parseSARIF(tt.sarif, staticcheckRuleLens)
			if err != nil {
				t.Fatalf("parseSARIF returned error: %v", err)
			}
			if len(results) != tt.wantCount {
				t.Errorf("want %d results, got %d", tt.wantCount, len(results))
			}
		})
	}
}

func TestParseSARIF_emptyRuns(t *testing.T) {
	sarif := `{"runs": []}`
	results, err := parseSARIF(sarif, staticcheckRuleLens)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("want 0 results, got %d", len(results))
	}
}

func TestParseSARIF_emptyStdout(t *testing.T) {
	_, err := parseSARIF("", staticcheckRuleLens)
	if err == nil {
		t.Fatal("want error for empty stdout, got nil")
	}
}

func TestParseSARIF_garbage(t *testing.T) {
	_, err := parseSARIF("this is not json at all", staticcheckRuleLens)
	if err == nil {
		t.Fatal("want error for garbage input, got nil")
	}
}

func TestParseSARIF_cap(t *testing.T) {
	// Build a SARIF document with maxResultsPerAnalyzer+50 results.
	var sb strings.Builder
	sb.WriteString(`{"runs":[{"results":[`)
	total := maxResultsPerAnalyzer + 50
	for i := 0; i < total; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"ruleId":"SA2001","message":{"text":"x"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"foo.go"},"region":{"startLine":1}}}]}`)
	}
	sb.WriteString(`]}]}`)

	results, err := parseSARIF(sb.String(), staticcheckRuleLens)
	if err != nil {
		t.Fatalf("parseSARIF returned error: %v", err)
	}
	if len(results) != maxResultsPerAnalyzer {
		t.Errorf("want cap %d, got %d", maxResultsPerAnalyzer, len(results))
	}
}

func TestParseSARIF_styleSkipped(t *testing.T) {
	// S1001 (staticcheck simplification) should be skipped by the rule→lens map.
	sarif := `{"runs":[{"results":[{"ruleId":"S1001","message":{"text":"simplify"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"foo.go"},"region":{"startLine":10}}}]}]}]}`
	results, err := parseSARIF(sarif, staticcheckRuleLens)
	if err != nil {
		t.Fatalf("parseSARIF returned error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("style rule S1001 should be skipped, got %d result(s)", len(results))
	}
}

func TestParseSARIF_uriNormalization(t *testing.T) {
	tests := []struct {
		uri      string
		wantFile string
	}{
		{"./pkg/foo.go", "pkg/foo.go"},
		{"pkg/foo.go", "pkg/foo.go"},
		// Absolute container paths under /workspace are stripped to repo-relative.
		{"file:///workspace/pkg/foo.go", "pkg/foo.go"},
		// Non-workspace absolute paths are normalized but not further stripped.
		{"file:///other/path/foo.go", "other/path/foo.go"},
	}
	for _, tt := range tests {
		sarif := `{"runs":[{"results":[{"ruleId":"SA2001","message":{"text":"x"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"` + tt.uri + `"},"region":{"startLine":5}}}]}]}]}`
		results, err := parseSARIF(sarif, staticcheckRuleLens)
		if err != nil {
			t.Fatalf("uri=%q: parseSARIF error: %v", tt.uri, err)
		}
		if len(results) != 1 {
			t.Fatalf("uri=%q: want 1 result, got %d", tt.uri, len(results))
		}
		if results[0].file != tt.wantFile {
			t.Errorf("uri=%q: file = %q, want %q", tt.uri, results[0].file, tt.wantFile)
		}
	}
}

// --------------------------------------------------------------------------
// Rule → lens mapping tests
// --------------------------------------------------------------------------

func TestStaticcheckRuleLens(t *testing.T) {
	tests := []struct {
		ruleID   string
		wantLens string
	}{
		{"SA2001", lensConcurrency},
		{"SA2000", lensConcurrency},
		{"SA5000", lensNilSafety},
		{"SA5008", lensNilSafety},
		{"SA1000", lensAPIContract},
		{"SA1016", lensAPIContract},
		{"SA4000", lensNilSafety},
		{"SA9003", lensNilSafety},
		{"S1001", ""},                // style: skip
		{"S1039", ""},                // style: skip
		{"ST1000", ""},               // style: skip
		{"QF1001", ""},               // quickfix: skip
		{"SA3001", lensAPIContract},  // SA3* test API misuse
		{"SA6000", lensAPIContract},  // SA6* performance
		{"UNKNOWN99", lensNilSafety}, // default
	}
	for _, tt := range tests {
		got := staticcheckRuleLens(tt.ruleID)
		if got != tt.wantLens {
			t.Errorf("staticcheckRuleLens(%q) = %q, want %q", tt.ruleID, got, tt.wantLens)
		}
	}
}

func TestRuffRuleLens(t *testing.T) {
	tests := []struct {
		ruleID   string
		wantLens string
	}{
		// Style: skip
		{"E101", ""},
		{"E201", ""},
		{"E302", ""},
		{"E401", ""},
		{"E501", ""},
		{"E711", ""},
		{"W191", ""},
		{"W291", ""},
		// Runtime errors: correctness
		{"E902", lensNilSafety},
		{"E999", lensNilSafety},
		// B* bugbear: real bugs
		{"B006", lensNilSafety},
		{"B007", lensNilSafety},
		{"B023", lensConcurrency}, // loop variable capture
		{"B904", lensNilSafety},
		// F4*: import issues → api-contract-misuse
		{"F401", lensAPIContract},
		// F8*: undefined names → nil-safety
		{"F811", lensNilSafety},
		{"F821", lensNilSafety},
		// S* security → injection
		{"S101", lensInjection},
		{"S301", lensInjection},
		// Style: skip
		{"C901", ""},
		{"I001", ""},
		{"D100", ""},
		{"N801", ""},
		// Default
		{"RUF100", lensNilSafety},
	}
	for _, tt := range tests {
		got := ruffRuleLens(tt.ruleID)
		if got != tt.wantLens {
			t.Errorf("ruffRuleLens(%q) = %q, want %q", tt.ruleID, got, tt.wantLens)
		}
	}
}

// --------------------------------------------------------------------------
// Registry detect tests
// --------------------------------------------------------------------------

func TestRegistryDetect(t *testing.T) {
	t.Run("staticcheck detects go.mod", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
			t.Fatal(err)
		}
		if !staticcheckSpec.detect(dir) {
			t.Error("staticcheck.detect should return true for dir with go.mod")
		}
	})

	t.Run("staticcheck skips no go.mod", func(t *testing.T) {
		dir := t.TempDir()
		if staticcheckSpec.detect(dir) {
			t.Error("staticcheck.detect should return false for dir without go.mod")
		}
	})

	t.Run("ruff detects requirements.txt", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeFile(dir, "requirements.txt", "requests==2.28.0\n"); err != nil {
			t.Fatal(err)
		}
		if !ruffSpec.detect(dir) {
			t.Error("ruff.detect should return true for dir with requirements.txt")
		}
	})

	t.Run("ruff detects pyproject.toml", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeFile(dir, "pyproject.toml", "[build-system]\n"); err != nil {
			t.Fatal(err)
		}
		if !ruffSpec.detect(dir) {
			t.Error("ruff.detect should return true for dir with pyproject.toml")
		}
	})

	t.Run("ruff skips no python markers", func(t *testing.T) {
		dir := t.TempDir()
		if ruffSpec.detect(dir) {
			t.Error("ruff.detect should return false for dir without python markers")
		}
	})
}

// --------------------------------------------------------------------------
// Seed with mock sandbox — scripted SARIF stdout → leads posted correctly
// --------------------------------------------------------------------------

func TestSeed_staticcheckLeads(t *testing.T) {
	dir := t.TempDir()
	// Set up a Go project marker.
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	sarifOut := `{
		"runs": [{
			"results": [
				{
					"ruleId": "SA2001",
					"message": {"text": "empty critical section"},
					"locations": [{
						"physicalLocation": {
							"artifactLocation": {"uri": "pkg/race.go"},
							"region": {"startLine": 12}
						}
					}]
				},
				{
					"ruleId": "SA5001",
					"message": {"text": "unchecked error"},
					"locations": [{
						"physicalLocation": {
							"artifactLocation": {"uri": "main.go"},
							"region": {"startLine": 7}
						}
					}]
				}
			]
		}]
	}`

	// Mock: staticcheck returns exitCode=1 (found issues) with SARIF on stdout.
	// gosec also detects go.mod — enqueue a binary-absent response (exit 127)
	// for it so it skips cleanly and only staticcheck's 2 leads are counted.
	mock := sandbox.NewMock(sandbox.MockResponse{
		// Default: binary absent (gosec or any extra invocation).
		Result: sandbox.Result{ExitCode: 127, Stderr: "command not found"},
	})
	// First call is staticcheck (registry order: staticcheck, ruff, gosec).
	// ruff is skipped (no Python marker), so only staticcheck + gosec run.
	mock.EnqueueResponse(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 1, Stdout: sarifOut},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "test-image")
	if err != nil {
		t.Fatalf("Seed returned error: %v", err)
	}

	if sum.TotalPosted != 2 {
		t.Errorf("TotalPosted = %d, want 2", sum.TotalPosted)
	}

	// Verify the leads are in the store with correct fields.
	leads, err := st.PendingLeads(ctx, lensConcurrency)
	if err != nil {
		t.Fatalf("PendingLeads(concurrency): %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("want 1 concurrency lead, got %d", len(leads))
	}
	l := leads[0]
	if l.PosterLens != "analyzer:staticcheck" {
		t.Errorf("PosterLens = %q, want 'analyzer:staticcheck'", l.PosterLens)
	}
	if l.File != "pkg/race.go" {
		t.Errorf("File = %q, want 'pkg/race.go'", l.File)
	}
	if l.Line != 12 {
		t.Errorf("Line = %d, want 12", l.Line)
	}
	if l.TargetLens != lensConcurrency {
		t.Errorf("TargetLens = %q, want %q", l.TargetLens, lensConcurrency)
	}

	nilLeads, err := st.PendingLeads(ctx, lensNilSafety)
	if err != nil {
		t.Fatalf("PendingLeads(nil-safety): %v", err)
	}
	if len(nilLeads) != 1 {
		t.Fatalf("want 1 nil-safety lead, got %d", len(nilLeads))
	}
	if nilLeads[0].File != "main.go" || nilLeads[0].Line != 7 {
		t.Errorf("nil-safety lead = {%s %d}, want {main.go 7}", nilLeads[0].File, nilLeads[0].Line)
	}
}

func TestSeed_binaryAbsent_notAnError(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	// Mock: exit 127 = "command not found"
	mock := sandbox.NewMock(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 127, Stderr: "staticcheck: command not found"},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "test-image")
	if err != nil {
		t.Fatalf("binary absent must not cause Seed to error: %v", err)
	}
	if sum.TotalPosted != 0 {
		t.Errorf("TotalPosted = %d, want 0", sum.TotalPosted)
	}

	// The analyzer summary should have a skip reason, not be marked Ran.
	var found bool
	for _, a := range sum.Analyzers {
		if a.Name == "staticcheck" {
			found = true
			if a.Ran {
				t.Error("staticcheck.Ran should be false when binary is absent")
			}
			if a.SkippedReason == "" {
				t.Error("staticcheck.SkippedReason should be non-empty when binary is absent")
			}
		}
	}
	if !found {
		t.Error("staticcheck entry not found in Analyzers summary")
	}
}

func TestSeed_garbageOutput_notAnError(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	// Mock: nonzero exit, garbage stdout — not SARIF
	mock := sandbox.NewMock(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 1, Stdout: "this is not sarif at all"},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "test-image")
	if err != nil {
		t.Fatalf("garbage output must not cause Seed to error: %v", err)
	}
	if sum.TotalPosted != 0 {
		t.Errorf("TotalPosted = %d, want 0", sum.TotalPosted)
	}
	for _, a := range sum.Analyzers {
		if a.Name == "staticcheck" && a.SkippedReason == "" {
			t.Error("staticcheck.SkippedReason should be set for garbage output")
		}
	}
}

func TestSeed_timeout_notAnError(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	// Mock: timed out
	mock := sandbox.NewMock(sandbox.MockResponse{
		Result: sandbox.Result{TimedOut: true, ExitCode: -1},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "test-image")
	if err != nil {
		t.Fatalf("timeout must not cause Seed to error: %v", err)
	}
	if sum.TotalPosted != 0 {
		t.Errorf("TotalPosted = %d, want 0", sum.TotalPosted)
	}
	for _, a := range sum.Analyzers {
		if a.Name == "staticcheck" && a.SkippedReason == "" {
			t.Error("staticcheck.SkippedReason should be set for timeout")
		}
	}
}

func TestSeed_infraError_notAnError(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	// Mock: infrastructure error from Exec
	mock := sandbox.NewMock(sandbox.MockResponse{
		Err: errFakeSandbox,
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "test-image")
	if err != nil {
		t.Fatalf("sandbox infrastructure error must not cause Seed to error: %v", err)
	}
	if sum.TotalPosted != 0 {
		t.Errorf("TotalPosted = %d, want 0", sum.TotalPosted)
	}
}

// errFakeSandbox is a sentinel error for tests that simulate a sandbox
// infrastructure failure.
var errFakeSandbox = sandboxError("sandbox: container could not be started")

type sandboxError string

func (e sandboxError) Error() string { return string(e) }

func TestSeed_notApplicable_skipped(t *testing.T) {
	// Empty dir: no go.mod, no Python markers → both analyzers skip.
	dir := t.TempDir()

	mock := sandbox.NewMock(sandbox.MockResponse{})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "")
	if err != nil {
		t.Fatalf("Seed returned error: %v", err)
	}
	if sum.TotalPosted != 0 {
		t.Errorf("TotalPosted = %d, want 0", sum.TotalPosted)
	}
	if mock.CallCount() != 0 {
		t.Errorf("sandbox.Exec should not have been called for non-applicable project, got %d calls", mock.CallCount())
	}
}

func TestSeed_upsertDedup(t *testing.T) {
	// Posting the same (targetLens, file, line) twice should result in 1 lead,
	// matching the store's UNIQUE constraint + upsert semantics.
	dir := t.TempDir()
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	sarifOut := `{"runs":[{"results":[{"ruleId":"SA2001","message":{"text":"race"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"foo.go"},"region":{"startLine":1}}}]}]}]}`
	mock := sandbox.NewMock(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 1, Stdout: sarifOut},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	_, err = Seed(ctx, mock, dir, st, "")
	if err != nil {
		t.Fatalf("first Seed: %v", err)
	}

	// Second Seed call: re-enqueue the same response.
	mock.EnqueueResponse(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 1, Stdout: sarifOut},
	})
	_, err = Seed(ctx, mock, dir, st, "")
	if err != nil {
		t.Fatalf("second Seed: %v", err)
	}

	leads, err := st.PendingLeads(ctx, lensConcurrency)
	if err != nil {
		t.Fatalf("PendingLeads: %v", err)
	}
	if len(leads) != 1 {
		t.Errorf("want 1 lead after upsert, got %d", len(leads))
	}
}

func TestSeed_ruff_leads(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "requirements.txt", "requests==2.28.0\n"); err != nil {
		t.Fatal(err)
	}

	sarifOut := `{
		"runs":[{"results":[
			{"ruleId":"S301","message":{"text":"insecure pickle"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"app.py"},"region":{"startLine":20}}}]},
			{"ruleId":"E501","message":{"text":"line too long"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"app.py"},"region":{"startLine":21}}}]}
		]}]
	}`

	mock := sandbox.NewMock(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 1, Stdout: sarifOut},
	})

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sum, err := Seed(ctx, mock, dir, st, "")
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// S301 → injection lens posted; E501 → style, skipped.
	if sum.TotalPosted != 1 {
		t.Errorf("TotalPosted = %d, want 1 (E501 style should be skipped)", sum.TotalPosted)
	}

	leads, err := st.PendingLeads(ctx, lensInjection)
	if err != nil {
		t.Fatalf("PendingLeads(injection): %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("want 1 injection lead, got %d", len(leads))
	}
	if leads[0].File != "app.py" || leads[0].Line != 20 {
		t.Errorf("lead = {%s %d}, want {app.py 20}", leads[0].File, leads[0].Line)
	}
}

// --------------------------------------------------------------------------
// gosec rule → lens mapping tests
// --------------------------------------------------------------------------

func TestGosecRuleLens(t *testing.T) {
	tests := []struct {
		ruleID   string
		wantLens string
	}{
		// G1xx — credentials / audit → injection
		{"G101", lensInjection}, // hardcoded credentials
		{"G102", lensInjection}, // network binding
		{"G107", lensInjection}, // URL from variable (SSRF precursor)
		{"G115", lensInjection}, // integer overflow (G1 prefix)

		// G2xx — injection sinks → injection
		{"G201", lensInjection}, // SQL injection (string formatting)
		{"G202", lensInjection}, // SQL injection (string concatenation)
		{"G203", lensInjection}, // template injection
		{"G204", lensInjection}, // command injection

		// G3xx — filesystem / permissions split by sub-rule
		{"G304", lensInjection}, // path traversal (file from variable) → injection
		{"G310", lensInjection}, // symlink follow → injection
		{"G301", lensResources}, // file permissions (mkdir) → resources
		{"G302", lensResources}, // file permissions (chmod) → resources
		{"G303", lensResources}, // tempfile in predictable location → resources
		{"G306", lensResources}, // file permissions (os.WriteFile) → resources
		{"G307", lensResources}, // deferred close → resources

		// G4xx — weak crypto → injection
		{"G401", lensInjection}, // use of MD5
		{"G402", lensInjection}, // TLS InsecureSkipVerify
		{"G403", lensInjection}, // weak RSA key
		{"G404", lensInjection}, // weak random (math/rand)
		{"G406", lensInjection}, // use of SHA1

		// G5xx — blocklisted imports → injection
		{"G501", lensInjection}, // import of crypto/md5 blocklisted
		{"G502", lensInjection}, // import of crypto/des
		{"G505", lensInjection}, // import of crypto/sha1

		// G6xx — memory safety → boundary
		{"G601", lensBoundary}, // implicit memory aliasing in for-range
		{"G602", lensBoundary}, // slice access out of bounds

		// Default: unknown rule → nil-safety
		{"UNKNOWN", lensNilSafety},
		{"G999", lensNilSafety}, // G9xx → default → nil-safety
	}
	for _, tt := range tests {
		got := gosecRuleLens(tt.ruleID)
		if got != tt.wantLens {
			t.Errorf("gosecRuleLens(%q) = %q, want %q", tt.ruleID, got, tt.wantLens)
		}
	}
}

func TestSeed_gosec_leads(t *testing.T) {
	dir := t.TempDir()
	// go.mod is the Go project marker for gosec detection.
	if err := writeFile(dir, "go.mod", "module example.com/foo\ngo 1.21\n"); err != nil {
		t.Fatal(err)
	}

	// Simulate gosec SARIF output: G401 (MD5) → injection, G601 → boundary,
	// and a hypothetical unknown rule → nil-safety.
	sarifOut := `{
		"runs":[{"results":[
			{"ruleId":"G401","message":{"text":"use of MD5"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"crypto.go"},"region":{"startLine":10}}}]},
			{"ruleId":"G601","message":{"text":"implicit memory aliasing in for loop"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"iter.go"},"region":{"startLine":5}}}]},
			{"ruleId":"G999","message":{"text":"unknown gosec rule"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"misc.go"},"region":{"startLine":3}}}]}
		]}]
	}`

	// The mock is keyed by cmd; since the registry runs staticcheck first
	// (go.mod present), we need to ensure the mock handles both staticcheck
	// and gosec invocations. Use a no-op staticcheck response and a gosec
	// response. sandbox.NewMock returns the same response for any cmd, so
	// we use a single mock and check TotalPosted across all runs.
	//
	// Strategy: run gosecSpec directly via runAnalyzer to test gosec in
	// isolation, exactly as the ruff integration test does.
	mock := sandbox.NewMock(sandbox.MockResponse{
		Result: sandbox.Result{ExitCode: 0, Stdout: sarifOut},
	})

	ctx := context.Background()
	arun := runAnalyzer(ctx, gosecSpec, mock, dir, "test-image")

	if !arun.Ran {
		t.Fatalf("gosec did not run: %s", arun.SkippedReason)
	}
	if arun.SkippedReason != "" {
		t.Fatalf("gosec skipped unexpectedly: %s", arun.SkippedReason)
	}
	if arun.Hits != 3 {
		t.Errorf("Hits = %d, want 3", arun.Hits)
	}

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	posted, err := postLeads(ctx, arun.results, "gosec", st)
	if err != nil {
		t.Fatalf("postLeads: %v", err)
	}
	if posted != 3 {
		t.Errorf("posted = %d, want 3", posted)
	}

	injLeads, err := st.PendingLeads(ctx, lensInjection)
	if err != nil {
		t.Fatalf("PendingLeads(injection): %v", err)
	}
	if len(injLeads) != 1 {
		t.Fatalf("injection leads = %d, want 1 (G401)", len(injLeads))
	}
	if injLeads[0].PosterLens != "analyzer:gosec" {
		t.Errorf("PosterLens = %q, want analyzer:gosec", injLeads[0].PosterLens)
	}

	bndLeads, err := st.PendingLeads(ctx, lensBoundary)
	if err != nil {
		t.Fatalf("PendingLeads(boundary): %v", err)
	}
	if len(bndLeads) != 1 {
		t.Fatalf("boundary leads = %d, want 1 (G601 only)", len(bndLeads))
	}

	nilLeads, err := st.PendingLeads(ctx, lensNilSafety)
	if err != nil {
		t.Fatalf("PendingLeads(nil-safety): %v", err)
	}
	if len(nilLeads) != 1 {
		t.Fatalf("nil-safety leads = %d, want 1 (G999)", len(nilLeads))
	}
}

// --------------------------------------------------------------------------
// Helper
// --------------------------------------------------------------------------

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
