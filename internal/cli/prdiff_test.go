package cli

import "testing"

// TestParseUnifiedDiff_AddedAndContext confirms added and context lines inside a
// single hunk are recorded as commentable, with the RIGHT-side numbering taken
// from the hunk header.
func TestParseUnifiedDiff_AddedAndContext(t *testing.T) {
	diff := `diff --git a/foo.go b/foo.go
index 111..222 100644
--- a/foo.go
+++ b/foo.go
@@ -10,3 +10,4 @@ func foo() {
 ctx line at 10
+added at 11
 ctx line at 12
 ctx line at 13
`
	c := parseUnifiedDiff([]byte(diff))
	for _, ln := range []int{10, 11, 12, 13} {
		if !c.has("foo.go", ln) {
			t.Errorf("expected foo.go:%d commentable", ln)
		}
	}
	if c.has("foo.go", 14) {
		t.Errorf("foo.go:14 is outside the hunk; should not be commentable")
	}
}

// TestParseUnifiedDiff_DeletedLineNoRightSide confirms deleted lines do not
// advance the new-file counter and are not commentable.
func TestParseUnifiedDiff_DeletedLineNoRightSide(t *testing.T) {
	diff := `--- a/bar.go
+++ b/bar.go
@@ -5,4 +5,3 @@
 keep at 5
-removed (no right side)
 keep at 6
 keep at 7
`
	c := parseUnifiedDiff([]byte(diff))
	if !c.has("bar.go", 5) || !c.has("bar.go", 6) || !c.has("bar.go", 7) {
		t.Errorf("context lines 5,6,7 should be commentable: %#v", c["bar.go"])
	}
	// Only 3 lines exist on the right; 8 must not.
	if c.has("bar.go", 8) {
		t.Errorf("bar.go:8 should not be commentable")
	}
}

// TestParseUnifiedDiff_MultiHunk confirms numbering resets per hunk header.
func TestParseUnifiedDiff_MultiHunk(t *testing.T) {
	diff := `--- a/baz.go
+++ b/baz.go
@@ -1,2 +1,2 @@
 a at 1
+b at 2
@@ -50,2 +60,2 @@
 x at 60
+y at 61
`
	c := parseUnifiedDiff([]byte(diff))
	for _, ln := range []int{1, 2, 60, 61} {
		if !c.has("baz.go", ln) {
			t.Errorf("expected baz.go:%d commentable", ln)
		}
	}
	if c.has("baz.go", 3) || c.has("baz.go", 50) {
		t.Errorf("lines between hunks must not be commentable")
	}
}

// TestParseUnifiedDiff_NewFile confirms a brand-new file (all added lines) is
// fully commentable and attributed to the +++ path.
func TestParseUnifiedDiff_NewFile(t *testing.T) {
	diff := `--- /dev/null
+++ b/new.go
@@ -0,0 +1,3 @@
+package main
+
+func main() {}
`
	c := parseUnifiedDiff([]byte(diff))
	for _, ln := range []int{1, 2, 3} {
		if !c.has("new.go", ln) {
			t.Errorf("new file line %d should be commentable", ln)
		}
	}
}

// TestParseUnifiedDiff_Deletion confirms a deleted file (+++ /dev/null) yields no
// commentable lines.
func TestParseUnifiedDiff_Deletion(t *testing.T) {
	diff := `--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-line one
-line two
`
	c := parseUnifiedDiff([]byte(diff))
	if len(c) != 0 {
		t.Errorf("a pure deletion should produce no commentable lines, got %#v", c)
	}
}

// TestParseUnifiedDiff_RenameWithEdit confirms a renamed-and-edited file's hunks
// are attributed to the NEW path from the +++ header.
func TestParseUnifiedDiff_RenameWithEdit(t *testing.T) {
	diff := `diff --git a/old/name.go b/new/name.go
similarity index 90%
rename from old/name.go
rename to new/name.go
--- a/old/name.go
+++ b/new/name.go
@@ -3,3 +3,4 @@
 unchanged at 3
+inserted at 4
 unchanged at 5
 unchanged at 6
`
	c := parseUnifiedDiff([]byte(diff))
	if !c.has("new/name.go", 4) {
		t.Errorf("renamed file's new path should be commentable at the inserted line")
	}
	if c.has("old/name.go", 4) {
		t.Errorf("old path must not be commentable after rename")
	}
}

// TestParseUnifiedDiff_EmptyContextLine confirms an empty context line inside a
// hunk (rendered as a bare empty line) still advances and is commentable.
func TestParseUnifiedDiff_EmptyContextLine(t *testing.T) {
	diff := "--- a/e.go\n+++ b/e.go\n@@ -1,3 +1,3 @@\n line 1\n\n line 3\n"
	c := parseUnifiedDiff([]byte(diff))
	for _, ln := range []int{1, 2, 3} {
		if !c.has("e.go", ln) {
			t.Errorf("e.go:%d should be commentable (empty context line at 2)", ln)
		}
	}
}
