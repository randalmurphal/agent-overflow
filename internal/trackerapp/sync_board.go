package trackerapp

import (
	"bytes"
	"errors"
	"fmt"

	"agent-overflow/internal/tracker"
)

// Bounds of one unit in a tick. A unit that reaches its bound stops with
// UnitResult.More; the next tick that names it continues where the mirror
// says.
const (
	// deltaPages is the most pages of changed rows one delta reads.
	deltaPages = 10
	// reconcileBatches is the most batches of missing rows one reconcile
	// reads, readBatch issues each.
	reconcileBatches = 10
)

// delta is the delta unit of board (SyncBoard), from the cursor of the
// sync state the tick read at its start, or from the start of the window
// when a structure the run stored changed the board's estimate
// definition. A page with no rows still stores the cursor it returned,
// which moves the next walk's start; a page with no rows that returned the
// cursor it started from is not written: the read the tick records admits
// that cursor. Each page names the estimate definition the walk began
// under; a page whose cursor the store refuses, because the definition
// changed since or a removal refused a new row of it, ends the unit with
// More, and the next tick walks from the store's state.
func (r *run) delta(board tracker.BoardID) (UnitResult, error) {
	var log changeLog
	more, err := r.deltaPages(board, &log)
	r.walking = more
	out := UnitResult{More: more}
	if err == nil {
		err = r.cascade(board, &out)
	}
	log.add(out.Changes)
	out.Changes = log.changes()
	return out, err
}

// deltaPages reads and stores the pages of the delta unit, adding what
// each stored to log, and reports whether the walk has more to read.
func (r *run) deltaPages(board tracker.BoardID, log *changeLog) (more bool, _ error) {
	ch, err := r.boardHeld(board)
	log.add(ch)
	if err != nil {
		return false, err
	}
	lifetime, err := r.lifetime(board)
	if err != nil {
		return false, err
	}
	var cursor tracker.Cursor
	if len(r.state.Cursor) > 0 {
		cursor = r.state.Cursor
	}
	for pages := 0; ; pages++ {
		if pages == deltaPages {
			return true, nil
		}
		q := tracker.DeltaQuery{Board: board, From: cursor, Start: r.window()}
		read := r.readAt()
		r.calls++
		page, err := r.p.ChangedRows(r.ctx, q)
		if err != nil && pages == 0 && cursor != nil && errors.Is(err, tracker.ErrInvalidInput) {
			// A cursor of an encoding the provider no longer reads: the walk
			// starts over from the history window.
			q.From, cursor = nil, nil
			read = r.readAt()
			r.calls++
			page, err = r.p.ChangedRows(r.ctx, q)
		}
		if err != nil {
			return false, fmt.Errorf("trackerapp: read the changed rows of board %s: %w", board, call(err))
		}
		m, err := mirrorRows(board, page.Rows, nil, page.Catalog, read)
		if err != nil {
			return false, err
		}
		m.Lifetime, m.EstimateResetAt = lifetime, r.state.EstimateResetAt
		if len(m.Rows) > 0 || cursor == nil || !bytes.Equal(page.Next, cursor) {
			stored, err := r.s.store.WriteMirrorRowPage(r.root, m, page.Next)
			if err != nil {
				return false, fmt.Errorf("trackerapp: store a page of board %s: %w", board, err)
			}
			r.markStructureDue(stored)
			log.add(changesOf(stored))
			if stored.CursorRefused {
				return true, nil
			}
		}
		cursor = page.Next
		if !page.More {
			return false, nil
		}
	}
}

