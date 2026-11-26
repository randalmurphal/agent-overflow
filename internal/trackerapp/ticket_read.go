package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The verified read of several tickets of one connection: IssueDetail
// reads a set of one, an open ticket's pump its own ticket, and a
// connection's round (ticket_round.go) every shown ticket together. Every
// read stores what it found in the issue record (store StoreMirrorTickets),
// the card, the body and the evidence of the status time together, and
// each write's changes are told as it commits (run.told).

// ticketRead is one ticket of a read of several: its issue, its open
// ticket's pump (nil for a read no pump makes), whether it is read in
// full, and the bind of tp it is made under (boundDetails), zero for a
// read no pump makes; then what the read found.
type ticketRead struct {
	issue tracker.IssueID
	tp    *ticketPump
	full  bool
	bind  store.MirrorBind
	// prior is the bookkeeping of the record's body a check starts from
	// (UpdatedAt zero: none), and behind whether that body is of an older
	// version than its card: the pump's view's (ticketReads), or the
	// record's for a read no pump makes (storedBodies), which keeps the
	// body it read (stored), decoded when the check is made, to show when
	// the check confirms it.
	prior  store.MirrorBodyState
	behind bool
	stored *ticketDoc

	// detail is the document IssueDetail shows of a read no pump makes:
	// the one its full read found, or the stored body its check confirmed;
	// held is the record's body as the read's write left it.
	detail *ticketDoc
	held   store.MirrorBodyState
	// unsettled reports an issue the mirror keeps whose record's status
	// time no final read decided for its version, as the read's write
	// returned it: the history read goes to it first (roundHistory).
	unsettled bool
	// answered reports that the ticket's own read ended, whatever the
	// others' did: the tracker confirmed the body the record holds, or
	// returned its document, or left it out, or its bind failed. A ticket
	// not answered takes how the read as a whole ended (detailsRead). A
	// read that returns no error answered every ticket.
	answered bool
	// err is this answered ticket's own failure: the tracker did not return
	// it (ErrIssueNotFound), its bind failed, or the mirror did not keep the
	// body an open ticket's read brought (errBodyNotKept).
	err error
}

// errBodyNotKept is an open ticket's full read whose body the mirror did
// not keep: the cap dropped it in the same write, or a restore replaced
// the ticket's bind. Its view keeps what it showed, and the ticket is read
// again on the retry schedule.
var errBodyNotKept = errors.New("trackerapp: the mirror did not keep the body the open ticket's read brought")

// detailsRead is what a read of several tickets did as a whole: what it
// stored, the mirror root it stored it under, captured when its run
// started (zero when none started), and the tracker's failure of the read,
// classified by the connection's kind as the run held it, with the time a
// rate limit lifts. The failure is the outcome of the tickets the read did
// not answer (ticketRead.answered); each answered one has its own.
// historyDue is the sync state's report, read before the read's writes,
// that an issue of the connection is due a history read
// (store MirrorSync.HistoryDue).
type detailsRead struct {
	ch         Changes
	root       int64
	kind       string
	failure    *Failure
	retryAt    time.Time
	historyDue bool
}

