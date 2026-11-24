package terminal

import (
	"bytes"
	"testing"
)

// Each case appends in order and reads back the newest bytes up to the
// capacity. "wraps across the end" takes the two-copy write path that splits
// an append across the end of the backing array; "wraps repeatedly" keeps the
// start pointer consistent after repeated overflowing writes.
func TestRingBufferAppend(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity int
		appends  []string
		want     string
	}{
		{"within capacity", 16, []string{"hello"}, "hello"},
		{"at capacity", 4, []string{"abcd", "ef"}, "cdef"},
		{"larger than capacity", 4, []string{"hello world"}, "orld"},
		{"evicts the oldest", 5, []string{"12345", "67"}, "34567"},
		{"wraps across the end", 8, []string{"abcd", "efghij"}, "cdefghij"},
		{"wraps repeatedly", 4, []string{"ab", "cd", "ef", "gh"}, "efgh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRingBuffer(tc.capacity)
			for _, chunk := range tc.appends {
				r.append([]byte(chunk))
			}
			if got := string(r.snapshot()); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if r.Len() != len(tc.want) {
				t.Fatalf("Len() = %d, want %d", r.Len(), len(tc.want))
			}
		})
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
