package trackerapp

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// A connection's round makes every background read of its open tickets:
// one goroutine and one timer per connection, alive while a client shows
// one of its tickets. Each ticket carries when its next read is due
// (ticketPump.due), set by whoever changes it under subMu: a settled read
// makes it due with the round's next regular read, a failed read on the
// connection's retry schedule, and a stored change of its card at once
// (readNowLocked). The round's timer is the earliest due time of the shown
// tickets, and each time it fires the round reads every shown ticket then
// due together, in one verified read (readTickets). A hidden ticket is
// left out and keeps its due time, so it is due when shown if its time
// passed while hidden. The pump makes only the reads a person waits on:
// the opening's first read and a refresh (pump_ticket.go).

// ticketRound is a connection's round. Its fields below wake are under
// subMu.
type ticketRound struct {
	conn   string
	every  time.Duration
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// wake makes the round take its timer again from the due times; one
	// queued wake is enough.
	wake chan struct{}

	// next is when the round's next regular read is due: a ticket whose
	// read settled is due then, so the shown tickets are read together.
	// Zero until the first read settles while the round runs.
	next time.Time
	// delay is the step of the retry schedule the connection's failed
	// ticket reads are on, retryAt when the scheduled retry is due, and
	// hold when a rate limit lifts: no round starts before it.
	delay         time.Duration
	retryAt, hold time.Time
}

// followRoundLocked starts connection conn's round when a ticket of it is
// shown and none runs, has a running round take its timer again (a ticket
// shown again may be due), and stops it when none is shown, returning the
// channel that closes when its goroutine exited, for a caller that joins
// it. The caller holds subMu.
func (s *Service) followRoundLocked(conn string) <-chan struct{} {
	var shown *ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == conn && tp.shownLocked() {
			shown = tp
			break
		}
	}
	switch rd := s.subs.rounds[conn]; {
	case shown != nil && rd == nil && !s.subs.halted:
		ctx, cancel := context.WithCancel(s.life)
		rd = &ticketRound{conn: conn, every: shown.every, ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
		if s.subs.rounds == nil {
			s.subs.rounds = map[string]*ticketRound{}
		}
		s.subs.rounds[conn] = rd
		s.workers.Add(1)
		go s.runTicketRound(rd)
	case shown != nil && rd != nil:
		nudge(rd.wake)
	case shown == nil && rd != nil:
		return s.stopRoundLocked(conn)
	}
	return nil
}

// shownLocked reports whether tp is live and a handle shows it. The
// caller holds subMu.
func (tp *ticketPump) shownLocked() bool {
	return !tp.over && !tp.core.DeadLocked() && !tp.core.Paused()
}

// stopRoundLocked cancels connection conn's round, if any, and returns the
// channel that closes when its goroutine exited. The caller holds subMu.
func (s *Service) stopRoundLocked(conn string) <-chan struct{} {
	rd := s.subs.rounds[conn]
	if rd == nil {
		return nil
	}
	rd.cancel()
	delete(s.subs.rounds, conn)
	return rd.done
}

// runTicketRound is a round's goroutine: it waits for the earliest due
// time of the shown tickets, or a wake, and reads the tickets then due.
func (s *Service) runTicketRound(rd *ticketRound) {
	defer s.workers.Done()
	defer close(rd.done)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		at, ok := s.roundAt(rd)
		if ok {
			timer.Reset(max(at.Sub(s.now()), 0))
		} else {
			timer.Stop()
		}
		select {
		case <-rd.ctx.Done():
			return
		case <-rd.wake:
		case <-timer.C:
			s.roundRead(rd, at, false)
		}
	}
}

// roundAt is when rd's next read is due: the earliest due time of the
// shown tickets whose first read settled, no earlier than a rate limit's
// release; ok is false when none is shown.
func (s *Service) roundAt(rd *ticketRound) (at time.Time, ok bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for k, tp := range s.subs.tickets {
		if k.connection == rd.conn && tp.shownLocked() && tp.settled > 0 && (!ok || tp.due.Before(at)) {
			at, ok = tp.due, true
		}
	}
	if ok && rd.hold.After(at) {
		at = rd.hold
	}
	return at, ok
}

