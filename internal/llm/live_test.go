//go:build live

// Package llm live conformance probe.
//
// This is a REAL-API smoke test for any OpenAI-compatible backend (MiniMax M3,
// Groq, vLLM, Ollama, ...). It is gated behind the `live` build tag and is
// skipped entirely unless the LLM_LIVE_* environment variables are set, so it
// never runs in normal `go test ./...` and never needs a key in CI. The
// orchestrator runs it with credentials:
//
//	LLM_LIVE_BASE_URL=https://api.minimax.io/v1 \
//	LLM_LIVE_MODEL=MiniMax-M3 \
//	LLM_LIVE_API_KEY=sk-... \
//	go test -tags live ./internal/llm/ -run TestLive -v
//
// It builds the client exactly the way production does (a config.Provider of
// type "openai-compatible" routed through llm.NewClient), so it exercises the
// real construction path — retry + recorder wrappers and all — not a shortcut.
// Prompts are kept tiny and max_tokens modest: this costs real money.
package llm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpoage/bugbot/internal/config"
)

// captureRecorder is a concurrency-safe Recorder that buffers every UsageEvent
// for later assertion.
type captureRecorder struct {
	mu  sync.Mutex
	evs []UsageEvent
}

func (c *captureRecorder) Record(ev UsageEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
}

func (c *captureRecorder) events() []UsageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]UsageEvent, len(c.evs))
	copy(out, c.evs)
	return out
}

// liveEnv holds the resolved live-probe configuration.
type liveEnv struct {
	baseURL string
	model   string
	apiKey  string
}

// liveAPIKeyEnvVar is the env var name the test wires into the synthetic
// config.Provider so NewClient's production construction path resolves the key
// the same way ResolveRole would in production.
const liveAPIKeyEnvVar = "LLM_LIVE_API_KEY"

// requireLiveEnv reads the live-probe environment and skips the test with a
// clear message when any required variable is absent.
func requireLiveEnv(t *testing.T) liveEnv {
	t.Helper()
	env := liveEnv{
		baseURL: os.Getenv("LLM_LIVE_BASE_URL"),
		model:   os.Getenv("LLM_LIVE_MODEL"),
		apiKey:  os.Getenv(liveAPIKeyEnvVar),
	}
	var missing []string
	if env.baseURL == "" {
		missing = append(missing, "LLM_LIVE_BASE_URL")
	}
	if env.model == "" {
		missing = append(missing, "LLM_LIVE_MODEL")
	}
	if env.apiKey == "" {
		missing = append(missing, liveAPIKeyEnvVar)
	}
	if len(missing) > 0 {
		t.Skipf("live probe skipped: set %s (and run with -tags live) to exercise a real OpenAI-compatible backend",
			strings.Join(missing, ", "))
	}
	return env
}

// newLiveClient builds a Client the way production does: a config.Provider of
// type openai-compatible whose APIKeyEnv names the live key variable, resolved
// through config.Config.APIKey and handed to llm.NewClient. rec, if non-nil, is
// attached as the usage Recorder.
func newLiveClient(t *testing.T, env liveEnv, rec Recorder) Client {
	t.Helper()

	provider := config.Provider{
		Type:      config.ProviderOpenAICompatible,
		BaseURL:   env.baseURL,
		APIKeyEnv: liveAPIKeyEnvVar,
	}

	// Resolve the key through config exactly like ResolveRole does in production,
	// rather than passing env.apiKey directly. This exercises the real env->key
	// resolution path.
	cfg := &config.Config{Providers: map[string]config.Provider{"live": provider}}
	apiKey, err := cfg.APIKey("live")
	if err != nil {
		t.Fatalf("resolve live api key: %v", err)
	}

	client, err := NewClient(context.Background(), provider, "live", env.model, apiKey, Options{
		Role:     "finder",
		Recorder: rec,
	})
	if err != nil {
		t.Fatalf("build live client: %v", err)
	}
	return client
}

// liveCtx returns a context with a generous-but-bounded deadline so a hung
// backend fails the test rather than blocking forever.
func liveCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 90*time.Second)
}

