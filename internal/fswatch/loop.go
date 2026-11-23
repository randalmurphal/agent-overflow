// Package fswatch runs the event loop shared by the app's debounced
// filesystem watchers. The caller arms an fsnotify watcher and decides what
// each event means; the loop owns the debounce, the emit, watcher-error
// logging and Close.
//
// The rule: a create, write, remove or rename event that the handler
// reports as a change starts a new debounce wait and abandons the pending
// one, so Emit runs once, after the wait started by the last change of a
// burst lapses. Permission-only events never reach the handler. A watcher
// error is logged with the watcher's label and the loop keeps running.
//
// Close begins by closing the stop signal: a handler waiting on it gives
// up, and no handler or Emit call starts after it. Close then waits for the
// loop goroutine to exit, so for any call already running, and only then
// closes the source, so no call ever runs on a closed source. A handler or
// Emit must therefore never call Close on its own loop: Close would wait
// for the loop goroutine that is calling it, and deadlock.
//
// The handler and Emit run on the loop goroutine, one call at a time: the
// handler is never called concurrently with itself or with Emit, so a
// handler may keep unlocked state that only it touches.
package fswatch

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Config is how one loop debounces and what it calls.
type Config struct {
	// Label names the watcher in setup errors and in the log line for a
	// watcher error ("theme watcher: ...").
	Label string
	// Debounce is how long the loop waits after the last change of a burst.
	Debounce time.Duration
	// After starts one debounce wait. Nil means time.After; tests pass a
	// clock whose waits they fire (fswatchtest.Clock).
	After func(time.Duration) <-chan time.Time
	// Emit is called once per lapsed debounce wait.
	Emit func()
}

// Validate reports the first option the loop cannot run with. A watcher
// calls it before it acquires anything, and Start calls it again.
func (c Config) Validate() error {
	switch {
	case c.Debounce <= 0:
		return fmt.Errorf("%s: debounce must be positive", c.Label)
	case c.Emit == nil:
		return fmt.Errorf("%s: emit callback is required", c.Label)
	}
	return nil
}

// Source is what the loop reads: events, watcher errors, and how to
// release them. In production it is an armed fsnotify watcher
// (FromWatcher).
type Source struct {
	Events <-chan fsnotify.Event
	Errors <-chan error
	// Close releases the source. Loop.Close calls it once.
	Close func() error
}

// FromWatcher is the Source of an armed fsnotify watcher.
func FromWatcher(watcher *fsnotify.Watcher) Source {
	return Source{Events: watcher.Events, Errors: watcher.Errors, Close: watcher.Close}
}

// Handler reports whether event changed content the watcher watches. stop
// is closed once Close begins; a handler that waits gives up then.
type Handler func(event fsnotify.Event, stop <-chan struct{}) bool

// Loop is one running watch loop.
type Loop struct {
	config Config
	source Source
	handle Handler
	after  func(time.Duration) <-chan time.Time
	// stop is closed when Close begins. It is the loop's exit signal and
	// the stop channel every handler receives.
	stop      chan struct{}
	closeOnce sync.Once
	exited    sync.WaitGroup
	// beforeAdmit, when set by a test, runs just before each admission.
	beforeAdmit func()
}

// Start runs the loop on its own goroutine. Start owns source from the
// call on: it closes source when it refuses to start, and Close closes it.
func Start(config Config, source Source, handle Handler) (*Loop, error) {
	return start(config, source, handle, nil)
}

func start(config Config, source Source, handle Handler, beforeAdmit func()) (*Loop, error) {
	invalid := config.Validate()
	switch {
	case invalid != nil:
	case source.Events == nil || source.Errors == nil || source.Close == nil:
		invalid = fmt.Errorf("%s: an event source is required", config.Label)
	case handle == nil:
		invalid = fmt.Errorf("%s: an event handler is required", config.Label)
	}
	if invalid != nil {
		if source.Close != nil {
			invalid = errors.Join(invalid, source.Close())
		}
		return nil, invalid
	}
	after := config.After
	if after == nil {
		after = time.After
	}
	loop := &Loop{config: config, source: source, handle: handle, after: after, stop: make(chan struct{}), beforeAdmit: beforeAdmit}
	loop.exited.Add(1)
	go loop.run()
	return loop, nil
}

// Close stops the loop and closes its source. Once Close has begun no
// handler or Emit call starts, including for a wait that lapsed while a
// handler held the loop. The source is closed after the loop goroutine has
// exited, so after any call that started before Close began has returned.
// Close is safe to call more than once.
func (l *Loop) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		close(l.stop)
		l.exited.Wait()
		closeErr = l.source.Close()
	})
	return closeErr
}

func (l *Loop) run() {
	defer l.exited.Done()
	// wait is the pending debounce wait. Each change replaces it, so only
	// the wait started by the last change of a burst can emit; an abandoned
	// wait's channel is never read again.
	var wait <-chan time.Time
	for {
		select {
		case <-l.stop:
			return
		case event, ok := <-l.source.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if !l.admit() {
				return
			}
			if l.handle(event, l.stop) {
				wait = l.after(l.config.Debounce)
			}
		case err, ok := <-l.source.Errors:
			if !ok {
				return
			}
			log.Printf("%s: %v", l.config.Label, err)
		case <-wait:
			wait = nil
			// A select with both ready picks either, so a wait that lapsed
			// while a handler held the loop can be chosen after Close began.
			if !l.admit() {
				return
			}
			l.config.Emit()
		}
	}
}

// admit reports whether a handler or Emit call may start: true until Close
// has begun. It runs on the loop goroutine just before the call, and Close
// closes the source only after that goroutine exits.
func (l *Loop) admit() bool {
	if l.beforeAdmit != nil {
		l.beforeAdmit()
	}
	select {
	case <-l.stop:
		return false
	default:
		return true
	}
}
