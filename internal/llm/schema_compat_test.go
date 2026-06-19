package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/dpoage/bugbot/internal/config"
)

// mapSchemaForTest mirrors the real reproducer/patch "files" contract: a
// free-form string map expressed as an object-valued additionalProperties,
// wrapped in a root object that itself closes with additionalProperties:false.
// This is the exact shape MiniMax rejects with a 400 (see bugbot-d80).
var mapSchemaForTest = json.RawMessage(`{
  "type":"object",
  "properties":{
    "files":{"type":"object","additionalProperties":{"type":"string"},"minProperties":1}
  },
  "required":["files"],
  "additionalProperties":false
}`)

func mustUnmarshalSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return m
}

func childMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	c, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, want object", key, m[key])
	}
	return c
}

// TestCoerceBoolAdditionalProperties exercises the schema-aware downgrade in
// isolation: object/array additionalProperties become boolean true, booleans
// (including false) are preserved, and the walk never touches additionalProperties
// that appears as a PROPERTY NAME or as instance data inside enum/const.
func TestCoerceBoolAdditionalProperties(t *testing.T) {
	t.Run("object additionalProperties downgraded, bool false preserved", func(t *testing.T) {
		m := mustUnmarshalSchema(t, string(mapSchemaForTest))
		coerceBoolAdditionalProperties(m)
		files := childMap(t, childMap(t, m, "properties"), "files")
		if files["additionalProperties"] != true {
			t.Errorf("files.additionalProperties = %#v, want true", files["additionalProperties"])
		}
		if m["additionalProperties"] != false {
			t.Errorf("root additionalProperties = %#v, want false (preserved)", m["additionalProperties"])
		}
		// Unrelated constraints survive the rewrite.
		if _, ok := files["minProperties"]; !ok {
			t.Error("files.minProperties dropped by rewrite")
		}
	})

	t.Run("boolean additionalProperties passed through", func(t *testing.T) {
		for _, raw := range []string{`{"additionalProperties":true}`, `{"additionalProperties":false}`} {
			m := mustUnmarshalSchema(t, raw)
			want := m["additionalProperties"]
			coerceBoolAdditionalProperties(m)
			if m["additionalProperties"] != want {
				t.Errorf("%s: additionalProperties = %#v, want %#v", raw, m["additionalProperties"], want)
			}
		}
	})

	t.Run("array-valued additionalProperties downgraded to true", func(t *testing.T) {
		m := mustUnmarshalSchema(t, `{"additionalProperties":["weird"]}`)
		coerceBoolAdditionalProperties(m)
		if m["additionalProperties"] != true {
			t.Errorf("additionalProperties = %#v, want true", m["additionalProperties"])
		}
	})

	t.Run("nested in items and $defs", func(t *testing.T) {
		m := mustUnmarshalSchema(t, `{
		  "type":"array",
		  "items":{"type":"object","additionalProperties":{"type":"number"}},
		  "$defs":{"M":{"type":"object","additionalProperties":{"type":"string"}}}
		}`)
		coerceBoolAdditionalProperties(m)
		if childMap(t, m, "items")["additionalProperties"] != true {
			t.Error("items.additionalProperties not downgraded")
		}
		if childMap(t, childMap(t, m, "$defs"), "M")["additionalProperties"] != true {
			t.Error("$defs.M.additionalProperties not downgraded")
		}
	})

	t.Run("property literally named additionalProperties is untouched", func(t *testing.T) {
		// A schema describing an object that HAS a property called
		// "additionalProperties" — the inner value is a subschema, not the keyword.
		m := mustUnmarshalSchema(t, `{"type":"object","properties":{"additionalProperties":{"type":"string"}}}`)
		coerceBoolAdditionalProperties(m)
		prop := childMap(t, m, "properties")
		ap, ok := prop["additionalProperties"].(map[string]any)
		if !ok {
			t.Fatalf("property named additionalProperties was corrupted to %#v", prop["additionalProperties"])
		}
		if ap["type"] != "string" {
			t.Errorf("property schema mangled: %#v", ap)
		}
	})

	t.Run("object literals in enum and const are untouched", func(t *testing.T) {
		m := mustUnmarshalSchema(t, `{
		  "enum":[{"additionalProperties":{"x":1}}],
		  "const":{"additionalProperties":{"y":2}}
		}`)
		coerceBoolAdditionalProperties(m)
		enum, ok := m["enum"].([]any)
		if !ok || len(enum) != 1 {
			t.Fatalf("enum mangled: %#v", m["enum"])
		}
		if _, ok := enum[0].(map[string]any)["additionalProperties"].(map[string]any); !ok {
			t.Errorf("enum literal additionalProperties coerced: %#v", enum[0])
		}
		if _, ok := childMap(t, m, "const")["additionalProperties"].(map[string]any); !ok {
			t.Errorf("const literal additionalProperties coerced: %#v", m["const"])
		}
	})
}

