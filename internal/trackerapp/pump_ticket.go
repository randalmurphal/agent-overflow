package trackerapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"time"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/tracker"
)

// ticketPump is an open ticket's pump: one goroutine for every handle on
// the issue, with no timer. It reads the ticket when it opens, on a
// person's refresh, when another read stored a changed card of the issue,
// and when shown again after it missed a read; its connection's round
// (ticket_round.go) polls it while shown. Its core and the fields below
// viewChanged are under subMu.
type ticketPump struct {
	core pump.Core[struct{}]
	key  ticketKey
	// every is the connection's round interval, the answer's PollEvery.
	every  time.Duration
	exited chan struct{}
	// cardChanged nudges the pump to read the ticket: another read stored
	// a changed card of the issue. viewChanged nudges it to rebuild its
	// view from what it holds (rebuildTicket): another read stored the
	// issue's detail, a changed card of an issue the view shows, or a
	// changed user or status. One queued nudge of each is enough.
	cardChanged, viewChanged chan struct{}

	// check is what the next read checks the ticket against, nil when it
	// decodes the mirror's copy. settled is when the last read settled into
	// the pump began (Service.nextReadLocked), zero before the first.
	check   *detailCheck
	settled uint64
	// missed reports a read the pump owes: a round passed while it was
	// hidden, a nudge came while hidden, or a read failed. stale reports a
	// view rebuild it owes from a nudge that came while hidden.
	missed, stale bool
	// digest identifies the view the subscribers were last told of (viewed
	// reports one is set), so a read that finds the same view emits
	// nothing. shows are the other issues that view shows (showsOf).
	digest uint64
	viewed bool
	shows  []tracker.IssueID
	// held is the last view when the pump, not the mirror, holds the
	// ticket's content (check.held is its document): the mirror does not
	// keep the issue. Which one holds it is decided on every successful
	// read. Nothing else of a ticket is held.
	held *heldDetail
	// failure is the failure the subscribers were last told of; changed and
	// due are what the next event carries.
	failure      *FailureView
	changed, due bool
	over         bool
}

// heldDetail is a view a ticket pump holds, with when its read began.
type heldDetail struct {
	view   DetailView
	readAt int64
}

// TicketView is an open ticket as its subscribers were last told of it:
// its detail when the mirror, or the ticket's pump, holds one (Loaded),
// with when its read began, and its failure, nil while its reads succeed.
type TicketView struct {
	Loaded  bool         `json:"loaded"`
	Detail  *DetailView  `json:"detail,omitempty"`
	ReadAt  int64        `json:"read_at,omitempty"`
	Failure *FailureView `json:"failure,omitempty"`
}

// TicketSubscription is a ticket subscribe's answer: the handle, the
// sequence number the answer is as of, and the ticket as of it. With no
// detail loaded, the pump's first read follows, and its event says so.
// PollEvery is the connection's round interval in milliseconds.
type TicketSubscription struct {
	ID        string `json:"id"`
	Seq       uint64 `json:"seq"`
	PollEvery int64  `json:"poll_every"`
	TicketView
}

