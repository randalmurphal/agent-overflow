package trackerapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"time"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// ticketPump is an open ticket's pump: one goroutine for every handle on
// the issue, with no timer. It makes the reads a person waits on (the
// opening's first read and a refresh) and rebuilds its view when an input
// of it moves; every other read of the ticket is its connection's round's
// (ticket_round.go). Its core and the fields below viewChanged are under
// subMu.
type ticketPump struct {
	core pump.Core[readRequest]
	key  ticketKey
	// number names the pump within the service's session: its row in the
	// mirror's open tickets (keepTicket). kept reports that the row is
	// written, from the keep to the drop; it is under opMu, as is every
	// write of the row.
	number int64
	kept   bool
	// gen is the mirror generation the pump shows (genLocked).
	gen uint64
	// every is the connection's round interval, the answer's PollEvery.
	every  time.Duration
	exited chan struct{}
	// viewChanged nudges the pump to rebuild its view (rebuildTicket): an
	// input of it other than its document moved. One queued nudge is
	// enough; a hidden pump rebuilds when shown again.
	viewChanged chan struct{}

	// check is the document: what the next read checks the ticket against,
	// nil when it decodes the mirror's copy. settled is when the last read
	// settled into the pump began (Service.nextStampLocked), zero before
	// the first; docs counts the documents settled into it.
	check         *detailCheck
	settled, docs uint64
	// due is when the round reads the ticket next. nudged is the stamp of
	// the last request to read it at once (readNowLocked): a read that
	// began before it neither undoes it nor counts as its read. inputs is
	// the stamp of the last change to an input of the view other than its
	// document (ticketInputs).
	due            time.Time
	nudged, inputs uint64
	// reading counts the reads of the ticket begun and not settled
	// (ticketReads, settleTickets): while one is in flight, the document a
	// read stored in the mirror is that read's to settle (rebuildTicket).
	reading int
	// view is the view the subscribers were last told of, and shows are
	// the other issues it shows (showsOf).
	view  toldView
	shows []tracker.IssueID
	// held is the view when the pump, not the mirror, holds the ticket's
	// content (check.held is its document): the mirror does not keep the
	// issue or no longer holds its detail. Which one holds it is decided
	// on every successful read. Nothing else of a ticket is held.
	held *heldDetail
	// failure is the failure the subscribers were last told of; changed and
	// pending are what the next event carries.
	failure          *FailureView
	changed, pending bool
	over             bool
}

// toldView is what a ticket's subscribers were last told of its view:
// whether it is loaded, whether it shows the time in its status, and a
// digest of its content, and what it was built from: the document
// (ticketPump.docs) and the stamp its build began at, which a later change
// of another input outdates.
type toldView struct {
	loaded     bool
	sinceKnown bool
	digest     uint64
	docs, at   uint64
}

// heldDetail is a view a ticket pump holds, with the wall time its read
// began.
type heldDetail struct {
	view DetailView
	wall int64
}

// TicketView is an open ticket as its subscribers were last told of it:
// its detail when the mirror, or the ticket's pump, holds one (Loaded),
// with when its read began (Unix milliseconds by this computer's wall
// clock), and its failure, nil while its reads succeed.
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
	asked := s.stamp()
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
	switch {
	case start:
		tp.view.loaded, tp.view.digest = answer.Loaded, digest
		s.launchTicketLocked(tp)
	case !answer.Loaded:
		s.answeredNotLoadedLocked(tp, asked)
	}
	return answer, nil
}

// stamp is the next stamp of the ticket pumps' order (nextStampLocked).
func (s *Service) stamp() uint64 {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.nextStampLocked()
}

// answeredNotLoadedLocked records that a subscriber of tp was answered,
// from what the pump and the mirror held after stamp asked, that its
// ticket is not loaded (notLoadedLocked), unless a read that began later
// settled since: a document held again is told then, whatever its
// content. The caller holds subMu.
func (s *Service) answeredNotLoadedLocked(tp *ticketPump, asked uint64) {
	if tp.settled < asked {
		s.notLoadedLocked(tp, asked)
	}
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
		s.subs.pumps++
		tp = &ticketPump{
			core: s.newCore(), key: key, number: s.subs.pumps, gen: s.genLocked(key.connection), every: s.cadence.ticket(kind),
			exited: make(chan struct{}), viewChanged: make(chan struct{}, 1),
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
		v.Loaded, v.Detail, v.ReadAt = true, &view, tp.held.wall
	}
	return v
}

