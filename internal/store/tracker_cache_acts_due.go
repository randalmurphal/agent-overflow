package store

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
)

// mirrorActsDuePage bounds one ListMirrorActsDue read: what one evidence
// unit reads (trackerapp's evidenceBatches reads of evidenceBatch
// issues).
const mirrorActsDuePage = 4 * mirrorHistoryBatch

// MirrorActsBoard is a board whose tickets credit counts, with the
// regrouping its project counts the board's site by, the support rule
// included (MirrorRegroup).
type MirrorActsBoard struct {
	BoardID string
	Regroup MirrorRegroup
}

// MirrorActsDue is an issue whose acts are due a read. Support reports
// that a board lists it as a support ticket (mirrorSupportSQL), whose
// worklogs are read from the window only.
type MirrorActsDue struct {
	IssueID string
	Support bool
}

// The arms of the acts due rule, in the order ListMirrorActsDue lists
// them.
const (
	mirrorActsArmActive = iota
	mirrorActsArmClosed
	mirrorActsArmSupport
)

// mirrorActsDueWhere holds for an issue i whose acts are due: no receipt
// of its comments, or one read for an update before the card's, and the
// same of its worklogs when ?6 is 1. A receipt's read_for is the update
// time the read returned, so a read that found a later update than the
// card's leaves the issue not due.
func mirrorActsDueWhere(i string) string {
	receipt := func(source int) string {
		return `NOT EXISTS (SELECT 1 FROM tracker_cache_issue_act_reads ar WHERE ar.root_id = ` + i + `.root_id AND ar.issue_id = ` + i + `.issue_id
		    AND ar.source = ` + strconv.Itoa(source) + ` AND ar.read_for >= ` + i + `.updated_at)`
	}
	return `(` + receipt(MirrorActComment) + ` OR CAST(?6 AS INTEGER) = 1 AND ` + receipt(MirrorActWorklog) + `)`
}

// mirrorActsDueSQL lists the issues of board ?2 of root ?1 whose acts are
// due (mirrorActsDueWhere), each arm at most a page and one, with its arm,
// its order within the arm (larger first) and whether the support rule ?4
// names its row (mirrorSupportSQL), by the regrouping ?3 (a JSON object)
// and the board's estimate reset ?5:
//   - active: members of the board's active sprints whose status counts
//     as done now, by update time;
//   - closed: members of its sprints closed at or after ?7 whose status at
//     the completion counted as done (mirrorStatusAtCompletionSQL: one
//     whose history is unread and was carried on is not done yet), by
//     completion;
//   - support: the board's rows the support rule names, by update time.
//
// Each tests the due rule, a probe of a receipt by key, before the rest,
// so a listing in the steady state, where almost nothing is due, reads
// each candidate's receipts and no history.
var mirrorActsDueSQL = func() string {
	limit := ` LIMIT ` + strconv.Itoa(mirrorActsDuePage+1)
	due := mirrorActsDueWhere("i")
	support := mirrorSupportSQL("?4", "?2", mirrorRowEstimate("?5"))
	members := func(sprint string) string {
		return ` CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = ` + sprint + `.root_id AND m.sprint_id = ` + sprint + `.sprint_id
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ` + sprint + `.board_id AND b.issue_id = m.issue_id
		  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id`
	}
	active := `SELECT * FROM (SELECT i.issue_id, ` + strconv.Itoa(mirrorActsArmActive) + `, i.updated_at, ` + support + `
		  FROM tracker_cache_sprints sp` + members("sp") + `
		 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND sp.state = ` + strconv.Itoa(MirrorSprintActive) + `
		   AND ` + due + ` AND i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `)
		 ORDER BY i.updated_at DESC` + limit + `)`
	closed := `SELECT * FROM (SELECT i.issue_id, ` + strconv.Itoa(mirrorActsArmClosed) + `, l.completed_at, ` + support + `
		  FROM tracker_cache_sprints l` + members("l") + `
		 WHERE l.root_id = CAST(?1 AS INTEGER) AND l.board_id = ?2 AND l.state = ` + strconv.Itoa(MirrorSprintClosed) + `
		   AND l.completed_at >= ?7 AND ` + due + `
		   AND coalesce(` + mirrorStatusAtCompletionSQL + ` IN (` + mirrorDoneStatusesSQL("?3") + `), 0)
		 ORDER BY l.completed_at DESC` + limit + `)`
	supported := `SELECT * FROM (SELECT i.issue_id, ` + strconv.Itoa(mirrorActsArmSupport) + `, i.updated_at, 1
		  FROM tracker_cache_board_issues b
		  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		 WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND ?4 IS NOT NULL AND ` + due + ` AND ` + support + `
		 ORDER BY i.updated_at DESC` + limit + `)`
	return active + "\n\tUNION ALL " + closed + "\n\tUNION ALL " + supported
}()

// ListMirrorActsDue lists up to mirrorActsDuePage issues of rootID whose
// acts credit counts and are due a read (mirrorActsDueSQL), on boards, each
// listed by the regrouping its project counts it by; from is the start of
// the window (Unix ms) and worklogs reports that worklogs are read. They
// are listed by arm, the active sprints' first, then the closed sprints'
// by newest completion, then support tickets by newest update, each issue
// once at its first place, and Support set when any board names it a
// support ticket. A board the mirror does not show lists nothing. more
// reports that more are due. A read of an issue writes its receipt, which
// takes it off the list, so the next listing reaches the issues behind
// it.
func (s *Store) ListMirrorActsDue(rootID int64, boards []MirrorActsBoard, worklogs bool, from int64) (due []MirrorActsDue, more bool, err error) {
	type listed struct {
		MirrorActsDue
		arm   int
		order int64
	}
	label := fmt.Sprintf("list tracker mirror %d acts due", rootID)
	all, err := mirrorReadSnapshot(s, label, func(tx sqlQueryer) ([]listed, error) {
		var all []listed
		for _, b := range boards {
			board, shown, err := readMirrorShownBoardTx(tx, rootID, b.BoardID)
			if err != nil || !shown {
				return nil, err
			}
			groups := b.Regroup.statuses
			if groups == "" {
				groups = "{}"
			}
			arms := [3]int{}
			err = scanMirrorRows(tx, mirrorActsDueSQL, []any{rootID, b.BoardID, groups, b.Regroup.supportArg(), board.estimateResetAt, worklogs, from},
				func(r rowScanner) error {
					var l listed
					if err := r.Scan(&l.IssueID, &l.arm, &l.order, &l.Support); err != nil {
						return err
					}
					arms[l.arm]++
					more = more || arms[l.arm] > mirrorActsDuePage
					all = append(all, l)
					return nil
				})
			if err != nil {
				return nil, fmt.Errorf("board %s: %w", b.BoardID, err)
			}
		}
		return all, nil
	})
	if err != nil {
		return nil, false, err
	}
	slices.SortStableFunc(all, func(a, b listed) int {
		return cmp.Or(cmp.Compare(a.arm, b.arm), cmp.Compare(b.order, a.order), cmp.Compare(a.IssueID, b.IssueID))
	})
	at := make(map[string]int, len(all))
	for _, l := range all {
		if i, ok := at[l.IssueID]; ok {
			due[i].Support = due[i].Support || l.Support
			continue
		}
		at[l.IssueID] = len(due)
		due = append(due, l.MirrorActsDue)
	}
	if len(due) > mirrorActsDuePage {
		return due[:mirrorActsDuePage], true, nil
	}
	return due, more, nil
}
