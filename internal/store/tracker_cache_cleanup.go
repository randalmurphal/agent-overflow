package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The mirror's deletes are bounded by structure: nothing with descendants
// whose number the write's input does not bound is deleted in a caller's
// transaction. A detached root (DetachMirror, a connection's removal), a
// retired board (its last listing gone) and an unkept issue (no row on a
// shown board and no link) are each absent to reads and refused by writes
// at once, and FinishMirrorCleanup deletes them later in transactions of
// at most clearMirrorChunk rows, with the generations of changes a history
// staging left (history_dirty).

// clearMirrorChunk bounds the rows one cleanup transaction deletes.
const clearMirrorChunk = 500

// mirrorSweepPage bounds the unkept issues one sweep transaction reads.
const mirrorSweepPage = 200

// mirrorTableKeys are the mirror's tables in the order a root's drain
// empties them, each with the columns that name one of its rows under a
// root: every table before the tables it references, so no batch cascades
// into another table. TestMirrorDrainCoversEveryTable holds it to the
// schema.
var mirrorTableKeys = []struct{ table, keys string }{
	{"tracker_cache_board_issues", "issue_id, board_id"},
	{"tracker_cache_issue_sprints", "issue_id, sprint_id"},
	{"tracker_cache_changes", "issue_id, gen, kind, seq"},
	{"tracker_cache_issue_details", "rowid"},
	{"tracker_cache_board_column_statuses", "board_id, status_id"},
	{"tracker_cache_board_columns", "board_id, position"},
	{"tracker_cache_sprints", "board_id, sprint_id"},
	{"tracker_cache_board_unexplained", "board_id, kind, ref_id"},
	{"tracker_cache_issues", "rowid"},
	{"tracker_cache_boards", "rowid"},
	{"tracker_cache_worklogs", "rowid"},
	{"tracker_cache_worklog_days", "author_id, day"},
	{"tracker_cache_sync", "rowid"},
	{"tracker_cache_users", "user_id"},
	{"tracker_cache_statuses", "status_id"},
}

// mirrorBoardTables are the tables a retired board's drain empties, in
// order, before it deletes the board.
var mirrorBoardTables = []string{
	"tracker_cache_board_issues", "tracker_cache_board_column_statuses",
	"tracker_cache_board_columns", "tracker_cache_sprints", "tracker_cache_board_unexplained",
}

// mirrorRootChunkSQL deletes one batch of a detached root's rows from each
// table, and mirrorBoardChunkSQL one batch of a retired board's: one fixed
// statement per table.
var mirrorRootChunkSQL, mirrorBoardChunkSQL = func() (root, board map[string]string) {
	limit := ` LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)`
	root, board = map[string]string{}, map[string]string{}
	for _, t := range mirrorTableKeys {
		// A rowid table's batch is found by rowid alone: with root_id
		// beside it the planner walks the root's rows to match the batch.
		outer := ` WHERE root_id = ?1 AND (` + t.keys + `) IN (SELECT `
		if t.keys == "rowid" {
			outer = ` WHERE rowid IN (SELECT `
		}
		root[t.table] = `DELETE FROM ` + t.table + outer + t.keys +
			` FROM ` + t.table + ` WHERE root_id = ?1` + limit +
			` AND EXISTS (SELECT 1 FROM tracker_cache_roots WHERE id = ?1 AND connection_id IS NULL)`
		if strings.Contains(t.keys, "board_id") {
			board[t.table] = `DELETE FROM ` + t.table + ` WHERE root_id = ?1 AND (` + t.keys + `) IN (SELECT ` + t.keys +
				` FROM ` + t.table + ` WHERE root_id = ?1 AND board_id = ?2` + limit +
				` AND EXISTS (SELECT 1 FROM tracker_cache_boards WHERE root_id = ?1 AND board_id = ?2 AND retired = 1)`
		}
	}
	return root, board
}()

// mirrorCleanupPendingSQL reports, in one statement, whether any cleanup
// is pending: a detached root, a retired board, an issue with generations
// of changes to delete, or an unkept issue. Each probe walks an index
// sized by what it finds; the last walks the issues no board row keeps,
// which are the linked ones when nothing is pending.
var mirrorCleanupPendingSQL = `SELECT EXISTS (SELECT 1 FROM tracker_cache_roots WHERE connection_id IS NULL)
	OR EXISTS (SELECT 1 FROM tracker_cache_boards WHERE retired = 1)
	OR EXISTS (SELECT 1 FROM tracker_cache_issues WHERE history_dirty = 1)
	OR EXISTS (SELECT 1 FROM tracker_cache_issues i WHERE i.board_rows = 0 AND NOT ` + mirrorIssueKept("i.root_id", "i.issue_id") + `)`

