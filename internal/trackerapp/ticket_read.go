package trackerapp

import (
	"context"
	"fmt"
	"hash/maphash"
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
// each write's changes are told as it commits (run.tell).

// detailCheck is what a ticket's next read checks it against without
// decoding a body: the body the ticket shows, named by when its read began
// (the record's body_read_at, an order time, with its wall time, which its
// age is measured from), or held when the mirror holds none of it; the
// update time of the version it shows; and a digest of its encoding, which
// tells a changed body from the same one.
type detailCheck struct {
	readAt  int64
	wall    int64
	updated int64
	held    *ticketDoc
	doc     uint64
}

// ticketRead is one ticket of a read of several: its issue, its open
// ticket's pump (nil for IssueDetail's read), the check its pump last
// settled (nil for none), whether it is read in full, and whether a stored
// card of its issue changed since that check settled (readNowLocked), then
// what the read found.
type ticketRead struct {
	issue  tracker.IssueID
	tp     *ticketPump
	prior  *detailCheck
	full   bool
	nudged bool

	// check is the ticket's check after the read; detail is the document
	// when the read read or decoded it, nil when the check confirmed the
	// prior one; changed reports a view other than prior's: another body,
	// another version, or a card or status time the read's write moved.
	check   detailCheck
	detail  *ticketDoc
	changed bool
	// unsettled reports an issue the mirror keeps whose record's status
	// time no final read decided for its version, as the read's write
	// returned it: the history read goes to it first (roundHistory).
	unsettled bool
	// answered reports that the ticket's own read ended, whatever the
	// others' did: the tracker confirmed the body its check names, or
	// returned its document, stored or held, or left it out, or its bind
	// failed. A ticket not answered takes how the read as a whole ended
	// (detailsRead). A read that returns no error answered every ticket.
	answered bool
	// err is this answered ticket's own failure: the tracker did not return
	// it (ErrIssueNotFound), its bind failed, or its view could not be
	// built.
	err error
	// view is the ticket's view built from detail when changed, with its
	// digest and the issues it shows of other issues.
	view   *DetailView
	digest uint64
	shows  []tracker.IssueID
}

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
	gen        uint64
	kind       string
	failure    *Failure
	retryAt    time.Time
	historyDue bool
}

// readDetails reads reads, tickets of connection id, as the tracker says
// they are now (verifiedDetails), after binding the open tickets the
// mirror does not keep yet (bindTickets), settles the run's tracker calls
// as the connection's problem, and builds the view of each answered ticket
// whose body changed, or that its pump is to hold and held none of
// (prior.held nil), however the read ended unless the connection did.
// opened records a person's opening or refresh of them. Each write's
// changes pass through the change funnel as it commits, origin the pumps
// whose own tickets these are (nil for none). A tracker failure of the
// read is its failure and is returned too, as IssueDetail returns it; any
// other error is returned alone. Either is the outcome of the tickets the
// read did not answer. What it stored is in the result either way.
func (s *Service) readDetails(ctx context.Context, id string, reads []*ticketRead, u Urgency, opened bool, origin *tickOrigin) (detailsRead, error) {
	r, err := s.start(ctx, id, u)
	if err != nil {
		return detailsRead{}, err
	}
	defer r.finish()
	var out detailsRead
	r.tell = func(ch Changes) {
		out.ch.add(ch)
		s.publish(r.root, r.gen, id, ch, nil, origin)
	}
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
	out.root, out.gen, out.kind = r.root, r.gen, r.row.Kind
	var o Outcome
	callErr, stop, err := r.failed(&o, readErr)
	if stop && err == nil {
		return out, fmt.Errorf("%w: connection %s stopped being used while its tickets were read", ErrDisabled, id)
	}
	s.ticketViews(r.root, r.row, reads)
	if err != nil {
		return out, err
	}
	if err := r.settleCalls(callErr); err != nil {
		return out, err
	}
	out.failure, out.retryAt = o.Failure, o.RetryAt
	return out, readErr
}