// SubscribeTicket opens issue on connection id for a client: an answer at
// once from what the service holds, with no tracker call, then the pump
// made on the issue's first handle reads it as the tracker says it is now
// (IssueDetail), interactive, and the connection's round reads it every
// Cadence.Ticket while shown. It is refused as SubscribeBoard is.
func (s *Service) SubscribeTicket(id string, issue tracker.IssueID) (TicketSubscription, error) {
	if id == "" || issue == "" {
		return TicketSubscription{}, fmt.Errorf("%w: a ticket subscribe names its connection and issue", ErrInvalidRequest)
	}
	if err := s.enter(); err != nil {
		return TicketSubscription{}, err
	}
	defer s.leave()
	hid, tp, start, seq, told, err := s.takeTicketHandle(ticketKey{connection: id, issue: issue})
	if err != nil {
		return TicketSubscription{}, err
	}
	answer := TicketSubscription{ID: hid, Seq: seq, PollEvery: tp.every.Milliseconds(), TicketView: told}
	if !answer.Loaded {
		v, readAt, err := s.MirroredDetail(id, issue)
		switch {
		case err == nil:
			answer.Loaded, answer.Detail, answer.ReadAt = true, &v, readAt
		case !errors.Is(err, ErrNotMirrored):
			// Another subscriber may have joined the pump meanwhile: it runs
			// unless this was its last handle.
			s.ReleaseSubscription(hid)
			s.subMu.Lock()
			if start {
				s.launchTicketLocked(tp)
			}
			s.subMu.Unlock()
			return TicketSubscription{}, err
		}
	}
	var digest uint64
	if start && answer.Loaded {
		digest = s.digestOf(*answer.Detail)
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if start {
		if !tp.viewed {
			tp.digest, tp.viewed = digest, answer.Loaded
		}
		s.launchTicketLocked(tp)
	}
	return answer, nil
}

// launchTicketLocked starts tp's goroutine and follows its connection's
// round. The caller holds subMu.
func (s *Service) launchTicketLocked(tp *ticketPump) {
	s.launchLocked(&tp.core, tp.exited, func() { s.runTicketPump(tp) })
	s.followRoundLocked(tp.key.connection)
}

// takeTicketHandle takes a handle on key's pump, making the pump when
// there is none; told is the ticket as its subscribers were last told of
// it, as of seq, its detail only when the pump holds one.
func (s *Service) takeTicketHandle(key ticketKey) (id string, tp *ticketPump, start bool, seq uint64, told TicketView, err error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	kind, err := s.subscribableLocked(key.connection)
	if err != nil {
		return "", nil, false, 0, TicketView{}, err
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case s.subs.halted:
		return "", nil, false, 0, TicketView{}, ErrStopped
	case s.subs.ticketHandles >= maxTicketSubscriptions:
		return "", nil, false, 0, TicketView{}, fmt.Errorf("%w: %d ticket subscriptions", ErrTooManySubscriptions, maxTicketSubscriptions)
	}
	tp = s.subs.tickets[key]
	if tp == nil {
		if s.subs.tickets == nil {
			s.subs.tickets = map[ticketKey]*ticketPump{}
		}
		tp = &ticketPump{
			core: s.newCore(), key: key, every: s.cadence.ticket(kind), exited: make(chan struct{}),
			cardChanged: make(chan struct{}, 1), viewChanged: make(chan struct{}, 1),
		}
		s.subs.tickets[key] = tp
		start = true
	}
	id = s.takeHandleLocked(&handle{ticket: tp}, &tp.core)
	if !start {
		// A shown handle on a hidden pump shows it: its connection's round
		// follows.
		s.followRoundLocked(key.connection)
	}
	return id, tp, start, s.nextSeqLocked(), tp.toldLocked(), nil
}

// toldLocked is tp's ticket as its subscribers were last told of it, its
// detail only when the pump holds it. The caller holds subMu.
func (tp *ticketPump) toldLocked() TicketView {
	v := TicketView{Failure: tp.failure}
	if tp.held != nil {
		view := tp.held.view
		v.Loaded, v.Detail, v.ReadAt = true, &view, tp.held.readAt
	}
	return v
}

// TicketDetail is handle id's ticket as its subscribers were last told of
// it: its failure, and the view its pump holds or else the mirror's copy
// (MirroredDetail), not Loaded while neither holds one.
func (s *Service) TicketDetail(id string) (TicketView, error) {
	s.subMu.Lock()
	h := s.subs.handles[id]
	var key ticketKey
	var told TicketView
	if h != nil && h.ticket != nil {
		key, told = h.ticket.key, h.ticket.toldLocked()
	}
	s.subMu.Unlock()
	switch {
	case h == nil || h.ticket == nil:
		return TicketView{}, fmt.Errorf("%w: ticket %s", ErrUnknownSubscription, id)
	case told.Loaded:
		return told, nil
	}
	v, readAt, err := s.MirroredDetail(key.connection, key.issue)
	switch {
	case errors.Is(err, ErrNotMirrored):
		return told, nil
	case err != nil:
		return TicketView{}, err
	}
	told.Loaded, told.Detail, told.ReadAt = true, &v, readAt
	return told, nil
}

// digestOf identifies v's content.
func (s *Service) digestOf(v DetailView) uint64 {
	encoded, err := json.Marshal(v)
	if err != nil {
		// A view always encodes; a failure reads as a change.
		return 0
	}
	return maphash.Bytes(s.digestSeed, encoded)
}

// runTicketPump is a ticket pump's goroutine: an interactive first read
// that records the opening, a full read on a person's request, a read when
// another read stored a changed card of the issue or when shown again after
// it missed one, and a view rebuild with no tracker call when what its view
// shows of other issues changed. A failed read is retried by the
// connection's round (oweRetryLocked).
func (s *Service) runTicketPump(tp *ticketPump) {
	defer s.workers.Done()
	defer close(tp.exited)
	read := func(u Urgency, full, opened bool) ticketsResult {
		return s.readTickets(tp.core.Context(), tp.key.connection, []*ticketPump{tp}, u, full, opened)
	}
	over := read(Interactive, false, true).over
	for !over {
		select {
		case <-tp.core.Done():
			return
		case req := <-tp.core.Requests():
			res := read(Interactive, true, true)
			over = res.over
			err := res.err
			if over {
				err = ErrSubscriptionEnded
			}
			req.Finish(err)
		case <-tp.core.Wake():
			switch missed, stale := s.owedOf(tp); {
			case missed:
				over = read(Background, false, false).over
			case stale:
				s.rebuildTicket(tp)
			}
		case <-tp.cardChanged:
			if !s.oweWhilePaused(tp, true) {
				over = read(Background, false, false).over
			}
		case <-tp.viewChanged:
			if !s.oweWhilePaused(tp, false) {
				s.rebuildTicket(tp)
			}
		}
	}
}

// owedOf takes what tp owes now that it is shown: a read, or a view
// rebuild; neither while it is paused.
func (s *Service) owedOf(tp *ticketPump) (missed, stale bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if tp.core.Paused() {
		return false, false
	}
	missed, stale = tp.missed, tp.stale && !tp.missed
	tp.missed, tp.stale = false, false
	return missed, stale
}

// oweWhilePaused records a nudge that came while tp is paused, a read
// when read is set and else a view rebuild, for the resume; it reports
// whether it recorded it.
func (s *Service) oweWhilePaused(tp *ticketPump, read bool) bool {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if !tp.core.Paused() {
		return false
	}
	if read {
		tp.missed = true
	} else {
		tp.stale = true
	}
	return true
}

// flushTicketLocked emits tp's pending event, if any, or its end. Every
// event names its ticket and carries its failure as of the event. The
// caller holds subMu.
func (s *Service) flushTicketLocked(tp *ticketPump, end bool) {
	if tp.over || (!tp.due && !end) || (tp.core.DeadLocked() && !end) {
		return
	}
	ev := IssueEvent{
		Connection: tp.key.connection, Issue: tp.key.issue, Seq: s.nextSeqLocked(),
		Changed: tp.changed, Failure: tp.failure, Ended: end,
	}
	tp.changed, tp.due = false, false
	s.emitIssue(ev)
}

// tellFailureLocked makes f tp's failure, due to be told when it is not the
// one its subscribers were last told of. The caller holds subMu.
func (tp *ticketPump) tellFailureLocked(f *FailureView) {
	if !sameFailure(tp.failure, f) {
		tp.failure, tp.due = f, true
	}
}

// endTicketLocked ends tp: it stops, it and its handles are forgotten, its
// subscribers are told once, and its connection's round follows. The
// caller holds subMu.
func (s *Service) endTicketLocked(tp *ticketPump) {
	if tp.over {
		return
	}
	// The end is told before the core stops, as a board's is
	// (endBoardLocked).
	s.flushTicketLocked(tp, true)
	tp.over, tp.held, tp.check = true, nil, nil
	tp.core.StopLocked()
	if s.subs.tickets[tp.key] == tp {
		delete(s.subs.tickets, tp.key)
	}
	s.endHandlesLocked(&tp.core)
	s.followRoundLocked(tp.key.connection)
}
