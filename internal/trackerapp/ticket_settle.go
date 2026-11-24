package trackerapp

import (
	"context"
	"errors"
	"slices"
	"time"
)

// What a read of open tickets settles into their pumps: each ticket's
// outcome (success, failure, rate limit, not found), ordered by when the
// reads began. What the read stored reaches the pumps through the change
// funnel as any write does, and the read builds the views of its tickets
// from the record as it settles them (buildTickets), so one event carries
// a read's outcome together with the view it caused.

// ticketsResult is how a read of tickets ended: failed for the read as a
// whole, errs by pump (errOf), over when every pump read ended.
type ticketsResult struct {
	failed bool
	// historyDue reports that the read found an issue of the connection
	// due a history read (detailsRead.historyDue); historyWanted that a
	// round's read wants the history unit run (roundRead).
	historyDue, historyWanted bool
	// unsettled are the issues of the tickets the read answered whose
	// records' status times no final read decided (ticketRead.unsettled),
	// in the read's order: the history read takes them first.
	unsettled []string
	// errs holds, at a pump's index, what the read answers the person who
	// asked for that ticket (askerErr); nil when no ticket has one.
	errs []error
	over bool
}

// errOf is what the read answers the person who asked for the ticket of
// the read's pump i.
func (res ticketsResult) errOf(i int) error {
	if res.errs == nil {
		return nil
	}
	return res.errs[i]
}

// askerErr is what a read that ended with readErr (detailsRead's error;
// failure for the tracker's) answers the person who asked for rd's
// ticket: why the read did not settle it with an answer of the tracker's.
// A ticket the tracker answered settles with its own outcome: nil, or the
// failure to build its view; a ticket the tracker answered gone is nil,
// told to its subscribers as not found. For a ticket the read did not
// answer, the tracker's failure is nil too, told to the subscribers alone
// as a board refresh's is; a failure outside the tracker, or a stop by the
// read's context, is that error; and a read that failed for none made no
// answer of it (errUnanswered).
func askerErr(rd *ticketRead, failure *Failure, readErr error) error {
	switch {
	case rd.answered:
		if rd.failed() {
			return rd.err
		}
		return nil
	case failure != nil:
		return nil
	case readErr != nil:
		return readErr
	}
	return errUnanswered
}

// readTickets reads the tickets of pumps, all of connection conn, together
// (readDetails), each checked against the view its pump shows and read in
// full when full is set (ticketReads), and settles each pump with its own
// outcome (settleTickets). opened records a person's opening.
func (s *Service) readTickets(ctx context.Context, conn string, pumps []*ticketPump, u Urgency, full, opened bool) ticketsResult {
	began, reads := s.ticketReads(pumps, full)
	at := millis(s.now())
	res, err := s.readDetails(ctx, conn, reads, u, opened)
	return s.settleTickets(ctx, conn, pumps, began, reads, res, err, at)
}

// ticketReads are the reads of pumps' tickets, each knowing the body its
// pump's view shows (ticketRead.prior) and read in full when full is set,
// and the stamp the read began at. Each is in flight (ticketPump.reading)
// until settleTickets settles it.
func (s *Service) ticketReads(pumps []*ticketPump, full bool) (began uint64, reads []*ticketRead) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	began = s.nextStampLocked()
	reads = make([]*ticketRead, len(pumps))
	for i, tp := range pumps {
		tp.reading++
		reads[i] = &ticketRead{issue: tp.key.issue, tp: tp, full: full}
		if v := tp.view.detail; v != nil {
			reads[i].prior, reads[i].behind = tp.view.body, v.BodyBehind
		}
	}
	return began, reads
}

