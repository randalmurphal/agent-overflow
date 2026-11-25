package store

import (
	"database/sql"
	"fmt"
	"slices"
)

// ReplaceMirrorHistories makes each history the whole stored history of
// its issue, with the time the issue's current status began, and returns
// each issue's outcome in input order. A history whose receipt is stale
// (MirrorHistory.Receipt) is superseded and changes nothing; an issue the
// mirror does not keep stores nothing (Unkept). Every write that changes
// an issue's history advances its receipt. It takes at most
// mirrorHistoryBatch histories of distinct issues, of any length, and the
// users and statuses the read names (catalog, stored as UpsertMirrorCards
// stores one from a read that began at readAt), reporting what the
// catalog changed (MirrorCatalogWrite). The catalog and the histories that fit are written in
// one transaction of four statements and one per kind the catalog names,
// the histories in more when their changes pass mirrorMaxBatchChanges or
// what they remove passes clearMirrorChunk; a history that alone passes
// either is staged in transactions of at most clearMirrorChunk changes
// and made the issue's history in one more (replaceStagedHistory). No
// transaction deletes more than clearMirrorChunk changes, and no read
// sees part of a history. sweep is the root's sweep captured before the
// reads (ErrTrackerCacheSwept). A call that fails after a transaction of
// it committed returns, with the error, the outcomes so far: Changed for
// every history a committed transaction changed, and for a staged history
// whose write had begun, which may not have completed, and what the first
// transaction changed of the catalog. The caller tells those changes
// whatever it does about the error; the other outcomes of a failed call
// say nothing.
func (s *Store) ReplaceMirrorHistories(rootID, sweep int64, histories []MirrorHistory, catalog MirrorCatalog,
	read MirrorRead) (results []MirrorHistoryResult, catalogWrite MirrorCatalogWrite, err error) {
	times, err := read.in(rootID)
	if err != nil {
		return nil, MirrorCatalogWrite{}, err
	}
	readAt := times.at
	named := len(catalog.Users)+len(catalog.Statuses) > 0
	if len(histories) > mirrorHistoryBatch || sweep <= 0 || readAt <= 0 {
		return nil, MirrorCatalogWrite{}, fmt.Errorf("%w: %d histories, over %d, or no sweep or read time", ErrInvalidTrackerCache, len(histories),
			mirrorHistoryBatch)
	}
	if err := catalog.check(); err != nil {
		return nil, MirrorCatalogWrite{}, err
	}
	// A refusal below is of the input: nothing was written.
	inputs := make([]mirrorHistoryInput, len(histories))
	results = make([]MirrorHistoryResult, len(histories))
	index := make(map[string]int, len(histories))
	for i, h := range histories {
		in, err := newMirrorHistoryInput(h, readAt)
		if err != nil {
			return nil, MirrorCatalogWrite{}, err
		}
		if _, ok := index[h.IssueID]; ok {
			return nil, MirrorCatalogWrite{}, fmt.Errorf("%w: issue %s has two histories", ErrInvalidTrackerCache, h.IssueID)
		}
		inputs[i], index[h.IssueID] = in, i
		results[i] = MirrorHistoryResult{IssueID: h.IssueID, Unkept: true}
	}
	action := fmt.Sprintf("store: replace tracker mirror %d histories", rootID)
	var staged []mirrorHistoryInput
	for pending, first := inputs, true; len(pending) > 0 || first && named; first = false {
		// What the transactions before this one committed: a transaction
		// that fails rolls back, and its outcomes with it.
		committed := slices.Clone(results)
		var changed MirrorCatalogWrite
		err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
			var err error
			if first && named {
				if changed, err = upsertMirrorCatalogTx(tx, rootID, catalog, readAt); err != nil {
					return err
				}
			}
			if len(pending) == 0 {
				return nil
			}
			var stage []mirrorHistoryInput
			pending, stage, err = replaceMirrorHistoriesTx(tx, rootID, pending, index, results)
			staged = append(staged, stage...)
			return err
		})
		if err != nil {
			return committed, catalogWrite, err
		}
		catalogWrite.Changed = catalogWrite.Changed || changed.Changed
		catalogWrite.Threads = unionSorted(catalogWrite.Threads, changed.Threads)
	}
	if len(staged) == 0 {
		return results, catalogWrite, nil
	}
	// A staged history runs with the cleanup's lock, so no cleanup deletes
	// its staging and no other staging of the issue runs meanwhile.
	s.mirrorCleanup.Lock()
	defer s.mirrorCleanup.Unlock()
	for _, in := range staged {
		if err := s.replaceStagedHistory(rootID, sweep, in, &results[index[in.h.IssueID]]); err != nil {
			return results, catalogWrite, err
		}
	}
	return results, catalogWrite, nil
}

