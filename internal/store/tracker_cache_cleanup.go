package store

import (
	"context"
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
	{"tracker_cache_issue_bodies", "rowid"},
	{"tracker_cache_issue_missing", "issue_id"},
	{"tracker_cache_open_tickets", "session, pump"},
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
		outer := ` WHERE root_id = CAST(?1 AS INTEGER) AND (` + t.keys + `) IN (SELECT `
		if t.keys == "rowid" {
			outer = ` WHERE rowid IN (SELECT `
		}
		root[t.table] = `DELETE FROM ` + t.table + outer + t.keys +
			` FROM ` + t.table + ` WHERE root_id = CAST(?1 AS INTEGER)` + limit +
			` AND EXISTS (SELECT 1 FROM tracker_cache_roots WHERE id = CAST(?1 AS INTEGER) AND connection_id IS NULL)`
		if strings.Contains(t.keys, "board_id") {
			board[t.table] = `DELETE FROM ` + t.table + ` WHERE root_id = CAST(?1 AS INTEGER) AND (` + t.keys + `) IN (SELECT ` + t.keys +
				` FROM ` + t.table + ` WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2` + limit +
				` AND EXISTS (SELECT 1 FROM tracker_cache_boards WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND retired = 1)`
		}
	}
	return root, board
}()

// mirrorBoardRestartSQL deletes a retired board that its drain emptied:
// one with a row left in a table its drain empties is kept.
var mirrorBoardRestartSQL = func() string {
	q := `DELETE FROM tracker_cache_boards WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND retired = 1`
	for _, table := range mirrorBoardTables {
		q += ` AND NOT EXISTS (SELECT 1 FROM ` + table + ` WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2)`
	}
	return q + ` RETURNING lifetime`
}()

// mirrorCleanupPendingSQL reports, in one statement, whether any cleanup
// is pending: a detached root, a retired board, an issue with generations
// of changes to delete, an unkept issue, or an absence of an unkept issue.
// Each probe walks an index sized by what it finds; the unkept issue's
// walks the issues no board row keeps, which are the linked ones when
// nothing is pending, and the absence's every absence, which are of
// linked issues a read missed.
var mirrorCleanupPendingSQL = `SELECT EXISTS (SELECT 1 FROM tracker_cache_roots WHERE connection_id IS NULL)
	OR EXISTS (SELECT 1 FROM tracker_cache_boards WHERE retired = 1)
	OR EXISTS (SELECT 1 FROM tracker_cache_issues WHERE history_dirty = 1)
	OR EXISTS (SELECT 1 FROM tracker_cache_issues i WHERE i.board_rows = 0 AND NOT ` + mirrorIssueKept("i.root_id", "i.issue_id") + `)
	OR EXISTS (` + mirrorUnkeptAbsencesSQL + `)`

