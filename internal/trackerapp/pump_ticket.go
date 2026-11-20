package trackerapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"slices"
	"time"

	"agent-overflow/internal/pump"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// ticketPump is an open ticket's pump: one goroutine and one timer for
// every handle on the issue, plus the retry timer while a read fails. Its
// core and the fields below cardChanged are under subMu.
type ticketPump struct {
	core pump.Core[struct{}]
	key  ticketKey
	// every is the poll interval of the connection's kind.
	every  time.Duration
	exited chan struct{}
	// cardChanged nudges the pump when a read for something else stored a
	// changed card of the issue (funnel.go); one queued nudge is enough.
	cardChanged chan struct{}

	// digest identifies the view the subscribers were last told of (read
	// reports one is set), so a read that finds the same view emits
	// nothing.
	digest uint64
	read   bool
	// held is the last view read when the mirror does not hold its
	// content: an issue the mirror does not keep has no stored detail.
	// Nothing else of a ticket is held.
	held *heldDetail
	// failure is the failure the subscribers were last told of; changed
	// and due are what the next event carries.
	failure      *FailureView
	changed, due bool
	over         bool
}

// heldDetail is a view a ticket pump holds, with when its read began.
type heldDetail struct {
	view   DetailView
	readAt int64
}

// TicketSubscription is a ticket subscribe's answer: the handle, the
// sequence number the answer is as of, and the ticket's detail when the
// mirror, or the ticket's pump, holds one (Loaded), with when its read
// began. With none, the pump's first read follows, and its event says so.
// PollEvery is the pump's interval in milliseconds.
type TicketSubscription struct {
	ID        string      `json:"id"`
	Seq       uint64      `json:"seq"`
	Loaded    bool        `json:"loaded"`
	Detail    *DetailView `json:"detail,omitempty"`
	ReadAt    int64       `json:"read_at,omitempty"`
	PollEvery int64       `json:"poll_every"`
}

// SubscribeTicket opens issue on connection id for a client: an answer at
// once from what the service holds, with no tracker call, then the pump
// made on the issue's first handle reads it as the tracker says it is now
// (IssueDetail), interactive, and every Cadence.Ticket while shown. It is
// refused as SubscribeBoard is.
func (s *Service) SubscribeTicket(id string, issue tracker.IssueID) (TicketSubscription, error) {
	if id == "" || issue == "" {
		return TicketSubscription{}, fmt.Errorf("%w: a ticket subscribe names its connection and issue", ErrInvalidRequest)
	}
	if err := s.enter(); err != nil {
		return TicketSubscription{}, err
	}
	defer s.leave()
	hid, tp, start, seq, held, err := s.takeTicketHandle(ticketKey{connection: id, issue: issue})
	if err != nil {
		return TicketSubscription{}, err
	}
	answer := TicketSubscription{ID: hid, Seq: seq, PollEvery: tp.every.Milliseconds()}
	if held != nil {
		answer.Loaded, answer.Detail, answer.ReadAt = true, &held.view, held.readAt
	} else {
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
				s.launchLocked(&tp.core, tp.exited, func() { s.runTicketPump(tp) })
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
		tp.digest, tp.read = digest, answer.Loaded
		s.launchLocked(&tp.core, tp.exited, func() { s.runTicketPump(tp) })
	}
	return answer, nil
}

// takeTicketHandle takes a handle on key's pump, making the pump when
// there is none; held is a copy of the view it holds.
func (s *Service) takeTicketHandle(key ticketKey) (id string, tp *ticketPump, start bool, seq uint64, held *heldDetail, err error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	kind, err := s.subscribableLocked(key.connection)
	if err != nil {
		return "", nil, false, 0, nil, err
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case s.subs.halted:
		return "", nil, false, 0, nil, ErrStopped
	case s.subs.ticketHandles >= maxTicketSubscriptions:
		return "", nil, false, 0, nil, fmt.Errorf("%w: %d ticket subscriptions", ErrTooManySubscriptions, maxTicketSubscriptions)
	}
	tp = s.subs.tickets[key]
	if tp == nil {
		if s.subs.tickets == nil {
			s.subs.tickets = map[ticketKey]*ticketPump{}
		}
		tp = &ticketPump{core: s.newCore(), key: key, every: s.cadence.ticket(kind), exited: make(chan struct{}), cardChanged: make(chan struct{}, 1)}
		s.subs.tickets[key] = tp
		start = true
	}
	if tp.held != nil {
		copied := *tp.held
		held = &copied
	}
	id = s.takeHandleLocked(&handle{ticket: tp}, &tp.core)
	return id, tp, start, s.nextSeqLocked(), held, nil
}

