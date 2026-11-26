package store

import "strconv"

// MirrorPaceSprints is how many finished sprints a board's pace averages:
// its last ones by completion.
const MirrorPaceSprints = 5

// MirrorSprintFinished is what one closed sprint of a board finished: its
// members on the board, those whose status when the sprint was completed
// counted as done, and the sum of the board's estimates of those, as the
// board holds them now (a row with no estimate, or one read no later than
// the estimate definition last changed, adds nothing).
type MirrorSprintFinished struct {
	SprintID string
	Members  int
	Done     int
	Estimate float64
}

// mirrorPaceSQL reads, for the last MirrorPaceSprints closed sprints of
// board ?2 of root ?1 by completion, each member's status when its sprint
// was completed and its estimate, and counts and sums those whose status
// then counted as done by the regrouping ?4 (a JSON object, '{}' for none)
// over the catalog's groups: the list of done statuses is one uncorrelated
// subquery, built once a statement, as mirrorRegroupedNotDone's is. The
// status then is the last status change at or before the completion,
// else the first change after it is from, else the issue's status now (an
// issue whose status never changed); each is read in the changes' key
// order, which is each kind's oldest first (MirrorHistory), at the issue's
// current history generation. A sprint none of whose members the board
// holds has no row. ?3 is the board's estimate reset.
var mirrorPaceSQL = `WITH last AS (
	  SELECT sprint_id, completed_at FROM tracker_cache_sprints
	   WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 AND state = 3 AND completed_at > 0
	   ORDER BY completed_at DESC, position DESC LIMIT ` + strconv.Itoa(MirrorPaceSprints) + `),
	members AS (
	  SELECT l.sprint_id, ` + mirrorRowEstimate("?3") + ` AS estimate, coalesce(
	    (SELECT c.to_value FROM tracker_cache_changes c
	      WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = 1 AND c.at <= l.completed_at
	      ORDER BY c.seq DESC LIMIT 1),
	    (SELECT c.from_value FROM tracker_cache_changes c
	      WHERE c.root_id = i.root_id AND c.issue_id = i.issue_id AND c.gen = i.history_gen AND c.kind = 1 AND c.at > l.completed_at
	      ORDER BY c.seq LIMIT 1),
	    i.status_id) AS status
	  FROM last l
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = l.sprint_id
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = m.root_id AND b.board_id = ?2 AND b.issue_id = m.issue_id
	  CROSS JOIN tracker_cache_issues i ON i.root_id = m.root_id AND i.issue_id = m.issue_id)
	SELECT sprint_id, count(*), total(done), total(iif(done, estimate, 0)) FROM (
	  SELECT sprint_id, estimate, status IN (SELECT st.status_id FROM tracker_cache_statuses st
	     WHERE st.root_id = CAST(?1 AS INTEGER) AND st.status_group = ` + strconv.Itoa(MirrorGroupDone) + ` AND st.status_id NOT IN (SELECT o.key FROM json_each(?4) o)
	     UNION ALL SELECT o.key FROM json_each(?4) o WHERE o.value = ` + strconv.Itoa(MirrorGroupDone) + `) AS done
	  FROM members)
	GROUP BY sprint_id`

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
