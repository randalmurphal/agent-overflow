package trackerapp

import (
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A create's refresh places the new issue before it reads anything else
// (tracker.Provider.NewRows): one read asks the tracker which of the
// create's board and the connection's other subscribed boards hold it,
// and returns its row on each that does, read so that it is the row the
// board's own list will deliver. A tracker's list may show a new issue
// late (Jira's search index); the placement does not wait for it, so the
// row is on the board when the write returns.
//
//   - A board that holds it: the row is stored and told as that board's
//     own change. On the create's board it is accounted refreshed, unless
//     the store did not take it (a removal on the board by a read that
//     began later); then it is owed the board's rows read, as any other
//     row.
//   - The create's board does not hold it: nothing is stored, the row is
//     not shown, and the write answers WriteView.NotOnBoard.
//   - The create's board is not judged (the read failed, or the tracker
//     could not judge that board): the row is owed the board's rows read,
//     as any write's row is.
//
// Another board that does not hold the issue, or was not judged, is left
// to its own sync.

// newIssue is the issue a create made and the board it was made from;
// zero for any other write.
type newIssue struct {
	issue tracker.IssueID
	board tracker.BoardID
}

// placeBoards are the boards a create's placement asks about: the
// create's board first, then the connection's other subscribed boards the
// mirror shows, in ID order.
func (r *run) placeBoards(n newIssue) []tracker.BoardID {
	r.s.subMu.Lock()
	var others []tracker.BoardID
	for scope, p := range r.s.subs.boards {
		if scope.Connection == r.row.ID && scope.Board != n.board && !p.over {
			others = append(others, scope.Board)
		}
	}
	r.s.subMu.Unlock()
	slices.Sort(others)
	return append([]tracker.BoardID{n.board}, others...)
}

// place places n on boards, the shown ones of placeBoards with the
// create's first, and stores its rows. It reports whether the create's
// board still owes its row the rows read, and returns why the refresh
// stops, nil to go on.
func (r *run) place(f *refreshed, n newIssue, boards []tracker.BoardID) (owed bool, stop error) {
	read := r.readAt()
	r.calls++
	pl, err := r.p.NewRows(r.ctx, n.issue, boards)
	if err != nil {
		// Nothing was judged; the create's board reads its row as any
		// write's would, and that read settles the connection's problem.
		if r.ctx.Err() != nil {
			return false, r.failedRead(f, call(err), tracker.RowOf(n.board, n.issue))
		}
		r.s.logf("trackerapp: place the new issue %s on %d boards: %v; its row is read through board %s", n.issue, len(boards), err, n.board)
		return true, nil
	}
	owed = true
	if slices.Contains(pl.Off, n.board) {
		f.covered(tracker.RowOf(n.board, n.issue))
		f.notOnBoard = n.board
		owed = false
	}
	for _, br := range pl.Rows {
		taken, err := r.storePlaced(f, br, pl.Catalog, read)
		switch {
		case errors.Is(err, store.ErrTrackerCacheGone):
			f.ended = true
			return false, err
		case err != nil && br.Board == n.board && !retired(err):
			return false, r.failedRead(f, err, tracker.RowOf(n.board, n.issue))
		case br.Board == n.board && (taken || retired(err)):
			f.covered(tracker.RowOf(n.board, n.issue))
			owed = false
		case err != nil:
			// Another board's row is its own sync's to bring.
			r.s.logf("trackerapp: store the new issue %s's row on board %s: %v", n.issue, br.Board, err)
		}
	}
	return owed, nil
}

// storePlaced stores br, read at read, and reports whether the store took
// the row: false when it kept a row a later read stored or refused a new
// row from a read that began before the board last lost rows.
func (r *run) storePlaced(f *refreshed, br tracker.BoardRow, cat tracker.Catalog, read int64) (bool, error) {
	lifetime, err := r.lifetime(br.Board)
	if err != nil {
		return false, err
	}
	page, err := mirrorRows(br.Board, []tracker.Row{br.Row}, nil, cat, read)
	if err != nil {
		return false, err
	}
	page.Lifetime = lifetime
	stored, err := r.s.store.WriteMirrorRows(r.root, page)
	if err != nil {
		return false, fmt.Errorf("trackerapp: store the new issue's row on board %s: %w", br.Board, err)
	}
	r.markStructureDue(stored)
	f.add(br.Board, changesOf(stored))
	return !slices.Contains(stored.Superseded, string(br.Row.Card.ID)), nil
}
