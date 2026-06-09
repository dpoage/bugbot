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
