package miner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// pySnapFile builds a snapshot pointing at a single testdata file,
// using the testdata directory as the snapshot root.
func pySnapFile(t *testing.T, relPath string) *ingest.Snapshot {
	t.Helper()
	dir := filepath.Join("testdata", filepath.Dir(relPath))
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs %s: %v", dir, err)
	}
	return &ingest.Snapshot{
		Commit: "test",
		Root:   abs,
		Files: []ingest.File{
			{Path: filepath.Base(relPath), Language: ingest.LangPython},
		},
	}
}

// ─── producer extraction tests ───────────────────────────────────────────────

func TestPyProducers_LiteralAssignment(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`Status = Literal['active', 'inactive', 'pending']`)
	tree, err := parsePyFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	if len(producers) != 1 {
		t.Fatalf("want 1 producer, got %d", len(producers))
	}
	p := producers[0]
	if p.name != "Status" {
		t.Errorf("name: want Status, got %s", p.name)
	}
	want := map[string]bool{"active": true, "inactive": true, "pending": true}
	for k := range want {
		if !p.members[k] {
			t.Errorf("missing member %q", k)
		}
	}
}

func TestPyProducers_MixedLiteralExcluded(t *testing.T) {
	// Structural whitelist: Literal['a', 1] → excluded (integer member).
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`Mixed = Literal['active', 1, 'inactive']`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	if len(producers) != 0 {
		t.Errorf("mixed literal should be excluded; got %d producers", len(producers))
	}
}

func TestPyProducers_StrEnum(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`from enum import StrEnum

class Color(StrEnum):
    RED = 'red'
    GREEN = 'green'
    BLUE = 'blue'
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	if len(producers) != 1 {
		t.Fatalf("want 1 producer (Color), got %d", len(producers))
	}
	p := producers[0]
	if p.name != "Color" {
		t.Errorf("name: want Color, got %s", p.name)
	}
	for _, m := range []string{"red", "green", "blue"} {
		if !p.members[m] {
			t.Errorf("missing member %q", m)
		}
	}
}

func TestPyProducers_StrEnumMixedExcluded(t *testing.T) {
	// Enum with non-string value must be excluded.
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`from enum import Enum

class Mixed(Enum):
    OK = 'ok'
    ERROR = 'error'
    CODE = 42
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	if len(producers) != 0 {
		t.Errorf("enum with non-string value should be excluded; got %d", len(producers))
	}
}

func TestPyProducers_StrMixinEnum(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`from enum import Enum

class State(str, Enum):
    OPEN = 'open'
    CLOSED = 'closed'
    MERGED = 'merged'
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	if len(producers) != 1 {
		t.Fatalf("want 1 producer (State), got %d", len(producers))
	}
}

// ─── binding tests ────────────────────────────────────────────────────────────

func TestPyBindings_TypedParam(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`Status = Literal['active', 'inactive']

def handle(status: Status) -> None:
    pass
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	known := make(map[string]bool, len(producers))
	for _, p := range producers {
		known[p.name] = true
	}
	bindings := passPy_Bindings(h, tree, src, known)

	var typed []pyBinding
	for _, b := range bindings {
		if b.isTypedUnion {
			typed = append(typed, b)
		}
	}
	if len(typed) != 1 {
		t.Fatalf("want 1 typed union binding, got %d", len(typed))
	}
	if typed[0].name != "status" || typed[0].typeName != "Status" {
		t.Errorf("unexpected typed binding: %+v", typed[0])
	}
}

