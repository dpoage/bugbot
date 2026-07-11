package miner

import (
	"context"
	"testing"

	gts "github.com/odvcencio/gotreesitter"
)

// ─── unit tests for field/validator extraction (tree-sitter) ─────────────────

func TestCFPyFields_Basic(t *testing.T) {
	h, err := loadCFPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`from pydantic import BaseModel, Field

class Config(BaseModel):
    timeout: int = Field(default=0)
    retries: int = Field(default=3)
`)
	parser := newPyParser(h)
	tree, _ := parser.Parse(src)
	defer tree.Release()

	fields := passCFPyFields(h, tree, src)
	if len(fields) != 2 {
		t.Fatalf("want 2 fields, got %d: %v", len(fields), fields)
	}
	found := map[string]string{}
	for _, f := range fields {
		found[f.fieldName] = f.sentinel
	}
	if found["timeout"] != "0" {
		t.Errorf("timeout sentinel: want 0, got %q", found["timeout"])
	}
	if found["retries"] != "3" {
		t.Errorf("retries sentinel: want 3, got %q", found["retries"])
	}
}

func TestCFPyFields_DocstringFalsePositive(t *testing.T) {
	// Oracle D1 repro: field-def appearing in docstring must NOT be extracted.
	h, err := loadCFPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`class Config(BaseModel):
    """
    timeout: int = Field(default=0)
    This documents the timeout field.
    """
    real_field: int = Field(default=5)
`)
	parser := newPyParser(h)
	tree, _ := parser.Parse(src)
	defer tree.Release()

	fields := passCFPyFields(h, tree, src)
	if len(fields) != 1 {
		t.Fatalf("want 1 field (real_field only); got %d: %v", len(fields), fields)
	}
	if fields[0].fieldName != "real_field" {
		t.Errorf("want real_field, got %s", fields[0].fieldName)
	}
}

func TestCFPyValidators_Basic(t *testing.T) {
	h, err := loadCFPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`from pydantic import BaseModel, validator

class Config(BaseModel):
    timeout: int = Field(default=0)

    @validator('timeout')
    def validate_timeout(cls, v):
        if v <= 0:
            raise ValueError('must be positive')
        return v
`)
	parser := newPyParser(h)
	tree, _ := parser.Parse(src)
	defer tree.Release()

	validators := passCFPyValidators(h, tree, src)
	if len(validators) != 1 {
		t.Fatalf("want 1 validator, got %d: %v", len(validators), validators)
	}
	if validators[0].fieldName != "timeout" {
		t.Errorf("field: want timeout, got %s", validators[0].fieldName)
	}
	if validators[0].rejectOp != "<=" {
		t.Errorf("op: want <=, got %s", validators[0].rejectOp)
	}
	if validators[0].sentinel != "0" {
		t.Errorf("sentinel: want 0, got %s", validators[0].sentinel)
	}
}

func TestCFPyValidators_NestedClassScoping(t *testing.T) {
	// Oracle D2 repro: validator in inner class must NOT be joined to outer class field.
	// Tree-sitter containment: each class_definition has its own classStart key.
	h, err := loadCFPyLangHandle()
	if err != nil {
		t.Fatalf("load handle: %v", err)
	}
	src := []byte(`from pydantic import BaseModel, validator

class Outer(BaseModel):
    timeout: int = Field(default=0)

    class Inner(BaseModel):
        port: int = Field(default=0)

        @validator('timeout')
        def validate_timeout(cls, v):
            if v <= 0:
                raise ValueError('bad')
`)
	parser := newPyParser(h)
	tree, _ := parser.Parse(src)
	defer tree.Release()

	fields := passCFPyFields(h, tree, src)
	validators := passCFPyValidators(h, tree, src)

	// The validator is inside Inner (not Outer). Its classStart must differ from Outer's.
	// Joining fields and validators on classStart must NOT join Outer.timeout with Inner's validator.
	var outerTimeout *cfPyField
	for i := range fields {
		if fields[i].className == "Outer" && fields[i].fieldName == "timeout" {
			outerTimeout = &fields[i]
		}
	}
	if outerTimeout == nil {
		t.Fatal("did not find Outer.timeout field")
	}
	for _, val := range validators {
		if val.fieldName == "timeout" && val.classStart == outerTimeout.classStart {
			t.Errorf("nested-class validator misjoined to outer class field (classStart match) — D2 FP repro")
		}
	}
}

// ─── cfPyContradicts unit tests ───────────────────────────────────────────────

