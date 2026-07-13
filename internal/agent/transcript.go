package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dpoage/bugbot/internal/llm"
)

// EventKind tags each [Event] in a [Transcript].
type EventKind string

const (
	// EventRequest records the messages sent to the model for one completion
	// (the full conversation state at that step).
	EventRequest EventKind = "request"
	// EventAssistant records the model's response: its text and any tool calls,
	// plus the usage and stop reason for that completion.
	EventAssistant EventKind = "assistant"
	// EventToolResult records the result of executing one tool call.
	EventToolResult EventKind = "tool_result"
)

// Event is a single ordered entry in a [Transcript]. Only the fields relevant
// to its Kind are populated; the rest stay at their zero value and are omitted
// from JSON. Events serialize one-per-line as JSONL.
type Event struct {
	// Kind discriminates the event.
	Kind EventKind `json:"kind"`
	// Step is the 1-based model-turn this event belongs to.
	Step int `json:"step"`
	// Time is when the event was recorded.
	Time time.Time `json:"time"`

	// Messages is set on EventRequest: the full conversation sent to the model.
	Messages []llm.Message `json:"messages,omitempty"`

	// Text is set on EventAssistant: the model's text output.
	Text string `json:"text,omitempty"`
	// ToolCalls is set on EventAssistant: the model's tool-use requests.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	// StopReason is set on EventAssistant.
	StopReason llm.StopReason `json:"stop_reason,omitempty"`
	// Usage is set on EventAssistant: token usage for that completion.
	Usage *llm.Usage `json:"usage,omitempty"`

	// ToolCallID is set on EventToolResult: the call this result answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is set on EventToolResult: the tool that produced the result.
	ToolName string `json:"tool_name,omitempty"`
	// Result is set on EventToolResult: the textual tool output (possibly an
	// "ERROR:"-prefixed message).
	Result string `json:"result,omitempty"`
	// IsError is set on EventToolResult when the tool returned an error.
	IsError bool `json:"is_error,omitempty"`
}

// Transcript is the ordered record of a single [Runner.Run]. It stores full
// content (not hashes) so the eval harness can replay it deterministically. A
// Transcript is not safe for concurrent mutation, but a single Runner appends
// to it sequentially.
//
// Streaming: when enableStreaming is called, every record* call below also
// appends the just-recorded event as one JSON line to a file, so an operator
// can `tail -f` a stuck run's transcript instead of waiting for it to finish.
// The file is opened lazily on the first recorded event (never for a run that
// records nothing) and closed via closeStream at run end. Streaming is
// best-effort: open/encode failures disable it silently (streamPath is
// cleared) so a broken disk never affects the run's result. The Runner that
// owns a Transcript is single-goroutine per run, so streamFile/streamEnc need
// no locking.
type Transcript struct {
	// Events are the run's events in chronological order.
	Events []Event `json:"-"`
	clock  func() time.Time

	streamPath string
	streamFile *os.File
	streamEnc  *json.Encoder
	// savedPath is the path streaming actually wrote to, set once on the first
	// successful file creation in streamAppend and never cleared afterward
	// (unlike streamPath, which closeStream clears to disarm re-opening on a
	// reused Transcript — see closeStream's doc). Callers use SavedPath after
	// the run to learn where — if anywhere — the transcript was persisted,
	// e.g. to store it as an exact join key on a database row.
	savedPath string
}

// NewTranscript returns an empty transcript using the real wall clock.
func NewTranscript() *Transcript {
	return &Transcript{clock: time.Now}
}

// enableStreaming arms incremental JSONL writes to path: the file is created
// (directories included) on the first subsequent record* call, not here, so a
// run that records nothing never touches disk. A no-op path (empty string)
// leaves streaming disabled.
func (t *Transcript) enableStreaming(path string) {
	t.streamPath = path
}

// SavedPath returns the path the transcript was actually streamed to on
// disk, or "" if streaming was never armed, never successfully opened a file
// (best-effort autosave silently disables itself on I/O failure — see
// streamAppend), or armed but the run recorded nothing (the file is opened
// lazily on first event). Safe to call any time, including after the run
// completes (unlike streamPath, this is never cleared by closeStream) — the
// intended use is a caller reading it from Outcome.Transcript once Run
// returns, to persist an exact reference to the transcript file (e.g. onto a
// database row) rather than relying on a timestamp-window guess later.
func (t *Transcript) SavedPath() string {
	return t.savedPath
}