// mirrorUnkeptAbsencesSQL reads the absences of the issues their live
// root no longer keeps, mirrorAbsencesSweepSQL deletes them, at most
// clearMirrorChunk a statement: a walk of every absence, of the linked
// issues a read missed. A detached root's go with its drain.
var (
	mirrorUnkeptAbsencesSQL = `SELECT m.root_id, m.issue_id FROM tracker_cache_issue_missing m
	  JOIN tracker_cache_roots r ON r.id = m.root_id AND r.connection_id IS NOT NULL
	 WHERE NOT ` + mirrorIssueKept("m.root_id", "m.issue_id")
	mirrorAbsencesSweepSQL = `DELETE FROM tracker_cache_issue_missing WHERE (root_id, issue_id) IN (` + mirrorUnkeptAbsencesSQL +
		` LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)`
)

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
// generations of changes a history staging left, every unkept issue with
// its changes, sprints and body, and the absences of unkept issues. Each transaction deletes at most
// clearMirrorChunk rows, and pause runs before each. When ctx ends it
// stops before the next transaction and returns ctx's cause. It is
// idempotent, finishes what a crash, a failure or a stop left, and costs
// one statement when nothing is pending. The two reads that walk every
// root's boardless issues wait for a history read slot
// (mirrorReadSnapshot). Calls are serialized; a call that waited for
// another runs after it. A failure of one item does not stop the others:
// their errors are joined.
func (s *Store) FinishMirrorCleanup(ctx context.Context, pause ChunkPause) error {
	s.mirrorCleanup.Lock()
	defer s.mirrorCleanup.Unlock()
	if ctx.Err() != nil {
		return fmt.Errorf("store: finish tracker mirror cleanup: %w: %w", errMirrorCleanupStopped, context.Cause(ctx))
	}
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
	// The context is read after the pause, which may be long.
	between := func() error {
		if pause != nil {
			pause()
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", errMirrorCleanupStopped, context.Cause(ctx))
		}
		return nil
	}
	var errs []error
	var stop error
	// item runs one item of the cleanup unless the cleanup stopped, so a
	// stop ends it with one error.
	item := func(fn func() error) {
		if stop != nil {
			return
		}
		if err := fn(); errors.Is(err, errMirrorCleanupStopped) {
			stop = err
		} else {
			errs = append(errs, err)
		}
	}
	roots, err := queryInt64s(s.reader(), `SELECT id FROM tracker_cache_roots WHERE connection_id IS NULL ORDER BY id`)
	errs = append(errs, err)
	for _, root := range roots {
		item(func() error { return s.drainMirrorRoot(root, between) })
	}
	boards, err := queryMirrorBoardRefs(s.reader(), `SELECT root_id, board_id FROM tracker_cache_boards WHERE retired = 1 ORDER BY root_id, board_id`)
	errs = append(errs, err)
	for _, board := range boards {
		item(func() error { return s.drainMirrorBoard(board.root, board.board, between) })
	}
	item(func() error { return s.finishMirrorHistoryStagings(between) })
	if stop == nil {
		roots, err = mirrorReadSnapshot(s, "list tracker mirror roots with unkept issues", func(q sqlQueryer) ([]int64, error) {
			return queryInt64s(q, mirrorUnkeptRootsSQL)
		})
		errs = append(errs, err)
		for _, root := range roots {
			item(func() error { return s.sweepMirrorRoot(root, between) })
		}
		item(func() error { return s.sweepMirrorAbsences(between) })
	}
	if err := errors.Join(append(errs, stop)...); err != nil {
		return fmt.Errorf("store: finish tracker mirror cleanup: %w", err)
	}
	return nil
}

// sweepMirrorAbsences deletes the absences of the issues their root no
// longer keeps, a chunk a transaction, when there is one. The probe walks
// every absence, so it reads in a history read slot.
func (s *Store) sweepMirrorAbsences(between func() error) error {
	any, err := mirrorReadSnapshot(s, "probe tracker mirror absences no issue keeps", func(q sqlQueryer) (bool, error) {
		var any bool
		err := q.QueryRow(`SELECT EXISTS (` + mirrorUnkeptAbsencesSQL + `)`).Scan(&any)
		return any, err
	})
	if err != nil {
		return err
	}
	if !any {
		return nil
	}
	return s.drainMirrorChunks(mirrorAbsencesSweepSQL, between)
}

// errMirrorCleanupStopped is the error of a cleanup whose context ended
// before one of its transactions.
var errMirrorCleanupStopped = errors.New("stopped")

