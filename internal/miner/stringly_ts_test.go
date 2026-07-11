package miner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
	"github.com/dpoage/bugbot/internal/store"
)

// loadTSHandleForTest loads the TypeScript tree-sitter language handle.
func loadTSHandleForTest(t *testing.T) *tsLangHandle {
	t.Helper()
	h, err := loadTSLangHandle("x.ts")
	if err != nil {
		t.Skipf("TypeScript grammar unavailable: %v", err)
	}
	return h
}

// ─── passTS_UnionTypes ─────────────────────────────────────────────────────────

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
	for _, w := range []string{"active", "inactive", "pending"} {
		if !u.members[w] {
			t.Errorf("expected member %q in union, members: %v", w, u.members)
		}
	}
}

func TestPassTSUnionTypes_MixedNumericLiteralExcluded(t *testing.T) {
	h := loadTSHandleForTest(t)
	// literal number in a literal_type — structural whitelist must exclude.
	src := []byte(`type Mixed = 'hello' | 42;`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	for _, u := range unions {
		if u.name == "Mixed" {
			t.Errorf("mixed string|number union must be excluded, got %+v", u)
		}
	}
}

// D2 adversarial: predefined_type keyword (number, boolean) as direct union branch.
func TestPassTSUnionTypes_PredefinedTypeExcluded(t *testing.T) {
	h := loadTSHandleForTest(t)
	// 'number' is a predefined_type node, not a literal_type — structural whitelist.
	src := []byte(`type Mixed2 = 'read' | 'write' | number;`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	for _, u := range unions {
		if u.name == "Mixed2" {
			t.Errorf("string|predefined_type union must be excluded, got %+v", u)
		}
	}
}

// D2 adversarial: object_type as union branch.
func TestPassTSUnionTypes_ObjectTypeExcluded(t *testing.T) {
	h := loadTSHandleForTest(t)
	src := []byte(`type Mixed3 = 'a' | 'b' | { custom: string };`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	for _, u := range unions {
		if u.name == "Mixed3" {
			t.Errorf("string|object_type union must be excluded, got %+v", u)
		}
	}
}

// D3 adversarial: object property types inside a type alias must NOT pollute
// the union member set. type Config = { mode: 'a' | 'b'; other: 'c' | 'd' }
// — this is an object_type, not a union alias; the miner must produce 0 unions.
func TestPassTSUnionTypes_ObjectPropertyTypesNotPolluted(t *testing.T) {
	h := loadTSHandleForTest(t)
	src := []byte(`type Config = { mode: 'a' | 'b'; other: 'c' | 'd' };`)
	tree, err := parseTSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	unions := passTS_UnionTypes(h, tree, src)
	if len(unions) != 0 {
		t.Errorf("object-type alias should produce 0 unions, got %+v", unions)
	}
}

func TestPassTSUnionTypes_SingleMemberExcluded(t *testing.T) {
	h := loadTSHandleForTest(t)
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

// ─── passTS_SwitchCases ────────────────────────────────────────────────────────

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

// ─── joinTSDrift ──────────────────────────────────────────────────────────────

// makeTypedBinding is a helper that creates a typed union binding.
func makeTypedBinding(name, typeName string, scopeStart, scopeEnd uint32) tsBinding {
	return tsBinding{
		name:         name,
		typeName:     typeName,
		scopeStart:   scopeStart,
		scopeEnd:     scopeEnd,
		isTypedUnion: true,
	}
}

// makeUntypedBinding is a helper that creates an untyped binding (shadow).
func makeUntypedBinding(name string, scopeStart, scopeEnd uint32) tsBinding {
	return tsBinding{
		name:         name,
		scopeStart:   scopeStart,
		scopeEnd:     scopeEnd,
		isTypedUnion: false,
	}
}

func TestJoinTSDrift_TypeA_TypoLiteral(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Status",
		members: map[string]bool{"active": true, "inactive": true, "pending": true},
		line:    1,
	}}
	bindings := []tsBinding{makeTypedBinding("s", "Status", 0, 1000)}
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

	typeA, typeB := joinTSDrift("test.ts", unions, bindings, switches)
	if len(typeA) != 1 {
		t.Fatalf("expected 1 type-A lead, got %d: %+v", len(typeA), typeA)
	}
	if typeA[0].Line != 6 {
		t.Errorf("expected type-A lead at line 6, got %d", typeA[0].Line)
	}
	// 'active' is not covered by any case (typo 'activ' != 'active'),
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
	bindings := []tsBinding{makeTypedBinding("d", "Dir", 0, 1000)}
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

	typeA, typeB := joinTSDrift("test.ts", unions, bindings, switches)
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
	bindings := []tsBinding{makeTypedBinding("lvl", "Level", 0, 1000)}
	switches := []tsSwitchInfo{{
		scrutinee:  "lvl",
		switchByte: 50,
		switchLine: 5,
		hasDefault: true,
		cases: []tsCaseLit{
			{value: "warn", line: 6},
			{value: "error", line: 7},
		},
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, bindings, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("expected 0 leads with default clause, got typeA=%d typeB=%d", len(typeA), len(typeB))
	}
}

func TestJoinTSDrift_NoBindingProducesNoLeads(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Status",
		members: map[string]bool{"active": true, "inactive": true},
		line:    1,
	}}
	var bindings []tsBinding // no bindings at all
	switches := []tsSwitchInfo{{
		scrutinee:  "s",
		switchByte: 50,
		switchLine: 5,
		hasDefault: false,
		cases:      []tsCaseLit{{value: "activ", line: 6}},
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, bindings, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("expected 0 leads without type binding, got typeA=%d typeB=%d", len(typeA), len(typeB))
	}
}

// D1 adversarial: untyped inner binding shadows outer typed union param.
// The nearest binding (inner, untyped) must win → 0 leads.
func TestJoinTSDrift_UntypedInnerShadowProducesNoLeads(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Event",
		members: map[string]bool{"click": true, "hover": true},
		line:    1,
	}}
	// Outer typed binding: funcStart=0, funcEnd=1000.
	// Inner untyped binding: narrower scope, same name "event".
	bindings := []tsBinding{
		makeTypedBinding("event", "Event", 0, 1000), // outer typed
		makeUntypedBinding("event", 200, 800),       // inner untyped — nearer to switch
	}
	switches := []tsSwitchInfo{{
		scrutinee:  "event",
		switchByte: 400, // inside the inner untyped binding's scope
		switchLine: 20,
		hasDefault: false,
		cases:      []tsCaseLit{{value: "clickk", line: 21}}, // typo
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, bindings, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("inner untyped shadow must suppress leads, got typeA=%d typeB=%d", len(typeA), len(typeB))
	}
}

// D1 adversarial: block-scoped const shadow of outer typed param → 0 leads.
func TestJoinTSDrift_BlockConstShadowProducesNoLeads(t *testing.T) {
	unions := []tsUnionType{{
		name:    "Mode",
		members: map[string]bool{"a": true, "b": true},
		line:    1,
	}}
	// Outer typed binding; inner block-scoped untyped const.
	bindings := []tsBinding{
		makeTypedBinding("mode", "Mode", 0, 1000),
		makeUntypedBinding("mode", 300, 700), // block const shadows the param
	}
	switches := []tsSwitchInfo{{
		scrutinee:  "mode",
		switchByte: 450,
		switchLine: 15,
		hasDefault: false,
		cases:      []tsCaseLit{{value: "c", line: 16}}, // not in union
	}}

	typeA, typeB := joinTSDrift("test.ts", unions, bindings, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("block-const shadow must suppress leads, got typeA=%d typeB=%d", len(typeA), len(typeB))
	}
}

// ─── fixture-based integration tests ──────────────────────────────────────────

// buildTSSnapshot builds a snapshot with the given relative paths under root.
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
	return &ingest.Snapshot{Commit: "test", Root: root, Files: files}
}

