package store

import (
	"context"
	"testing"
)

// TestLatestScanRun covers the most-recent selection and ErrNotFound.
func TestLatestScanRun(t *testing.T) {
	ctx := context.Background()
	st := openTemp(t)

	if _, err := st.LatestScanRun(ctx); err != ErrNotFound {
		t.Fatalf("empty store: err = %v, want ErrNotFound", err)
	}

	id1, err := st.BeginScanRun(ctx, ScanSweep, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishScanRun(ctx, id1, "{}"); err != nil {
		t.Fatal(err)
	}
	id2, err := st.BeginScanRun(ctx, ScanTargeted, "c2")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.LatestScanRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id2 || got.Kind != ScanTargeted {
		t.Errorf("latest = %+v, want id %s (targeted)", got, id2)
	}
}
