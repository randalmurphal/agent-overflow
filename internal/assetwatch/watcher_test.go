package assetwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// debounceClock replaces a watcher's debounce waits. No wait fires until the
// test releases them, so a burst stays inside one debounce window however
// the scheduler spaces its writes and their events.
type debounceClock struct {
	mu        sync.Mutex
	pending   []chan time.Time
	armed     int
	durations []time.Duration
}

func (c *debounceClock) after(duration time.Duration) <-chan time.Time {
	wait := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, wait)
	c.armed++
	c.durations = append(c.durations, duration)
	return wait
}

// armedCount reports how many debounce waits the loop has started.
func (c *debounceClock) armedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed
}

// fireAbandoned fires every wait started so far except the latest, as if
// the clock passed the deadlines the loop has already replaced, and returns
// how many it fired.
func (c *debounceClock) fireAbandoned() int {
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

// fireLatest fires the wait the loop started last.
func (c *debounceClock) fireLatest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return false
	}
	c.pending[len(c.pending)-1] <- time.Now()
	c.pending = nil
	return true
}

// observedWatcher runs the shared core with a held debounce clock and a
// sentinel hook, so a test can tell when the loop has handled every event
// it caused.
type observedWatcher struct {
	core      *watcher
	clock     *debounceClock
	emitted   chan struct{}
	sentinels chan string
	next      int
}

// sentinelExtension names files no watcher in this package treats as
// content. The test predicate reports them before declaring them
// irrelevant.
const sentinelExtension = ".sentinel"

func startObservedWatcher(t *testing.T, label string, relevantName func(name string) bool) *observedWatcher {
	t.Helper()
	observed := &observedWatcher{
		clock:     &debounceClock{},
		emitted:   make(chan struct{}, 8),
		sentinels: make(chan string, 64),
	}
	observe := func(name string) bool {
		if strings.HasSuffix(name, sentinelExtension) {
			// Never block the loop: Close waits for it. A dropped sentinel
			// fails the waiting test with a timeout naming it.
			select {
			case observed.sentinels <- name:
			default:
			}
			return false
		}
		return relevantName(name)
	}
	core, err := openWatcher(label, t.TempDir(), 25*time.Millisecond, observe, func() { observed.emitted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	core.after = observed.clock.after
	core.start()
	t.Cleanup(func() {
		if err := core.Close(); err != nil {
			t.Errorf("close %s: %v", label, err)
		}
	})
	observed.core = core
	return observed
}

func (o *observedWatcher) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(o.core.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sync writes a sentinel file and waits until the loop has handled it. The
// kernel queues a change's events before the syscall returns, and the loop
// handles them in delivery order on one goroutine, so every event from an
// earlier change has been handled too.
func (o *observedWatcher) sync(t *testing.T) {
	t.Helper()
	o.next++
	name := fmt.Sprintf("%d%s", o.next, sentinelExtension)
	o.write(t, name, "")
	deadline := time.After(2 * time.Second)
	for {
		select {
		case seen := <-o.sentinels:
			if seen == name {
				o.checkDurations(t)
				return
			}
		case <-deadline:
			t.Fatalf("the watch loop never handled sentinel %s", name)
		}
	}
}

func (o *observedWatcher) checkDurations(t *testing.T) {
	t.Helper()
	o.clock.mu.Lock()
	defer o.clock.mu.Unlock()
	for _, duration := range o.clock.durations {
		if duration != o.core.debounce {
			t.Fatalf("debounce wait = %v, want %v", duration, o.core.debounce)
		}
	}
}

func (o *observedWatcher) expectNoEmit(t *testing.T, why string) {
	t.Helper()
	select {
	case <-o.emitted:
		t.Fatalf("%s emitted a change event", why)
	default:
	}
}

// expectQueued syncs and asserts that the changes since armedBefore queued
// at least minimum debounce waits.
func (o *observedWatcher) expectQueued(t *testing.T, armedBefore, minimum int, why string) {
	t.Helper()
	o.sync(t)
	if armed := o.clock.armedCount() - armedBefore; armed < minimum {
		t.Fatalf("%s queued %d debounce waits, want at least %d", why, armed, minimum)
	}
	o.expectNoEmit(t, why+" before its debounce elapsed")
}

// expectIgnored syncs and asserts that the changes since armedBefore queued
// nothing.
func (o *observedWatcher) expectIgnored(t *testing.T, armedBefore int, why string) {
	t.Helper()
	o.sync(t)
	if armed := o.clock.armedCount() - armedBefore; armed != 0 {
		t.Fatalf("%s queued %d debounce waits, want none", why, armed)
	}
	o.expectNoEmit(t, why)
}

// releaseOneEmit lets the pending debounce waits elapse oldest first and
// asserts that only the wait queued by the last event emits, exactly once.
func (o *observedWatcher) releaseOneEmit(t *testing.T, why string) {
	t.Helper()
	if o.clock.fireAbandoned() > 0 {
		o.sync(t)
		o.expectNoEmit(t, why+" through a replaced debounce wait")
	}
	if !o.clock.fireLatest() {
		t.Fatalf("%s: no debounce wait is pending", why)
	}
	select {
	case <-o.emitted:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: timed out waiting for a change event", why)
	}
	o.sync(t)
	o.expectNoEmit(t, why+" a second time")
}
