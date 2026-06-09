package report

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/store"
)

// openStore opens a fresh on-disk store seeded with two findings whose ids share
// a common prefix so ambiguity can be exercised.
func openStore(t *testing.T) (*store.Store, store.Finding, store.Finding) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mk := func(lens, file string, line int, title string) store.Finding {
		f := store.Finding{
			Fingerprint: store.Fingerprint(lens, file, line, title),
			Title:       title,
			Description: "d",
			Reasoning:   "r",
			Severity:    "high",
			Tier:        2,
			Status:      store.StatusOpen,
			Lens:        lens,
			File:        file,
			Line:        line,
			CommitSHA:   "c1",
			FileHash:    "h",
		}
		stored, err := st.UpsertFinding(ctx, f)
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		return stored
	}

	a := mk("race", "a.go", 1, "first finding")
	b := mk("nilcheck", "b.go", 2, "second finding")
	return st, a, b
}

func TestResolveID_Exact(t *testing.T) {
	st, a, _ := openStore(t)
	got, err := ResolveID(context.Background(), st, a.ID)
	if err != nil {
		t.Fatalf("ResolveID exact: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("got %s, want %s", got.ID, a.ID)
	}
}

func TestResolveID_UniquePrefix(t *testing.T) {
	st, a, _ := openStore(t)
	// ids are 32 hex chars: 16 of timestamp + 16 of random. Findings created in
	// the same millisecond share the timestamp half, so take a prefix that
	// reaches into the random half (28 chars) to guarantee uniqueness while
	// still exercising prefix (not exact) resolution.
	prefix := a.ID[:28]
	got, err := ResolveID(context.Background(), st, prefix)
	if err != nil {
		t.Fatalf("ResolveID prefix: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("got %s, want %s", got.ID, a.ID)
	}
}

func TestResolveID_NotFound(t *testing.T) {
	st, _, _ := openStore(t)
	_, err := ResolveID(context.Background(), st, "ffffffffffffffff")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestResolveID_Ambiguous(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed findings and find a prefix shared by >=2 ids. ids begin with an
	// 8-byte ms timestamp, so findings created in the same millisecond share a
	// long common prefix; use a 1-char prefix which is essentially always shared.
	for i := 0; i < 8; i++ {
		f := store.Finding{
			Fingerprint: store.Fingerprint("race", "f.go", i, "t"),
			Title:       "t",
			Severity:    "low",
			Tier:        3,
			Status:      store.StatusOpen,
			Lens:        "race",
			File:        "f.go",
			Line:        i,
		}
		if _, err := st.UpsertFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.ListFindings(ctx, store.FindingFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Find a 1-char prefix matching at least two ids.
	counts := map[byte]int{}
	for _, f := range all {
		counts[f.ID[0]]++
	}
	var prefix string
	for c, n := range counts {
		if n >= 2 {
			prefix = string(c)
			break
		}
	}
	if prefix == "" {
		t.Skip("no shared single-char prefix among generated ids (unlikely)")
	}

	_, err = ResolveID(ctx, st, prefix)
	var amb *ErrAmbiguousID
	if !errors.As(err, &amb) {
		t.Fatalf("want ErrAmbiguousID, got %v", err)
	}
	if len(amb.Matches) < 2 {
		t.Errorf("ambiguous error should list >=2 matches, got %d", len(amb.Matches))
	}
}

func TestResolveID_EmptyErrors(t *testing.T) {
	st, _, _ := openStore(t)
	if _, err := ResolveID(context.Background(), st, "  "); err == nil {
		t.Error("expected error for empty id")
	}
}

func TestCollectOpen_OnlyOpen(t *testing.T) {
	ctx := context.Background()
	st, a, b := openStore(t)

	// Dismiss b; CollectOpen should then return only a.
	if err := st.AddSuppression(ctx, b.Fingerprint, "not a bug"); err != nil {
		t.Fatal(err)
	}

	rep, err := CollectOpen(ctx, st, fixtureMeta())
	if err != nil {
		t.Fatalf("CollectOpen: %v", err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].ID != a.ID {
		t.Fatalf("CollectOpen returned %d findings, want only %s", len(rep.Findings), a.ID)
	}
}

// TestDismissFlow exercises the full dismissal path the CLI uses: AddSuppression
// writes the suppression, IsSuppressed reports true, and the finding now lists
// under status=dismissed (and no longer under open).
func TestDismissFlow(t *testing.T) {
	ctx := context.Background()
	st, a, _ := openStore(t)

	if err := st.AddSuppression(ctx, a.Fingerprint, "false positive"); err != nil {
		t.Fatalf("AddSuppression: %v", err)
	}

	suppressed, err := st.IsSuppressed(ctx, a.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !suppressed {
		t.Fatal("fingerprint should be suppressed after dismiss")
	}

	open, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range open {
		if f.ID == a.ID {
			t.Error("dismissed finding still appears under status=open")
		}
	}

	dismissed, err := st.ListFindings(ctx, store.FindingFilter{Status: store.StatusDismissed})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range dismissed {
		if f.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Error("dismissed finding should list under status=dismissed")
	}
}
