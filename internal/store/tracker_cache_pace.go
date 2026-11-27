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
//
// With credit (MirrorRegroup.WithCredit) it also holds the sprint's credit
// (mirrorCreditRowsSQL) beside what it finished: the estimate its review
// and support credit add (Review, Support) and the same in tickets
// (ReviewShares, SupportShares), and Unread, the tickets whose review or
// support credit is not counted.
type MirrorSprintFinished struct {
	SprintID               string
	Members                int
	Done                   int
	Estimate               float64
	Review, ReviewShares   float64
	Support, SupportShares float64
	Unread                 int
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
//     (mirrorHistoryReadForCard: the status never changed) or this sprint is the
//     latest sprint of the board, closed or open, the issue is in (it was
//     carried nowhere after it). Otherwise the member is not done: an
//     issue carried on from the sprint, its history unread, finished in a
//     later sprint if at all.
//   - It is part of the section totals (countMirrorSectionsTx), so it is
//     stale exactly when they are: the board's totals_revision advances on
//     every change of its inputs, rows, members, sprints and their states,
//     estimates, statuses and their groups by trigger, a history write
//     of an issue on the board that is in one of its closed sprints by
//     advanceMirrorPaceTx, and, for its credit, an evidence write that
//     changes what is read of an issue on the board (replaceMirrorActsTx).
//
// mirrorPaceSQL reads those sprints of board ?2 of root ?1, each member's
// status at its completion and its estimate, and counts and sums those
// done by the regrouping ?4 (a JSON object, '{}' for none): the list of
// done statuses is one uncorrelated subquery, built once a statement, as
// mirrorRegroupedNotDone's is. Each change is read in the changes' key
// order, which is each kind's oldest first (MirrorHistory). ?3 is the
// board's estimate reset.
var mirrorPaceSQL = `WITH ` + mirrorPaceLastSQL + `,
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

// mirrorPaceLastSQL is the CTE last of the pace's sprints: board ?2 of
// root ?1's last MirrorPaceSprints closed sprints by completion.
var mirrorPaceLastSQL = `last AS MATERIALIZED (
	  SELECT sprint_id, completed_at FROM tracker_cache_sprints
	   WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = ` + strconv.Itoa(MirrorSprintClosed) + ` AND completed_at > 0
	   ORDER BY completed_at DESC, position DESC LIMIT ` + strconv.Itoa(MirrorPaceSprints) + `)`

// mirrorPaceCreditSQL is mirrorPaceSQL with credit: each of the sprints
// of last, what it finished as mirrorPaceSQL counts it (its members' done
// by the pace's rule, cr_m) but for its support tickets, which count by
// their time, and its credit rows' sums (cr_rows), ?5 the support rule
// and ?6 the credit rules. It reads closed sprints alone, whose acts end
// at their completion, so it needs no clock. mirrorPaceCreditPlainSQL is
// it for a project that names no support tickets, with no support term.
var mirrorPaceCreditSQL, mirrorPaceCreditPlainSQL = mirrorPaceCredit(true), mirrorPaceCredit(false)

func mirrorPaceCredit(support bool) string {
	p := mirrorCreditParams{groups: "?4", reset: "?3", rules: "?6", from: "0", until: mirrorExportNever}
	if support {
		p.support = "?5"
	}
	chosen := func(alias string) string { return alias + `.sprint_id IN (SELECT sprint_id FROM last)` }
	with, rows := mirrorCreditSQL(p, mirrorPaceLastSQL, chosen, false)
	return with + `,
	  cr_rows(` + mirrorCreditColumns + `) AS (` + rows + `)
	SELECT l.sprint_id, coalesce(f.members, 0), coalesce(f.done, 0), coalesce(f.estimate, 0), coalesce(c.review, 0), coalesce(c.review_shares, 0),
	  coalesce(c.support, 0), coalesce(c.support_shares, 0), coalesce(c.unread, 0)
	FROM last l
	LEFT JOIN (SELECT sprint_id, count(*) AS members, total(done = 1 AND NOT support) AS done, total(iif(done = 1 AND NOT support, points, 0)) AS estimate
	    FROM cr_m GROUP BY sprint_id) f
	  ON f.sprint_id = l.sprint_id
	LEFT JOIN (SELECT sprint, total(iif(kind = 'review', value, 0)) AS review, total(iif(kind = 'review', share, 0)) AS review_shares,
	    total(iif(kind = 'support', value, 0)) AS support, total(iif(kind = 'support', share, 0)) AS support_shares,
	    count(DISTINCT iif(value IS NULL AND kind <> 'finished', ticket, NULL)) AS unread
	  FROM cr_rows GROUP BY sprint) c ON c.sprint = l.sprint_id`
}

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
	    iif((` + mirrorHistoryReadForCard("i") + `) OR NOT EXISTS (SELECT 1 FROM tracker_cache_issue_sprints later
	        CROSS JOIN tracker_cache_sprints sp ON sp.root_id = later.root_id AND sp.board_id = ?2 AND sp.sprint_id = later.sprint_id
	       WHERE later.root_id = i.root_id AND later.issue_id = i.issue_id AND later.sprint_id <> l.sprint_id
	         AND (sp.state IN (` + strconv.Itoa(MirrorSprintActive) + `, ` + strconv.Itoa(MirrorSprintFuture) + `) OR sp.completed_at > l.completed_at)),
	      i.status_id, NULL))`

// mirrorDoneStatusesSQL lists the statuses of root ?1 that count as done
// under the regrouping g binds (a JSON object): the catalog's done
// statuses g does not regroup, and those g puts in done.
func mirrorDoneStatusesSQL(g string) string { return mirrorGroupStatusesSQL(g, MirrorGroupDone) }

// mirrorGroupStatusesSQL lists the statuses of root ?1 in group under the
// regrouping g binds (a JSON object): the catalog's statuses of the group
// that g does not regroup, and those g puts in it. It is one uncorrelated
// subquery, built once a statement.
func mirrorGroupStatusesSQL(g string, group int) string {
	return `SELECT st.status_id FROM tracker_cache_statuses st
	   WHERE st.root_id = CAST(?1 AS INTEGER) AND st.status_group = ` + strconv.Itoa(group) + ` AND st.status_id NOT IN (SELECT o.key FROM json_each(` + g + `) o)
	   UNION ALL SELECT o.key FROM json_each(` + g + `) o WHERE o.value = ` + strconv.Itoa(group)
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
// finished (mirrorPaceSQL), with its credit when the regrouping counts
// credit (mirrorPaceCreditSQL), in no order. A board without estimates
// adds no estimate.
func readMirrorPaceTx(tx sqlQueryer, rootID int64, boardID string, board mirrorShownBoard, regroup MirrorRegroup) ([]MirrorSprintFinished, error) {
	groups := regroup.statuses
	if groups == "" {
		groups = "{}"
	}
	estimated := board.unit != MirrorEstimateNone
	query, args := mirrorPaceSQL, []any{rootID, boardID, board.estimateResetAt, groups}
	switch {
	case regroup.credit != "" && regroup.support == "":
		query, args = mirrorPaceCreditPlainSQL, append(args, nil, regroup.credit)
	case regroup.credit != "":
		query, args = mirrorPaceCreditSQL, append(args, regroup.supportArg(), regroup.credit)
	}
	var out []MirrorSprintFinished
	err := scanMirrorRows(tx, query, args, func(r rowScanner) error {
		var f MirrorSprintFinished
		var done float64
		dest := []any{&f.SprintID, &f.Members, &done, &f.Estimate}
		if regroup.credit != "" {
			dest = append(dest, &f.Review, &f.ReviewShares, &f.Support, &f.SupportShares, &f.Unread)
		}
		if err := r.Scan(dest...); err != nil {
			return err
		}
		f.Done = int(done)
		if !estimated {
			f.Estimate, f.Review, f.Support = 0, 0, 0
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

// MirrorBoardPace is a shown board's pace as its section totals carry it
// (MirrorSectionCounts.Pace): its estimate unit (MirrorEstimate*), whether
// it plans sprints, and what each of its last finished sprints finished,
// none for a board that plans none.
type MirrorBoardPace struct {
	Unit         int
	PlansSprints bool
	Finished     []MirrorSprintFinished
}

// GetMirrorBoardPace reads a board's pace by the regrouping, in one
// snapshot, as CountMirrorSections reads it; ok is false for a board the
// mirror does not show.
func (s *Store) GetMirrorBoardPace(rootID int64, boardID string, regroup MirrorRegroup) (MirrorBoardPace, bool, error) {
	label := fmt.Sprintf("read tracker mirror %d board %s pace", rootID, boardID)
	pace, err := mirrorReadSnapshot(s, label, func(tx sqlQueryer) (*MirrorBoardPace, error) {
		board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
		if err != nil || !shown {
			return nil, err
		}
		out := &MirrorBoardPace{Unit: board.unit, PlansSprints: board.plansSprints}
		if board.plansSprints {
			if out.Finished, err = readMirrorPaceTx(tx, rootID, boardID, board, regroup); err != nil {
				return nil, fmt.Errorf("pace: %w", err)
			}
		}
		return out, nil
	})
	if err != nil || pace == nil {
		return MirrorBoardPace{}, false, err
	}
	return *pace, true, nil
}
