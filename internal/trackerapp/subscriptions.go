package trackerapp

import (
	"crypto/rand"
	"errors"
	"fmt"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/tracker"
)

// Subscriptions. A client subscribes to a board or a ticket and gets a
// handle by ID: an answer from the mirror at once, then events. One pump
// per scope (a board, or an issue of a connection) serves every handle on
// it: made on the first handle, stopped on the last, paused while every
// handle is hidden (internal/pump). Its tick is the scope's sync (tick.go)
// or the ticket's verified read; what a tick stores passes through the
// change funnel (funnel.go), which turns it into events for every
// subscription it concerns. Events are nudges with bounded payloads: the
// client pulls content (Board, BoardPatch, SectionRows, TicketDetail).
//
// Sequence: one counter for the service, advanced under subMu. A
// subscribe takes its answer's number when it takes its handle, before it
// reads the mirror; an event takes its number after the write it reports
// committed and is emitted under the same hold of subMu. So a change the
// answer does not show has an event numbered above the answer, an event
// numbered at or below it reports a change the answer shows (its patch
// re-reads the current state, which is harmless), and one subscription's
// events arrive in number order.

// Bounds of the subscriptions.
const (
	// maxBoardSubscriptions and maxTicketSubscriptions bound the handles
	// held at once, and so the pumps and what they retain.
	maxBoardSubscriptions  = 64
	maxTicketSubscriptions = 64
	// maxEventIssues bounds the issue IDs an event names and a patch reads;
	// an event over it names none and tells the client to read again.
	maxEventIssues = 100
)

var (
	// ErrUnknownSubscription is a handle ID the service does not hold:
	// never made, released, or ended.
	ErrUnknownSubscription = errors.New("trackerapp: no such tracker subscription")
	// ErrSubscriptionEnded is a request to a subscription that ended before
	// it was served: its connection was turned off or removed, its board
	// is no longer listed, or its last handle was released.
	ErrSubscriptionEnded = errors.New("trackerapp: the tracker subscription ended")
	// ErrTooManySubscriptions is a subscribe over the handle cap.
	ErrTooManySubscriptions = errors.New("trackerapp: too many tracker subscriptions")
	// ErrNotListed is a board subscribe naming a board the project does not
	// list.
	ErrNotListed = errors.New("trackerapp: the project does not list the board")
)

