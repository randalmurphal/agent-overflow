// Package fswatchtest supplies the debounce clock tests hand to an
// fswatch loop. Production code must not import it.
package fswatchtest

import (
	"sync"
	"testing"
	"time"
)

// Clock replaces a loop's debounce waits (fswatch.Config.After). No wait
// fires until the test releases it, so a burst stays inside one debounce
// window however the scheduler spaces its writes and their events.
type Clock struct {
	mu        sync.Mutex
	pending   []chan time.Time
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
	c.pending = append(c.pending, wait)
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
	return len(c.durations)
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
		if len(c.durations) >= n {
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

// FireAbandoned fires every wait started so far except the latest, as if
// the clock passed the deadlines the loop has already replaced, and returns
// how many it fired.
func (c *Clock) FireAbandoned() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) < 2 {
		return 0
	}
	abandoned := c.pending[:len(c.pending)-1]
	for _, wait := range abandoned {
		wait <- time.Now()
	}
	c.pending = c.pending[len(c.pending)-1:]
	return len(abandoned)
}

// FireLatest fires the wait the loop started last and reports whether one
// was pending.
func (c *Clock) FireLatest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return false
	}
	c.pending[len(c.pending)-1] <- time.Now()
	c.pending = nil
	return true
}
