package terminal

import (
	"bytes"
	"testing"
)

func TestRingBufferAppendWithinCapacity(t *testing.T) {
	r := newRingBuffer(16)
	r.append([]byte("hello"))
	if got := string(r.snapshot()); got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
	if r.Len() != 5 {
		t.Fatalf("Len() = %d, want 5", r.Len())
	}
}

func TestRingBufferAppendAtCapacity(t *testing.T) {
	r := newRingBuffer(4)
	r.append([]byte("abcd"))
	r.append([]byte("ef"))
	got := r.snapshot()
	if !bytes.Equal(got, []byte("cdef")) {
		t.Fatalf("got %q, want cdef", got)
	}
}

func TestRingBufferAppendLargerThanCapacity(t *testing.T) {
	r := newRingBuffer(4)
	r.append([]byte("hello world"))
	got := r.snapshot()
	if !bytes.Equal(got, []byte("orld")) {
		t.Fatalf("got %q, want orld", got)
	}
}

func TestRingBufferAppendEviction(t *testing.T) {
	r := newRingBuffer(5)
	r.append([]byte("12345"))
	r.append([]byte("67"))
	got := r.snapshot()
	if !bytes.Equal(got, []byte("34567")) {
		t.Fatalf("got %q, want 34567", got)
	}
	if r.Len() != 5 {
		t.Fatalf("Len() = %d, want 5", r.Len())
	}
}

func TestRingBufferSnapshotIsCopy(t *testing.T) {
	r := newRingBuffer(8)
	r.append([]byte("abc"))
	s1 := r.snapshot()
	s1[0] = 'X'
	s2 := r.snapshot()
	if !bytes.Equal(s2, []byte("abc")) {
		t.Fatalf("snapshot mutated internal state: got %q", s2)
	}
}

// TestRingBufferAppendWrapAroundSplit exercises the two-copy write path that
// splits an append across the end of the backing array.
func TestRingBufferAppendWrapAroundSplit(t *testing.T) {
	r := newRingBuffer(8)
	r.append([]byte("abcd"))   // fills positions 0..3
	r.append([]byte("efghij")) // last 2 bytes must wrap to positions 0..1
	got := r.snapshot()
	if !bytes.Equal(got, []byte("cdefghij")) {
		t.Fatalf("got %q, want cdefghij", got)
	}
}

// TestRingBufferMultipleWraps makes sure the start pointer stays consistent
// after repeated overflowing writes.
func TestRingBufferMultipleWraps(t *testing.T) {
	r := newRingBuffer(4)
	r.append([]byte("ab"))
	r.append([]byte("cd"))
	r.append([]byte("ef"))
	r.append([]byte("gh"))
	got := r.snapshot()
	if !bytes.Equal(got, []byte("efgh")) {
		t.Fatalf("got %q, want efgh", got)
	}
}
