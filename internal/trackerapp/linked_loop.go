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
// issues' cards and the history due (SyncLinked) until no unit reports
// More, and the mirror's cleanup runs (FinishMirrorCleanup). A round with
// no client connected is skipped whole. A failure is reported to
// Deps.LoopFailed and logged, and the loop goes on.

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

// StartLinkedSync arms the linked loop, the app's call from its
// unattended-work boot step: it starts now when a Jira or Linear
// connection is on, and from then on with the first one turned on or
// added, and it stops with the last one turned off or removed. Calling it
// again does nothing more. Its cost with no connection on is the one read
// of the connections. When that read fails, the loop runs (syncLoopLocked)
// and the error is returned.
func (s *Service) StartLinkedSync() error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.subMu.Lock()
	armed := s.subs.loop.armed
	s.subs.loop.armed = true
	s.subMu.Unlock()
	if armed {
		return nil
	}
	_, err := s.syncLoopLocked()
	return err
}

// syncLoopLocked starts or stops the armed loop to match whether a Jira
// or Linear connection is on, one read of the connections. A loop it
// stops is returned for the caller to join once it released opMu: the
// loop's tick may be waiting for it. When the read fails, the loop runs
// (started if none runs) and the error is returned: a loop with no
// connection on costs a read of the connections a round, and the next
// change to the connections stops it, where a loop left stopped with a
// connection on would wait for a change that may never come. The caller
// holds opMu, so changes to the connections and their effect on the loop
// apply in one order.
func (s *Service) syncLoopLocked() (<-chan struct{}, error) {
	s.subMu.Lock()
	armed := s.subs.loop.armed
	s.subMu.Unlock()
	if !armed {
		return nil, nil
	}
	rows, listErr := s.store.ListTrackerConnections()
	if listErr != nil {
		listErr = fmt.Errorf("trackerapp: list connections for the linked-issues loop: %w", listErr)
	}
	on := listErr != nil
	for _, row := range rows {
		on = on || (row.Enabled && row.Kind != store.TrackerKindTempo)
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch l := &s.subs.loop; {
	case on && l.cancel == nil && !s.subs.halted:
		ctx, cancel := context.WithCancel(s.life)
		l.cancel, l.done = cancel, make(chan struct{})
		s.workers.Add(1)
		go s.runLinkedLoop(ctx, l.done)
	case !on && l.cancel != nil:
		return s.stopLoopLocked(), nil
	}
	return nil, listErr
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
	for first := true; ; first = false {
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
		// The cleanup runs once after the settle wait, and after each round.
		if first || connected {
			if err := s.FinishMirrorCleanup(); err != nil && !errors.Is(err, ErrStopped) {
				s.loopFailure(err)
			}
		}
		next.Reset(s.cadence.Linked)
	}
}

// linkedRound syncs the linked issues of each Jira or Linear connection
// that is on (linkedConnection).
func (s *Service) linkedRound(ctx context.Context) {
	if err := s.enter(); err != nil {
		return
	}
	rows, err := s.store.ListTrackerConnections()
	s.leave()
	if err != nil {
		s.loopFailure(fmt.Errorf("trackerapp: list connections for the linked issues: %w", err))
		return
	}
	for _, row := range rows {
		if !row.Enabled || row.Kind == store.TrackerKindTempo {
			continue
		}
		s.linkedConnection(ctx, row.ID)
		if ctx.Err() != nil {
			return
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
	due := LinkedDue{Cards: true, History: true}
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
			case u.Unit == UnitHistory:
				due.History = true
			}
		}
		if !due.Cards && !due.History || !s.clientConnected() {
			return
		}
	}
}

// loopFailure reports err, a failure of the loop's work it survives.
func (s *Service) loopFailure(err error) {
	s.logf("trackerapp: the linked-issues loop: %v", err)
	s.loopFailed(err)
}
