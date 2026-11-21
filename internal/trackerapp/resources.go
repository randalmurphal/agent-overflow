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
// failure keeps the batches already stored.

// readBatch is the most issues one provider read and one store
// transaction take.
const readBatch = 100

// mirrorRows is rows as one store page of board's rows, read from readAt.
func mirrorRows(board tracker.BoardID, rows []tracker.Row, missing []tracker.IssueID, catalog tracker.Catalog, readAt int64) (store.MirrorRowPage, error) {
	page := store.MirrorRowPage{BoardID: string(board), Missing: idStrings(missing), Rows: make([]store.MirrorRow, 0, len(rows)), ReadAt: readAt}
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

// boardRows reads board's rows of issues and stores them; the issues the
// board no longer lists lose their row. A new row the store refused
// because the board lost rows after its read began is read once more,
// from after the removal; refused again, the read fails. A write the
// store finds due a structure read marks the run's structure due
// (run.cascade). The board's structure must be in the mirror
// (store.ErrTrackerCacheNoBoard otherwise). found are the issues whose
// row the tracker returned and the store took, of the batches stored.
func (r *run) boardRows(board tracker.BoardID, issues []tracker.IssueID) (out Changes, found []tracker.IssueID, _ error) {
	lifetime, err := r.lifetime(board)
	if err != nil {
		return out, nil, err
	}
	for batch := range slices.Chunk(issues, readBatch) {
		for again := false; len(batch) > 0; again = true {
			read := r.readAt()
			r.calls++
			rows, err := r.p.BoardRows(r.ctx, board, batch)
			if err != nil {
				return out, found, fmt.Errorf("trackerapp: read %d rows of board %s: %w", len(batch), board, call(err))
			}
			page, err := mirrorRows(board, rows.Rows, rows.Missing, rows.Catalog, read)
			if err != nil {
				return out, found, err
			}
			page.Lifetime = lifetime
			stored, err := r.s.store.WriteMirrorRows(r.root, page)
			if err != nil {
				return out, found, fmt.Errorf("trackerapp: store rows of board %s: %w", board, err)
			}
			r.markStructureDue(stored)
			out.add(changesOf(stored))
			for _, row := range rows.Rows {
				if _, refused := slices.BinarySearch(stored.Refused, string(row.Card.ID)); !refused {
					found = append(found, row.Card.ID)
				}
			}
			if again && len(stored.Refused) > 0 {
				return out, found, fmt.Errorf("trackerapp: store rows of board %s: the board lost rows during both reads of %v", board, stored.Refused)
			}
			batch = issueIDsInOrder(stored.Refused)
		}
	}
	return out, found, nil
}

// cards reads the cards of issues with no board and stores those of the
// issues the mirror keeps. An issue the tracker does not return keeps
// what the mirror holds of it.
func (r *run) cards(issues []tracker.IssueID) (Changes, error) {
	var out Changes
	for batch := range slices.Chunk(issues, readBatch) {
		err := r.againIfSwept(func() error {
			_, ch, err := r.cardsOf(batch)
			out.add(ch)
			return err
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// cardsNow is the cards read of one batch of issues (at most readBatch):
// the cards the tracker returned, those of the issues the mirror keeps
// stored.
func (r *run) cardsNow(batch []tracker.IssueID) ([]tracker.Card, Changes, error) {
	var read []tracker.Card
	var out Changes
	err := r.againIfSwept(func() error {
		var ch Changes
		var err error
		read, ch, err = r.cardsOf(batch)
		out.add(ch)
		return err
	})
	return read, out, err
}

// cardsOf reads and stores the cards of one batch of issues, and returns
// the cards read.
func (r *run) cardsOf(batch []tracker.IssueID) ([]tracker.Card, Changes, error) {
	readAt := r.readAt()
	r.calls++
	read, err := r.p.Cards(r.ctx, batch)
	if err != nil {
		return nil, Changes{}, fmt.Errorf("trackerapp: read %d cards: %w", len(batch), call(err))
	}
	cards := make([]store.MirrorCard, 0, len(read.Cards))
	for _, c := range read.Cards {
		m, err := mirrorCard(c)
		if err != nil {
			return nil, Changes{}, err
		}
		cards = append(cards, m)
	}
	catalog, err := mirrorCatalog(read.Catalog)
	if err != nil {
		return nil, Changes{}, err
	}
	stored, err := r.s.store.UpsertMirrorCards(r.root, r.state.Sweep, cards, catalog, readAt)
	if err != nil {
		return nil, Changes{}, fmt.Errorf("trackerapp: store %d cards: %w", len(cards), err)
	}
	return read.Cards, changesOf(stored), nil
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
	read := r.readAt()
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

// worklogs reads author's worklogs whose day is from to to (at most
// worklogSpan days) through the connection's time tracker, and makes them
// the mirror's for those days. Worklogs name no issue the mirror must
// keep, so they change no view the funnel tells of.
func (r *run) worklogs(author tracker.UserID, from, to tracker.Date) error {
	tt := r.p.Time()
	if tt == nil {
		return fmt.Errorf("trackerapp: connection %s has no time tracking: %w", r.row.ID, tracker.ErrUnsupported)
	}
	read := r.readAt()
	r.calls++
	logs, err := tt.Worklogs(r.ctx, tracker.WorklogQuery{Author: author, From: from, To: to})
	if err != nil {
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
	if _, err := r.s.store.ReplaceMirrorWorklogs(r.root, string(author), from.String(), to.String(), read, stored); err != nil {
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