// mirrorDirtyHistoriesSQL lists the issues of live roots whose history
// staging left other generations, with the generation to keep.
const mirrorDirtyHistoriesSQL = `SELECT i.root_id, i.issue_id, i.history_gen FROM tracker_cache_issues i
	WHERE i.history_dirty = 1
	  AND EXISTS (SELECT 1 FROM tracker_cache_roots r WHERE r.id = i.root_id AND r.connection_id IS NOT NULL)`

// mirrorUnkeptRootsSQL lists the live roots with an unkept issue.
var mirrorUnkeptRootsSQL = `SELECT DISTINCT i.root_id FROM tracker_cache_issues i
	WHERE i.board_rows = 0 AND NOT ` + mirrorIssueKept("i.root_id", "i.issue_id") + `
	  AND EXISTS (SELECT 1 FROM tracker_cache_roots r WHERE r.id = i.root_id AND r.connection_id IS NOT NULL)`

// FinishMirrorCleanup deletes everything the mirror owes: the rows of every
// detached root and then the root, the rows of every retired board and
// then the board (starting a new lifetime of a board listed again), the
// generations of changes a history staging left, and every unkept issue
// with its changes, sprints and detail. Each transaction deletes at most
// clearMirrorChunk rows, and pause
// runs before each. It is idempotent, finishes what a crash or a failure
// stopped, and costs one statement when nothing is pending. The two reads
// that walk every root's boardless issues wait for a history read slot
// (mirrorReadSnapshot). Calls are serialized; a call that waited for
// another runs after it. A failure of one item does not stop the others:
// their errors are joined.
func (s *Store) FinishMirrorCleanup(pause ChunkPause) error {
	s.mirrorCleanup.Lock()
	defer s.mirrorCleanup.Unlock()
	pending, err := mirrorReadSnapshot(s, "check for tracker mirror cleanup", func(q sqlQueryer) (bool, error) {
		var pending bool
		err := q.QueryRow(mirrorCleanupPendingSQL).Scan(&pending)
		return pending, err
	})
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	between := func() {
		if pause != nil {
			pause()
		}
	}
	var errs []error
	roots, err := queryInt64s(s.reader(), `SELECT id FROM tracker_cache_roots WHERE connection_id IS NULL ORDER BY id`)
	errs = append(errs, err)
	for _, root := range roots {
		errs = append(errs, s.drainMirrorRoot(root, between))
	}
	boards, err := queryMirrorBoardRefs(s.reader(), `SELECT root_id, board_id FROM tracker_cache_boards WHERE retired = 1 ORDER BY root_id, board_id`)
	errs = append(errs, err)
	for _, board := range boards {
		errs = append(errs, s.drainMirrorBoard(board.root, board.board, between))
	}
	errs = append(errs, s.finishMirrorHistoryStagings(between))
	roots, err = mirrorReadSnapshot(s, "list tracker mirror roots with unkept issues", func(q sqlQueryer) ([]int64, error) {
		return queryInt64s(q, mirrorUnkeptRootsSQL)
	})
	errs = append(errs, err)
	for _, root := range roots {
		errs = append(errs, s.sweepMirrorRoot(root, between))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("store: finish tracker mirror cleanup: %w", err)
	}
	return nil
}

