package store

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
)

// mirrorActsDuePage bounds one ListMirrorActsDue read, and so what one
// evidence unit reads: 4 reads of trackerapp's evidenceBatch issues.
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

// The acts due statements of board ?2 of root ?1, under the regrouping ?3
// (a JSON object), the support rule ?4 (mirrorSupportSQL, NULL for none),
// the board's estimate reset ?5 and ?6 (worklogs are read), each at most a
// page and one of issue IDs with whether the support rule names the row.
// None sorts: a listing's order is the caller's walk of the arms and of
// the closed sprints by completion, and the due rule, a probe of a
// receipt by key, is tested before the rest, so a listing in the steady
// state, where almost nothing is due, reads each candidate's receipts and
// no history.
//   - active: members of the board's active sprints whose status counts
//     as done now;
//   - sprints: the board's closed sprints completed at or after ?7, with
//     their completion;
//   - closed: members of closed sprint ?7 whose status at the completion
//     counted as done (mirrorStatusAtCompletionSQL: one whose history is
//     unread and was carried on is not done yet);
//   - support: the board's rows the support rule names, the rule tested
//     first, since a row with an estimate fails it on the row alone.
var mirrorActsDueSQL = func() (out struct{ active, sprints, closed, support string }) {
	limit := ` LIMIT ` + strconv.Itoa(mirrorActsDuePage+1)
	due := mirrorActsDueWhere("i")
	support := mirrorSupportSQL("?4", "?2", mirrorRowEstimate("?5"))
	members := func(sprint string) string {
		return ` CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = ` + sprint + `.root_id AND m.sprint_id = ` + sprint + `.sprint_id
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ` + sprint + `.board_id AND b.issue_id = m.issue_id
		  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id`
	}
	out.active = `SELECT i.issue_id, ` + support + ` FROM tracker_cache_sprints sp` + members("sp") + `
		 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND sp.state = ` + strconv.Itoa(MirrorSprintActive) + `
		   AND ` + due + ` AND i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `)` + limit
	out.sprints = `SELECT sprint_id, completed_at FROM tracker_cache_sprints
		 WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = ` + strconv.Itoa(MirrorSprintClosed) + ` AND completed_at >= ?7`
	out.closed = `SELECT i.issue_id, ` + support + ` FROM tracker_cache_sprints l` + members("l") + `
		 WHERE l.root_id = CAST(?1 AS INTEGER) AND l.board_id = ?2 AND l.sprint_id = ?7
		   AND ` + due + ` AND coalesce(` + mirrorStatusAtCompletionSQL + ` IN (` + mirrorDoneStatusesSQL("?3") + `), 0)` + limit
	out.support = `SELECT i.issue_id, 1 FROM tracker_cache_board_issues b
		  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		 WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND ` + support + ` AND ` + due + limit
	return out
}()

// ListMirrorActsDue lists up to mirrorActsDuePage issues of rootID whose
// acts credit counts and are due a read (mirrorActsDueSQL), on boards, each
// listed by the regrouping its project counts it by; from is the start of
// the window (Unix ms) and worklogs reports that worklogs are read. They
// are listed by arm, the active sprints' members first, then the closed
// sprints' by newest completion, then support tickets, each issue once at
// its first place, Support set when any board names it a support ticket;
// within an arm (a sprint, for the closed one) the order is the index's.
// A board the mirror does not show lists nothing. more reports that more
// are due. A read of an issue writes its receipt, which takes it off the
// list, so the next listing reaches the issues behind it.
func (s *Store) ListMirrorActsDue(rootID int64, boards []MirrorActsBoard, worklogs bool, from int64) (due []MirrorActsDue, more bool, err error) {
	label := fmt.Sprintf("list tracker mirror %d acts due", rootID)
	type shown struct {
		MirrorActsBoard
		groups string
		reset  int64
	}
	at := map[string]int{}
	_, err = mirrorReadSnapshot(s, label, func(tx sqlQueryer) (struct{}, error) {
		var listed []shown
		for _, b := range boards {
			board, ok, err := readMirrorShownBoardTx(tx, rootID, b.BoardID)
			switch {
			case err != nil:
				return struct{}{}, fmt.Errorf("board %s: %w", b.BoardID, err)
			case ok:
				groups := b.Regroup.statuses
				if groups == "" {
					groups = "{}"
				}
				listed = append(listed, shown{b, groups, board.estimateResetAt})
			}
		}
		// add lists the rows of one arm's statement; full reports that
		// the listing holds more than a page.
		add := func(b shown, query string, args ...any) (full bool, err error) {
			args = append([]any{rootID, b.BoardID, b.groups, b.Regroup.supportArg(), b.reset, worklogs}, args...)
			n := 0
			err = scanMirrorRows(tx, query, args, func(r rowScanner) error {
				var d MirrorActsDue
				if err := r.Scan(&d.IssueID, &d.Support); err != nil {
					return err
				}
				n++
				if i, ok := at[d.IssueID]; ok {
					due[i].Support = due[i].Support || d.Support
					return nil
				}
				at[d.IssueID] = len(due)
				due = append(due, d)
				return nil
			})
			if err != nil {
				return false, fmt.Errorf("board %s: %w", b.BoardID, err)
			}
			more = more || n > mirrorActsDuePage || len(due) > mirrorActsDuePage
			return more, nil
		}
		for _, b := range listed {
			if full, err := add(b, mirrorActsDueSQL.active); full || err != nil {
				return struct{}{}, err
			}
		}
		type closed struct {
			board       shown
			sprint      string
			completedAt int64
		}
		var sprints []closed
		for _, b := range listed {
			err := scanMirrorRows(tx, mirrorActsDueSQL.sprints, []any{rootID, b.BoardID, nil, nil, nil, nil, from}, func(r rowScanner) error {
				c := closed{board: b}
				err := r.Scan(&c.sprint, &c.completedAt)
				sprints = append(sprints, c)
				return err
			})
			if err != nil {
				return struct{}{}, fmt.Errorf("board %s sprints: %w", b.BoardID, err)
			}
		}
		slices.SortStableFunc(sprints, func(a, b closed) int { return cmp.Compare(b.completedAt, a.completedAt) })
		for _, c := range sprints {
			if full, err := add(c.board, mirrorActsDueSQL.closed, c.sprint); full || err != nil {
				return struct{}{}, err
			}
		}
		for _, b := range listed {
			if b.Regroup.supportArg() == nil {
				continue
			}
			if full, err := add(b, mirrorActsDueSQL.support); full || err != nil {
				return struct{}{}, err
			}
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, false, err
	}
	if len(due) > mirrorActsDuePage {
		return due[:mirrorActsDuePage], true, nil
	}
	return due, more, nil
}
