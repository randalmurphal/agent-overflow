package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A history too long for one transaction is staged. An issue's history is
// the generation of its changes that tracker_cache_issues.history_gen
// names; other generations exist only while history_dirty is set. Staging
// writes the new history to the next generation a chunk per transaction,
// makes it the issue's history in one more, and then deletes the old one a
// chunk per transaction, so no read sees part of a history and no
// transaction deletes more than clearMirrorChunk changes. A crash between
// transactions leaves history_dirty set and the old history visible (or
// the new one, once made); FinishMirrorCleanup deletes the other
// generations. Staging runs with the cleanup's lock, and a batch write
// stages every issue with history_dirty set rather than write it, so
// nothing else changes the issue's changes while it runs.

var (
	// mirrorHistoryProbeSQL reads, of a chunk ?2 of a history's changes at
	// the issue's generation, those the stored history lacks or holds
	// otherwise.
	mirrorHistoryProbeSQL = `SELECT c.key, s.root_id IS NOT NULL FROM json_each(?2) c
		LEFT JOIN tracker_cache_changes s ON s.root_id = ?1 AND s.issue_id = c.value ->> 'i' AND s.gen = c.value ->> 'g'
		  AND s.kind = c.value ->> 'k' AND s.seq = c.value ->> 'q'
		WHERE s.root_id IS NULL OR s.at IS NOT c.value ->> 't' OR s.author_id IS NOT c.value ->> 'a'
		   OR s.board_id IS NOT c.value ->> 'b' OR s.from_value IS NOT c.value ->> 'f' OR s.to_value IS NOT c.value ->> 'o'
		   OR s.from_number IS NOT c.value ->> 'x' OR s.to_number IS NOT c.value ->> 'y'`
	// mirrorHistoryPurgeSQL deletes a chunk of an issue's changes of the
	// generations other than ?3, walking the key ranges below and above it.
	mirrorHistoryPurgeSQL = `DELETE FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id = ?2 AND (gen, kind, seq) IN (
		SELECT gen, kind, seq FROM (
		  SELECT gen, kind, seq FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id = ?2 AND gen < ?3
		  UNION ALL
		  SELECT gen, kind, seq FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id = ?2 AND gen > ?3)
		LIMIT ` + fmt.Sprint(clearMirrorChunk) + `)`
	// mirrorHistorySwitchSQL makes generation ?3, complete from ?4, read
	// for update time ?5 and boards revision ?6, answered at ?7 with a wait
	// of ?8, the issue's history, with the status result ?10 giving ?11 it
	// gives the card (mirrorStatusWriteSQL), when the issue's receipt is
	// still ?9, and advances the receipt.
	mirrorHistorySwitchSQL = func() string {
		since, finalFor := mirrorStatusWriteSQL("?10", "?11", "?5")
		return `UPDATE tracker_cache_issues SET history_gen = ?3, history_from = ?4, history_for = ?5, history_boards_rev = ?6,
		history_answered_at = ?7, history_wait = ?8, history_receipt = ?9 + 1, status_since = ` + since + `, status_final_for = ` + finalFor + `
		WHERE root_id = ?1 AND issue_id = ?2 AND history_receipt = ?9`
	}()
)

// errStaleReceipt rolls back a staged history's switch when the issue's
// receipt moved since the history was admitted.
var errStaleReceipt = errors.New("store: the history's receipt went stale")

// mirrorHistoryProbe is how a history compares with the stored one: the
// changes the stored history holds otherwise (differs), those it lacks (at
// most clearMirrorChunk and one), and whether it holds changes the history
// does not (extra).
type mirrorHistoryProbe struct {
	differs, extra bool
	missing        []mirrorChangeRow
}

// identical reports that the stored changes are the history's.
func (p mirrorHistoryProbe) identical() bool { return !p.differs && !p.extra && len(p.missing) == 0 }

