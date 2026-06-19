package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- helpers ----------------------------------------------------------------

func runPostLeadTool(t *testing.T, tool *PostLeadTool, args interface{}) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), raw)
}

var testLensNames = []string{
	"nil-safety/error-handling",
	"concurrency",
	"resource-leaks",
	"boundary-conditions",
	"api-contract-misuse",
	"injection/input-validation",
}

func newTestPostLeadTool(posterLens string, posted *[]postLeadCapture) *PostLeadTool {
	onPost := func(targetLens, file string, line int, note string, confidence float64) error {
		*posted = append(*posted, postLeadCapture{targetLens, file, line, note, confidence})
		return nil
	}
	return NewPostLeadTool(posterLens, testLensNames, onPost)
}

type postLeadCapture struct {
	targetLens string
	file       string
	line       int
	note       string
	confidence float64
}

// --- Tests ------------------------------------------------------------------

func TestPostLeadTool_Def(t *testing.T) {
	tool := NewPostLeadTool("concurrency", testLensNames, func(_, _ string, _ int, _ string, _ float64) error { return nil })
	def := tool.Def()
	if def.Name != "post_lead" {
		t.Errorf("name = %q, want post_lead", def.Name)
	}
	if def.Description == "" {
		t.Error("description is empty")
	}
	// Schema must be valid JSON with additionalProperties:false.
	var schema map[string]interface{}
	if err := json.Unmarshal(def.Parameters, &schema); err != nil {
		t.Fatalf("parameters schema is not valid JSON: %v", err)
	}
	if ap, ok := schema["additionalProperties"]; !ok || ap != false {
		t.Error("additionalProperties must be false")
	}
	// All four fields are required.
	req, _ := schema["required"].([]interface{})
	reqSet := map[string]bool{}
	for _, r := range req {
		reqSet[r.(string)] = true
	}
	for _, field := range []string{"target_lens", "file", "line", "note"} {
		if !reqSet[field] {
			t.Errorf("field %q not in required list", field)
		}
	}
}

func TestPostLeadTool_ValidPost(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	out, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "pkg/cache.go",
		"line":        42,
		"note":        "locking around cache map looks inconsistent",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out, "concurrency") {
		t.Errorf("output missing target lens: %q", out)
	}
	if len(captured) != 1 {
		t.Fatalf("want 1 callback, got %d", len(captured))
	}
	c := captured[0]
	if c.targetLens != "concurrency" {
		t.Errorf("targetLens = %q", c.targetLens)
	}
	if c.file != "pkg/cache.go" {
		t.Errorf("file = %q", c.file)
	}
	if c.line != 42 {
		t.Errorf("line = %d", c.line)
	}
	if c.note != "locking around cache map looks inconsistent" {
		t.Errorf("note = %q", c.note)
	}
}

func TestPostLeadTool_PostToOwnLens_Allowed(t *testing.T) {
	// Posting to the poster's own lens is allowed (the description discourages it
	// but it is not an error — a finder may post a meta-observation).
	var captured []postLeadCapture
	tool := newTestPostLeadTool("concurrency", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "a.go",
		"line":        1,
		"note":        "self-directed note",
	})
	if err != nil {
		t.Errorf("posting to own lens should be allowed, got error: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("want 1 callback, got %d", len(captured))
	}
}

func TestPostLeadTool_UnknownTargetLens_ListsValidNames(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "garbage-lens",
		"file":        "a.go",
		"line":        1,
		"note":        "some note",
	})
	if err == nil {
		t.Fatal("unknown target_lens must return an error")
	}
	// Error message must list valid lens names.
	for _, name := range testLensNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error message missing lens name %q: %v", name, err)
		}
	}
	if len(captured) != 0 {
		t.Errorf("callback should not be called on invalid args")
	}
}

func TestPostLeadTool_LineZero_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "a.go",
		"line":        0,
		"note":        "some note",
	})
	if err == nil {
		t.Fatal("line 0 must return an error")
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error should mention line: %v", err)
	}
	if len(captured) != 0 {
		t.Errorf("callback should not be called on invalid args")
	}
}

func TestPostLeadTool_NegativeLine_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "a.go",
		"line":        -1,
		"note":        "some note",
	})
	if err == nil {
		t.Fatal("negative line must return an error")
	}
}

func TestPostLeadTool_EmptyNote_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "a.go",
		"line":        1,
		"note":        "",
	})
	if err == nil {
		t.Fatal("empty note must return an error")
	}
	if !strings.Contains(err.Error(), "note") {
		t.Errorf("error should mention note: %v", err)
	}
}

