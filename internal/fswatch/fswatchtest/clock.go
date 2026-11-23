// Package fswatchtest supplies what tests hand to an fswatch loop: a
// debounce clock whose waits the test fires, and a relay that feeds a
// watcher's real events to its loop and lets the test know when one was
// handled. Production code must not import it.
package fswatchtest

import (
	"sync"
	"testing"
	"time"
)

// Clock replaces a loop's debounce waits (fswatch.Config.After). No wait
// fires until the test fires it by its index, so a burst stays inside one
// debounce window however the scheduler spaces its events, and the test
// knows which wait it released.
type Clock struct {
	mu        sync.Mutex
	waits     []chan time.Time
	durations []time.Duration
	// started is closed and replaced each time a wait starts, so WaitArmed
	// blocks on the next start instead of polling.
	started chan struct{}
}

// After starts one held wait. Pass it as fswatch.Config.After.
func (c *Clock) After(duration time.Duration) <-chan time.Time {
	wait := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, wait)
	c.durations = append(c.durations, duration)
	if c.started != nil {
		close(c.started)
		c.started = nil
	}
	return wait
}

// Armed reports how many waits the loop has started.
func (c *Clock) Armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

// Durations returns the duration of every wait started so far, in order.
func (c *Clock) Durations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.durations...)
}

// WaitArmed blocks until the loop has started at least n waits. It fails
// the test after five seconds, which only a hung loop reaches.
func (c *Clock) WaitArmed(t testing.TB, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		if len(c.waits) >= n {
			c.mu.Unlock()
			return
		}
		if c.started == nil {
			c.started = make(chan struct{})
		}
		started := c.started
		c.mu.Unlock()
		select {
		case <-started:
		case <-deadline:
			t.Fatalf("the loop started %d debounce waits, want at least %d", c.Armed(), n)
		}
	}
}

// Fire lapses the index-th wait the loop started, counting from 0.
func (c *Clock) Fire(t testing.TB, index int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if index >= len(c.waits) {
		t.Fatalf("fire wait %d: the loop started %d", index, len(c.waits))
	}
	c.waits[index] <- time.Now()
}
