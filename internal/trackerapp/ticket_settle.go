package trackerapp

import (
	"context"
	"errors"
	"hash/maphash"
	"slices"
	"time"

	"agent-overflow/internal/tracker"
)

// What settles into an open ticket's pump. A ticket's view is derived from
// its document and from what the mirror holds of other things
// (ticketInputs), which move independently: the document by reads, ordered
// by when they began, and the other inputs by the change funnel. One rule
// installs a view (installViewLocked): it was built from the document the
// pump holds now, and its build began after the last change to the other
// inputs. A read settles its document and the view it built under that
// rule; a view that misses it is built again (rebuildTicket) by the
// goroutine that settled the document, or by the pump when an input
// moved. Whether the view is loaded is part of what is told: a view that
// becomes loaded, or stops being, is told even when its content is the
// same.

// ticketsResult is how a read of tickets ended: failed for the read as a
// whole, errs by pump (errOf), over when every pump read ended.
type ticketsResult struct {
	failed bool
	// historyDue reports that the read found an issue of the connection
	// due a history read (detailsRead.historyDue); historyWanted that a
	// round's read wants the history unit run (roundRead).
	historyDue, historyWanted bool
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

// ticketInputs reports what ch, changes stored by one read, moves of tp's
// view: read when the issue's own card changed and the same read stored no
// document of it, so the ticket is due a full read at once; rebuild when
// an input of the view other than its document may have changed, or
// another read stored its document, which the rebuild reads from the
// mirror's copy. The inputs are those detailView reads: the
// mirror's card of the issue (its status time, from the card and its
// history), the cards of the issues it shows (its parent and the issues it
// links) and whether the mirror keeps them (a row stored or removed), the
// connection's users and statuses, and the mirror's copy of the document
// (another read stored it, or the cap dropped it, or the issue stopped or
// started being kept). origin reports that ch is the read's of the ticket
// itself, which settles its own card and document.
func ticketInputs(tp *ticketPump, ch Changes, origin bool) (read, rebuild bool) {
	own := func(ids []tracker.IssueID) bool { return slices.Contains(ids, tp.key.issue) }
	shown := func(ids []tracker.IssueID) bool {
		return slices.ContainsFunc(tp.shows, func(id tracker.IssueID) bool { return slices.Contains(ids, id) })
	}
	read = !origin && own(ch.Cards) && !own(ch.Details)
	rebuild = ch.Catalog || own(ch.Cards) || own(ch.History) || own(ch.Rows) || own(ch.Removed) || own(ch.Dropped) ||
		!origin && own(ch.Details) || shown(ch.Cards) || shown(ch.Rows) || shown(ch.Removed)
	return read, rebuild
}

// readTickets reads the tickets of pumps, all of connection conn, together
// (readDetails), each checked against what its pump settled last and read
// in full when full is set (ticketReads),
// passes what the read stored through the change funnel under the root the
// read held, and settles each pump with its own outcome (settleTickets).
// opened records a person's opening.
func (s *Service) readTickets(ctx context.Context, conn string, pumps []*ticketPump, u Urgency, full, opened bool) ticketsResult {
	began, reads := s.ticketReads(pumps, full)
	at := millis(s.now())
	res, err := s.readDetails(ctx, conn, reads, u, opened)
	s.publish(res.root, res.gen, conn, res.ch, nil, &tickOrigin{tickets: pumps})
	return s.settleTickets(ctx, conn, pumps, began, reads, res, err, at)
}

// ticketReads are the reads of pumps' tickets, each checked against what
// its pump settled last and read in full when full is set, and the stamp
// the read began at. A read at once (readNowLocked) is checked as any
// other, and read in full when the card stored since is newer than its
// document (mirrorMoved). Each is in flight (ticketPump.reading) until
// settleTickets settles it.
func (s *Service) ticketReads(pumps []*ticketPump, full bool) (began uint64, reads []*ticketRead) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	began = s.nextStampLocked()
	reads = make([]*ticketRead, len(pumps))
	for i, tp := range pumps {
		tp.reading++
		reads[i] = &ticketRead{issue: tp.key.issue, full: full, nudged: tp.nudged > tp.settled}
		if tp.check != nil {
			prior := *tp.check
			reads[i].prior = &prior
		}
	}
	return began, reads
}

