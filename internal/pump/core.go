// Package pump is the mechanism every polling pump shares: one goroutine
// per watched resource, kept alive by the handles of the callers watching
// it, paused while every handle reports itself hidden, woken when one
// shows again, and handed the requests a person's action makes, paused or
// not. Retry is the schedule a pump follows after a failed poll.
//
// The owner supplies the rest: the goroutine and its loop, what a poll
// reads and emits, the errors a refused request returns, and the lock that
// guards a core's handles and dead mark. One owner lock may guard many
// cores. The package knows nothing of what is polled.
//
// A core ends one way, StopLocked, whether its last handle is released or
// its goroutine exits on its own: it is marked dead under the owner's
// lock, its context is cancelled and Done closes, which refuses every
// request still waiting.
package pump

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Refusals are the owner's errors for a request a core cannot hand over.
type Refusals struct {
	// Stopped is returned when the core stopped before it served the
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
// place (a field of the owner's pump), and not copied once in use. The
// zero value reads as live with no handles (DeadLocked, HandlesLocked,
// Paused) for an owner's test fixture; every other method panics on it.
type Core[R any] struct {
	lifetime context.Context
	refusals Refusals
	// ctx is every call the pump makes; cancel ends it, which cancels a
	// call in flight.
	ctx    context.Context
	cancel context.CancelFunc
	// done closes once, on StopLocked.
	done     chan struct{}
	stopOnce sync.Once
	// dead, refs and active are guarded by the owner's lock. dead is set
	// when the core stops; refs counts the handles and active the visible
	// ones.
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

func (c *Core[R]) mustBeMade() {
	if c.done == nil {
		panic("pump: Core used without NewCore")
	}
}

// Context is the context of every call the pump makes. It ends on
// StopLocked and when the core's lifetime ends.
func (c *Core[R]) Context() context.Context {
	c.mustBeMade()
	return c.ctx
}

// Done closes when the core stops: the loop exits on it, and nothing is
// emitted for the pump after it.
func (c *Core[R]) Done() <-chan struct{} {
	c.mustBeMade()
	return c.done
}

// Wake receives a nudge when the pump resumes after a pause.
func (c *Core[R]) Wake() <-chan struct{} {
	c.mustBeMade()
	return c.wake
}

// Requests receives each request a caller hands the goroutine.
func (c *Core[R]) Requests() <-chan *Request[R] {
	c.mustBeMade()
	return c.requests
}

// Paused reports whether every handle is hidden. The goroutine reads it
// without the owner's lock.
func (c *Core[R]) Paused() bool { return c.paused.Load() }

// TakeLocked registers one new handle, visible until it votes otherwise,
// and wakes a pump the last hidden handle had paused: the poll the
// newcomer waits for would otherwise wait out a tick the pause made it
// miss. A dead core takes no handle: it panics, since its goroutine polls
// nothing.
func (c *Core[R]) TakeLocked() *Handle[R] {
	c.mustBeMade()
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
	return &Handle[R]{core: c, visible: true}
}

// HandlesLocked reports the handles held and how many of them are visible.
func (c *Core[R]) HandlesLocked() (refs, active int) { return c.refs, c.active }

// DeadLocked reports whether the core stopped: a dead pump stores, stamps
// and emits nothing, and no caller is handed a handle on it.
func (c *Core[R]) DeadLocked() bool { return c.dead }

// StopLocked ends the core: it is marked dead, so a poll in flight cannot
// store or stamp state after a replacement exists, its context is
// cancelled, which cancels a call in flight, and Done closes, which ends
// the loop and refuses every waiting request with Refusals.Stopped. It is
// the one way a core ends: on the last release (Handle.ReleaseLocked), and
// from the owner when its goroutine exits on its own. Repeated calls do
// nothing.
func (c *Core[R]) StopLocked() {
	c.mustBeMade()
	c.dead = true
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

// ErrNotTaken marks a request whose caller gave up before the goroutine
// took it: the goroutine never ran it.
var ErrNotTaken = errors.New("pump: the request was not taken")

// Request hands body to the goroutine and waits until it ran, returning
// the error the goroutine finished it with. It is served whether or not
// the pump is paused: a person asked. It returns Refusals.Stopped when the
// core stops first, including after the goroutine took it and exited
// without finishing it, ctx's error when the caller gives up, wrapped
// with ErrNotTaken when the goroutine had not taken the request, and
// Refusals.LifetimeEnded when the core's lifetime ends before the
// goroutine took the request, stopped or not. The pump's own goroutine
// must not call it.
func (c *Core[R]) Request(ctx context.Context, body R) error {
	c.mustBeMade()
	req := &Request[R]{body: body, done: make(chan struct{})}
	select {
	case c.requests <- req:
	case <-c.done:
		// A core whose goroutine exited because its lifetime ended stops
		// too; the caller is told the lifetime ended.
		if c.lifetime.Err() != nil {
			return c.refusals.LifetimeEnded
		}
		return c.refusals.Stopped
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrNotTaken, ctx.Err())
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

// Handle is one caller's reference on a core, from TakeLocked. It
// remembers its own vote, so a repeated vote changes nothing, and it is
// released once. Its methods run under the owner's lock. A Handle is used
// through the pointer TakeLocked returned and never copied: a copy would
// hold its own vote and release, and count the one reference twice. go vet
// refuses a copy (noCopy).
type Handle[R any] struct {
	_        noCopy
	core     *Core[R]
	visible  bool
	released bool
}

// noCopy marks a type that go vet's copylocks check refuses to copy.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// VisibleLocked reports whether the handle's caller shows the pump: its
// last vote, visible from its take until it votes otherwise. A released
// handle shows nothing.
func (h *Handle[R]) VisibleLocked() bool { return h.visible && !h.released }

// VoteLocked records whether the handle's caller shows the pump. The core
// pauses while every handle is hidden, and a vote that resumes it wakes
// its loop. A vote that repeats the handle's last one, or lands on a dead
// core, changes nothing; a vote on a released handle panics.
func (h *Handle[R]) VoteLocked(visible bool) {
	if h.released {
		panic("pump: vote on a released handle")
	}
	c := h.core
	if h.visible == visible || c.dead {
		return
	}
	h.visible = visible
	if visible {
		c.active++
	} else {
		c.active--
	}
	c.paused.Store(c.active <= 0)
	if visible && c.active == 1 {
		c.wakeUp()
	}
}

// ReleaseLocked drops the handle and reports whether it was the last, in
// which case the core is stopped (StopLocked). A second release panics.
func (h *Handle[R]) ReleaseLocked() (last bool) {
	if h.released {
		panic("pump: handle released twice")
	}
	h.released = true
	c := h.core
	if h.visible {
		c.active--
	}
	c.refs--
	if c.refs == 0 {
		c.StopLocked()
		return true
	}
	c.paused.Store(c.active <= 0)
	return false
}

// Request is one hand-off to a pump goroutine. The goroutine reads Body,
// does the work, and calls Finish once; a goroutine that exits without
// finishing it leaves the caller to Refusals.Stopped.
type Request[R any] struct {
	body     R
	done     chan struct{}
	err      error
	finished bool
}

// Body is what the caller asked for.
func (r *Request[R]) Body() R { return r.body }

// Finish reports the work done, with err as the caller's answer. A second
// call panics.
func (r *Request[R]) Finish(err error) {
	if r.finished {
		panic("pump: request finished twice")
	}
	r.finished = true
	r.err = err
	close(r.done)
}
