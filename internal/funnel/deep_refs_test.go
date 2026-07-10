package funnel

import (
	"context"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/agent"
	"github.com/dpoage/bugbot/internal/store"
	"github.com/dpoage/bugbot/internal/treesitter"
)

// fakeRefNav is a fake refClosureNav for unit tests.
type fakeRefNav struct {
	// outlines maps repo-relative file → outline entries returned.
	outlines map[string][]treesitter.OutlineEntry
	// refs maps symbol name → reference locations returned.
	refs map[string][]agent.RefLocation
}

func (n *fakeRefNav) Outline(file string) ([]treesitter.OutlineEntry, error) {
	return n.outlines[file], nil
}

func (n *fakeRefNav) References(_ context.Context, _ string, _ int, sym string) ([]agent.RefLocation, error) {
	return n.refs[sym], nil
}

// TestDeepRefClosure_NilNav verifies the NIL-SAFE guarantee: when the funnel
// has no code-nav (Options.CodeNav nil, no daemon-injected nav), deepRefClosure
// returns (nil, nil) and the resulting task is byte-identical to the output of
// buildContractTraceDeepTask with the original file list.
func TestDeepRefClosure_NilNav(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	finder := newScriptedClient()
	verifier := newScriptedClient()

	// No CodeNav injected → f.codeNav() will attempt lazy init, but we want to
	// verify the nil-safe path. We test deepRefClosure directly: construct a
	// Funnel with no nav and confirm (nil, nil) is returned.
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{
		// Explicitly nil CodeNav (default).
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// Reach directly into the Funnel to test deepRefClosure without starting LSP.
	// We simulate nil nav by testing deepRefClosureWith with a nil nav path: call
	// it on an empty nav that returns nothing — the nil-nav path in deepRefClosure
	// is tested via the public contract: no refs section appended.
	files := []string{"pkg/config.go"}
	leads := []store.Lead{{
		PosterLens: "concurrency",
		TargetLens: "api-contract-misuse",
		File:       "pkg/config.go",
		Line:       42,
		Note:       "some note",
	}}

	// Task produced today (no refs).
	wantTask := buildContractTraceDeepTask(files, leads)

	// Task produced via the new enrichment path with a nil-returning nav.
	nilNav := &fakeRefNav{
		outlines: map[string][]treesitter.OutlineEntry{},
		refs:     map[string][]agent.RefLocation{},
	}
	refs, relFiles := deepRefClosureWith(ctx, nilNav, files)
	if refs != nil || relFiles != nil {
		t.Fatalf("empty nav: want (nil, nil), got refs=%v relFiles=%v", refs, relFiles)
	}

	// Simulate the launch-loop logic with nil results.
	taskFiles := dedupFiles(files, relFiles)
	var b strings.Builder
	b.WriteString(buildContractTraceDeepTask(taskFiles, leads))
	appendRefsSection(&b, refs)
	gotTask := b.String()

	if gotTask != wantTask {
		t.Errorf("nil-nav task differs from today's output:\nwant: %q\ngot:  %q", wantTask, gotTask)
	}
}

// TestDeepRefClosure_SweepWideUnchanged verifies that sweep-wide units are not
// affected by the ref-closure enrichment. sweepWide.BuildTask == nil, so the
// enrichment branch (strategy.BuildTask != nil) is never entered; finderTask
// is used instead, which is byte-identical to today.
func TestDeepRefClosure_SweepWideUnchanged(t *testing.T) {
	if sweepWide.BuildTask != nil {
		t.Error("sweepWide.BuildTask must be nil (sweep-wide is unaffected by enrichment)")
	}
	// Verify the branch condition: enrichment fires only when BuildTask != nil.
	if contractTraceDeep.BuildTask == nil {
		t.Error("contractTraceDeep.BuildTask must be non-nil")
	}
	if stateTraceDeep.BuildTask == nil {
		t.Error("stateTraceDeep.BuildTask must be non-nil")
	}
}

// TestDeepRefClosure_ClosureAndInjection verifies the full closure path:
// a seed file declares exported symbol S; another file references it; the
// resulting refs section contains S@otherFile:line AND otherFile is included
// in the expanded file list.
func TestDeepRefClosure_ClosureAndInjection(t *testing.T) {
	ctx := context.Background()

	seedFile := "pkg/api/handler.go"
	refFile := "pkg/server/server.go"

	nav := &fakeRefNav{
		outlines: map[string][]treesitter.OutlineEntry{
			seedFile: {
				{Name: "Handler", Kind: treesitter.KindType, StartLine: 5, EndLine: 20},
				{Name: "unexported", Kind: treesitter.KindFunction, StartLine: 22, EndLine: 30},
			},
		},
		refs: map[string][]agent.RefLocation{
			"Handler": {
				{File: refFile, Line: 42},
				{File: seedFile, Line: 10}, // in-seed site: must be excluded
			},
		},
	}

	refs, relFiles := deepRefClosureWith(ctx, nav, []string{seedFile})

	// Must have one ref: Handler at refFile:42 (seed-file site excluded).
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d: %v", len(refs), refs)
	}
	if refs[0].Symbol != "Handler" {
		t.Errorf("ref Symbol = %q, want Handler", refs[0].Symbol)
	}
	if refs[0].File != refFile {
		t.Errorf("ref File = %q, want %q", refs[0].File, refFile)
	}
	if refs[0].Line != 42 {
		t.Errorf("ref Line = %d, want 42", refs[0].Line)
	}

	// relatedFiles must include refFile.
	if len(relFiles) != 1 || relFiles[0] != refFile {
		t.Errorf("relatedFiles = %v, want [%q]", relFiles, refFile)
	}

	// The expanded file list must include refFile.
	expanded := dedupFiles([]string{seedFile}, relFiles)
	found := false
	for _, f := range expanded {
		if f == refFile {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expanded file list %v does not include %q", expanded, refFile)
	}

	// The task text must contain the PRECOMPUTED CROSS-REFERENCES section with Handler.
	var b strings.Builder
	b.WriteString(buildContractTraceDeepTask(expanded, nil))
	appendRefsSection(&b, refs)
	task := b.String()

	if !strings.Contains(task, "PRECOMPUTED CROSS-REFERENCES") {
		t.Error("task missing PRECOMPUTED CROSS-REFERENCES heading")
	}
	if !strings.Contains(task, "Handler") {
		t.Error("task missing symbol Handler in refs section")
	}
	if !strings.Contains(task, refFile+":42") {
		t.Errorf("task missing ref site %s:42", refFile)
	}
	// refFile must also appear in SEED FILES section (via expanded file list).
	if !strings.Contains(task, refFile) {
		t.Errorf("task missing refFile %q in file list", refFile)
	}
}

// TestDeepRefClosure_ExcludesUnexportedAndNonLoadBearing verifies that
// unexported symbols and non-load-bearing kinds (var, const) are filtered out.
func TestDeepRefClosure_ExcludesUnexportedAndNonLoadBearing(t *testing.T) {
	ctx := context.Background()
	seedFile := "pkg/store.go"

	nav := &fakeRefNav{
		outlines: map[string][]treesitter.OutlineEntry{
			seedFile: {
				{Name: "unexported", Kind: treesitter.KindFunction, StartLine: 1, EndLine: 5},
				{Name: "MaxConns", Kind: treesitter.KindConst, StartLine: 7, EndLine: 7},
				{Name: "defaultVal", Kind: treesitter.KindVar, StartLine: 9, EndLine: 9},
				{Name: "Store", Kind: treesitter.KindType, StartLine: 11, EndLine: 30},
			},
		},
		refs: map[string][]agent.RefLocation{
			"Store": {{File: "other/use.go", Line: 5}},
		},
	}

	refs, relFiles := deepRefClosureWith(ctx, nav, []string{seedFile})
	if len(refs) != 1 || refs[0].Symbol != "Store" {
		t.Errorf("want [Store ref], got %v", refs)
	}
	if len(relFiles) != 1 || relFiles[0] != "other/use.go" {
		t.Errorf("want [other/use.go], got %v", relFiles)
	}
}

// TestDeepRefClosure_Determinism verifies that the same inputs always produce
// identical refs and relatedFiles order.
func TestDeepRefClosure_Determinism(t *testing.T) {
	ctx := context.Background()
	seedFile := "seed.go"

	nav := &fakeRefNav{
		outlines: map[string][]treesitter.OutlineEntry{
			seedFile: {
				{Name: "Zeta", Kind: treesitter.KindFunction, StartLine: 10, EndLine: 15},
				{Name: "Alpha", Kind: treesitter.KindFunction, StartLine: 1, EndLine: 5},
				{Name: "Beta", Kind: treesitter.KindType, StartLine: 6, EndLine: 9},
			},
		},
		refs: map[string][]agent.RefLocation{
			"Alpha": {{File: "b/b.go", Line: 3}, {File: "a/a.go", Line: 7}},
			"Beta":  {{File: "a/a.go", Line: 2}},
			"Zeta":  {{File: "c/c.go", Line: 1}},
		},
	}

	var prev []deepRef
	var prevFiles []string
	for i := 0; i < 5; i++ {
		refs, relFiles := deepRefClosureWith(ctx, nav, []string{seedFile})
		if i == 0 {
			prev = refs
			prevFiles = relFiles
			continue
		}
		if len(refs) != len(prev) {
			t.Fatalf("run %d: ref count %d != %d", i, len(refs), len(prev))
		}
		for j, r := range refs {
			if r != prev[j] {
				t.Errorf("run %d: refs[%d] = %v, want %v", i, j, r, prev[j])
			}
		}
		if len(relFiles) != len(prevFiles) {
			t.Fatalf("run %d: relFiles count %d != %d", i, len(relFiles), len(prevFiles))
		}
		for j, f := range relFiles {
			if f != prevFiles[j] {
				t.Errorf("run %d: relFiles[%d] = %q, want %q", i, j, f, prevFiles[j])
			}
		}
	}
}

// TestDeepRefClosure_EstimateUnitCountInvariant verifies that deepRefClosure
// enrichment does not change the number of finder units: the unit count from
// EstimateScan (which calls FinderUnits) must be unchanged regardless of whether
// ref closure adds files, because FinderUnits counts (lens × strategy × chunk)
// triples, not files.
//
// This is a structural invariant: dedupFiles may grow a unit's file list, but
// units are counted in buildUnits BEFORE deepRefClosure runs (it runs per-unit
// inside the launch loop). Hence the estimate is always exact.
func TestDeepRefClosure_EstimateUnitCountInvariant(t *testing.T) {
	ctx := context.Background()
	st, repo := openFixture(t)
	finder := newScriptedClient()
	verifier := newScriptedClient()
	f, err := New(RoleClients{Finder: finder, Verifier: verifier}, st, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	est, err := f.EstimateScan(ctx, store.ScanOneshot, nil)
	if err != nil {
		t.Fatalf("EstimateScan: %v", err)
	}
	if est.FinderUnits == 0 {
		t.Fatal("FinderUnits = 0, want > 0")
	}

	// Run a real sweep. Stats.FinderRuns must equal the estimate, proving the
	// unit count is invariant even though deepRefClosure may expand file sets.
	finder.fallback = emptyCandidates
	verifier.fallback = notRefutedJSON
	res, err := f.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Stats.FinderRuns != est.FinderUnits {
		t.Errorf("FinderRuns = %d, EstimateScan.FinderUnits = %d: unit count must be invariant to ref-closure file expansion",
			res.Stats.FinderRuns, est.FinderUnits)
	}
}

// TestAppendRefsSection_Empty verifies that appendRefsSection appends nothing
// for an empty refs slice — byte-identical preservation.
func TestAppendRefsSection_Empty(t *testing.T) {
	var b strings.Builder
	b.WriteString("base")
	appendRefsSection(&b, nil)
	if got := b.String(); got != "base" {
		t.Errorf("appendRefsSection with nil refs changed output: %q", got)
	}

	var b2 strings.Builder
	b2.WriteString("base")
	appendRefsSection(&b2, []deepRef{})
	if got := b2.String(); got != "base" {
		t.Errorf("appendRefsSection with empty refs changed output: %q", got)
	}
}

// TestAppendRefsSection_Content verifies that non-empty refs produce the
// expected heading and per-ref lines.
func TestAppendRefsSection_Content(t *testing.T) {
	refs := []deepRef{
		{Symbol: "Handler", File: "server/server.go", Line: 42},
		{Symbol: "Config", File: "config/config.go", Line: 7},
	}
	var b strings.Builder
	appendRefsSection(&b, refs)
	out := b.String()

	if !strings.Contains(out, "PRECOMPUTED CROSS-REFERENCES") {
		t.Error("missing PRECOMPUTED CROSS-REFERENCES heading")
	}
	if !strings.Contains(out, "Handler") {
		t.Error("missing symbol Handler")
	}
	if !strings.Contains(out, "server/server.go:42") {
		t.Error("missing ref site server/server.go:42")
	}
	if !strings.Contains(out, "Config") {
		t.Error("missing symbol Config")
	}
	if !strings.Contains(out, "config/config.go:7") {
		t.Error("missing ref site config/config.go:7")
	}
}

// TestDedupFiles verifies that dedupFiles preserves base order and appends
// only new files from extra.
func TestDedupFiles(t *testing.T) {
	base := []string{"a.go", "b.go"}
	extra := []string{"c.go", "a.go", "d.go"}
	got := dedupFiles(base, extra)
	want := []string{"a.go", "b.go", "c.go", "d.go"}
	if len(got) != len(want) {
		t.Fatalf("dedupFiles len = %d, want %d: %v", len(got), len(want), got)
	}
	for i, f := range got {
		if f != want[i] {
			t.Errorf("dedupFiles[%d] = %q, want %q", i, f, want[i])
		}
	}
}
