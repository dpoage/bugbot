package sandbox

import (
	"bytes"
	"sync"
)

// cappedBuffer is an io.Writer that retains at most max bytes. Once full it
// silently discards further writes but keeps counting so the caller knows the
// stream was truncated. It is safe for concurrent writes, which lets it be
// handed to exec.Cmd as Stdout/Stderr (those are written from a reader
// goroutine).
type cappedBuffer struct {
	max int

	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	total     int64 // total bytes ever written, including those discarded over cap
}

func newCappedBuffer(max int) *cappedBuffer {
	return &cappedBuffer{max: max}
}

// Write implements io.Writer, retaining only up to max bytes.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))

	if c.max <= 0 {
		// No cap configured: retain everything.
		return c.buf.Write(p)
	}

	remaining := c.max - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil // report full consumption so the pipe keeps draining
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

// truncationMarker is appended to a captured stream that was truncated so the
// consumer can tell the output is incomplete.
const truncationMarker = "\n... [output truncated by sandbox] ...\n"

// result returns the captured text (with a marker appended if truncated) and
// whether truncation occurred.
func (c *cappedBuffer) result() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.truncated {
		return c.buf.String() + truncationMarker, true
	}
	return c.buf.String(), false
}

// written returns the total number of bytes ever handed to Write, including
// bytes discarded after the cap was reached. It is the idle watchdog's
// output-activity signal, so it must keep growing even once the buffer is full.
func (c *cappedBuffer) written() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
