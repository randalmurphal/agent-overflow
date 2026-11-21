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
	var out UnitResult
	ch, err := r.boardHeld(board)
	out.Changes.add(ch)
	if err != nil {
		return out, err
	}
	lifetime, err := r.lifetime(board)
	if err != nil {
		return out, err
	}
	var cursor tracker.Cursor
	if len(r.state.Cursor) > 0 {
		cursor = r.state.Cursor
	}
	for pages := 0; ; pages++ {
		if pages == deltaPages {
			out.More = true
			break
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
			return out, fmt.Errorf("trackerapp: read the changed rows of board %s: %w", board, call(err))
		}
		m, err := mirrorRows(board, page.Rows, nil, page.Catalog, read)
		if err != nil {
			return out, err
		}
		m.Lifetime, m.EstimateResetAt = lifetime, r.state.EstimateResetAt
		if len(m.Rows) > 0 || cursor == nil || !bytes.Equal(page.Next, cursor) {
			stored, err := r.s.store.WriteMirrorRowPage(r.root, m, page.Next)
			if err != nil {
				return out, fmt.Errorf("trackerapp: store a page of board %s: %w", board, err)
			}
			r.markStructureDue(stored)
			out.Changes.add(changesOf(stored))
			if stored.CursorRefused {
				out.More = true
				break
			}
		}
		cursor = page.Next
		if !page.More {
			break
		}
	}
	r.walking = out.More
	return out, r.cascade(board, &out)
}

// reconcile is the reconcile unit of board (SyncBoard): it lists the
// board, removes the unlisted rows read more than tracker.DeltaOverlap
// before the listing began, and reads by ID, in the same tick, the listed
// issues with no row and the unlisted rows read since. In a tick whose
// delta stopped with more of its walk to read it lists nothing and
// reports More: the walk delivers the rows a listing would find missing,
// so a first sync longer than one tick's pages reads each row once and
// lists the board once, after the walk.
func (r *run) reconcile(board tracker.BoardID) (UnitResult, error) {
	var out UnitResult
	if r.walking {
		out.More = true
		return out, nil
	}
	ch, err := r.boardHeld(board)
	out.Changes.add(ch)
	if err != nil {
		return out, err
	}
	lifetime, err := r.lifetime(board)
	if err != nil {
		return out, err
	}
	read := r.readAt()
	r.calls++
	listing, err := r.p.BoardIssues(r.ctx, board, r.window())
	if err != nil {
		return out, fmt.Errorf("trackerapp: list the issues of board %s: %w", board, call(err))
	}
	// A tracker may list an issue up to DeltaOverlap late (a search index),
	// so the listing judges only the rows read before that; an unlisted
	// row read since is read by ID, which removes it only when the board
	// no longer holds it.
	settled := read - tracker.DeltaOverlap.Milliseconds()
	result, err := r.s.store.ReconcileMirrorBoard(r.root, string(board), lifetime, idStrings(listing.IDs), listing.Complete, read, settled)
	// A reconcile that failed part way removed what it reports.
	out.Changes.add(Changes{Removed: issueIDs(result.Removed)})
	if err != nil {
		return out, fmt.Errorf("trackerapp: reconcile board %s: %w", board, err)
	}
	missing := append(issueIDsInOrder(result.Missing), issueIDsInOrder(result.Unsettled)...)
	if limit := reconcileBatches * readBatch; len(missing) > limit {
		missing, out.More = missing[:limit], true
	}
	ch, _, err = r.boardRows(board, missing)
	out.Changes.add(ch)
	if err != nil {
		return out, err
	}
	return out, r.cascade(board, &out)
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