func TestPyBindings_LambdaShadow(t *testing.T) {
	// Lambda param should be recorded as untyped shadow sentinel.
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`Status = Literal['active', 'inactive']

def outer(status: Status) -> None:
    check = lambda status: status == 'active'
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	producers := passPy_Producers(h, tree, src)
	known := make(map[string]bool, len(producers))
	for _, p := range producers {
		known[p.name] = true
	}
	bindings := passPy_Bindings(h, tree, src, known)

	// Should have at least 2 bindings named 'status': one typed, one lambda shadow.
	count := 0
	for _, b := range bindings {
		if b.name == "status" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("want ≥2 bindings for 'status' (typed + lambda shadow), got %d", count)
	}
}

// ─── if-chain tests ───────────────────────────────────────────────────────────

func TestPyIfChain_Basic(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`if status == 'active':
    pass
elif status == 'inactive':
    pass
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	chains := passPy_IfChains(h, tree, src)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d", len(chains))
	}
	if chains[0].scrutinee != "status" {
		t.Errorf("scrutinee: want status, got %s", chains[0].scrutinee)
	}
	if len(chains[0].cases) != 2 {
		t.Errorf("want 2 cases, got %d", len(chains[0].cases))
	}
	if chains[0].hasElse {
		t.Errorf("should not have else")
	}
}

func TestPyIfChain_ElseSuppressesTypeB(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`if status == 'active':
    pass
else:
    pass
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	chains := passPy_IfChains(h, tree, src)
	if len(chains) != 1 {
		t.Fatalf("want 1 chain, got %d", len(chains))
	}
	if !chains[0].hasElse {
		t.Errorf("should have else")
	}
}

// ─── match statement tests ────────────────────────────────────────────────────

func TestPyMatchStmt_Basic(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`match status:
    case 'active':
        pass
    case 'inactive':
        pass
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	stmts := passPy_MatchStmts(h, tree, src)
	if len(stmts) != 1 {
		t.Fatalf("want 1 match stmt, got %d", len(stmts))
	}
	if stmts[0].scrutinee != "status" {
		t.Errorf("scrutinee: want status, got %s", stmts[0].scrutinee)
	}
	if len(stmts[0].cases) != 2 {
		t.Errorf("want 2 cases, got %d", len(stmts[0].cases))
	}
	if stmts[0].hasWildcard {
		t.Errorf("should not have wildcard")
	}
}

func TestPyMatchStmt_WildcardSuppressesTypeB(t *testing.T) {
	h, err := loadPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`match status:
    case 'active':
        pass
    case _:
        pass
`)
	tree, _ := parsePyFile(h, src)
	defer tree.Release()

	stmts := passPy_MatchStmts(h, tree, src)
	if len(stmts) != 1 {
		t.Fatalf("want 1 match stmt, got %d", len(stmts))
	}
	if !stmts[0].hasWildcard {
		t.Errorf("should have wildcard")
	}
}

// ─── unquoting tests ──────────────────────────────────────────────────────────

