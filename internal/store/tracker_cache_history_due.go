package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// mirrorHistoryDuePage bounds one ListMirrorHistoryDue read: what one
// history unit reads (trackerapp's historyBatches reads of
// mirrorHistoryBatch issues).
const mirrorHistoryDuePage = 4 * mirrorHistoryBatch

// MirrorHistoryDue is an issue due a history read, with the boards the
// mirror shows it on, sorted (none for an issue only a link keeps), and the
// revision of that set the read names (MirrorHistory.BoardsRev). UpdatedAt
// is the issue's update time, the version the read is for
// (MirrorHistory.For). For, AnsweredAt and Wait are the stored history's
// (zero: none), which tell the caller whether its read can be final.
// Receipt is what the write of the read names (MirrorHistory.Receipt).
type MirrorHistoryDue struct {
	IssueID          string
	Boards           []string
	BoardsRev        int64
	UpdatedAt        int64
	For              int64
	AnsweredAt, Wait int64
	Receipt          int64
}

// mirrorHistoryDueArm is one arm of the history due rule: its terms on
// an issue of root ?1, and the order its index walks them in.
type mirrorHistoryDueArm struct{ where, order string }

// mirrorHistoryDue is the one rule for an issue i due a history read at
// now, the caller's steady clock in Unix ms (an SQL expression), as its
// arms in the order ListMirrorHistoryDue lists them. Each comparison stays
// in one domain. Each arm's terms are the predicate and key range of one
// partial index, and every entry in that range is due, so a read seeks
// the due issues and never visits one waiting out its wait:
//   - waited: a read that was not final (history_wait over zero) whose
//     wait has elapsed since its answer arrived, both times the caller's;
//     idx_tracker_cache_issues_history_waiting, keyed by that deadline;
//   - stepped back: a read that was not final whose answer arrived after
//     now, which only a later process whose clock stepped back sees: due
//     at once rather than wait out the step;
//     idx_tracker_cache_issues_history_answered, keyed by the answer;
//   - version: updated later than the update time its stored history was
//     read for (history_for, zero for none), or read for fewer of its
//     boards: versions and revisions the mirror wrote, due at once;
//     idx_tracker_cache_issues_history_due, by issue ID.
//
// No issue is in both of the first two (its deadline is after its answer);
// one in either can be in the third.
func mirrorHistoryDue(i, now string) []mirrorHistoryDueArm {
	deadline := i + `.history_answered_at + ` + i + `.history_wait`
	return []mirrorHistoryDueArm{
		{i + `.history_wait > 0 AND ` + deadline + ` <= ` + now, deadline},
		{i + `.history_wait > 0 AND ` + i + `.history_answered_at > ` + now, i + `.history_answered_at`},
		{`(` + i + `.history_for < ` + i + `.updated_at OR ` + i + `.history_boards_rev < ` + i + `.boards_rev)`, i + `.issue_id`},
	}
}

// mirrorHistoryCovers holds for an issue i whose stored history covers its
// card: read for its update time and its boards, and final for them. It is
// due by none of the arms of mirrorHistoryDue: the version arm's terms
// fail and it waits for nothing. A history that does not cover the card
// says nothing of the card's values over the stretch since its latest
// change, which may be newer than the history.
func mirrorHistoryCovers(i string) string {
	return i + `.history_for > 0 AND NOT ` + mirrorHistoryDue(i, "0")[2].where + ` AND ` + i + `.history_wait = 0`
}

