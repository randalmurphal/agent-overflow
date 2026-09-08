package procutil

import (
	"strings"
	"sync"
	"testing"
)

func TestTailBufferRetainsTheEndOfTheStream(t *testing.T) {
	buffer := NewTailBuffer(6)
	if _, err := buffer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if buffer.Truncated() {
		t.Fatal("buffer reported truncation before exceeding the limit")
	}
	if _, err := buffer.Write([]byte("defgh")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "cdefgh" {
		t.Fatalf("tail = %q, want cdefgh", got)
	}
	if !buffer.Truncated() {
		t.Fatal("buffer did not report truncation after exceeding the limit")
	}
}

// One oversized write is the shape a build tool produces: the whole stream in a
// single chunk, of which only the tail may be retained.
func TestTailBufferSingleOversizedWrite(t *testing.T) {
	buffer := NewTailBuffer(4)
	if _, err := buffer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "ello" {
		t.Fatalf("tail = %q, want ello", got)
	}
	if !buffer.Truncated() {
		t.Fatal("oversized write did not report truncation")
	}
}

func TestTailBufferZeroLimitRetainsNothingButCounts(t *testing.T) {
	buffer := NewTailBuffer(0)
	written, err := buffer.Write([]byte("data"))
	if err != nil || written != 4 {
		t.Fatalf("Write = %d, %v", written, err)
	}
	if got := buffer.String(); got != "" {
		t.Fatalf("tail = %q, want empty", got)
	}
	if !buffer.Truncated() {
		t.Fatal("dropped bytes did not report truncation")
	}
}

// stdout and stderr of one command are pumped by two goroutines into the same
// buffer, so concurrent Write must neither race nor lose bytes.
func TestTailBufferConcurrentWrites(t *testing.T) {
	buffer := NewTailBuffer(8)
	var group sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 50; i++ {
				if _, err := buffer.Write([]byte("xy")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
	if got := buffer.String(); got != strings.Repeat("xy", 4) {
		t.Fatalf("tail = %q", got)
	}
	if !buffer.Truncated() {
		t.Fatal("concurrent overflow did not report truncation")
	}
}

// The ring is exercised past several wraps with writes of every length
// short of the limit, against the reference "last N bytes of everything
// written" — the property the linear slide had and the ring must keep.
func TestTailBufferWrapsLikeASlidingWindow(t *testing.T) {
	const limit = 7
	buffer := NewTailBuffer(limit)
	var all []byte
	for i := range 40 {
		chunk := []byte(strings.Repeat(string(rune('a'+i%26)), 1+i%(limit-1)))
		all = append(all, chunk...)
		if _, err := buffer.Write(chunk); err != nil {
			t.Fatal(err)
		}
		want := string(all[max(0, len(all)-limit):])
		if got := buffer.String(); got != want {
			t.Fatalf("after write %d: tail = %q, want %q", i, got, want)
		}
	}
	if !buffer.Truncated() {
		t.Fatal("a stream longer than the limit was not reported truncated")
	}
}

func BenchmarkTailBufferWrite(b *testing.B) {
	buffer := NewTailBuffer(128 << 10)
	line := []byte(strings.Repeat("x", 80) + "\n")
	for b.Loop() {
		buffer.Write(line)
	}
}
