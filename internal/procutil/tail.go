package procutil

import "sync"

// TailBuffer keeps the last `limit` bytes written to it. Command output is
// unbounded and its useful end is the tail, so a supervised process never
// buffers a whole stream.
//
// Safe for concurrent writes: one buffer is routinely wired to both stdout and
// stderr of the same command, which os/exec pumps from two goroutines.
type TailBuffer struct {
	mu    sync.Mutex
	limit int
	total int64
	// A ring: once full, `start` is the oldest retained byte and a write
	// overwrites the oldest bytes in place. Sliding a linear buffer moved the
	// whole retained window on every write past capacity, which for a chatty
	// command is the window's size again per line of output.
	data  []byte
	start int
}

// NewTailBuffer returns a buffer retaining at most limit bytes. A limit of zero
// or less retains nothing while still counting what passed through.
func NewTailBuffer(limit int) *TailBuffer {
	capacity := max(limit, 0)
	return &TailBuffer{limit: limit, data: make([]byte, 0, capacity)}
}

func (b *TailBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(payload)
	b.total += int64(written)
	if b.limit <= 0 {
		return written, nil
	}
	if written >= b.limit {
		b.data = append(b.data[:0], payload[written-b.limit:]...)
		b.start = 0
		return written, nil
	}
	// Fill the linear part first; nothing is overwritten until it is full.
	if room := b.limit - len(b.data); room > 0 {
		n := min(room, written)
		b.data = append(b.data, payload[:n]...)
		payload = payload[n:]
		if len(payload) == 0 {
			return written, nil
		}
	}
	// Full: overwrite from the oldest byte on, wrapping once at most since
	// the payload is shorter than the ring.
	n := copy(b.data[b.start:], payload)
	copy(b.data, payload[n:])
	b.start = (b.start + len(payload)) % b.limit
	return written, nil
}

// String returns the retained tail.
func (b *TailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.start == 0 {
		return string(b.data)
	}
	out := make([]byte, 0, len(b.data))
	out = append(out, b.data[b.start:]...)
	out = append(out, b.data[:b.start]...)
	return string(out)
}

// Truncated reports whether writes exceeded the retained tail.
func (b *TailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > int64(len(b.data))
}