// replaceMirrorHistoriesTx writes the admitted histories of pending that
// fit one transaction, records each outcome in results, and returns the
// histories left for the next transaction and those to stage: a history
// whose changes or removals alone pass a chunk, or whose issue holds other
// generations a staging left (history_dirty).
func replaceMirrorHistoriesTx(tx *sql.Tx, rootID int64, pending []mirrorHistoryInput, index map[string]int,
	results []MirrorHistoryResult) (next, staged []mirrorHistoryInput, err error) {
	rows := make([]mirrorHistoryRow, len(pending))
	for i, in := range pending {
		rows[i] = in.row
	}
	listed, err := encodeMirrorJSON(rows)
	if err != nil {
		return nil, nil, err
	}
	states, err := readMirrorHistoryStatesTx(tx, rootID, listed)
	if err != nil {
		return nil, nil, err
	}
	// The first transaction reads every history of the call, those it
	// stages included, so a refusal leaves the mirror as it was; boards_rev
	// and updated_at never move back, so a later transaction or a staging
	// refuses nothing.
	for _, in := range pending {
		if st, ok := states[in.h.IssueID]; ok {
			if err := st.checkAhead(in); err != nil {
				return nil, nil, err
			}
		}
	}
	var write []mirrorHistoryRow
	var changes []mirrorChangeRow
	removeBudget, writeBudget := clearMirrorChunk, mirrorMaxBatchChanges
	for n, in := range pending {
		st, ok := states[in.h.IssueID]
		result := &results[index[in.h.IssueID]]
		switch {
		case !ok || !st.kept:
			continue
		case st.supersedes(in):
			result.Unkept, result.Superseded = false, true
			continue
		case st.dirty || len(in.changes) > clearMirrorChunk || st.removes > clearMirrorChunk:
			staged = append(staged, in)
			continue
		case (st.removes > removeBudget || len(in.changes) > writeBudget) && len(write) > 0:
			return pending[n:], staged, writeMirrorHistoriesTx(tx, rootID, write, changes, states, index, results)
		}
		result.Unkept = false
		removeBudget -= st.removes
		writeBudget -= len(in.changes)
		in.setGen(st.gen)
		write, changes = append(write, st.rowOf(in, st.gen)), append(changes, in.changes...)
	}
	return nil, staged, writeMirrorHistoriesTx(tx, rootID, write, changes, states, index, results)
}

// mirrorHistoryState is an issue's stored history as a write of it reads
// it.
type mirrorHistoryState struct {
	storedFrom, storedFor, storedAnswered, storedWait, storedBoardsRev int64
	receipt, boardsRev, updatedAt, gen                                 int64
	dirty, kept                                                        bool
	removes                                                            int
	// status and createdAt are the issue's card's, for the status result
	// of a history read for its version (statusOf); since and finalFor its
	// stored status time and the update time a final read decided it for.
	status                     string
	createdAt, since, finalFor int64
}

// final reports that a final read decided the stored status time for the
// card's version.
func (st mirrorHistoryState) final() bool { return st.finalFor >= st.updatedAt }

// statusOf is the status result in gives the issue's card, of the
// version and status st holds (MirrorStatusSince): none for a history read
// for an earlier update, whose evidence is of another version. The latest
// status change at or before the version is the history's own, held in in
// whole even when it is staged: a change after it belongs to a later
// version the tracker reached since, and the stay it starts is not the
// card's. The history is final for its version when it waits for nothing.
// The writer applies the result only to a record still of that version
// and status (rowOf, mirrorStatusWriteSQL).
func (st mirrorHistoryState) statusOf(in mirrorHistoryInput) (mirrorStatusResult, int64) {
	if in.h.For != st.updatedAt {
		return 0, 0
	}
	e := MirrorStatusEvidence{Status: st.status, CreatedAt: st.createdAt, UpdatedAt: st.updatedAt,
		Complete: in.h.CompleteFrom == 0, Final: in.h.Wait == 0, Settle: in.h.Wait}
	for _, c := range in.h.Changes {
		if c.Kind == MirrorChangeStatus && c.At <= st.updatedAt {
			e.LatestTo, e.LatestAt = c.To, c.At
		}
	}
	return mirrorStatusSince(e)
}

// rowOf is in's row for generation gen with the status result it gives
// the issue's card.
func (st mirrorHistoryState) rowOf(in mirrorHistoryInput, gen int64) mirrorHistoryRow {
	row := in.rowAt(gen)
	var r mirrorStatusResult
	r, row.Since = st.statusOf(in)
	row.Status, row.Of = int(r), st.status
	return row
}

