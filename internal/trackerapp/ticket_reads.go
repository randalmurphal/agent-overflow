package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// Reads of several tickets of one connection at once, for a caller that
// plans writes on them (the tracker tools): each ticket's answer, or the
// failure of its own read, in the order asked, and the read's failure as
// a whole, which no ticket answers.

// TicketRead is one ticket of UnmarkedIssueDetails: its view, or why it
// could not be read.
type TicketRead struct {
	View DetailView
	Err  error
}

// UnmarkedIssueDetails are issues' details on connection id, each as
// UnmarkedIssueDetail reads it, read together (readDetails): one cards
// read checks the bodies the mirror holds, a readBatch at a time, and one
// Details read reads the rest. A ticket the tracker did not return fails
// wrapping ErrIssueNotFound. A full read's issue, as its provider read it,
// is kept in ctx's write session for the first write to it
// (write_session.go).
func (s *Service) UnmarkedIssueDetails(ctx context.Context, id string, issues []tracker.IssueID, u Urgency) ([]TicketRead, error) {
	reads, err := ticketReadsOf(issues)
	if err != nil {
		return nil, err
	}
	res, err := s.readDetails(ctx, id, reads, u, true)
	if err != nil {
		return nil, err
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	row, err := s.viewRow(id)
	if err != nil {
		return nil, err
	}
	sess := sessionOf(ctx)
	out := make([]TicketRead, len(reads))
	var docs []ticketDoc
	var at []int
	for i, rd := range reads {
		if rd.err != nil {
			out[i].Err = fmt.Errorf("%w: issue %s of %s", rd.err, rd.issue, id)
			continue
		}
		sess.keepRead(id, rd.detail.d)
		docs, at = append(docs, *rd.detail), append(at, i)
	}
	views, _, err := s.detailViews(res.root, row, docs, false)
	if err != nil {
		return nil, err
	}
	for k, i := range at {
		out[i].View = views[k]
	}
	return out, nil
}

// ticketReadsOf are the reads of issues, refused when one is empty or
// named twice.
func ticketReadsOf(issues []tracker.IssueID) ([]*ticketRead, error) {
	reads := make([]*ticketRead, len(issues))
	for i, issue := range issues {
		if issue == "" || slices.Contains(issues[:i], issue) {
			return nil, fmt.Errorf("%w: issue %q is empty or named twice", ErrInvalidRequest, issue)
		}
		reads[i] = &ticketRead{issue: issue}
	}
	return reads, nil
}

// TicketCardView is a ticket's card as the mirror holds it after a read
// of it, with its rows on the boards the mirror shows, in board order.
type TicketCardView struct {
	Card CardView
	Rows []TicketRowView
}

// TicketCardRead is one ticket of IssueCards: its card, or why it could
// not be read.
type TicketCardRead struct {
	View TicketCardView
	Err  error
}

// IssueCards reads the cards of issues on connection id as the tracker
// says they are now, a readBatch a read, stores those of the issues the
// mirror keeps as any cards read does, and answers each as the mirror
// then holds it, with its rows (cardReads). A tracker failure of the read
// is settled as the connection's problem and returned.
func (s *Service) IssueCards(ctx context.Context, id string, issues []tracker.IssueID, u Urgency) ([]TicketCardRead, error) {
	if _, err := ticketReadsOf(issues); err != nil {
		return nil, err
	}
	root, missing, err := s.readCards(ctx, id, u, func(r *run) ([]tracker.IssueID, error) {
		_, missing, _, err := r.cards(issues)
		return missing, err
	})
	if err != nil {
		return nil, err
	}
	return s.cardReads(id, root, issues, missing)
}

// IssueRow is an issue and the board IssueRows reads its row on.
type IssueRow struct {
	Issue tracker.IssueID
	Board tracker.BoardID
}

// IssueRows reads each issue's row on its board, on connection id, one
// rows read per board for all its issues, as a write's refresh reads a
// row again (run.refresh), and answers each as IssueCards does: its card,
// which the row's read stored, and its rows, the one read now and the
// others as the mirror holds them. An issue its board did not return a
// row of fails wrapping ErrNotOnBoard, whether the mirror still keeps its
// card for another board or keeps it no longer: no other board is read
// for it. Nothing else is read: a board the issue
// is on that rows does not name is not asked, nor a card by itself. A
// tracker failure of any of the reads is settled as the connection's
// problem and returned.
func (s *Service) IssueRows(ctx context.Context, id string, rows []IssueRow, u Urgency) ([]TicketCardRead, error) {
	issues := make([]tracker.IssueID, len(rows))
	resources := make([]tracker.Resource, len(rows))
	for i, row := range rows {
		if row.Board == "" {
			return nil, fmt.Errorf("%w: issue %q names no board", ErrInvalidRequest, row.Issue)
		}
		issues[i], resources[i] = row.Issue, tracker.RowOf(row.Board, row.Issue)
	}
	if _, err := ticketReadsOf(issues); err != nil {
		return nil, err
	}
	root, _, err := s.readCards(ctx, id, u, func(r *run) ([]tracker.IssueID, error) {
		return nil, r.refresh(resources, newIssue{}).readErr()
	})
	if err != nil {
		return nil, err
	}
	out, err := s.cardReads(id, root, issues, nil)
	if err != nil {
		return nil, err
	}
	for i, row := range rows {
		// A card the mirror no longer keeps after its row's read lost the
		// row that kept it: no shown board lists it now.
		gone := errors.Is(out[i].Err, ErrNotMirrored)
		if gone || out[i].Err == nil && !slices.ContainsFunc(out[i].View.Rows, func(v TicketRowView) bool { return v.Board == string(row.Board) }) {
			out[i] = TicketCardRead{Err: fmt.Errorf("%w: issue %s of %s on board %s", ErrNotOnBoard, row.Issue, id, row.Board)}
		}
	}
	return out, nil
}

// cardReads are issues as the mirror of root holds them after a read of
// them that did not return missing, each with its rows. A ticket the
// tracker did not return fails wrapping ErrIssueNotFound, and one the
// mirror does not keep wrapping ErrNotMirrored: no view of it names its
// users and statuses.
func (s *Service) cardReads(id string, root int64, issues, missing []tracker.IssueID) ([]TicketCardRead, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	ids := idStrings(issues)
	held, err := s.store.ListMirrorCards(root, ids)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: read %d cards from the mirror: %w", len(ids), err)
	}
	var shown []shownCard
	var at []int
	out := make([]TicketCardRead, len(issues))
	for i, issue := range issues {
		j := slices.IndexFunc(held, func(m store.MirrorCard) bool { return m.IssueID == string(issue) })
		switch {
		case slices.Contains(missing, issue):
			out[i].Err = fmt.Errorf("%w: issue %s of %s", ErrIssueNotFound, issue, id)
		case j < 0:
			out[i].Err = fmt.Errorf("%w: issue %s of %s", ErrNotMirrored, issue, id)
		default:
			c, _ := shownOf(held[j])
			shown, at = append(shown, c), append(at, i)
		}
	}
	if len(shown) == 0 {
		return out, nil
	}
	views, _, err := s.cardViews(root, shown, nil)
	if err != nil {
		return nil, err
	}
	rows, err := s.issueRows(root, ids, nil)
	if err != nil {
		return nil, err
	}
	for k, i := range at {
		r := rows[issues[i]]
		if r == nil {
			r = []TicketRowView{}
		}
		out[i].View = TicketCardView{Card: views[k], Rows: r}
	}
	return out, nil
}

// readCards runs read, a read of cards on connection id that answers the
// issues the tracker did not return, and settles the run's tracker calls
// as the connection's problem, as readDetails does. It returns the mirror
// root they were stored under and the issues the tracker did not return.
func (s *Service) readCards(ctx context.Context, id string, u Urgency, read func(*run) ([]tracker.IssueID, error)) (int64, []tracker.IssueID, error) {
	r, err := s.start(ctx, id, u)
	if err != nil {
		return 0, nil, err
	}
	defer r.finish()
	r.scope = Scope{Connection: id}
	readErr := r.startErr
	if readErr == nil {
		readErr = r.readState()
	}
	var missing []tracker.IssueID
	if readErr == nil {
		missing, readErr = read(r)
	}
	var o Outcome
	callErr, stop, err := r.failed(&o, readErr)
	if stop && err == nil {
		return 0, nil, fmt.Errorf("%w: connection %s stopped being used while its tickets were read", ErrDisabled, id)
	}
	if err != nil {
		return 0, nil, err
	}
	if err := r.settleCalls(callErr); err != nil {
		return 0, nil, err
	}
	return r.root, missing, readErr
}
