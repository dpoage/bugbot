package control

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dpoage/bugbot/internal/progress"
)

// TestFrame_EventAgentIDSurvivesNDJSONRoundTrip verifies that a FrameKindEvent
// carrying a progress.Event with AgentID set survives an encode/decode cycle
// through the wire's newline-delimited JSON encoding (json.Encoder/Decoder,
// matching client.go/server.go's actual usage) — i.e. the additive agent_id
// field on progress.Event flows through Frame without any protocol.go change
// beyond the embedded *progress.Event already carrying it.
func TestFrame_EventAgentIDSurvivesNDJSONRoundTrip(t *testing.T) {
	sent := Frame{
		V:    ProtocolVersion,
		Kind: FrameKindEvent,
		Event: &progress.Event{
			Kind:    progress.KindToolCall,
			Role:    progress.RoleReproducer,
			Label:   "duplicate finding title",
			AgentID: "abc123deadbeef",
			Tool:    "read_file",
			Phase:   "start",
		},
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(sent); err != nil {
		t.Fatalf("Encode() error: %v", err)
	}

	var got Frame
	if err := json.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("Decode() error: %v", err)
	}

	if got.Event == nil {
		t.Fatal("decoded Frame.Event is nil")
	}
	if got.Event.AgentID != sent.Event.AgentID {
		t.Errorf("AgentID = %q, want %q", got.Event.AgentID, sent.Event.AgentID)
	}
	if got.Event.Role != sent.Event.Role || got.Event.Label != sent.Event.Label {
		t.Errorf("Role/Label = %q/%q, want %q/%q", got.Event.Role, got.Event.Label, sent.Event.Role, sent.Event.Label)
	}
}

// TestFrame_EventEmptyAgentIDOmittedFromWire verifies the omitempty tag: an
// Event with no AgentID (a pre-identity emitter) round-trips to an empty
// string, not a JSON null or a literal "agent_id" key — so an old daemon's
// events remain byte-compatible with a build that has never heard of
// AgentID.
func TestFrame_EventEmptyAgentIDOmittedFromWire(t *testing.T) {
	sent := Frame{
		V:    ProtocolVersion,
		Kind: FrameKindEvent,
		Event: &progress.Event{
			Kind:  progress.KindAgentStarted,
			Role:  progress.RoleReproducer,
			Label: "some finding",
		},
	}

	data, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if bytes.Contains(data, []byte("agent_id")) {
		t.Errorf("wire payload unexpectedly contains agent_id: %s", data)
	}

	var got Frame
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if got.Event.AgentID != "" {
		t.Errorf("AgentID = %q, want empty", got.Event.AgentID)
	}
}

// TestVerbReconcile_KnownAndInTable verifies VerbReconcile (bugbot-7bjl) is
// registered in Verbs and Known() reports true, matching every sibling
// dispatch verb.
func TestVerbReconcile_KnownAndInTable(t *testing.T) {
	if !VerbReconcile.Known() {
		t.Error("VerbReconcile.Known() = false, want true")
	}
	found := false
	for _, v := range Verbs {
		if v == VerbReconcile {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Verbs = %v, want it to contain VerbReconcile", Verbs)
	}
}
