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

// loadRSHandleForTest loads the Rust tree-sitter language handle.
func loadRSHandleForTest(t *testing.T) *rsLangHandle {
	t.Helper()
	h, err := loadRSLangHandle()
	if err != nil {
		t.Skipf("Rust grammar unavailable: %v", err)
	}
	return h
}

// ─── passRS_Consts ─────────────────────────────────────────────────────────────

func TestPassRSConsts_BasicStrConst(t *testing.T) {
	h := loadRSHandleForTest(t)
	src := []byte(`
const STATUS_ACTIVE: &str = "active";
const STATUS_INACTIVE: &str = "inactive";
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	consts := passRS_Consts(h, tree, src)
	if len(consts) != 2 {
		t.Fatalf("expected 2 consts, got %d: %+v", len(consts), consts)
	}
	vals := map[string]bool{}
	for _, c := range consts {
		vals[c.value] = true
	}
	for _, want := range []string{"active", "inactive"} {
		if !vals[want] {
			t.Errorf("expected const value %q, got %v", want, vals)
		}
	}
}

func TestPassRSConsts_RawStringConst(t *testing.T) {
	h := loadRSHandleForTest(t)
	src := []byte(`
const PATH: &str = r"hello/world";
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	consts := passRS_Consts(h, tree, src)
	if len(consts) != 1 {
		t.Fatalf("expected 1 const, got %d: %+v", len(consts), consts)
	}
	if consts[0].value != "hello/world" {
		t.Errorf("expected value %q, got %q", "hello/world", consts[0].value)
	}
}

func TestPassRSConsts_NonStrConstExcluded(t *testing.T) {
	h := loadRSHandleForTest(t)
	// const with non-reference type should not be collected.
	src := []byte(`
const MAX: u32 = 42;
const NAME: String = "ignored".to_string();
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	consts := passRS_Consts(h, tree, src)
	if len(consts) != 0 {
		t.Errorf("expected 0 consts for non-&str types, got %d: %+v", len(consts), consts)
	}
}

func TestPassRSConsts_NestedConstExcluded(t *testing.T) {
	h := loadRSHandleForTest(t)
	// const inside a function must not be treated as a top-level producer.
	src := []byte(`
fn foo() {
    const LOCAL: &str = "local";
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	consts := passRS_Consts(h, tree, src)
	if len(consts) != 0 {
		t.Errorf("expected 0 consts for function-local const, got %d: %+v", len(consts), consts)
	}
}

// ─── passRS_Bindings ───────────────────────────────────────────────────────────

func TestPassRSBindings_StrParam(t *testing.T) {
	h := loadRSHandleForTest(t)
	src := []byte(`
fn dispatch(status: &str) {
    match status { "a" => {} _ => {} }
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	bindings := passRS_Bindings(h, tree, src)
	var found *rsBinding
	for i := range bindings {
		if bindings[i].name == "status" && bindings[i].isTypedStr {
			found = &bindings[i]
		}
	}
	if found == nil {
		t.Fatalf("expected typed &str binding for 'status', got %+v", bindings)
	}
}

func TestPassRSBindings_ClosureParamIsSentinel(t *testing.T) {
	h := loadRSHandleForTest(t)
	src := []byte(`
fn dispatch(status: &str) {
    let v: Vec<&str> = vec![];
    v.iter().for_each(|status| {
        match status { "a" => {} _ => {} }
    });
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	bindings := passRS_Bindings(h, tree, src)
	// There should be a typed binding for `status` (the fn param) AND
	// an untyped sentinel for `status` (the closure param).
	var hasTyped, hasSentinel bool
	for _, b := range bindings {
		if b.name == "status" {
			if b.isTypedStr {
				hasTyped = true
			} else {
				hasSentinel = true
			}
		}
	}
	if !hasTyped {
		t.Error("expected typed &str binding for 'status' fn param")
	}
	if !hasSentinel {
		t.Error("expected untyped sentinel binding for closure param 'status'")
	}
}

func TestPassRSBindings_LetShadowIsSentinel(t *testing.T) {
	h := loadRSHandleForTest(t)
	src := []byte(`
fn dispatch(s: &str) {
    let s = s.trim();
    match s { "a" => {} _ => {} }
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	bindings := passRS_Bindings(h, tree, src)
	var hasSentinel bool
	for _, b := range bindings {
		if b.name == "s" && !b.isTypedStr {
			hasSentinel = true
		}
	}
	if !hasSentinel {
		t.Error("expected untyped sentinel for let-shadowed 's'")
	}
}

// ─── passRS_MatchExprs ─────────────────────────────────────────────────────────

func TestPassRSMatchExprs_BasicMatch(t *testing.T) {
	h := loadRSHandleForTest(t)
	src := []byte(`
fn dispatch(status: &str) {
    match status {
        "active" => {},
        "inactive" => {},
        _ => {}
    }
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	matches := passRS_MatchExprs(h, tree, src)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d: %+v", len(matches), matches)
	}
	m := matches[0]
	if m.scrutinee != "status" {
		t.Errorf("expected scrutinee 'status', got %q", m.scrutinee)
	}
	if len(m.arms) != 2 {
		t.Errorf("expected 2 string arms, got %d: %+v", len(m.arms), m.arms)
	}
}

func TestPassRSMatchExprs_MacroBodyExcluded(t *testing.T) {
	h := loadRSHandleForTest(t)
	// match inside macro_rules! body must not be captured.
	src := []byte(`
macro_rules! my_match {
    ($x:expr) => {
        match $x {
            "foo" => 1,
            _ => 0,
        }
    }
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	matches := passRS_MatchExprs(h, tree, src)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (macro body excluded), got %d: %+v", len(matches), matches)
	}
}

func TestPassRSMatchExprs_NonIdentScrutineeExcluded(t *testing.T) {
	h := loadRSHandleForTest(t)
	// match val.as_str() — scrutinee is a call_expression, not a bare identifier.
	src := []byte(`
fn dispatch(val: String) {
    match val.as_str() {
        "a" => {},
        _ => {}
    }
}
`)
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	matches := passRS_MatchExprs(h, tree, src)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches (non-identifier scrutinee excluded), got %d: %+v", len(matches), matches)
	}
}

// ─── joinRSDrift ──────────────────────────────────────────────────────────────

func TestJoinRSDrift_TypeA(t *testing.T) {
	consts := []rsConstStr{
		{name: "STATUS_ACTIVE", value: "active"},
		{name: "STATUS_INACTIVE", value: "inactive"},
	}
	bindings := []rsBinding{
		{name: "status", scopeStart: 0, scopeEnd: 1000, isTypedStr: true},
	}
	matches := []rsMatchInfo{
		{
			scrutinee: "status",
			exprStart: 100,
			exprEnd:   200,
			exprLine:  10,
			arms: []rsArmLit{
				{value: "active", line: 11},
				{value: "inactve", line: 12}, // typo
			},
		},
	}
	leads := joinRSDrift("src/lib.rs", consts, bindings, matches)
	if len(leads) != 1 {
		t.Fatalf("expected 1 lead, got %d: %+v", len(leads), leads)
	}
	if leads[0].Line != 12 {
		t.Errorf("expected lead on line 12, got %d", leads[0].Line)
	}
	if !strings.Contains(leads[0].Note, "inactve") {
		t.Errorf("expected note to mention 'inactve', got %q", leads[0].Note)
	}
}

func TestJoinRSDrift_UntypedBindingSuppresses(t *testing.T) {
	// Nearest binding is untyped (let shadow) — must suppress.
	consts := []rsConstStr{
		{name: "A", value: "a"},
	}
	bindings := []rsBinding{
		// Outer typed param — wider scope.
		{name: "s", scopeStart: 0, scopeEnd: 1000, isTypedStr: true},
		// Inner let shadow — narrower scope, nearer.
		{name: "s", scopeStart: 50, scopeEnd: 500, isTypedStr: false},
	}
	matches := []rsMatchInfo{
		{
			scrutinee: "s",
			exprStart: 100,
			exprEnd:   200,
			arms:      []rsArmLit{{value: "c", line: 10}},
		},
	}
	leads := joinRSDrift("src/lib.rs", consts, bindings, matches)
	if len(leads) != 0 {
		t.Errorf("expected 0 leads (untyped nearest binding suppresses), got %d", len(leads))
	}
}

func TestJoinRSDrift_NoBindingForScrutinee(t *testing.T) {
	consts := []rsConstStr{{name: "A", value: "a"}}
	bindings := []rsBinding{
		// Binding exists for a different name.
		{name: "other", scopeStart: 0, scopeEnd: 1000, isTypedStr: true},
	}
	matches := []rsMatchInfo{
		{
			scrutinee: "s",
			exprStart: 100,
			exprEnd:   200,
			arms:      []rsArmLit{{value: "c", line: 10}},
		},
	}
	leads := joinRSDrift("src/lib.rs", consts, bindings, matches)
	if len(leads) != 0 {
		t.Errorf("expected 0 leads (no binding for scrutinee), got %d", len(leads))
	}
}

func TestJoinRSDrift_AllArmsInProducerSet(t *testing.T) {
	consts := []rsConstStr{
		{name: "A", value: "a"},
		{name: "B", value: "b"},
	}
	bindings := []rsBinding{
		{name: "s", scopeStart: 0, scopeEnd: 1000, isTypedStr: true},
	}
	matches := []rsMatchInfo{
		{
			scrutinee: "s",
			exprStart: 100,
			exprEnd:   200,
			arms: []rsArmLit{
				{value: "a", line: 10},
				{value: "b", line: 11},
			},
		},
	}
	leads := joinRSDrift("src/lib.rs", consts, bindings, matches)
	if len(leads) != 0 {
		t.Errorf("expected 0 leads (all arms in producer set), got %d", len(leads))
	}
}

// ─── D1 oracle repro tests ──────────────────────────────────────────────────

// TestJoinRSDrift_D1_ZeroOverlapSuppresses verifies the arm↔pool anchor:
// when no arm literal is in the producer set, the match is not over the
// const domain and the miner must emit nothing.
func TestJoinRSDrift_D1_ZeroOverlapSuppresses(t *testing.T) {
	// Oracle repro: const VERSION="1.4.2" + subcommand dispatch.
	consts := []rsConstStr{{name: "VERSION", value: "1.4.2"}}
	bindings := []rsBinding{
		{name: "cmd", scopeStart: 0, scopeEnd: 1000, isTypedStr: true},
	}
	matches := []rsMatchInfo{
		{
			scrutinee: "cmd",
			exprStart: 100,
			exprEnd:   300,
			arms: []rsArmLit{
				{value: "run", line: 10},
				{value: "build", line: 11},
				{value: "test", line: 12},
			},
		},
	}
	leads := joinRSDrift("src/main.rs", consts, bindings, matches)
	if len(leads) != 0 {
		t.Errorf("D1: expected 0 leads (zero arm↔pool overlap), got %d: %+v", len(leads), leads)
	}
}

// TestJoinRSDrift_D1_VersionConstSubcommand is the file-level repro for D1.
func TestJoinRSDrift_D1_VersionConstSubcommand(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_clean", "version_const_subcommand.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}
	leads := joinRSDrift("version_const_subcommand.rs",
		passRS_Consts(h, tree, src),
		passRS_Bindings(h, tree, src),
		passRS_MatchExprs(h, tree, src),
	)
	if len(leads) != 0 {
		t.Errorf("D1 version+subcommand: expected 0 leads, got %d: %+v", len(leads), leads)
	}
}

// TestJoinRSDrift_D1_ColorConstHTTPVerb is the second file-level D1 repro.
func TestJoinRSDrift_D1_ColorConstHTTPVerb(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_clean", "color_const_http_verb.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}
	leads := joinRSDrift("color_const_http_verb.rs",
		passRS_Consts(h, tree, src),
		passRS_Bindings(h, tree, src),
		passRS_MatchExprs(h, tree, src),
	)
	if len(leads) != 0 {
		t.Errorf("D1 color+HTTP verb: expected 0 leads, got %d: %+v", len(leads), leads)
	}
}

// TestJoinRSDrift_D1_OverlapAnchorTypoFires is the positive control:
// when arms overlap the pool AND there is a typo arm, exactly 1 lead fires.
func TestJoinRSDrift_D1_OverlapAnchorTypoFires(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_drift", "overlap_anchor_typo.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}
	leads := joinRSDrift("overlap_anchor_typo.rs",
		passRS_Consts(h, tree, src),
		passRS_Bindings(h, tree, src),
		passRS_MatchExprs(h, tree, src),
	)
	if len(leads) != 1 {
		t.Fatalf("D1 positive control: expected 1 lead for 'pendig', got %d: %+v", len(leads), leads)
	}
	if !strings.Contains(leads[0].Note, "pendig") {
		t.Errorf("lead note should mention 'pendig', got: %q", leads[0].Note)
	}
	if leads[0].Line != 13 {
		t.Errorf("expected lead on line 13, got %d", leads[0].Line)
	}
}

// ─── D2 oracle repro tests ──────────────────────────────────────────────────

// TestJoinRSDrift_D2_LetElseDestructureSuppresses verifies that let-else
// destructuring patterns (tuple_struct_pattern) are registered as sentinels.
func TestJoinRSDrift_D2_LetElseDestructureSuppresses(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_clean", "let_else_destructure.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}
	leads := joinRSDrift("let_else_destructure.rs",
		passRS_Consts(h, tree, src),
		passRS_Bindings(h, tree, src),
		passRS_MatchExprs(h, tree, src),
	)
	if len(leads) != 0 {
		t.Errorf("D2 let-else: expected 0 leads (cmd is tuple_struct_pattern binding), got %d: %+v", len(leads), leads)
	}
}

// ─── isRSTestPath ──────────────────────────────────────────────────────────────

func TestIsRSTestPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"src/lib.rs", false},
		{"src/main.rs", false},
		{"tests/integration.rs", true},
		{"crate/tests/foo.rs", true},
		{"src/foo_test.rs", true},
		{"testdata/sample.rs", true},
		{"benches/bench.rs", true},
		{"src/util/mod.rs", false},
	}
	for _, tc := range cases {
		got := isRSTestPath(tc.path)
		if got != tc.want {
			t.Errorf("isRSTestPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ─── fixture integration tests ─────────────────────────────────────────────────

// fixtureRS runs the miner over a directory of .rs fixtures and returns leads.
func fixtureRS(t *testing.T, dir string) []store.Lead {
	t.Helper()
	h := loadRSHandleForTest(t)

	var leads []store.Lead
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rs") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		tree, err := parseRSFile(h, src)
		if err != nil {
			t.Fatalf("parse fixture %s: %v", path, err)
		}
		if tree.RootNode().HasError() {
			t.Logf("fixture %s has parse errors (skipped)", path)
			tree.Release()
			continue
		}
		consts := passRS_Consts(h, tree, src)
		bindings := passRS_Bindings(h, tree, src)
		matches := passRS_MatchExprs(h, tree, src)
		tree.Release()
		fileLeads := joinRSDrift(e.Name(), consts, bindings, matches)
		leads = append(leads, fileLeads...)
	}
	return leads
}

func TestRSFixture_DriftProducesLeads(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_rs_drift")
	leads := fixtureRS(t, dir)
	if len(leads) == 0 {
		t.Fatal("expected at least 1 lead from drift fixtures, got 0")
	}
	// Verify each lead has a file and line.
	for _, l := range leads {
		if l.File == "" {
			t.Errorf("lead missing File: %+v", l)
		}
		if l.Line <= 0 {
			t.Errorf("lead missing Line: %+v", l)
		}
		if l.PosterLens != stringlyRsPosterLens {
			t.Errorf("unexpected PosterLens %q", l.PosterLens)
		}
	}
	t.Logf("drift fixtures produced %d leads", len(leads))
}

func TestRSFixture_CleanProducesNoLeads(t *testing.T) {
	dir := filepath.Join("testdata", "stringly_rs_clean")
	leads := fixtureRS(t, dir)
	if len(leads) != 0 {
		t.Errorf("expected 0 leads from clean fixtures, got %d:", len(leads))
		for _, l := range leads {
			t.Errorf("  %s:%d %s", l.File, l.Line, l.Note)
		}
	}
}

// ─── specific fixture assertions ───────────────────────────────────────────────

func TestRSFixture_TypoArmLead(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_drift", "typo_arm.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}

	consts := passRS_Consts(h, tree, src)
	bindings := passRS_Bindings(h, tree, src)
	matches := passRS_MatchExprs(h, tree, src)
	leads := joinRSDrift("typo_arm.rs", consts, bindings, matches)

	if len(leads) != 1 {
		t.Fatalf("expected 1 lead for 'inactve' typo, got %d: %+v", len(leads), leads)
	}
	if !strings.Contains(leads[0].Note, "inactve") {
		t.Errorf("lead note should mention 'inactve', got: %q", leads[0].Note)
	}
	if leads[0].Line != 11 {
		t.Errorf("expected lead on line 11, got %d", leads[0].Line)
	}
}

func TestRSFixture_StaleArmLead(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_drift", "stale_arm.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}

	consts := passRS_Consts(h, tree, src)
	bindings := passRS_Bindings(h, tree, src)
	matches := passRS_MatchExprs(h, tree, src)
	leads := joinRSDrift("stale_arm.rs", consts, bindings, matches)

	if len(leads) != 1 {
		t.Fatalf("expected 1 lead for 'legacy' stale arm, got %d: %+v", len(leads), leads)
	}
	if !strings.Contains(leads[0].Note, "legacy") {
		t.Errorf("lead note should mention 'legacy', got: %q", leads[0].Note)
	}
	if leads[0].Line != 14 {
		t.Errorf("expected lead on line 14, got %d", leads[0].Line)
	}
}

func TestRSFixture_ShadowLetSuppresses(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_clean", "shadow_let.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}

	consts := passRS_Consts(h, tree, src)
	bindings := passRS_Bindings(h, tree, src)
	matches := passRS_MatchExprs(h, tree, src)
	leads := joinRSDrift("shadow_let.rs", consts, bindings, matches)

	if len(leads) != 0 {
		t.Errorf("expected 0 leads for shadow_let fixture, got %d: %+v", len(leads), leads)
	}
}

func TestRSFixture_MacroBodySuppresses(t *testing.T) {
	h := loadRSHandleForTest(t)
	src, err := os.ReadFile(filepath.Join("testdata", "stringly_rs_clean", "macro_body.rs"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tree, err := parseRSFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("fixture has parse errors")
	}

	consts := passRS_Consts(h, tree, src)
	bindings := passRS_Bindings(h, tree, src)
	matches := passRS_MatchExprs(h, tree, src)
	leads := joinRSDrift("macro_body.rs", consts, bindings, matches)

	if len(leads) != 0 {
		t.Errorf("expected 0 leads for macro_body fixture (macro_rules bodies excluded), got %d: %+v", len(leads), leads)
	}
}

// ─── Seed integration test ──────────────────────────────────────────────────

func TestSeedStringlyRsDrift_Basic(t *testing.T) {
	root := t.TempDir()
	// Write a file with a typo arm.
	src := `const STATUS_ACTIVE: &str = "active";
const STATUS_INACTIVE: &str = "inactive";

fn dispatch(status: &str) {
    match status {
        "active" => {},
        "inactve" => {},
        _ => {}
    }
}
`
	if err := os.WriteFile(filepath.Join(root, "lib.rs"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: root,
		Files: []ingest.File{
			{Path: "lib.rs", Language: ingest.LangRust},
		},
	}

	st := openStore(t)
	ctx := context.Background()
	var sum Summary
	if err := seedStringlyRsDrift(ctx, snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyRsDrift: %v", err)
	}

	if sum.StringlyRsDriftLeads != 1 {
		t.Errorf("expected 1 lead, got %d", sum.StringlyRsDriftLeads)
	}
	leads, err := st.ListLeads(ctx)
	if err != nil {
		t.Fatalf("st.ListLeads: %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("expected 1 stored lead, got %d", len(leads))
	}
	if leads[0].PosterLens != stringlyRsPosterLens {
		t.Errorf("unexpected PosterLens %q", leads[0].PosterLens)
	}
}

func TestSeedStringlyRsDrift_TestFileSkipped(t *testing.T) {
	root := t.TempDir()
	src := `const A: &str = "a";

fn test_dispatch(s: &str) {
    match s {
        "a" => {},
        "c" => {},
        _ => {}
    }
}
`
	// File is in tests/ directory.
	testsDir := filepath.Join(root, "tests")
	if err := os.MkdirAll(testsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testsDir, "integration.rs"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &ingest.Snapshot{
		Root: root,
		Files: []ingest.File{
			{Path: "tests/integration.rs", Language: ingest.LangRust},
		},
	}

	st := openStore(t)
	var sum Summary
	if err := seedStringlyRsDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyRsDrift: %v", err)
	}
	if sum.StringlyRsDriftLeads != 0 {
		t.Errorf("expected 0 leads (test file skipped), got %d", sum.StringlyRsDriftLeads)
	}
}

func TestSeedStringlyRsDrift_NonRustSkipped(t *testing.T) {
	root := t.TempDir()
	snap := &ingest.Snapshot{
		Root: root,
		Files: []ingest.File{
			{Path: "main.go", Language: ingest.LangGo},
			{Path: "index.ts", Language: ingest.LangTypeScript},
		},
	}
	st := openStore(t)
	var sum Summary
	if err := seedStringlyRsDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyRsDrift: %v", err)
	}
	if sum.StringlyRsDriftLeads != 0 {
		t.Errorf("expected 0 leads for non-Rust files, got %d", sum.StringlyRsDriftLeads)
	}
}
