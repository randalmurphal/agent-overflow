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
// connection's round (ticket_round.go) every shown ticket together.

// detailCheck is what a ticket's next read checks it against without
// decoding a document: the document the ticket shows, named by when its
// read began (the stored document's MirrorDetail.ReadAt), or held when the
// mirror holds none of it; its card's update time; and a digest of its
// encoding, which tells a changed document from the same one.
type detailCheck struct {
	readAt  int64
	updated int64
	held    *tracker.Detail
	doc     uint64
}

// ticketRead is one ticket of a read of several: its issue and the check
// its pump last settled (nil for none), then what the read found.
type ticketRead struct {
	issue tracker.IssueID
	prior *detailCheck

	// check is the ticket's check after the read; detail is the document
	// when the read read or decoded it, nil when the check confirmed the
	// prior one; changed reports a document other than prior's.
	check   detailCheck
	detail  *tracker.Detail
	changed bool
	// err is this ticket's own failure: the tracker did not return it
	// (ErrIssueNotFound). stored reports that the read stored a changed
	// document of it.
	err    error
	stored bool
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
// rate limit lifts.
type detailsRead struct {
	ch      Changes
	root    int64
	kind    string
	failure *Failure
	retryAt time.Time
}

// readDetails reads reads, tickets of connection id, as the tracker says
// they are now (verifiedDetails), settles the run's tracker calls as the
// connection's problem, and builds the view of each ticket whose document
// changed, or that its pump is to hold and held none of (prior.held nil).
// full reads each in full; openedAt is when a person opened or refreshed
// them, zero for a read no person asked for. A tracker failure of the read
// is its failure and is returned too, as IssueDetail returns it; any other
// error is returned alone. What it stored is in the result either way.
func (s *Service) readDetails(ctx context.Context, id string, reads []*ticketRead, u Urgency, full bool, openedAt int64) (detailsRead, error) {
	r, err := s.start(ctx, id, u)
	if err != nil {
		return detailsRead{}, err
	}
	defer r.finish()
	var out detailsRead
	// The linked scope's sync state carries the root's sweep the details'
	// and the cards' writes name.
	r.scope = Scope{Connection: id}
	readErr := r.startErr
	if readErr == nil {
		readErr = r.readState()
	}
	if readErr == nil {
		out.ch, readErr = r.verifiedDetails(reads, full, openedAt)
	}
	out.root, out.kind = r.root, r.row.Kind
	var o Outcome
	callErr, stop, err := r.failed(&o, readErr)
	switch {
	case err != nil:
		return out, err
	case stop:
		return out, fmt.Errorf("%w: connection %s stopped being used while its tickets were read", ErrDisabled, id)
	}
	if err := r.settleCalls(callErr); err != nil {
		return out, err
	}
	if o.Failure != nil {
		out.failure, out.retryAt = o.Failure, o.RetryAt
		return out, readErr
	}
	return out, s.ticketViews(r.root, reads)
}

// ticketViews builds the view of each of reads whose document changed, or
// that its pump is to hold and held none of (prior.held nil), named from
// root.
func (s *Service) ticketViews(root int64, reads []*ticketRead) error {
	for _, rd := range reads {
		heldNew := rd.check.held != nil && (rd.prior == nil || rd.prior.held == nil)
		if rd.err != nil || rd.detail == nil || !rd.changed && !heldNew {
			continue
		}
		v, err := s.detailView(root, *rd.detail)
		if err != nil {
			return err
		}
		rd.view, rd.digest, rd.shows = &v, s.digestOf(v), showsOf(*rd.detail)
	}
	return nil
}

// detailReadAtNext is when a detail read begins as the store orders the
// documents (StoreMirrorDetails): now, or a millisecond after the last such
// time when that is later, so two reads that begin in one millisecond are
// ordered as they began and the store keeps the later one's document.
func (s *Service) detailReadAtNext() int64 {
	for {
		last, now := s.detailReadAt.Load(), millis(s.now())
		next := max(now, last+1)
		if s.detailReadAt.CompareAndSwap(last, next) {
			return next
		}
	}
}

// showsOf are the other issues d's view shows from the mirror's cards:
// its parent and the issues it links.
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

// verifiedDetails sets each read's detail as the tracker says it is now.
// On a tracker whose update time moves with everything a detail shows
// (tracker.Capabilities.DetailMovesUpdated) a document the mirror or the
// pump holds is checked (checkDetails); every other ticket is read in full
// (details), all of them together. full reads every ticket in full.
func (r *run) verifiedDetails(reads []*ticketRead, full bool, openedAt int64) (Changes, error) {
	var out Changes
	toRead := reads
	if !full && r.p.Capabilities().DetailMovesUpdated {
		var err error
		if toRead, err = r.checkDetails(reads, openedAt, &out); err != nil || len(toRead) == 0 {
			return out, err
		}
	}
	err := r.againIfSwept(func() error {
		ch, err := r.details(toRead, openedAt)
		out.add(ch)
		return err
	})
	return out, err
}

// checkDetails checks reads against the tracker with one cards read of
// their own issues (a read a readBatch of them), and returns those to read
// in full: a ticket whose card's update time differs from its document's,
// whose document the mirror's card is already newer than, whose document's
// read began more than detailMaxAge ago, whose card the tracker did not
// return, and one with no document to check. The document checked is the
// pump's prior one when the mirror still holds it, named by one read of
// the stored details' states for every ticket with a prior (no decode),
// and else the mirror's, decoded. A person's opening (openedAt) is
// recorded on each stored document confirmed, and a document no longer
// held is read in full.
func (r *run) checkDetails(reads []*ticketRead, openedAt int64, out *Changes) ([]*ticketRead, error) {
	var withPrior []string
	for _, rd := range reads {
		if rd.prior != nil {
			withPrior = append(withPrior, string(rd.issue))
		}
	}
	var states []store.MirrorDetail
	if len(withPrior) > 0 {
		var err error
		if states, err = r.s.store.ListMirrorDetailStates(r.root, withPrior); err != nil {
			return nil, fmt.Errorf("trackerapp: read the details of %d issues from the mirror: %w", len(withPrior), err)
		}
	}
	now := millis(r.s.now())
	stored := make(map[tracker.IssueID]store.MirrorDetail, len(reads))
	var toRead, checked []*ticketRead
	for _, rd := range reads {
		var st store.MirrorDetail
		var ok bool
		if rd.prior != nil {
			if i := slices.IndexFunc(states, func(m store.MirrorDetail) bool { return m.IssueID == string(rd.issue) }); i >= 0 {
				st, ok = states[i], true
			}
		}
		st, ok, err := r.priorCheck(rd, st, ok)
		switch {
		case err != nil:
			return nil, err
		case ok:
			stored[rd.issue] = st
		}
		switch {
		case rd.check == (detailCheck{}), now-rd.check.readAt > detailMaxAge.Milliseconds(), ok && st.CardUpdatedAt > rd.check.updated:
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
		cards, ch, err := r.cardsNow(issues)
		out.add(ch)
		if err != nil {
			return nil, err
		}
		for _, rd := range batch {
			i := slices.IndexFunc(cards, func(c tracker.Card) bool { return c.ID == rd.issue })
			if i < 0 || millis(cards[i].Updated) != rd.check.updated {
				toRead = append(toRead, rd)
				continue
			}
			if st, ok := stored[rd.issue]; openedAt > 0 && ok && rd.check.held == nil {
				confirmed, err := r.s.store.ConfirmMirrorDetail(r.root, st, openedAt)
				switch {
				case err != nil:
					return nil, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", rd.issue, err)
				case !confirmed.Stored:
					toRead = append(toRead, rd)
					continue
				}
			}
			rd.changed = rd.prior == nil || rd.prior.doc != rd.check.doc
		}
	}
	return toRead, nil
}

// priorCheck sets rd's check to the document a check compares: the pump's
// prior one when it names the document the mirror holds (st, when stored)
// or is held by the pump while the mirror holds none, else the mirror's,
// decoded, which it returns as st. It sets no check when there is none.
func (r *run) priorCheck(rd *ticketRead, st store.MirrorDetail, stored bool) (store.MirrorDetail, bool, error) {
	switch p := rd.prior; {
	case p != nil && stored && p.held == nil && p.readAt == st.ReadAt, p != nil && !stored && p.held != nil:
		rd.check = *p
		return st, stored, nil
	case p != nil && !stored:
		return st, false, nil
	}
	m, ok, err := r.s.store.GetMirrorDetail(r.root, string(rd.issue))
	if err != nil {
		return st, false, fmt.Errorf("trackerapp: read the detail of %s from the mirror: %w", rd.issue, err)
	}
	if !ok {
		return st, false, nil
	}
	d, ok := detailOf(m)
	if !ok {
		return st, false, nil
	}
	rd.detail = &d
	rd.check = detailCheck{readAt: m.ReadAt, updated: millis(d.Card.Updated), doc: maphash.Bytes(r.s.digestSeed, m.Data)}
	return m, true, nil
}

// details reads the details of reads in full, together (Provider.Details),
// and stores each document the mirror keeps, read when the read began, in
// one write; a person's opening (openedAt set) is recorded as when this
// read began, the read that serves it. A document the mirror does not
// store, because it does not keep the issue or holds no opening of it, is
// held by the read's check. A ticket the tracker did not return fails with
// ErrIssueNotFound.
func (r *run) details(reads []*ticketRead, openedAt int64) (Changes, error) {
	readAt := r.s.detailReadAtNext()
	if openedAt > 0 {
		openedAt = readAt
	}
	ids := make([]tracker.IssueID, len(reads))
	for i, rd := range reads {
		ids[i] = rd.issue
	}
	r.calls++
	got, err := r.p.Details(r.ctx, ids)
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: read the details of %d issues: %w", len(ids), call(err))
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
	docs := make([]store.MirrorDetailDoc, 0, len(got.Details))
	read := make([]*ticketRead, 0, len(got.Details))
	for i := range got.Details {
		d := &got.Details[i]
		rd := readOf(d.Card.ID)
		if rd == nil || slices.Contains(read, rd) {
			return Changes{}, fmt.Errorf("trackerapp: the details of %d issues: %w", len(ids),
				call(fmt.Errorf("the tracker answered with issue %q", d.Card.ID)))
		}
		boundHistory(d)
		data, err := mirrorDetail(*d)
		if err != nil {
			return Changes{}, err
		}
		docs = append(docs, store.MirrorDetailDoc{IssueID: string(d.Card.ID), Data: data})
		read = append(read, rd)
		rd.err, rd.detail = nil, d
		rd.check = detailCheck{readAt: readAt, updated: millis(d.Card.Updated), doc: maphash.Bytes(r.s.digestSeed, data)}
	}
	for _, rd := range reads {
		if rd.err == nil && !slices.Contains(read, rd) {
			return Changes{}, fmt.Errorf("trackerapp: the details of %d issues: %w", len(ids),
				call(fmt.Errorf("the tracker neither returned nor left out issue %s", rd.issue)))
		}
	}
	var out Changes
	if len(docs) == 0 {
		return out, nil
	}
	written, err := r.s.store.StoreMirrorDetails(r.root, r.state.Sweep, docs, openedAt, readAt)
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: store the details of %d issues: %w", len(docs), err)
	}
	for i, w := range written {
		rd := read[i]
		if w.Changed {
			out.Details = append(out.Details, rd.issue)
			rd.stored = true
		}
		out.Dropped = unite(out.Dropped, issueIDs(w.Dropped))
		switch {
		case !w.Stored:
			rd.check.held = rd.detail
		case w.Superseded:
			// A read that began later stored another document: the next
			// read checks that one, and its writer tells the ticket.
			rd.check, rd.detail = detailCheck{}, nil
			continue
		}
		rd.changed = rd.prior == nil || rd.prior.doc != rd.check.doc
	}
	return out, nil
}