// captureBody starts a server that records the request body as a decoded map and
// replies with a minimal successful completion.
func captureBody(t *testing.T, captured *map[string]any) string {
	t.Helper()
	return newServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mockTextBody("openai-compatible", "ok", 1, 1)))
	})
}

// TestSchemaCompat_OpenAICompatible_DowngradesResponseFormat asserts that an
// openai-compatible adapter with the downgrade flag rewrites object-valued
// additionalProperties to boolean true in the response_format schema on the
// wire, while the root additionalProperties:false is preserved.
func TestSchemaCompat_OpenAICompatible_DowngradesResponseFormat(t *testing.T) {
	var captured map[string]any
	base := captureBody(t, &captured)
	adapter := newOpenAIAdapter("llama-test", openaiOptions{
		apiKey:                     "k",
		baseURL:                    base,
		provider:                   "openai-compatible",
		caps:                       withCaps(openAICompatibleCapabilities(), true),
		requireBoolAdditionalProps: true,
	})
	req := simpleRequest()
	req.ResponseSchema = mapSchemaForTest
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	schema := childMap(t, childMap(t, childMap(t, captured, "response_format"), "json_schema"), "schema")
	files := childMap(t, childMap(t, schema, "properties"), "files")
	if files["additionalProperties"] != true {
		t.Errorf("wire files.additionalProperties = %#v, want true", files["additionalProperties"])
	}
	if schema["additionalProperties"] != false {
		t.Errorf("wire root additionalProperties = %#v, want false", schema["additionalProperties"])
	}
}

// TestSchemaCompat_OpenAICompatible_DowngradesToolParameters asserts the same
// downgrade is applied to tool parameter schemas — the sandbox_exec tool's
// "files" argument is the latent case this protects.
func TestSchemaCompat_OpenAICompatible_DowngradesToolParameters(t *testing.T) {
	var captured map[string]any
	base := captureBody(t, &captured)
	adapter := newOpenAIAdapter("llama-test", openaiOptions{
		apiKey:                     "k",
		baseURL:                    base,
		provider:                   "openai-compatible",
		caps:                       withCaps(openAICompatibleCapabilities(), true),
		requireBoolAdditionalProps: true,
	})
	req := simpleRequest()
	req.Tools = []ToolDef{{Name: "sandbox_exec", Parameters: mapSchemaForTest}}
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools missing or wrong shape: %#v", captured["tools"])
	}
	params := childMap(t, childMap(t, tools[0].(map[string]any), "function"), "parameters")
	files := childMap(t, childMap(t, params, "properties"), "files")
	if files["additionalProperties"] != true {
		t.Errorf("wire tool files.additionalProperties = %#v, want true", files["additionalProperties"])
	}
}

// TestSchemaCompat_OpenAI_PreservesSubschema locks the blast radius: a
// first-party OpenAI adapter (flag off) leaves the object-valued
// additionalProperties intact, because OpenAI accepts the subschema form.
func TestSchemaCompat_OpenAI_PreservesSubschema(t *testing.T) {
	var captured map[string]any
	base := captureBody(t, &captured)
	adapter := newOpenAIAdapter("gpt-test", openaiOptions{
		apiKey:   "k",
		baseURL:  base,
		provider: "openai",
		caps:     openAICapabilities("gpt-test"),
		// requireBoolAdditionalProps deliberately left false.
	})
	req := simpleRequest()
	req.ResponseSchema = mapSchemaForTest
	if _, err := adapter.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	schema := childMap(t, childMap(t, childMap(t, captured, "response_format"), "json_schema"), "schema")
	files := childMap(t, childMap(t, schema, "properties"), "files")
	if _, ok := files["additionalProperties"].(map[string]any); !ok {
		t.Errorf("openai files.additionalProperties = %#v, want preserved object", files["additionalProperties"])
	}
}

// TestSchemaCompat_Registry_OpenAICompatibleEnablesDowngrade exercises the full
// factory path: a provider configured as openai-compatible (with structured
// output forced on) must downgrade object additionalProperties on the wire,
// proving the registry wires requireBoolAdditionalProps=true.
func TestSchemaCompat_Registry_OpenAICompatibleEnablesDowngrade(t *testing.T) {
	var captured map[string]any
	base := captureBody(t, &captured)
	on := true
	client, err := NewClient(context.Background(), config.Provider{
		Type:             config.ProviderOpenAICompatible,
		BaseURL:          base,
		StructuredOutput: &on,
	}, "minimax", "MiniMax-M3", "k", Options{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	req := simpleRequest()
	req.ResponseSchema = mapSchemaForTest
	if _, err := client.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	schema := childMap(t, childMap(t, childMap(t, captured, "response_format"), "json_schema"), "schema")
	files := childMap(t, childMap(t, schema, "properties"), "files")
	if files["additionalProperties"] != true {
		t.Errorf("registry wire files.additionalProperties = %#v, want true", files["additionalProperties"])
	}
}
