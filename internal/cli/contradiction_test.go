package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/store"
)

// setupContradicted creates a store with one contradicted finding (exit_zero_count >= threshold)
// and returns the config path and the finding's short id prefix.
func setupContradicted(t *testing.T) (cfgPath string, shortID string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	reportDir := filepath.Join(dir, "reports")

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	f := domain.Finding{
		Fingerprint: domain.Fingerprint("uaf", "alloc.go", fmt.Sprintf("%d|%s", 77, "uaf-in-alloc")),
		Title:       "heap use after free in allocator",
		Description: "allocator reuses freed memory.",
		Reasoning:   "verifier confirmed; repro exited 0 twice.",
		Severity:    "high",
		Tier:        2,
		Status:      domain.StatusOpen,
		Lens:        "uaf",
		File:        "alloc.go",
		Line:        77,
		CommitSHA:   "abc",
	}
	stored, err := st.UpsertFinding(ctx, f)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	// Enqueue and accumulate >= threshold exit-zero attempts.
	if _, err := st.EnqueueRepro(ctx, stored.Fingerprint); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < domain.ReproContradictionThreshold; i++ {
		if err := st.RecordExitZeroAttempt(ctx, stored.Fingerprint); err != nil {
			t.Fatalf("record exit zero #%d: %v", i+1, err)
		}
	}

	cfgYAML := strings.NewReplacer("%DBPATH%", dbPath, "%REPORTDIR%", reportDir).Replace(minimalConfig)
	cfgPath = filepath.Join(dir, "bugbot.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	return cfgPath, stored.ID[:12]
}

// TestReportList_ContradictedFlag verifies that `report list` shows the
// repro-contradicted flag in the FLAGS column for a contradicted finding.
func TestReportList_ContradictedFlag(t *testing.T) {
	cfgPath, _ := setupContradicted(t)
	out, err := run(t, cfgPath, "report", "list")
	if err != nil {
		t.Fatalf("report list: %v", err)
	}
	if !strings.Contains(out, "repro-contradicted") {
		t.Errorf("report list: FLAGS column missing 'repro-contradicted' for contradicted finding.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "FLAGS") {
		t.Errorf("report list: FLAGS column header missing.\nGot:\n%s", out)
	}
}

// TestReportShow_ContradictedLine verifies that `report show` prints a
// Repro-contradicted line for a contradicted finding.
func TestReportShow_ContradictedLine(t *testing.T) {
	cfgPath, shortID := setupContradicted(t)
	out, err := run(t, cfgPath, "report", "show", shortID)
	if err != nil {
		t.Fatalf("report show: %v", err)
	}
	if !strings.Contains(out, "Repro-contradicted") {
		t.Errorf("report show: Repro-contradicted line missing.\nGot:\n%s", out)
	}
	if !strings.Contains(out, "2") { // threshold
		t.Errorf("report show: Repro-contradicted line must include the threshold count.\nGot:\n%s", out)
	}
}

// TestReportList_NoFlagWhenNotContradicted verifies that a normal (non-contradicted)
// finding shows an empty FLAGS column, not a spurious label.
func TestReportList_NoFlagWhenNotContradicted(t *testing.T) {
	cfgPath, _, _ := setup(t)
	out, err := run(t, cfgPath, "report", "list")
	if err != nil {
		t.Fatalf("report list: %v", err)
	}
	// FLAGS header must still appear; the column value for non-contradicted is empty.
	if !strings.Contains(out, "FLAGS") {
		t.Errorf("report list: FLAGS header missing.\nGot:\n%s", out)
	}
	if strings.Contains(out, "repro-contradicted") {
		t.Errorf("report list: 'repro-contradicted' must not appear for non-contradicted finding.\nGot:\n%s", out)
	}
}
