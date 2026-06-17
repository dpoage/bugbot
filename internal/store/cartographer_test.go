package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPackageSummaries_RoundTrip covers the four behaviours the contract
// pins: insert two distinct packages, get them back with correct fields,
// update one in place, and assert an absent pkg is missing from the map.
//
// An in-memory DB is private per database/sql connection, which would
// surface as "table not found" the first time the pool handed a query to a
// different connection. The funnel test harness avoids that by going
// file-backed (see openTemp). We do the same here so this test is
// representative of how the funnel will read the table.
func TestPackageSummaries_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	// Insert two distinct packages.
	original := []PackageSummary{
		{Pkg: "internal/store", Fingerprint: "fp-store-1", Summary: "durable state for findings, watermarks, summaries", Model: "test-model"},
		{Pkg: "internal/ingest", Fingerprint: "fp-ingest-1", Summary: "git-backed repository snapshot, blast-radius, fingerprinting", Model: "test-model"},
	}
	if err := st.UpsertPackageSummaries(ctx, original); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	got, err := st.GetPackageSummaries(ctx, []string{"internal/store", "internal/ingest", "internal/missing"})
	if err != nil {
		t.Fatalf("GetPackageSummaries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d summaries, want 2 (the missing pkg must be absent from the map)", len(got))
	}
	for _, want := range original {
		gotPS, ok := got[want.Pkg]
		if !ok {
			t.Errorf("summary for %q missing", want.Pkg)
			continue
		}
		if gotPS.Fingerprint != want.Fingerprint {
			t.Errorf("%s: fingerprint = %q, want %q", want.Pkg, gotPS.Fingerprint, want.Fingerprint)
		}
		if gotPS.Summary != want.Summary {
			t.Errorf("%s: summary = %q, want %q", want.Pkg, gotPS.Summary, want.Summary)
		}
		if gotPS.Model != want.Model {
			t.Errorf("%s: model = %q, want %q", want.Pkg, gotPS.Model, want.Model)
		}
		if gotPS.UpdatedAt.IsZero() {
			t.Errorf("%s: UpdatedAt is zero (upsert should fill in now)", want.Pkg)
		}
	}

	// Update one row with a NEW fingerprint and summary. ON CONFLICT(pkg)
	// DO UPDATE means the same Pkg primary key is overwritten in place.
	updated := []PackageSummary{{
		Pkg:         "internal/store",
		Fingerprint: "fp-store-2-CHANGED",
		Summary:     "durable state, now with summaries table",
		Model:       "test-model-v2",
	}}
	if err := st.UpsertPackageSummaries(ctx, updated); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got2, err := st.GetPackageSummaries(ctx, []string{"internal/store"})
	if err != nil {
		t.Fatalf("GetPackageSummaries after update: %v", err)
	}
	if got2["internal/store"].Fingerprint != "fp-store-2-CHANGED" {
		t.Errorf("update did not take: got fingerprint %q, want %q",
			got2["internal/store"].Fingerprint, "fp-store-2-CHANGED")
	}
	if got2["internal/store"].Summary != "durable state, now with summaries table" {
		t.Errorf("update did not take: got summary %q", got2["internal/store"].Summary)
	}
	if got2["internal/store"].Model != "test-model-v2" {
		t.Errorf("update did not take: got model %q", got2["internal/store"].Model)
	}
	// The other row must be untouched.
	got3, err := st.GetPackageSummaries(ctx, []string{"internal/ingest"})
	if err != nil {
		t.Fatalf("GetPackageSummaries (untouched): %v", err)
	}
	if got3["internal/ingest"].Fingerprint != "fp-ingest-1" {
		t.Errorf("unrelated row mutated: got fingerprint %q, want %q",
			got3["internal/ingest"].Fingerprint, "fp-ingest-1")
	}
}

// TestPackageSummaries_AbsentPkgMissingFromMap pins the contract: a pkg that
// has no row is absent from the result map (callers must treat absence as
// "not summarized yet"), not returned as a zero-valued PackageSummary.
func TestPackageSummaries_AbsentPkgMissingFromMap(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if err := st.UpsertPackageSummaries(ctx, []PackageSummary{
		{Pkg: "pkg/present", Fingerprint: "fp1", Summary: "s1"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.GetPackageSummaries(ctx, []string{"pkg/present", "pkg/absent"})
	if err != nil {
		t.Fatalf("GetPackageSummaries: %v", err)
	}
	if _, ok := got["pkg/absent"]; ok {
		t.Error("absent pkg present in result map; callers expect absence to signal \"not summarized\"")
	}
	if _, ok := got["pkg/present"]; !ok {
		t.Error("present pkg missing from result map")
	}
}

// TestPackageSummaries_RejectsInvalidRow pins the validation guard: an empty
// Pkg, Fingerprint, or Summary must fail loudly rather than write a corrupt
// or colliding row. The empty Pkg is the most insidious (it would silently
// match every other empty-Pkg write in the same transaction), so we exercise
// that case explicitly.
func TestPackageSummaries_RejectsInvalidRow(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	cases := []struct {
		name string
		ps   PackageSummary
	}{
		{"empty pkg", PackageSummary{Fingerprint: "fp", Summary: "s"}},
		{"empty fingerprint", PackageSummary{Pkg: "p", Summary: "s"}},
		{"empty summary", PackageSummary{Pkg: "p", Fingerprint: "fp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.UpsertPackageSummaries(ctx, []PackageSummary{tc.ps})
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !errors.Is(err, ErrInvalidPackageSummary) {
				t.Errorf("err = %v, want ErrInvalidPackageSummary", err)
			}
		})
	}
}

// TestPackageSummaries_EmptyInputNoOp pins the no-op semantics: an empty
// batch must not open a transaction. This is a small but useful invariant
// for the funnel's "I have no uncached packages" fast path.
func TestPackageSummaries_EmptyInputNoOp(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)
	if err := st.UpsertPackageSummaries(ctx, nil); err != nil {
		t.Errorf("nil batch should be a no-op, got %v", err)
	}
	if err := st.UpsertPackageSummaries(ctx, []PackageSummary{}); err != nil {
		t.Errorf("empty batch should be a no-op, got %v", err)
	}
}

// TestPackageSummaries_UpdatedAtCallerOverride pins that a caller-supplied
// UpdatedAt is honored: the funnel may want to back-date a row for testing
// or re-issuance. The default fill-in is tested by RoundTrip; this test
// covers the explicit path.
func TestPackageSummaries_UpdatedAtCallerOverride(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := st.UpsertPackageSummaries(ctx, []PackageSummary{{
		Pkg: "p", Fingerprint: "fp", Summary: "s", UpdatedAt: want,
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetPackageSummaries(ctx, []string{"p"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got["p"].UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %s, want %s", got["p"].UpdatedAt, want)
	}
}
