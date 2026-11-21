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
//     and its rows whose parent's card changed or whose parent may have
//     started or stopped being kept (a row shows its parent's key and title
//     while the mirror keeps the parent, so a parent's row added to or
//     removed from any board moves it; a changed order or estimate of a
//     row the board held does not), from one indexed read of the issues'
//     boards made only when a board of the connection is subscribed;
//   - a change of the board's structure, or of the connection's users and
//     statuses (a name, a status's group, Changes.Catalog), moves rows no
//     ID names: the board is to be read again, as every board is when the
//     lookup failed;
//   - each open ticket is told what moved an input of its view
//     (ticketInputs): it is due a read at once, or rebuilds its view with
//     no tracker call.
//
// What changes the origin pump's own scope is left pending for that pump,
// which sends it with its failure in one event, and a read of tickets
// settles its own tickets (readTickets); every other subscription is sent
// its event or nudge here. root is the connection's mirror root the
// change was stored under, as the run that stored it held it, and gen its
// generation: only the pumps of that generation show that mirror, so only
// they are told (genLocked). A change stored under a mirror a turn off
// detached since reaches no subscription of the mirror made after it. ch
// is everything the run stored on connection conn, and own the changes of
// its reads through each board, whose rows, removed rows, cards and
// structure are that board's: a tick's own board, or every board a
// write's refresh read through.
func (s *Service) publish(root int64, gen uint64, conn string, ch Changes, own []boardChanges, origin *tickOrigin) {
	if ch.Empty() {
		return
	}
	cards := unite(slices.Clone(ch.Cards), ch.History)
	parents := unite(unite(slices.Clone(ch.Cards), ch.Added), ch.Removed)
	s.subMu.Lock()
	var boards []*boardPump
	var tickets []*ticketPump
	for sc, p := range s.subs.boards {
		if sc.Connection == conn && p.gen == gen {
			boards = append(boards, p)
		}
	}
	for k, tp := range s.subs.tickets {
		if k.connection == conn && tp.gen == gen {
			tickets = append(tickets, tp)
		}
	}
	s.subMu.Unlock()
	if len(boards)+len(tickets) == 0 {
		return
	}
	var rowsOf map[string][]tracker.IssueID
	lookupFailed := false
	if len(boards) > 0 && len(cards)+len(parents) > 0 {
		var err error
		if root == 0 {
			err = fmt.Errorf("no mirror root")
		} else {
			rowsOf, err = s.boardsOf(root, cards, parents)
		}
		if err != nil {
			s.logf("trackerapp: the boards of %d changed issues of %s: %v; their boards read again", len(parents), conn, err)
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
		if tp.over || tp.core.DeadLocked() || origin != nil && slices.Contains(origin.tickets, tp) {
			continue
		}
		switch read, rebuild := ticketInputs(tp, ch, false); {
		case read:
			s.readNowLocked(tp)
			fallthrough
		case rebuild:
			tp.inputs = s.nextStampLocked()
			nudge(tp.viewChanged)
		}
	}
}

// boardsOf is, by board, the issues each board of root holds a row of
// among ids (sorted) and among the children of parents (sorted), read
// lookupBatch issues of each list a statement.
func (s *Service) boardsOf(root int64, ids, parents []tracker.IssueID) (map[string][]tracker.IssueID, error) {
	out := map[string][]tracker.IssueID{}
	for i := 0; i < max(len(ids), len(parents)); i += lookupBatch {
		rows, err := s.store.ListMirrorIssueBoards(root, idStrings(window(ids, i)), idStrings(window(parents, i)))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.BoardID] = append(out[r.BoardID], tracker.IssueID(r.IssueID))
		}
	}
	return out, nil
}

// window is the lookupBatch issues of ids from i, none past its end.
func window(ids []tracker.IssueID, i int) []tracker.IssueID {
	return ids[min(i, len(ids)):min(i+lookupBatch, len(ids))]
}
