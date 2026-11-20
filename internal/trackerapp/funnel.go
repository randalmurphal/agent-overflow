package trackerapp

import (
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
)

// lookupBatch is the most issues one read of their boards names (store
// ListMirrorIssueBoards).
const lookupBatch = 500

// boardChanges are what a run's reads through one board stored: the
// board's rows and removed rows, the cards those rows carried, and its
// structure.
type boardChanges struct {
	board tracker.BoardID
	ch    Changes
}

// ownOf is the changes of a tick of scope as its own board's: none for a
// scope with no board.
func ownOf(scope Scope, ch Changes) []boardChanges {
	if scope.Board == "" {
		return nil
	}
	return []boardChanges{{board: scope.Board, ch: ch}}
}

// publish is the change funnel: every tick's changes (tick.go) and every
// read of open tickets pass through it, and it turns them into what each
// subscription of the connection needs, under subMu:
//
//   - the board of the change's own scope gets its changed rows, cards and
//     removed rows: a board tick's cards are rows of its board;
//   - every subscribed board of the connection gets its rows of the
//     changed cards and histories (a history moves a card's status time)
//     and its rows whose parent is among them (a row shows its parent's
//     key and title), from one indexed read of the issues' boards made
//     only when a board of the connection is subscribed;
//   - a change of the board's structure, or of the connection's users and
//     statuses (a name, a status's group, Changes.Catalog), moves rows no
//     ID names: the board is to be read again, as every board is when the
//     lookup failed;
//   - an open ticket of a changed card reads at once (its pump's nudge);
//     one whose stored detail another read changed, whose view shows a
//     changed card (its parent, an issue it links), or whose names may
//     have changed (Catalog) rebuilds its view with no tracker call.
//
// What changes the origin pump's own scope is left pending for that pump,
// which sends it with its failure in one event, and a read of tickets
// settles its own tickets' cards and details; every other subscription is
// sent its event or nudge here. root is the connection's mirror root the
// change was stored under, as the run that stored it held it. ch is
// everything the run stored on connection conn, and own the changes of
// its reads through each board, whose rows, removed rows, cards and
// structure are that board's: a tick's own board, or every board a
// write's refresh read through.
func (s *Service) publish(root int64, conn string, ch Changes, own []boardChanges, origin *tickOrigin) {
	if ch.Empty() {
		return
	}
	cards := unite(slices.Clone(ch.Cards), ch.History)
	s.subMu.Lock()
	var boards []*boardPump
	var tickets []*ticketPump
	for sc, p := range s.subs.boards {
		if sc.Connection == conn {
			boards = append(boards, p)
		}
	}
	for k, tp := range s.subs.tickets {
		if k.connection == conn {
			tickets = append(tickets, tp)
		}
	}
	s.subMu.Unlock()
	if len(boards)+len(tickets) == 0 {
		return
	}
	var rowsOf map[string][]tracker.IssueID
	lookupFailed := false
	if len(boards) > 0 && len(cards) > 0 {
		var err error
		if root == 0 {
			err = fmt.Errorf("no mirror root")
		} else {
			rowsOf, err = s.boardsOf(root, cards)
		}
		if err != nil {
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
		readAgain := ch.Catalog || lookupFailed
		if i := slices.IndexFunc(own, func(b boardChanges) bool { return b.board == p.scope.Board }); i >= 0 {
			o := own[i].ch
			changed = unite(unite(slices.Clone(o.Rows), o.Cards), changed)
			removed = o.Removed
			readAgain = readAgain || o.Structure
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
		issue := tp.key.issue
		read := origin != nil && slices.Contains(origin.tickets, tp)
		switch {
		case !read && slices.Contains(ch.Cards, issue):
			nudge(tp.cardChanged)
		case !read && slices.Contains(ch.Details, issue), ch.Catalog,
			slices.ContainsFunc(tp.shows, func(id tracker.IssueID) bool { return slices.Contains(ch.Cards, id) }):
			nudge(tp.viewChanged)
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
