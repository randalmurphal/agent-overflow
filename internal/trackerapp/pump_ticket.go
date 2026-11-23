package trackerapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"slices"
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
	// mirror's open tickets, the ticket's one record of its lifetime.
	// bound reports that the row is written under root (bindTickets,
	// keepTicketsAgainLocked); dropped that the pump ended and deleted it
	// (dropTicket), after which nothing binds it again. They are under
	// opMu, as is every write of the row.
	number  int64
	bound   bool
	dropped bool
	root    int64
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
	// issue or no longer holds its body. Which one holds it is decided on
	// every successful read. Nothing else of a ticket is held.
	held *heldDetail
	// failure is the failure the subscribers were last told of; changed and
	// pending are what the next event carries.
	failure          *FailureView
	changed, pending bool
	over             bool
}

// toldView is what a ticket's subscribers were last told of its view:
// whether it is loaded and a digest of its content, and what it was built
// from: the document (ticketPump.docs) and the stamp its build began at,
// which a later change of another input outdates.
type toldView struct {
	loaded   bool
	digest   uint64
	docs, at uint64
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
// connection's round. The mirror keeps the ticket's issue from the bind
// its first read makes (bindTickets) until the goroutine returns
// (dropTicket), which every end of the pump comes to: its last handle
// released, its connection turned off or removed, Stop, and a read that
// found the pump ended.
func (s *Service) runTicketPump(tp *ticketPump) {
	defer s.workers.Done()
	defer close(tp.exited)
	defer s.dropTicket(tp)
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

// bindTickets binds the open tickets of reads that the mirror does not
// keep yet, those of the run's mirror generation that are live, under the
// run's root (store KeepMirrorTicket), bound at one read begun before the
// read of the tickets begins: the bind admits a body from any read that
// begins after it (store StoreMirrorTickets). A bind that fails is its
// ticket's failure, answered without a read of it, which the retry
// schedule reads, and binds, again. A pump whose bind failed shows its
// failure until a bind succeeds; so a ticket whose content the mirror does
// not keep is one of an issue it does not keep, never one it failed to. A
// bind that shows a held record again (a ticket closed and opened again
// before the cleanup) is published as its keep commits
// (Changes.Visibility), before the read of the tickets begins.
func (r *run) bindTickets(reads []*ticketRead) error {
	s := r.s
	unbound := func(rd *ticketRead) bool {
		return rd.tp != nil && !rd.tp.bound && !rd.tp.dropped && rd.tp.gen == r.gen
	}
	s.opMu.Lock()
	any := slices.ContainsFunc(reads, unbound)
	s.opMu.Unlock()
	if !any {
		return nil
	}
	opened, err := r.beginRead()
	if err != nil {
		return err
	}
	var shown []tracker.IssueID
	s.opMu.Lock()
	for _, rd := range reads {
		if !unbound(rd) {
			continue
		}
		again, err := s.keepTicketLocked(rd.tp, r.root, opened)
		switch {
		case err != nil:
			rd.err, rd.answered = fmt.Errorf("trackerapp: keep the open ticket %s of connection %s: %w", rd.issue, r.row.ID, err), true
		case again:
			shown = append(shown, rd.issue)
		}
	}
	s.opMu.Unlock()
	if len(shown) > 0 {
		slices.Sort(shown)
		s.publish(r.root, r.gen, r.row.ID, Changes{Visibility: shown}, nil, nil)
	}
	return nil
}

// keepTicketLocked writes tp's keep under root, bound at opened (store
// KeepMirrorTicket), and reports whether it showed the issue's held record
// again. The caller holds opMu.
func (s *Service) keepTicketLocked(tp *ticketPump, root int64, opened store.MirrorRead) (bool, error) {
	shown, err := s.store.KeepMirrorTicket(root, string(tp.key.issue), s.session, tp.number, opened)
	if err != nil {
		return false, err
	}
	tp.bound, tp.root = true, root
	return shown, nil
}

// owedDrop is the keep of an ended pump whose drop has not committed: the
// store keeps the issue until it does.
type owedDrop struct {
	key    ticketKey
	number int64
	root   int64
	gen    uint64
}

// dropTicket ends tp's keep as its goroutine returns, and no bind follows
// it (dropKeep). A drop that fails stays owed: the next run retries it
// (payOwedDrops), and Stop makes a last try, after which the next process
// drops the row (dropEarlierTickets).
func (s *Service) dropTicket(tp *ticketPump) {
	s.opMu.Lock()
	tp.dropped = true
	bound := tp.bound
	tp.bound = false
	s.opMu.Unlock()
	if bound {
		s.dropKeep(owedDrop{key: tp.key, number: tp.number, root: tp.root, gen: tp.gen}, "the next run tries it again")
	}
}

// dropKeep deletes d's keep (store DropMirrorTicket) under opMu, the lock
// of every write of the row. When the issue is kept no longer, the change
// reaches every subscription that shows it as another's parent or link
// (Changes.Visibility). A drop that fails is logged with retry, what
// follows it, and owed until a later try commits.
func (s *Service) dropKeep(d owedDrop, retry string) {
	s.opMu.Lock()
	unkept, err := s.store.DropMirrorTicket(s.session, d.number)
	if err != nil {
		s.owedDrops = append(s.owedDrops, d)
	}
	s.opMu.Unlock()
	switch {
	case err != nil:
		s.logf("trackerapp: drop the open ticket %s of connection %s: %v; %s", d.key.issue, d.key.connection, err, retry)
	case unkept:
		s.publish(d.root, d.gen, d.key.connection, Changes{Visibility: []tracker.IssueID{d.key.issue}}, nil, nil)
	}
}

// payOwedDrops tries again every drop owed (dropKeep), retry naming what
// follows one that fails again.
func (s *Service) payOwedDrops(retry string) {
	s.opMu.Lock()
	owed := s.owedDrops
	s.owedDrops = nil
	s.opMu.Unlock()
	for _, d := range owed {
		s.dropKeep(d, retry)
	}
}

// keepTicketsAgainLocked binds again, under root, every live open ticket
// of connection id's generation gen, root being a root made in place of
// one a restore emptied (mirrorRootLocked): the restore emptied their rows
// with the mirror, so none is bound any more, and a ticket whose earlier
// bind failed is bound with the others. One read begun now binds them all.
// A bind that fails leaves its ticket unbound, which its next read binds
// (bindTickets), and is logged. The caller holds opMu.
func (s *Service) keepTicketsAgainLocked(id string, gen uint64, root int64) {
	s.subMu.Lock()
	var live []*ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == id && tp.gen == gen && !tp.over {
			live = append(live, tp)
		}
	}
	s.subMu.Unlock()
	live = slices.DeleteFunc(live, func(tp *ticketPump) bool { return tp.dropped })
	for _, tp := range live {
		tp.bound = false
	}
	if len(live) == 0 {
		return
	}
	opened, err := s.store.BeginMirrorRead(root, millis(s.now()))
	if err != nil {
		s.logf("trackerapp: keep the %d open tickets of connection %s again after its mirror was emptied: %v; each read binds its own", len(live), id, err)
		return
	}
	for _, tp := range live {
		// A root made now holds no record, so no keep shows one again.
		if _, err := s.keepTicketLocked(tp, root, opened); err != nil {
			s.logf("trackerapp: keep the open ticket %s of connection %s again after its mirror was emptied: %v; its next read binds it", tp.key.issue, id, err)
		}
	}
}
