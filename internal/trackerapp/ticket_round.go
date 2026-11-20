package trackerapp

import (
	"cmp"
	"context"
	"errors"
	"hash/maphash"
	"slices"
	"time"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/tracker"
)

// A connection's round polls its open tickets together: one goroutine and
// one timer per connection, alive while a client shows one of its
// tickets, reads every shown ticket each Cadence.Ticket of the
// connection's kind in one verified read (readTickets), and owns the
// retry schedule of the connection's ticket reads, a pump's own included.
// A hidden ticket is left out and owes a read its pump makes when shown
// again.

// ticketRound is a connection's round. Its fields below done are under
// subMu.
type ticketRound struct {
	conn   string
	every  time.Duration
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// owed wakes the round when a pump's own read failed; owing and owedAt
	// (when a rate limit lifts) say what it owes.
	owed   chan struct{}
	owing  bool
	owedAt time.Time
}

// followRoundLocked starts connection conn's round when a ticket of it is
// shown and none runs, and stops it when none is shown, returning the
// channel that closes when its goroutine exited, for a caller that joins
// it. The caller holds subMu.
func (s *Service) followRoundLocked(conn string) <-chan struct{} {
	var shown *ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == conn && !tp.over && !tp.core.DeadLocked() && !tp.core.Paused() {
			shown = tp
			break
		}
	}
	switch rd := s.subs.rounds[conn]; {
	case shown != nil && rd == nil && !s.subs.halted:
		ctx, cancel := context.WithCancel(s.life)
		rd = &ticketRound{conn: conn, every: shown.every, ctx: ctx, cancel: cancel, done: make(chan struct{}), owed: make(chan struct{}, 1)}
		if s.subs.rounds == nil {
			s.subs.rounds = map[string]*ticketRound{}
		}
		s.subs.rounds[conn] = rd
		s.workers.Add(1)
		go s.runTicketRound(rd)
	case shown == nil && rd != nil:
		return s.stopRoundLocked(conn)
	}
	return nil
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

// runTicketRound is a round's goroutine: a round every interval, and on
// the retry schedule while the connection's ticket reads fail.
func (s *Service) runTicketRound(rd *ticketRound) {
	defer s.workers.Done()
	defer close(rd.done)
	next := time.NewTimer(rd.every)
	defer next.Stop()
	retry := pump.Retry{Base: s.cadence.RetryBase, Interval: rd.every}
	round := func() {
		res := s.roundTick(rd)
		if rd.ctx.Err() != nil {
			return
		}
		retry.Arm(res.failed, res.retryAt)
		next.Reset(rd.every)
	}
	for {
		select {
		case <-rd.ctx.Done():
			return
		case <-rd.owed:
			// A pump's own read failed: the round retries it, unless a retry
			// is armed already.
			if at, ok := s.takeOwed(rd); ok && retry.C() == nil {
				retry.Arm(true, at)
			}
		case <-retry.C():
			round()
		case <-next.C:
			// A rate limit leaves the timer unarmed: its release runs the round.
			if retry.Held() {
				continue
			}
			round()
		}
	}
}

// takeOwed takes what rd owes from its pumps' failed reads.
func (s *Service) takeOwed(rd *ticketRound) (time.Time, bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	at, ok := rd.owedAt, rd.owing
	rd.owing, rd.owedAt = false, time.Time{}
	return at, ok
}

// oweRetryLocked has connection conn's round retry a pump's failed read,
// no earlier than at when set (a rate limit). The caller holds subMu.
func (s *Service) oweRetryLocked(conn string, at time.Time) {
	rd := s.subs.rounds[conn]
	if rd == nil {
		return
	}
	rd.owing = true
	if at.After(rd.owedAt) {
		rd.owedAt = at
	}
	select {
	case rd.owed <- struct{}{}:
	default:
	}
}

