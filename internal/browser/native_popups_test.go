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

// An engine's stop closes its held popups through the receipts their
// discards settle: a discard made while the stop's close is queued on a
// stalled UI thread answers that the close is pending, never that the
// popup is gone, and the stop returns once the close ran, with its
// outcome. A close a discard began before the stop is waited for too.
func TestAStopsPopupCloseIsTheOneADiscardJoins(t *testing.T) {
	loop := &queuedLoop{}
	thread := uicall.NewThread(loop.post, func() bool { return true }, time.Hour)
	var closes atomic.Int32
	closed := errors.New("the close's own answer")
	release := func(unsafe.Pointer) error {
		thread.Settle(func() { closes.Add(1) })
		return closed
	}
	popups := newHeldPopups()
	popups.hold("discarded", heldPopupView{view: unsafe.Pointer(new(byte)), n: 1})
	popups.hold("held", heldPopupView{view: unsafe.Pointer(new(byte)), n: 2})
	early, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	if err := popups.discard(early, "discarded", release); !errors.Is(err, errNativeReleasePending) {
		t.Fatalf("a discard on a stalled UI thread answered %v, want the close pending", err)
	}
	cancel()

	stopped := make(chan error, 1)
	go func() { stopped <- popups.closeAll(t.Context(), release, false) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		popups.mu.Lock()
		closing := popups.releasing["held"] != nil
		popups.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stop never began the held popup's close")
		}
	}
	during, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := popups.discard(during, "held", release); !errors.Is(err, errNativeReleasePending) {
		t.Fatalf("a discard during the stop's queued close answered %v, want the close pending", err)
	}
	select {
	case err := <-stopped:
		t.Fatalf("the stop returned %v before the closes ran", err)
	default:
	}
	for deadline := time.Now().Add(5 * time.Second); closes.Load() < 2; time.Sleep(time.Millisecond) {
		loop.drain()
		if time.Now().After(deadline) {
			t.Fatal("the closes never ran")
		}
	}
	if err := <-stopped; !errors.Is(err, closed) {
		t.Fatalf("the stop answered %v, want the closes' outcome", err)
	}
	if got := closes.Load(); got != 2 {
		t.Fatalf("the views were closed %d times, want once each", got)
	}
	if err := popups.discard(t.Context(), "held", release); err != nil {
		t.Fatalf("a discard after the stop's close ran answered %v", err)
	}
}

// On the UI thread the stop's closes run in the caller, inline, so a stop
// there never waits on the thread it is blocking.
func TestAStopOnTheUIThreadClosesItsPopupsInline(t *testing.T) {
	var closes int
	release := func(unsafe.Pointer) error {
		closes++
		return nil
	}
	popups := newHeldPopups()
	popups.hold("held", heldPopupView{view: unsafe.Pointer(new(byte)), n: 1})
	if err := popups.closeAll(t.Context(), release, true); err != nil {
		t.Fatalf("the stop answered %v", err)
	}
	if closes != 1 {
		t.Fatalf("the stop closed %d views in its caller, want 1", closes)
	}
	if held := popups.take("held"); held.view != nil {
		t.Fatal("the stop left the popup held")
	}
}
