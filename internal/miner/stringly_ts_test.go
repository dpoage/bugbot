package miner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// loadTSHandle loads the TypeScript tree-sitter language handle once per test
// binary run. A nil return means the grammar is unavailable (degrade gracefully).
func loadTSHandleForTest(t *testing.T) *tsLangHandle {
	t.Helper()
	h, err := loadTSLangHandle("x.ts")
	if err != nil {
		t.Skipf("TypeScript grammar unavailable: %v", err)
	}
	return h
}

// parseSrcForTest is a helper that parses a TypeScript source snippet.
func parseSrcForTest(t *testing.T, h *tsLangHandle, src []byte) interface{ Release() } {
	t.Helper()
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parseTSFile: %v", err)
	}
	return tree
}

// ─── passTS_UnionTypes ────────────────────────────────────────────────────────

func TestPassTSUnionTypes_BasicUnion(t *testing.T) {
	h := loadTSHandleForTest(t)
	src := []byte(`type Status = 'active' | 'inactive' | 'pending';`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	if len(unions) != 1 {
		t.Fatalf("expected 1 union, got %d: %+v", len(unions), unions)
	}
	u := unions[0]
	if u.name != "Status" {
		t.Errorf("expected type name Status, got %q", u.name)
	}
	want := []string{"active", "inactive", "pending"}
	for _, w := range want {
		if !u.members[w] {
			t.Errorf("expected member %q in union, members: %v", w, u.members)
		}
	}
}

func TestPassTSUnionTypes_MixedUnionExcluded(t *testing.T) {
	h := loadTSHandleForTest(t)
	// A union with a number member must be excluded.
	src := []byte(`type Mixed = 'hello' | 42;`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	for _, u := range unions {
		if u.name == "Mixed" {
			t.Errorf("mixed union should be excluded, got %+v", u)
		}
	}
}

func TestPassTSUnionTypes_SingleMemberExcluded(t *testing.T) {
	h := loadTSHandleForTest(t)
	// A single-member type alias is not an enum-style union.
	src := []byte(`type Only = 'singleton';`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	if len(unions) != 0 {
		t.Errorf("single-member union should be excluded, got %+v", unions)
	}
}

func TestPassTSUnionTypes_DoubleQuotes(t *testing.T) {
	h := loadTSHandleForTest(t)
	src := []byte(`type Dir = "north" | "south" | "east";`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	if len(unions) != 1 {
		t.Fatalf("expected 1 union, got %d", len(unions))
	}
	for _, want := range []string{"north", "south", "east"} {
		if !unions[0].members[want] {
			t.Errorf("expected member %q", want)
		}
	}
}

// ─── passTS_SwitchCases ────────────────────────────────────────────────────

func TestPassTSSwitchCases_BasicSwitch(t *testing.T) {
	h := loadTSHandleForTest(t)
	src := []byte(`
type Status = 'active' | 'inactive';
function handle(s: Status): string {
  switch (s) {
    case 'active': return 'ok';
    case 'inactive': return 'off';
  }
  return '';
}
`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	switches := passTS_SwitchCases(h, tree, src)
	if len(switches) != 1 {
		t.Fatalf("expected 1 switch, got %d: %+v", len(switches), switches)
	}
	sw := switches[0]
	if sw.scrutinee != "s" {
		t.Errorf("expected scrutinee 's', got %q", sw.scrutinee)
	}
	if sw.hasDefault {
		t.Error("expected no default clause")
	}
	got := map[string]bool{}
	for _, c := range sw.cases {
		got[c.value] = true
	}
	for _, want := range []string{"active", "inactive"} {
		if !got[want] {
			t.Errorf("expected case literal %q in switch cases", want)
		}
	}
}

func TestPassTSSwitchCases_DefaultDetected(t *testing.T) {
	h := loadTSHandleForTest(t)
	src := []byte(`
type Level = 'low' | 'high';
function check(level: Level): boolean {
  switch (level) {
    case 'low': return false;
    default: return true;
  }
}
`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	switches := passTS_SwitchCases(h, tree, src)
	// The switch has a case 'low' plus a default.
	found := false
	for _, sw := range switches {
		if sw.scrutinee == "level" {
			found = true
			if !sw.hasDefault {
				t.Error("expected hasDefault=true")
			}
		}
	}
	if !found {
		t.Error("switch on 'level' not found")
	}
}

// ─── joinTSDrift ─────────────────────────────────────────────────────────────

func TestJoinTSDrift_TypeA_TypoLiteral(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Status",
		members: map[string]bool{"active": true, "inactive": true, "pending": true},
		line:    1,
	}}
	params := []tsFuncParam{{
		paramName: "s",
		typeName:  "Status",
		funcStart: 0,
		funcEnd:   1000,
	}}
	switches := []tsSwitchInfo{{
		scrutinee:  "s",
		switchByte: 50,
		switchLine: 5,
		hasDefault: false,
		cases: []tsCaseLit{
			{value: "activ", line: 6},    // typo
			{value: "inactive", line: 7}, // OK
			{value: "pending", line: 8},  // OK
		},
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, params, switches)
	if len(typeA) != 1 {
		t.Fatalf("expected 1 type-A lead, got %d: %+v", len(typeA), typeA)
	}
	if typeA[0].Line != 6 {
		t.Errorf("expected type-A lead at line 6, got %d", typeA[0].Line)
	}
	// 'active' is not covered by any case (the typo 'activ' != 'active'),
	// so type-B also fires for the missing 'active' arm — correct behavior.
	if len(typeB) != 1 {
		t.Errorf("expected 1 type-B lead (missing 'active'), got %d: %+v", len(typeB), typeB)
	}
}

func TestJoinTSDrift_TypeB_MissingArm(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Dir",
		members: map[string]bool{"north": true, "south": true, "east": true, "west": true},
		line:    1,
	}}
	params := []tsFuncParam{{
		paramName: "d",
		typeName:  "Dir",
		funcStart: 0,
		funcEnd:   1000,
	}}
	switches := []tsSwitchInfo{{
		scrutinee:  "d",
		switchByte: 50,
		switchLine: 5,
		hasDefault: false,
		cases: []tsCaseLit{
			{value: "north", line: 6},
			{value: "south", line: 7},
			{value: "east", line: 8},
			// 'west' missing
		},
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, params, switches)
	if len(typeA) != 0 {
		t.Errorf("expected 0 type-A leads, got %d: %+v", len(typeA), typeA)
	}
	if len(typeB) != 1 {
		t.Fatalf("expected 1 type-B lead, got %d: %+v", len(typeB), typeB)
	}
}

func TestJoinTSDrift_DefaultSuppressesTypeB(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Level",
		members: map[string]bool{"debug": true, "info": true, "warn": true, "error": true},
		line:    1,
	}}
	params := []tsFuncParam{{
		paramName: "lvl",
		typeName:  "Level",
		funcStart: 0,
		funcEnd:   1000,
	}}
	switches := []tsSwitchInfo{{
		scrutinee:  "lvl",
		switchByte: 50,
		switchLine: 5,
		hasDefault: true, // default clause present
		cases: []tsCaseLit{
			{value: "warn", line: 6},
			{value: "error", line: 7},
		},
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, params, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("expected 0 leads with default clause, got typeA=%d typeB=%d", len(typeA), len(typeB))
	}
}

func TestJoinTSDrift_UntypedScrutineeProducesNoLeads(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Status",
		members: map[string]bool{"active": true, "inactive": true},
		line:    1,
	}}
	// No params — scrutinee has no type binding.
	var params []tsFuncParam
	switches := []tsSwitchInfo{{
		scrutinee:  "s",
		switchByte: 50,
		switchLine: 5,
		hasDefault: false,
		cases: []tsCaseLit{
			{value: "activ", line: 6}, // typo but no binding
		},
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, params, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("expected 0 leads without type binding, got typeA=%d typeB=%d", len(typeA), len(typeB))
	}
}

// ─── fixture-based integration tests ─────────────────────────────────────────

// buildTSSnapshot builds a snapshot with TypeScript language for the given
// relative paths under root.
func buildTSSnapshot(t *testing.T, root string, rels []string) *ingest.Snapshot {
	t.Helper()
	files := make([]ingest.File, 0, len(rels))
	for _, rel := range rels {
		abs := filepath.Join(root, rel)
		fi, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("stat %s: %v", abs, err)
		}
		files = append(files, ingest.File{
			Path:     filepath.ToSlash(rel),
			Language: ingest.DetectLanguage(rel),
			Size:     fi.Size(),
		})
	}
	return &ingest.Snapshot{
		Commit: "test",
		Root:   root,
		Files:  files,
	}
}

// collectLeads collects all leads posted to the store during seedStringlyTSDrift.
func runTSDriftFixture(t *testing.T, root string, rels []string) (int, []store.Lead) {
	t.Helper()
	snap := buildTSSnapshot(t, root, rels)
	st := openStore(t)
	var sum Summary
	ctx := context.Background()
	if err := seedStringlyTSDrift(ctx, snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyTSDrift: %v", err)
	}
	leads, err := st.ListLeads(ctx)
	if err != nil {
		t.Fatalf("st.Leads: %v", err)
	}
	return sum.StringlyDriftLeads, leads
}

// TestStringlyTSDrift_PositiveTypoCase: typo_case.ts has a typo 'activ' in a
// switch on a Status parameter. The switch has a default clause covering the
// missing-arm concern; only the type-A typo lead should fire.
func TestStringlyTSDrift_PositiveTypoCase(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_drift")
	n, leads := runTSDriftFixture(t, dir, []string{"typo_case.ts"})
	if n != 1 {
		t.Errorf("expected 1 StringlyDriftLeads, got %d", n)
	}
	if len(leads) != 1 {
		t.Fatalf("expected 1 lead, got %d: %+v", len(leads), leads)
	}
	lead := leads[0]
	if lead.PosterLens != stringlyTSPosterLens {
		t.Errorf("expected PosterLens %q, got %q", stringlyTSPosterLens, lead.PosterLens)
	}
	if lead.TargetLens != stringlyTSTargetLens {
		t.Errorf("expected TargetLens %q, got %q", stringlyTSTargetLens, lead.TargetLens)
	}
}

// TestStringlyTSDrift_PositiveMissingArm: missing_arm.ts has 'west' missing
// from a switch over Direction with no default → 1 type-B lead.
func TestStringlyTSDrift_PositiveMissingArm(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_drift")
	n, leads := runTSDriftFixture(t, dir, []string{"missing_arm.ts"})
	if n != 1 {
		t.Errorf("expected 1 StringlyDriftLeads, got %d", n)
	}
	if len(leads) != 1 {
		t.Fatalf("expected 1 lead, got %d: %+v", len(leads), leads)
	}
}

// TestStringlyTSDrift_NegativeExhaustiveSwitch: all members covered, 0 leads.
func TestStringlyTSDrift_NegativeExhaustiveSwitch(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"exhaustive_switch.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads on exhaustive switch, got %d", n)
	}
}