// BoardEvent tells a board's subscribers that the mirror changed. Changed
// and Removed name the board's rows a write changed or removed (a patch
// reads them, BoardPatch); ReadAgain says the change moved rows it does
// not name (the board's structure, a status's name or group) or named more
// than maxEventIssues (Truncated), and the client reads the board view
// again. Sync is the board's sync state; Failure in it is sent once when
// it starts or changes and once when it clears. Ended is the last event:
// the subscription ended (the connection was turned off or removed, or the
// board is no longer listed), and every handle on it with it.
type BoardEvent struct {
	Scope     Scope             `json:"scope"`
	Seq       uint64            `json:"seq"`
	Changed   []tracker.IssueID `json:"changed,omitempty"`
	Removed   []tracker.IssueID `json:"removed,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	ReadAgain bool              `json:"read_again,omitempty"`
	Sync      SyncView          `json:"sync"`
	Ended     bool              `json:"ended,omitempty"`
}

// IssueEvent tells an open ticket's subscribers, of Issue on Connection,
// that its view changed (Changed; TicketDetail reads it), that its reads
// failed or recovered, or that the subscription ended. Failure is the
// ticket's failure as of the event, nil while its reads succeed: an event
// is sent when it starts, changes or clears. Ended is the last event: the
// connection was turned off or removed, and every handle on the ticket
// ended with it.
type IssueEvent struct {
	Connection string          `json:"connection"`
	Issue      tracker.IssueID `json:"issue"`
	Seq        uint64          `json:"seq"`
	Changed    bool            `json:"changed,omitempty"`
	Failure    *FailureView    `json:"failure,omitempty"`
	Ended      bool            `json:"ended,omitempty"`
}

// subscriptions is the registry of handles and pumps, under subMu. The
// maps are made on the first subscribe.
type subscriptions struct {
	handles map[string]*handle
	boards  map[Scope]*boardPump
	tickets map[ticketKey]*ticketPump
	// boardHandles and ticketHandles count the handles of each kind.
	boardHandles, ticketHandles int
	seq                         uint64
	// halted is set by Stop: no pump or loop starts after it.
	halted bool
	// loop is the linked loop (linked_loop.go).
	loop loopState
	// rounds are the connections' ticket rounds (ticket_round.go), and
	// stamps the counter that orders what settles into ticket pumps
	// (nextStampLocked).
	rounds map[string]*ticketRound
	stamps uint64
	// gens is each connection's mirror generation (genLocked).
	gens map[string]uint64
	// pumps counts the ticket pumps made, numbering each
	// (ticketPump.number).
	pumps int64
}

// genLocked is connection conn's mirror generation: it moves when the
// connection's subscriptions end (turned off or removed), which is also
// when its mirror is detached, so a run and a pump of one generation
// store into and show one mirror. A run reads it with its mirror root
// (Service.hold), a pump when it is made, and the funnel delivers a run's
// changes only to the pumps of its generation. The caller holds subMu.
func (s *Service) genLocked(conn string) uint64 {
	return s.subs.gens[conn]
}

// handle is one subscriber's reference on a pump.
type handle struct {
	board  *boardPump
	ticket *ticketPump
	ref    *pump.Handle[readRequest]
}

// ticketKey names an open ticket's pump.
type ticketKey struct {
	connection string
	issue      tracker.IssueID
}

// nextSeqLocked is the next sequence number. The caller holds subMu.
func (s *Service) nextSeqLocked() uint64 {
	s.subs.seq++
	return s.subs.seq
}

// newCore is a pump core under the service's lifetime: a request it
// cannot serve fails with ErrSubscriptionEnded, or ErrStopped once the
// service's lifetime ended.
func (s *Service) newCore() pump.Core[readRequest] {
	return pump.NewCore[readRequest](s.life, pump.Refusals{Stopped: ErrSubscriptionEnded, LifetimeEnded: ErrStopped})
}

// takeHandleLocked registers a handle on core and returns its ID. The
// caller holds subMu and has checked the cap.
func (s *Service) takeHandleLocked(h *handle, core *pump.Core[readRequest]) string {
	if s.subs.handles == nil {
		s.subs.handles = map[string]*handle{}
	}
	h.ref = core.TakeLocked()
	id := rand.Text()
	s.subs.handles[id] = h
	if h.board != nil {
		s.subs.boardHandles++
	} else {
		s.subs.ticketHandles++
	}
	return id
}

// forgetHandleLocked drops handle id from the registry. The caller holds
// subMu.
func (s *Service) forgetHandleLocked(id string, h *handle) {
	delete(s.subs.handles, id)
	if h.board != nil {
		s.subs.boardHandles--
	} else {
		s.subs.ticketHandles--
	}
}

// SetSubscriptionVisible records whether the client of handle id shows
// what it subscribed to. A pump pauses while every handle on it is
// hidden: it makes no request and no store call, and catches up with one
// tick when a handle shows it again after it missed one.
func (s *Service) SetSubscriptionVisible(id string, visible bool) error {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	h := s.subs.handles[id]
	if h == nil {
		return fmt.Errorf("%w: %s", ErrUnknownSubscription, id)
	}
	h.ref.VoteLocked(visible)
	if h.ticket != nil {
		s.followRoundLocked(h.ticket.key.connection)
	}
	return nil
}

// ReleaseSubscription drops handle id; the last handle on a pump stops
// it. Releasing a handle the service does not hold does nothing, so a
// release and the client's disconnect may both run.
func (s *Service) ReleaseSubscription(id string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	h := s.subs.handles[id]
	if h == nil {
		return
	}
	s.forgetHandleLocked(id, h)
	last := h.ref.ReleaseLocked()
	switch {
	case last && h.board != nil && s.subs.boards[h.board.scope] == h.board:
		delete(s.subs.boards, h.board.scope)
	case last && h.ticket != nil && s.subs.tickets[h.ticket.key] == h.ticket:
		delete(s.subs.tickets, h.ticket.key)
	}
	if h.ticket != nil {
		// A released handle that showed the ticket may leave none shown.
		s.followRoundLocked(h.ticket.key.connection)
	}
}

// coreOfLocked is the pump core handle id is on. The caller holds subMu.
func (h *handle) core() *pump.Core[readRequest] {
	if h.board != nil {
		return &h.board.core
	}
	return &h.ticket.core
}

// endHandlesLocked forgets every handle on core: its pump ended. The
// caller holds subMu.
func (s *Service) endHandlesLocked(core *pump.Core[readRequest]) {
	for id, h := range s.subs.handles {
		if h.core() == core {
			s.forgetHandleLocked(id, h)
		}
	}
}

// launchLocked starts a pump's goroutine unless the pump already stopped
// (Stop came first, its subscribe failed, or its connection was turned
// off meanwhile), in which case exited closes here, saving a goroutine
// that would exit at once: the goroutine closes it otherwise. The caller
// holds subMu, inside the subscribe's call, so Stop joins what it starts.
func (s *Service) launchLocked(core *pump.Core[readRequest], exited chan struct{}, run func()) {
	if core.DeadLocked() {
		close(exited)
		return
	}
	s.workers.Add(1)
	go run()
}

// haltSubscriptions stops every pump and the linked loop, with no event,
// and refuses any later one; Stop then joins them.
func (s *Service) haltSubscriptions() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.subs.halted = true
	for _, p := range s.subs.boards {
		p.core.StopLocked()
	}
	for _, p := range s.subs.tickets {
		p.core.StopLocked()
	}
	for conn := range s.subs.rounds {
		s.stopRoundLocked(conn)
	}
	clear(s.subs.handles)
	clear(s.subs.boards)
	clear(s.subs.tickets)
	s.subs.boardHandles, s.subs.ticketHandles = 0, 0
	s.stopLoopLocked()
}

// endConnectionLocked ends every pump of connection id and its ticket
// round, telling each pump's subscribers once, and returns the channels
// that close when their goroutines have exited. The caller holds opMu and
// subMu.
func (s *Service) endConnectionLocked(id string) []<-chan struct{} {
	if s.subs.gens == nil {
		s.subs.gens = map[string]uint64{}
	}
	s.subs.gens[id]++
	var exited []<-chan struct{}
	if done := s.stopRoundLocked(id); done != nil {
		exited = append(exited, done)
	}
	for _, p := range s.subs.boards {
		if p.scope.Connection == id {
			s.endBoardLocked(p)
			exited = append(exited, p.exited)
		}
	}
	for _, p := range s.subs.tickets {
		if p.key.connection == id {
			s.endTicketLocked(p)
			exited = append(exited, p.exited)
		}
	}
	return exited
}

// join waits for each channel of exited to close.
func join(exited []<-chan struct{}) {
	for _, c := range exited {
		<-c
	}
}