// streamAppend writes ev as one JSON line to the streaming file, opening it
// (and its parent directory) on first use. Best-effort: any failure disables
// further attempts for this transcript by clearing streamPath, matching the
// never-fail-the-run autosave contract.
func (t *Transcript) streamAppend(ev *Event) {
	if t.streamPath == "" {
		return
	}
	if t.streamFile == nil {
		if err := os.MkdirAll(filepath.Dir(t.streamPath), 0o755); err != nil {
			t.streamPath = ""
			return
		}
		f, err := os.Create(t.streamPath)
		if err != nil {
			t.streamPath = ""
			return
		}
		t.streamFile = f
		t.streamEnc = json.NewEncoder(f)
		t.savedPath = t.streamPath
	}
	if err := t.streamEnc.Encode(ev); err != nil {
		// Leave the file open (a later event might still succeed); just drop
		// this line, matching autosave's discard-on-error contract.
		return
	}
}

// closeStream closes the streaming file, if one was opened, and disarms
// streaming by clearing streamPath. Idempotent and best-effort: called
// unconditionally at run end (and every early-return path) regardless of
// whether streaming was ever armed or actually opened a file.
//
// Clearing streamPath (not just streamFile/streamEnc) matters because a
// Transcript outlives one run() call: RunJSON's repair() reuses the same
// *Transcript for its one repair completion AFTER run() has already called
// closeStream. Without clearing streamPath, streamAppend would see
// streamPath still set and streamFile nil, and reopen the same path with
// os.Create — truncating the just-closed main-run transcript down to only
// the repair's two events, and leaking the newly reopened file handle since
// nothing closes it again. Clearing streamPath makes closeStream a true
// terminal state: repair's events are recorded in-memory only (Events), same
// as pre-streaming behavior, and the on-disk file keeps the complete
// main-run transcript untouched.
func (t *Transcript) closeStream() {
	if t.streamFile != nil {
		_ = t.streamFile.Close()
		t.streamFile = nil
		t.streamEnc = nil
	}
	t.streamPath = ""
}

// now returns the current time, allowing tests to inject a fixed clock.
func (t *Transcript) now() time.Time {
	if t.clock != nil {
		return t.clock()
	}
	return time.Now()
}

// recordRequest appends an EventRequest capturing the messages sent for step.
func (t *Transcript) recordRequest(step int, msgs []llm.Message) {
	// Copy the slice so later mutation of the conversation doesn't alias the
	// recorded snapshot.
	snap := make([]llm.Message, len(msgs))
	copy(snap, msgs)
	t.Events = append(t.Events, Event{
		Kind:     EventRequest,
		Step:     step,
		Time:     t.now(),
		Messages: snap,
	})
	t.streamAppend(&t.Events[len(t.Events)-1])
}

// recordAssistant appends an EventAssistant for the model's response at step.
func (t *Transcript) recordAssistant(step int, resp llm.Response) {
	u := resp.Usage
	t.Events = append(t.Events, Event{
		Kind:       EventAssistant,
		Step:       step,
		Time:       t.now(),
		Text:       resp.Text,
		ToolCalls:  resp.ToolCalls,
		StopReason: resp.StopReason,
		Usage:      &u,
	})
	t.streamAppend(&t.Events[len(t.Events)-1])
}

// recordToolResult appends an EventToolResult for one executed tool call.
func (t *Transcript) recordToolResult(step int, call llm.ToolCall, result string, isErr bool) {
	t.Events = append(t.Events, Event{
		Kind:       EventToolResult,
		Step:       step,
		Time:       t.now(),
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Result:     result,
		IsError:    isErr,
	})
	t.streamAppend(&t.Events[len(t.Events)-1])
}

// SaveJSONL writes the transcript as JSON Lines (one Event per line) to w.
func (t *Transcript) SaveJSONL(w io.Writer) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for i := range t.Events {
		if err := enc.Encode(&t.Events[i]); err != nil {
			// Best-effort: flush any successfully-encoded prefix to w so a
			// partial transcript is at least recoverable on the next load.
			// We discard the flush error — the encode error is the one to
			// surface, and the trailing bw.Flush in the success path
			// remains the authoritative one.
			_ = bw.Flush()
			return fmt.Errorf("agent: encode transcript event %d: %w", i, err)
		}
	}
	return bw.Flush()
}

// LoadJSONL reads a JSONL transcript (as written by SaveJSONL) from r.
func LoadJSONL(r io.Reader) (*Transcript, error) {
	t := NewTranscript()
	sc := bufio.NewScanner(r)
	// Transcript lines can be large (full conversations); raise the line cap.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, fmt.Errorf("agent: decode transcript line %d: %w", line, err)
		}
		t.Events = append(t.Events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("agent: read transcript: %w", err)
	}
	return t, nil
}