// readDetails reads reads, tickets of connection id, as the tracker says
// they are now (verifiedDetails), after binding the open tickets the
// mirror does not keep yet (bindTickets), and settles the run's tracker
// calls as the connection's problem. opened records a person's opening or
// refresh of them. Each write's changes pass through the change funnel as
// it commits. A tracker failure of the read is its failure and is returned
// too, as IssueDetail returns it; any other error is returned alone.
// Either is the outcome of the tickets the read did not answer. What it
// stored is in the result either way.
func (s *Service) readDetails(ctx context.Context, id string, reads []*ticketRead, u Urgency, opened bool) (detailsRead, error) {
	r, err := s.start(ctx, id, u)
	if err != nil {
		return detailsRead{}, err
	}
	defer r.finish()
	var out detailsRead
	r.tell = func(ch Changes) { out.ch.add(ch) }
	// The linked scope's sync state carries the root's sweep the tickets'
	// writes name.
	r.scope = Scope{Connection: id}
	readErr := r.startErr
	if readErr == nil {
		readErr = r.readState()
	}
	if readErr == nil {
		out.historyDue = r.state.HistoryDue
		readErr = r.boundDetails(reads, opened)
	}
	out.root, out.kind = r.root, r.row.Kind
	var o Outcome
	callErr, stop, err := r.failed(&o, readErr)
	if stop && err == nil {
		return out, fmt.Errorf("%w: connection %s stopped being used while its tickets were read", ErrDisabled, id)
	}
	if err != nil {
		return out, err
	}
	if err := r.settleCalls(callErr); err != nil {
		return out, err
	}
	out.failure, out.retryAt = o.Failure, o.RetryAt
	return out, readErr
}

// showsOf are the other issues d's view shows from the mirror's cards: the
// issues it links, d's card being the card the view rendered
// (detailViews). Its parent is shown from the parent's name, which no
// card holds.
func showsOf(d tracker.Detail) []tracker.IssueID {
	var out []tracker.IssueID
	for _, l := range d.Links {
		if l.Issue.ID != "" && !slices.Contains(out, l.Issue.ID) {
			out = append(out, l.Issue.ID)
		}
	}
	return out
}

// boundDetails binds the open tickets of reads the mirror does not keep
// yet (bindTickets) and reads the others as the tracker says they are now
// (verifiedDetails), each open ticket's under its pump's bind as the read
// begins (ticketRead.bind): a ticket whose bind failed is answered with
// its failure and not read, and so is one whose pump retired, bound or
// not. A read made under a bind is never written under none.
func (r *run) boundDetails(reads []*ticketRead, opened bool) error {
	if err := r.bindTickets(reads); err != nil {
		return err
	}
	r.s.subMu.Lock()
	for _, rd := range reads {
		switch {
		case rd.tp == nil || rd.answered:
		case rd.tp.bind.IsZero():
			rd.err, rd.answered = ErrSubscriptionEnded, true
		default:
			rd.bind = rd.tp.bind
		}
	}
	r.s.subMu.Unlock()
	pending := slices.DeleteFunc(slices.Clone(reads), func(rd *ticketRead) bool { return rd.answered })
	if len(pending) == 0 {
		return nil
	}
	return r.verifiedDetails(pending, opened)
}

// verifiedDetails reads reads as the tracker says they are now. On a
// tracker whose update time moves with everything a detail shows
// (tracker.Capabilities.DetailMovesUpdated) a body the record holds is
// checked (checkDetails); every other ticket, and one asked in full, is
// read in full (details), all of them together. A person's opening
// (opened) begins a read on the root's clock that the bodies it confirms
// record as their opening.
func (r *run) verifiedDetails(reads []*ticketRead, opened bool) error {
	var opening store.MirrorRead
	if opened {
		var err error
		if opening, err = r.beginRead(); err != nil {
			return err
		}
	}
	toRead := reads
	if r.p.Capabilities().DetailMovesUpdated {
		var err error
		if toRead, err = r.checkDetails(reads, opening); err != nil || len(toRead) == 0 {
			return err
		}
	}
	return r.againIfSwept(func() error { return r.details(toRead, opening) })
}

