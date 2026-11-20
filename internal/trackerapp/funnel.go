package trackerapp

import (
	"slices"

	"agent-overflow/internal/tracker"
)

// lookupBatch is the most issues one read of their boards names (store
// ListMirrorIssueBoards).
const lookupBatch = 500

// publish is the change funnel: every tick's changes (tick.go) and every
// open ticket's read pass through it, and it turns them into what each
// subscription of the connection needs, under subMu:
//
//   - the board of the change's own scope gets its changed rows, cards and
//     removed rows: a board tick's cards are rows of its board;
//   - every other subscribed board of the connection gets the changed
//     cards and histories (a history moves a card's status time) of the
//     issues it has a row of, from one indexed read of the issues' boards
//     made only when such a board is subscribed;
//   - a change of the board's structure, or of the connection's users and
//     statuses (a name, a status's group), moves rows no ID names: the
//     board is to be read again, as it is when the lookup failed;
//   - an open ticket of a changed card reads at once (its pump's nudge); one
//     whose stored detail another read changed is told so.
//
// What changes the origin pump's own scope is left pending for that pump,
// which sends it with its failure in one event; every other subscription
// is sent its event here. root is the connection's mirror root the change
// was stored under.
func (s *Service) publish(root int64, scope Scope, ch Changes, origin *tickOrigin) {
	if ch.Empty() {
		return
	}
	conn := scope.Connection
	cards := unite(slices.Clone(ch.Cards), ch.History)
	s.subMu.Lock()
	var boards []*boardPump
	var tickets []*ticketPump
	lookup := false
	for sc, p := range s.subs.boards {
		if sc.Connection != conn {
			continue
		}
		boards = append(boards, p)
		lookup = lookup || (sc != scope && len(cards) > 0) || (sc == scope && len(ch.History) > 0)
	}
	for k, tp := range s.subs.tickets {
		if k.connection == conn && (origin == nil || origin.ticket != tp) {
			tickets = append(tickets, tp)
		}
	}
	s.subMu.Unlock()
	if len(boards)+len(tickets) == 0 {
		return
	}
	var rowsOf map[string][]tracker.IssueID
	lookupFailed := false
	if lookup && root != 0 {
		var err error
		if rowsOf, err = s.boardsOf(root, cards); err != nil {
			s.logf("trackerapp: the boards of %d changed issues of %s: %v; their boards read again", len(cards), conn, err)
			lookupFailed = true
		}
	}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, p := range boards {
		if p.over || p.core.DeadLocked() {
			continue
		}
		changed := rowsOf[string(p.scope.Board)]
		var removed []tracker.IssueID
		readAgain := ch.Catalog || (lookupFailed && (p.scope != scope || len(ch.History) > 0))
		if p.scope == scope {
			changed = unite(unite(slices.Clone(ch.Rows), ch.Cards), changed)
			removed = ch.Removed
			readAgain = readAgain || ch.Structure
		}
		p.pending.add(changed, removed, readAgain)
		if origin == nil || origin.board != p {
			s.flushBoardLocked(p)
		}
	}
	for _, tp := range tickets {
		if tp.over || tp.core.DeadLocked() {
			continue
		}
		switch issue := tp.key.issue; {
		case slices.Contains(ch.Cards, issue):
			select {
			case tp.cardChanged <- struct{}{}:
			default:
			}
		case slices.Contains(ch.Details, issue):
			// Another read stored the detail: the mirror's copy is the view.
			tp.held, tp.changed, tp.due = nil, true, true
			s.flushTicketLocked(tp, false)
		}
	}
}

// boardsOf is the issues of ids (sorted) that each board of root holds a
// row of, by board, read lookupBatch issues a statement.
func (s *Service) boardsOf(root int64, ids []tracker.IssueID) (map[string][]tracker.IssueID, error) {
	out := map[string][]tracker.IssueID{}
	for batch := range slices.Chunk(ids, lookupBatch) {
		rows, err := s.store.ListMirrorIssueBoards(root, idStrings(batch))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.BoardID] = append(out[r.BoardID], tracker.IssueID(r.IssueID))
		}
	}
	return out, nil
}
