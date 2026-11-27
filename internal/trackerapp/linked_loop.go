package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/store"
)

// The linked loop: one goroutine for the service, running only while a
// Jira or Linear connection is on and only after StartLinkedSync. It waits
// for the first client's reads to settle (Deps.Settled), then runs a round
// at once and every Cadence.Linked: while a client is connected
// (Deps.ClientConnected), each connection that is on syncs its linked
// issues' cards, the names its mirror owes, the history due and the
// evidence credit counts that is due (SyncLinked) until no unit reports
// More, then the mirror's cleanup runs (FinishMirrorCleanup), and then the
// forge pull request history that is due is read (Deps.ReadForgeHistory). A round
// with no client connected is skipped whole, its cleanup included. Every read of the connections, a
// round's included, decides whether the loop runs (followLoopLocked). A
// failure is reported to Deps.LoopFailed and logged, and the loop goes on.

// loopState is the loop's state, under subMu.
type loopState struct {
	// armed is set by StartLinkedSync: from then on a connection change
	// starts or stops the loop.
	armed bool
	// cancel ends the running loop, whose goroutine closes done; both nil
	// while none runs.
	cancel context.CancelFunc
	done   chan struct{}
}

// StartLinkedSync arms the linked loop; it belongs in the app's
// unattended-work boot step, which does not call it yet. The loop starts
// now when a Jira or Linear connection is on, and from then on with the
// first one turned on or added, and it stops with the last one turned off
// or removed. Each call reads the connections and starts or stops the
// loop to match. Its cost with no connection on is that one read. When
// the read fails, the loop runs (followLoopLocked) and the error is
// returned. A loop it stops has exited when it returns.
func (s *Service) StartLinkedSync() error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	s.opMu.Lock()
	s.subMu.Lock()
	s.subs.loop.armed = true
	s.subMu.Unlock()
	done, err := s.syncLoopLocked()
	s.opMu.Unlock()
	if done != nil {
		<-done
	}
	return err
}

// syncLoopLocked starts or stops the armed loop to match the connections
// after a change to them, one read of the connections; it reads nothing
// while the loop is not armed. A loop it stops is returned for the caller
// to join once it released opMu: the loop's tick may be waiting for it.
// The caller holds opMu.
func (s *Service) syncLoopLocked() (<-chan struct{}, error) {
	s.subMu.Lock()
	armed := s.subs.loop.armed
	s.subMu.Unlock()
	if !armed {
		return nil, nil
	}
	rows, err := s.listForLoop()
	return s.followLoopLocked(rows, err), err
}

// listForLoop reads the connections for the loop.
func (s *Service) listForLoop() ([]store.TrackerConnection, error) {
	rows, err := s.store.ListTrackerConnections()
	if err != nil {
		return nil, fmt.Errorf("trackerapp: list connections for the linked-issues loop: %w", err)
	}
	return rows, nil
}

// followLoopLocked is the one rule for whether the armed loop runs, applied
// to every read of the connections, rows or the read's failure listErr:
// at its start and on each change to the connections (syncLoopLocked), and
// at each of its rounds (linkedRound). It runs while a Jira or Linear
// connection is on, and when the read failed: a loop with no connection on
// costs a read of the connections a round and the next successful read
// stops it, where a loop left stopped with a connection on would wait for
// a change that may never come. A loop it stops is returned for a caller
// other than the loop to join. The caller holds opMu, so the reads of the
// connections and their effect on the loop apply in one order.
func (s *Service) followLoopLocked(rows []store.TrackerConnection, listErr error) <-chan struct{} {
	on := listErr != nil
	for _, row := range rows {
		on = on || (row.Enabled && row.Kind != store.TrackerKindTempo)
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch l := &s.subs.loop; {
	case !l.armed:
	case on && l.cancel == nil && !s.subs.halted:
		ctx, cancel := context.WithCancel(s.life)
		l.cancel, l.done = cancel, make(chan struct{})
		s.workers.Add(1)
		go s.runLinkedLoop(ctx, l.done)
	case !on && l.cancel != nil:
		return s.stopLoopLocked()
	}
	return nil
}

// stopLoopLocked cancels the running loop, if any, and returns the channel
// that closes when it has exited. The caller holds subMu.
func (s *Service) stopLoopLocked() <-chan struct{} {
	l := &s.subs.loop
	if l.cancel == nil {
		return nil
	}
	done := l.done
	l.cancel()
	l.cancel, l.done = nil, nil
	return done
}

// runLinkedLoop is the loop's goroutine; it exits when ctx ends.
func (s *Service) runLinkedLoop(ctx context.Context, done chan struct{}) {
	defer s.workers.Done()
	defer close(done)
	if err := s.settled(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.loopFailure(fmt.Errorf("trackerapp: wait for the first reads to settle: %w", err))
	}
	next := time.NewTimer(0)
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-next.C:
		}
		connected := s.clientConnected()
		if connected {
			s.linkedRound(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		// The cleanup runs after each round; the one after the settle wait
		// is the app's start (cleanup.go).
		if connected {
			if err := s.FinishMirrorCleanup(); err != nil && !errors.Is(err, ErrStopped) {
				s.loopFailure(err)
			}
			if err := s.readForgeHist(ctx); err != nil && ctx.Err() == nil {
				s.loopFailure(fmt.Errorf("trackerapp: read the forge pull request history: %w", err))
			}
		}
		next.Reset(s.cadence.Linked)
	}
}

// linkedRound syncs the linked issues of each Jira or Linear connection
// that is on (linkedConnection). Its read of the connections decides
// whether the loop goes on (followLoopLocked): a loop that finds none on
// stops itself, ending ctx, and is not joined here.
func (s *Service) linkedRound(ctx context.Context) {
	if err := s.enter(); err != nil {
		return
	}
	s.opMu.Lock()
	rows, err := s.listForLoop()
	s.followLoopLocked(rows, err)
	s.opMu.Unlock()
	s.leave()
	if err != nil {
		s.loopFailure(err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if row.Enabled && row.Kind != store.TrackerKindTempo {
			s.linkedConnection(ctx, row.ID)
		}
	}
}

// linkedConnection syncs connection id's linked issues: ticks of its
// linked scope while a unit reports More, each running only the units
// still owed one (a unit that finished is not run again), each tick
// bounded as SyncLinked bounds it. It stops at a failure, the tracker's
// (recorded in the scope's sync state; a rate limit's release is the next
// round's) or any other (reported to the loop), when ctx ends, and when no
// client is connected any more.
func (s *Service) linkedConnection(ctx context.Context, id string) {
	due := LinkedDue{Cards: true, Names: true, History: true, Evidence: true}
	for {
		out, err := s.SyncLinked(ctx, id, due, Background)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			if !errors.Is(err, ErrStopped) {
				s.loopFailure(fmt.Errorf("trackerapp: sync the linked issues of %s: %w", id, err))
			}
			return
		case out.Failure != nil || out.Ended:
			return
		}
		due = LinkedDue{}
		for _, u := range out.Ran {
			switch {
			case !u.More:
			case u.Unit == UnitCards:
				due.Cards = true
			case u.Unit == UnitNames:
				due.Names = true
			case u.Unit == UnitHistory:
				due.History = true
			case u.Unit == UnitEvidence:
				due.Evidence = true
			}
		}
		if due == (LinkedDue{}) || !s.clientConnected() {
			return
		}
	}
}

// loopFailure reports err, a failure of the loop's work it survives.
func (s *Service) loopFailure(err error) {
	s.logf("trackerapp: the linked-issues loop: %v", err)
	s.loopFailed(err)
}