// replaceStagedHistory writes in's history as ReplaceMirrorHistories
// promises, for a history too long for a batch transaction or an issue
// with generations to delete. The caller holds s.mirrorCleanup, so no
// sweep runs meanwhile: the write that marks the issue names sweep, the
// root's sweep the caller captured, and those after it follow it.
func (s *Store) replaceStagedHistory(rootID, sweep int64, in mirrorHistoryInput, result *MirrorHistoryResult) error {
	action := fmt.Sprintf("store: replace tracker mirror %d issue %s history", rootID, in.h.IssueID)
	st, admitted, err := s.stagedHistoryState(rootID, in, result)
	if err != nil || !admitted {
		return err
	}
	if st.dirty {
		if err := s.purgeMirrorHistoryGens(rootID, in.h.IssueID, st.gen, noMirrorPause); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	if st.sameTimes(in) && st.removes == 0 {
		probe, err := s.probeMirrorHistory(rootID, in, st.gen)
		if err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
		if probe.identical() {
			return s.writeIdenticalHistory(rootID, sweep, in, result, action)
		}
	}
	// From here no batch write changes the issue's changes: it stages the
	// issue instead, behind this one.
	err = s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tracker_cache_issues SET history_dirty = 1 WHERE root_id = ?1 AND issue_id = ?2`, rootID, in.h.IssueID)
		return err
	})
	if err != nil {
		return err
	}
	if st, admitted, err = s.stagedHistoryState(rootID, in, result); err != nil {
		return err
	}
	if !admitted {
		if !result.Superseded {
			return nil
		}
		// Written between the admission and the mark: the mark is cleared.
		return s.purgeMirrorHistoryGens(rootID, in.h.IssueID, st.gen, noMirrorPause)
	}
	result.Changed = true
	err = s.writeStagedHistory(rootID, in, st, action)
	if !errors.Is(err, errStaleReceipt) {
		return err
	}
	// Written meanwhile: what this history staged is deleted, leaving the
	// issue's history as that write made it.
	result.Changed, result.Superseded = false, true
	return s.purgeMirrorHistoryGens(rootID, in.h.IssueID, st.gen, noMirrorPause)
}

// writeIdenticalHistory writes in, a history whose changes and times the
// issue's stored history holds, as a batch write writes it
// (mirrorHistoryTimesSQL): the status result it gives the card, and the
// receipt advanced, only when that result differs from the stored one. It
// reads the issue's state again in its transaction, so a write admitted
// since the probe (which moved the receipt) supersedes it.
func (s *Store) writeIdenticalHistory(rootID, sweep int64, in mirrorHistoryInput, result *MirrorHistoryResult, action string) error {
	listed, err := encodeMirrorJSON([]mirrorHistoryRow{in.row})
	if err != nil {
		return err
	}
	return s.mirrorSweptWrite(rootID, sweep, action, func(tx *sql.Tx) error {
		states, err := readMirrorHistoryStatesTx(tx, rootID, listed)
		if err != nil {
			return err
		}
		st, ok := states[in.h.IssueID]
		switch {
		case !ok || !st.kept:
			result.Unkept, result.Superseded = true, false
			return nil
		case st.supersedes(in):
			result.Unkept, result.Superseded = false, true
			return nil
		}
		row, err := encodeMirrorJSON([]mirrorHistoryRow{st.rowOf(in, st.gen)})
		if err != nil {
			return err
		}
		changed, err := queryIDs(tx, mirrorHistoryTimesSQL, rootID, row)
		if err != nil {
			return fmt.Errorf("write history times: %w", err)
		}
		result.Changed = len(changed) > 0
		return nil
	})
}

// writeStagedHistory writes in over the issue's stored history st, in
// place or staged; a staged one fails with errStaleReceipt at the switch,
// leaving its staged changes for the caller to delete, when the issue's
// receipt moved since st was read.
func (s *Store) writeStagedHistory(rootID int64, in mirrorHistoryInput, st mirrorHistoryState, action string) error {
	if st.storedFor == 0 {
		// No history is stored, so none of it can be kept in place.
		return s.stageMirrorHistory(rootID, in, st, action)
	}
	probe, err := s.probeMirrorHistory(rootID, in, st.gen)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !probe.differs && !probe.extra && len(probe.missing) <= clearMirrorChunk {
		return s.finishMirrorHistoryInPlace(rootID, in, st, probe.missing, action)
	}
	return s.stageMirrorHistory(rootID, in, st, action)
}

// stagedHistoryState reads the issue's stored history and records in
// result whether in is admitted: a kept issue whose receipt is in's.
func (s *Store) stagedHistoryState(rootID int64, in mirrorHistoryInput, result *MirrorHistoryResult) (mirrorHistoryState, bool, error) {
	listed, err := encodeMirrorJSON([]mirrorHistoryRow{in.row})
	if err != nil {
		return mirrorHistoryState{}, false, err
	}
	states, err := readMirrorHistoryStatesTx(s.db, rootID, listed)
	if err != nil {
		return mirrorHistoryState{}, false, fmt.Errorf("store: replace tracker mirror %d issue %s history: %w", rootID, in.h.IssueID, err)
	}
	st, ok := states[in.h.IssueID]
	switch {
	case !ok || !st.kept:
		result.Unkept, result.Superseded = true, false
		return st, false, nil
	case st.supersedes(in):
		result.Unkept, result.Superseded = false, true
		return st, false, nil
	}
	result.Unkept, result.Superseded = false, false
	return st, true, nil
}

// probeMirrorHistory compares in's changes with the issue's stored history
// at generation gen, a read of clearMirrorChunk changes at a time, each
// copied and encoded alone, and stops after the chunk that shows the
// history cannot be kept in place: no caller acts on more than that.
func (s *Store) probeMirrorHistory(rootID int64, in mirrorHistoryInput, gen int64) (mirrorHistoryProbe, error) {
	var probe mirrorHistoryProbe
	listed, err := encodeMirrorJSON([]mirrorHistoryRow{in.rowAt(gen)})
	if err != nil {
		return probe, err
	}
	var removes int
	if err := s.reader().QueryRow(`SELECT `+mirrorRemovalsSQL+` FROM json_each(?2) h
		CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = h.value ->> 'i'`, rootID, listed).Scan(&removes); err != nil {
		return probe, fmt.Errorf("count changes to remove: %w", err)
	}
	probe.extra = removes > 0
	chunk := make([]mirrorChangeRow, 0, clearMirrorChunk)
	for start := 0; start < len(in.changes) && !probe.differs && !probe.extra && len(probe.missing) <= clearMirrorChunk; start += clearMirrorChunk {
		chunk = in.chunkAt(gen, start, chunk)
		encoded, err := encodeMirrorJSON(chunk)
		if err != nil {
			return probe, err
		}
		err = scanMirrorRows(s.reader(), mirrorHistoryProbeSQL, []any{rootID, encoded}, func(r rowScanner) error {
			var i int
			var stored bool
			if err := r.Scan(&i, &stored); err != nil {
				return err
			}
			switch {
			case stored:
				probe.differs = true
			case !probe.differs && len(probe.missing) <= clearMirrorChunk:
				probe.missing = append(probe.missing, chunk[i])
			}
			return nil
		})
		if err != nil {
			return probe, fmt.Errorf("compare changes: %w", err)
		}
	}
	return probe, nil
}

// finishMirrorHistoryInPlace adds the missing changes of a history whose
// stored changes it otherwise holds, writes its times, advances the
// issue's receipt and clears history_dirty, in one transaction; what it
// writes is encoded before. It runs right after the admission that
// followed the staging mark, with the cleanup's lock, so no write moved
// the receipt since.
func (s *Store) finishMirrorHistoryInPlace(rootID int64, in mirrorHistoryInput, st mirrorHistoryState, missing []mirrorChangeRow, action string) error {
	rows, err := encodeMirrorJSON(missing)
	if err != nil {
		return err
	}
	row := st.rowOf(in, st.gen)
	// Called only for a history that differs from the stored one.
	row.Changes = true
	listed, err := encodeMirrorJSON([]mirrorHistoryRow{row})
	if err != nil {
		return err
	}
	return s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		if len(missing) > 0 {
			if _, err := tx.Exec(mirrorChangesUpsertSQL, rootID, rows); err != nil {
				return fmt.Errorf("add changes: %w", err)
			}
		}
		if _, err := tx.Exec(mirrorHistoryTimesSQL, rootID, listed); err != nil {
			return fmt.Errorf("write history times: %w", err)
		}
		if _, err := tx.Exec(`UPDATE tracker_cache_issues SET history_dirty = 0 WHERE root_id = ?1 AND issue_id = ?2`, rootID, in.h.IssueID); err != nil {
			return fmt.Errorf("clear the staging mark: %w", err)
		}
		return nil
	})
}

// stageMirrorHistory writes in's changes to generation st.gen+1 a chunk
// per transaction, makes it the issue's history with the status result it
// gives the card (statusOf), and deletes the old one.
func (s *Store) stageMirrorHistory(rootID int64, in mirrorHistoryInput, st mirrorHistoryState, action string) error {
	next := st.gen + 1
	status, since := st.statusOf(in)
	chunk := make([]mirrorChangeRow, 0, clearMirrorChunk)
	for start := 0; start < len(in.changes); start += clearMirrorChunk {
		chunk = in.chunkAt(next, start, chunk)
		encoded, err := encodeMirrorJSON(chunk)
		if err != nil {
			return err
		}
		err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
			if _, err := tx.Exec(mirrorChangesUpsertSQL, rootID, encoded); err != nil {
				return fmt.Errorf("stage changes: %w", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	err := s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		res, err := tx.Exec(mirrorHistorySwitchSQL, rootID, in.h.IssueID, next, in.h.CompleteFrom, in.h.For, in.h.BoardsRev,
			in.h.AnsweredAt, in.h.Wait, in.h.Receipt, int(status), since)
		if err != nil {
			return fmt.Errorf("make the staged history the issue's: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errors.Join(errStaleReceipt, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.purgeMirrorHistoryGens(rootID, in.h.IssueID, next, noMirrorPause)
}

// noMirrorPause is the between of a purge run inside a write: no pause,
// no stop.
func noMirrorPause() error { return nil }

// purgeMirrorHistoryGens deletes the issue's changes of every generation
// but keep, a chunk per transaction after between, and clears
// history_dirty in the transaction that leaves none, while keep is still
// the issue's generation. An error from between stops it.
func (s *Store) purgeMirrorHistoryGens(rootID int64, issueID string, keep int64, between func() error) error {
	action := fmt.Sprintf("store: delete tracker mirror %d issue %s replaced history", rootID, issueID)
	for done := false; !done; {
		if err := between(); err != nil {
			return err
		}
		err := s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
			res, err := tx.Exec(mirrorHistoryPurgeSQL, rootID, issueID, keep)
			if err != nil {
				return fmt.Errorf("delete changes: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if done = n < clearMirrorChunk; done {
				if _, err := tx.Exec(`UPDATE tracker_cache_issues SET history_dirty = 0
					WHERE root_id = ?1 AND issue_id = ?2 AND history_gen = ?3 AND history_dirty = 1`, rootID, issueID, keep); err != nil {
					return fmt.Errorf("clear the staging mark: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
