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

// ticketRead is one ticket of a read of several: its issue, the check its
// pump last settled (nil for none), whether it is read in full, and
// whether a stored card of its issue changed since that check settled
// (readNowLocked), then what the read found.
type ticketRead struct {
	issue  tracker.IssueID
	prior  *detailCheck
	full   bool
	nudged bool

	// check is the ticket's check after the read; detail is the document
	// when the read read or decoded it, nil when the check confirmed the
	// prior one; changed reports a document other than prior's. mirrored
	// is the mirror's copy the read decoded, as ConfirmMirrorDetail names
	// it.
	check    detailCheck
	detail   *tracker.Detail
	changed  bool
	mirrored *store.MirrorDetail
	// answered reports that the ticket's own read ended, whatever the
	// others' did: the tracker confirmed the document its check names, or
	// returned its document, stored or held, or left it out. A ticket not
	// answered takes how the read as a whole ended (detailsRead). A read
	// that returns no error answered every ticket.
	answered bool
	// err is this answered ticket's own failure: the tracker did not return
	// it (ErrIssueNotFound), or its view could not be built.
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
// they are now (verifiedDetails), settles the run's tracker calls as the
// connection's problem, and builds the view of each answered ticket whose
// document changed, or that its pump is to hold and held none of
// (prior.held nil), however the read ended unless the connection did.
// openedAt is when a person opened or refreshed them, zero for a read no
// person asked for. A tracker failure of the read
// is its failure and is returned too, as IssueDetail returns it; any other
// error is returned alone. Either is the outcome of the tickets the read
// did not answer. What it stored is in the result either way.
func (s *Service) readDetails(ctx context.Context, id string, reads []*ticketRead, u Urgency, openedAt int64) (detailsRead, error) {
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
		out.historyDue = r.state.HistoryDue
		out.ch, readErr = r.verifiedDetails(reads, openedAt)
	}
	out.root, out.gen, out.kind = r.root, r.gen, r.row.Kind
	var o Outcome
	callErr, stop, err := r.failed(&o, readErr)
	if stop && err == nil {
		return out, fmt.Errorf("%w: connection %s stopped being used while its tickets were read", ErrDisabled, id)
	}
	s.ticketViews(r.root, reads)
	if err != nil {
		return out, err
	}
	if err := r.settleCalls(callErr); err != nil {
		return out, err
	}
	out.failure, out.retryAt = o.Failure, o.RetryAt
	return out, readErr
}

// ticketViews builds the view of each answered read of reads whose
// document changed, or that its pump is to hold and held none of
// (prior.held nil), named from root, the views together (detailViews). A
// build that fails is the failure of each read it was to build.
func (s *Service) ticketViews(root int64, reads []*ticketRead) {
	var built []*ticketRead
	var ds []tracker.Detail
	for _, rd := range reads {
		heldNew := rd.check.held != nil && (rd.prior == nil || rd.prior.held == nil)
		if !rd.answered || rd.err != nil || rd.detail == nil || !rd.changed && !heldNew {
			continue
		}
		built, ds = append(built, rd), append(ds, *rd.detail)
	}
	if len(ds) == 0 {
		return
	}
	views, err := s.detailViews(root, ds)
	for i, rd := range built {
		if err != nil {
			rd.err = fmt.Errorf("trackerapp: build the view of %s: %w", rd.issue, err)
			continue
		}
		rd.view, rd.digest, rd.shows = &views[i], s.digestOf(views[i]), showsOf(ds[i])
	}
}

// readAtNext is when a read of the tracker begins, as the store orders
// what it stores (a card, a row, a document, a listing): now, or a
// millisecond after the last such time when that is later, so two reads
// that begin in one millisecond are ordered as they began. It is the one
// source of a read's begin time (run.readAt): the store's card rule
// (store mirrorCardSupersededSQL) compares the begin times of reads made
// through every path, so one path on another clock could order an older
// answer after a newer one.
func (s *Service) readAtNext() int64 {
	for {
		last, now := s.readAtLast.Load(), millis(s.now())
		next := max(now, last+1)
		if s.readAtLast.CompareAndSwap(last, next) {
			return next
		}
	}
}