// checkAhead refuses a history read for a boards revision or an update
// time the issue has not reached: it would record boards the issue does
// not have yet as read, or an update not yet seen as covered, and
// suppress the read the issue is due when it reaches them.
func (st mirrorHistoryState) checkAhead(in mirrorHistoryInput) error {
	if in.h.BoardsRev > st.boardsRev || in.h.For > st.updatedAt {
		return fmt.Errorf("%w: issue %s history names boards revision %d and update time %d, ahead of the issue's %d and %d "+
			"(MirrorHistoryDue)", ErrInvalidTrackerCache, in.h.IssueID, in.h.BoardsRev, in.h.For, st.boardsRev, st.updatedAt)
	}
	return nil
}

// supersedes reports that in was listed before the issue's history last
// changed: its receipt is not the issue's. Only the receipt orders
// writes; no time does, since the steady clocks of two processes are not
// one order.
func (st mirrorHistoryState) supersedes(in mirrorHistoryInput) bool {
	return in.h.Receipt != st.receipt
}

// sameTimes reports that in has the stored history's times and boards.
func (st mirrorHistoryState) sameTimes(in mirrorHistoryInput) bool {
	return st.storedFrom == in.h.CompleteFrom && st.storedFor == in.h.For && st.storedAnswered == in.h.AnsweredAt &&
		st.storedWait == in.h.Wait && st.storedBoardsRev == in.h.BoardsRev
}

// readMirrorHistoryStatesTx reads mirrorHistoryStateSQL for the encoded
// list of mirrorHistoryRow, by issue; an issue the mirror lacks is absent.
func readMirrorHistoryStatesTx(q sqlQueryer, rootID int64, listed string) (map[string]mirrorHistoryState, error) {
	states := map[string]mirrorHistoryState{}
	err := scanMirrorRows(q, mirrorHistoryStateSQL, []any{rootID, listed}, func(r rowScanner) error {
		var id string
		var st mirrorHistoryState
		err := r.Scan(&id, &st.storedFrom, &st.storedFor, &st.storedAnswered, &st.storedWait, &st.storedBoardsRev, &st.receipt,
			&st.boardsRev, &st.updatedAt, &st.gen, &st.dirty, &st.kept, &st.removes, &st.status, &st.createdAt, &st.since, &st.finalFor)
		states[id] = st
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read issues: %w", err)
	}
	return states, nil
}

// writeMirrorHistoriesTx writes the histories write, with their changes,
// over the issues' stored ones, whose state the transaction read
// (states), and marks Changed each issue whose changes or shown status
// changed.
func writeMirrorHistoriesTx(tx *sql.Tx, rootID int64, write []mirrorHistoryRow, changes []mirrorChangeRow,
	states map[string]mirrorHistoryState, index map[string]int, results []MirrorHistoryResult) error {
	if len(write) == 0 {
		return nil
	}
	histories, err := encodeMirrorJSON(write)
	if err != nil {
		return err
	}
	var rows string
	if len(changes) > 0 {
		if rows, err = encodeMirrorJSON(changes); err != nil {
			return err
		}
	}
	mark := func(changed []string) {
		for _, id := range changed {
			results[index[id]].Changed = true
		}
	}
	for _, step := range []struct{ what, query, list string }{
		{"write changes", mirrorChangesUpsertSQL, rows},
		{"remove changes", mirrorChangesRemoveSQL, histories},
	} {
		if step.list == "" {
			continue
		}
		changed, err := queryIDs(tx, step.query, rootID, step.list)
		if err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
		mark(changed)
	}
	// The times statement advances the receipt of an issue whose changes
	// changed though its times did not.
	if histories, err = markChangedHistories(write, index, results, histories); err != nil {
		return err
	}
	changed, err := writeMirrorHistoryTimesTx(tx, rootID, histories, states)
	if err != nil {
		return fmt.Errorf("write history times: %w", err)
	}
	mark(changed)
	return nil
}

// markChangedHistories returns listed, the encoded write, with the rows of
// the issues whose changes changed marked (mirrorHistoryRow.Changes),
// encoded again only when there is one.
func markChangedHistories(write []mirrorHistoryRow, index map[string]int, results []MirrorHistoryResult, listed string) (string, error) {
	marked := false
	for k := range write {
		if results[index[write[k].Issue]].Changed {
			write[k].Changes, marked = true, true
		}
	}
	if !marked {
		return listed, nil
	}
	return encodeMirrorJSON(write)
}