// TicketDetail is the detail of handle id's ticket as its subscribers were
// last told of it, with when its read began: the view its pump holds, or
// the mirror's copy (MirroredDetail). It fails wrapping ErrNotMirrored
// while neither holds one.
func (s *Service) TicketDetail(id string) (DetailView, int64, error) {
	s.subMu.Lock()
	h := s.subs.handles[id]
	var key ticketKey
	var held *heldDetail
	if h != nil && h.ticket != nil {
		key, held = h.ticket.key, h.ticket.held
	}
	s.subMu.Unlock()
	switch {
	case h == nil || h.ticket == nil:
		return DetailView{}, 0, fmt.Errorf("%w: ticket %s", ErrUnknownSubscription, id)
	case held != nil:
		return held.view, held.readAt, nil
	}
	return s.MirroredDetail(key.connection, key.issue)
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

// runTicketPump is a ticket pump's goroutine: an interactive first read,
// then a read every Cadence.Ticket while a handle shows the ticket, at
// once when another read stored a changed card of the issue, on a
// person's request, and on the retry schedule while reads fail.
func (s *Service) runTicketPump(tp *ticketPump) {
	defer s.workers.Done()
	defer close(tp.exited)
	next := time.NewTimer(tp.every)
	defer next.Stop()
	retry := pump.Retry{Base: s.cadence.RetryBase, Interval: tp.every}
	missed := false
	step := func(u Urgency) (over bool, err error) {
		missed = false
		res := s.ticketTick(tp, u)
		retry.Arm(res.failed, res.retryAt)
		next.Reset(tp.every)
		return res.over, res.err
	}
	over, _ := step(Interactive)
	for !over {
		select {
		case <-tp.core.Done():
			return
		case req := <-tp.core.Requests():
			// TODO(forced-read): a person's refresh reads in full
			var err error
			over, err = step(Interactive)
			if over {
				err = ErrSubscriptionEnded
			}
			req.Finish(err)
		case <-tp.core.Wake():
			if !tp.core.Paused() && missed {
				over, _ = step(Background)
			}
		case <-tp.cardChanged:
			if tp.core.Paused() {
				missed = true
				continue
			}
			over, _ = step(Background)
		case <-retry.C():
			if tp.core.Paused() {
				retry.FiredWhilePaused()
				missed = true
				continue
			}
			over, _ = step(Background)
		case <-next.C:
			if tp.core.Paused() {
				missed = true
				continue
			}
			if retry.Held() {
				continue
			}
			over, _ = step(Background)
		}
	}
}

// ticketTick reads tp's issue through IssueDetail, passes what the read
// stored through the change funnel, and settles the outcome into the
// pump: the view it holds, a change or a failure to tell, and its end when
// the connection was turned off or removed.
func (s *Service) ticketTick(tp *ticketPump, u Urgency) tickResult {
	ctx := tp.core.Context()
	conn, issue := tp.key.connection, tp.key.issue
	readAt := s.now().UnixMilli()
	v, ch, err := s.IssueDetail(ctx, conn, issue, u)
	kind, root := s.heldKindRoot(conn)
	s.publish(root, Scope{Connection: conn}, ch, &tickOrigin{ticket: tp})
	var res tickResult
	var failure *FailureView
	stopped := err != nil && ctx.Err() != nil
	ended := errors.Is(err, errEnded) || errors.Is(err, ErrDisabled) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrStopped)
	var digest uint64
	switch failed := (*callError)(nil); {
	case err == nil:
		digest = s.digestOf(v)
	case stopped, ended:
	case errors.As(err, &failed):
		f := ClassifyFailure(kind, failed.err)
		failure = &FailureView{Kind: string(f.Kind), Message: f.Message, At: readAt}
		res.failed = true
		if limited, ok := errors.AsType[*trackerapi.RateLimitedError](failed.err); ok {
			res.retryAt = limited.Until
		}
	default:
		res.failed, res.err = true, err
		s.logf("trackerapp: the read of open ticket %s of %s failed: %v", issue, conn, err)
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case tp.core.DeadLocked():
		res.over = true
		return res
	case ended:
		s.endTicketLocked(tp)
		res.over = true
		return res
	case stopped:
		return res
	}
	if err == nil {
		if !tp.read || digest != tp.digest {
			tp.digest, tp.read, tp.changed, tp.due = digest, true, true, true
			if !slices.Contains(ch.Details, issue) {
				tp.held = &heldDetail{view: v, readAt: readAt}
			}
		}
		if slices.Contains(ch.Details, issue) {
			tp.held = nil
		}
	}
	if (err == nil || failure != nil) && !sameFailure(tp.failure, failure) {
		tp.failure, tp.due = failure, true
	}
	s.flushTicketLocked(tp, false)
	return res
}

// heldKindRoot is the kind and the mirror root of connection id as the
// service holds them (Service.holds); empty and zero when it holds none.
func (s *Service) heldKindRoot(id string) (string, int64) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if h := s.holds[id]; h != nil {
		return h.row.Kind, h.root
	}
	return "", 0
}

// flushTicketLocked emits tp's pending event, if any, or its end. The
// caller holds subMu.
func (s *Service) flushTicketLocked(tp *ticketPump, end bool) {
	if tp.over || (!tp.due && !end) || (tp.core.DeadLocked() && !end) {
		return
	}
	ev := IssueEvent{Connection: tp.key.connection, Seq: s.nextSeqLocked(), Failure: tp.failure, Ended: end}
	if tp.changed {
		ev.Issues = []tracker.IssueID{tp.key.issue}
	}
	tp.changed, tp.due = false, false
	s.emitIssue(ev)
}

// endTicketLocked ends tp: it stops, it and its handles are forgotten, and
// its subscribers are told once. The caller holds subMu.
func (s *Service) endTicketLocked(tp *ticketPump) {
	if tp.over {
		return
	}
	tp.core.StopLocked()
	if s.subs.tickets[tp.key] == tp {
		delete(s.subs.tickets, tp.key)
	}
	s.endHandlesLocked(&tp.core)
	s.flushTicketLocked(tp, true)
	tp.over, tp.held = true, nil
}
