package terminal

import "sync"

// ringBuffer is a byte-oriented circular buffer used for session replay.
// Writes past the capacity drop the oldest bytes. All methods are safe for
// concurrent use.
type ringBuffer struct {
	mu   sync.Mutex
	data []byte // len == capacity; wraps at start+len >= capacity
	// start points at the oldest byte; length is how many bytes are currently
	// stored. When length == len(data) the buffer is full and further writes
	// evict from start.
	start  int
	length int
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &ringBuffer{data: make([]byte, capacity)}
}

// append writes p into the buffer, evicting older bytes as necessary.
func (r *ringBuffer) append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	capacity := len(r.data)
	if len(p) >= capacity {
		// Only the last `capacity` bytes will survive; keep the tail.
		copy(r.data, p[len(p)-capacity:])
		r.start = 0
		r.length = capacity
		return
	}

	// Write into the buffer using at most two copy() calls so appends don't
	// cost one loop iteration per byte for typical 4KB chunks.
	writeStart := (r.start + r.length) % capacity
	firstChunk := capacity - writeStart
	if firstChunk > len(p) {
		firstChunk = len(p)
	}
	copy(r.data[writeStart:], p[:firstChunk])
	if firstChunk < len(p) {
		copy(r.data, p[firstChunk:])
	}

	overflow := r.length + len(p) - capacity
	if overflow > 0 {
		r.start = (r.start + overflow) % capacity
		r.length = capacity
	} else {
		r.length += len(p)
	}
}

// snapshot returns a new byte slice containing the ordered buffer contents
// from oldest to newest.
func (r *ringBuffer) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.length)
	capacity := len(r.data)
	firstLen := capacity - r.start
	if firstLen > r.length {
		firstLen = r.length
	}
	copy(out, r.data[r.start:r.start+firstLen])
	if firstLen < r.length {
		copy(out[firstLen:], r.data[:r.length-firstLen])
	}
	return out
}

// Len returns the current number of stored bytes.
func (r *ringBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.length
}