// runTSDriftFixture runs the TS drift miner over the given fixtures and returns
// the lead count and all posted leads.
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
		t.Fatalf("st.ListLeads: %v", err)
	}
	return sum.StringlyTSDriftLeads, leads
}

// TestStringlyTSDrift_PositiveTypoCase: typo_case.ts has 'activ' (typo) in a
// switch on a Status parameter. The switch has a default clause so only the
// type-A lead fires. Asserts file, line, and lens values.
func TestStringlyTSDrift_PositiveTypoCase(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_drift")
	n, leads := runTSDriftFixture(t, dir, []string{"typo_case.ts"})
	if n != 1 {
		t.Errorf("expected 1 StringlyTSDriftLeads, got %d", n)
	}
	if len(leads) != 1 {
		t.Fatalf("expected 1 lead, got %d: %+v", len(leads), leads)
	}
	lead := leads[0]
	if lead.PosterLens != stringlyTSPosterLens {
		t.Errorf("PosterLens: want %q, got %q", stringlyTSPosterLens, lead.PosterLens)
	}
	if lead.TargetLens != stringlyTSTargetLens {
		t.Errorf("TargetLens: want %q, got %q", stringlyTSTargetLens, lead.TargetLens)
	}
	if lead.File != "typo_case.ts" {
		t.Errorf("File: want %q, got %q", "typo_case.ts", lead.File)
	}
	// The case 'activ' is on line 8 in the fixture file.
	if lead.Line != 8 {
		t.Errorf("Line: want 8 (case 'activ' line), got %d", lead.Line)
	}
	if !strings.Contains(lead.Note, "activ") {
		t.Errorf("Note should mention typo literal 'activ', got: %q", lead.Note)
	}
}

