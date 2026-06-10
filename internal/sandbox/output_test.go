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
