package forgeapi

// TailBuffer is an io.Writer that keeps the last Cap bytes written. Given
// to Client.Stream as dst it also declares the tail the caller wants: a
// body whose Content-Length exceeds Cap is re-requested as a Range from
// the end instead of read whole. Not safe for concurrent use.
type TailBuffer struct {
	ring  []byte
	start int   // index of the oldest byte once the ring is full
	total int64 // bytes ever written
}

// NewTailBuffer returns a buffer keeping the last capacity bytes; capacity
// must be positive.
func NewTailBuffer(capacity int) *TailBuffer {
	if capacity <= 0 {
		panic("forgeapi: TailBuffer capacity must be positive")
	}
	return &TailBuffer{ring: make([]byte, 0, capacity)}
}

// Cap is the number of bytes the buffer keeps.
func (b *TailBuffer) Cap() int { return cap(b.ring) }

// Write keeps the tail of everything written; it never fails.
func (b *TailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	capacity := cap(b.ring)
	if n >= capacity {
		b.ring = append(b.ring[:0], p[n-capacity:]...)
		b.start = 0
		return n, nil
	}
	if room := capacity - len(b.ring); room > 0 {
		take := min(room, len(p))
		b.ring = append(b.ring, p[:take]...)
		p = p[take:]
	}
	for len(p) > 0 {
		copied := copy(b.ring[b.start:], p)
		p = p[copied:]
		b.start = (b.start + copied) % capacity
	}
	return n, nil
}

// Bytes returns a copy of the kept tail, oldest byte first.
func (b *TailBuffer) Bytes() []byte {
	out := make([]byte, 0, len(b.ring))
	out = append(out, b.ring[b.start:]...)
	return append(out, b.ring[:b.start]...)
}

// Len is the number of bytes kept.
func (b *TailBuffer) Len() int { return len(b.ring) }

// Total is the number of bytes written, kept or not.
func (b *TailBuffer) Total() int64 { return b.total }

// Truncated reports whether bytes before the kept tail were dropped.
func (b *TailBuffer) Truncated() bool { return b.total > int64(len(b.ring)) }

// Reset empties the buffer.
func (b *TailBuffer) Reset() {
	b.ring = b.ring[:0]
	b.start = 0
	b.total = 0
}