// TicketDetail is handle id's ticket as its subscribers were last told of
// it: its failure, and the view its pump holds or else the mirror's copy
// (MirroredDetail), not Loaded while neither holds one, which its
// subscribers are then told when they were told otherwise.
func (s *Service) TicketDetail(id string) (TicketView, error) {
	s.subMu.Lock()
	h := s.subs.handles[id]
	var key ticketKey
	var told TicketView
	if h != nil && h.ticket != nil {
		key, told = h.ticket.key, h.ticket.toldLocked()
	}
	asked := s.nextStampLocked()
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
		s.subMu.Lock()
		s.answeredNotLoadedLocked(h.ticket, asked)
		s.subMu.Unlock()
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
// that records the opening, a full read on a person's request, and a view
// rebuild with no tracker call when an input of the view moved, made when
// shown again if it came while hidden. A failed read is retried by the
// connection's round. The mirror keeps the ticket's issue from before the
// first read until the goroutine returns (keepTicket), which every end of
// the pump comes to: its last handle released, its connection turned off
// or removed, Stop, and a read that found the pump ended.
func (s *Service) runTicketPump(tp *ticketPump) {
	defer s.workers.Done()
	defer close(tp.exited)
	defer s.keepTicket(tp)()
	ctx, conn, self := tp.core.Context(), tp.key.connection, []*ticketPump{tp}
	over := s.readTickets(ctx, conn, self, Interactive, false, true).over
	for !over {
		select {
		case <-tp.core.Done():
			return
		case req := <-tp.core.Requests():
			res := s.readTickets(ctx, conn, self, Interactive, true, true)
			over = res.over
			err := res.errOf(0)
			if over {
				err = ErrSubscriptionEnded
			}
			req.Finish(err)
		case <-tp.core.Wake():
			s.subMu.Lock()
			behind := tp.behindLocked()
			s.subMu.Unlock()
			if behind {
				s.rebuildTicket(tp)
			}
		case <-tp.viewChanged:
			if !tp.core.Paused() {
				s.rebuildTicket(tp)
			}
		}
	}
}

// flushTicketLocked emits tp's pending event, if any, or its end. Every
// event names its ticket and carries its failure as of the event. The
// caller holds subMu.
func (s *Service) flushTicketLocked(tp *ticketPump, end bool) {
	if tp.over || (!tp.pending && !end) || (tp.core.DeadLocked() && !end) {
		return
	}
	ev := IssueEvent{
		Connection: tp.key.connection, Issue: tp.key.issue, Seq: s.nextSeqLocked(),
		Changed: tp.changed, Failure: tp.failure, Ended: end,
	}
	tp.changed, tp.pending = false, false
	s.emitIssue(ev)
}

// tellFailureLocked makes f tp's failure, due to be told when it is not the
// one its subscribers were last told of. The caller holds subMu.
func (tp *ticketPump) tellFailureLocked(f *FailureView) {
	if !sameFailure(tp.failure, f) {
		tp.failure, tp.pending = f, true
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

// keepTicket makes the mirror keep tp's issue (store KeepMirrorTicket) in
// the root of tp's connection, and returns what ends the keep (store
// DropMirrorTicket), which the pump's goroutine runs as it returns, the
// one place every end of the pump comes to. A root a restore emptied
// since the run took it is replaced (rehold) and the keep made under the
// new one; a root made after the keep, within the pump's generation,
// keeps it again (keepTicketsAgainLocked). A pump of a mirror generation
// that ended keeps nothing: it is ending. A keep that fails leaves the
// ticket working as one of an issue the mirror does not keep (its pump
// holds its document), and is logged; so is a drop that fails, whose row
// the next boot drops (StartLinkedSync).
func (s *Service) keepTicket(tp *ticketPump) (drop func()) {
	drop = func() {
		s.opMu.Lock()
		defer s.opMu.Unlock()
		if !tp.kept {
			return
		}
		tp.kept = false
		if err := s.store.DropMirrorTicket(s.session, tp.number); err != nil {
			s.logf("trackerapp: drop the open ticket %s of connection %s: %v; the next boot drops it", tp.key.issue, tp.key.connection, err)
		}
	}
	r, err := s.start(tp.core.Context(), tp.key.connection, Background)
	switch {
	case errors.Is(err, errEnded), errors.Is(err, ErrStopped), errors.Is(err, context.Canceled):
		return drop
	case err != nil:
		s.logf("trackerapp: keep the open ticket %s of connection %s: %v", tp.key.issue, tp.key.connection, err)
		return drop
	}
	defer r.finish()
	if r.gen != tp.gen {
		return drop
	}
	err = s.keepTicketLocking(tp, r.root)
	if errors.Is(err, store.ErrTrackerCacheGone) {
		var root int64
		if root, err = s.rehold(tp.key.connection, r.root, r.gen, err); err == nil {
			err = s.keepTicketLocking(tp, root)
		}
	}
	if err != nil && !errors.Is(err, errEnded) {
		s.logf("trackerapp: keep the open ticket %s of connection %s: %v", tp.key.issue, tp.key.connection, err)
	}
	return drop
}

// keepTicketLocking writes tp's keep under root, taking opMu.
func (s *Service) keepTicketLocking(tp *ticketPump, root int64) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.keepTicketLocked(tp, root)
}

// keepTicketLocked writes tp's keep under root (store KeepMirrorTicket).
// The caller holds opMu.
func (s *Service) keepTicketLocked(tp *ticketPump, root int64) error {
	if err := s.store.KeepMirrorTicket(root, string(tp.key.issue), s.session, tp.number); err != nil {
		return err
	}
	tp.kept = true
	return nil
}

// keepTicketsAgainLocked keeps the open tickets of connection id's
// generation gen that were kept under root, a root made in place of one a
// restore emptied (mirrorRootLocked): the restore emptied their rows with
// the mirror. A keep that fails is logged, as a pump's own is
// (keepTicket). The caller holds opMu.
func (s *Service) keepTicketsAgainLocked(id string, gen uint64, root int64) {
	s.subMu.Lock()
	var kept []*ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == id && tp.gen == gen && tp.kept {
			kept = append(kept, tp)
		}
	}
	s.subMu.Unlock()
	for _, tp := range kept {
		if err := s.keepTicketLocked(tp, root); err != nil {
			s.logf("trackerapp: keep the open ticket %s of connection %s again after its mirror was emptied: %v", tp.key.issue, id, err)
		}
	}
}
