package trackerapp

import (
	"bytes"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
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

// delta is the delta unit of board (syncBoard), from the cursor of the
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
		read, err := r.beginRead()
		if err != nil {
			return false, err
		}
		r.calls++
		page, err := r.p.ChangedRows(r.ctx, q)
		if err != nil && pages == 0 && cursor != nil && errors.Is(err, tracker.ErrInvalidInput) {
			// A cursor of an encoding the provider no longer reads: the walk
			// starts over from the history window.
			q.From, cursor = nil, nil
			if read, err = r.beginRead(); err != nil {
				return false, err
			}
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
			r.told(board, changesOf(stored))
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

// reconcile is the reconcile unit of board (syncBoard). A pass lists the
// board and reads by ID the listed issues with no row and the unlisted
// rows, which the read removes when the board no longer holds them (a
// listing may show an issue late, so it removes none on its own word):
// its candidates, each attempted
// once, up to reconcileBatches batches a tick. The pass is the board
// pump's (run.pass), open from the reconcile that begins it in a board
// lifetime, an ID the store never reuses, until every candidate was
// attempted: one with candidates left reports More, and the pump's next
// tick goes on with them, listing nothing. One the rows read keeps
// answering missing is attempted again by the next pass, a reconcile
// interval later. In a tick whose delta stopped with more of its walk to
// read it reads nothing and reports More, the pass open and not listed:
// the walk delivers the rows a listing would find missing, so a first
// sync longer than one tick's pages reads each row once and lists the
// board once, after the walk. A listing that fails leaves the pass open
// and not listed. A rows read that refuses the listing's rule
// (tracker.ErrRuleRefused, the tracker refusing the query that carried
// it) ends the pass, the rows stored before it standing, and is the
// reconcile's failure: the store holds it, refused, and the board lists
// again a reconcile interval after it (Service.refusalWaits), or at a
// person's refresh. Every other failed rows read keeps the pass's
// candidates. An open pass is due at the pump's next tick whatever the
// interval (boardDue), so a pass a refresh began goes on at once. The
// reconcile's outcome is explicit
// (run.reconciled): completed when the pass attempted every candidate of
// its listing, failed with the read its failing call began and the time
// that call returned, and neither while it goes on (More).
func (r *run) reconcile(board tracker.BoardID) (UnitResult, error) {
	out, err := r.reconcileUnit(board)
	switch {
	case err != nil:
		r.reconciled = reconcileOutcome{failed: true, read: r.lastRead, returned: millis(r.s.now())}
	case !out.More:
		r.reconciled = reconcileOutcome{completed: true}
	}
	return out, err
}

// reconcileOutcome is how a tick's reconcile ended (run.reconciled): the
// zero outcome is one that did not run or goes on.
type reconcileOutcome struct {
	completed, failed bool
	// read is the read the failing call began, and returned when it
	// returned, in Unix ms, set when failed.
	read     store.MirrorRead
	returned int64
}

// reconcileUnit is the reconcile's pass: its reads and the cascade.
func (r *run) reconcileUnit(board tracker.BoardID) (UnitResult, error) {
	lifetime, err := r.lifetime(board)
	if err != nil {
		return UnitResult{}, err
	}
	pass := r.pass
	if !pass.openIn(lifetime) {
		*pass = reconcilePass{lifetime: lifetime}
	}
	if r.walking {
		return UnitResult{More: true}, nil
	}
	var log changeLog
	err = r.reconcileReads(board, pass, &log)
	if errors.Is(err, tracker.ErrRuleRefused) {
		*pass = reconcilePass{}
	}
	out := UnitResult{More: len(pass.left) > 0}
	if err == nil {
		err = r.cascade(board, &out)
	}
	log.add(out.Changes)
	out.Changes = log.changes()
	return out, err
}

// reconcilePass is a board pump's reconcile pass (boardPump.pass), open in
// one board lifetime: the candidates of its listing not yet attempted,
// none before it listed. The zero pass is none open.
type reconcilePass struct {
	lifetime int64
	left     []tracker.IssueID
	// rule is what the pass's listing listed by, which the reads by ID
	// of its candidates judge by too (tracker.Listing.Rule).
	rule tracker.Eligibility
}

// openIn reports whether p is open in board lifetime lifetime.
func (p *reconcilePass) openIn(lifetime int64) bool { return p.lifetime == lifetime && lifetime != 0 }

// reconcileReads lists board when pass, open in the run's lifetime of
// board, has no candidate left, then attempts up to reconcileBatches
// batches of its candidates, taking each batch attempted off the pass,
// and adds what it stored to log. A pass with no candidate left after
// its listing ends.
func (r *run) reconcileReads(board tracker.BoardID, pass *reconcilePass, log *changeLog) error {
	ch, err := r.boardHeld(board)
	log.add(ch)
	if err != nil {
		return err
	}
	if len(pass.left) == 0 {
		read, err := r.beginRead()
		if err != nil {
			return err
		}
		r.calls++
		listing, err := r.p.BoardIssues(r.ctx, board, r.window())
		if err != nil {
			return fmt.Errorf("trackerapp: list the issues of board %s: %w", board, call(err))
		}
		result, err := r.s.store.ReconcileMirrorBoard(r.root, string(board), pass.lifetime, idStrings(listing.IDs), listing.Complete, read)
		if err != nil {
			return fmt.Errorf("trackerapp: reconcile board %s: %w", board, err)
		}
		pass.left = append(issueIDsInOrder(result.Missing), issueIDsInOrder(result.Unsettled)...)
		pass.rule = listing.Rule
	}
	for i := 0; i < reconcileBatches && len(pass.left) > 0; i++ {
		batch := pass.left[:min(readBatch, len(pass.left))]
		ch, _, unread, err := r.boardRows(board, batch, pass.rule)
		log.add(ch)
		if errors.Is(err, tracker.ErrRuleRefused) {
			// The tracker refused the query that carried the listing's rule
			// (a sprint deleted since, or a refusal of its own): no candidate
			// is judged by it, so the pass, still open in its lifetime, awaits
			// a listing (reconcile). The rows stored before the refusal stand.
			pass.left, pass.rule = nil, tracker.Eligibility{}
			return err
		}
		if err != nil {
			// The batch's issues a stored write settled were attempted; the
			// pass keeps the rest of it, moved up against the batches after.
			k := len(batch) - len(unread)
			copy(pass.left[k:], unread)
			pass.left = pass.left[k:]
			return err
		}
		pass.left = pass.left[len(batch):]
	}
	if len(pass.left) == 0 {
		*pass = reconcilePass{}
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