// drainMirrorChunks runs one chunk statement until a run deletes less than
// a chunk, each run its own transaction after between.
func (s *Store) drainMirrorChunks(query string, between func() error, args ...any) error {
	for {
		if err := between(); err != nil {
			return err
		}
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

// drainMirrorRoot deletes a detached root's rows in batches, then the root
// and its read clock.
func (s *Store) drainMirrorRoot(rootID int64, between func() error) error {
	for _, t := range mirrorTableKeys {
		if err := s.drainMirrorChunks(mirrorRootChunkSQL[t.table], between, rootID); err != nil {
			return fmt.Errorf("drain detached tracker mirror %d: %s: %w", rootID, t.table, err)
		}
	}
	if err := between(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM tracker_cache_roots WHERE id = ? AND connection_id IS NULL`, rootID); err != nil {
		return fmt.Errorf("delete detached tracker mirror %d: %w", rootID, err)
	}
	s.forgetMirrorRead(rootID)
	return nil
}

// ReadyTrackerBoards gives each of boards whose stored copy is retired
// its next lifetime, so a board a project lists again opens
// (TrackerBoardListing.Relisted): its rows, sprints and columns are
// drained in batches (drainMirrorBoard) and nothing else is. It does not
// wait for a cleanup another caller runs: every statement of a board's
// drain acts only on a retired board, so the two may drain one board at
// once. When ctx ends it stops before the next transaction and returns
// ctx's cause. The errors of the boards it could not ready are joined;
// the next FinishMirrorCleanup readies them.
func (s *Store) ReadyTrackerBoards(ctx context.Context, boards []TrackerBoardRef) error {
	between := func() error {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", errMirrorCleanupStopped, context.Cause(ctx))
		}
		return nil
	}
	var errs []error
	for _, ref := range boards {
		var root int64
		err := s.db.QueryRow(`SELECT b.root_id FROM tracker_cache_roots r JOIN tracker_cache_boards b ON b.root_id = r.id
			WHERE r.connection_id = ?1 AND b.board_id = ?2 AND b.retired = 1`, ref.ConnectionID, ref.BoardID).Scan(&root)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// Another drain readied it first.
			continue
		case err == nil:
			err = s.drainMirrorBoard(root, ref.BoardID, between)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("board %s of connection %s: %w", ref.BoardID, ref.ConnectionID, err))
		}
		if errors.Is(err, errMirrorCleanupStopped) {
			break
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("store: ready tracker boards listed again: %w", err)
	}
	return nil
}

// drainMirrorBoard deletes a retired board's rows, sprints and columns in
// batches, then the board, in the transaction that starts the board's next
// lifetime when a project lists it again. Its sync state went when it was
// retired. The board is deleted only while retired and empty, so a drain
// that another drain of the board overtook, after which the board lived
// again, filled and was retired again, deletes nothing: its delete would
// cascade through every row of the new lifetime in one transaction.
func (s *Store) drainMirrorBoard(rootID int64, boardID string, between func() error) error {
	for _, table := range mirrorBoardTables {
		if err := s.drainMirrorChunks(mirrorBoardChunkSQL[table], between, rootID, boardID); err != nil {
			return fmt.Errorf("drain retired tracker mirror %d board %s: %s: %w", rootID, boardID, table, err)
		}
	}
	if err := between(); err != nil {
		return err
	}
	action := fmt.Sprintf("store: delete retired tracker mirror %d board %s", rootID, boardID)
	return s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		deleted, err := execChangedTx(tx, mirrorBoardRestartSQL, rootID, boardID)
		if err != nil || !deleted {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tracker_cache_boards(root_id, board_id) SELECT ?1, ?2 WHERE EXISTS (
			SELECT 1 FROM tracker_cache_roots r JOIN project_tracker_boards p ON p.connection_id = r.connection_id
			 WHERE r.id = CAST(?1 AS INTEGER) AND p.board_id = ?2)`, rootID, boardID); err != nil {
			return fmt.Errorf("start the board's next lifetime: %w", err)
		}
		return nil
	})
}