// settleTickets settles reads, what a read of pumps' tickets on connection
// conn that began at began found, into each pump, and rebuilds the view of
// each whose view the settlement left behind: res and err are how the read
// ended (readDetails), res.ch everything the run stored that the funnel
// was handed with these pumps as its origin, ctx the context the read ran
// under, and at when it ended. Each ticket the read answered settles with
// its own outcome; the read's failure is told to each ticket it did not
// answer and makes it due on the retry schedule. A read stopped by its
// context settles only the tickets it answered; a connection that ended
// ends every pump. Each pump's asker is answered its own ticket's outcome
// (ticketsResult.errOf).
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
	var behind []*ticketPump
	for i, tp := range pumps {
		tp.reading--
		switch {
		case tp.over || tp.core.DeadLocked():
		case ended:
			s.endTicketLocked(tp)
		case stopped && !reads[i].answered:
			// A rebuild that left its document to this read builds now.
			if tp.behindLocked() {
				behind = append(behind, tp)
			}
			out.over = false
		default:
			if s.settleTicketLocked(tp, began, reads[i], res.ch, end) {
				behind = append(behind, tp)
			}
			out.over = false
		}
	}
	s.subMu.Unlock()
	for _, tp := range behind {
		s.rebuildTicket(tp)
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
// leaving the ticket out, a view that could not be built, or the document
// now and which of the mirror and the pump holds it, with the view the
// read built of it. ch is what the read stored. A changed view or failure
// is told. It reports whether the view is left behind its inputs. The
// caller holds subMu.
func (s *Service) settleTicketLocked(tp *ticketPump, began uint64, rd *ticketRead, ch Changes, end readEnd) bool {
	_, moved := ticketInputs(tp, ch, true)
	if began < tp.settled {
		// What an earlier read stored may postdate the view the later one
		// settled, and a rebuild may have left its document to this read.
		if moved {
			tp.inputs = s.nextStampLocked()
		}
		return tp.behindLocked()
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
		if failure.Kind == string(FailureInternal) {
			// What the read stored may not be in the view: the next read
			// decodes and rebuilds it.
			tp.check = nil
		}
		tp.tellFailureLocked(failure)
	case rd.err != nil:
		s.dueLocked(tp, began, s.regularLocked(tp, now, end.ok))
		tp.tellFailureLocked(notFoundFailure(end.kind, end.at))
	default:
		s.dueLocked(tp, began, s.regularLocked(tp, now, end.ok))
		tp.check = nil
		if rd.check != (detailCheck{}) {
			check := rd.check
			tp.check = &check
		}
		if rd.check.held == nil {
			tp.held = nil
		}
		if rd.view != nil {
			tp.docs++
			if began >= tp.inputs {
				tp.shows = rd.shows
				s.installViewLocked(tp, rd.view, rd.digest, rd.check.wall, began)
			}
		}
		tp.tellFailureLocked(nil)
	}
	if moved && tp.view.at < began {
		// The read stored a change to an input of a view it did not build.
		tp.inputs = max(tp.inputs, began)
	}
	s.flushTicketLocked(tp, false)
	return tp.behindLocked()
}

// behindLocked reports that tp's view is not built from its document or
// predates a change to its other inputs. The caller holds subMu.
func (tp *ticketPump) behindLocked() bool {
	return tp.view.docs != tp.docs || tp.view.at < tp.inputs
}

// installViewLocked makes v (nil: not loaded), with digest, tp's view,
// built from the document tp holds now (held by the pump when its check
// holds it, the document's read begun at wall time wall) in a build that began at
// stamp at. A view that differs from the one told, in content or in
// being loaded, is due to be told. The caller holds subMu and has checked
// that the view is current (settleTicketLocked, rebuildTicket).
func (s *Service) installViewLocked(tp *ticketPump, v *DetailView, digest uint64, wall int64, at uint64) {
	loaded := v != nil
	if loaded != tp.view.loaded || digest != tp.view.digest {
		tp.changed, tp.pending = true, true
	}
	tp.view = toldView{loaded: loaded, sinceKnown: loaded && v.Card.StatusSince != 0, digest: digest, docs: tp.docs, at: at}
	tp.held = nil
	if loaded && tp.check != nil && tp.check.held != nil {
		tp.held = &heldDetail{view: *v, wall: wall}
	}
}

// notLoadedLocked records, at stamp at, that neither the mirror nor tp's
// pump holds its ticket's document, as a rebuild found or as a subscriber
// was just answered (SubscribeTicket, TicketDetail). The view is told as
// not loaded when it was told as loaded, and the ticket is then due a read
// at once. The pump forgets its document as of at, so a read that began
// earlier settles nothing and the next read builds the view of whatever
// it finds and tells it. A pump that already told so and holds no document
// has nothing to forget: a read that began since is the one that loads it,
// and finding the ticket not loaded again leaves that read standing; the
// not loaded view is still the one built at at. A ticket whose first read
// has not settled is that read's to tell. The caller holds subMu.
func (s *Service) notLoadedLocked(tp *ticketPump, at uint64) {
	if tp.over || tp.core.DeadLocked() || tp.settled == 0 {
		return
	}
	was := tp.view.loaded
	if was || tp.check != nil {
		tp.check, tp.shows, tp.settled = nil, nil, max(tp.settled, at)
	}
	s.installViewLocked(tp, nil, 0, 0, at)
	if was {
		s.readNowLocked(tp)
	}
	s.flushTicketLocked(tp, false)
}

// rebuildTicket builds tp's view again, with no tracker call, from the
// newer of the document its pump holds and the mirror's copy (rebuiltView),
// and installs it unless a read settled another document meanwhile (that
// read builds its own) or an input moved since the build began (its nudge
// builds again). A mirror's copy that is not the pump's document becomes
// it, and the pump lets go of the one it held, unless a read of the ticket
// is in flight: the copy may be that read's, and a view of it told before
// the read settles would show its document with the failure the read
// ends.
// With no document anywhere the view is told as not loaded and the ticket
// is due a read at once. A build that fails is told as a failure outside
// the tracker when no failure is told, and is tried again at the next
// read or nudge.
func (s *Service) rebuildTicket(tp *ticketPump) {
	s.subMu.Lock()
	b, docs := s.nextStampLocked(), tp.docs
	var held *tracker.Detail
	var readAt, wall int64
	if tp.check != nil {
		held, readAt, wall = tp.check.held, tp.check.readAt, tp.check.wall
	}
	first := tp.settled == 0
	s.subMu.Unlock()
	if first {
		// The first read settles the view.
		return
	}
	at := millis(s.now())
	v, d, mirrored, ok, err := s.rebuiltView(tp.key, held, readAt)
	s.subMu.Lock()
	defer s.subMu.Unlock()
	switch {
	case tp.over || tp.core.DeadLocked() || tp.docs != docs || tp.inputs > b:
		return
	case err != nil:
		s.logf("trackerapp: rebuild the view of open ticket %s of %s: %v", tp.key.issue, tp.key.connection, err)
		tp.check = nil
		if tp.failure == nil {
			tp.tellFailureLocked(internalFailure(at))
		}
	case !ok:
		s.notLoadedLocked(tp, b)
		return
	case mirrored != (detailCheck{}) && (tp.check == nil || *tp.check != mirrored) && tp.reading > 0:
		// The mirror's copy is another read's document, and a read of the
		// ticket is in flight: it may be that read's, which settles it with
		// its outcome. That read builds the view, or rebuilds when it
		// settles behind.
		return
	default:
		if mirrored != (detailCheck{}) && (tp.check == nil || *tp.check != mirrored) {
			// The mirror's copy is another read's document.
			tp.check = &mirrored
			tp.docs++
			wall = mirrored.wall
		}
		tp.shows = showsOf(d)
		s.installViewLocked(tp, &v, s.digestOf(v), wall, b)
	}
	s.flushTicketLocked(tp, false)
}

// rebuiltView is key's view built from the newer, by when their reads
// began, of the document its pump holds (held, its read begun at heldAt;
// nil for none) and the mirror's copy, which another read may have stored
// beside the pump's. mirrored is the mirror's copy's check when the view is
// built from it. ok is false when neither holds a document.
func (s *Service) rebuiltView(key ticketKey, held *tracker.Detail, heldAt int64) (v DetailView, d tracker.Detail, mirrored detailCheck, ok bool, err error) {
	root, err := s.readRoot(key.connection)
	if errors.Is(err, ErrNotMirrored) {
		return DetailView{}, tracker.Detail{}, detailCheck{}, false, nil
	}
	if err != nil {
		return DetailView{}, tracker.Detail{}, detailCheck{}, false, err
	}
	defer s.leave()
	m, stored, isStored, err := s.storedDetail(root, key.issue)
	switch {
	case err != nil:
		return DetailView{}, tracker.Detail{}, detailCheck{}, false, err
	case isStored && (held == nil || m.ReadAt > heldAt):
		d = stored
		mirrored = detailCheck{readAt: m.ReadAt, wall: m.ReadWall, updated: millis(stored.Card.Updated), doc: maphash.Bytes(s.digestSeed, m.Data)}
	case held != nil:
		d = *held
	default:
		return DetailView{}, tracker.Detail{}, detailCheck{}, false, nil
	}
	v, err = s.detailView(root, d)
	return v, d, mirrored, err == nil, err
}
