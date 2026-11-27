package store

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// mirrorActsDuePage bounds one ListMirrorActsDue read, and so what one
// evidence unit reads: 4 reads of trackerapp's evidenceBatch issues.
const mirrorActsDuePage = 4 * mirrorHistoryBatch

// mirrorActsRevalidate is how long a worklog receipt of a ticket the
// active-sprint arm or the support arm lists stays current: a worklog
// deleted, summarized by the time source once a day, or processed late
// moves no card, so a live ticket's worklogs are read again this long
// after their read. A closed sprint's are not: a later add or edit moves
// the card. mirrorActsRetry is how long a source whose read failed for its
// issue alone (MirrorActsAttempt) is not due.
const (
	mirrorActsRevalidate = 24 * time.Hour
	mirrorActsRetry      = 24 * time.Hour
)

// MirrorActsBoard is a board whose tickets credit counts, with the
// regrouping its project counts the board's site by, the support rule
// included (MirrorRegroup).
type MirrorActsBoard struct {
	BoardID string
	Regroup MirrorRegroup
}

// MirrorActsDue is an issue whose acts are due a read: its comments, its
// worklogs, or both, and the start of the worklogs the read reads
// (WorklogsFrom, Unix ms, 0 for every one), the earliest that any board,
// arm or receipt of the issue needs.
type MirrorActsDue struct {
	IssueID            string
	Comments, Worklogs bool
	WorklogsFrom       int64
}

// The due rule of an issue i's sources, its receipts r1 (comments) and
// r2 (worklogs) joined by key. A source is due when it has no receipt, a
// receipt for an earlier card update (or for none, 0, since a changed
// worklog made it due), or, for worklogs, a receipt that starts later
// than the need or, on an arm that revalidates, one read before ?7; and
// no read of it failed since ?8. ?7 and ?8 are wall times.
var (
	mirrorActsReceiptsSQL = `
	  LEFT JOIN tracker_cache_issue_act_reads r1 ON r1.root_id = i.root_id AND r1.issue_id = i.issue_id AND r1.source = ` + strconv.Itoa(MirrorActComment) + `
	  LEFT JOIN tracker_cache_issue_act_reads r2 ON r2.root_id = i.root_id AND r2.issue_id = i.issue_id AND r2.source = ` + strconv.Itoa(MirrorActWorklog)
	mirrorCommentsDueSQL = `((coalesce(r1.read_for, 0) = 0 OR r1.read_for < i.updated_at) AND (coalesce(r1.tried_at, 0) = 0 OR r1.tried_at < ` + intParam("?8") + `))`
)

// mirrorWorklogsDueSQL is the worklogs' due rule for the need from need
// (an SQL expression), revalidated when revalidate.
func mirrorWorklogsDueSQL(need string, revalidate bool) string {
	stale := `coalesce(r2.read_for, 0) = 0 OR r2.read_for < i.updated_at OR r2.read_at > 0 AND r2.read_from > ` + need
	if revalidate {
		stale += ` OR r2.read_at > 0 AND r2.read_wall < ` + intParam("?7")
	}
	return `((` + stale + `) AND (coalesce(r2.tried_at, 0) = 0 OR r2.tried_at < ` + intParam("?8") + `))`
}

// mirrorWorklogsFromSQL is where a read of the worklogs of an issue
// needed from need starts: need, or earlier when its receipt holds them
// from earlier, so a read never narrows what the mirror holds.
func mirrorWorklogsFromSQL(need string) string {
	return `iif(r2.read_at > 0, min(` + need + `, r2.read_from), ` + need + `)`
}

