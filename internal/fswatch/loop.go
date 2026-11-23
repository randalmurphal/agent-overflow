// Package fswatch runs the event loop shared by the app's debounced
// filesystem watchers. The caller arms an fsnotify watcher and decides what
// each event means; the loop owns the debounce, the emit, watcher-error
// logging and Close.
//
// The rule: a create, write, remove or rename event that Handle reports as
// a change starts a new debounce wait and abandons the pending one, so Emit
// runs once, after the wait started by the last change of a burst lapses.
// Permission-only events never reach Handle. A watcher error is logged with
// the watcher's label and the loop keeps running. Close drops a pending
// wait without emitting and returns once the loop has exited.
//
// Handle and Emit run on the loop goroutine, one call at a time: Handle is
// never called concurrently with itself or with Emit, so a handler may keep
// unlocked state that only it touches.
package fswatch

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Config is what one loop runs on.
type Config struct {
	// Label names the watcher in setup errors and in the log line for a
	// watcher error ("theme watcher: ...").
	Label string
	// Watcher is the armed fsnotify watcher. Start owns it from the call on:
	// Start closes it when it fails, and Close closes it.
	Watcher *fsnotify.Watcher
	// Debounce is how long the loop waits after the last change of a burst.
	Debounce time.Duration
	// After starts one debounce wait. Nil means time.After; tests pass a
	// clock whose waits they release (fswatchtest.Clock).
	After func(time.Duration) <-chan time.Time
	// Handle reports whether event changed content the caller watches. stop
	// is closed once Close begins; a handler that waits gives up then.
	Handle func(event fsnotify.Event, stop <-chan struct{}) bool
	// Emit is called once per lapsed debounce wait.
	Emit func()
}

// Loop is one running watch loop.
type Loop struct {
	config    Config
	after     func(time.Duration) <-chan time.Time
	done      chan struct{}
	closeOnce sync.Once
	exited    sync.WaitGroup
}

// Start validates config and runs the loop on its own goroutine.
func Start(config Config) (*Loop, error) {
	var invalid error
	switch {
	case config.Debounce <= 0:
		invalid = fmt.Errorf("%s: debounce must be positive", config.Label)
	case config.Emit == nil:
		invalid = fmt.Errorf("%s: emit callback is required", config.Label)
	}
	if invalid != nil {
		return nil, errors.Join(invalid, config.Watcher.Close())
	}
	after := config.After
	if after == nil {
		after = time.After
	}
	loop := &Loop{config: config, after: after, done: make(chan struct{})}
	loop.exited.Add(1)
	go loop.run()
	return loop, nil
}

// Close stops the loop and closes the fsnotify watcher. A pending debounce
// wait is dropped without emitting. Close returns after the loop goroutine
// has exited, and is safe to call more than once.
func (l *Loop) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		close(l.done)
		closeErr = l.config.Watcher.Close()
		l.exited.Wait()
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
		case <-l.done:
			return
		case event, ok := <-l.config.Watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if l.config.Handle(event, l.done) {
				wait = l.after(l.config.Debounce)
			}
		case err, ok := <-l.config.Watcher.Errors:
			if !ok {
				return
			}
			log.Printf("%s: %v", l.config.Label, err)
		case <-wait:
			wait = nil
			l.config.Emit()
		}
	}
}
