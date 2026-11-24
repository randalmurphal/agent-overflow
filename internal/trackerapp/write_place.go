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
// those the mirror shows, and returns its row on each that does, read so
// that it is the row the board's own list will deliver. A tracker's list
// may show a new issue late (Jira's search index); the placement does not
// wait for it, so the row is on the board when the write returns. A
// create's board the mirror does not show is not asked and its row is not
// shown; the other boards are asked all the same. With no board to ask,
// nothing is read.
//
//   - A board that holds it: the row is stored and told as that board's
//     own change. On the create's board it is accounted refreshed, unless
//     the store did not take it (a removal on the board by a read that
//     began later, or a failure of that board's write); then it is owed
//     the board's rows read, as any other row. A failure of one board's
//     write leaves the other boards' rows stored.
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
// create's first when it is one, and stores its rows; with no board it
// asks nothing. It reports whether that settled the row of the create's
// board (stored, not on the board, or the board retired), which otherwise
// is owed the rows read when the board is shown, and returns why the
// refresh stops, nil to go on. A store failure is its board's alone: the
// next board's row is still stored.
func (r *run) place(f *refreshed, n newIssue, boards []tracker.BoardID) (settled bool, stop error) {
	if len(boards) == 0 {
		return false, nil
	}
	read, err := r.beginRead()
	if err != nil {
		if errors.Is(err, store.ErrTrackerCacheGone) {
			f.ended = true
		}
		return false, err
	}
	r.calls++
	pl, err := r.p.NewRows(r.ctx, n.issue, boards)
	if err != nil {
		// Nothing was judged; the create's board reads its row as any
		// write's would, and that read settles the connection's problem.
		if r.ctx.Err() != nil {
			return false, r.failedRead(f, call(err), tracker.RowOf(n.board, n.issue))
		}
		r.s.logf("trackerapp: place the new issue %s on boards %v: %v", n.issue, boards, err)
		return false, nil
	}
	if slices.Contains(pl.Off, n.board) {
		f.covered(tracker.RowOf(n.board, n.issue))
		f.notOnBoard = n.board
		settled = true
	}
	for _, br := range pl.Rows {
		taken, err := r.storePlaced(f, br, pl.Catalog, read)
		switch {
		case errors.Is(err, store.ErrTrackerCacheGone):
			f.ended = true
			return false, err
		case err != nil && !retired(err):
			// The row stays owed: the create's board's to its rows read,
			// another board's to its own sync.
			r.s.logf("trackerapp: store the new issue %s's row on board %s: %v", n.issue, br.Board, err)
		case br.Board == n.board && (taken || retired(err)):
			f.covered(tracker.RowOf(n.board, n.issue))
			settled = true
		}
	}
	return settled, nil
}

// storePlaced stores br, read at read, and reports whether the store took
// the row: false when it kept a row a later read stored or refused a new
// row from a read that began before the board last lost rows.
func (r *run) storePlaced(f *refreshed, br tracker.BoardRow, cat tracker.Catalog, read store.MirrorRead) (bool, error) {
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
	r.told(br.Board, changesOf(stored))
	f.ch.add(changesOf(stored))
	return !slices.Contains(stored.Superseded, string(br.Row.Card.ID)), nil
}
