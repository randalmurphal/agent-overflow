package store

import (
	"database/sql"
	"fmt"
	"strconv"
)

// MirrorPaceSprints is how many finished sprints a board's pace averages:
// its last ones by completion.
const MirrorPaceSprints = 5

// MirrorSprintFinished is what one closed sprint of a board finished: its
// members on the board, those whose status when the sprint was completed
// counted as done, and the sum of the board's estimates of those, as the
// board holds them now (a row with no estimate, or one read no later than
// the estimate definition last changed, adds nothing). A sprint none of
// whose members the board holds finished nothing: the mirror keeps a
// board's closed sprints with their members for the history window.
type MirrorSprintFinished struct {
	SprintID string
	Members  int
	Done     int
	Estimate float64
}

// The pace of a board, what must hold:
//   - It averages the last MirrorPaceSprints closed sprints of the board by
//     completion, every one of them, a sprint with no member on the board
//     as zero.
//   - A member counts as done when its status at the sprint's completion
//     counted as done by the regrouping over the catalog's groups. That
//     status is the last status change at or before the completion, else
//     the first change after it is from, both of the issue's current
//     history generation; with no such change, it is the status now only
//     when the issue's history was read for its current version
//     (history_for > 0: the status never changed) or this sprint is the
//     latest sprint of the board, closed or open, the issue is in (it was
//     carried nowhere after it). Otherwise the member is not done: an
//     issue carried on from the sprint, its history unread, finished in a
//     later sprint if at all.
//   - It is part of the section totals (countMirrorSectionsTx), so it is
//     stale exactly when they are: the board's totals_revision advances on
//     every change of its inputs, rows, members, sprints and their states,
//     estimates, statuses and their groups by trigger, and a history write
//     of an issue on the board that is in one of its closed sprints by
//     advanceMirrorPaceTx.
//
// mirrorPaceSQL reads those sprints of board ?2 of root ?1, each member's
// status at its completion and its estimate, and counts and sums those
// done by the regrouping ?4 (a JSON object, '{}' for none): the list of
// done statuses is one uncorrelated subquery, built once a statement, as
// mirrorRegroupedNotDone's is. Each change is read in the changes' key
// order, which is each kind's oldest first (MirrorHistory). ?3 is the
// board's estimate reset.
var mirrorPaceSQL = `WITH last AS MATERIALIZED (
	  SELECT sprint_id, completed_at FROM tracker_cache_sprints
	   WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = ` + strconv.Itoa(MirrorSprintClosed) + ` AND completed_at > 0
	   ORDER BY completed_at DESC, position DESC LIMIT ` + strconv.Itoa(MirrorPaceSprints) + `),
	members AS (
	  SELECT l.sprint_id, ` + mirrorRowEstimate("?3") + ` AS estimate, ` + mirrorStatusAtCompletionSQL + ` AS status
	  FROM last l
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = l.sprint_id
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ?2 AND b.issue_id = m.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = m.root_id AND i.issue_id = m.issue_id),
	finished AS (
	  SELECT sprint_id, count(*) AS members, total(done) AS done, total(iif(done, estimate, 0)) AS estimate FROM (
	    SELECT sprint_id, estimate, coalesce(status IN (` + mirrorDoneStatusesSQL("?4") + `), 0) AS done
	    FROM members)
	  GROUP BY sprint_id)
	SELECT l.sprint_id, coalesce(f.members, 0), coalesce(f.done, 0), coalesce(f.estimate, 0)
	FROM last l LEFT JOIN finished f ON f.sprint_id = l.sprint_id`

