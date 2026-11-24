// Package fswatchtest supplies what tests hand to an fswatch loop: a
// debounce clock whose waits the test fires, and an event source the test
// feeds. Production code must not import it.
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
}

// After starts one held wait. Pass it as fswatch.Config.After.
func (c *Clock) After(duration time.Duration) <-chan time.Time {
	wait := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, wait)
	c.durations = append(c.durations, duration)
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
