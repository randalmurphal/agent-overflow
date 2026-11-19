package app

import (
	"context"
	"log"
	"sync"
	"time"

	"agent-overflow/internal/store"
)

// The remote command watcher's two loops: one checks the watches due by
// now, the other sends the cancels owed by now. Each sleeps until its next
// due row, as the ledger reports it, and a write that adds or brings work
// forward nudges both (nudgeRemoteWatches). With no commands neither does
// anything until a nudge.

// remoteWatchFloor is the shortest gap between two passes of one loop, so
// a row a pass could not reschedule (a refused write) is retried at the
// cadence the watcher always had rather than spun on.
const remoteWatchFloor = 2 * time.Second

// remoteWatchScheduleRetry is how soon a loop reads its schedule again
// after the read failed.
const remoteWatchScheduleRetry = 30 * time.Second

// remoteWatchNudges wakes the loops when work arrives. The zero value is
// ready; a nudge before the loops start is answered by their first pass.
type remoteWatchNudges struct {
	once            sync.Once
	watches, cancel chan struct{}
}

func (n *remoteWatchNudges) channels() (watches, cancel chan struct{}) {
	n.once.Do(func() {
		n.watches, n.cancel = make(chan struct{}, 1), make(chan struct{}, 1)
	})
	return n.watches, n.cancel
}

// nudgeRemoteWatches tells both loops that a watch or an owed cancel was
// added or brought forward: registered, observed, given back, owed, settled
// or unarchived. Non-blocking: a nudge already pending covers this one.
func (a *App) nudgeRemoteWatches() {
	watches, cancel := a.remoteWatchNudges.channels()
	for _, nudge := range []chan struct{}{watches, cancel} {
		select {
		case nudge <- struct{}{}:
		default:
		}
	}
}

func (a *App) startRemoteWatches() {
	if a.backends == nil {
		return
	}
	watches, cancel := a.remoteWatchNudges.channels()
	a.remoteWatchWG.Add(2)
	// Owed cancels are sent on their own loop: one the destination does not
	// answer waits out its timeout without holding up the checks below.
	go func() {
		defer a.remoteWatchWG.Done()
		runDueLoop(a.lifeCtx(), cancel, a.store.NextOwedRemoteCancel, a.retryOwedRemoteCancels)
	}()
	go func() {
		defer a.remoteWatchWG.Done()
		runDueLoop(a.lifeCtx(), watches, a.store.NextRemoteWatchCheck, a.checkDueRemoteWatches)
	}()
}

// checkDueRemoteWatches checks the watches due by now, four destinations at
// a time.
func (a *App) checkDueRemoteWatches(now time.Time) {
	rows, err := a.store.ListRemoteWatches("", now.UnixMilli(), 32)
	if err != nil {
		log.Printf("remote completion watches: %v", err)
		return
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	limit := make(chan struct{}, 4)
	for _, w := range rows {
		select {
		case limit <- struct{}{}:
		case <-a.lifeCtx().Done():
			return
		}
		wg.Go(func() {
			defer func() { <-limit }()
			a.checkRemoteWatch(w)
		})
	}
}

// runDueLoop runs pass at once, then each time the moment next reports
// arrives, no sooner than remoteWatchFloor after the last pass, or a nudge
// comes. With nothing scheduled it waits for a nudge. A nudge raised by a
// pass's own writes is answered by the schedule read after it rather than
// by another pass. It returns when ctx ends.
func runDueLoop(ctx context.Context, nudge <-chan struct{}, next func() (store.ScheduledMoment, error), pass func(time.Time)) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		pass(time.Now())
		select {
		case <-nudge:
		default:
		}
		timer.Stop()
		wake := timer.C
		switch moment, err := next(); {
		case err != nil:
			log.Printf("remote command watcher: read the next due row: %v", err)
			timer.Reset(remoteWatchScheduleRetry)
		case moment.Set:
			timer.Reset(max(time.Until(time.UnixMilli(moment.At)), remoteWatchFloor))
		default:
			wake = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-nudge:
		case <-wake:
		}
	}
}
