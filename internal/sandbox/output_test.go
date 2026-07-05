package sandbox

import (
	"strings"
	"testing"
)

func TestCappedBufferTruncates(t *testing.T) {
	b := newCappedBuffer(10)
	n, err := b.Write([]byte("0123456789ABCDEF")) // 16 bytes into a 10-byte cap
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 16 {
		t.Errorf("Write should report full consumption (16) to keep the pipe draining, got %d", n)
	}

	got, truncated := b.result()
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if !strings.HasPrefix(got, "0123456789") {
		t.Errorf("retained prefix wrong: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker in output, got %q", got)
	}
}

func TestCappedBufferUnderCap(t *testing.T) {
	b := newCappedBuffer(100)
	_, _ = b.Write([]byte("hello"))
	got, truncated := b.result()
	if truncated {
		t.Error("did not expect truncation")
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestCappedBufferMultipleWritesCrossCap(t *testing.T) {
	b := newCappedBuffer(8)
	_, _ = b.Write([]byte("abcd"))
	_, _ = b.Write([]byte("efgh"))
	_, _ = b.Write([]byte("ijkl")) // pushes over the cap
	got, truncated := b.result()
	if !truncated {
		t.Fatal("expected truncation after crossing cap")
	}
	if !strings.HasPrefix(got, "abcdefgh") {
		t.Errorf("retained prefix wrong: %q", got)
	}
}

func TestCappedBufferZeroCapRetainsAll(t *testing.T) {
	b := newCappedBuffer(0)
	_, _ = b.Write([]byte("anything goes"))
	got, truncated := b.result()
	if truncated {
		t.Error("zero cap should not truncate")
	}
	if got != "anything goes" {
		t.Errorf("got %q", got)
	}
}

// TestCappedBufferRetainsTail_HeadTailWindow verifies dual-window retention:
// when the stream overflows the ring, bytes from BOTH the head and the tail
// are retained. Test runners print failure summaries at the END of output, so
// a pure head-only buffer would lose them (bugbot-yjm1).
func TestCappedBufferRetainsTail_HeadTailWindow(t *testing.T) {
	// cap = headBytes+32 (32-byte ring). Write headBytes + 64 bytes.
	// Ring receives 64 bytes but only holds 32 → overwrites first 32.
	// Last 32 bytes (tailLast) must survive; first write (tailFirst) is elided.
	ringSize := 32
	cap := headBytes + ringSize
	b := newCappedBuffer(cap)

	headContent := make([]byte, headBytes)
	for i := range headContent {
		headContent[i] = 'A'
	}
	tailFirst := make([]byte, 32)
	for i := range tailFirst {
		tailFirst[i] = 'B'
	}
	tailLast := []byte("--- FAIL: TestBug 00000000000000\n")
	tailLast = tailLast[:32]

	_, _ = b.Write(headContent)
	_, _ = b.Write(tailFirst) // fills ring; ringFull set; truncated=true
	_, _ = b.Write(tailLast)  // overwrites ring with FAIL marker

	got, truncated := b.result()
	if !truncated {
		t.Fatal("expected truncated=true: ring overwrote earlier tail bytes")
	}
	if got[0] != 'A' {
		t.Errorf("head content missing: first byte = %q", got[0])
	}
	if !strings.Contains(got, "--- FAIL") {
		t.Errorf("tail FAIL marker missing from result; got suffix=%q", got[maxInt(0, len(got)-60):])
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("expected elision gap marker in result; got=%q", got[:minInt(len(got), 300)])
	}
}

// TestCappedBufferHeadTailGapMarker verifies the gap marker appears and the tail
// survives when ring overflows.
func TestCappedBufferHeadTailGapMarker(t *testing.T) {
	// Ring = 8 bytes. Write headBytes + 100 bytes. Ring keeps last 8 = "TAILDATA".
	cap := headBytes + 8
	b := newCappedBuffer(cap)

	headContent := make([]byte, headBytes)
	for i := range headContent {
		headContent[i] = 'H'
	}
	overflow := make([]byte, 100)
	for i := range overflow {
		overflow[i] = 'M'
	}
	copy(overflow[92:], "TAILDATA") // last 8 bytes

	_, _ = b.Write(headContent)
	_, _ = b.Write(overflow)

	got, truncated := b.result()
	if !truncated {
		t.Fatal("expected truncated=true when ring overflows")
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("expected elision gap marker; got=%q", got[:minInt(len(got), 300)])
	}
	if !strings.HasSuffix(got, "TAILDATA") {
		t.Errorf("tail content missing; got suffix=%q", got[maxInt(0, len(got)-20):])
	}
	if got[0] != 'H' {
		t.Errorf("head content missing")
	}
}

// TestCappedBufferFailSummaryAtTail verifies acceptance criterion for bugbot-yjm1:
// a stream exceeding the cap whose final lines contain '--- FAIL' still exposes
// that text after capping so interpret() can classify demonstrated.
func TestCappedBufferFailSummaryAtTail(t *testing.T) {
	// Ring = 1024. Write headBytes + 2048 filler + failSummary.
	// Filler overflows ring; failSummary survives in the last ring bytes.
	ringSize := 1024
	cap := headBytes + ringSize
	b := newCappedBuffer(cap)

	headContent := make([]byte, headBytes)
	for i := range headContent {
		headContent[i] = 'X'
	}
	filler := make([]byte, 2048)
	for i := range filler {
		filler[i] = 'Y'
	}
	failSummary := "--- FAIL: TestRaceCondition (0.123s)\nFAIL\tgithub.com/example/pkg\n"

	_, _ = b.Write(headContent)
	_, _ = b.Write(filler)
	_, _ = b.Write([]byte(failSummary))

	got, truncated := b.result()
	if !truncated {
		t.Fatal("expected truncated=true: ring overflowed")
	}
	if !strings.Contains(got, "--- FAIL") {
		t.Errorf("failure summary lost after capping; got tail=%q", got[maxInt(0, len(got)-200):])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestCappedBufferPartialRing_TailRetained is the regression guard for the
// data-loss bug: when a stream overflows the head window but its tail fits in
// the ring WITHOUT wrapping (total in (head, max]), result() must still return
// the ring/tail content. The prior implementation returned head-only on
// !truncated, silently dropping the tail — reintroducing the very
// false-negative bugbot-yjm1 set out to fix (a trailing "--- FAIL" past the
// 256KB head was lost for the whole 256KB..1MiB band under the 1MiB cap).
func TestCappedBufferPartialRing_TailRetained(t *testing.T) {
	ringSize := 4096
	b := newCappedBuffer(headBytes + ringSize) // 256KB head + 4KB ring

	headContent := make([]byte, headBytes)
	for i := range headContent {
		headContent[i] = 'H'
	}
	// Tail is smaller than the ring, so the ring never wraps: truncated stays
	// false, yet these bytes MUST survive.
	tail := "leading tail bytes ...\n--- FAIL: TestLate (0.10s)\nFAIL\tgithub.com/example/pkg\n"

	_, _ = b.Write(headContent)
	_, _ = b.Write([]byte(tail))

	got, truncated := b.result()
	if truncated {
		t.Error("no bytes were lost (tail fit the ring without wrapping); truncated must be false")
	}
	if got[0] != 'H' {
		t.Errorf("head content missing: first byte = %q", got[0])
	}
	if !strings.HasSuffix(got, tail) {
		t.Errorf("tail content dropped by result(); got suffix=%q", got[maxInt(0, len(got)-80):])
	}
	if !strings.Contains(got, "--- FAIL") {
		t.Error("trailing FAIL marker lost after capping — interpret() would misclassify not_demonstrated")
	}
	// No elision marker: head and tail are contiguous, nothing was dropped.
	if strings.Contains(got, "elided") {
		t.Errorf("no bytes were elided, but result() emitted an elision marker; got=%q", got[maxInt(0, len(got)-120):])
	}
	// Full fidelity: exactly head + tail, byte for byte.
	if len(got) != headBytes+len(tail) {
		t.Errorf("result length = %d, want %d (head+tail, no loss)", len(got), headBytes+len(tail))
	}
}
