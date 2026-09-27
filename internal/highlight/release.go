package highlight

import (
	"sync"
	"time"
)

const (
	// releaseIdle is how long the highlighter stays idle before its freed
	// C memory is returned to the OS. A streaming code block recomputes
	// on every flush, so a release waits for the stream to stop.
	releaseIdle = time.Second

	// releaseMaxDelay bounds how long freed memory stays resident while
	// computations keep arriving without an idle gap.
	releaseMaxDelay = 30 * time.Second
)

// heapRelease returns the C heap freed by tree-sitter work to the OS.
// A parse allocates many times its source size in small C blocks. The C
// allocator keeps those blocks after the tree is closed, so without a
// release a burst of large inputs stays resident for the life of the
// process. A release walks only free memory, holding each allocator arena's
// lock while it trims that arena, so a computation that overlaps it can wait
// on the lock (tens of milliseconds after a burst that freed hundreds of
// MiB) and then fault its pages back in; it never affects correctness.
type heapRelease struct {
	release func()

	mu     sync.Mutex
	active int       // computations in flight
	last   time.Time // latest computation finish
	first  time.Time // first finish since the last release
	armed  bool
}

func (h *heapRelease) begin() {
	h.mu.Lock()
	h.active++
	h.mu.Unlock()
}

func (h *heapRelease) end() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active--
	h.last = time.Now()
	if !h.armed {
		h.armed = true
		h.first = h.last
		time.AfterFunc(releaseIdle, h.fire)
	}
}

func (h *heapRelease) fire() {
	h.mu.Lock()
	now := time.Now()
	if now.Sub(h.first) < releaseMaxDelay {
		wait := releaseIdle - now.Sub(h.last)
		if h.active > 0 {
			wait = releaseIdle
		}
		if wait > 0 {
			time.AfterFunc(wait, h.fire)
			h.mu.Unlock()
			return
		}
	}
	h.armed = false
	h.mu.Unlock()
	h.release()
}