// finishMirrorHistoryStagings deletes, for each issue of a live root whose
// history staging a crash or a failure stopped, its changes of every
// generation but its history's. A stop (an error from between) ends it.
func (s *Store) finishMirrorHistoryStagings(between func() error) error {
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
		if errors.Is(err, errMirrorCleanupStopped) {
			return err
		}
		if !errors.Is(err, ErrTrackerCacheGone) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepMirrorRoot removes a live root's unkept issues, a transaction at a
// time. A root detached meanwhile is left to its drain. Every page either
// moves past an issue or deletes a row (sweepMirrorPage fails otherwise),
// so the pages end.
func (s *Store) sweepMirrorRoot(rootID int64, between func() error) error {
	after := ""
	for {
		if err := between(); err != nil {
			return err
		}
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
// and its sprints (each counted to at most one more than a chunk) and
// whether it has a body: with the issue itself, what its delete cascades
// to. A board row cannot cascade: the issue has none.
var mirrorSweepCandidatesSQL = `SELECT i.issue_id,
	  (SELECT count(*) FROM (SELECT 1 FROM tracker_cache_changes c WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id
	    LIMIT ` + strconv.Itoa(clearMirrorChunk+1) + `)),
	  (SELECT count(*) FROM (SELECT 1 FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id
	    LIMIT ` + strconv.Itoa(clearMirrorChunk+1) + `)),
	  i.body_read_at > 0
	FROM tracker_cache_issues i
	WHERE i.root_id = CAST(?1 AS INTEGER) AND i.board_rows = 0 AND i.issue_id > ?2 AND NOT ` + mirrorIssueKept("i.root_id", "i.issue_id") + `
	ORDER BY i.issue_id LIMIT ` + strconv.Itoa(mirrorSweepPage+1)

// mirrorSweepSQL are the statements of a sweep page's partial removal of
// an issue, each of ?1 root, ?2 issue and ?3 the rows to delete, the
// first ?3 by key. Removing changes resets the issue's history times and
// status time with the read orders of its evidence (mirrorStatusWriteSQL),
// and moves its receipt, so a history read listed before it
// is superseded rather than written over the removal, and an issue kept
// again has its history read again and never shows it in part. Removing
// sprints sets sprint_set to mirrorSprintSetPartial, so the next write
// that admits the issue's card replaces its sprints whatever it names.
var mirrorSweepSQL = struct{ resetHistory, changes, partialSprints, sprints string }{
	resetHistory: `UPDATE tracker_cache_issues SET history_from = 0, history_for = 0, history_answered_at = 0, history_wait = 0,
		status_since = 0, status_final_for = 0, status_floor_at = 0, status_read_at = 0, history_receipt = history_receipt + 1
		WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2
		AND (history_from, history_for, history_answered_at, history_wait, status_since, status_final_for, status_floor_at, status_read_at)
		    IS NOT (0, 0, 0, 0, 0, 0, 0, 0)`,
	changes: `DELETE FROM tracker_cache_changes WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2
		AND (gen, kind, seq) <= (SELECT gen, kind, seq FROM tracker_cache_changes WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2
		                          ORDER BY gen, kind, seq LIMIT 1 OFFSET ?3 - 1)`,
	partialSprints: `UPDATE tracker_cache_issues SET sprint_set = x'` + mirrorSprintSetPartial + `'
		WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2 AND sprint_set IS NOT x'` + mirrorSprintSetPartial + `'`,
	sprints: `DELETE FROM tracker_cache_issue_sprints WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2
		AND sprint_id <= (SELECT sprint_id FROM tracker_cache_issue_sprints WHERE root_id = CAST(?1 AS INTEGER) AND issue_id = ?2
		                   ORDER BY sprint_id LIMIT 1 OFFSET ?3 - 1)`,
}

// sweepMirrorPage removes, in one transaction of at most clearMirrorChunk
// deleted rows, the unkept issues after after that fit, leaf first: an
// issue's changes and sprints, then the issue with its body. Of the
// first issue that does not fit it removes what the rest of the budget
// holds, changes first, then sprints (mirrorSweepSQL), so a page never
// stops at an issue it cannot finish: the page's first issue always
// leaves it with a row deleted. A page that deletes anything advances the
// root's sweep in its transaction, so a card, body or history read
// before it is refused (ErrTrackerCacheSwept). next is the last issue
// fully removed (or after); more reports work after it. A page with work
// after it that neither moves past an issue nor deletes a row fails, so
// its caller never reads the same page again.
func (s *Store) sweepMirrorPage(rootID int64, after string) (next string, more bool, err error) {
	action := fmt.Sprintf("store: sweep tracker mirror %d for unkept issues", rootID)
	next = after
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		type candidate struct {
			id               string
			changes, sprints int
			body             bool
		}
		var page []candidate
		err := scanMirrorRows(tx, mirrorSweepCandidatesSQL, []any{rootID, after}, func(r rowScanner) error {
			var c candidate
			err := r.Scan(&c.id, &c.changes, &c.sprints, &c.body)
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
		var partial candidate
		for _, c := range page {
			cost := c.changes + c.sprints + 1
			if c.body {
				cost++
			}
			if cost <= budget {
				whole, budget, next = append(whole, c.id), budget-cost, c.id
				continue
			}
			partial, more = c, true
			break
		}
		deleted, err := deleteMirrorIssuesTx(tx, rootID, whole)
		if err != nil {
			return err
		}
		if partial.id != "" {
			for _, step := range []struct {
				what         string
				held         int
				mark, delete string
			}{
				{"changes", partial.changes, mirrorSweepSQL.resetHistory, mirrorSweepSQL.changes},
				{"sprints", partial.sprints, mirrorSweepSQL.partialSprints, mirrorSweepSQL.sprints},
			} {
				n := min(budget, step.held)
				if n == 0 {
					continue
				}
				if _, err := tx.Exec(step.mark, rootID, partial.id); err != nil {
					return fmt.Errorf("mark %s of %s removed in part: %w", step.what, partial.id, err)
				}
				removed, err := execCountTx(tx, step.delete, rootID, partial.id, n)
				if err != nil {
					return fmt.Errorf("delete %s of %s: %w", step.what, partial.id, err)
				}
				deleted, budget = deleted+removed, budget-n
				if n < step.held {
					break
				}
			}
		}
		if deleted == 0 {
			if more && next == after {
				return fmt.Errorf("a sweep page after %q deleted nothing and moved past no issue", after)
			}
			return nil
		}
		if _, err := tx.Exec(`UPDATE tracker_cache_roots SET sweep = sweep + 1 WHERE id = CAST(?1 AS INTEGER)`, rootID); err != nil {
			return fmt.Errorf("advance sweep: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return next, more, nil
}

// deleteMirrorIssuesTx deletes unkept issues with no board row, their
// changes first, and returns how many rows the deletes removed, the
// cascades not counted.
func deleteMirrorIssuesTx(tx *sql.Tx, rootID int64, issues []string) (int, error) {
	if len(issues) == 0 {
		return 0, nil
	}
	encoded, err := encodeMirrorIDs(issues)
	if err != nil {
		return 0, err
	}
	changes, err := execCountTx(tx, `DELETE FROM tracker_cache_changes WHERE root_id = CAST(?1 AS INTEGER) AND issue_id IN (SELECT value FROM json_each(?2))`, rootID, encoded)
	if err != nil {
		return 0, fmt.Errorf("delete changes of unkept issues: %w", err)
	}
	removed, err := execCountTx(tx, `DELETE FROM tracker_cache_issues WHERE root_id = CAST(?1 AS INTEGER) AND issue_id IN (SELECT value FROM json_each(?2))
		AND board_rows = 0`, rootID, encoded)
	if err != nil {
		return 0, fmt.Errorf("delete unkept issues: %w", err)
	}
	return changes + removed, nil
}

// execCountTx runs query and returns the rows it changed.
func execCountTx(tx *sql.Tx, query string, args ...any) (int, error) {
	result, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
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
