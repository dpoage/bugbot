package funnel

import (
	"reflect"
	"sort"
	"testing"
)

// cand is a terse Candidate constructor for the clustering tests. Description is
// what the similarity guard reads, so the tests set it deliberately: duplicates
// of one defect share words, distinct defects do not.
func cand(lens, file string, line int, title, sev, conf, desc string) Candidate {
	return Candidate{
		Lens: lens, File: file, Line: line, Title: title,
		Severity: sev, Confidence: conf, Description: desc,
		// A unique fingerprint per candidate (mergeClusters uses it only as a
		// reference; order restoration no longer depends on it, but triage sets it
		// in production so the tests mirror that).
		Fingerprint: lens + "|" + file + "|" + title,
	}
}

// titles extracts the surviving candidates' titles in returned order.
func titles(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Title
	}
	return out
}

func TestMergeClusters_CrossLensMergeWithinWindow(t *testing.T) {
	// Two lenses report the same defect at the same line with different titles
	// (so different fingerprints — they survive exact dedup). They must collapse
	// to one primary, with the other lens recorded as corroboration.
	in := []Candidate{
		cand("resource-leaks", "read.go", 17, "fd leak on error path", "high", "high",
			"file f is not closed on the io.ReadFull error return path"),
		cand("nil-safety/error-handling", "read.go", 17, "unclosed file descriptor", "medium", "high",
			"the file descriptor f leaks when ReadFull returns an error"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)

	if len(got) != 1 {
		t.Fatalf("want 1 survivor, got %d: %v", len(got), titles(got))
	}
	// Primary is the higher-severity one (high > medium).
	if got[0].Title != "fd leak on error path" {
		t.Errorf("primary = %q, want the high-severity report", got[0].Title)
	}
	if want := []string{"nil-safety/error-handling"}; !reflect.DeepEqual(got[0].CorroboratingLenses, want) {
		t.Errorf("corroborating = %v, want %v", got[0].CorroboratingLenses, want)
	}
	if stats.MergedCrossLens != 1 || stats.MergedWithinLens != 0 {
		t.Errorf("merge stats = within %d / cross %d, want 0/1", stats.MergedWithinLens, stats.MergedCrossLens)
	}
}

func TestMergeClusters_NoMergeAcrossFiles(t *testing.T) {
	in := []Candidate{
		cand("resource-leaks", "a.go", 10, "leak A", "high", "high", "file handle leaks on error return"),
		cand("nil-safety/error-handling", "b.go", 10, "leak B", "high", "high", "file handle leaks on error return"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 2 {
		t.Fatalf("want 2 survivors (different files), got %d: %v", len(got), titles(got))
	}
	if stats.MergedCrossLens != 0 || stats.MergedWithinLens != 0 {
		t.Errorf("no merge expected, got within %d / cross %d", stats.MergedWithinLens, stats.MergedCrossLens)
	}
}

func TestMergeClusters_NoMergeBeyondWindow(t *testing.T) {
	// Same defect description but far apart: line distance exceeds the window, so
	// they are different occurrences and must not merge.
	in := []Candidate{
		cand("resource-leaks", "x.go", 10, "leak high", "high", "high", "file handle leaks on the error return path"),
		cand("nil-safety/error-handling", "x.go", 30, "leak low", "high", "high", "file handle leaks on the error return path"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 2 {
		t.Fatalf("want 2 survivors (>window apart), got %d: %v", len(got), titles(got))
	}
}

func TestMergeClusters_DistinctDefectsWithinWindowStaySeparate(t *testing.T) {
	// The hard case the spec calls out: two genuinely-distinct defects within the
	// window (a negative-length panic two lines from a file-descriptor leak).
	// Proximity alone would merge them; the description-similarity guard keeps
	// them apart so neither real finding is lost.
	in := []Candidate{
		cand("boundary-conditions", "read.go", 15, "negative n panic", "medium", "high",
			"make([]byte, n) panics when the caller passes a negative length n"),
		cand("resource-leaks", "read.go", 17, "fd leak", "high", "high",
			"the opened file f is never closed on the io.ReadFull error return path"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 2 {
		t.Fatalf("distinct defects within window must stay separate; got %d: %v", len(got), titles(got))
	}
	if stats.MergedCrossLens != 0 || stats.MergedWithinLens != 0 {
		t.Errorf("no merge expected for distinct defects, got within %d / cross %d", stats.MergedWithinLens, stats.MergedCrossLens)
	}
}

func TestMergeClusters_PrimaryBySeverityThenConfidence(t *testing.T) {
	// All three at the same line with similar descriptions => one cluster. Primary
	// must be chosen by severity first, then confidence.
	in := []Candidate{
		cand("l1", "f.go", 5, "low sev high conf", "low", "high", "the shared buffer is read after free in the cleanup path"),
		cand("l2", "f.go", 5, "high sev low conf", "high", "low", "the shared buffer is read after free in the cleanup path"),
		cand("l3", "f.go", 5, "high sev high conf", "high", "high", "the shared buffer is read after free in the cleanup path"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 1 {
		t.Fatalf("want 1 survivor, got %d: %v", len(got), titles(got))
	}
	if got[0].Title != "high sev high conf" {
		t.Errorf("primary = %q, want highest severity then highest confidence", got[0].Title)
	}
	if want := []string{"l1", "l2"}; !equalStringSet(got[0].CorroboratingLenses, want) {
		t.Errorf("corroborating = %v, want %v", got[0].CorroboratingLenses, want)
	}
	if stats.MergedCrossLens != 2 {
		t.Errorf("cross-lens merges = %d, want 2", stats.MergedCrossLens)
	}
}

func TestMergeClusters_PrimaryByDescriptionLengthTieBreak(t *testing.T) {
	// Same severity and confidence => tie-break on the longest (most specific)
	// description.
	short := "buffer reused after free in cleanup"
	long := "buffer reused after free in cleanup leading to a use-after-free read that can crash"
	in := []Candidate{
		cand("l1", "f.go", 8, "short", "high", "high", short),
		cand("l2", "f.go", 8, "long", "high", "high", long),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 1 {
		t.Fatalf("want 1 survivor, got %d", len(got))
	}
	if got[0].Title != "long" {
		t.Errorf("primary = %q, want the longest-description member", got[0].Title)
	}
}

func TestMergeClusters_WithinVsCrossLensCounterSplit(t *testing.T) {
	// One cluster with the primary and two non-primary members: one shares the
	// primary's lens (within-lens), one differs (cross-lens). The within-lens
	// member must NOT appear as a corroborating lens (it is the same lens), but it
	// must still be counted.
	in := []Candidate{
		cand("resource-leaks", "f.go", 12, "primary", "high", "high",
			"connection conn is never closed on the timeout error return path"),
		cand("resource-leaks", "f.go", 12, "same lens dup", "medium", "high",
			"connection conn leaks because Close is skipped on the timeout error path"),
		cand("concurrency", "f.go", 12, "other lens dup", "medium", "high",
			"connection conn is left open on the timeout error path, a leak"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 1 {
		t.Fatalf("want 1 survivor, got %d: %v", len(got), titles(got))
	}
	if stats.MergedWithinLens != 1 {
		t.Errorf("within-lens merges = %d, want 1", stats.MergedWithinLens)
	}
	if stats.MergedCrossLens != 1 {
		t.Errorf("cross-lens merges = %d, want 1", stats.MergedCrossLens)
	}
	if want := []string{"concurrency"}; !reflect.DeepEqual(got[0].CorroboratingLenses, want) {
		t.Errorf("corroborating = %v, want %v (within-lens dup excluded)", got[0].CorroboratingLenses, want)
	}
}

func TestMergeClusters_TransitiveChaining(t *testing.T) {
	// Transitivity: A and C are 8 lines apart (within the window of 10) and share
	// the same defect description, with B at the midpoint. All three collapse into
	// one cluster even though the chain spans the file via pairwise proximity +
	// similarity. Documents the implemented behavior: membership is transitive
	// through pairwise-near, pairwise-similar links.
	desc := "the mutex mu is acquired but not released on the early validation error return"
	in := []Candidate{
		cand("l1", "f.go", 20, "A", "high", "high", desc),
		cand("l2", "f.go", 24, "B", "high", "high", desc),
		cand("l3", "f.go", 28, "C", "high", "high", desc),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if len(got) != 1 {
		t.Fatalf("transitive chain should collapse to 1, got %d: %v", len(got), titles(got))
	}
	if stats.MergedCrossLens != 2 {
		t.Errorf("cross-lens merges = %d, want 2", stats.MergedCrossLens)
	}
}

func TestMergeClusters_WindowZeroDisablesMerge(t *testing.T) {
	in := []Candidate{
		cand("l1", "f.go", 5, "a", "high", "high", "same exact defect description here words"),
		cand("l2", "f.go", 5, "b", "high", "high", "same exact defect description here words"),
	}
	var stats Stats
	got := mergeClusters(in, 0, &stats)
	if len(got) != 2 {
		t.Fatalf("window 0 must disable merging, got %d survivors", len(got))
	}
	if stats.MergedCrossLens != 0 || stats.MergedWithinLens != 0 {
		t.Errorf("window 0 must not count merges")
	}
}

func TestMergeClusters_PreservesOriginalOrder(t *testing.T) {
	// Survivors come back in first-seen order, not per-file line order.
	in := []Candidate{
		cand("l1", "b.go", 50, "second-file", "high", "high", "index out of range on empty slice access"),
		cand("l2", "a.go", 5, "first-file", "high", "high", "nil map written without initialization"),
	}
	var stats Stats
	got := mergeClusters(in, DefaultMergeWindow, &stats)
	if want := []string{"second-file", "first-file"}; !reflect.DeepEqual(titles(got), want) {
		t.Errorf("order = %v, want %v (first-seen order preserved)", titles(got), want)
	}
}

// equalStringSet compares two string slices as sets (order-independent), since
// corroborating lenses are sorted but the test's expectation is order-free.
func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	return reflect.DeepEqual(ac, bc)
}
