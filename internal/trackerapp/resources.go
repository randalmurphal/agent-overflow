package trackerapp

import (
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The mirror is filled by resource, and each resource has one read that
// sync and writes share: it reads from the tracker, stores through the
// store and returns what the store changed. A read of many issues is made
// in batches of readBatch, each read and stored before the next, so a
// failure keeps the batches already stored; it returns the issues it did
// not read and store (unread): those of the batch that failed that no
// stored write of it settled, and every issue of the batches after it.
// Every other issue asked was read and stored.

// readBatch is the most issues one provider read and one store
// transaction take.
const readBatch = 100

// mirrorRows is rows as one store page of board's rows, read from read.
func mirrorRows(board tracker.BoardID, rows []tracker.Row, missing []tracker.IssueID, catalog tracker.Catalog, read store.MirrorRead) (store.MirrorRowPage, error) {
	page := store.MirrorRowPage{BoardID: string(board), Missing: idStrings(missing), Rows: make([]store.MirrorRow, 0, len(rows)), ReadAt: read}
	for _, row := range rows {
		m, err := mirrorRow(row)
		if err != nil {
			return store.MirrorRowPage{}, err
		}
		page.Rows = append(page.Rows, m)
	}
	var err error
	page.Catalog, err = mirrorCatalog(catalog)
	return page, err
}

// boardRows reads board's rows of issues that rule lists and stores them;
// the issues the board no longer lists under it lose their row. What the store refused (store
// MirrorChanges.Refused: a new row, because the board lost rows after the
// read began, or an issue's absence, because a complete listing began
// after it) is read once more, from after that removal or listing;
// refused again, the read fails. A write the store finds due a
// structure read marks the run's structure due (run.cascade). The
// board's structure must be in the mirror (store.ErrTrackerCacheNoBoard
// otherwise). found are the issues whose row the tracker returned and the
// store took. unread are exactly the issues no stored write settled: of
// the batch that failed, all of it when its first read failed and else
// those its last stored write refused, then every batch after it, in the
// order of issues when issues are sorted; none when it returns no error.
func (r *run) boardRows(board tracker.BoardID, issues []tracker.IssueID, rule tracker.Eligibility) (out Changes, found, unread []tracker.IssueID, _ error) {
	lifetime, err := r.lifetime(board)
	if err != nil {
		return out, nil, issues, err
	}
	for i := 0; i < len(issues); i += readBatch {
		next := min(i+readBatch, len(issues))
		// left are the batch's issues no stored write settled yet.
		left := issues[i:next]
		unsettled := func() []tracker.IssueID { return slices.Concat(left, issues[next:]) }
		for again := false; len(left) > 0; again = true {
			read, err := r.beginRead()
			if err != nil {
				return out, found, unsettled(), err
			}
			r.calls++
			rows, err := r.p.BoardRows(r.ctx, board, left, rule)
			if err != nil {
				return out, found, unsettled(), fmt.Errorf("trackerapp: read %d rows of board %s: %w", len(left), board, call(err))
			}
			sessionOf(r.ctx).keepRows(r.row.ID, board, rows)
			page, err := mirrorRows(board, rows.Rows, rows.Missing, rows.Catalog, read)
			if err != nil {
				return out, found, unsettled(), err
			}
			page.Lifetime = lifetime
			stored, err := r.s.store.WriteMirrorRows(r.root, page)
			if err != nil {
				return out, found, unsettled(), fmt.Errorf("trackerapp: store rows of board %s: %w", board, err)
			}
			r.markStructureDue(stored)
			w := changesOf(stored)
			r.told(board, w)
			out.add(w.Changes)
			for _, row := range rows.Rows {
				if _, refused := slices.BinarySearch(stored.Refused, string(row.Card.ID)); !refused {
					found = append(found, row.Card.ID)
				}
			}
			left = issueIDsInOrder(stored.Refused)
			if again && len(left) > 0 {
				return out, found, unsettled(), fmt.Errorf("trackerapp: store rows of board %s: the board's rows moved during both reads of %v", board, stored.Refused)
			}
		}
	}
	return out, found, nil, nil
}

// cards reads the cards of issues with no board and stores those of the
// issues the mirror keeps. An issue the tracker does not return keeps
// what the mirror holds of it, and is one of missing. unread are the
// issues it did not read, none when it returns no error.
func (r *run) cards(issues []tracker.IssueID) (out Changes, missing, unread []tracker.IssueID, _ error) {
	for i := 0; i < len(issues); i += readBatch {
		batch := issues[i:min(i+readBatch, len(issues))]
		err := r.againIfSwept(func() error {
			ch, gone, began, err := r.cardsOf(batch)
			out.add(ch)
			missing = append(missing, gone...)
			if err != nil {
				return err
			}
			return r.markMissing(began, gone)
		})
		if err != nil {
			return out, missing, issues[i:], err
		}
	}
	return out, missing, nil, nil
}

// cardsOf reads and stores the cards of one batch of issues, and answers
// those the tracker did not return and the read it began.
func (r *run) cardsOf(batch []tracker.IssueID) (Changes, []tracker.IssueID, store.MirrorRead, error) {
	began, err := r.beginRead()
	if err != nil {
		return Changes{}, nil, began, err
	}
	r.calls++
	read, err := r.p.Cards(r.ctx, batch)
	if err != nil {
		return Changes{}, nil, began, fmt.Errorf("trackerapp: read %d cards: %w", len(batch), call(err))
	}
	cards := make([]store.MirrorCard, 0, len(read.Cards))
	for _, c := range read.Cards {
		m, err := mirrorCard(c)
		if err != nil {
			return Changes{}, nil, began, err
		}
		cards = append(cards, m)
	}
	catalog, err := mirrorCatalog(read.Catalog)
	if err != nil {
		return Changes{}, nil, began, err
	}
	stored, err := r.s.store.UpsertMirrorCards(r.root, r.state.Sweep, cards, catalog, began)
	if err != nil {
		return Changes{}, nil, began, fmt.Errorf("trackerapp: store %d cards: %w", len(cards), err)
	}
	w := changesOf(stored)
	r.told("", w)
	return w.Changes, read.Missing, began, nil
}

// markMissing records that the read begun at began did not return the
// issues missing: the links to them show not found until a read returns
// them (store MarkMirrorIssuesMissing). The links it moved are told as
// any write's.
func (r *run) markMissing(began store.MirrorRead, missing []tracker.IssueID) error {
	if len(missing) == 0 {
		return nil
	}
	threads, err := r.s.store.MarkMirrorIssuesMissing(r.root, began, idStrings(missing))
	if err != nil {
		return fmt.Errorf("trackerapp: record the %d issues the tracker did not return: %w", len(missing), err)
	}
	r.told("", written{links: threads})
	return nil
}

// againIfSwept runs read, a read of content outside a board and its
// writes, and runs it once more when the store refused a write because a
// sweep ran since the run captured the root's sweep (the content may
// describe an issue's earlier lifetime): after reading the sweep again.
func (r *run) againIfSwept(read func() error) error {
	err := read()
	if !errors.Is(err, store.ErrTrackerCacheSwept) {
		return err
	}
	st, _, err := r.s.store.GetMirrorSync(r.root, string(r.scope.Board), r.steadyAt())
	if err != nil {
		return fmt.Errorf("trackerapp: read the sweep of the mirror of %s again: %w", r.row.ID, err)
	}
	r.state.Sweep = st.Sweep
	return read()
}

// structure reads board's structure and stores it. A structure that
// changed the board's estimate definition, this one or a later one the
// store holds, makes the run's walk start over: the store refuses the
// cursor of a walk begun under an earlier definition.
func (r *run) structure(board tracker.BoardID) (Changes, error) {
	lifetime, err := r.lifetime(board)
	if err != nil {
		return Changes{}, err
	}
	read, err := r.beginRead()
	if err != nil {
		return Changes{}, err
	}
	r.calls++
	st, err := r.p.Structure(r.ctx, board, r.window())
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: read the structure of board %s: %w", board, call(err))
	}
	if st.Board.ID != board {
		return Changes{}, fmt.Errorf("trackerapp: the structure of board %s: %w", board,
			call(fmt.Errorf("the tracker answered with board %q", st.Board.ID)))
	}
	m, err := mirrorStructure(st, read)
	if err != nil {
		return Changes{}, err
	}
	m.Lifetime = lifetime
	// A write that failed after its structure committed returns what the
	// structure changed with the error; a reset only moves forward, so a
	// later one is the store's whether or not the call failed.
	stored, err := r.s.store.ReplaceMirrorStructure(r.root, m)
	ch := Changes{Structure: stored.Changed, Catalog: stored.Catalog}
	r.told(board, written{Changes: ch, links: stored.Threads})
	if board == r.scope.Board && stored.EstimateResetAt > r.state.EstimateResetAt {
		r.state.EstimateResetAt, r.state.Cursor = stored.EstimateResetAt, nil
	}
	if err != nil {
		return ch, fmt.Errorf("trackerapp: store the structure of board %s: %w", board, err)
	}
	if board == r.scope.Board {
		r.stored, r.structureDue = true, false
	}
	return ch, nil
}