// TestLive_Complete asserts a plain completion returns non-empty text and
// non-zero usage accounting from the real backend.
func TestLive_Complete(t *testing.T) {
	env := requireLiveEnv(t)
	client := newLiveClient(t, env, nil)

	ctx, cancel := liveCtx(t)
	defer cancel()

	resp, err := client.Complete(ctx, Request{
		System:    "You are a terse assistant. Answer in one short sentence.",
		Messages:  []Message{{Role: RoleUser, Content: "Say hello."}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if strings.TrimSpace(resp.Text) == "" {
		t.Errorf("Complete returned empty text; want non-empty")
	}
	if resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
		t.Errorf("Complete returned zero usage (%+v); want non-zero accounting", resp.Usage)
	}
	t.Logf("text=%q stop=%s usage=%+v", resp.Text, resp.StopReason, resp.Usage)
}

// TestLive_ToolRoundTrip exercises the full tool round-trip against the real
// backend: offer a read_file tool, demand its use, assert the model requests it
// with a parseable path argument, then feed a tool result back and assert a
// final text answer arrives.
func TestLive_ToolRoundTrip(t *testing.T) {
	env := requireLiveEnv(t)
	client := newLiveClient(t, env, nil)

	readFile := ToolDef{
		Name:        "read_file",
		Description: "Read the full contents of a file at the given repo-relative path.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {"path": {"type": "string", "description": "repo-relative file path"}},
			"required": ["path"]
		}`),
	}

	const wantPath = "config.yaml"

	ctx, cancel := liveCtx(t)
	defer cancel()

	// Turn 1: demand the tool. Keep the prompt tiny but unambiguous.
	first, err := client.Complete(ctx, Request{
		System: "You are a coding agent. To read a file you MUST call the read_file tool. Do not guess file contents.",
		Messages: []Message{{
			Role:    RoleUser,
			Content: "Read the file named " + wantPath + " using the read_file tool, then tell me what it contains.",
		}},
		Tools:     []ToolDef{readFile},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Complete (turn 1): %v", err)
	}
	if first.StopReason != StopToolUse {
		t.Fatalf("turn 1 StopReason = %q, want %q (model did not request the tool); text=%q",
			first.StopReason, StopToolUse, first.Text)
	}
	if len(first.ToolCalls) == 0 {
		t.Fatalf("turn 1 returned no tool calls despite StopToolUse")
	}

	call := first.ToolCalls[0]
	if call.Name != readFile.Name {
		t.Errorf("tool call name = %q, want %q", call.Name, readFile.Name)
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("tool call arguments not parseable JSON: %v (raw %q)", err, call.Arguments)
	}
	if !strings.Contains(args.Path, wantPath) {
		t.Errorf("tool call path = %q, want it to contain %q", args.Path, wantPath)
	}
	t.Logf("tool call: name=%s args=%s", call.Name, call.Arguments)

	// Turn 2: feed the tool result back and expect a final text answer. The
	// assistant turn must echo the tool_use the model produced so the
	// conversation is well-formed for the backend.
	const fileBody = "service: bugbot\nversion: 1\n"
	second, err := client.Complete(ctx, Request{
		System: "You are a coding agent.",
		Messages: []Message{
			{Role: RoleUser, Content: "Read the file named " + wantPath + " using the read_file tool, then tell me what it contains."},
			{Role: RoleAssistant, Content: first.Text, ToolCalls: first.ToolCalls},
			{Role: RoleToolResult, ToolCallID: call.ID, Content: fileBody},
		},
		Tools:     []ToolDef{readFile},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Complete (turn 2): %v", err)
	}
	if strings.TrimSpace(second.Text) == "" {
		t.Errorf("turn 2 returned empty final text; want a text answer (stop=%s)", second.StopReason)
	}
	t.Logf("final answer: %q (stop=%s)", second.Text, second.StopReason)
}

// TestLive_UsageRecorded asserts usage accounting flows through WithRecorder on
// the production construction path: NewClient wraps the adapter with the
// recorder, so a real completion must emit a UsageEvent tagged with the role,
// provider name, and model.
func TestLive_UsageRecorded(t *testing.T) {
	env := requireLiveEnv(t)

	rec := &captureRecorder{}
	client := newLiveClient(t, env, rec)

	ctx, cancel := liveCtx(t)
	defer cancel()

	if _, err := client.Complete(ctx, Request{
		Messages:  []Message{{Role: RoleUser, Content: "Reply with the single word: ok"}},
		MaxTokens: 16,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	events := rec.events()
	if len(events) != 1 {
		t.Fatalf("recorder got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Role != "finder" {
		t.Errorf("UsageEvent.Role = %q, want %q", ev.Role, "finder")
	}
	if ev.Provider != "live" {
		t.Errorf("UsageEvent.Provider = %q, want %q", ev.Provider, "live")
	}
	if ev.Model != env.model {
		t.Errorf("UsageEvent.Model = %q, want %q", ev.Model, env.model)
	}
	if ev.Usage.InputTokens == 0 && ev.Usage.OutputTokens == 0 {
		t.Errorf("UsageEvent usage is zero (%+v); want non-zero accounting", ev.Usage)
	}
	t.Logf("usage event: %+v", ev)
}