func TestCFPyContradicts(t *testing.T) {
	// cfPyContradicts uses (classStart, fieldName) via the caller's join;
	// the function itself only evaluates sentinel vs rejectOp.
	// The "different classes" guard is enforced by the join loop, not here.
	cases := []struct {
		field cfPyField
		val   cfPyValidator
		want  bool
		desc  string
	}{
		{
			cfPyField{sentinel: "0"},
			cfPyValidator{rejectOp: "<=", sentinel: "0"},
			true, "default=0 rejected by <=0",
		},
		{
			cfPyField{sentinel: "5"},
			cfPyValidator{rejectOp: "<=", sentinel: "0"},
			false, "default=5 not rejected by <=0",
		},
		{
			cfPyField{sentinel: "-1"},
			cfPyValidator{rejectOp: "<", sentinel: "0"},
			true, "default=-1 rejected by <0",
		},
		{
			cfPyField{sentinel: "0"},
			cfPyValidator{rejectOp: "==", sentinel: "0"},
			true, "default=0 rejected by ==0",
		},
		{
			cfPyField{sentinel: "None"},
			cfPyValidator{rejectOp: "is", sentinel: "None"},
			true, "default=None rejected by is None",
		},
		{
			cfPyField{sentinel: "0"},
			cfPyValidator{rejectOp: ">=", sentinel: "1"},
			false, "default=0 not >=1 so not rejected",
		},
	}
	for _, tc := range cases {
		got := cfPyContradicts(tc.field, tc.val)
		if got != tc.want {
			t.Errorf("cfPyContradicts(%s): got %v, want %v", tc.desc, got, tc.want)
		}
	}
}

// ─── end-to-end fixture tests ─────────────────────────────────────────────────

func TestCFPy_Positive_PydanticZero(t *testing.T) {
	snap := pySnapFile(t, "cfpy_positive/pydantic_zero.py")
	st := openStore(t)
	var sum Summary
	if err := seedConfigFieldPyContradictions(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldPyContradictions: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) == 0 {
		t.Fatal("want ≥1 lead for pydantic contradiction fixture, got 0")
	}
	found := false
	for _, l := range leads {
		if findSubstring(l.Note, "timeout") && findSubstring(l.Note, "default=0") {
			found = true
		}
	}
	if !found {
		t.Errorf("want lead mentioning 'timeout' and 'default=0'; leads: %v", leads)
	}
}

func TestCFPy_Negative_NoContradiction(t *testing.T) {
	snap := pySnapFile(t, "cfpy_negative/no_contradiction.py")
	st := openStore(t)
	var sum Summary
	if err := seedConfigFieldPyContradictions(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldPyContradictions: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("want 0 leads for no-contradiction fixture, got %d: %v", len(leads), leads)
	}
}

func TestCFPy_Negative_DifferentClass(t *testing.T) {
	snap := pySnapFile(t, "cfpy_negative/different_class.py")
	st := openStore(t)
	var sum Summary
	if err := seedConfigFieldPyContradictions(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldPyContradictions: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("want 0 leads for different-class fixture, got %d: %v", len(leads), leads)
	}
}

// TestCFPy_Negative_Docstring is the D1 oracle repro: field-def in docstring → 0 leads.
func TestCFPy_Negative_Docstring(t *testing.T) {
	snap := pySnapFile(t, "cfpy_negative/docstring_field.py")
	st := openStore(t)
	var sum Summary
	if err := seedConfigFieldPyContradictions(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldPyContradictions: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("docstring D1 repro: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// TestCFPy_Negative_NestedClass is the D2 oracle repro: validator in inner class
// must not join outer class field — class scoping from tree-sitter startByte key.
func TestCFPy_Negative_NestedClass(t *testing.T) {
	snap := pySnapFile(t, "cfpy_negative/nested_class.py")
	st := openStore(t)
	var sum Summary
	if err := seedConfigFieldPyContradictions(context.Background(), snap, st, &sum); err != nil {
		t.Fatalf("seedConfigFieldPyContradictions: %v", err)
	}
	leads, _ := st.ListLeads(context.Background())
	if len(leads) != 0 {
		t.Errorf("nested-class D2 repro: want 0 leads, got %d: %v", len(leads), leads)
	}
}

// ─── helper ───────────────────────────────────────────────────────────────────

func newPyParser(h *cfPyLangHandle) *gts.Parser {
	return gts.NewParser(h.lang)
}
