package sandbox

import (
	"bytes"
	"fmt"
	"sync"
)

// headBytes is the number of bytes retained at the head of a capped stream.
// Failure summaries (--- FAIL, FAILED, test result:) print at the END of
// test output, so the budget is weighted toward the tail.
const headBytes = 256 * 1024 // 256 KB

// cappedBuffer is an io.Writer that retains the first headBytes and the last
// (max-headBytes) bytes of the stream, joined by a gap marker noting how many
// bytes were elided. Once the head window is full, subsequent bytes are written
// into a circular ring buffer that always holds the most-recent tail content.
//
// This dual-window approach keeps early build errors (which land in stderr
// before any test output) AND late test-runner summaries (--- FAIL, FAILED,
// test result: FAILED) simultaneously visible to interpret() — solving the
// false-negative classification that arose when cappedBuffer discarded the tail.
//
// written() counts every byte ever handed to Write (including those that fell
// into neither window once both were full) so the idle watchdog's output-activity
// signal keeps growing after the cap is reached. It is safe for concurrent
// writes, which lets it be handed to exec.Cmd as Stdout/Stderr.
type cappedBuffer struct {
	max  int // total budget; 0 = unlimited
	head int // head window size (min(headBytes, max))

	mu        sync.Mutex
	headBuf   bytes.Buffer // retains the first `head` bytes
	ring      []byte       // circular tail buffer, size = max - head
	ringWrite int          // next write position in ring
	ringFull  bool         // ring has wrapped at least once
	truncated bool         // at least one byte was overwritten in the ring
	total     int64        // total bytes ever written, including discarded
}

func newCappedBuffer(max int) *cappedBuffer {
	c := &cappedBuffer{max: max}
	if max > 0 {
		h := headBytes
		if h >= max {
			h = max // entire budget goes to head; no ring needed
		}
		c.head = h
		tailSize := max - h
		if tailSize > 0 {
			c.ring = make([]byte, tailSize)
		}
	}
	return c
}

// Write implements io.Writer, retaining the first head bytes and the last
// (max-head) bytes; any bytes between are counted but overwritten.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))

	if c.max <= 0 {
		// No cap configured: retain everything.
		return c.headBuf.Write(p)
	}

	written := 0
	for written < len(p) {
		chunk := p[written:]

		// Phase 1: head window still has room.
		if c.headBuf.Len() < c.head {
			room := c.head - c.headBuf.Len()
			take := chunk
			if len(take) > room {
				take = chunk[:room]
			}
			c.headBuf.Write(take)
			written += len(take)
			continue
		}

		// Phase 2: head window is full; route remaining bytes into the ring.
		if len(c.ring) == 0 {
			// No tail budget: discard everything past head. The remainder of p
			// is consumed either way, so written needs no further tracking.
			c.truncated = true
			break
		}

		// Write chunk into ring with wrap-around, in up to two copies.
		remaining := chunk
		for len(remaining) > 0 {
			space := len(c.ring) - c.ringWrite
			take := remaining
			if len(take) > space {
				take = remaining[:space]
			}
			copy(c.ring[c.ringWrite:], take)
			c.ringWrite += len(take)
			if c.ringWrite >= len(c.ring) {
				c.ringWrite = 0
				c.ringFull = true
				c.truncated = true
			}
			remaining = remaining[len(take):]
		}
		break
	}

	return len(p), nil
}

// truncationMarker is appended to a captured stream that was truncated so the
// consumer can tell the output is incomplete.
const truncationMarker = "\n... [output truncated by sandbox] ...\n"

// result returns the captured text (with a gap marker if bytes were elided
// between head and tail windows) and whether any truncation occurred.
func (c *cappedBuffer) result() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	head := c.headBuf.String()

	// Reconstruct any tail content held in the ring. The ring can hold live
	// bytes even when NOTHING was lost — a stream that overflowed the 256KB
	// head window but fit within the ring WITHOUT wrapping leaves
	// truncated=false yet still has tail bytes. Those MUST be returned, so the
	// tail is rebuilt independently of the truncated flag. (The prior early
	// return on !truncated silently dropped the entire tail for every stream
	// whose total landed in (head, max] — e.g. a 400KB run under the 1MiB cap
	// lost its last 144KB, including any trailing "--- FAIL".)
	var tail []byte
	if len(c.ring) > 0 {
		if c.ringFull {
			// Ring has wrapped: oldest bytes start at ringWrite.
			tail = make([]byte, len(c.ring))
			copy(tail, c.ring[c.ringWrite:])
			copy(tail[len(c.ring)-c.ringWrite:], c.ring[:c.ringWrite])
		} else {
			// Ring has not wrapped: live content is ring[0:ringWrite].
			tail = c.ring[:c.ringWrite]
		}
	}

	if len(tail) == 0 {
		// No tail bytes: the stream fit in the head window, or the head-only
		// discard path (no ring budget) dropped everything past it. The
		// truncated flag distinguishes the two and drives the marker.
		if c.truncated {
			return head + truncationMarker, true
		}
		return head, false
	}

	// Head and tail both hold content. Any bytes captured by neither window
	// were elided; report the count. When the windows are contiguous
	// (total == len(head)+len(tail)) nothing was lost: no gap, no truncation.
	elided := c.total - int64(len(head)) - int64(len(tail))
	if elided <= 0 {
		return head + string(tail), c.truncated
	}
	gap := fmt.Sprintf("\n... [%d bytes elided by sandbox] ...\n", elided)
	return head + gap + string(tail), true
}

// written returns the total number of bytes ever handed to Write, including
// bytes discarded after the cap was reached. It is the idle watchdog's
// output-activity signal, so it must keep growing even once the buffer is full.
func (c *cappedBuffer) written() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
