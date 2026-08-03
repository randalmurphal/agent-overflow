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
	data  []byte
}

// NewTailBuffer returns a buffer retaining at most limit bytes. A limit of zero
// or less retains nothing while still counting what passed through.
func NewTailBuffer(limit int) *TailBuffer {
	capacity := limit
	if capacity < 0 {
		capacity = 0
	}
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
	if len(payload) >= b.limit {
		b.data = append(b.data[:0], payload[len(payload)-b.limit:]...)
		return written, nil
	}
	overflow := len(b.data) + len(payload) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, payload...)
	return written, nil
}

// String returns the retained tail.
func (b *TailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// Truncated reports whether writes exceeded the retained tail.
func (b *TailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > int64(len(b.data))
}