// checkDetails checks reads against the tracker with one cards read of
// their own issues (a read a readBatch of them), stored with each
// ticket's record (ticketCards), and returns those to read in full: one
// asked in full, or with no body the check knows of (prior), or whose
// body is behind its card or was read more than detailMaxAge ago, read in
// full without the cards read; then one the tracker did not return, and
// one whose record, as the write left it, holds no body of the version of
// the card the tracker returned, or whose own card is of another. A read
// no pump makes is checked against the body it read from the record before
// (storedBodies), which it shows, so another body the record holds now is
// read in full too; that body is decoded only once the check is to be
// made, and one this version does not read (bodyOf) is none, read in full
// without the cards read. A confirmed ticket is answered with the
// record's body.
func (r *run) checkDetails(reads []*ticketRead, opening store.MirrorRead) ([]*ticketRead, error) {
	stored, err := r.storedBodies(reads)
	if err != nil {
		return nil, err
	}
	var toRead, checked []*ticketRead
	for _, rd := range reads {
		age, known := r.s.sinceWall(rd.prior.Wall)
		switch {
		case rd.full, rd.prior.UpdatedAt == 0, rd.behind, !known, age > detailMaxAge:
			toRead = append(toRead, rd)
		default:
			if b, ok := stored[rd]; ok {
				doc, readable := bodyOf(*b)
				if !readable {
					toRead = append(toRead, rd)
					continue
				}
				// Its card is the record's before the check's write; the view
				// reads the card that write committed (detailViews).
				doc.record = false
				rd.stored = &doc
			}
			checked = append(checked, rd)
		}
	}
	for batch := range slices.Chunk(checked, readBatch) {
		var cards []tracker.Card
		var receipts []store.MirrorTicketWrite
		err := r.againIfSwept(func() error {
			var err error
			cards, receipts, err = r.ticketCards(batch, opening)
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, rd := range batch {
			i := slices.IndexFunc(cards, func(c tracker.Card) bool { return c.ID == rd.issue })
			if i < 0 {
				toRead = append(toRead, rd)
				continue
			}
			w, version := receipts[i], millis(cards[i].Updated)
			switch {
			case !w.Kept, w.Held.ReadAt == 0, w.Held.UpdatedAt != version, w.UpdatedAt != version:
				// No body of the version the tracker returned, or the record's
				// card is of another (a board read stored a later one while the
				// tracker answered this).
				toRead = append(toRead, rd)
			case rd.stored != nil && w.Held.ReadAt != rd.prior.ReadAt:
				toRead = append(toRead, rd)
			default:
				if rd.stored != nil {
					rd.detail = rd.stored
				}
				rd.held, rd.unsettled, rd.answered = w.Held, !w.StatusFinal, true
			}
		}
	}
	return toRead, nil
}

// storedBodies reads from the record, a store.MirrorMaxBodies a
// statement, the bodies of the reads of reads no pump makes and none asks
// in full: what their checks start from (prior), and the body each read,
// undecoded, by read.
func (r *run) storedBodies(reads []*ticketRead) (map[*ticketRead]*store.MirrorBody, error) {
	var ids []string
	for _, rd := range reads {
		if rd.tp == nil && !rd.full {
			ids = append(ids, string(rd.issue))
		}
	}
	out := map[*ticketRead]*store.MirrorBody{}
	for batch := range slices.Chunk(ids, store.MirrorMaxBodies) {
		bodies, err := r.s.store.ListMirrorBodies(r.root, batch)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: read the bodies of %d issues from the mirror: %w", len(batch), err)
		}
		for i := range bodies {
			b := &bodies[i]
			for _, rd := range reads {
				if rd.tp == nil && !rd.full && string(rd.issue) == b.IssueID {
					rd.prior = store.MirrorBodyState{UpdatedAt: b.UpdatedAt, ReadAt: b.ReadAt, Wall: b.Wall}
					rd.behind, out[rd] = b.Card.UpdatedAt > b.UpdatedAt, b
				}
			}
		}
	}
	return out, nil
}

// ticketCards reads the cards of the issues of reads (at most readBatch)
// and stores them with each ticket's record under its read's bind, a
// ticket write that brings no body (store StoreMirrorTickets), which
// records opening (the zero read for none) on each body the record holds.
// It returns the cards the tracker returned and each one's receipt, in
// the same order, and tells what the write stored.
func (r *run) ticketCards(reads []*ticketRead, opening store.MirrorRead) ([]tracker.Card, []store.MirrorTicketWrite, error) {
	issues := make([]tracker.IssueID, len(reads))
	for i, rd := range reads {
		issues[i] = rd.issue
	}
	began, err := r.beginRead()
	if err != nil {
		return nil, nil, err
	}
	r.calls++
	read, err := r.p.Cards(r.ctx, issues)
	if err != nil {
		return nil, nil, fmt.Errorf("trackerapp: read %d cards: %w", len(issues), call(err))
	}
	if err := r.markMissing(began, read.Missing); err != nil {
		return nil, nil, err
	}
	if len(read.Cards) == 0 {
		return nil, nil, nil
	}
	tickets := make([]store.MirrorTicket, len(read.Cards))
	for i, c := range read.Cards {
		at := slices.Index(issues, c.ID)
		if at < 0 || slices.ContainsFunc(read.Cards[:i], func(o tracker.Card) bool { return o.ID == c.ID }) {
			return nil, nil, fmt.Errorf("trackerapp: the cards of %d issues: %w", len(issues),
				call(fmt.Errorf("the tracker answered with issue %q", c.ID)))
		}
		if tickets[i].Card, err = mirrorCard(c); err != nil {
			return nil, nil, err
		}
		tickets[i].Bind = reads[at].bind
	}
	catalog, err := mirrorCatalog(read.Catalog)
	if err != nil {
		return nil, nil, err
	}
	w, err := r.s.store.StoreMirrorTickets(r.root, r.state.Sweep, tickets, catalog, opening, began)
	if err != nil {
		return nil, nil, fmt.Errorf("trackerapp: store the cards of %d tickets: %w", len(tickets), err)
	}
	r.told("", ticketChanges(tickets, w))
	return read.Cards, w.Tickets, nil
}

// details reads the details of reads in full, together (Provider.Details),
// and stores each in its issue's record, read when the read began, in one
// write (store StoreMirrorTickets): its card through the one card path,
// with the catalog entries the card names; its body, by the record's order
// of bodies; and the evidence of its status time, taken before
// boundHistory. A person's opening (opening set) is recorded as this
// read, the read that serves it. Each open ticket's read is written under
// its read's bind (ticketRead.bind), and fails with errBodyNotKept when
// the record does not keep its body; a read no pump makes keeps its
// document (ticketRead.detail) whatever the record did with it. A ticket
// the tracker did not return fails with ErrIssueNotFound.
func (r *run) details(reads []*ticketRead, opening store.MirrorRead) error {
	began, err := r.beginRead()
	if err != nil {
		return err
	}
	if opening != (store.MirrorRead{}) {
		opening = began
	}
	ids := make([]tracker.IssueID, len(reads))
	for i, rd := range reads {
		ids[i] = rd.issue
	}
	r.calls++
	got, err := r.p.Details(r.ctx, ids)
	if err != nil {
		return fmt.Errorf("trackerapp: read the details of %d issues: %w", len(ids), call(err))
	}
	readOf := func(issue tracker.IssueID) *ticketRead {
		i := slices.Index(ids, issue)
		if i < 0 {
			return nil
		}
		return reads[i]
	}
	var missing []tracker.IssueID
	for _, issue := range got.Missing {
		if rd := readOf(issue); rd != nil {
			rd.err = ErrIssueNotFound
			missing = append(missing, issue)
		}
	}
	if err := r.markMissing(began, missing); err != nil {
		return err
	}
	settle := r.p.Capabilities().HistorySettle
	tickets := make([]store.MirrorTicket, 0, len(got.Details))
	read := make([]*ticketRead, 0, len(got.Details))
	var named tracker.Catalog
	for i := range got.Details {
		d := got.Details[i]
		rd := readOf(d.Card.ID)
		if rd == nil || slices.Contains(read, rd) {
			return fmt.Errorf("trackerapp: the details of %d issues: %w", len(ids),
				call(fmt.Errorf("the tracker answered with issue %q", d.Card.ID)))
		}
		evidence := statusEvidence(d, settle)
		boundHistory(&d)
		card, err := mirrorCard(d.Card)
		if err != nil {
			return err
		}
		body, err := mirrorBody(d)
		if err != nil {
			return err
		}
		tickets = append(tickets, store.MirrorTicket{Card: card, Body: body, Status: &evidence, Bind: rd.bind})
		read = append(read, rd)
		cardNames(&named, d)
		rd.err = nil
		if rd.tp == nil {
			rd.detail = &ticketDoc{d: d, version: millis(d.Card.Updated), status: &evidence}
		}
	}
	for _, rd := range reads {
		if rd.err == nil && !slices.Contains(read, rd) {
			return fmt.Errorf("trackerapp: the details of %d issues: %w", len(ids),
				call(fmt.Errorf("the tracker neither returned nor left out issue %s", rd.issue)))
		}
	}
	// A ticket the tracker left out is answered whether or not the others
	// are stored.
	for _, rd := range reads {
		rd.answered = rd.err != nil
	}
	if len(tickets) == 0 {
		return nil
	}
	catalog, err := mirrorCatalog(named)
	if err != nil {
		return err
	}
	written, err := r.s.store.StoreMirrorTickets(r.root, r.state.Sweep, tickets, catalog, opening, began)
	if err != nil {
		return fmt.Errorf("trackerapp: store the tickets of %d issues: %w", len(tickets), err)
	}
	r.told("", ticketChanges(tickets, written))
	for i, w := range written.Tickets {
		rd := read[i]
		rd.answered, rd.held = true, w.Held
		rd.unsettled = w.Kept && !w.StatusFinal
		switch w.Body {
		case store.MirrorBodyStored, store.MirrorBodySuperseded:
			// The record holds this read's body, or a later read's, whose
			// writer told the ticket.
		case store.MirrorBodyNotKept:
			if rd.tp != nil {
				rd.err = fmt.Errorf("%w: issue %s", errBodyNotKept, rd.issue)
			}
		case store.MirrorBodyNone:
			rd.err = fmt.Errorf("trackerapp: the store reported no outcome of the body of %s, which the write brought", rd.issue)
		}
	}
	return nil
}

// ticketChanges are the changes of w, the write of tickets: what its card
// path stored, the issues whose bodies it stored new or changed (Details)
// or dropped for the cap, and those whose status time it moved, which the
// views of their cards show as a history read's (History).
func ticketChanges(tickets []store.MirrorTicket, w store.MirrorTicketsWrite) written {
	ch := changesOf(w.Changes)
	ch.Dropped = issueIDs(w.Dropped)
	var bodies, status []string
	for i, t := range w.Tickets {
		id := tickets[i].Card.IssueID
		if t.BodyChanged {
			bodies = append(bodies, id)
		}
		if t.StatusChanged {
			status = append(status, id)
		}
	}
	ch.Details, ch.History = issueIDs(bodies), issueIDs(status)
	return ch
}

// cardNames adds to named the entries of d's catalog that d's card names,
// its status and assignee, each once: only what the card names goes to
// the mirror's shared catalog; the body keeps the rest of its catalog
// (comment and history authors, the statuses its moves and links name).
func cardNames(named *tracker.Catalog, d tracker.Detail) {
	for _, u := range d.Users {
		if u.ID == d.Card.Assignee && !slices.ContainsFunc(named.Users, func(o tracker.User) bool { return o.ID == u.ID }) {
			named.Users = append(named.Users, u)
		}
	}
	for _, st := range d.Statuses {
		if st.ID == d.Card.Status && !slices.ContainsFunc(named.Statuses, func(o tracker.Status) bool { return o.ID == st.ID }) {
			named.Statuses = append(named.Statuses, st)
		}
	}
}