// mirrorStatusAtCompletionSQL is the status issue i had when sprint l
// (its sprint_id and completed_at) of board ?2 was completed, by the
// pace's rule: the last status change at or before the completion, else
// what the first change after it is from, both of the issue's current
// history generation; with no such change, the status now when the
// issue's history was read for its current version or no later sprint of
// the board, closed or open, holds the issue; else NULL (unknown). The
// pace and the board export (mirrorExportFills) read it, so a dashboard's
// pace is the Backlog's.
var mirrorStatusAtCompletionSQL = `coalesce(
	    (SELECT c.to_value FROM tracker_cache_changes c
	      WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = 1 AND c.at <= l.completed_at
	      ORDER BY c.seq DESC LIMIT 1),
	    (SELECT c.from_value FROM tracker_cache_changes c
	      WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = 1 AND c.at > l.completed_at
	      ORDER BY c.seq LIMIT 1),
	    iif(i.history_for > 0 OR NOT EXISTS (SELECT 1 FROM tracker_cache_issue_sprints later
	        CROSS JOIN tracker_cache_sprints sp ON sp.root_id = later.root_id AND sp.board_id = ?2 AND sp.sprint_id = later.sprint_id
	       WHERE later.root_id = i.root_id AND later.issue_id = i.issue_id AND later.sprint_id <> l.sprint_id
	         AND (sp.state IN (` + strconv.Itoa(MirrorSprintActive) + `, ` + strconv.Itoa(MirrorSprintFuture) + `) OR sp.completed_at > l.completed_at)),
	      i.status_id, NULL))`

// mirrorDoneStatusesSQL lists the statuses of root ?1 that count as done
// under the regrouping g binds (a JSON object): the catalog's done
// statuses g does not regroup, and those g puts in done. It is one
// uncorrelated subquery, built once a statement.
func mirrorDoneStatusesSQL(g string) string {
	return `SELECT st.status_id FROM tracker_cache_statuses st
	   WHERE st.root_id = CAST(?1 AS INTEGER) AND st.status_group = ` + strconv.Itoa(MirrorGroupDone) + ` AND st.status_id NOT IN (SELECT o.key FROM json_each(` + g + `) o)
	   UNION ALL SELECT o.key FROM json_each(` + g + `) o WHERE o.value = ` + strconv.Itoa(MirrorGroupDone)
}

// mirrorPaceAdvanceSQL advances the totals revision of each board of root
// ?1 that holds a row of an issue of the JSON list ?2 that is in one of the
// board's closed sprints, each row and membership found by key.
var mirrorPaceAdvanceSQL = `UPDATE tracker_cache_boards SET totals_revision = totals_revision + 1
	WHERE root_id = CAST(?1 AS INTEGER) AND board_id IN (
	  SELECT b.board_id FROM json_each(?2) l
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = CAST(?1 AS INTEGER) AND b.issue_id = l.value
	  WHERE EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	    CROSS JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
	   WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND sp.state = ` + strconv.Itoa(MirrorSprintClosed) + `))`

// advanceMirrorPaceTx tells the boards whose pace a history write of
// issues may have moved (mirrorPaceAdvanceSQL), in the write's
// transaction; issues is a JSON list, empty for none.
func advanceMirrorPaceTx(tx *sql.Tx, rootID int64, issues string) error {
	if issues == "" {
		return nil
	}
	if _, err := tx.Exec(mirrorPaceAdvanceSQL, rootID, issues); err != nil {
		return fmt.Errorf("advance the totals of the boards whose pace the history moved: %w", err)
	}
	return nil
}

// readMirrorPaceTx reads what each of the last finished sprints of a board
// finished (mirrorPaceSQL), in no order.
func readMirrorPaceTx(tx sqlQueryer, rootID int64, boardID string, board mirrorShownBoard, regroup MirrorRegroup) ([]MirrorSprintFinished, error) {
	groups := regroup.statuses
	if groups == "" {
		groups = "{}"
	}
	estimated := board.unit != MirrorEstimateNone
	var out []MirrorSprintFinished
	err := scanMirrorRows(tx, mirrorPaceSQL, []any{rootID, boardID, board.estimateResetAt, groups}, func(r rowScanner) error {
		var f MirrorSprintFinished
		var done float64
		if err := r.Scan(&f.SprintID, &f.Members, &done, &f.Estimate); err != nil {
			return err
		}
		f.Done = int(done)
		if !estimated {
			f.Estimate = 0
		}
		out = append(out, f)
		return nil
	})
	return out, err
}
