package fswatchtest

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/fswatch"
)

// Relay stands between a watcher's real fsnotify watcher and its loop, as
// the loop's event source. It forwards the real events and errors through
// unbuffered channels, so a forwarded event is one the loop has taken, and
// it lets the test send events of its own the same way.
//
// That gives a test an order-free way to know an event was handled in
// full: wait until the real event it caused was forwarded (WaitDelivered),
// then send one event of its own (Send). The loop takes events one at a
// time, so when that send returns the earlier event's handling, and any
// wait it started, are done. Waiting names the event's path and kind, so
// nothing depends on the order events arrive in.
type Relay struct {
	watcher *fsnotify.Watcher
	events  chan fsnotify.Event
	errors  chan error
	done    chan struct{}

	closeOnce  sync.Once
	closeErr   error
	forwarders sync.WaitGroup

	mu sync.Mutex
	// delivered and consumed count forwarded events by path and kind.
	delivered map[deliveryKey]int
	consumed  map[deliveryKey]int
	closed    bool
	// forwarded is closed and replaced each time an event is forwarded,
	// so WaitDelivered blocks on the next one instead of polling.
	forwarded chan struct{}
}

// deliveryKey is one kind of event for one path.
type deliveryKey struct {
	path string
	op   fsnotify.Op
}

// kinds are the event kinds a test can wait for.
var kinds = []fsnotify.Op{fsnotify.Create, fsnotify.Write, fsnotify.Remove, fsnotify.Rename, fsnotify.Chmod}

// NewRelay starts forwarding watcher's events and errors; the loop reads
// them through Source.
func NewRelay(watcher *fsnotify.Watcher) *Relay {
	r := &Relay{
		watcher: watcher, events: make(chan fsnotify.Event), errors: make(chan error), done: make(chan struct{}),
		delivered: make(map[deliveryKey]int), consumed: make(map[deliveryKey]int), forwarded: make(chan struct{}),
	}
	r.forwarders.Add(2)
	go func() {
		defer r.forwarders.Done()
		for {
			select {
			case <-r.done:
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				select {
				case r.events <- event:
					r.record(event)
				case <-r.done:
					return
				}
			}
		}
	}()
	go func() {
		defer r.forwarders.Done()
		for {
			select {
			case <-r.done:
				return
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				select {
				case r.errors <- err:
				case <-r.done:
					return
				}
			}
		}
	}()
	return r
}

func (r *Relay) record(event fsnotify.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, op := range kinds {
		if event.Op&op != 0 {
			r.delivered[deliveryKey{filepath.Clean(event.Name), op}]++
		}
	}
	close(r.forwarded)
	r.forwarded = make(chan struct{})
}

// Source is what the loop under test reads.
func (r *Relay) Source() fswatch.Source {
	return fswatch.Source{Events: r.events, Errors: r.errors, Close: r.Close}
}

// Close stops forwarding and closes the real watcher; the loop calls it
// through its source.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		close(r.done)
		r.forwarders.Wait()
		r.closeErr = r.watcher.Close()
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	})
	return r.closeErr
}

// Closed reports whether the loop closed its source.
func (r *Relay) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Send hands the loop an event of the test's own and returns once the
// loop has taken it, which is after it finished every event before it.
func (r *Relay) Send(t testing.TB, event fsnotify.Event) {
	t.Helper()
	select {
	case r.events <- event:
	case <-time.After(5 * time.Second):
		t.Fatalf("the loop never took %v", event)
	}
}

// WaitDelivered blocks until a real event of kind op naming path has been
// forwarded since the last WaitDelivered for that path and kind returned.
// It fails the test after five seconds: no such event came, so nothing
// watches where it happened.
func (r *Relay) WaitDelivered(t testing.TB, path string, op fsnotify.Op) {
	t.Helper()
	key := deliveryKey{filepath.Clean(path), op}
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		if r.delivered[key] > r.consumed[key] {
			r.consumed[key] = r.delivered[key]
			r.mu.Unlock()
			return
		}
		forwarded := r.forwarded
		r.mu.Unlock()
		select {
		case <-forwarded:
		case <-deadline:
			t.Fatalf("no %v event for %s reached the loop", op, path)
		}
	}
}
