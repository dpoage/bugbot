package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestParseToolParameters_FullSchema covers the common case: a JSON Schema
// object with a "properties" map and a "required" list. The helper should
// unwrap both into the shape the Anthropic SDK's ToolInputSchemaParam wants.
func TestParseToolParameters_FullSchema(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string"},
			"depth": {"type": "integer"}
		},
		"required": ["path"]
	}`)
	props, required, err := parseToolParameters(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := props["path"]; !ok {
		t.Errorf("props missing \"path\" key: %v", props)
	}
	if _, ok := props["depth"]; !ok {
		t.Errorf("props missing \"depth\" key: %v", props)
	}
	if !reflect.DeepEqual(required, []string{"path"}) {
		t.Errorf("required = %v, want [path]", required)
	}
}

// TestParseToolParameters_BareProperties covers the lenient case where the
// caller passed a bare properties object (no "type"/"properties" envelope).
// The helper should treat the whole object as the properties map.
func TestParseToolParameters_BareProperties(t *testing.T) {
	in := json.RawMessage(`{
		"path": {"type": "string"},
		"depth": {"type": "integer"}
	}`)
	props, required, err := parseToolParameters(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := props["path"]; !ok {
		t.Errorf("props missing \"path\" key: %v", props)
	}
	if _, ok := props["depth"]; !ok {
		t.Errorf("props missing \"depth\" key: %v", props)
	}
	if required != nil {
		t.Errorf("required = %v, want nil", required)
	}
}

// TestParseToolParameters_Empty covers the "no schema" case: a zero-length
// input should return nil values with no error, matching the per-adapter
// `if len(t.Parameters) > 0` guards the helper replaced.
func TestParseToolParameters_Empty(t *testing.T) {
	props, required, err := parseToolParameters(nil)
	if err != nil {
		t.Errorf("nil input: unexpected error: %v", err)
	}
	if props != nil {
		t.Errorf("nil input: props = %v, want nil", props)
	}
	if required != nil {
		t.Errorf("nil input: required = %v, want nil", required)
	}

	empty := json.RawMessage(``)
	props, required, err = parseToolParameters(empty)
	if err != nil {
		t.Errorf("empty input: unexpected error: %v", err)
	}
	if props != nil {
		t.Errorf("empty input: props = %v, want nil", props)
	}
	if required != nil {
		t.Errorf("empty input: required = %v, want nil", required)
	}
}

// TestParseToolParameters_Invalid covers the malformed-JSON case: the helper
// should surface the json.Unmarshal error verbatim so callers can wrap it.
func TestParseToolParameters_Invalid(t *testing.T) {
	in := json.RawMessage(`{not valid json`)
	_, _, err := parseToolParameters(in)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// TestParseResponseSchema_Valid covers the happy path: a valid JSON Schema
// unmarshals cleanly, and the returned name matches the caller's default
// exactly (the helper does NOT look inside the schema for a "name" key).
func TestParseResponseSchema_Valid(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"answer": {"type": "string"}
		}
	}`)
	schema, name, err := parseResponseSchema(in, "response")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "response" {
		t.Errorf("name = %q, want %q", name, "response")
	}
	// schema is `any`; assert the unwrapped map has the expected keys.
	m, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("schema is %T, want map[string]any", schema)
	}
	if _, ok := m["properties"]; !ok {
		t.Errorf("schema map missing \"properties\" key: %v", m)
	}
}

// TestParseResponseSchema_OverrideName covers the case where the caller
// supplies a non-default name: the helper must return it verbatim.
func TestParseResponseSchema_OverrideName(t *testing.T) {
	in := json.RawMessage(`{"type": "object"}`)
	schema, name, err := parseResponseSchema(in, "my_schema")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my_schema" {
		t.Errorf("name = %q, want %q", name, "my_schema")
	}
	if schema == nil {
		t.Errorf("schema = nil, want non-nil")
	}
}

// TestParseResponseSchema_Invalid covers the malformed-JSON case.
func TestParseResponseSchema_Invalid(t *testing.T) {
	in := json.RawMessage(`{not valid json`)
	schema, name, err := parseResponseSchema(in, "response")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if schema != nil {
		t.Errorf("schema = %v, want nil on error", schema)
	}
	if name != "" {
		t.Errorf("name = %q, want empty on error", name)
	}
}
