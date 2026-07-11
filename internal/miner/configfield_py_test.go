package miner

import (
	"context"
	"testing"
)

// ─── unit tests for field/validator extraction ────────────────────────────────

func TestPyCFFields_Basic(t *testing.T) {
	src := `from pydantic import BaseModel, Field

class Config(BaseModel):
    timeout: int = Field(default=0)
    retries: int = Field(default=3)
`
	fields := passPyCFFields("test.py", src)
	if len(fields) != 2 {
		t.Fatalf("want 2 fields, got %d: %v", len(fields), fields)
	}
	if fields[0].fieldName != "timeout" || fields[0].sentinel != "0" {
		t.Errorf("field[0]: want timeout/0, got %v", fields[0])
	}
	if fields[1].fieldName != "retries" || fields[1].sentinel != "3" {
		t.Errorf("field[1]: want retries/3, got %v", fields[1])
	}
}

func TestPyCFValidators_Basic(t *testing.T) {
	src := `from pydantic import BaseModel, validator

class Config(BaseModel):
    timeout: int = Field(default=0)

    @validator('timeout')
    def validate_timeout(cls, v):
        if v <= 0:
            raise ValueError('must be positive')
        return v
`
	validators := passPyCFValidators("test.py", src)
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

func TestCFPyContradicts(t *testing.T) {
	cases := []struct {
		field pyCFField
		val   pyCFValidator
		want  bool
		desc  string
	}{
		{
			pyCFField{className: "C", fieldName: "x", sentinel: "0"},
			pyCFValidator{className: "C", fieldName: "x", rejectOp: "<=", sentinel: "0"},
			true, "default=0 rejected by <=0",
		},
		{
			pyCFField{className: "C", fieldName: "x", sentinel: "5"},
			pyCFValidator{className: "C", fieldName: "x", rejectOp: "<=", sentinel: "0"},
			false, "default=5 not rejected by <=0",
		},
		{
			pyCFField{className: "C", fieldName: "x", sentinel: "-1"},
			pyCFValidator{className: "C", fieldName: "x", rejectOp: "<", sentinel: "0"},
			true, "default=-1 rejected by <0",
		},
		{
			pyCFField{className: "C", fieldName: "x", sentinel: "0"},
			pyCFValidator{className: "C", fieldName: "x", rejectOp: "==", sentinel: "0"},
			true, "default=0 rejected by ==0",
		},
		{
			pyCFField{className: "C", fieldName: "x", sentinel: "None"},
			pyCFValidator{className: "C", fieldName: "x", rejectOp: "is None", sentinel: "None"},
			true, "default=None rejected by is None",
		},
		{
			pyCFField{className: "A", fieldName: "x", sentinel: "0"},
			pyCFValidator{className: "B", fieldName: "x", rejectOp: "<=", sentinel: "0"},
			false, "different classes: no join",
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