// TestStringlyTSDrift_PositiveMissingArm: missing_arm.ts is missing 'west'
// from Direction switch with no default → 1 type-B lead.
func TestStringlyTSDrift_PositiveMissingArm(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_drift")
	n, leads := runTSDriftFixture(t, dir, []string{"missing_arm.ts"})
	if n != 1 {
		t.Errorf("expected 1 StringlyTSDriftLeads, got %d", n)
	}
	if len(leads) != 1 {
		t.Fatalf("expected 1 lead, got %d: %+v", len(leads), leads)
	}
	lead := leads[0]
	if lead.File != "missing_arm.ts" {
		t.Errorf("File: want %q, got %q", "missing_arm.ts", lead.File)
	}
	// The switch is on line 8 in the fixture.
	if lead.Line != 8 {
		t.Errorf("Line: want 8 (switch line), got %d", lead.Line)
	}
	if !strings.Contains(lead.Note, "west") {
		t.Errorf("Note should mention missing member 'west', got: %q", lead.Note)
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

// TestStringlyTSDrift_NegativeUntypedScrutinee: plain string param → 0 leads.
func TestStringlyTSDrift_NegativeUntypedScrutinee(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"untyped_scrutinee.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for untyped scrutinee, got %d", n)
	}
}

// TestStringlyTSDrift_NegativeDiscriminatedUnion: exhaustive shape union → 0 leads.
func TestStringlyTSDrift_NegativeDiscriminatedUnion(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"discriminated_union.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads on discriminated union, got %d", n)
	}
}

// D1 adversarial fixture: untyped forEach shadow → 0 leads.
func TestStringlyTSDrift_NegativeUntypedForEachShadow(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"untyped_foreach_shadow.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for untyped forEach shadow, got %d", n)
	}
}

// D1 adversarial fixture: block-scoped const shadow → 0 leads.
func TestStringlyTSDrift_NegativeBlockConstShadow(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"block_const_shadow.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for block-const shadow, got %d", n)
	}
}

// D2 adversarial fixture: string|number (predefined_type) union → 0 leads.
func TestStringlyTSDrift_NegativePredefinedTypeMixedUnion(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"predefined_type_union.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for string|number union, got %d", n)
	}
}

// D2/D3 adversarial fixture: object property types inside type alias → 0 leads.
func TestStringlyTSDrift_NegativeObjectPropertyTypePollution(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"object_property_pollution.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for object-property union pollution, got %d", n)
	}
}

// D4: test-file paths must be skipped by isTSTestPath gate.
func TestStringlyTSDrift_TestFileSkipped(t *testing.T) {
	// Build a snapshot that includes a .test.ts file that would produce a lead
	// if scanned, but must be silently skipped.
	dir := filepath.Join("testdata", "stringly_ts_drift")
	// Use typo_case.ts as the content but route it through a test-looking path.
	snap := &ingest.Snapshot{
		Commit: "test",
		Root:   dir,
		Files: []ingest.File{
			{
				Path:     "typo_case.test.ts", // test file path
				Language: ingest.LangTypeScript,
				Size:     1,
			},
		},
	}
	st := openStore(t)
	var sum Summary
	if err := seedStringlyTSDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyTSDrift: %v", err)
	}
	if sum.StringlyTSDriftLeads != 0 {
		t.Errorf("test file must be skipped, got %d leads", sum.StringlyTSDriftLeads)
	}
}