func TestPostLeadTool_WhitespaceOnlyNote_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "a.go",
		"line":        1,
		"note":        "   ",
	})
	if err == nil {
		t.Fatal("whitespace-only note must return an error")
	}
}

func TestPostLeadTool_EmptyFile_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "",
		"line":        1,
		"note":        "some note",
	})
	if err == nil {
		t.Fatal("empty file must return an error")
	}
}

func TestPostLeadTool_AbsoluteFile_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "/absolute/path/file.go",
		"line":        1,
		"note":        "some note",
	})
	if err == nil {
		t.Fatal("absolute path must return an error")
	}
}

func TestPostLeadTool_InvalidJSON_IsError(t *testing.T) {
	tool := NewPostLeadTool("nil-safety/error-handling", testLensNames, func(_, _ string, _ int, _ string, _ float64) error { return nil })
	_, err := tool.Run(context.Background(), []byte(`not json`))
	if err == nil {
		t.Fatal("invalid JSON must return an error")
	}
}

// TestPostLeadTool_DotDotFile_IsError pins that ".." traversal is rejected: a
// lead must point inside the repository.
func TestPostLeadTool_DotDotFile_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("concurrency", &captured)
	for _, file := range []string{"../etc/passwd", "a/../../b.go", ".."} {
		_, err := runPostLeadTool(t, tool, map[string]interface{}{
			"target_lens": "concurrency",
			"file":        file,
			"line":        1,
			"note":        "n",
		})
		if err == nil {
			t.Errorf("file %q escaping the repo must return an error", file)
		}
	}
	if len(captured) != 0 {
		t.Errorf("onPost must not fire for rejected paths, fired %d times", len(captured))
	}
}

// TestPostLeadTool_OverlongNote_IsError pins the note length cap (the note is
// rendered into a future finder prompt; see maxLeadNoteLen).
func TestPostLeadTool_OverlongNote_IsError(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("concurrency", &captured)
	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "a.go",
		"line":        1,
		"note":        strings.Repeat("x", maxLeadNoteLen+1),
	})
	if err == nil {
		t.Fatal("overlong note must return an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should state the cap, got: %v", err)
	}
	if len(captured) != 0 {
		t.Errorf("onPost must not fire for rejected note, fired %d times", len(captured))
	}
}

// TestPostLeadTool_ConfidenceForwarded verifies that an explicit confidence
// value is forwarded to the onPost callback unchanged.
func TestPostLeadTool_ConfidenceForwarded(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	out, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "pkg/cache.go",
		"line":        10,
		"note":        "suspicious locking",
		"confidence":  0.75,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out, "concurrency") {
		t.Errorf("output missing target lens: %q", out)
	}
	if len(captured) != 1 {
		t.Fatalf("want 1 callback, got %d", len(captured))
	}
	if captured[0].confidence != 0.75 {
		t.Errorf("confidence = %v, want 0.75", captured[0].confidence)
	}
}

// TestPostLeadTool_ConfidenceAbsent verifies that an absent confidence field
// defaults to 0 in the callback.
func TestPostLeadTool_ConfidenceAbsent(t *testing.T) {
	var captured []postLeadCapture
	tool := newTestPostLeadTool("nil-safety/error-handling", &captured)

	_, err := runPostLeadTool(t, tool, map[string]interface{}{
		"target_lens": "concurrency",
		"file":        "pkg/cache.go",
		"line":        1,
		"note":        "suspicion",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("want 1 callback, got %d", len(captured))
	}
	if captured[0].confidence != 0.0 {
		t.Errorf("confidence = %v, want 0.0", captured[0].confidence)
	}
}

// TestPostLeadTool_ConfidenceClamped verifies that out-of-range confidence
// values are clamped to [0,1].
func TestPostLeadTool_ConfidenceClamped(t *testing.T) {
	tests := []struct {
		name  string
		input float64
		want  float64
	}{
		{"negative", -0.5, 0.0},
		{"above one", 1.5, 1.0},
		{"exactly zero", 0.0, 0.0},
		{"exactly one", 1.0, 1.0},
		{"midrange", 0.4, 0.4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured []postLeadCapture
			tool := newTestPostLeadTool("nil-safety/error-handling", &captured)
			_, err := runPostLeadTool(t, tool, map[string]interface{}{
				"target_lens": "concurrency",
				"file":        "a.go",
				"line":        1,
				"note":        "note",
				"confidence":  tc.input,
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(captured) != 1 {
				t.Fatalf("want 1 capture, got %d", len(captured))
			}
			if captured[0].confidence != tc.want {
				t.Errorf("confidence = %v, want %v", captured[0].confidence, tc.want)
			}
		})
	}
}
