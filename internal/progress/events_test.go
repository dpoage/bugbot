package progress

import "testing"

// TestEventValidate checks the documented Kind/field invariants.
func TestEventValidate(t *testing.T) {
	// Empty Kind must fail.
	if err := (Event{}).Validate(); err == nil {
		t.Error("Validate: expected error for empty Kind")
	}

	// KindStageStarted without Stage must fail.
	if err := (Event{Kind: KindStageStarted}).Validate(); err == nil {
		t.Error("Validate: KindStageStarted without Stage should fail")
	}
	// KindStageStarted with Stage must pass.
	if err := (Event{Kind: KindStageStarted, Stage: StageHypothesize}).Validate(); err != nil {
		t.Errorf("Validate: KindStageStarted with Stage unexpected error: %v", err)
	}

	// KindAgentFinished without Role/Label must fail.
	if err := (Event{Kind: KindAgentFinished}).Validate(); err == nil {
		t.Error("Validate: KindAgentFinished without Role/Label should fail")
	}
	if err := (Event{Kind: KindAgentFinished, Role: RoleFinder}).Validate(); err == nil {
		t.Error("Validate: KindAgentFinished without Label should fail")
	}
	// KindAgentFinished with both must pass.
	if err := (Event{Kind: KindAgentFinished, Role: RoleFinder, Label: "lens"}).Validate(); err != nil {
		t.Errorf("Validate: KindAgentFinished valid event unexpected error: %v", err)
	}

	// KindAgentActivity requires Activity too.
	if err := (Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lens"}).Validate(); err == nil {
		t.Error("Validate: KindAgentActivity without Activity should fail")
	}
	if err := (Event{Kind: KindAgentActivity, Role: RoleFinder, Label: "lens", Activity: "reading"}).Validate(); err != nil {
		t.Errorf("Validate: KindAgentActivity valid event unexpected error: %v", err)
	}

	// KindFindingVerified without File must fail.
	if err := (Event{Kind: KindFindingVerified}).Validate(); err == nil {
		t.Error("Validate: KindFindingVerified without File should fail")
	}
	if err := (Event{Kind: KindFindingVerified, File: "main.go"}).Validate(); err != nil {
		t.Errorf("Validate: KindFindingVerified with File unexpected error: %v", err)
	}

	// KindScanStarted has no mandatory sub-fields — must pass with just Kind.
	if err := (Event{Kind: KindScanStarted}).Validate(); err != nil {
		t.Errorf("Validate: KindScanStarted unexpected error: %v", err)
	}

	// KindToolUnhealthy requires Role, Label, Tool, and Severity. Each missing
	// field must fail; the fully-populated event must pass.
	if err := (Event{Kind: KindToolUnhealthy}).Validate(); err == nil {
		t.Error("Validate: KindToolUnhealthy without any fields should fail")
	}
	if err := (Event{Kind: KindToolUnhealthy, Role: RoleFinder, Label: "lens", Tool: "sandbox", Severity: "high"}).Validate(); err != nil {
		t.Errorf("Validate: KindToolUnhealthy fully populated unexpected error: %v", err)
	}
	if err := (Event{Kind: KindToolUnhealthy, Label: "lens", Tool: "sandbox", Severity: "high"}).Validate(); err == nil {
		t.Error("Validate: KindToolUnhealthy without Role should fail")
	}
	if err := (Event{Kind: KindToolUnhealthy, Role: RoleFinder, Tool: "sandbox", Severity: "high"}).Validate(); err == nil {
		t.Error("Validate: KindToolUnhealthy without Label should fail")
	}
	if err := (Event{Kind: KindToolUnhealthy, Role: RoleFinder, Label: "lens", Severity: "high"}).Validate(); err == nil {
		t.Error("Validate: KindToolUnhealthy without Tool should fail")
	}
	if err := (Event{Kind: KindToolUnhealthy, Role: RoleFinder, Label: "lens", Tool: "sandbox"}).Validate(); err == nil {
		t.Error("Validate: KindToolUnhealthy without Severity should fail")
	}
}

// TestCountsNilSemantics documents and exercises the nil-vs-zero distinction.
func TestCountsNilSemantics(t *testing.T) {
	// Nil Counts: no accounting available.
	e := Event{Kind: KindStageFinished, Stage: StageVerify, Counts: nil}
	if e.Counts != nil {
		t.Error("nil Counts should remain nil")
	}

	// Non-nil zero Counts: stage ran but produced nothing.
	e2 := Event{Kind: KindStageFinished, Stage: StageVerify, Counts: &Counts{}}
	if e2.Counts == nil {
		t.Error("non-nil zero Counts should not be nil")
	}
	if e2.Counts.Verified != 0 {
		t.Errorf("zero Counts.Verified should be 0, got %d", e2.Counts.Verified)
	}
}