// reconcile is the reconcile unit of board (SyncBoard). A pass lists the
// board, removes the unlisted rows read more than tracker.DeltaOverlap
// before the listing began, and reads by ID the listed issues with no
// row and the unlisted rows read since: its candidates, each attempted
// once, up to reconcileBatches batches a tick. A pass with candidates
// left reports More, and the next tick's reconcile goes on with them,
// listing nothing, while the run's pass (run.pass) is of the same board
// lifetime, an ID the store never reuses; a pass ends when every candidate was attempted, so one the
// rows read keeps answering missing is attempted again by the next pass,
// a reconcile interval later. In a tick whose delta stopped with more of
// its walk to read it reads nothing and reports More: the walk delivers
// the rows a listing would find missing, so a first sync longer than one
// tick's pages reads each row once and lists the board once, after the
// walk.
func (r *run) reconcile(board tracker.BoardID) (UnitResult, error) {
	if r.walking {
		return UnitResult{More: true}, nil
	}
	var log changeLog
	pass := r.pass
	if pass == nil {
		pass = &reconcilePass{}
	}
	err := r.reconcileReads(board, pass, &log)
	out := UnitResult{More: len(pass.left) > 0}
	if err == nil {
		err = r.cascade(board, &out)
	}
	log.add(out.Changes)
	out.Changes = log.changes()
	return out, err
}

// reconcilePass is a reconcile's candidates not yet attempted, from the
// listing of one board lifetime. A board's pump keeps one between its
// ticks (boardPump.pass); any other tick's lives for that tick.
type reconcilePass struct {
	lifetime int64
	left     []tracker.IssueID
}

// reconcileReads lists board when pass has no candidate left of this
// lifetime, then attempts up to reconcileBatches batches of the
// pass's candidates, taking each batch attempted off the pass, and adds
// what it stored to log.
func (r *run) reconcileReads(board tracker.BoardID, pass *reconcilePass, log *changeLog) error {
	ch, err := r.boardHeld(board)
	log.add(ch)
	if err != nil {
		return err
	}
	lifetime, err := r.lifetime(board)
	if err != nil {
		return err
	}
	if pass.lifetime != lifetime || len(pass.left) == 0 {
		*pass = reconcilePass{}
		read := r.readAt()
		r.calls++
		listing, err := r.p.BoardIssues(r.ctx, board, r.window())
		if err != nil {
			return fmt.Errorf("trackerapp: list the issues of board %s: %w", board, call(err))
		}
		// A tracker may list an issue up to DeltaOverlap late (a search
		// index), so the listing judges only the rows read before that; an
		// unlisted row read since is read by ID, which removes it only when
		// the board no longer holds it.
		settled := read - tracker.DeltaOverlap.Milliseconds()
		result, err := r.s.store.ReconcileMirrorBoard(r.root, string(board), lifetime, idStrings(listing.IDs), listing.Complete, read, settled)
		// A reconcile that failed part way removed what it reports.
		log.add(Changes{Removed: issueIDs(result.Removed)})
		if err != nil {
			return fmt.Errorf("trackerapp: reconcile board %s: %w", board, err)
		}
		left := append(issueIDsInOrder(result.Missing), issueIDsInOrder(result.Unsettled)...)
		*pass = reconcilePass{lifetime: lifetime, left: left}
	}
	for i := 0; i < reconcileBatches && len(pass.left) > 0; i++ {
		batch := pass.left[:min(readBatch, len(pass.left))]
		ch, _, _, err := r.boardRows(board, batch)
		log.add(ch)
		if err != nil {
			return err
		}
		pass.left = pass.left[len(batch):]
	}
	if len(pass.left) == 0 {
		pass.left = nil
	}
	return nil
}

// boardHeld makes sure the mirror holds board's structure, which the store
// requires before the board's rows. The store shows a board's sync state
// only once it holds the structure, so a state the tick found means no
// read is needed.
func (r *run) boardHeld(board tracker.BoardID) (Changes, error) {
	if r.stored {
		return Changes{}, nil
	}
	ch, err := r.ensureStructure(board)
	if err == nil {
		r.stored = true
	}
	return ch, err
}

// issueIDsInOrder is ids as issue IDs, in their order.
func issueIDsInOrder(ids []string) []tracker.IssueID {
	out := make([]tracker.IssueID, len(ids))
	for i, id := range ids {
		out[i] = tracker.IssueID(id)
	}
	return out
}