// mirrorHistoryDueSQL lists the kept issues of root ?1 due at ?2: first
// those of the JSON list ?3 due by any arm, each found by key; then each arm of the rule a walk of its index of at most a
// page and one, passing the listed issues, the version arm passing too
// the issues the waiting arms listed. Each arm is read only while the page
// is short, so the walk is bounded by the page and the list (and the
// unkept issues the cleanup has yet to delete), not by how many issues
// are due or waiting.
var mirrorHistoryDueSQL = func() string {
	arms := mirrorHistoryDue("i", "?2")
	limit := strconv.Itoa(mirrorHistoryDuePage + 1)
	const columns = `i.issue_id, i.boards_rev, i.updated_at, i.history_for, i.history_answered_at, i.history_wait, i.history_receipt`
	kept := mirrorIssueKept("i.root_id", "i.issue_id")
	byAny := make([]string, len(arms))
	for k, arm := range arms {
		byAny[k] = `(` + arm.where + `)`
	}
	selects := make([]string, len(arms)+1)
	selects[0] = `SELECT * FROM (SELECT ` + columns + ` FROM json_each(?3) f CROSS JOIN tracker_cache_issues i
	  WHERE i.root_id = CAST(?1 AS INTEGER) AND i.issue_id = f.value AND (` + strings.Join(byAny, " OR ") + `) AND ` + kept + ` LIMIT ` + limit + `)`
	for k, arm := range arms {
		where := arm.where
		if k == len(arms)-1 {
			where += ` AND NOT (` + arms[0].where + `) AND NOT (` + arms[1].where + `)`
		}
		selects[k+1] = `SELECT * FROM (SELECT ` + columns + ` FROM tracker_cache_issues i
		  WHERE i.root_id = CAST(?1 AS INTEGER) AND ` + where + ` AND i.issue_id NOT IN (SELECT value FROM json_each(?3)) AND ` + kept + `
		  ORDER BY ` + arm.order + ` LIMIT ` + limit + `)`
	}
	return `SELECT d.issue_id,
	  (SELECT json_group_array(b.board_id) FROM tracker_cache_board_issues b
	    WHERE b.root_id = CAST(?1 AS INTEGER) AND b.issue_id = d.issue_id AND ` + mirrorBoardShown("b.root_id", "b.board_id") + `), d.boards_rev,
	  d.updated_at, d.history_for, d.history_answered_at, d.history_wait, d.history_receipt
	FROM (` + strings.Join(selects, "\n\tUNION ALL ") + `) d LIMIT ` + limit
}()

// mirrorHistoryDueExistsSQL holds when root ?1 keeps an issue due at now:
// a probe of each arm's index that stops at the first issue kept.
func mirrorHistoryDueExistsSQL(now string) string {
	arms := mirrorHistoryDue("hd", now)
	probes := make([]string, len(arms))
	for k, arm := range arms {
		probes[k] = `EXISTS (SELECT 1 FROM tracker_cache_issues hd WHERE hd.root_id = CAST(?1 AS INTEGER) AND ` + arm.where + `
		    AND ` + mirrorIssueKept("hd.root_id", "hd.issue_id") + `)`
	}
	return `(` + strings.Join(probes, "\n\t  OR ") + `)`
}

// ListMirrorHistoryDue lists up to mirrorHistoryDuePage kept issues of
// rootID due a history read at now, the caller's steady clock in Unix
// milliseconds (mirrorHistoryDue): those of first that are due (issues a
// person is shown, whose status time a read is to decide first), then those whose wait is over, longest over first, then those
// whose answer is after now, then those of a new version or boards by
// issue ID, each once, with its boards. more reports that more are due. A
// read that clears an issue's due state takes it off the list, and one
// superseded leaves the state the write that superseded it stored, so the
// next listing reaches the issues behind them.
func (s *Store) ListMirrorHistoryDue(rootID int64, now int64, first ...string) (due []MirrorHistoryDue, more bool, err error) {
	if len(first) > mirrorHistoryDuePage {
		return nil, false, fmt.Errorf("%w: %d issues to list first, over %d", ErrInvalidTrackerCache, len(first), mirrorHistoryDuePage)
	}
	list, err := encodeMirrorIDs(first)
	if err != nil {
		return nil, false, err
	}
	err = scanMirrorRows(s.reader(), mirrorHistoryDueSQL, []any{rootID, now, list}, func(r rowScanner) error {
		var d MirrorHistoryDue
		var boards []byte
		if err := r.Scan(&d.IssueID, &boards, &d.BoardsRev, &d.UpdatedAt, &d.For, &d.AnsweredAt, &d.Wait, &d.Receipt); err != nil {
			return err
		}
		if len(boards) > 2 {
			if err := json.Unmarshal(boards, &d.Boards); err != nil {
				return fmt.Errorf("decode issue %s boards: %w", d.IssueID, err)
			}
		}
		due = append(due, d)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %d history due: %w", rootID, err)
	}
	if len(due) > mirrorHistoryDuePage {
		return due[:mirrorHistoryDuePage], true, nil
	}
	return due, false, nil
}
