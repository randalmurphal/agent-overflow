// Package pump is the mechanism every polling pump shares: one goroutine
// per watched resource, kept alive by the handles of the callers watching
// it, paused while every handle reports itself hidden, woken when one
// shows again, and handed the requests a person's action makes, paused or
// not. Retry is the schedule a pump follows after a failed poll.
//
// The owner supplies the rest: the goroutine and its loop, what a poll
// reads and emits, the errors a refused request returns, and the lock that
// guards a core's handle counts and dead mark. One owner lock may guard
// many cores. The package knows nothing of what is polled.
package pump

import (
	"context"
	"sync"
	"sync/atomic"
)

// Refusals are the owner's errors for a request a core cannot hand over.
type Refusals struct {
	// Stopped is returned when the core was stopped before it served the
	// request.
	Stopped error
	// LifetimeEnded is returned when the lifetime the core was created
	// under ended before the goroutine took the request.
	LifetimeEnded error
}

// Core is one pump's lifetime, its handles and their visibility votes, and
// the request hand-off to its goroutine, whose requests have body type R.
//
// Every method named ...Locked must be called with the owner's lock held;
// the others are safe without it. A Core is made by NewCore, stored in
// place (a field of the owner's pump), and not copied once in use.
// The zero value is a core with no handles that is not dead, for an
// owner's test fixture that never runs it.
type Core[R any] struct {
	lifetime context.Context
	refusals Refusals
	// ctx is every call the pump makes; cancel ends it, which cancels a
	// call in flight.
	ctx    context.Context
	cancel context.CancelFunc
	// done closes once, on Stop.
	done     chan struct{}
	stopOnce sync.Once
	// dead, refs and active are guarded by the owner's lock. dead is set
	// when the pump stops for good; refs counts the handles and active the
	// ones not hidden.
	dead   bool
	refs   int
	active int
	// paused mirrors active <= 0 for the goroutine, which reads it without
	// the lock. wake nudges the loop on resume; a wake already queued does
	// the same job.
	paused   atomic.Bool
	wake     chan struct{}
	requests chan *Request[R]
}

// NewCore is a live core with no handles, to store in place. Its context
// derives from lifetime, and Request refuses with refusals' errors, both
// of which must be set.
func NewCore[R any](lifetime context.Context, refusals Refusals) Core[R] {
	if refusals.Stopped == nil || refusals.LifetimeEnded == nil {
		panic("pump: NewCore needs both refusal errors")
	}
	ctx, cancel := context.WithCancel(lifetime)
	return Core[R]{
		lifetime: lifetime,
		refusals: refusals,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		requests: make(chan *Request[R]),
	}
}

// Context is the context of every call the pump makes. It ends on Stop,
// on Cancel and when the core's lifetime ends.
func (c *Core[R]) Context() context.Context { return c.ctx }

// Done closes when the core is stopped: the loop exits on it, and nothing
// is emitted for the pump after it.
func (c *Core[R]) Done() <-chan struct{} { return c.done }

// Wake receives a nudge when the pump resumes after a pause.
func (c *Core[R]) Wake() <-chan struct{} { return c.wake }

// Requests receives each request a caller hands the goroutine.
func (c *Core[R]) Requests() <-chan *Request[R] { return c.requests }

// Paused reports whether every handle is hidden. The goroutine reads it
// without the owner's lock.
func (c *Core[R]) Paused() bool { return c.paused.Load() }

// TakeLocked counts one new handle, visible until it votes otherwise, and
// wakes a pump the last hidden handle had paused: the poll the newcomer
// waits for would otherwise wait out a tick the pause made it miss. A dead
// core takes no handle: it panics, since its goroutine polls nothing.
func (c *Core[R]) TakeLocked() {
	if c.dead {
		panic("pump: handle taken on a dead core")
	}
	c.refs++
	c.active++
	resumed := c.refs > 1 && c.active == 1
	c.paused.Store(false)
	if resumed {
		c.wakeUp()
	}
}

// VoteLocked applies one handle's change of visibility; the caller tracks
// each handle's last vote and calls only on a change. A vote that resumes
// the pump wakes its loop. A dead core counts nothing.
func (c *Core[R]) VoteLocked(active bool) {
	if c.dead {
		return
	}
	if active {
		c.active++
	} else {
		c.active--
	}
	c.paused.Store(c.active <= 0)
	if active && c.active == 1 {
		c.wakeUp()
	}
}

// ReleaseLocked drops one handle, visible or not as its last vote said,
// and reports whether it was the last. A last release marks the core dead,
// so a poll it has in flight cannot store or stamp state after a
// replacement exists; the caller then stops it once it released the lock.
// A release with no handle held panics.
func (c *Core[R]) ReleaseLocked(wasActive bool) (last bool) {
	if c.refs <= 0 {
		panic("pump: release without a handle")
	}
	if wasActive {
		c.active--
	}
	c.refs--
	if c.refs == 0 {
		c.dead = true
		return true
	}
	c.paused.Store(c.active <= 0)
	return false
}

// HandlesLocked reports the handles held and how many of them are visible.
func (c *Core[R]) HandlesLocked() (refs, active int) { return c.refs, c.active }

// DeadLocked reports whether the core stopped for good: a dead pump
// stores, stamps and emits nothing, and no caller is handed a handle on
// it.
func (c *Core[R]) DeadLocked() bool { return c.dead }

// MarkDeadLocked marks the core dead, for an owner whose goroutine exited
// on its own.
func (c *Core[R]) MarkDeadLocked() { c.dead = true }

// Cancel ends the core's context without stopping it, for an owner whose
// goroutine exited on its own.
func (c *Core[R]) Cancel() { c.cancel() }

// Stop ends the pump: its context is cancelled, which cancels a call in
// flight, and Done closes, which ends the loop and refuses requests.
// Repeated calls do nothing.
func (c *Core[R]) Stop() {
	c.stopOnce.Do(func() {
		c.cancel()
		close(c.done)
	})
}

func (c *Core[R]) wakeUp() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Request hands body to the goroutine and waits until it ran, returning
// the error the goroutine finished it with. It is served whether or not
// the pump is paused: a person asked. It returns Refusals.Stopped when the
// core stops first, ctx's error when the caller gives up, and
// Refusals.LifetimeEnded when the core's lifetime ends before the
// goroutine took the request.
func (c *Core[R]) Request(ctx context.Context, body R) error {
	req := &Request[R]{body: body, done: make(chan struct{})}
	select {
	case c.requests <- req:
	case <-c.done:
		return c.refusals.Stopped
	case <-ctx.Done():
		return ctx.Err()
	case <-c.lifetime.Done():
		return c.refusals.LifetimeEnded
	}
	select {
	case <-req.done:
		return req.err
	case <-c.done:
		return c.refusals.Stopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Request is one hand-off to a pump goroutine. The goroutine reads Body,
// does the work, and calls Finish exactly once.
type Request[R any] struct {
	body R
	done chan struct{}
	err  error
}

// Body is what the caller asked for.
func (r *Request[R]) Body() R { return r.body }

// Finish reports the work done, with err as the caller's answer.
func (r *Request[R]) Finish(err error) {
	r.err = err
	close(r.done)
}