// The acts due statements of board ?2 of root ?1, under the regrouping ?3
// (a JSON object), the support rule ?4 (mirrorSupportSQL, NULL for none)
// and the board's estimate reset ?5, the window from ?6 and the due
// rule's cutoffs ?7 and ?8, each at most a page and one of issue IDs with
// their sources due and the start of their worklogs. None sorts: a
// listing's order is the caller's walk of the arms and of the closed
// sprints by completion, and the due rule, a probe of each receipt by
// key, is tested before the rest, so a listing in the steady state, where
// almost nothing is due, reads each candidate's receipts and no history.
//   - active: members of the board's active sprints whose status counts
//     as done now, their comments and every worklog, revalidated;
//   - sprints: the board's closed sprints completed at or after ?6, with
//     their completion;
//   - closed: members of closed sprint ?9 whose status at the completion
//     counted as done (mirrorStatusAtCompletionSQL: one whose history is
//     unread and was carried on is not done yet), their comments and
//     every worklog;
//   - weeks: of a board that plans no sprints, its rows done now whose
//     card moved at or after ?6 (a ticket's finish is one of its
//     updates), walked by the update index, their comments and every
//     worklog;
//   - support: the board's rows the support rule names whose card moved
//     at or after ?6, walked by the update index (a worklog written moves
//     its card), their worklogs from ?6, revalidated.
var mirrorActsDueSQL = func() (out struct{ active, sprints, closed, weeks, support string }) {
	limit := ` LIMIT ` + strconv.Itoa(mirrorActsDuePage+1)
	support := mirrorSupportSQL("?4", "?2", mirrorRowEstimate("?5"))
	window := intParam("?6")
	members := func(sprint string) string {
		return ` CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = ` + sprint + `.root_id AND m.sprint_id = ` + sprint + `.sprint_id
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ` + sprint + `.board_id AND b.issue_id = m.issue_id
		  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id` + mirrorActsReceiptsSQL
	}
	member := func(revalidate bool) (columns, due string) {
		worklogs := mirrorWorklogsDueSQL("0", revalidate)
		return `SELECT i.issue_id, ` + mirrorCommentsDueSQL + `, ` + worklogs + `, 0`, `(` + mirrorCommentsDueSQL + ` OR ` + worklogs + `)`
	}
	columns, due := member(true)
	out.active = columns + ` FROM tracker_cache_sprints sp` + members("sp") + `
		 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND sp.state = ` + strconv.Itoa(MirrorSprintActive) + `
		   AND ` + due + ` AND i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `)` + limit
	out.sprints = `SELECT sprint_id, completed_at FROM tracker_cache_sprints
		 WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = ` + strconv.Itoa(MirrorSprintClosed) + ` AND completed_at >= ` + window
	columns, due = member(false)
	out.closed = columns + ` FROM tracker_cache_sprints l` + members("l") + `
		 WHERE l.root_id = CAST(?1 AS INTEGER) AND l.board_id = ?2 AND l.sprint_id = ?9
		   AND ` + due + ` AND coalesce(` + mirrorStatusAtCompletionSQL + ` IN (` + mirrorDoneStatusesSQL("?3") + `), 0)` + limit
	out.weeks = columns + ` FROM tracker_cache_issues i
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id` + mirrorActsReceiptsSQL + `
		 WHERE i.root_id = CAST(?1 AS INTEGER) AND i.updated_at > 0 AND i.updated_at >= ` + window + `
		   AND ` + due + ` AND i.status_id IN (` + mirrorDoneStatusesSQL("?3") + `)` + limit
	out.support = `SELECT i.issue_id, 0, 1, ` + mirrorWorklogsFromSQL(window) + ` FROM tracker_cache_issues i
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.board_id = ?2 AND b.issue_id = i.issue_id` + mirrorActsReceiptsSQL + `
		 WHERE i.root_id = CAST(?1 AS INTEGER) AND i.updated_at > 0 AND i.updated_at >= ` + window + `
		   AND ` + mirrorWorklogsDueSQL(window, true) + ` AND ` + support + limit
	return out
}()

// ListMirrorActsDue lists up to mirrorActsDuePage issues of rootID whose
// acts credit counts and are due a read (mirrorActsDueSQL), on boards, each
// listed by the regrouping its project counts it by; from is the start of
// the window and now the wall clock (Unix ms). They are listed by arm,
// the active sprints' members first, then the closed sprints' by newest
// completion, then the done tickets of each board that plans no sprints,
// then support tickets, each issue once at its first place
// with every arm's need merged: a source due on any, the earliest start
// of worklogs any needs. The arms that need comments and every worklog
// come before the support arm, so an issue the support arm lists was
// weighed by every other first. Within an arm (a sprint, for the closed
// one) the order is the index's. A board the mirror does not show lists
// nothing. more reports that more are due. A read of an issue writes its
// receipt, or its attempt when the read failed for it alone, which takes
// it off the list either way, so the next listing reaches the issues
// behind it.
func (s *Store) ListMirrorActsDue(rootID int64, boards []MirrorActsBoard, from, now int64) (due []MirrorActsDue, more bool, err error) {
	label := fmt.Sprintf("list tracker mirror %d acts due", rootID)
	type shown struct {
		MirrorActsBoard
		groups       string
		reset        int64
		plansSprints bool
	}
	revalidate, retry := now-mirrorActsRevalidate.Milliseconds(), now-mirrorActsRetry.Milliseconds()
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
				listed = append(listed, shown{b, groups, board.estimateResetAt, board.plansSprints})
			}
		}
		// add lists the rows of one arm's statement; full reports that
		// the listing holds more than a page.
		add := func(b shown, query string, args ...any) (full bool, err error) {
			args = append([]any{rootID, b.BoardID, b.groups, b.Regroup.supportArg(), b.reset, from, revalidate, retry}, args...)
			n := 0
			err = scanMirrorRows(tx, query, args, func(r rowScanner) error {
				var d MirrorActsDue
				if err := r.Scan(&d.IssueID, &d.Comments, &d.Worklogs, &d.WorklogsFrom); err != nil {
					return err
				}
				n++
				if i, ok := at[d.IssueID]; ok {
					due[i].Comments = due[i].Comments || d.Comments
					due[i].Worklogs = due[i].Worklogs || d.Worklogs
					due[i].WorklogsFrom = min(due[i].WorklogsFrom, d.WorklogsFrom)
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
			err := scanMirrorRows(tx, mirrorActsDueSQL.sprints, []any{rootID, b.BoardID, nil, nil, nil, from}, func(r rowScanner) error {
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
			if b.plansSprints {
				continue
			}
			if full, err := add(b, mirrorActsDueSQL.weeks); full || err != nil {
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
