package transport

import "time"

const (
	DefaultCoalesceMaxEvents = 50
	DefaultCoalesceWindow    = 16 * time.Millisecond
)

// coalesceBuffer accumulates events for batched delivery over the
// wire. Not safe for concurrent use — owned by the single pumpEvents
// goroutine per connection.
type coalesceBuffer struct {
	events []Event
	timer  *time.Timer
	max    int
	window time.Duration
	flush  func([]Event)
}

func newCoalesceBuffer(max int, window time.Duration, flush func([]Event)) *coalesceBuffer {
	return &coalesceBuffer{
		events: make([]Event, 0, max),
		max:    max,
		window: window,
		flush:  flush,
	}
}

// add appends an event to the buffer. If the count threshold is
// reached, the buffer flushes immediately. Otherwise, the timer is
// started on the first event in the batch (throttle, not debounce)
// so latency is bounded to exactly DefaultCoalesceWindow regardless
// of event rate.
func (b *coalesceBuffer) add(e Event) {
	wasEmpty := len(b.events) == 0
	b.events = append(b.events, e)
	if len(b.events) >= b.max {
		b.flushNow()
		return
	}
	if wasEmpty {
		if b.timer == nil {
			b.timer = time.NewTimer(b.window)
		} else {
			b.timer.Reset(b.window)
		}
	}
}

// flushNow delivers all accumulated events and resets the buffer.
func (b *coalesceBuffer) flushNow() {
	if b.timer != nil {
		b.timer.Stop()
	}
	if len(b.events) == 0 {
		return
	}
	b.flush(b.events)
	clear(b.events)
	b.events = b.events[:0]
}

// timerC returns the timer's channel for use in a select loop.
// Returns nil when no timer is running, which blocks forever in
// select — the correct behavior when the buffer is empty.
func (b *coalesceBuffer) timerC() <-chan time.Time {
	if b.timer == nil {
		return nil
	}
	return b.timer.C
}

// stop flushes any remaining events and releases the timer.
func (b *coalesceBuffer) stop() {
	b.flushNow()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}
