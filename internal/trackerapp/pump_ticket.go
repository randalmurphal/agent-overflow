package trackerapp

import (
	"encoding/json"
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
// opening's first read and a refresh) and builds the views of its
// connection's tickets when an input of one moved (ticket_view.go); every
// other read of the ticket is its connection's round's (ticket_round.go).
// Its core and the fields below viewChanged are under subMu.
type ticketPump struct {
	core pump.Core[readRequest]
	key  ticketKey
	// number names the pump within the service's session: its row in the
	// mirror's open tickets, which keeps its issue. kept reports that the
	// row committed under root (keepTicketLocked), and dropped that the
	// pump ended and deleted it (dropTicket), after which nothing keeps it
	// again. They are under opMu, as is every write of the row.
	number  int64
	kept    bool
	dropped bool
	root    int64
	// bind is the pump's bind while it is the issue's current one (store
	// ActivateMirrorBind), made only after the row committed and while the
	// pump is live, and zero from its retirement (retireTicketLocked): a
	// read of the ticket carries it as the read begins (boundDetails). It
	// is under subMu.
	bind store.MirrorBind
	// gen is the mirror generation the pump shows (genLocked).
	gen uint64
	// every is the connection's round interval, the answer's PollEvery.
	every  time.Duration
	exited chan struct{}
	// viewChanged nudges the pump to build the views of its connection's
	// tickets that owe one (buildShown). One queued nudge is enough; a
	// hidden pump builds when shown again.
	viewChanged chan struct{}

	// dirty counts the stored changes to an input of the view
	// (tellTicketLocked), built is the count the view was built after, and
	// building counts the builds of it in flight (buildTickets). A pump is
	// made with dirty one: its first paint owes a build.
	dirty, built uint64
	building     int
	// reading counts the reads of the ticket begun and not settled
	// (ticketReads, settleTickets): the read builds the view as it settles,
	// so a nudge leaves the pump to it.
	reading int
	// settled is when the last read settled into the pump began
	// (nextStampLocked), zero before the first. due is when the round reads
	// the ticket next. nudged is the stamp of the last request to read it at
	// once (readNowLocked): a read that began before it neither undoes it nor
	// counts as its read. owedFor is the card version a view behind its card
	// last made the ticket due at once for (installViewLocked).
	settled, nudged uint64
	due             time.Time
	owedFor         int64
	// view is the view the subscribers were last told of.
	view toldView
	// failure is the failure the subscribers were last told of; changed and
	// pending are what the next event carries.
	failure          *FailureView
	changed, pending bool
	over             bool
}

// toldView is what a ticket's subscribers were last told of its view: the
// view (nil: not loaded), a digest of it, the other issues it shows
// (showsOf), and the bookkeeping of the record's body it was built from,
// whose read time a read that found the same body moves (noteBodyLocked).
type toldView struct {
	detail *DetailView
	digest uint64
	shows  []tracker.IssueID
	body   store.MirrorBodyState
}

// TicketView is an open ticket as its subscribers were last told of it:
// its detail when the mirror holds one (Loaded), with when the read of its
// body began (Unix milliseconds by this computer's wall clock), and its
// failure, nil while its reads succeed.
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
// once from the mirror, with no tracker call (the first handle's pump
// builds its first paint from the record; a later handle is answered what
// the subscribers were last told), then the pump made on the issue's first
// handle reads it as the tracker says it is now, interactive, and the
// connection's round reads it every Cadence.Ticket while shown. It is
// refused, and tied by tie, as SubscribeBoard is.
func (s *Service) SubscribeTicket(id string, issue tracker.IssueID, tie Tie) (TicketSubscription, error) {
	if id == "" || issue == "" || tie == nil {
		return TicketSubscription{}, fmt.Errorf("%w: a ticket subscribe names its connection and issue and ties its handle to its client", ErrInvalidRequest)
	}
	if err := s.enter(); err != nil {
		return TicketSubscription{}, err
	}
	defer s.leave()
	hid, tp, start, seq, told, err := s.takeTicketHandle(ticketKey{connection: id, issue: issue}, tie)
	if err != nil {
		return TicketSubscription{}, err
	}
	answer := TicketSubscription{ID: hid, Seq: seq, PollEvery: tp.every.Milliseconds(), TicketView: told}
	if !start {
		return answer, nil
	}
	if err := s.buildTickets(id, 0, []*ticketPump{tp}, false); err != nil {
		// Another subscriber may have joined the pump meanwhile: it runs
		// unless this was its last handle, and its goroutine runs either way,
		// whose exit drops any row a read kept meanwhile.
		s.ReleaseSubscription(hid)
		s.subMu.Lock()
		s.launchTicketLocked(tp)
		s.subMu.Unlock()
		return TicketSubscription{}, err
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.launchTicketLocked(tp)
	// Taken after the paint was installed, under the same hold: an event the
	// install sent is numbered at or below the answer, which shows it.
	answer.Seq, answer.TicketView = s.nextSeqLocked(), tp.toldLocked()
	return answer, nil
}

// launchTicketLocked starts tp's goroutine and follows its connection's
// round. The goroutine runs for a pump stopped before its launch too (Stop
// came first, its subscribe failed, or its connection was turned off
// meanwhile): a read may have kept its issue meanwhile (a write's refresh
// during the first paint), and the goroutine's exit drops the row
// (dropTicket). The caller holds subMu, inside the subscribe's call, so
// Stop joins it.
func (s *Service) launchTicketLocked(tp *ticketPump) {
	s.workers.Add(1)
	go s.runTicketPump(tp)
	s.followRoundLocked(tp.key.connection)
}

// takeTicketHandle takes a handle on key's pump, tied by tie, making the
// pump when there is none; told is the ticket as its subscribers were last told of
// it, as of seq.
func (s *Service) takeTicketHandle(key ticketKey, tie Tie) (id string, tp *ticketPump, start bool, seq uint64, told TicketView, err error) {
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
	id = newHandleID()
	untie, err := tie(id)
	if err != nil {
		return "", nil, false, 0, TicketView{}, err
	}
	tp = s.subs.tickets[key]
	if tp == nil {
		if s.subs.tickets == nil {
			s.subs.tickets = map[ticketKey]*ticketPump{}
		}
		s.subs.pumps++
		tp = &ticketPump{
			core: s.newCore(), key: key, number: s.subs.pumps, gen: s.genLocked(key.connection), every: s.cadence.ticket(kind),
			exited: make(chan struct{}), viewChanged: make(chan struct{}, 1), dirty: 1,
		}
		s.subs.tickets[key] = tp
		start = true
	}
	s.takeHandleLocked(id, &handle{ticket: tp, untie: untie}, &tp.core)
	if !start {
		// A shown handle on a hidden pump shows it: its connection's round
		// follows.
		s.followRoundLocked(key.connection)
	}
	return id, tp, start, s.nextSeqLocked(), tp.toldLocked(), nil
}

// toldLocked is tp's ticket as its subscribers were last told of it. The
// caller holds subMu.
func (tp *ticketPump) toldLocked() TicketView {
	v := TicketView{Failure: tp.failure}
	if d := tp.view.detail; d != nil {
		view := *d
		v.Loaded, v.Detail, v.ReadAt = true, &view, tp.view.body.Wall
	}
	return v
}

// TicketDetail is handle id's ticket as its subscribers were last told of
// it: its failure, and its view or not loaded. It reads nothing.
func (s *Service) TicketDetail(id string) (TicketView, error) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	h := s.subs.handles[id]
	if h == nil || h.ticket == nil {
		return TicketView{}, fmt.Errorf("%w: ticket %s", ErrUnknownSubscription, id)
	}
	return h.ticket.toldLocked(), nil
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
// that records the opening, a full read on a person's request, and the
// builds of its connection's views when an input of one moved, made when
// shown again if it came while hidden. A failed read is retried by the
// connection's round. The mirror keeps the ticket's issue from the bind
// its first read makes (bindTickets) until the goroutine returns
// (dropTicket), which every end of the pump comes to: its last handle
// released, its connection turned off or removed, Stop, and a read that
// found the pump ended; for a pump stopped before its launch, its first
// read ends at once with its context.
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
			s.buildShown(conn)
		case <-tp.viewChanged:
			if !tp.core.Paused() {
				s.buildShown(conn)
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

// endTicketLocked ends tp: its subscribers are told once, it retires
// (retireTicketLocked), its handles are forgotten, and its connection's
// round follows. The caller holds subMu.
func (s *Service) endTicketLocked(tp *ticketPump) {
	if tp.over {
		return
	}
	// The end is told before the core stops, as a board's is
	// (endBoardLocked).
	s.flushTicketLocked(tp, true)
	tp.over = true
	s.retireTicketLocked(tp)
	s.endHandlesLocked(&tp.core)
	s.followRoundLocked(tp.key.connection)
}

// retireTicketLocked is the one end of tp as its issue's open ticket, by
// its identity, and tells no one: its core stops, it leaves the open
// tickets unless another pump of its issue replaced it there, and its bind
// ends (store EndMirrorBind), so no ticket write whose admission follows
// stores a body or an opening of it; a write admitted before commits. Its
// last release, endTicketLocked (its connection turned off or removed, its
// generation ended, a read that found it ended) and Stop come here; the
// app's exit is Stop. Its row is its goroutine's to drop (dropTicket). The
// caller holds subMu.
func (s *Service) retireTicketLocked(tp *ticketPump) {
	tp.core.StopLocked()
	if s.subs.tickets[tp.key] == tp {
		delete(s.subs.tickets, tp.key)
	}
	s.store.EndMirrorBind(tp.bind)
	tp.bind = store.MirrorBind{}
}

// ticketLiveLocked reports that tp is its issue's open ticket, live, of
// connection generation gen. The caller holds subMu.
func (s *Service) ticketLiveLocked(tp *ticketPump, gen uint64) bool {
	return s.subs.tickets[tp.key] == tp && !tp.over && !tp.core.DeadLocked() && tp.gen == gen && s.genLocked(tp.key.connection) == gen
}

// bindTickets binds the open tickets of reads that the mirror does not
// keep yet, those of the run's mirror generation that are live, under the
// run's root (keepTicketLocked), kept at one read begun before the read of
// the tickets begins: the bind admits a body from any read that begins
// after it (store StoreMirrorTickets). A bind that fails is its
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
		return rd.tp != nil && !rd.tp.kept && !rd.tp.dropped && rd.tp.gen == r.gen
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
		again, err := s.keepTicketLocked(rd.tp, r.root, r.gen, opened)
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

// keepTicketLocked writes tp's keep under root, kept at opened (store
// KeepMirrorTicket), and reports whether it showed the issue's held record
// again, only while tp is live, of generation gen, both as the keep starts
// and once it committed: a pump retired before makes no row, and one
// retired during the keep shows nothing, its bind ends unactivated and its
// goroutine drops the row. Once the row committed, the row is the pump's
// to drop, and the bind the keep issued becomes the issue's current one
// (store ActivateMirrorBind), ending the pump's earlier one.
// The caller holds opMu, and the keep's SQL runs without subMu held.
func (s *Service) keepTicketLocked(tp *ticketPump, root int64, gen uint64, opened store.MirrorRead) (bool, error) {
	s.subMu.Lock()
	live := s.ticketLiveLocked(tp, gen)
	s.subMu.Unlock()
	if !live {
		return false, nil
	}
	bind, shown, err := s.store.KeepMirrorTicket(root, string(tp.key.issue), s.session, tp.number, opened)
	if err != nil {
		return false, err
	}
	tp.kept, tp.root = true, root
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if !s.ticketLiveLocked(tp, gen) {
		s.store.EndMirrorBind(bind)
		return false, nil
	}
	// A bind of the root a restore emptied ends as this one replaces it.
	s.store.EndMirrorBind(tp.bind)
	tp.bind = store.MirrorBind{}
	if !s.store.ActivateMirrorBind(bind) {
		return false, fmt.Errorf("trackerapp: the store refused to activate the bind its keep of %s just issued", tp.key.issue)
	}
	tp.bind = bind
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
	kept := tp.kept
	tp.kept = false
	s.opMu.Unlock()
	if kept {
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
// with the mirror, so none is kept any more, and a ticket whose earlier
// bind failed is kept with the others. One read begun now keeps them all,
// and each new bind replaces the pump's bind of the emptied root only for
// a pump still live once its keep committed (keepTicketLocked); a pump
// whose keep fails carries no read under the old one, as its next read
// keeps it first (bindTickets). A bind that fails leaves its ticket
// unbound, which its next read binds (bindTickets), and is logged. The
// caller holds opMu.
func (s *Service) keepTicketsAgainLocked(id string, gen uint64, root int64) {
	s.subMu.Lock()
	var live []*ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == id && s.ticketLiveLocked(tp, gen) {
			live = append(live, tp)
		}
	}
	s.subMu.Unlock()
	live = slices.DeleteFunc(live, func(tp *ticketPump) bool { return tp.dropped })
	for _, tp := range live {
		tp.kept = false
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
		if _, err := s.keepTicketLocked(tp, root, gen, opened); err != nil {
			s.logf("trackerapp: keep the open ticket %s of connection %s again after its mirror was emptied: %v; its next read binds it", tp.key.issue, id, err)
		}
	}
}