// timeSourceChanged is the failure of worklogs read from a time source
// the connection stopped using during the read (store
// ErrTrackerCacheSourceChanged).
const timeSourceChanged = "this connection's time tracking changed (its Tempo connection was added, turned on or off, or removed) while its worklogs were read; refresh to read them again"

// worklogs reads author's worklogs whose day is from to to (at most
// worklogSpan days) through the connection's time tracker, and makes them
// the mirror's for those days. Worklogs name no issue the mirror must
// keep, so they change no view the funnel tells of.
func (r *run) worklogs(author tracker.UserID, from, to tracker.Date) error {
	tt := r.p.Time()
	if tt == nil {
		return fmt.Errorf("trackerapp: connection %s has no time tracking: %w", r.row.ID, tracker.ErrUnsupported)
	}
	read, err := r.beginRead()
	if err != nil {
		return err
	}
	r.calls++
	logs, err := tt.Worklogs(r.ctx, tracker.WorklogQuery{Author: author, From: from, To: to})
	if f, refused := worklogRefusal(author, from, to, err); refused {
		return &requestRefused{f: f, err: err}
	}
	if err != nil {
		err = timeAnswered(r.timeBy, err)
		return fmt.Errorf("trackerapp: read the worklogs of %s from %s to %s: %w", author, from, to, call(err))
	}
	stored := make([]store.MirrorWorklog, len(logs))
	for i, w := range logs {
		if w.Author != author || w.Day.Compare(from) < 0 || w.Day.Compare(to) > 0 {
			return fmt.Errorf("trackerapp: the worklogs of %s from %s to %s: %w", author, from, to,
				call(fmt.Errorf("the tracker answered worklog %s of %s on %s", w.ID, w.Author, w.Day)))
		}
		stored[i] = mirrorWorklog(w)
	}
	_, err = r.s.store.ReplaceMirrorWorklogs(r.root, r.row.TimeSourceGen, string(author), from.String(), to.String(), read, stored)
	switch {
	case errors.Is(err, store.ErrTrackerCacheSourceChanged):
		return &failureError{f: Failure{Kind: FailureTransient, Message: timeSourceChanged}}
	case errors.Is(err, store.ErrTooManyWorklogs):
		return &requestRefused{err: err, f: Failure{Kind: FailureInternal, Message: fmt.Sprintf(
			"%s has more than %d worklogs from %s to %s, which is more than one read can store; read fewer days at a time",
			author, store.MaxReplacedWorklogs, from, to)}}
	}
	if err != nil {
		return fmt.Errorf("trackerapp: store the worklogs of %s from %s to %s: %w", author, from, to, err)
	}
	return nil
}

// markStructureDue marks the run's structure due when a row write found
// the board due a structure read (store MirrorChanges.StructureDue).
func (r *run) markStructureDue(stored store.MirrorChanges) {
	if stored.StructureDue {
		r.structureDue = true
	}
}

// cascade reads board's structure when it is due: the tick's sync state
// said so, or a row write since the run's last structure read did, because
// its rows name a status, user or sprint the mirror lacks that no complete
// structure read left unexplained (store MirrorSync.StructureDueAt). A
// failed read leaves it due in the store: the unit fails, and the next
// tick reads it again.
func (r *run) cascade(board tracker.BoardID, out *UnitResult) error {
	if !r.structureDue {
		return nil
	}
	ch, err := r.structure(board)
	out.Changes.add(ch)
	return err
}

// ensureStructure reads board's structure when the mirror does not hold
// it, since the store refuses rows of a board it does not know.
func (r *run) ensureStructure(board tracker.BoardID) (Changes, error) {
	_, ok, err := r.s.store.GetMirrorBoard(r.root, string(board))
	if err != nil {
		return Changes{}, fmt.Errorf("trackerapp: read board %s from the mirror: %w", board, err)
	}
	if ok {
		return Changes{}, nil
	}
	return r.structure(board)
}