// payOwedCards reads the cards the documents of written owe (docs, read
// from readAt, in written's order) and stores those documents again,
// replacing their outcomes in written and adding what the cards read
// stored to out. A document still owed after it, because the tracker
// returned no card of the issue or an older one, keeps its outcome:
// not stored. The reads run under the run's sweep as the documents' read
// did; a sweep between them fails the whole read (againIfSwept reads the
// documents again).
func (r *run) payOwedCards(docs []store.MirrorDetailDoc, openedAt, readAt int64, written *store.MirrorDetailWrites, out *Changes) error {
	var owed []int
	var issues []tracker.IssueID
	for i, w := range written.Docs {
		if w.CardOwed {
			owed = append(owed, i)
			issues = append(issues, tracker.IssueID(docs[i].IssueID))
		}
	}
	if len(owed) == 0 {
		return nil
	}
	for batch := range slices.Chunk(issues, readBatch) {
		_, ch, err := r.cardsOf(batch)
		out.add(ch)
		if err != nil {
			return err
		}
	}
	again := make([]store.MirrorDetailDoc, len(owed))
	for j, i := range owed {
		again[j] = docs[i]
	}
	paid, err := r.s.store.StoreMirrorDetails(r.root, r.state.Sweep, again, openedAt, readAt)
	if err != nil {
		return fmt.Errorf("trackerapp: store the details of %d issues after their cards: %w", len(again), err)
	}
	for j, i := range owed {
		written.Docs[i] = paid.Docs[j]
	}
	written.Dropped = append(written.Dropped, paid.Dropped...)
	return nil
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
// pump holds is checked (checkDetails); every other ticket, and one asked
// in full, is read in full (details), all of them together.
func (r *run) verifiedDetails(reads []*ticketRead, openedAt int64) (Changes, error) {
	var out Changes
	toRead := reads
	if r.p.Capabilities().DetailMovesUpdated {
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
// in full: one asked in full, one with no document to check, whose
// document's read began more than detailMaxAge ago or whose card the
// mirror holds newer (priorCheck, mirrorMoved), and one whose card's
// update time differs from its document's or that the tracker did not
// return. The document checked is the pump's prior one, else the
// mirror's, decoded. A document is
// confirmed only once the tracker answered (confirmHolders): one no longer
// held where its check says is read in full, so a successful read always
// leaves the ticket's content with the mirror or the pump.
func (r *run) checkDetails(reads []*ticketRead, openedAt int64, out *Changes) ([]*ticketRead, error) {
	now := millis(r.s.now())
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
		switch {
		case rd.full, rd.check == (detailCheck{}), now-rd.check.readAt > detailMaxAge.Milliseconds():
			toRead = append(toRead, rd)
		default:
			checked = append(checked, rd)
		}
	}
	var current []*ticketRead
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
			if i := slices.IndexFunc(cards, func(c tracker.Card) bool { return c.ID == rd.issue }); i < 0 || millis(cards[i].Updated) != rd.check.updated {
				toRead = append(toRead, rd)
				continue
			}
			current = append(current, rd)
		}
	}
	gone, err := r.confirmHolders(current, openedAt)
	return append(toRead, gone...), err
}