func TestUnquotePyString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"'active'", "active"},
		{`"pending"`, "pending"},
		{"''", ""},
		{"'a'", "a"},
		{"active", ""},        // no quotes
		{"'multi\nline'", ""}, // multiline rejected
	}
	for _, tc := range cases {
		got := unquotePyString(tc.in)
		if got != tc.want {
			t.Errorf("unquotePyString(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// ─── test-file gate tests ─────────────────────────────────────────────────────

func TestIsPyTestPath(t *testing.T) {
	yes := []string{
		"tests/test_foo.py",
		"test_bar.py",
		"foo_test.py",
		"testdata/fixtures/x.py",
		"src/tests/handler.py",
		"test/integration.py",
	}
	no := []string{
		"src/handler.py",
		"internal/api/status.py",
		"mytest_module.py", // doesn't start with test_ or end with _test
	}
	for _, p := range yes {
		if !isPyTestPath(p) {
			t.Errorf("isPyTestPath(%q) = false; want true", p)
		}
	}
	for _, p := range no {
		if isPyTestPath(p) {
			t.Errorf("isPyTestPath(%q) = true; want false", p)
		}
	}
}

// ─── end-to-end fixture tests (fail-before / pass-after) ─────────────────────

// TestPyDrift_TypeA_FailBeforePassAfter verifies the type-A fixture produces
// exactly one lead at the expected file+line.
func TestPyDrift_TypeA_FailBeforePassAfter(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_drift/type_a.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, err := st.ListLeads(context.Background())
	if err != nil {
		t.Fatalf("ListLeads: %v", err)
	}
	if len(leads) == 0 {
		t.Fatal("want ≥1 lead for type-A fixture, got 0")
	}
	// Lead should be at line 10 ('stale' is on that line in type_a.py).
	found := false
	for _, l := range leads {
		if l.Line == 10 {
			found = true
		}
	}
	if !found {
		t.Errorf("want lead at line 10; leads: %v", leads)
	}
}

// TestPyDrift_TypeB_If verifies the type-B if-chain fixture.
func TestPyDrift_TypeB_If(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_drift/type_b_if.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) == 0 {
		t.Fatal("want ≥1 lead for type-B if fixture, got 0")
	}
	// Lead should reference 'pending'.
	found := false
	for _, l := range leads {
		if contains(l.Note, "pending") {
			found = true
		}
	}
	if !found {
		t.Errorf("want lead mentioning 'pending'; leads: %v", leads)
	}
}

// TestPyDrift_TypeA_Match verifies the type-A match fixture.
func TestPyDrift_TypeA_Match(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_drift/type_a_match.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) == 0 {
		t.Fatal("want ≥1 lead for type-A match fixture, got 0")
	}
	found := false
	for _, l := range leads {
		if contains(l.Note, "typo_case") {
			found = true
		}
	}
	if !found {
		t.Errorf("want lead mentioning 'typo_case'; leads: %v", leads)
	}
}

// TestPyDrift_TypeB_Match verifies the type-B match fixture.
func TestPyDrift_TypeB_Match(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_drift/type_b_match.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) == 0 {
		t.Fatal("want ≥1 lead for type-B match fixture, got 0")
	}
	found := false
	for _, l := range leads {
		if contains(l.Note, "green") {
			found = true
		}
	}
	if !found {
		t.Errorf("want lead mentioning 'green'; leads: %v", leads)
	}
}

// TestPyDrift_StrEnum_TypeA verifies the str+Enum mixin fixture.
func TestPyDrift_StrEnum_TypeA(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_drift/strenum_type_a.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) == 0 {
		t.Fatal("want ≥1 lead for StrEnum type-A fixture, got 0")
	}
	found := false
	for _, l := range leads {
		if contains(l.Note, "draft") {
			found = true
		}
	}
	if !found {
		t.Errorf("want lead mentioning 'draft'; leads: %v", leads)
	}
}

// ─── adversarial negative fixture tests ──────────────────────────────────────

// TestPyDrift_Clean_MixedLiteral verifies mixed Literal is excluded.
func TestPyDrift_Clean_MixedLiteral(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/mixed_literal.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("mixed literal: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestPyDrift_Clean_ElseSuppression verifies else clause suppresses type-B.
func TestPyDrift_Clean_ElseSuppression(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/else_suppression.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("else suppression: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestPyDrift_Clean_MatchWildcard verifies case _: suppresses type-B.
func TestPyDrift_Clean_MatchWildcard(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/match_wildcard.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("match wildcard: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestPyDrift_Clean_StrEnumNonStrMixin verifies enum with non-str value excluded.
func TestPyDrift_Clean_StrEnumNonStrMixin(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/strenum_nonstr_mixin.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("non-str enum mixin: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestPyDrift_Clean_ShadowLambda verifies lambda param shadows typed binding.
func TestPyDrift_Clean_ShadowLambda(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/shadow_lambda.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("lambda shadow: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestPyDrift_Clean_ShadowFor verifies for-loop variable shadows typed binding.
func TestPyDrift_Clean_ShadowFor(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/shadow_for.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("for shadow: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestPyDrift_Clean_ShadowMatchCapture is the D3 oracle repro: match-case
// capture pattern (`case status:`) binds the subject name, shadowing the outer
// typed param. Expected: 0 leads.
func TestPyDrift_Clean_ShadowMatchCapture(t *testing.T) {
	snap := pySnapFile(t, "stringly_py_clean/shadow_match_capture.py")
	st := openStore(t)
	var sum Summary
	if err := seedStringlyPyDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedStringlyPyDrift: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("match-capture D3 repro: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