// ticketViews builds the view of each answered read of reads whose body
// changed, or that its pump is to hold and held none of (prior.held nil),
// named from root on connection row, the views together (detailViews). A
// build that fails is the failure of each read it was to build.
func (s *Service) ticketViews(root int64, row store.TrackerConnection, reads []*ticketRead) {
	var built []*ticketRead
	var docs []ticketDoc
	for _, rd := range reads {
		heldNew := rd.check.held != nil && (rd.prior == nil || rd.prior.held == nil)
		if !rd.answered || rd.err != nil || rd.detail == nil || !rd.changed && !heldNew {
			continue
		}
		built, docs = append(built, rd), append(docs, *rd.detail)
	}
	if len(docs) == 0 {
		return
	}
	views, shows, err := s.detailViews(root, row, docs)
	for i, rd := range built {
		if err != nil {
			rd.err = fmt.Errorf("trackerapp: build the view of %s: %w", rd.issue, err)
			continue
		}
		rd.view, rd.digest, rd.shows = &views[i], s.digestOf(views[i]), shows[i]
	}
}

// showsOf are the other issues d's view shows from the mirror's cards:
// its parent and the issues it links, d's card being the card the view
// rendered (detailViews).
func showsOf(d tracker.Detail) []tracker.IssueID {
	var out []tracker.IssueID
	if d.Card.Parent != "" {
		out = append(out, d.Card.Parent)
	}
	for _, l := range d.Links {
		if l.Issue.ID != "" && !slices.Contains(out, l.Issue.ID) {
			out = append(out, l.Issue.ID)
		}
	}
	return out
}

// boundDetails binds the open tickets of reads the mirror does not keep
// yet (bindTickets) and reads the others as the tracker says they are now
// (verifiedDetails): a ticket whose bind failed is answered with its
// failure and not read.
func (r *run) boundDetails(reads []*ticketRead, opened bool) error {
	if err := r.bindTickets(reads); err != nil {
		return err
	}
	pending := slices.DeleteFunc(slices.Clone(reads), func(rd *ticketRead) bool { return rd.answered })
	if len(pending) == 0 {
		return nil
	}
	return r.verifiedDetails(pending, opened)
}