// confirmHolders confirms, after the tracker answered, that the document
// each of reads checked is held where its check says: by the pump while
// the mirror holds none, or by the mirror as the same document. A person's
// opening (openedAt) is recorded on each stored document confirmed with
// the copy the read decoded (ConfirmMirrorDetail, which confirms it); the
// others are confirmed by one read of the stored details' states. It
// returns the reads whose document is no longer held so (evicted, swept,
// replaced, or the mirror holds one now beside the pump's), to read in
// full.
func (r *run) confirmHolders(reads []*ticketRead, openedAt int64) ([]*ticketRead, error) {
	var listed []string
	for _, rd := range reads {
		if openedAt == 0 || rd.mirrored == nil {
			listed = append(listed, string(rd.issue))
		}
	}
	var states []store.MirrorDetail
	if len(listed) > 0 {
		var err error
		if states, err = r.s.store.ListMirrorDetailStates(r.root, listed); err != nil {
			return nil, fmt.Errorf("trackerapp: read the details of %d issues from the mirror: %w", len(listed), err)
		}
	}
	var gone []*ticketRead
	for _, rd := range reads {
		st := rd.mirrored
		if openedAt == 0 || st == nil {
			st = nil
			if i := slices.IndexFunc(states, func(m store.MirrorDetail) bool { return m.IssueID == string(rd.issue) }); i >= 0 {
				st = &states[i]
			}
		}
		switch {
		case rd.check.held != nil && st == nil:
		case rd.check.held != nil || st == nil || st.ReadAt != rd.check.readAt:
			gone = append(gone, rd)
			continue
		case openedAt > 0:
			confirmed, err := r.s.store.ConfirmMirrorDetail(r.root, *st, openedAt)
			switch {
			case err != nil:
				return nil, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", rd.issue, err)
			case !confirmed.Stored:
				gone = append(gone, rd)
				continue
			}
		}
		rd.changed = rd.prior == nil || rd.prior.doc != rd.check.doc
		rd.answered = true
	}
	return gone, nil
}

// priorCheck sets rd's check to the document a check compares: the pump's
// prior one, else the mirror's, decoded (rd.mirrored), read in full when
// the mirror's card of the issue is already newer than it. It sets no
// check when there is none.
func (r *run) priorCheck(rd *ticketRead) error {
	if rd.prior != nil {
		rd.check = *rd.prior
		return nil
	}
	m, ok, err := r.s.store.GetMirrorDetail(r.root, string(rd.issue))
	if err != nil {
		return fmt.Errorf("trackerapp: read the detail of %s from the mirror: %w", rd.issue, err)
	}
	if !ok {
		return nil
	}
	d, ok := detailOf(m)
	if !ok {
		return nil
	}
	rd.detail, rd.mirrored = &d, &m
	rd.check = detailCheck{readAt: m.ReadAt, updated: millis(d.Card.Updated), doc: maphash.Bytes(r.s.digestSeed, m.Data)}
	rd.full = m.CardUpdatedAt > rd.check.updated
	return nil
}

// mirrorMoved reads in full each of reads checked against its pump's prior
// document whose stored card changed since (ticketRead.nudged) and is
// newer than that document, as priorCheck does for the mirror's copy: the
// read that stored the card is the evidence, so no cards read is asked
// for it. One whose stored card has the document's update time (a rename
// the board read) goes on to the check. One read of the mirror's cards
// serves them all; a read with no such ticket reads nothing.
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
// and stores each document the mirror keeps, read when the read began, in
// one write; a person's opening (openedAt set) is recorded as when this
// read began, the read that serves it. A document whose card the mirror
// holds older, or none of, is owed its card (store
// MirrorDetailWrite.CardOwed): one cards read of the owed issues stores
// their cards through the one card path (cardsOf), and one more write
// stores their documents. A document the mirror does not store, because
// it does not keep the issue, holds no opening of it, holds a later card
// (the document is stale: the next read reads it again), or the cards
// read returned no card of it, is held by the read's check. A ticket the
// tracker did not return fails with ErrIssueNotFound.
func (r *run) details(reads []*ticketRead, openedAt int64) (Changes, error) {
	readAt := r.readAt()
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
		docs = append(docs, store.MirrorDetailDoc{IssueID: string(d.Card.ID), Data: data, CardUpdatedAt: millis(d.Card.Updated)})
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
	// A ticket the tracker left out is answered whether or not the others
	// are stored.
	for _, rd := range reads {
		rd.answered = rd.err != nil
	}
	var out Changes
	if len(docs) == 0 {
		return out, nil
	}
	written, err := r.s.store.StoreMirrorDetails(r.root, r.state.Sweep, docs, openedAt, readAt)
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: store the details of %d issues: %w", len(docs), err)
	}
	if err := r.payOwedCards(docs, openedAt, readAt, &written, &out); err != nil {
		return out, err
	}
	out.Dropped = unite(out.Dropped, issueIDs(written.Dropped))
	for i, w := range written.Docs {
		rd := read[i]
		rd.answered = true
		if w.Changed {
			out.Details = append(out.Details, rd.issue)
		}
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