// TestStringlyTSDrift_NegativeParenlessArrowShadow: paren-less arrow param shadows
// outer typed param — round-2 oracle repro (FP before fix, 0 after). Expected: 0 leads.
func TestStringlyTSDrift_NegativeParenlessArrowShadow(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	n, _ := runTSDriftFixture(t, dir, []string{"parenless_arrow_shadow.ts"})
	if n != 0 {
		t.Errorf("expected 0 leads for paren-less arrow shadow, got %d", n)
	}
}

// TestStringlyTSDrift_NegativeCleanCorpus: all clean fixtures together → 0 leads.
func TestStringlyTSDrift_NegativeCleanCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_ts_clean")
	rels := []string{
		"exhaustive_switch.ts",
		"default_suppresses_type_b.ts",
		"mixed_union_excluded.ts",
		"untyped_scrutinee.ts",
		"discriminated_union.ts",
		"untyped_foreach_shadow.ts",
		"block_const_shadow.ts",
		"predefined_type_union.ts",
		"object_property_pollution.ts",
		"parenless_arrow_shadow.ts",
	}
	n, leads := runTSDriftFixture(t, dir, rels)
	if n != 0 || len(leads) != 0 {
		t.Errorf("clean corpus: expected 0 leads, got StringlyTSDriftLeads=%d, leads=%+v", n, leads)
	}
}

// ─── HasError parse-failure gate ─────────────────────────────────────────────

// TestStringlyTSDrift_ParseErrorFileSkipped verifies that files producing
// HasError()=true trees are counted in TSParseFailures and produce 0 leads.
// We exercise this by writing a snippet that the gotreesitter TS grammar v0.20.2
// cannot parse (typed-param arrow function) into a temp snapshot.
func TestStringlyTSDrift_ParseErrorFileSkipped(t *testing.T) {
	h := loadTSHandleForTest(t)
	// Confirm this snippet actually produces a parse error.
	src := []byte("const f = (x: string) => x;")
	tree, err := parseTSFile(h, src)
	if err != nil || tree == nil {
		t.Skip("parseTSFile returned err/nil; cannot exercise HasError path")
	}
	defer tree.Release()
	if !tree.RootNode().HasError() {
		t.Skip("grammar now parses typed arrows without error; HasError guard still safe to skip")
	}
	// Now run the full miner over this file (must be written to disk).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "typed_arrow.ts"), src, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	snap := &ingest.Snapshot{
		Commit: "test",
		Root:   dir,
		Files:  []ingest.File{{Path: "typed_arrow.ts", Language: ingest.LangTypeScript, Size: int64(len(src))}},
	}
	st := openStore(t)
	var sum Summary
	if err := seedStringlyTSDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyTSDrift: %v", err)
	}
	if sum.TSParseFailures != 1 {
		t.Errorf("expected TSParseFailures=1, got %d", sum.TSParseFailures)
	}
	if sum.StringlyTSDriftLeads != 0 {
		t.Errorf("expected 0 leads on parse-error file, got %d", sum.StringlyTSDriftLeads)
	}
}

// ─── isTSTestPath unit tests ──────────────────────────────────────────────────

func TestIsTSTestPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"src/auth/login.ts", false},
		{"src/auth/login.test.ts", true},
		{"src/auth/login.spec.tsx", true},
		{"src/auth/login.TEST.ts", true}, // lowercased
		{"__tests__/auth.ts", true},
		{"test/auth.ts", true},
		{"tests/auth.ts", true},
		{"src/__tests__/auth.ts", true},
		{"src/testdata/fixture.ts", true},
		{"src/auth/testhelper.ts", false}, // 'test' as prefix, not segment
		{"src/auth/testing.ts", false},
	}
	for _, tc := range cases {
		got := isTSTestPath(tc.path)
		if got != tc.want {
			t.Errorf("isTSTestPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