// roundRead reads the connection's shown tickets due at at (the timer's
// time, which counts as now when the clock reads earlier), or every shown
// ticket when all is set, together. One whose first read has not settled
// is its pump's to read.
func (s *Service) roundRead(rd *ticketRound, at time.Time, all bool) ticketsResult {
	s.subMu.Lock()
	now := s.now()
	if at.After(now) {
		now = at
	}
	var members []*ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == rd.conn && tp.shownLocked() && tp.settled > 0 && (all || !tp.due.After(now)) {
			members = append(members, tp)
		}
	}
	s.subMu.Unlock()
	if len(members) == 0 {
		return ticketsResult{}
	}
	slices.SortFunc(members, func(a, b *ticketPump) int { return cmp.Compare(a.key.issue, b.key.issue) })
	return s.readTickets(rd.ctx, rd.conn, members, Background, false, false)
}

// readNowLocked makes tp due a read at once, a full one (a stored card
// of its issue changed, or nothing holds its document), and wakes its
// connection's round. A read that began before this does not undo it. The
// caller holds subMu.
func (s *Service) readNowLocked(tp *ticketPump) {
	tp.nudged = s.nextStampLocked()
	tp.due = s.now()
	if rd := s.subs.rounds[tp.key.connection]; rd != nil {
		nudge(rd.wake)
	}
}

// dueLocked makes tp due at t after a read that began at began settled,
// unless a request to read it at once came after that read began. The
// caller holds subMu.
func (s *Service) dueLocked(tp *ticketPump, began uint64, t time.Time) {
	if tp.nudged > began {
		return
	}
	tp.due = t
	if rd := s.subs.rounds[tp.key.connection]; rd != nil {
		nudge(rd.wake)
	}
}

// regularLocked is when a ticket of tp's connection whose read settled now
// is next due: with the round's next regular read, which a read settling
// past it moves an interval on, or an interval from now while no round
// runs. A successful read ends the retry schedule. The caller holds
// subMu.
func (s *Service) regularLocked(tp *ticketPump, now time.Time) time.Time {
	rd := s.subs.rounds[tp.key.connection]
	if rd == nil {
		return now.Add(tp.every)
	}
	rd.delay, rd.retryAt = 0, time.Time{}
	if !rd.next.After(now) {
		rd.next = now.Add(rd.every)
	}
	return rd.next
}

// retryLocked is when a ticket of tp's connection whose read failed now is
// due again: when the rate limit lifts (release), which holds the round
// until then; else with the retry already scheduled, or at the schedule's
// next step, doubling from Cadence.RetryBase up to the interval, as
// pump.Retry's curve does. With no round running it is the first step.
// The caller holds subMu.
func (s *Service) retryLocked(tp *ticketPump, now, release time.Time) time.Time {
	rd := s.subs.rounds[tp.key.connection]
	switch {
	case !release.IsZero():
		if rd != nil && release.After(rd.hold) {
			rd.hold = release
		}
		return release
	case rd == nil:
		return now.Add(min(s.cadence.RetryBase, tp.every))
	case rd.retryAt.After(now):
		return rd.retryAt
	}
	rd.delay = min(max(rd.delay*2, s.cadence.RetryBase), rd.every)
	rd.retryAt = now.Add(rd.delay)
	return rd.retryAt
}

// nextStampLocked is the next stamp of the counter that orders what
// settles into ticket pumps: the reads by when they began, view builds,
// changes to a view's inputs and requests to read at once. The caller
// holds subMu.
func (s *Service) nextStampLocked() uint64 {
	s.subs.stamps++
	return s.subs.stamps
}

// nudge queues one nudge on c, a channel of capacity one.
func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
