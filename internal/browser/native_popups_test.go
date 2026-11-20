//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"agent-overflow/internal/browser/uicall"
)

// queuedLoop is a UI loop whose posts wait until the test runs them, as a
// stalled GTK or AppKit main thread does.
type queuedLoop struct {
	mu     sync.Mutex
	queued []func()
}

func (l *queuedLoop) post(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queued = append(l.queued, fn)
}

func (l *queuedLoop) drain() {
	l.mu.Lock()
	queued := l.queued
	l.queued = nil
	l.mu.Unlock()
	for _, fn := range queued {
		fn()
	}
}

// waitingContext closes waiting the first time its caller reads Done, which
// a discard does only once it holds its popup's receipt.
type waitingContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// A native discard on a stalled UI thread answers at its caller's
// deadline that the close is pending, not at the dispatcher's timeout; a
// retry before the close has run joins it and says so too, never that the
// popup is gone; the close runs once when the thread gets to it, and a
// discard waiting then answers its outcome.
func TestANativeDiscardAnswersWithinItsDeadlineAndARetryJoinsIt(t *testing.T) {
	loop := &queuedLoop{}
	const dispatch = 130 * time.Millisecond
	thread := uicall.NewThread(loop.post, func() bool { return true }, dispatch)
	var closes atomic.Int32
	closed := errors.New("the close's own answer")
	release := func(unsafe.Pointer) error {
		if !thread.Settle(func() { closes.Add(1) }) {
			return errors.New("the loop went away")
		}
		return closed
	}
	popups := newHeldPopups()
	view := heldPopupView{view: unsafe.Pointer(new(byte)), opener: "opener", n: 1}
	popups.hold("popup", view)

	for i, what := range []string{"the first discard", "a retry"} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		began := time.Now()
		err := popups.discard(ctx, "popup", release)
		cancel()
		if !errors.Is(err, errNativeReleasePending) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s on a stalled UI thread answered %v, want the close pending", what, err)
		}
		if took := time.Since(began); took >= dispatch {
			t.Fatalf("%s took %v, past its 10ms deadline to the dispatcher's timeout", what, took)
		}
		if got := closes.Load(); got != 0 {
			t.Fatalf("after attempt %d the close ran %d times before the thread did", i, got)
		}
	}
	if held := popups.take("popup"); held.view != nil {
		t.Fatal("a popup being discarded can still be attached")
	}
	waiting := &waitingContext{Context: t.Context(), waiting: make(chan struct{})}
	waited := make(chan error, 1)
	go func() { waited <- popups.discard(waiting, "popup", release) }()
	<-waiting.waiting
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		loop.drain()
		if closes.Load() > 0 || time.Now().After(deadline) {
			break
		}
	}
	if err := <-waited; !errors.Is(err, closed) {
		t.Fatalf("the discard waiting for the close answered %v, want its outcome", err)
	}
	if err := popups.discard(t.Context(), "popup", release); err != nil {
		t.Fatalf("a discard after the close ran answered %v", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("the popup's view was closed %d times, want once", got)
	}
}
