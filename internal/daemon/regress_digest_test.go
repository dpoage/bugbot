package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/domain"
	"github.com/dpoage/bugbot/internal/funnel"
	"github.com/dpoage/bugbot/internal/store"
)

// TestIntroducedSince proves the attribution core: against a baseline commit A,
// a finding on a file added AFTER A is introduced, while a finding on a file
// present at A is pre-existing and excluded.
func TestIntroducedSince(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write("old.go", "package p\n\nfunc Old() {}\n")
	a := fr.commit("A: old.go")
	fr.write("new.go", "package p\n\nfunc New() {}\n")
	fr.commit("B: add new.go")
	repo := fr.open()

	findings := []domain.Finding{
		{File: "new.go", Line: 3, Title: "bug in new code"}, // absent at A -> introduced
		{File: "old.go", Line: 3, Title: "bug in old code"}, // present at A -> pre-existing
	}
	got := introducedSince(context.Background(), repo, a, findings)
	if len(got) != 1 {
		t.Fatalf("introducedSince returned %d findings, want 1: %+v", len(got), got)
	}
	if got[0].File != "new.go" {
		t.Errorf("introduced finding = %q, want new.go", got[0].File)
	}
}

// TestEmitRegressDigest is the end-to-end 'new since last green' path: a prior
// FINISHED sweep at commit A is the baseline; this cycle's findings include one
// on a file added after A (introduced) and one on a pre-existing file. The
// digest must report only the introduced finding, with the baseline and count.
func TestEmitRegressDigest(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write("old.go", "package p\n\nfunc Old() {}\n")
	a := fr.commit("A: old.go")
	fr.write("new.go", "package p\n\nfunc New() {}\n")
	fr.commit("B: add new.go")

	st := openStore(t)
	ctx := context.Background()

	// Seed a prior FINISHED sweep at commit A — the 'last green' baseline.
	sweep, err := st.BeginScanRun(ctx, store.ScanSweep, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, sweep, "{}"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	d, err := New(Deps{
		Repo:    fr.open(),
		Store:   st,
		Clients: funnel.RoleClients{Finder: newFakeLLM("", ""), Verifier: newFakeLLM("", "")},
		Logger:  slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, DaemonConfig{PollInterval: time.Hour, SweepInterval: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// This cycle's run is a targeted poll (not itself a sweep) so the sweep at A
	// remains the baseline.
	cur, err := st.BeginScanRun(ctx, store.ScanTargeted, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	fres := &funnel.Result{
		ScanRunID: cur,
		Findings: []domain.Finding{
			{File: "new.go", Line: 3, Title: "introduced-bug", Severity: "high"},
			{File: "old.go", Line: 3, Title: "preexisting-bug", Severity: "low"},
		},
	}
	d.emitRegressDigest(ctx, fres)

	out := buf.String()
	if !strings.Contains(out, "introduced since last green sweep") {
		t.Fatalf("digest header missing:\n%s", out)
	}
	if !strings.Contains(out, "introduced=1") {
		t.Errorf("want introduced=1 in digest:\n%s", out)
	}
	if !strings.Contains(out, "new.go") || !strings.Contains(out, "introduced-bug") {
		t.Errorf("introduced finding (new.go) not reported:\n%s", out)
	}
	if strings.Contains(out, "preexisting-bug") {
		t.Errorf("pre-existing finding must NOT be in the digest:\n%s", out)
	}
}

// TestEmitRegressDigest_NoBaseline verifies that with no prior finished sweep,
// the digest is a silent no-op (nothing logged) rather than an error.
func TestEmitRegressDigest_NoBaseline(t *testing.T) {
	fr := newFixtureRepo(t)
	fr.write("a.go", "package p\n")
	fr.commit("init")

	st := openStore(t)
	ctx := context.Background()

	var buf bytes.Buffer
	d, err := New(Deps{
		Repo:    fr.open(),
		Store:   st,
		Clients: funnel.RoleClients{Finder: newFakeLLM("", ""), Verifier: newFakeLLM("", "")},
		Logger:  slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, DaemonConfig{PollInterval: time.Hour, SweepInterval: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cur, err := st.BeginScanRun(ctx, store.ScanTargeted, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	fres := &funnel.Result{
		ScanRunID: cur,
		Findings:  []domain.Finding{{File: "a.go", Line: 1, Title: "bug"}},
	}
	d.emitRegressDigest(ctx, fres)

	if strings.Contains(buf.String(), "regress digest") {
		t.Errorf("digest must be silent when no prior sweep exists, got:\n%s", buf.String())
	}
}
