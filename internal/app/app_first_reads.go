package app

import (
	"context"
	"sync"
	"time"
)

// firstReadsFallback bounds how long heavy post-boot work waits for a
// client's first catalog reads. A boot that no page opens starts the work
// when it runs out.
const firstReadsFallback = 15 * time.Second

// The catalog reads a page needs before its sidebar can show anything.
const (
	firstReadThreads uint8 = 1 << iota
	firstReadProjects
	firstReadsAll = firstReadThreads | firstReadProjects
)

// firstReadsGate holds heavy post-boot work until a client has read its
// catalogs, so a full scan of the database does not compete with the reads
// that fill the first window.
//
// It settles once a ListThreads and a ListProjects have both answered, or
// once the fallback deadline has passed with no catalog read in flight. A
// read in flight at the deadline is allowed to finish first. Settling is
// permanent.
//
// The fallback runs from the start of the wait. Heavy work starts waiting in
// startUnattendedWork, which runs where clients may begin to read: just
// before MarkReady on an ordinary boot, and after commit on a supervisor
// trial, whose clients are admitted only then. The gate owns no timer; each
// waiter's timer ends with its wait.
type firstReadsGate struct {
	mu       sync.Mutex
	answered uint8
	inFlight int
	expired  bool
	settled  bool
	done     chan struct{}
}

// doneLocked returns the channel closed on settling.
func (g *firstReadsGate) doneLocked() chan struct{} {
	if g.done == nil {
		g.done = make(chan struct{})
	}
	return g.done
}

// read records a catalog read the caller is about to serve. The returned
// function ends it: a nil error means the client has its answer.
func (g *firstReadsGate) read(kind uint8) func(err *error) {
	g.mu.Lock()
	g.inFlight++
	g.mu.Unlock()
	return func(err *error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.inFlight--
		if *err == nil {
			g.answered |= kind
		}
		g.settleIfDueLocked()
	}
}

// expire records that the fallback deadline has passed.
func (g *firstReadsGate) expire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expired = true
	g.settleIfDueLocked()
}

func (g *firstReadsGate) settleIfDueLocked() {
	if g.settled {
		return
	}
	if g.answered != firstReadsAll && (!g.expired || g.inFlight > 0) {
		return
	}
	g.settled = true
	close(g.doneLocked())
}

// await blocks until the gate settles or ctx ends. A waiter outlasts its
// fallback only while a catalog read is in flight.
func (g *firstReadsGate) await(ctx context.Context, fallback time.Duration) error {
	g.mu.Lock()
	if g.settled {
		g.mu.Unlock()
		return nil
	}
	done := g.doneLocked()
	g.mu.Unlock()

	timer := time.NewTimer(fallback)
	defer timer.Stop()
	expire := timer.C
	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-expire:
			expire = nil
			g.expire()
		}
	}
}

// awaitFirstReadsSettled blocks heavy post-boot work until a client has read
// its catalogs or the fallback has passed. It returns ctx's error when ctx
// ends first.
func (a *App) awaitFirstReadsSettled(ctx context.Context) error {
	return a.firstReads.await(ctx, a.firstReadsFallbackDuration())
}

func (a *App) firstReadsFallbackDuration() time.Duration {
	return orDuration(a.maintenance.firstReadsFallback, firstReadsFallback)
}
