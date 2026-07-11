package miner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

// loadCLangHandle is a test helper that loads the C grammar handle or skips.
func loadCLangHandle(t *testing.T) *cppLangHandle {
	t.Helper()
	h, err := loadCppLangHandle("x.c")
	if err != nil {
		t.Skipf("C grammar unavailable: %v", err)
	}
	return h
}

// loadCppLangHandleForTest loads the C++ grammar handle or skips.
func loadCppLangHandleForTest(t *testing.T) *cppLangHandle {
	t.Helper()
	h, err := loadCppLangHandle("x.cpp")
	if err != nil {
		t.Skipf("C++ grammar unavailable: %v", err)
	}
	return h
}

// ─── passC_EnumDecls ─────────────────────────────────────────────────────────

// TestPassCEnumDecls_BasicMembersNoValues proves that enumerator names are
// extracted correctly from a plain enum without explicit values.
func TestPassCEnumDecls_BasicMembersNoValues(t *testing.T) {
	h := loadCLangHandle(t)
	src := []byte(`
enum Color {
    COLOR_RED,
    COLOR_GREEN,
    COLOR_BLUE
};
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	members, valByMem, memByVal := passC_EnumDecls(h, tree, src)
	for _, want := range []string{"COLOR_RED", "COLOR_GREEN", "COLOR_BLUE"} {
		if !members[want] {
			t.Errorf("expected member %q, members: %v", want, members)
		}
	}
	// No explicit values → value maps should be nil (precision-first)
	if valByMem != nil {
		t.Errorf("expected nil valByMem for enum without explicit values, got %v", valByMem)
	}
	if memByVal != nil {
		t.Errorf("expected nil memByVal for enum without explicit values, got %v", memByVal)
	}
}

// TestPassCEnumDecls_AllExplicitValues proves that explicit integer values are
// extracted and the allExplicit flag is effective (value maps populated).
func TestPassCEnumDecls_AllExplicitValues(t *testing.T) {
	h := loadCLangHandle(t)
	src := []byte(`
enum Color {
    COLOR_RED   = 0,
    COLOR_GREEN = 1,
    COLOR_BLUE  = 2
};
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	members, valByMem, memByVal := passC_EnumDecls(h, tree, src)
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d: %v", len(members), members)
	}
	if valByMem == nil {
		t.Fatal("expected non-nil valByMem for all-explicit enum")
	}
	if valByMem["COLOR_RED"] != 0 {
		t.Errorf("COLOR_RED value: got %d, want 0", valByMem["COLOR_RED"])
	}
	if valByMem["COLOR_GREEN"] != 1 {
		t.Errorf("COLOR_GREEN value: got %d, want 1", valByMem["COLOR_GREEN"])
	}
	if memByVal == nil {
		t.Fatal("expected non-nil memByVal")
	}
	if memByVal[2] != "COLOR_BLUE" {
		t.Errorf("memByVal[2]: got %q, want COLOR_BLUE", memByVal[2])
	}
}

