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
// stores one from a read that began at readAt), reporting whether the
// catalog changed. The catalog and the histories that fit are written in
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
// whose write had begun, which may not have completed, and whether the
// first transaction changed the catalog. The caller tells those changes
// whatever it does about the error; the other outcomes of a failed call
// say nothing.
func (s *Store) ReplaceMirrorHistories(rootID, sweep int64, histories []MirrorHistory, catalog MirrorCatalog,
	readAt int64) (results []MirrorHistoryResult, catalogChanged bool, err error) {
	named := len(catalog.Users)+len(catalog.Statuses) > 0
	switch {
	case len(histories) > mirrorHistoryBatch || sweep <= 0:
		return nil, false, fmt.Errorf("%w: %d histories, over %d, or no sweep", ErrInvalidTrackerCache, len(histories), mirrorHistoryBatch)
	case named && readAt <= 0:
		return nil, false, fmt.Errorf("%w: a catalog with no read time", ErrInvalidTrackerCache)
	}
	if err := catalog.check(); err != nil {
		return nil, false, err
	}
	// A refusal below is of the input: nothing was written.
	inputs := make([]mirrorHistoryInput, len(histories))
	results = make([]MirrorHistoryResult, len(histories))
	index := make(map[string]int, len(histories))
	for i, h := range histories {
		in, err := newMirrorHistoryInput(h)
		if err != nil {
			return nil, false, err
		}
		if _, ok := index[h.IssueID]; ok {
			return nil, false, fmt.Errorf("%w: issue %s has two histories", ErrInvalidTrackerCache, h.IssueID)
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
		var changed bool
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
			return committed, catalogChanged, err
		}
		catalogChanged = catalogChanged || changed
	}
	if len(staged) == 0 {
		return results, catalogChanged, nil
	}
	// A staged history runs with the cleanup's lock, so no cleanup deletes
	// its staging and no other staging of the issue runs meanwhile.
	s.mirrorCleanup.Lock()
	defer s.mirrorCleanup.Unlock()
	for _, in := range staged {
		if err := s.replaceStagedHistory(rootID, sweep, in, &results[index[in.h.IssueID]]); err != nil {
			return results, catalogChanged, err
		}
	}
	return results, catalogChanged, nil
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
			return pending[n:], staged, writeMirrorHistoriesTx(tx, rootID, write, changes, index, results)
		}
		result.Unkept = false
		removeBudget -= st.removes
		writeBudget -= len(in.changes)
		in.setGen(st.gen)
		write, changes = append(write, in.rowAt(st.gen)), append(changes, in.changes...)
	}
	return nil, staged, writeMirrorHistoriesTx(tx, rootID, write, changes, index, results)
}

// mirrorHistoryState is an issue's stored history as a write of it reads
// it.
type mirrorHistoryState struct {
	storedFrom, storedFor, storedAnswered, storedWait, storedBoardsRev int64
	receipt, boardsRev, updatedAt, gen                                 int64
	dirty, kept                                                        bool
	removes                                                            int
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
			&st.boardsRev, &st.updatedAt, &st.gen, &st.dirty, &st.kept, &st.removes)
		states[id] = st
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read issues: %w", err)
	}
	return states, nil
}

func writeMirrorHistoriesTx(tx *sql.Tx, rootID int64, write []mirrorHistoryRow, changes []mirrorChangeRow, index map[string]int,
	results []MirrorHistoryResult) error {
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
	for _, step := range []struct{ what, query, list string }{
		{"write changes", mirrorChangesUpsertSQL, rows},
		{"remove changes", mirrorChangesRemoveSQL, histories},
		{"write history times", mirrorHistoryTimesSQL, histories},
	} {
		if step.query == mirrorHistoryTimesSQL {
			// The times statement advances the receipt of an issue whose
			// changes changed though its times did not.
			if step.list, err = markChangedHistories(write, index, results, step.list); err != nil {
				return err
			}
		}
		if step.list == "" {
			continue
		}
		changed, err := queryIDs(tx, step.query, rootID, step.list)
		if err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
		for _, id := range changed {
			results[index[id]].Changed = true
		}
	}
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