// drainMirrorChunks runs one chunk statement until a run deletes less than
// a chunk, each run its own transaction after between.
func (s *Store) drainMirrorChunks(query string, between func(), args ...any) error {
	for {
		between()
		result, err := s.db.Exec(query, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n < clearMirrorChunk {
			return nil
		}
	}
}

// drainMirrorRoot deletes a detached root's rows in batches, then the root.
func (s *Store) drainMirrorRoot(rootID int64, between func()) error {
	for _, t := range mirrorTableKeys {
		if err := s.drainMirrorChunks(mirrorRootChunkSQL[t.table], between, rootID); err != nil {
			return fmt.Errorf("drain detached tracker mirror %d: %s: %w", rootID, t.table, err)
		}
	}
	between()
	if _, err := s.db.Exec(`DELETE FROM tracker_cache_roots WHERE id = ? AND connection_id IS NULL`, rootID); err != nil {
		return fmt.Errorf("delete detached tracker mirror %d: %w", rootID, err)
	}
	return nil
}

// drainMirrorBoard deletes a retired board's rows, sprints and columns in
// batches, then the board, in the transaction that starts the board's next
// lifetime when a project lists it again. Its sync state went when it was
// retired.
func (s *Store) drainMirrorBoard(rootID int64, boardID string, between func()) error {
	for _, table := range mirrorBoardTables {
		if err := s.drainMirrorChunks(mirrorBoardChunkSQL[table], between, rootID, boardID); err != nil {
			return fmt.Errorf("drain retired tracker mirror %d board %s: %s: %w", rootID, boardID, table, err)
		}
	}
	between()
	action := fmt.Sprintf("store: delete retired tracker mirror %d board %s", rootID, boardID)
	return s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		deleted, err := execChangedTx(tx, `DELETE FROM tracker_cache_boards WHERE root_id = ?1 AND board_id = ?2 AND retired = 1
			RETURNING lifetime`, rootID, boardID)
		if err != nil || !deleted {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tracker_cache_boards(root_id, board_id) SELECT ?1, ?2 WHERE EXISTS (
			SELECT 1 FROM tracker_cache_roots r JOIN project_tracker_boards p ON p.connection_id = r.connection_id
			 WHERE r.id = ?1 AND p.board_id = ?2)`, rootID, boardID); err != nil {
			return fmt.Errorf("start the board's next lifetime: %w", err)
		}
		return nil
	})
}

// finishMirrorHistoryStagings deletes, for each issue of a live root whose
// history staging a crash or a failure stopped, its changes of every
// generation but its history's.
func (s *Store) finishMirrorHistoryStagings(between func()) error {
	type dirty struct {
		root    int64
		issue   string
		history int64
	}
	var issues []dirty
	err := scanMirrorRows(s.reader(), mirrorDirtyHistoriesSQL, nil, func(r rowScanner) error {
		var d dirty
		err := r.Scan(&d.root, &d.issue, &d.history)
		issues = append(issues, d)
		return err
	})
	errs := []error{err}
	for _, d := range issues {
		err := s.purgeMirrorHistoryGens(d.root, d.issue, d.history, between)
		if !errors.Is(err, ErrTrackerCacheGone) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepMirrorRoot removes a live root's unkept issues, a transaction at a
// time. A root detached meanwhile is left to its drain.
func (s *Store) sweepMirrorRoot(rootID int64, between func()) error {
	after := ""
	for {
		between()
		next, more, err := s.sweepMirrorPage(rootID, after)
		if errors.Is(err, ErrTrackerCacheGone) {
			return nil
		}
		if err != nil || !more {
			return err
		}
		after = next
	}
}

// mirrorSweepCandidatesSQL reads a page of a root's unkept issues by ID
// after ?2, from idx_tracker_cache_issues_boardless, each with its changes
// (at most one more than a chunk) and the rows its own delete cascades to:
// the issue, its sprints and its detail. A board row cannot cascade: the
// issue has none.
var mirrorSweepCandidatesSQL = `SELECT i.issue_id,
	  (SELECT count(*) FROM (SELECT 1 FROM tracker_cache_changes c WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id
	    LIMIT ` + strconv.Itoa(clearMirrorChunk+1) + `)),
	  1 + (SELECT count(*) FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id)
	    + (SELECT count(*) FROM tracker_cache_issue_details d WHERE d.root_id = i.root_id AND d.issue_id = i.issue_id)
	FROM tracker_cache_issues i
	WHERE i.root_id = ?1 AND i.board_rows = 0 AND i.issue_id > ?2 AND NOT ` + mirrorIssueKept("i.root_id", "i.issue_id") + `
	ORDER BY i.issue_id LIMIT ` + strconv.Itoa(mirrorSweepPage+1)

// sweepMirrorPage removes, in one transaction of at most clearMirrorChunk
// deleted rows, the unkept issues after after that fit, leaf first: an
// issue's changes, then the issue with its sprints and detail. An issue
// whose changes do not fit has a batch of them deleted and its history
// times and status time reset in the same transaction, so if it is kept
// again its history is read again and never shows in part. A page that
// deletes anything advances the root's sweep in its transaction, so a card,
// detail or history read before it is refused (ErrTrackerCacheSwept). next
// is the last issue fully removed (or after); more reports work after it.
func (s *Store) sweepMirrorPage(rootID int64, after string) (next string, more bool, err error) {
	action := fmt.Sprintf("store: sweep tracker mirror %d for unkept issues", rootID)
	next = after
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		type candidate struct {
			id               string
			changes, cascade int
		}
		var page []candidate
		err := scanMirrorRows(tx, mirrorSweepCandidatesSQL, []any{rootID, after}, func(r rowScanner) error {
			var c candidate
			err := r.Scan(&c.id, &c.changes, &c.cascade)
			page = append(page, c)
			return err
		})
		if err != nil {
			return fmt.Errorf("read unkept issues: %w", err)
		}
		if len(page) > mirrorSweepPage {
			page, more = page[:mirrorSweepPage], true
		}
		budget := clearMirrorChunk
		var whole []string
		partial, partialChanges := "", 0
		for _, c := range page {
			if cost := c.changes + c.cascade; cost <= budget {
				whole, budget, next = append(whole, c.id), budget-cost, c.id
				continue
			}
			if c.changes > 0 && budget > 0 {
				partial, partialChanges = c.id, min(budget, c.changes)
			}
			more = true
			break
		}
		if err := deleteMirrorIssuesTx(tx, rootID, whole); err != nil {
			return err
		}
		if len(whole) == 0 && partial == "" {
			return nil
		}
		if _, err := tx.Exec(`UPDATE tracker_cache_roots SET sweep = sweep + 1 WHERE id = ?1`, rootID); err != nil {
			return fmt.Errorf("advance sweep: %w", err)
		}
		if partial == "" {
			return nil
		}
		// The reset advances the receipt, so a history read listed before it
		// is superseded rather than written over the partial deletion.
		if _, err := tx.Exec(`UPDATE tracker_cache_issues SET history_from = 0, history_for = 0, history_answered_at = 0, history_wait = 0,
			status_since = 0, history_receipt = history_receipt + 1 WHERE root_id = ?1 AND issue_id = ?2
			AND (history_from, history_for, history_answered_at, history_wait, status_since) IS NOT (0, 0, 0, 0, 0)`,
			rootID, partial); err != nil {
			return fmt.Errorf("reset history of %s: %w", partial, err)
		}
		if _, err := tx.Exec(`DELETE FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id = ?2
			AND (gen, kind, seq) <= (SELECT gen, kind, seq FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id = ?2
			                          ORDER BY gen, kind, seq LIMIT 1 OFFSET ?3)`, rootID, partial, partialChanges-1); err != nil {
			return fmt.Errorf("delete changes of %s: %w", partial, err)
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return next, more, nil
}

// deleteMirrorIssuesTx deletes unkept issues with no board row, their
// changes first.
func deleteMirrorIssuesTx(tx *sql.Tx, rootID int64, issues []string) error {
	if len(issues) == 0 {
		return nil
	}
	encoded, err := encodeMirrorIDs(issues)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tracker_cache_changes WHERE root_id = ?1 AND issue_id IN (SELECT value FROM json_each(?2))`, rootID, encoded); err != nil {
		return fmt.Errorf("delete changes of unkept issues: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM tracker_cache_issues WHERE root_id = ?1 AND issue_id IN (SELECT value FROM json_each(?2))
		AND board_rows = 0`, rootID, encoded); err != nil {
		return fmt.Errorf("delete unkept issues: %w", err)
	}
	return nil
}

// mirrorBoardRef names a board of a root.
type mirrorBoardRef struct {
	root  int64
	board string
}

func queryMirrorBoardRefs(q sqlQueryer, query string, args ...any) ([]mirrorBoardRef, error) {
	var out []mirrorBoardRef
	err := scanMirrorRows(q, query, args, func(r rowScanner) error {
		var ref mirrorBoardRef
		err := r.Scan(&ref.root, &ref.board)
		out = append(out, ref)
		return err
	})
	return out, err
}

func queryInt64s(q sqlQueryer, query string, args ...any) ([]int64, error) {
	var out []int64
	err := scanMirrorRows(q, query, args, func(r rowScanner) error {
		var v int64
		err := r.Scan(&v)
		out = append(out, v)
		return err
	})
	return out, err
}
