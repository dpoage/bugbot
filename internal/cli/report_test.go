package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// minimalConfig is a valid config sufficient for store-backed report commands.
const minimalConfig = `providers:
  anthropic:
    type: anthropic
    api_key_env: ANTHROPIC_API_KEY
roles:
  finder: {provider: anthropic, model: m}
  verifier: {provider: anthropic, model: m}
  reproducer: {provider: anthropic, model: m}
storage:
  path: %DBPATH%
report:
  dir: %REPORTDIR%
  sinks: [stdout]
`

// setup writes a config file pointing at a fresh store seeded with one open
// finding, and returns the config path, store, and the seeded finding.
func setup(t *testing.T) (cfgPath string, st *store.Store, f store.Finding) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	reportDir := filepath.Join(dir, "reports")

	ctx := context.Background()
	var err error
	st, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	f = store.Finding{
		Fingerprint: store.Fingerprint("race", "x.go", 7, "boom"),
		Title:       "boom",
		Description: "desc",
		Reasoning:   "trace",
		Severity:    "high",
		Tier:        2,
		Status:      store.StatusOpen,
		Lens:        "race",
		File:        "x.go",
		Line:        7,
		CommitSHA:   "c1",
	}
	f, err = st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Close so the CLI command can open the same db (WAL allows concurrent, but
	// keep it clean by reopening fresh handles per command).
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cfgYAML := strings.NewReplacer("%DBPATH%", dbPath, "%REPORTDIR%", reportDir).Replace(minimalConfig)
	cfgPath = filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, st, f
}

// run executes the root command with args and returns combined stdout/stderr.
func run(t *testing.T, cfgPath string, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	full := append([]string{"--config", cfgPath}, args...)
	root.SetArgs(full)
	err := root.Execute()
	return buf.String(), err
}

func TestReportList(t *testing.T) {
	cfgPath, _, f := setup(t)
	out, err := run(t, cfgPath, "report", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "x.go:7") {
		t.Errorf("list output missing finding: %s", out)
	}
	if !strings.Contains(out, f.ID[:12]) {
		t.Errorf("list output missing short id: %s", out)
	}
}

func TestReportListJSON(t *testing.T) {
	cfgPath, _, _ := setup(t)
	out, err := run(t, cfgPath, "report", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if !strings.Contains(out, `"Title": "boom"`) {
		t.Errorf("json output missing title: %s", out)
	}
}

func TestReportShowByPrefix(t *testing.T) {
	cfgPath, _, f := setup(t)
	out, err := run(t, cfgPath, "report", "show", f.ID[:12])
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"boom", "trace", "x.go:7", f.Fingerprint} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}
}

func TestReportShowUnknown(t *testing.T) {
	cfgPath, _, _ := setup(t)
	if _, err := run(t, cfgPath, "report", "show", "nope"); err == nil {
		t.Error("expected error for unknown id")
	}
}

func TestReportDismissRequiresReason(t *testing.T) {
	cfgPath, _, f := setup(t)
	if _, err := run(t, cfgPath, "report", "dismiss", f.ID[:12]); err == nil {
		t.Error("dismiss without --reason should error")
	}
}

func TestReportDismissFlow(t *testing.T) {
	cfgPath, _, f := setup(t)

	out, err := run(t, cfgPath, "report", "dismiss", f.ID[:12], "--reason", "false positive")
	if err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if !strings.Contains(out, "fingerprint suppressed") {
		t.Errorf("dismiss confirmation missing: %s", out)
	}

	// Now it must be suppressed in the store.
	ctx := context.Background()
	st, err := store.Open(ctx, configStoragePath(t, cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suppressed, err := st.IsSuppressed(ctx, f.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !suppressed {
		t.Error("finding not suppressed after dismiss")
	}

	// And list --status dismissed must show it.
	out, err = run(t, cfgPath, "report", "list", "--status", "dismissed")
	if err != nil {
		t.Fatalf("list dismissed: %v", err)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("dismissed finding not listed: %s", out)
	}

	// Default (open) list must no longer show it.
	out, err = run(t, cfgPath, "report", "list")
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if strings.Contains(out, "boom") {
		t.Errorf("dismissed finding still shows under open: %s", out)
	}
}

func TestReportEmitStdout(t *testing.T) {
	cfgPath, _, _ := setup(t)
	out, err := run(t, cfgPath, "report", "emit", "--sink", "stdout")
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(out, "# Bugbot Report") || !strings.Contains(out, "boom") {
		t.Errorf("emit did not render markdown: %s", out)
	}
}

func TestReportEmitFS(t *testing.T) {
	cfgPath, _, _ := setup(t)
	reportDir := configReportDir(t, cfgPath)
	if _, err := run(t, cfgPath, "report", "emit", "--sink", "fs"); err != nil {
		t.Fatalf("emit fs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reportDir, "latest.sarif")); err != nil {
		t.Errorf("fs sink did not write latest.sarif: %v", err)
	}
}

// configStoragePath / configReportDir read paths back out of the written config
// without re-deriving them, keeping the test resilient to TempDir layout.
func configStoragePath(t *testing.T, cfgPath string) string {
	t.Helper()
	return grepYAMLPath(t, cfgPath, "path:")
}

func configReportDir(t *testing.T, cfgPath string) string {
	t.Helper()
	return grepYAMLPath(t, cfgPath, "dir:")
}

func grepYAMLPath(t *testing.T, cfgPath, key string) string {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		}
	}
	t.Fatalf("key %q not found in config", key)
	return ""
}