// roundTick reads the connection's shown tickets together. A hidden one
// owes its pump a read; one whose first read has not settled is its
// pump's to read.
func (s *Service) roundTick(rd *ticketRound) ticketsResult {
	s.subMu.Lock()
	var members []*ticketPump
	for k, tp := range s.subs.tickets {
		switch {
		case k.connection != rd.conn || tp.over || tp.core.DeadLocked() || tp.settled == 0:
		case tp.core.Paused():
			tp.missed = true
		default:
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

// ticketsResult is how a read of tickets ended: failed for the read as a
// whole, with when a rate limit lifts, err for a failure that was not the
// tracker's, over when every pump read ended.
type ticketsResult struct {
	failed  bool
	retryAt time.Time
	err     error
	over    bool
}

// nextReadLocked orders the reads settled into ticket pumps by when they
// began. The caller holds subMu.
func (s *Service) nextReadLocked() uint64 {
	s.subs.reads++
	return s.subs.reads
}

// readTickets reads the tickets of pumps, all of connection conn, together
// (readDetails), each checked against what its pump settled last, passes
// what the read stored through the change funnel under the root the read
// held, and settles each pump with its own outcome. opened records a
// person's opening. A failure is told to each pump; a pump's own failed
// read is owed to the round's retry schedule.
func (s *Service) readTickets(ctx context.Context, conn string, pumps []*ticketPump, u Urgency, full, opened bool) ticketsResult {
	s.subMu.Lock()
	began := s.nextReadLocked()
	reads := make([]*ticketRead, len(pumps))
	for i, tp := range pumps {
		reads[i] = &ticketRead{issue: tp.key.issue}
		if tp.check != nil {
			prior := *tp.check
			reads[i].prior = &prior
		}
		tp.missed = false
	}
	s.subMu.Unlock()
	at := millis(s.now())
	var openedAt int64
	if opened {
		openedAt = at
	}
	res, err := s.readDetails(ctx, conn, reads, u, full, openedAt)
	s.publish(res.root, Scope{Connection: conn}, res.ch, &tickOrigin{tickets: pumps})
	var out ticketsResult
	var failure *FailureView
	stopped := err != nil && ctx.Err() != nil
	ended := errors.Is(err, errEnded) || errors.Is(err, ErrDisabled) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrStopped)
	switch {
	case stopped, ended:
	case res.failure != nil:
		failure = &FailureView{Kind: string(res.failure.Kind), Message: res.failure.Message, At: at}
		out.failed, out.retryAt = true, res.retryAt
	case err != nil:
		failure = internalFailure(at)
		out.failed, out.err = true, err
		s.logf("trackerapp: the read of %d open tickets of %s failed: %v", len(pumps), conn, err)
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	out.over = true
	for i, tp := range pumps {
		switch {
		case tp.over || tp.core.DeadLocked():
		case ended:
			s.endTicketLocked(tp)
		case stopped:
			out.over = false
		default:
			s.settleTicketLocked(tp, began, reads[i], failure, res.kind, at)
			out.over = false
		}
	}
	if out.failed && len(pumps) == 1 {
		s.oweRetryLocked(conn, out.retryAt)
	}
	return out
}

// settleTicketLocked settles rd, what a read that began at began found of
// tp's ticket, into tp, unless a read that began later settled already: a
// failure of the read as a whole, the tracker leaving the ticket out, or
// the document now and which of the mirror and the pump holds it. A
// changed view or failure is told. kind is the connection's kind as the
// read held it. The caller holds subMu.
func (s *Service) settleTicketLocked(tp *ticketPump, began uint64, rd *ticketRead, failure *FailureView, kind string, at int64) {
	if began < tp.settled {
		if rd.stored {
			// The mirror's copy may be this read's: the view is rebuilt from it.
			nudge(tp.viewChanged)
		}
		return
	}
	tp.settled = began
	switch {
	case failure != nil:
		tp.missed = true
		if failure.Kind == string(FailureInternal) {
			// What the read stored may not be in the view: the next read
			// decodes and rebuilds it.
			tp.check = nil
		}
		tp.tellFailureLocked(failure)
	case rd.err != nil:
		tp.tellFailureLocked(notFoundFailure(kind, at))
	default:
		tp.check = nil
		if rd.check != (detailCheck{}) {
			check := rd.check
			tp.check = &check
		}
		switch {
		case rd.check.held == nil:
			tp.held = nil
		case rd.view != nil:
			tp.held = &heldDetail{view: *rd.view, readAt: rd.check.readAt}
		}
		if rd.view != nil {
			tp.shows = rd.shows
			tp.viewLocked(rd.digest)
		}
		tp.tellFailureLocked(nil)
	}
	s.flushTicketLocked(tp, false)
}

// viewLocked records digest as tp's view, a change to tell when it differs
// from the one its subscribers were last told of. The caller holds subMu.
func (tp *ticketPump) viewLocked(digest uint64) {
	if !tp.viewed || digest != tp.digest {
		tp.digest, tp.viewed, tp.changed, tp.due = digest, true, true, true
	}
}

// nudge queues one nudge on c, a channel of capacity one.
func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// rebuildTicket rebuilds tp's view, with no tracker call, from the
// document its pump holds or else the mirror's copy, and tells a changed
// view. A read that settled after the rebuild began wins. A rebuild that
// fails is told as a failure outside the tracker when no failure is told,
// and leaves the next read to decode and rebuild.
func (s *Service) rebuildTicket(tp *ticketPump) {
	s.subMu.Lock()
	began := s.nextReadLocked()
	var held *tracker.Detail
	if tp.check != nil {
		held = tp.check.held
	}
	s.subMu.Unlock()
	at := millis(s.now())
	v, d, check, ok, err := s.rebuiltView(tp.key, held)
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case tp.over || tp.core.DeadLocked() || tp.settled > began:
		return
	case err != nil:
		s.logf("trackerapp: rebuild the view of open ticket %s of %s: %v", tp.key.issue, tp.key.connection, err)
		tp.check = nil
		if tp.failure == nil {
			tp.tellFailureLocked(internalFailure(at))
		}
	case !ok:
		// Neither holds a document: the next read reads it.
		tp.check, tp.missed = nil, true
		return
	default:
		switch {
		case held == nil:
			tp.check, tp.held = &check, nil
		case tp.held != nil:
			tp.held = &heldDetail{view: v, readAt: tp.held.readAt}
		}
		tp.shows = showsOf(d)
		tp.viewLocked(s.digestOf(v))
	}
	s.flushTicketLocked(tp, false)
}

// rebuiltView is key's view built from held when the pump holds its
// document, else from the mirror's copy decoded, with that copy's check;
// ok is false when neither holds one.
func (s *Service) rebuiltView(key ticketKey, held *tracker.Detail) (DetailView, tracker.Detail, detailCheck, bool, error) {
	root, err := s.readRoot(key.connection)
	if errors.Is(err, ErrNotMirrored) {
		return DetailView{}, tracker.Detail{}, detailCheck{}, false, nil
	}
	if err != nil {
		return DetailView{}, tracker.Detail{}, detailCheck{}, false, err
	}
	defer s.leave()
	var check detailCheck
	d := held
	if d == nil {
		m, stored, ok, err := s.storedDetail(root, key.issue)
		if err != nil || !ok {
			return DetailView{}, tracker.Detail{}, detailCheck{}, false, err
		}
		d = &stored
		check = detailCheck{readAt: m.ReadAt, updated: millis(stored.Card.Updated), doc: maphash.Bytes(s.digestSeed, m.Data)}
	}
	v, err := s.detailView(root, *d)
	return v, *d, check, err == nil, err
}
