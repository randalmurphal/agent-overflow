package fswatchtest

import (
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/fswatch"
)

// Feed is the event source of a loop under test: the loop reads only the
// events the test sends, in the order sent, so a test states an exact
// operation sequence that no platform's fsnotify backend can reorder or
// drop. The watcher's own fsnotify watcher stays armed, for a handler that
// adds or removes watches, but its events and errors are drained and
// dropped.
type Feed struct {
	watcher *fsnotify.Watcher
	events  chan fsnotify.Event
	errors  chan error
	drained sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	closed    bool
}

// NewFeed starts dropping watcher's events and errors; the loop reads the
// test's through Source.
func NewFeed(watcher *fsnotify.Watcher) *Feed {
	f := &Feed{watcher: watcher, events: make(chan fsnotify.Event), errors: make(chan error)}
	f.drained.Add(2)
	go func() {
		defer f.drained.Done()
		for range watcher.Events {
		}
	}()
	go func() {
		defer f.drained.Done()
		for range watcher.Errors {
		}
	}()
	return f
}

// Source is what the loop under test reads. Its Close closes the watcher.
func (f *Feed) Source() fswatch.Source {
	return fswatch.Source{Events: f.events, Errors: f.errors, Close: f.Close}
}

// Close closes the watcher, which ends the drains; the loop calls it
// through its source.
func (f *Feed) Close() error {
	f.closeOnce.Do(func() {
		f.closeErr = f.watcher.Close()
		f.drained.Wait()
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
	})
	return f.closeErr
}

// Closed reports whether the loop closed its source.
func (f *Feed) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// Send hands the loop one event and returns once the loop has taken it.
// The loop takes events one at a time, so by then every event sent before
// it has been handled in full, including any debounce wait it started.
func (f *Feed) Send(t testing.TB, event fsnotify.Event) {
	t.Helper()
	select {
	case f.events <- event:
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop never took %v", event)
	}
}
