package app

import (
	"context"
	"sync/atomic"
	"time"

	"agent-overflow/internal/eventchan"
)

// pumpCore is what every forge pump shares (the PR pump, the pipeline
// pump, the PR list pump): a goroutine that ends when done closes or ctx
// ends, the handles that keep it alive and their visibility votes, the
// sequence its frames are stamped with, and the requests an RPC hands it.
// Everything but paused, wake and requests is guarded by App.prUpdates.mu,
// which every pump shares, so one global sequence orders all their
// frames.
type pumpCore struct {
	done chan struct{}
	// ctx is every forge call the pump makes, derived from the app's
	// lifetime; cancel ends it when the pump dies, which cancels a forge
	// request the pump has in flight.
	ctx    context.Context
	cancel context.CancelFunc
	// dead is set when the pump stops for good; a dead pump stores, stamps
	// and emits nothing, and no caller is handed a reference on it.
	dead bool
	// paused suspends polling while every handle reports itself hidden:
	// active counts the handles that do not, refs all of them. wake nudges
	// the loop on resume.
	paused atomic.Bool
	wake   chan struct{}
	refs   int
	active int
	// seq stamps the state the pump holds, from App.prUpdates.seq.
	seq      uint64
	requests chan *prPumpRequest
}

// newPumpCore is a live core under the app's lifetime.
func (a *App) newPumpCore() pumpCore {
	ctx, cancel := context.WithCancel(a.lifeCtx())
	return pumpCore{
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
		wake:     make(chan struct{}, 1),
		requests: make(chan *prPumpRequest),
	}
}

// takeLocked counts one new handle, visible until it says otherwise, and
// wakes a pump the last hidden handle had paused: the poll the newcomer
// waits for would otherwise wait out a tick the pause made it miss.
func (c *pumpCore) takeLocked() {
	c.refs++
	c.active++
	resumed := c.refs > 1 && c.active == 1
	c.paused.Store(false)
	if resumed {
		c.wakeUp()
	}
}

// voteLocked applies one handle's change of visibility (the caller checked
// that it changed) and reports whether the pump resumed, which the caller
// answers with wakeUp once it released the lock.
func (c *pumpCore) voteLocked(active bool) (resumed bool) {
	if active {
		c.active++
	} else {
		c.active--
	}
	c.paused.Store(c.active <= 0)
	return active && c.active == 1
}

// releaseLocked drops one handle and reports whether it was the last; a
// last release marks the pump dead, so a poll it has in flight cannot
// stamp state after a replacement exists.
func (c *pumpCore) releaseLocked(wasActive bool) (last bool) {
	if wasActive {
		c.active--
	}
	c.refs--
	if c.refs <= 0 {
		c.dead = true
		return true
	}
	c.paused.Store(c.active <= 0)
	return false
}

// wakeUp nudges a resumed pump's loop without blocking: the channel is
// capacity-1, and a wake already queued does the same job.
func (c *pumpCore) wakeUp() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// stop ends a pump taken out of its map: its loop exits and a forge call
// it has in flight is cancelled.
func (c *pumpCore) stop() {
	c.cancel()
	close(c.done)
}

// requestPump hands a request to the pump goroutine and waits for it to
// run. The pump serves requests whether or not it is paused: this is a
// person's action, not a background tick.
func (a *App) requestPump(ctx context.Context, core *pumpCore, req *prPumpRequest) error {
	req.done = make(chan struct{})
	select {
	case core.requests <- req:
	case <-core.done:
		return errPRUpdatesUnknownSubscription
	case <-ctx.Done():
		return ctx.Err()
	case <-a.lifeCtx().Done():
		return ErrShuttingDown
	}
	select {
	case <-req.done:
		return req.err
	case <-core.done:
		return errPRUpdatesUnknownSubscription
	case <-ctx.Done():
		return ctx.Err()
	}
}

// emitPumpEvent emits on behalf of a pump unless it was torn down in the
// meantime.
func (a *App) emitPumpEvent(core *pumpCore, channel eventchan.Channel, event any) {
	select {
	case <-core.done:
		return
	default:
	}
	a.emit(channel, event)
}

// retrySchedule is a pump's retry timer after a failed poll: a doubling
// delay from the retry base up to the interval, or for a rate limit a wait
// for its release, during which hold says no background poll runs. Owned
// by the pump goroutine.
type retrySchedule struct {
	timer <-chan time.Time
	delay time.Duration
	hold  time.Time
}

// arm re-arms the schedule after a poll: none while healthy, the release
// for a rate limit, else the next doubling step.
func (r *retrySchedule) arm(retrying bool, release time.Time, base, interval time.Duration) {
	switch {
	case !retrying:
		r.timer, r.delay, r.hold = nil, 0, time.Time{}
	case !release.IsZero():
		r.delay, r.hold = 0, release
		r.timer = time.After(heldUntil(release))
	default:
		r.hold = time.Time{}
		if r.delay == 0 {
			r.delay = min(base, interval)
		} else {
			r.delay = min(r.delay*2, interval)
		}
		r.timer = time.After(r.delay)
	}
}

// pausedRetry is a retry that fired while paused: the resume polls once
// and the curve restarts from there.
func (r *retrySchedule) pausedRetry() {
	r.timer = nil
	r.delay = 0
}

// held reports whether a rate limit holds the poll, re-arming the timer
// for its release when a resume after a pause dropped it.
func (r *retrySchedule) held() bool {
	if !time.Now().Before(r.hold) {
		return false
	}
	if r.timer == nil {
		r.timer = time.After(heldUntil(r.hold))
	}
	return true
}