// verifiedDetails sets each read's detail as the tracker says it is now.
// On a tracker whose update time moves with everything a detail shows
// (tracker.Capabilities.DetailMovesUpdated) a body the mirror or the pump
// holds is checked (checkDetails); every other ticket, and one asked in
// full, is read in full (details), all of them together. A person's
// opening (opened) begins a read on the root's clock that the bodies it
// confirms record as their opening.
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
// asked in full, one with no body to check, whose body's read began more
// than detailMaxAge ago or whose card the mirror holds newer (priorCheck,
// mirrorMoved), one whose card's update time differs from its body's or
// that the tracker did not return, and one whose body is no longer held
// where its check says, as the write found the record: by the pump while
// the mirror holds none, or by the mirror as the same body (evicted,
// swept, replaced, or the mirror holds one now beside the pump's). So a
// successful read always leaves the ticket's content with the mirror or
// the pump. The body checked is the pump's prior one, else the mirror's,
// decoded.
func (r *run) checkDetails(reads []*ticketRead, opening store.MirrorRead) ([]*ticketRead, error) {
	var toRead, checked []*ticketRead
	for _, rd := range reads {
		if !rd.full {
			if err := r.priorCheck(rd); err != nil {
				return nil, err
			}
		}
	}
	if err := r.mirrorMoved(reads); err != nil {
		return nil, err
	}
	for _, rd := range reads {
		age, known := r.s.sinceWall(rd.check.wall)
		switch {
		case rd.full, rd.check == (detailCheck{}), !known, age > detailMaxAge:
			toRead = append(toRead, rd)
		default:
			checked = append(checked, rd)
		}
	}
	for batch := range slices.Chunk(checked, readBatch) {
		issues := make([]tracker.IssueID, len(batch))
		for i, rd := range batch {
			issues[i] = rd.issue
		}
		var cards []tracker.Card
		var receipts []store.MirrorTicketWrite
		err := r.againIfSwept(func() error {
			var err error
			cards, receipts, err = r.ticketCards(issues, opening)
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, rd := range batch {
			i := slices.IndexFunc(cards, func(c tracker.Card) bool { return c.ID == rd.issue })
			if i < 0 || millis(cards[i].Updated) != rd.check.updated {
				toRead = append(toRead, rd)
				continue
			}
			w := receipts[i]
			switch {
			case rd.check.held != nil && w.Held.ReadAt == 0:
			case rd.check.held != nil || !w.Kept || w.Held.ReadAt != rd.check.readAt:
				toRead = append(toRead, rd)
				continue
			}
			rd.unsettled = w.Kept && !w.StatusFinal
			rd.changed = rd.prior == nil || rd.prior.doc != rd.check.doc || w.CardChanged || w.StatusChanged
			rd.answered = true
		}
	}
	return toRead, nil
}

// ticketCards reads the cards of issues (at most readBatch) and stores
// them with each ticket's record, a ticket write that brings no body
// (store StoreMirrorTickets), which records opening (the zero read for
// none) on each body the record holds. It returns the cards the tracker
// returned and each one's receipt, in the same order, and tells what the
// write stored.
func (r *run) ticketCards(issues []tracker.IssueID, opening store.MirrorRead) ([]tracker.Card, []store.MirrorTicketWrite, error) {
	began, err := r.beginRead()
	if err != nil {
		return nil, nil, err
	}
	r.calls++
	read, err := r.p.Cards(r.ctx, issues)
	if err != nil {
		return nil, nil, fmt.Errorf("trackerapp: read %d cards: %w", len(issues), call(err))
	}
	if len(read.Cards) == 0 {
		return nil, nil, nil
	}
	tickets := make([]store.MirrorTicket, len(read.Cards))
	for i, c := range read.Cards {
		if !slices.Contains(issues, c.ID) || slices.ContainsFunc(read.Cards[:i], func(o tracker.Card) bool { return o.ID == c.ID }) {
			return nil, nil, fmt.Errorf("trackerapp: the cards of %d issues: %w", len(issues),
				call(fmt.Errorf("the tracker answered with issue %q", c.ID)))
		}
		if tickets[i].Card, err = mirrorCard(c); err != nil {
			return nil, nil, err
		}
	}
	catalog, err := mirrorCatalog(read.Catalog)
	if err != nil {
		return nil, nil, err
	}
	w, err := r.s.store.StoreMirrorTickets(r.root, r.state.Sweep, tickets, catalog, opening, began)
	if err != nil {
		return nil, nil, fmt.Errorf("trackerapp: store the cards of %d tickets: %w", len(tickets), err)
	}
	r.tell(ticketChanges(tickets, w))
	return read.Cards, w.Tickets, nil
}

// priorCheck sets rd's check to the body a check compares: the pump's
// prior one, else the mirror's, decoded (rd.detail), read in full when
// the record's card is already newer than it. It sets no check when there
// is none.
func (r *run) priorCheck(rd *ticketRead) error {
	if rd.prior != nil {
		rd.check = *rd.prior
		return nil
	}
	b, doc, ok, err := r.s.storedDetail(r.root, rd.issue)
	if err != nil || !ok {
		return err
	}
	rd.detail = &doc
	rd.check = detailCheck{readAt: b.ReadAt, wall: b.Wall, updated: b.UpdatedAt, doc: maphash.Bytes(r.s.digestSeed, b.Data)}
	rd.full = b.Card.UpdatedAt > b.UpdatedAt
	return nil
}

// mirrorMoved reads in full each of reads checked against its pump's prior
// body whose stored card changed since (ticketRead.nudged) and is newer
// than that body, as priorCheck does for the mirror's copy: the read that
// stored the card is the evidence, so no cards read is asked for it. One
// whose stored card has the body's update time (a rename the board read)
// goes on to the check. One read of the mirror's cards serves them all; a
// read with no such ticket reads nothing.
func (r *run) mirrorMoved(reads []*ticketRead) error {
	var issues []string
	for _, rd := range reads {
		if rd.nudged && !rd.full && rd.prior != nil {
			issues = append(issues, string(rd.issue))
		}
	}
	if len(issues) == 0 {
		return nil
	}
	cards, err := r.s.store.ListMirrorCards(r.root, issues)
	if err != nil {
		return fmt.Errorf("trackerapp: read the cards of %d issues from the mirror: %w", len(issues), err)
	}
	for _, rd := range reads {
		if rd.nudged && !rd.full && rd.prior != nil {
			i := slices.IndexFunc(cards, func(c store.MirrorCard) bool { return c.IssueID == string(rd.issue) })
			rd.full = i >= 0 && cards[i].UpdatedAt > rd.check.updated
		}
	}
	return nil
}

// details reads the details of reads in full, together (Provider.Details),
// and stores each in its issue's record, read when the read began, in one
// write (store StoreMirrorTickets): its card through the one card path,
// with the catalog entries the card names; its body, by the record's order
// of bodies; and the evidence of its status time, taken before
// boundHistory. A person's opening (opening set) is recorded as this
// read, the read that serves it. A body the record does not keep (the
// mirror does not keep the issue, or holds no body of it and the read is
// neither an opening nor begun after its open ticket's bind, or the cap
// dropped it) is held by the read's check; one the record holds a later
// read's of in place of is not shown, and the next read checks the
// record's. A ticket the tracker did not return fails with
// ErrIssueNotFound.
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
	for _, issue := range got.Missing {
		if rd := readOf(issue); rd != nil {
			rd.err = ErrIssueNotFound
		}
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
		tickets = append(tickets, store.MirrorTicket{Card: card, Body: body, Status: &evidence})
		read = append(read, rd)
		cardNames(&named, d)
		rd.err, rd.detail = nil, &ticketDoc{d: d, version: millis(d.Card.Updated), status: &evidence}
		rd.check = detailCheck{readAt: began.At(), wall: began.Wall(), updated: millis(d.Card.Updated), doc: maphash.Bytes(r.s.digestSeed, body)}
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
	r.tell(ticketChanges(tickets, written))
	for i, w := range written.Tickets {
		rd := read[i]
		rd.answered = true
		rd.unsettled = w.Kept && !w.StatusFinal
		switch w.Body {
		case store.MirrorBodyStored:
			// The record holds this read's body, which the check names.
		case store.MirrorBodyNotKept:
			rd.check.held = rd.detail
		case store.MirrorBodySuperseded:
			// A read that began later, or of a later version, stored another
			// body: the next read checks that one, and its writer tells the
			// ticket.
			rd.check, rd.detail = detailCheck{}, nil
			continue
		case store.MirrorBodyNone:
			rd.err = fmt.Errorf("trackerapp: the store reported no outcome of the body of %s, which the write brought", rd.issue)
			continue
		}
		rd.changed = rd.prior == nil || rd.prior.doc != rd.check.doc || rd.prior.updated != rd.check.updated || w.CardChanged || w.StatusChanged
	}
	return nil
}

// ticketChanges are the changes of w, the write of tickets: what its card
// path stored, the issues whose bodies it stored new or changed (Details)
// or dropped for the cap, and those whose status time it moved, which the
// views of their cards show as a history read's (History).
func ticketChanges(tickets []store.MirrorTicket, w store.MirrorTicketsWrite) Changes {
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
