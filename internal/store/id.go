package store

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
)

// newID returns a 26-char-ish lexicographically-sortable identifier: an 8-byte
// big-endian millisecond timestamp followed by 8 random bytes, hex-encoded.
// Like a ULID, IDs created later sort after earlier ones, which keeps primary
// keys roughly time-ordered without an external dependency.
func newID() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(nowUTC().UnixMilli()))
	_, _ = rand.Read(b[8:])
	return hex.EncodeToString(b[:])
}

// NewID returns a new identifier in the same lexicographically-sortable form
// used for every primary key in this package (see newID). Exported so a
// caller that needs to know a row's ID BEFORE inserting it — e.g. a funnel
// agent run that embeds the future agent_units.id in its transcript
// filename via agent.WithTranscriptKey, so the TUI can join filename to row
// by an exact ID match instead of a timestamp-window heuristic (see
// internal/tui/transcript.go) — can generate it up front and pass it back in
// as AgentUnit.ID.
func NewID() string {
	return newID()
}