// TestStringlyTSDrift_NegativeDefaultSuppressesTypeB: explicit-subset + default → 0 leads.
func TestStringlyTSDrift_NegativeDefaultSuppressesTypeB(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"default_suppresses_type_b.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads when default suppresses type-B, got %d", n)
	}
}

// TestStringlyTSDrift_NegativeMixedUnion: mixed string|number union → 0 leads.
func TestStringlyTSDrift_NegativeMixedUnion(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"mixed_union_excluded.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for mixed union, got %d", n)
	}
}

// TestStringlyTSDrift_NegativeUntypedScrutinee: switch over plain string param → 0 leads.
func TestStringlyTSDrift_NegativeUntypedScrutinee(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"untyped_scrutinee.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for untyped scrutinee, got %d", n)
	}
}

// TestStringlyTSDrift_NegativeDiscriminatedUnion: all members of a shape union covered → 0 leads.
func TestStringlyTSDrift_NegativeDiscriminatedUnion(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"discriminated_union.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads on discriminated union, got %d", n)
	}
}

// TestStringlyTSDrift_NonTSFileSkipped: Go file renamed .ts should not match
// TS grammar but ingest.DetectLanguage on .ts returns LangTypeScript;
// if the file contains valid TS syntax (no union types), 0 leads result.
func TestStringlyTSDrift_NegativeCleanCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	// Run all clean fixtures at once — total leads must be 0.
	rels := []string{
		"exhaustive_switch.ts",
		"default_suppresses_type_b.ts",
		"mixed_union_excluded.ts",
		"untyped_scrutinee.ts",
		"discriminated_union.ts",
	}
	n, leads := runTSDriftFixture(t, dir, rels)
	if n != 0 || len(leads) != 0 {
		t.Errorf("clean corpus: expected 0 leads, got StringlyDriftLeads=%d, leads=%+v", n, leads)
	}
}