// settleTickets settles reads, what a read of pumps' tickets on connection
// conn that began at began found, into each pump, then builds the views of
// those whose record changed since their views were built and sends each
// pump's event (buildTickets): res and err are how the read ended
// (readDetails), ctx the context the read ran under, and at when it ended.
// Each ticket the read answered settles with its own outcome; the read's
// failure is told to each ticket it did not answer and makes it due on the
// retry schedule. A read stopped by its context settles only the tickets
// it answered; a connection that ended ends every pump. Each pump's asker
// is answered its own ticket's outcome (ticketsResult.errOf).
func (s *Service) settleTickets(ctx context.Context, conn string, pumps []*ticketPump, began uint64, reads []*ticketRead, res detailsRead, err error, at int64) ticketsResult {
	out := ticketsResult{historyDue: res.historyDue}
	end := readEnd{kind: res.kind, at: at, ok: !slices.ContainsFunc(reads, (*ticketRead).failed)}
	stopped := err != nil && ctx.Err() != nil
	ended := errors.Is(err, errEnded) || errors.Is(err, ErrDisabled) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrStopped)
	switch {
	case stopped, ended:
	case res.failure != nil:
		end.failure, end.release = &FailureView{Kind: string(res.failure.Kind), Message: res.failure.Message, At: at}, res.retryAt
		out.failed = true
	case err != nil:
		end.failure = internalFailure(at)
		out.failed = true
		s.logf("trackerapp: the read of %d open tickets of %s failed: %v", len(pumps), conn, err)
	}
	for _, rd := range reads {
		if rd.answered && rd.err == nil && rd.unsettled {
			out.unsettled = append(out.unsettled, string(rd.issue))
		}
	}
	for i := range pumps {
		if e := askerErr(reads[i], res.failure, err); e != nil {
			if out.errs == nil {
				out.errs = make([]error, len(pumps))
			}
			out.errs[i] = e
		}
	}
	s.subMu.Lock()
	out.over = true
	var live []*ticketPump
	for i, tp := range pumps {
		tp.reading--
		switch {
		case tp.over || tp.core.DeadLocked():
		case ended:
			s.endTicketLocked(tp)
		default:
			if !stopped || reads[i].answered {
				s.settleTicketLocked(tp, began, reads[i], end)
			}
			live = append(live, tp)
			out.over = false
		}
	}
	// The builds are claimed with the settlement, so no one who sees the
	// read settled sees the ticket owe no build before its view is built.
	claimed, counted := s.claimBuildsLocked(live, false)
	s.subMu.Unlock()
	if err := s.buildClaimed(conn, res.root, claimed, counted, false); err != nil {
		if !errors.Is(err, ErrStopped) {
			s.logf("trackerapp: build the views of %d open tickets of %s: %v", len(claimed), conn, err)
		}
		// A ticket whose view could not be built answers its asker that
		// failure, unless its read answers one of its own.
		for i, tp := range pumps {
			if slices.Contains(claimed, tp) && out.errOf(i) == nil {
				if out.errs == nil {
					out.errs = make([]error, len(pumps))
				}
				out.errs[i] = err
			}
		}
	}
	return out
}

// readEnd is how a read of tickets ended, as each of its tickets settles
// it: failure is told to each ticket the read did not answer, due again
// at release when a rate limit holds it; ok reports a read that failed for
// no ticket (ticketRead.failed), which alone ends the connection's retry
// schedule; kind is the connection's kind as the read held it, and at
// when the read ended.
type readEnd struct {
	failure *FailureView
	release time.Time
	ok      bool
	kind    string
	at      int64
}

// errUnanswered is a ticket a read that failed for none left unanswered: a
// defect of the read, told as a failure outside the tracker.
var errUnanswered = errors.New("trackerapp: the read of open tickets made no answer of it")

// failed reports that rd settles as a failure: the read did not answer it,
// or its view could not be built.
func (rd *ticketRead) failed() bool {
	return !rd.answered || rd.err != nil && !errors.Is(rd.err, ErrIssueNotFound)
}

// settleTicketLocked settles rd, what a read that began at began found of
// tp's ticket, into tp, unless a read that began later settled already:
// for a ticket the read did not answer, the read's failure (due again at
// release when a rate limit holds it); for one it answered, the tracker
// leaving the ticket out, or a failure of its own, or success, with the
// read time of the body the record holds (noteBodyLocked). A changed
// failure is due to be told (flushTicketLocked). The caller holds subMu.
func (s *Service) settleTicketLocked(tp *ticketPump, began uint64, rd *ticketRead, end readEnd) {
	if began < tp.settled {
		return
	}
	tp.settled = began
	now := s.now()
	switch {
	case rd.failed():
		failure := end.failure
		switch {
		case rd.answered:
			s.logf("trackerapp: open ticket %s of %s: %v", tp.key.issue, tp.key.connection, rd.err)
			failure = internalFailure(end.at)
		case failure == nil:
			s.logf("trackerapp: open ticket %s of %s: %v", tp.key.issue, tp.key.connection, errUnanswered)
			failure = internalFailure(end.at)
		}
		s.dueLocked(tp, began, s.retryLocked(tp, now, end.release))
		tp.tellFailureLocked(failure)
	case rd.err != nil:
		s.dueLocked(tp, began, s.regularLocked(tp, now, end.ok))
		tp.tellFailureLocked(notFoundFailure(end.kind, end.at))
	default:
		s.dueLocked(tp, began, s.regularLocked(tp, now, end.ok))
		tp.noteBodyLocked(rd.held)
		tp.tellFailureLocked(nil)
	}
}