// TestPassCEnumDecls_PartialValuesDisablesTypeA proves that partial explicit
// values (not all members) make the value maps nil — Type-A integer checks
// are disabled (precision-first).
func TestPassCEnumDecls_PartialValuesDisablesTypeA(t *testing.T) {
	h := loadCLangHandle(t)
	src := []byte(`
enum Mixed {
    MIXED_A = 0,
    MIXED_B,
    MIXED_C = 5
};
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	_, valByMem, memByVal := passC_EnumDecls(h, tree, src)
	// MIXED_B has no explicit value — maps should be nil.
	if valByMem != nil {
		t.Errorf("expected nil valByMem for partial-explicit enum, got %v", valByMem)
	}
	if memByVal != nil {
		t.Errorf("expected nil memByVal for partial-explicit enum, got %v", memByVal)
	}
}

// TestPassCEnumDecls_WorksOnErrorTree proves that enumerator extraction works
// even on trees that have HasError()=true (the expected condition for all C files
// with enum declarations due to the gotreesitter v0.20.2 grammar limitation).
func TestPassCEnumDecls_WorksOnErrorTree(t *testing.T) {
	h := loadCLangHandle(t)
	// This is the realistic real-world case: enum + function in same file.
	// The grammar produces HasError()=true, but enumerator nodes are preserved.
	src := []byte(`
enum Color {
    COLOR_RED = 0,
    COLOR_GREEN = 1,
    COLOR_BLUE = 2
};
void process(Color c) {
    switch (c) {
        case COLOR_RED: break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	// File should have HasError due to grammar limitation.
	if !tree.RootNode().HasError() {
		t.Log("note: expected HasError=true for file with enum decl; grammar may have been upgraded")
	}

	members, _, _ := passC_EnumDecls(h, tree, src)
	for _, want := range []string{"COLOR_RED", "COLOR_GREEN", "COLOR_BLUE"} {
		if !members[want] {
			t.Errorf("expected member %q even on error tree, members: %v", want, members)
		}
	}
}

// ─── passC_Switches ───────────────────────────────────────────────────────────

// TestPassCSwitches_BasicSwitch proves that a switch statement's scrutinee and
// case arms are correctly extracted from a clean-parsing file.
func TestPassCSwitches_BasicSwitch(t *testing.T) {
	h := loadCLangHandle(t)
	src := []byte(`
void process(Color c) {
    switch (c) {
        case COLOR_RED:
            break;
        case COLOR_GREEN:
            break;
        case 99:
            break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("file has parse error — passC_Switches skips HasError files")
	}

	_, switches := passC_Switches(h, tree, src, "process.c")
	if len(switches) != 1 {
		t.Fatalf("expected 1 switch, got %d: %+v", len(switches), switches)
	}
	sw := switches[0]
	if sw.scrutinee != "c" {
		t.Errorf("scrutinee: got %q, want %q", sw.scrutinee, "c")
	}

	// Case identifiers should include COLOR_RED and COLOR_GREEN.
	identSet := make(map[string]bool, len(sw.caseIdents))
	for _, id := range sw.caseIdents {
		identSet[id] = true
	}
	for _, want := range []string{"COLOR_RED", "COLOR_GREEN"} {
		if !identSet[want] {
			t.Errorf("expected case ident %q, got %v", want, sw.caseIdents)
		}
	}

	// Integer case arm: 99
	if len(sw.caseInts) != 1 || sw.caseInts[0].value != 99 {
		t.Errorf("expected case int 99, got %v", sw.caseInts)
	}
	if sw.hasDefault {
		t.Errorf("expected no default clause")
	}
}

// TestPassCSwitches_DefaultDetected proves that a switch with a default clause
// sets hasDefault=true, suppressing type-B leads.
func TestPassCSwitches_DefaultDetected(t *testing.T) {
	h := loadCLangHandle(t)
	src := []byte(`
void foo(Color c) {
    switch (c) {
        case COLOR_RED:
            break;
        default:
            break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("parse error — skip")
	}

	_, switches := passC_Switches(h, tree, src, "foo.c")
	if len(switches) != 1 {
		t.Fatalf("expected 1 switch, got %d", len(switches))
	}
	if !switches[0].hasDefault {
		t.Error("expected hasDefault=true for switch with default clause")
	}
}

// TestPassCSwitches_ParamTypeBinding proves that a typed parameter binding is
// extracted correctly.
func TestPassCSwitches_ParamTypeBinding(t *testing.T) {
	h := loadCLangHandle(t)
	src := []byte(`
void foo(Color c) {
    switch (c) {
        case COLOR_RED: break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()

	if tree.RootNode().HasError() {
		t.Skip("parse error — skip")
	}

	bindings, _ := passC_Switches(h, tree, src, "foo.c")
	found := false
	for _, b := range bindings {
		if b.name == "c" && b.typeName == "Color" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected binding c:Color, got %+v", bindings)
	}
}

// ─── joinCppDrift ─────────────────────────────────────────────────────────────

// TestJoinCppDrift_TypeA_IntegerLiteral proves that a case integer literal
// colliding with an enum member value emits a Type-A lead with exact file+line.
func TestJoinCppDrift_TypeA_IntegerLiteral(t *testing.T) {
	h := loadCLangHandle(t)

	// Enum in a separate "header" source.
	enumSrc := []byte(`
enum Color {
    COLOR_RED   = 0,
    COLOR_GREEN = 1,
    COLOR_BLUE  = 2
};
typedef enum { COLOR_RED = 0, COLOR_GREEN = 1, COLOR_BLUE = 2 } Color;
`)
	enumTree, err := parseCppFile(h, enumSrc)
	if err != nil {
		t.Fatalf("parse enum: %v", err)
	}
	defer enumTree.Release()

	members, valByMem, memByVal := passC_EnumDecls(h, enumTree, enumSrc)
	if len(members) == 0 {
		t.Skip("no members extracted — grammar may have changed")
	}

	// Build typedef names from a fake clean file. In the fixture the typedef
	// is in the same source as the enum — since the grammar produces HasError,
	// we manually build the pool for this unit test.
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {
				name:          "Color",
				members:       members,
				valueByMember: valByMem,
				memberByValue: memByVal,
				allExplicit:   valByMem != nil,
			},
		},
	}

	// Switch file (clean).
	switchSrc := []byte(`
void process(Color c) {
    switch (c) {
        case COLOR_RED:
            break;
        case 1:
            break;
    }
}
`)
	switchTree, err := parseCppFile(h, switchSrc)
	if err != nil {
		t.Fatalf("parse switch: %v", err)
	}
	defer switchTree.Release()

	if switchTree.RootNode().HasError() {
		t.Skip("switch file has parse error")
	}

	bindings, switches := passC_Switches(h, switchTree, switchSrc, "process.c")
	if len(switches) == 0 {
		t.Fatal("no switches extracted")
	}

	typeA, _ := joinCppDrift("process.c", ".", pool, bindings, switches)
	if len(typeA) == 0 {
		t.Error("expected at least one Type-A lead for case 1 == COLOR_GREEN value")
	}
	// Verify the lead points to the right file.
	for _, lead := range typeA {
		if lead.File != "process.c" {
			t.Errorf("lead.File = %q, want process.c", lead.File)
		}
		if lead.Line <= 0 {
			t.Errorf("lead.Line = %d, want > 0", lead.Line)
		}
	}
}

// TestJoinCppDrift_TypeB_MissingArm proves that a missing enumerator arm in a
// switch without a default emits a Type-B lead.
func TestJoinCppDrift_TypeB_MissingArm(t *testing.T) {
	h := loadCLangHandle(t)

	members := map[string]bool{
		"COLOR_RED":   true,
		"COLOR_GREEN": true,
		"COLOR_BLUE":  true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {
				name:    "Color",
				members: members,
			},
		},
	}

	// Switch that handles RED and GREEN but not BLUE — no default.
	switchSrc := []byte(`
void process(Color c) {
    switch (c) {
        case COLOR_RED:
            break;
        case COLOR_GREEN:
            break;
    }
}
`)
	switchTree, err := parseCppFile(h, switchSrc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer switchTree.Release()

	if switchTree.RootNode().HasError() {
		t.Skip("switch file has parse error")
	}

	bindings, switches := passC_Switches(h, switchTree, switchSrc, "process.c")
	if len(switches) == 0 {
		t.Fatal("no switches found")
	}

	_, typeB := joinCppDrift("process.c", ".", pool, bindings, switches)
	if len(typeB) == 0 {
		t.Error("expected a Type-B lead for COLOR_BLUE (missing arm, no default)")
	}
	found := false
	for _, lead := range typeB {
		if lead.File == "process.c" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected lead in process.c, got %+v", typeB)
	}
}

// TestJoinCppDrift_DefaultSuppressesTypeB proves that a switch with a default
// clause does NOT emit a Type-B lead even when enum members are missing.
func TestJoinCppDrift_DefaultSuppressesTypeB(t *testing.T) {
	h := loadCLangHandle(t)

	members := map[string]bool{
		"STATUS_OK":    true,
		"STATUS_ERROR": true,
		"STATUS_DONE":  true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Status": {
				name:    "Status",
				members: members,
			},
		},
	}

	switchSrc := []byte(`
void check(Status s) {
    switch (s) {
        case STATUS_OK:
            break;
        case STATUS_ERROR:
            break;
        default:
            break;
    }
}
`)
	switchTree, err := parseCppFile(h, switchSrc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer switchTree.Release()

	if switchTree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, switchTree, switchSrc, "check.c")
	_, typeB := joinCppDrift("check.c", ".", pool, bindings, switches)
	if len(typeB) != 0 {
		t.Errorf("expected 0 Type-B leads with default clause, got %d: %+v", len(typeB), typeB)
	}
}

// TestJoinCppDrift_UntypedScrutineeNoLead proves that a switch over a plain
// int parameter (no matching enum type binding) produces no leads.
func TestJoinCppDrift_UntypedScrutineeNoLead(t *testing.T) {
	h := loadCLangHandle(t)

	members := map[string]bool{
		"STATUS_OK":    true,
		"STATUS_ERROR": true,
		"STATUS_DONE":  true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Status": {
				name:    "Status",
				members: members,
			},
		},
	}

	// int parameter, not Status — no type binding → no leads.
	switchSrc := []byte(`
void process_raw(int code) {
    switch (code) {
        case 0:
            break;
        case 1:
            break;
    }
}
`)
	switchTree, err := parseCppFile(h, switchSrc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer switchTree.Release()

	if switchTree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, switchTree, switchSrc, "raw.c")
	typeA, typeB := joinCppDrift("raw.c", ".", pool, bindings, switches)
	if len(typeA)+len(typeB) != 0 {
		t.Errorf("expected 0 leads for int-typed scrutinee, got A=%d B=%d", len(typeA), len(typeB))
	}
}

// ─── isCTestPath ─────────────────────────────────────────────────────────────

func TestIsCTestPath(t *testing.T) {
	positive := []string{
		"src/foo_test.c",
		"src/bar_tests.cpp",
		"test/core.c",
		"tests/suite.cc",
		"gtest/main.cpp",
		"googletest/helpers.cc",
		"catch2/tests.cpp",
	}
	for _, p := range positive {
		if !isCTestPath(p) {
			t.Errorf("isCTestPath(%q) = false, want true", p)
		}
	}
	negative := []string{
		"src/main.c",
		"lib/color.h",
		"src/process.cc",
		"include/api.hpp",
	}
	for _, p := range negative {
		if isCTestPath(p) {
			t.Errorf("isCTestPath(%q) = true, want false", p)
		}
	}
}

// ─── end-to-end: fixture directories ─────────────────────────────────────────

// TestCppEnumDrift_PositiveFixtures proves that the miner detects leads in the
// cpp_enum_drift testdata directory and reports the exact file+line.
//
// Fixture layout:
//   - colors.h — typedef enum Color { RED=0, GREEN=1, BLUE=2 } Color;
//   - process.c — switch(c) with case 99 (Type-A) and missing BLUE (Type-B)
func TestCppEnumDrift_PositiveFixtures(t *testing.T) {
	root := filepath.Join("testdata", "cpp_enum_drift")
	snap := &ingest.Snapshot{
		Commit: "test",
		Root:   root,
		Files: []ingest.File{
			{Path: "colors.h", Language: ingest.LangC},
			{Path: "process.c", Language: ingest.LangC},
		},
	}
	st := openStore(t)

	var sum Summary
	if err := seedCppEnumDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedCppEnumDrift: %v", err)
	}

	leads, err := st.ListLeads(context.Background())
	if err != nil {
		t.Fatalf("ListLeads: %v", err)
	}

	t.Logf("CppDriftLeads=%d CppParseFailures=%d total leads=%d", sum.CppDriftLeads, sum.CppParseFailures, len(leads))
	for i, l := range leads {
		t.Logf("  lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}

	// We must see at least one lead (Type-A or Type-B).
	if len(leads) == 0 {
		t.Error("expected at least one lead from positive fixture directory")
	}
	// All leads must come from the process.c file.
	for _, l := range leads {
		if l.File != "process.c" {
			t.Errorf("lead from unexpected file %q, want process.c", l.File)
		}
	}
}

// TestCppEnumDrift_NegativeFixtures proves that the miner emits ZERO leads
// for the cpp_enum_clean testdata directory.
//
// Fixture layout:
//   - status.h — typedef enum Status
//   - exhaustive_switch.c — covers all members
//   - default_suppresses.c — switch with default
//   - switch_over_int.c — int scrutinee, not Status
//   - enum_in_comment.c — enum name only in comments
func TestCppEnumDrift_NegativeFixtures(t *testing.T) {
	root := filepath.Join("testdata", "cpp_enum_clean")
	snap := &ingest.Snapshot{
		Commit: "test",
		Root:   root,
		Files: []ingest.File{
			{Path: "status.h", Language: ingest.LangC},
			{Path: "exhaustive_switch.c", Language: ingest.LangC},
			{Path: "default_suppresses.c", Language: ingest.LangC},
			{Path: "switch_over_int.c", Language: ingest.LangC},
			{Path: "enum_in_comment.c", Language: ingest.LangC},
		},
	}
	st := openStore(t)

	var sum Summary
	if err := seedCppEnumDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedCppEnumDrift: %v", err)
	}

	leads, err := st.ListLeads(context.Background())
	if err != nil {
		t.Fatalf("ListLeads: %v", err)
	}

	t.Logf("CppDriftLeads=%d CppParseFailures=%d total leads=%d", sum.CppDriftLeads, sum.CppParseFailures, len(leads))
	for i, l := range leads {
		t.Logf("  FP lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}

	if len(leads) != 0 {
		t.Errorf("expected 0 leads from clean fixtures, got %d", len(leads))
	}
}

// ─── D1 oracle repros: scope isolation ───────────────────────────────────────

// TestPassCSwitches_CrossFunctionIsolation proves the D1 fix: a typed binding
// in function f does not leak into function g's switch statement.
//
// Oracle repro: void f(Color x){...} + void g(int x){ switch(x){case A:case B:} }
// Before fix: file-wide scope [0,len] made f's Color-x binding visible in g's
// switch → false type-B lead for any Color member not covered in g's switch.
// After fix: real function-body scopes prevent the cross-function binding.
func TestPassCSwitches_CrossFunctionIsolation(t *testing.T) {
	h := loadCLangHandle(t)

	// Two functions in one file: f has Color param, g has int param.
	// g's switch is over an int — no enum lead should fire.
	src := []byte(`
void f(Color x) {
    switch (x) {
        case COLOR_RED: break;
    }
}
void g(int x) {
    switch (x) {
        case 0:
        case 1:
            break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, tree, src, "multi.c")
	if len(switches) == 0 {
		t.Skip("no switches — grammar limitation")
	}

	// Pool with Color typedef.
	members := map[string]bool{
		"COLOR_RED": true, "COLOR_GREEN": true, "COLOR_BLUE": true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {name: "Color", members: members},
		},
	}

	typeA, typeB := joinCppDrift("multi.c", ".", pool, bindings, switches)
	// g's switch is over int x (primitive sentinel) — must produce 0 leads.
	// f's switch IS over Color x but only covers COLOR_RED; however f's switch
	// is in the bindings too — we accept leads from f but not from g.
	// The key check: any lead from g (switches[1]) must NOT exist.
	// Since we can't distinguish which switch a lead came from in this test,
	// we verify by checking that leads don't exceed what f's single switch could produce.
	// f covers COLOR_RED → COLOR_GREEN and COLOR_BLUE are uncovered → max 2 type-B leads from f.
	// g's switch on int x should produce 0 leads (primitive shadow).
	for _, lead := range append(typeA, typeB...) {
		// All leads must be from g = nil (no enum join) or from f.
		// Simplest: leads must point to a line within f's body (lines 2-7),
		// not g's body (lines 8-14). f's switch is at line 3.
		if lead.Line >= 8 {
			t.Errorf("D1 cross-function FP: lead at line %d is inside g's body (int switch): %s",
				lead.Line, lead.Note)
		}
	}
}

// TestPassCSwitches_IntShadowNoLead proves the D1 fix: an int local variable
// shadowing an outer Color parameter makes the switch invisible to the miner.
//
// Oracle repro: void h(Color x){ int x = 0; switch(x){...} }
// Before fix: both the Color-param binding and the int-local binding had
// file-wide scope → same span → first-in-slice won (unpredictable).
// After fix: the int-local binding has the inner compound_statement scope
// (smaller span) and wins as nearest binding → typeName="" → no leads.
func TestPassCSwitches_IntShadowNoLead(t *testing.T) {
	h := loadCLangHandle(t)

	// Color x is the outer param; int x shadows it inside the block.
	src := []byte(`
void h(Color x) {
    int x = 0;
    switch (x) {
        case 0: break;
        case 1: break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, tree, src, "shadow.c")
	if len(switches) == 0 {
		t.Skip("no switches")
	}

	members := map[string]bool{
		"COLOR_RED": true, "COLOR_GREEN": true, "COLOR_BLUE": true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {name: "Color", members: members},
		},
	}

	typeA, typeB := joinCppDrift("shadow.c", ".", pool, bindings, switches)
	if total := len(typeA) + len(typeB); total != 0 {
		t.Errorf("D1 int-shadow FP: expected 0 leads when int x shadows Color x, got %d", total)
		for _, l := range append(typeA, typeB...) {
			t.Logf("  FP: %s:%d: %s", l.File, l.Line, l.Note)
		}
	}
}

// TestPassCSwitches_PositiveControlSameFunctionFires proves that the scope
// fix does not over-suppress: a legitimately typed switch in the same function
// still produces Type-B leads.
func TestPassCSwitches_PositiveControlSameFunctionFires(t *testing.T) {
	h := loadCLangHandle(t)

	// Color c param, switch missing COLOR_BLUE — should fire.
	src := []byte(`
void check(Color c) {
    switch (c) {
        case COLOR_RED: break;
        case COLOR_GREEN: break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, tree, src, "pos.c")
	if len(switches) == 0 {
		t.Skip("no switches")
	}

	members := map[string]bool{
		"COLOR_RED": true, "COLOR_GREEN": true, "COLOR_BLUE": true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {name: "Color", members: members},
		},
	}

	_, typeB := joinCppDrift("pos.c", ".", pool, bindings, switches)
	if len(typeB) == 0 {
		t.Error("positive control: expected Type-B lead for missing COLOR_BLUE")
	}
}

// ─── D2 oracle repro: two-dir same-name enum ─────────────────────────────────

// TestCppEnumDrift_TwoDirSameNameNoLead proves the D2 fix: two directories
// each defining `typedef enum {...} Color;` with DIFFERENT members do NOT
// produce false Type-A leads when their integer value maps conflict.
//
// Before fix: the pool merged the two enums by intersection + kept the first
// value map → value map had dir_a's values but dir_b's values might collide.
// After fix: pool keyed by dir+typeName; cross-dir enums are separate and the
// switch file in dir_b only joins against dir_b's Color enum.
//
// Fixture layout:
//   - dir_a/colors.h — Color { RED=0, GREEN=1, BLUE=2 }
//   - dir_b/colors.h — Color { ALPHA=10, BETA=20, GAMMA=30 }
//   - dir_b/switch.c — switch(c) over dir_b's Color (exhaustive, no lead expected)
func TestCppEnumDrift_TwoDirSameNameNoLead(t *testing.T) {
	root := filepath.Join("testdata", "cpp_oracle_repro")

	// dir_b/switch.c — exhaustive switch over dir_b's Color (ALPHA, BETA, GAMMA)
	switchC := filepath.Join(root, "dir_b", "switch.c")
	if err := os.WriteFile(switchC, []byte(`
void handle(Color c) {
    switch (c) {
        case COLOR_ALPHA: break;
        case COLOR_BETA:  break;
        case COLOR_GAMMA: break;
    }
}
`), 0o644); err != nil {
		t.Fatalf("write switch.c: %v", err)
	}
	t.Cleanup(func() { os.Remove(switchC) })

	snap := &ingest.Snapshot{
		Commit: "test",
		Root:   root,
		Files: []ingest.File{
			{Path: "dir_a/colors.h", Language: ingest.LangC},
			{Path: "dir_b/colors.h", Language: ingest.LangC},
			{Path: "dir_b/switch.c", Language: ingest.LangC},
		},
	}
	st := openStore(t)

	var sum Summary
	if err := seedCppEnumDrift(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedCppEnumDrift: %v", err)
	}

	leads, err := st.ListLeads(context.Background())
	if err != nil {
		t.Fatalf("ListLeads: %v", err)
	}

	t.Logf("CppDriftLeads=%d CppParseFailures=%d total leads=%d", sum.CppDriftLeads, sum.CppParseFailures, len(leads))
	for i, l := range leads {
		t.Logf("  FP lead[%d] %s:%d: %s", i, l.File, l.Line, l.Note)
	}

	if len(leads) != 0 {
		t.Errorf("D2: expected 0 leads (no cross-dir enum merge), got %d", len(leads))
	}
}

// ─── P2 oracle repro: uninitialized local variable shadowing ─────────────────

// TestPassCSwitches_UninitPrimShadowNoLead proves that an UNINITIALIZED
// primitive local (int x; x = compute();) is captured as a shadow sentinel
// and prevents the outer Color param from matching the switch scrutinee.
//
// Oracle repro: void h(Color x){ int x; x=compute(); switch(x){case 0:case 1:} }
// Before fix: the no-init declarator form was not captured → int x was invisible
// → outer Color x param leaked with file-wide scope → false type-B leads.
// After fix: no-init primitive declarator IS captured → typeName="" sentinel
// wins nearest-binding (smaller compound_statement scope) → 0 leads.
func TestPassCSwitches_UninitPrimShadowNoLead(t *testing.T) {
	h := loadCLangHandle(t)

	src := []byte(`
void h(Color x) {
    int x;
    x = 42;
    switch (x) {
        case 0: break;
        case 1: break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, tree, src, "uninit_shadow.c")
	if len(switches) == 0 {
		t.Skip("no switches")
	}

	members := map[string]bool{
		"COLOR_RED": true, "COLOR_GREEN": true, "COLOR_BLUE": true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {name: "Color", members: members},
		},
	}

	typeA, typeB := joinCppDrift("uninit_shadow.c", ".", pool, bindings, switches)
	if total := len(typeA) + len(typeB); total != 0 {
		t.Errorf("P2 uninit-prim-shadow FP: expected 0 leads, got %d", total)
		for _, l := range append(typeA, typeB...) {
			t.Logf("  FP: %s:%d: %s", l.File, l.Line, l.Note)
		}
	}
}

// TestPassCSwitches_UninitTypedLocalFires proves the positive twin: an
// UNINITIALIZED typed local (Color c; c = get_color(); switch(c){...}) IS
// captured as a typed binding and still produces Type-B leads when enum
// members are missing.
func TestPassCSwitches_UninitTypedLocalFires(t *testing.T) {
	h := loadCLangHandle(t)

	src := []byte(`
void use_color(void) {
    Color c;
    c = COLOR_RED;
    switch (c) {
        case COLOR_RED: break;
        case COLOR_GREEN: break;
    }
}
`)
	tree, err := parseCppFile(h, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Skip("parse error")
	}

	bindings, switches := passC_Switches(h, tree, src, "uninit_typed.c")
	if len(switches) == 0 {
		t.Skip("no switches")
	}

	members := map[string]bool{
		"COLOR_RED": true, "COLOR_GREEN": true, "COLOR_BLUE": true,
	}
	pool := &cppEnumPool{
		allMembers:  members,
		memberTypes: make(map[string]string),
		byTypeName: map[string]*cppEnum{
			"./Color": {name: "Color", members: members},
		},
	}

	_, typeB := joinCppDrift("uninit_typed.c", ".", pool, bindings, switches)
	if len(typeB) == 0 {
		t.Error("P2 uninit-typed-local positive twin: expected Type-B lead for missing COLOR_BLUE")
	}
}
