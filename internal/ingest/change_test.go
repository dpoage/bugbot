package ingest

import (
	"context"
	"strings"
	"testing"
)

// TestChangedFiles_DashDashFlagInjectionGuard verifies that "--end-of-options"
// is present in the git argv for ChangedFiles, preventing a ref that starts
// with "-" from being parsed as a flag. The positive case (a normal SHA
// works) proves the marker does not break the normal path; the negative case
// (a ref starting with "-" returning an error rather than being treated as a
// flag) is the actual injection guard — git treats the value after
// "--end-of-options" as a ref even if it begins with "-", so a non-existent
// dash-ref surfaces as a "bad revision" / "unknown revision" error from git
// instead of being silently swallowed as an unknown option.
func TestChangedFiles_DashDashFlagInjectionGuard(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	base := r.commit("base commit")

	r.write("b.go", "package b\n")
	head := r.commit("head commit")

	repo := r.open()
	// Normal SHAs must still work with "--end-of-options" present.
	changes, err := repo.ChangedFiles(context.Background(), base, head)
	if err != nil {
		t.Fatalf("ChangedFiles with full SHAs: %v", err)
	}
	if len(changes) == 0 {
		t.Errorf("expected at least one change between %s and %s, got 0", base, head)
	}

	// A from-ref starting with '-' must return an error (the ref is treated
	// as a path/rev after "--end-of-options" and is not found), NOT be
	// silently interpreted as a git flag.
	_, err = repo.ChangedFiles(context.Background(), "-fake-flag", head)
	if err == nil {
		t.Fatal("ChangedFiles with from='-fake-flag' should return an error (not parsed as flag)")
	}
	// Sanity: the error should not look like a flag-parsing success — git's
	// revision-resolution path produces a message that mentions the ref
	// (e.g. "unknown revision" / "bad revision"). We do not pin the exact
	// text, but we do require the offending ref string to appear so a
	// future regression to a silent flag swallow is caught.
	if !strings.Contains(err.Error(), "-fake-flag") {
		t.Errorf("error should mention the dash-ref %q, got: %v", "-fake-flag", err)
	}
}

// TestUnifiedDiff_DashDashFlagInjectionGuard verifies that "--end-of-options"
// is present in the git argv for UnifiedDiff, preventing a `from` ref that
// starts with "-" from being parsed as a flag (the joined from...to range
// string leads with `from` verbatim, so "--output=/x" would otherwise become
// the option "--output=/x...HEAD"). The positive case proves the marker
// does not break the normal path; the negative case proves a dash-leading
// from-ref is treated as a bad revision rather than silently swallowed as an
// option.
func TestUnifiedDiff_DashDashFlagInjectionGuard(t *testing.T) {
	r := newTestRepo(t)
	r.write("main.go", "package main\n\nfunc main() {}\n")
	base := r.commit("base")
	r.write("main.go", "package main\n\nfunc main() { println(\"x\") }\n")
	head := r.commit("head")

	repo := r.open()
	// Normal SHAs must still work with "--end-of-options" present.
	diff, err := repo.UnifiedDiff(context.Background(), base, head)
	if err != nil {
		t.Fatalf("UnifiedDiff with full SHAs: %v", err)
	}
	if !strings.Contains(string(diff), "+++ b/main.go") {
		t.Errorf("expected diff to name post-image path:\n%s", diff)
	}

	// A from-ref starting with '-' must return an error. Without the
	// marker, git would parse "-fake-flag...HEAD" as the option
	// "-fake-flag...HEAD" and the call could either succeed silently
	// (swallowing the user's intent) or fail with an unknown-option error;
	// either is a security/correctness regression. With the marker, git
	// treats the entire "-fake-flag...HEAD" as a revision spec and errors
	// out as an unknown / bad revision.
	_, err = repo.UnifiedDiff(context.Background(), "-fake-flag", head)
	if err == nil {
		t.Fatal("UnifiedDiff with from='-fake-flag' should return an error (not parsed as flag)")
	}
	// Sanity: the error should mention the offending ref, proving git saw
	// it as a revision and not an option.
	if !strings.Contains(err.Error(), "-fake-flag") {
		t.Errorf("error should mention the dash-ref %q, got: %v", "-fake-flag", err)
	}
}

// TestChangeValidate checks the OldPath invariant on Change.
func TestChangeValidate(t *testing.T) {
	// NewRename must have OldPath set and pass Validate.
	r := NewRename("old/file.go", "new/file.go")
	if r.OldPath == "" {
		t.Error("NewRename: OldPath must not be empty")
	}
	if r.Kind != ChangeRenamed {
		t.Errorf("NewRename: Kind=%v, want ChangeRenamed", r.Kind)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("NewRename.Validate() unexpected error: %v", err)
	}

	// NewChange for non-rename kinds must leave OldPath empty and pass Validate.
	for _, kind := range []ChangeKind{ChangeAdded, ChangeModified, ChangeDeleted} {
		c := NewChange(kind, "some/file.go")
		if c.OldPath != "" {
			t.Errorf("NewChange(%s): OldPath should be empty, got %q", kind, c.OldPath)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("NewChange(%s).Validate() unexpected error: %v", kind, err)
		}
	}

	// A manually constructed non-rename Change with OldPath set must fail Validate.
	bad := Change{Kind: ChangeAdded, Path: "a.go", OldPath: "b.go"}
	if err := bad.Validate(); err == nil {
		t.Error("Validate: expected error for non-rename with OldPath set, got nil")
	}

	// A manually constructed ChangeRenamed with no OldPath must fail Validate.
	badRename := Change{Kind: ChangeRenamed, Path: "new.go"}
	if err := badRename.Validate(); err == nil {
		t.Error("Validate: expected error for ChangeRenamed without OldPath, got nil")
	}
}

// TestParseNameStatusZ_RenameCarriesOldPath verifies that the parser produces
// valid Changes (OldPath set IFF Kind==ChangeRenamed).
func TestParseNameStatusZ_RenameCarriesOldPath(t *testing.T) {
	// NUL-delimited: R100\x00old.go\x00new.go\x00A\x00added.go\x00
	input := "R100\x00old.go\x00new.go\x00A\x00added.go\x00"
	changes, err := parseNameStatusZ([]byte(input))
	if err != nil {
		t.Fatalf("parseNameStatusZ: %v", err)
	}
	for _, c := range changes {
		if err := c.Validate(); err != nil {
			t.Errorf("parseNameStatusZ produced invalid Change: %v", err)
		}
	}
	// Confirm rename has OldPath and non-rename does not.
	var foundRename, foundAdded bool
	for _, c := range changes {
		switch c.Kind {
		case ChangeRenamed:
			foundRename = true
			if c.OldPath == "" {
				t.Error("rename Change: OldPath is empty")
			}
		case ChangeAdded:
			foundAdded = true
			if c.OldPath != "" {
				t.Errorf("added Change: OldPath should be empty, got %q", c.OldPath)
			}
		}
	}
	if !foundRename {
		t.Error("expected a ChangeRenamed in output")
	}
	if !foundAdded {
		t.Error("expected a ChangeAdded in output")
	}
}
