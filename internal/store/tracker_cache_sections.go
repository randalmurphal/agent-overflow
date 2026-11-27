package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"agent-overflow/internal/slicesx"
)

// MirrorSectionKind is a section of a board.
type MirrorSectionKind int

const (
	// MirrorSectionSprint is the rows in one sprint.
	MirrorSectionSprint MirrorSectionKind = iota + 1
	// MirrorSectionBacklog is the rows in no future or active sprint of
	// the board whose status is not in the done or canceled group, by the
	// group tracker_cache_statuses holds; a status the mirror does not
	// know counts as not done. On a board with no sprints, every row not
	// done.
	MirrorSectionBacklog
	// MirrorSectionColumn is the rows whose status is in one column.
	MirrorSectionColumn
)

// MirrorSection names a section: a sprint by ID, the backlog, or a column
// by position.
type MirrorSection struct {
	Kind     MirrorSectionKind
	SprintID string
	Column   int
}

// MirrorRowKey is a row's place in a board's order at a revision of one
// lifetime of the board: a page continues after it. A key with no issue
// starts at the first row, at the board's current lifetime and revision.
type MirrorRowKey struct {
	OrderKey, IssueID  string
	Lifetime, Revision int64
}

// MirrorSectionPage is a page of a section's rows in order-key order, read
// at the board's Revision. More reports that rows follow Next. Names are
// what the rows' cards name, read in the same snapshot (ListMirrorSection;
// a board view names its pages together, in MirrorBoardView.Names).
type MirrorSectionPage struct {
	Rows     []MirrorRow
	Next     MirrorRowKey
	More     bool
	Revision int64
	Names    MirrorNames
}

// mirrorSectionRowsSQL reads ?1 root, ?2 board, after (?3, ?4), a
// section's ?5, ?6 the board's estimate reset, ?7 the regrouping and ?8
// the support rule (MirrorSection.statement).
var mirrorSectionRowsSQL = `SELECT ` + mirrorRowColumnsSQL + `
	  FROM tracker_cache_board_issues b
	  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND (b.order_key, b.issue_id) > (?3, ?4) AND `

const mirrorSectionOrderSQL = ` ORDER BY b.order_key, b.issue_id LIMIT `

// mirrorRowEstimate is the estimate a board row b shows: none when the
// row was read no later than reset, the SQL expression of its board's
// estimate reset, since its number is in the board's earlier estimate
// definition. Every read of a row's estimate states it with this.
func mirrorRowEstimate(reset string) string {
	return `iif(b.read_at <= ` + reset + `, NULL, b.estimate)`
}

// mirrorRowColumnsSQL are a section row's columns, of a board row b of
// board ?2 and its issue i, as scanMirrorRow reads them, ?6 being the
// board's estimate reset and ?8 the support rule. Every read of a
// section's rows, by page or by ID, projects them.
var mirrorRowColumnsSQL = mirrorCardColumns + `, b.order_key, ` + mirrorRowEstimate("?6") + `, ` +
	mirrorSupportSQL("?8", "?2", mirrorRowEstimate("?6"))

// scanMirrorRow reads a row of mirrorRowColumnsSQL.
func scanMirrorRow(r rowScanner) (MirrorRow, error) {
	var row MirrorRow
	var err error
	row.Card, err = scanMirrorCard(r, &row.OrderKey, &row.Estimate, &row.Support)
	return row, err
}

// ruleArg is the value a section's rule names as ?5: its sprint or its
// column's position.
func (section MirrorSection) ruleArg() any {
	switch section.Kind {
	case MirrorSectionSprint:
		return section.SprintID
	case MirrorSectionColumn:
		return section.Column
	case MirrorSectionBacklog:
		// The backlog's rule names no ?5.
	}
	return nil
}

// statement is a section statement's text and arguments: the six every
// one binds, then the regrouping as ?7 and the support rule as ?8. The
// text is the plain text of the section's kind, or, for the backlog (the
// one section whose rule reads a status's group) under a regrouping that
// regroups something, regroupedBacklog, the one that reads ?7.
func (section MirrorSection) statement(regroup MirrorRegroup, plain map[MirrorSectionKind]string, regroupedBacklog string,
	six ...any) (string, []any) {
	if section.Kind == MirrorSectionBacklog && regroup.statuses != "" {
		return regroupedBacklog, append(six, regroup.statuses, regroup.supportArg())
	}
	return plain[section.Kind], append(six, nil, regroup.supportArg())
}

// The sprint rules hold for a row b of board ?2 of root ?1 in a future or
// active sprint of the board (InOpenSprint) or in sprint ?5 (InSprint).
// Each is stated in two forms, by how many rows a statement tests: a Probe
// reads the row's own sprints, which costs a few rows a test, for a
// statement that tests few rows (a read by ID, the backlog's page); Members
// reads the sprints' members once a statement
// (idx_tracker_cache_issue_sprints_sprint), which costs every member
// once, for a statement that walks the board for few of them (the counts,
// a sprint's page). The pages, the patch read and the counts are checked
// against one another (TestMirrorCountsMatchThePages, requirePatchParity).
const (
	mirrorInOpenSprintProbeSQL = `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  CROSS JOIN tracker_cache_sprints sp ON sp.root_id = m.root_id AND sp.board_id = b.board_id AND sp.sprint_id = m.sprint_id
	 WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND sp.state IN (1, 2))`
	mirrorInOpenSprintMembersSQL = `b.issue_id IN (SELECT m.issue_id FROM tracker_cache_sprints sp
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = sp.root_id AND m.sprint_id = sp.sprint_id
	 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2 AND sp.state IN (1, 2))`
	mirrorInSprintProbeSQL = `EXISTS (SELECT 1 FROM tracker_cache_issue_sprints m
	  WHERE m.root_id = b.root_id AND m.issue_id = b.issue_id AND m.sprint_id = ?5)`
	mirrorInSprintMembersSQL = `b.issue_id IN (SELECT m.issue_id FROM tracker_cache_issue_sprints m
	  WHERE m.root_id = CAST(?1 AS INTEGER) AND m.sprint_id = ?5)`
)

// MirrorGroupLeftOut is the group a project puts a status in to leave it
// out of counts: its rows show in their columns and sprints, are not in
// the backlog, and add to no total. Only a MirrorRegroup names it; no
// tracker status is in it.
const MirrorGroupLeftOut = 6

// mirrorRegroupedGroupNameSQL names the group expr (an SQL expression of
// its number) as a regrouping can place a status: by MirrorGroupNames,
// and MirrorGroupLeftOut as "left_out", the one group no status view
// names; NULL for none.
func mirrorRegroupedGroupNameSQL(expr string) string {
	return mirrorGroupCaseSQL(expr, ` WHEN `+strconv.Itoa(MirrorGroupLeftOut)+` THEN 'left_out'`, "NULL")
}

// MirrorRegroup is how a project counts the rows of one tracker site's
// boards, which a board read applies. Its regrouping of the site's
// statuses: by status ID, the group the status counts in
// (MirrorGroupBacklog to MirrorGroupCanceled, or MirrorGroupLeftOut). A
// status it does not name keeps the group tracker_cache_statuses holds.
// The zero value regroups nothing, and a read with it runs the plain
// statements, which read a row's status by its key; a regrouping that
// regroups something runs the regrouped ones, which bind it. Its support
// rule (WithSupport): which rows are support tickets, which a total does
// not count as without an estimate (mirrorSupportSQL). The zero value
// names no row support.
type MirrorRegroup struct {
	// statuses is the JSON object the regrouped statements read with
	// json_each, empty for the zero value.
	statuses string
	// support is the JSON object mirrorSupportSQL reads, empty for none.
	support string
}

// MirrorSupportRule names a project's support tickets of one tracker
// site: those of an issue type (by ID), with a label (by name,
// MirrorCard.Labels) or on a board (by ID) it lists.
type MirrorSupportRule struct {
	Types, Labels, Boards []string
}

// mirrorSupport is the JSON object mirrorSupportSQL reads: the rule's
// lists.
type mirrorSupport struct {
	Boards []string `json:"boards"`
	Types  []string `json:"types"`
	Labels []string `json:"labels"`
}

// WithSupport is r with rule as its support rule; a rule that names
// nothing names no row.
func (r MirrorRegroup) WithSupport(rule MirrorSupportRule) (MirrorRegroup, error) {
	r.support = ""
	if len(rule.Types)+len(rule.Labels)+len(rule.Boards) == 0 {
		return r, nil
	}
	encoded, err := encodeMirrorJSON(mirrorSupport{Boards: slicesx.OrEmpty(rule.Boards), Types: slicesx.OrEmpty(rule.Types),
		Labels: slicesx.OrEmpty(rule.Labels)})
	if err != nil {
		return MirrorRegroup{}, err
	}
	r.support = encoded
	return r, nil
}

// supportArg is the support rule as a statement binds it: NULL for none.
func (r MirrorRegroup) supportArg() any {
	if r.support == "" {
		return nil
	}
	return r.support
}

// mirrorSupportSQL holds for a row b of board, with its issue i, that is a
// support ticket by the support rule the JSON object rule binds (NULL for
// none): its estimate (estimate, the SQL expression every read of a row's
// estimate states, mirrorRowEstimate) is none, and the rule lists its
// board, its type, or one of its labels (tracker_cache_issues.labels). A
// row that has an estimate is never support. The arms run cheapest first
// and SQLite stops at the first that decides: a row with an estimate
// reads nothing more, the board's arm is one test a statement, and the
// type's and the labels' read the issue's row, not its card's data. Each
// list of the rule is built once a statement. Every read that tells
// support (a row's flag, the totals) states it with this.
func mirrorSupportSQL(rule, board, estimate string) string {
	return `(` + estimate + ` IS NULL AND ` + rule + ` IS NOT NULL AND (` +
		board + ` IN (SELECT value FROM json_each(` + rule + `, '$.boards'))
		 OR i.type_id IN (SELECT value FROM json_each(` + rule + `, '$.types'))
		 OR i.labels <> '[]' AND EXISTS (SELECT 1 FROM json_each(i.labels) l
		     WHERE l.value IN (SELECT value FROM json_each(` + rule + `, '$.labels')))))`
}

// NewMirrorRegroup is the regrouping groups gives, by status ID. A
// status ID is non-empty and a group one a MirrorRegroup holds.
func NewMirrorRegroup(groups map[string]int) (MirrorRegroup, error) {
	if len(groups) == 0 {
		return MirrorRegroup{}, nil
	}
	for id, group := range groups {
		switch {
		case id == "":
			return MirrorRegroup{}, fmt.Errorf("%w: a regrouped status has no ID", ErrInvalidTrackerCache)
		case group < MirrorGroupBacklog || group > MirrorGroupLeftOut:
			return MirrorRegroup{}, fmt.Errorf("%w: status %s regrouped into group %d", ErrInvalidTrackerCache, id, group)
		}
	}
	encoded, err := encodeMirrorJSON(groups)
	if err != nil {
		return MirrorRegroup{}, err
	}
	return MirrorRegroup{statuses: encoded}, nil
}

// mirrorNotDoneSQL holds for a row of b whose issue i has a status the
// mirror does not place in the done or canceled group, probed by key.
const mirrorNotDoneSQL = `NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st
	 WHERE st.root_id = b.root_id AND st.status_id = i.status_id AND st.status_group IN (4, 5))`

// mirrorRegroupedNotDone holds for a row of b whose issue i has a status
// that does not count as done for the regrouping the JSON object g binds:
// a status the tracker places in the done or canceled group that g does
// not name, or one g puts in done, canceled or left out, does. A status
// the mirror does not know counts as not done unless g names it. The list
// is one uncorrelated subquery, which the statement builds once from the
// root's catalog and probes per row.
func mirrorRegroupedNotDone(g string) string {
	return `i.status_id NOT IN (SELECT st.status_id FROM tracker_cache_statuses st
	 WHERE st.root_id = CAST(?1 AS INTEGER) AND st.status_group IN (4, 5) AND st.status_id NOT IN (SELECT o.key FROM json_each(` + g + `) o)
	 UNION ALL SELECT o.key FROM json_each(` + g + `) o WHERE o.value IN (4, 5, 6))`
}

// mirrorLeftOut holds for a row whose issue i has a status the
// regrouping g leaves out of counts. Only the count of a board's rows
// states it (mirrorRegroupedCountRowsSQL); the sprints' totals leave out
// the issues that count found (mirrorCountSprintsLeftOutSQL).
func mirrorLeftOut(g string) string {
	return `i.status_id IN (SELECT o.key FROM json_each(` + g + `) o WHERE o.value = 6)`
}

// mirrorBacklog holds for a row of b in the board's backlog, with
// inOpenSprint one form of the open-sprint rule and notDone one of the
// not-done rule (mirrorNotDoneSQL, mirrorRegroupedNotDone).
func mirrorBacklog(inOpenSprint, notDone string) string {
	return `NOT ` + inOpenSprint + ` AND ` + notDone
}

// mirrorColumnRuleSQL holds for a row b (with its issue i) of board ?2 of
// root ?1 in the column at position ?5. Every read of a section's rows, by
// page or by ID, states its rule with it, mirrorBacklog or a sprint rule.
const mirrorColumnRuleSQL = `i.status_id IN (SELECT c.status_id FROM tracker_cache_board_column_statuses c
	  WHERE c.root_id = CAST(?1 AS INTEGER) AND c.board_id = ?2 AND c.position = ?5)`

// mirrorSectionSQL reads a page of a section of each kind, and
// mirrorRegroupedBacklogSectionSQL one of the backlog under a regrouping
// (MirrorSection.statement).
var (
	mirrorSectionSQL = map[MirrorSectionKind]string{
		MirrorSectionSprint:  mirrorSectionRowsSQL + mirrorInSprintMembersSQL + mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
		MirrorSectionBacklog: mirrorSectionRowsSQL + mirrorBacklog(mirrorInOpenSprintProbeSQL, mirrorNotDoneSQL) + mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
		MirrorSectionColumn:  mirrorSectionRowsSQL + mirrorColumnRuleSQL + mirrorSectionOrderSQL + strconv.Itoa(mirrorSectionPageSize+1),
	}
	mirrorRegroupedBacklogSectionSQL = mirrorSectionRowsSQL + mirrorBacklog(mirrorInOpenSprintProbeSQL, mirrorRegroupedNotDone("?7")) + mirrorSectionOrderSQL +
		strconv.Itoa(mirrorSectionPageSize+1)
)

// mirrorShownBoardSQL reads the lifetime, revisions, estimate unit and
// estimate reset of a board the mirror shows.
var mirrorShownBoardSQL = `SELECT b.lifetime, b.revision, b.totals_revision, b.estimate_unit, b.estimate_reset_at, b.plans_sprints FROM tracker_cache_boards b
	WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND ` + mirrorBoardShown(intParam("?1"), "?2")

// mirrorShownBoard is a shown board's state as a read of its rows needs it.
type mirrorShownBoard struct {
	lifetime, revision, totalsRevision, estimateResetAt int64
	unit                                                int
	plansSprints                                        bool
}

// readMirrorShownBoardTx reads a shown board's lifetime, revisions and
// estimate unit; ok is false for a board the mirror does not show.
func readMirrorShownBoardTx(tx sqlQueryer, rootID int64, boardID string) (board mirrorShownBoard, ok bool, err error) {
	err = tx.QueryRow(mirrorShownBoardSQL, rootID, boardID).Scan(&board.lifetime, &board.revision, &board.totalsRevision, &board.unit,
		&board.estimateResetAt, &board.plansSprints)
	if errors.Is(err, sql.ErrNoRows) {
		return mirrorShownBoard{}, false, nil
	}
	if err != nil {
		return mirrorShownBoard{}, false, fmt.Errorf("read board: %w", err)
	}
	return board, true, nil
}

// ListMirrorSection reads up to mirrorSectionPageSize rows of one section
// of a board, in order-key order, after after, in one snapshot with the
// board's revision and what the rows' cards name (MirrorNames). A
// continuation (after names an issue) read at another
// lifetime or revision fails with ErrTrackerCacheStale: a change between
// the pages could have moved a row across the cursor, and a board unlisted
// and listed again holds other rows. A board the mirror does not show has
// no rows.
func (s *Store) ListMirrorSection(rootID int64, boardID string, regroup MirrorRegroup, section MirrorSection, after MirrorRowKey) (page MirrorSectionPage, err error) {
	if _, ok := mirrorSectionSQL[section.Kind]; !ok {
		return MirrorSectionPage{}, fmt.Errorf("%w: section kind %d", ErrInvalidTrackerCache, section.Kind)
	}
	label := fmt.Sprintf("read tracker mirror %d board %s section", rootID, boardID)
	return mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorSectionPage, error) {
		board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
		switch {
		case err != nil:
			return MirrorSectionPage{}, err
		case after.IssueID != "" && (!shown || board.lifetime != after.Lifetime || board.revision != after.Revision):
			return MirrorSectionPage{}, ErrTrackerCacheStale
		case !shown:
			return MirrorSectionPage{}, nil
		}
		page, err := readMirrorSectionTx(tx, rootID, boardID, board, regroup, section, after)
		if err != nil {
			return MirrorSectionPage{}, err
		}
		page.Names, err = readMirrorNamesTx(tx, rootID, [][]MirrorRow{page.Rows}, nil)
		return page, err
	})
}

// readMirrorSectionTx reads a page of a valid section of a shown board,
// after after, in tx's snapshot.
func readMirrorSectionTx(tx sqlQueryer, rootID int64, boardID string, board mirrorShownBoard, regroup MirrorRegroup, section MirrorSection,
	after MirrorRowKey) (page MirrorSectionPage, err error) {
	query, args := section.statement(regroup, mirrorSectionSQL, mirrorRegroupedBacklogSectionSQL,
		rootID, boardID, after.OrderKey, after.IssueID, section.ruleArg(), board.estimateResetAt)
	page.Revision = board.revision
	err = scanMirrorRows(tx, query, args, func(r rowScanner) error {
		row, err := scanMirrorRow(r)
		page.Rows = append(page.Rows, row)
		return err
	})
	if err != nil {
		return MirrorSectionPage{}, err
	}
	if len(page.Rows) > mirrorSectionPageSize {
		page.Rows, page.More = page.Rows[:mirrorSectionPageSize], true
	}
	if n := len(page.Rows); n > 0 {
		page.Next = MirrorRowKey{OrderKey: page.Rows[n-1].OrderKey, IssueID: page.Rows[n-1].Card.IssueID,
			Lifetime: board.lifetime, Revision: board.revision}
	}
	return page, nil
}

// mirrorLastPlanRowSQL reads the last row, in the board's order, of
// board ?2 of root ?1 in sprint ?5 (mirrorLastSprintRowSQL) or in no open
// sprint of the board (mirrorLastBacklogRowSQL), a superset of the
// backlog under any regrouping. Each walks idx_tracker_cache_board_issues_order
// backwards from the board's end to the first row that holds the rule,
// with the rule in the form its section's page uses.
var (
	mirrorLastPlanRowSQL = `SELECT b.issue_id, b.order_key FROM tracker_cache_board_issues b
	 WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2 AND ` + mirrorBoardShown(intParam("?1"), "?2") + ` AND `
	mirrorLastSprintRowSQL  = mirrorLastPlanRowSQL + mirrorInSprintMembersSQL + ` ORDER BY b.order_key DESC, b.issue_id DESC LIMIT 1`
	mirrorLastBacklogRowSQL = mirrorLastPlanRowSQL + `NOT ` + mirrorInOpenSprintProbeSQL + ` ORDER BY b.order_key DESC, b.issue_id DESC LIMIT 1`
)

// LastMirrorPlanRow is the last row, in the board's order, of a board the
// mirror shows in sprint sprintID, or in no open sprint of the board when
// sprintID is empty: what a ticket made at that section's foot lands
// after. ok is false when there is none.
func (s *Store) LastMirrorPlanRow(rootID int64, boardID, sprintID string) (issueID, orderKey string, ok bool, err error) {
	query, args := mirrorLastBacklogRowSQL, []any{rootID, boardID}
	if sprintID != "" {
		query, args = mirrorLastSprintRowSQL, []any{rootID, boardID, nil, nil, sprintID}
	}
	label := fmt.Sprintf("read the last row of tracker mirror %d board %s section %q", rootID, boardID, sprintID)
	row, err := mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorRowKey, error) {
		var row MirrorRowKey
		err := tx.QueryRow(query, args...).Scan(&row.IssueID, &row.OrderKey)
		if errors.Is(err, sql.ErrNoRows) {
			return MirrorRowKey{}, nil
		}
		return row, err
	})
	return row.IssueID, row.OrderKey, row.IssueID != "", err
}

// mirrorSupportOfSQL reads the issues of the JSON list ?3 whose rows on
// board ?2 of root ?1 the support rule ?5 names, ?4 being the board's
// estimate reset, each row probed by key.
var mirrorSupportOfSQL = `SELECT b.issue_id FROM json_each(?3) l
	  CROSS JOIN tracker_cache_board_issues b ON b.root_id = CAST(?1 AS INTEGER) AND b.issue_id = l.value AND b.board_id = ?2
	  JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE ` + mirrorSupportSQL("?5", "?2", mirrorRowEstimate("?4"))

// MirrorSupportOf is which of some issues are support tickets on a board
// the mirror shows, and the board's estimate unit.
type MirrorSupportOf struct {
	Unit    int
	Support []string
}

// ListMirrorSupport reads which of issueIDs (at most mirrorMaxLookup) are
// support tickets on a board the mirror shows by regroup's support rule
// (mirrorSupportSQL), in no order, with the board's estimate unit, in one
// snapshot. An issue the board has no row of is not one. ok is false for a
// board the mirror does not show.
func (s *Store) ListMirrorSupport(rootID int64, boardID string, regroup MirrorRegroup, issueIDs []string) (MirrorSupportOf, bool, error) {
	if len(issueIDs) > mirrorMaxLookup {
		return MirrorSupportOf{}, false, fmt.Errorf("%w: %d issues, over %d", ErrInvalidTrackerCache, len(issueIDs), mirrorMaxLookup)
	}
	list, err := encodeMirrorIDs(issueIDs)
	if err != nil {
		return MirrorSupportOf{}, false, err
	}
	label := fmt.Sprintf("read the support tickets of tracker mirror %d board %s", rootID, boardID)
	var ok bool
	out, err := mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorSupportOf, error) {
		board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
		if err != nil || !shown {
			return MirrorSupportOf{}, err
		}
		ok = true
		out := MirrorSupportOf{Unit: board.unit}
		if regroup.support == "" {
			return out, nil
		}
		err = scanMirrorRows(tx, mirrorSupportOfSQL, []any{rootID, boardID, list, board.estimateResetAt, regroup.support}, func(r rowScanner) error {
			var id string
			err := r.Scan(&id)
			out.Support = append(out.Support, id)
			return err
		})
		return out, err
	})
	if err != nil {
		return MirrorSupportOf{}, false, err
	}
	return out, ok, nil
}

// MirrorSectionTotal is how many rows a section holds, the sum of the
// board's estimates of them and how many of them have none and are not
// support tickets (mirrorSupportSQL); a row with no estimate, or one read
// no later than the board's estimate definition last changed (unknown),
// adds nothing to the sum and counts as having none, and a board with no
// estimate unit sums to zero and counts none.
type MirrorSectionTotal struct {
	Rows        int
	Estimate    float64
	Unestimated int
}

// MirrorSectionCounts are the totals of each section of a board at its
// Revision and TotalsRevision. A sprint or column with no rows is absent.
// Pace is what the board's last finished sprints finished
// (MirrorSprintFinished, mirrorPaceSQL), in no order, read for a board that
// plans sprints.
type MirrorSectionCounts struct {
	Revision, TotalsRevision int64
	All, Backlog             MirrorSectionTotal
	Sprints                  map[string]MirrorSectionTotal
	Columns                  map[int]MirrorSectionTotal
	Pace                     []MirrorSprintFinished
}

// The count statements sort nothing, and read an issue's table row only
// for a row with no estimate under a support rule that lists types or
// labels (mirrorSupportSQL). mirrorCountRowsSQL walks the board's rows
// once, in idx_tracker_cache_board_issues_order, reads each row's status
// from idx_tracker_cache_issues_status, tests it against the open sprints'
// members (read once), and returns a row per board row, which
// countMirrorSectionsTx sums: a GROUP BY would sort every row, in memory
// that grows with the board. Its last column is the issue of a row the
// support rule ?5 names, NULL for every other. mirrorRegroupedCountRowsSQL
// does the same under the regrouping ?4 (its list built once), with one
// more column before it: the issue of a row left out of counts, NULL for
// every other. mirrorCountSprintsSQL totals each sprint of the board from
// its members, in the sprints' key order, a member in the JSON list ?5
// (the support rows the first statement found) not counting as without an
// estimate; mirrorCountSprintsLeftOutSQL does the same without the issues
// of the JSON list ?4, the rows the first statement found left out, so a
// sprint member's status and support are read once a board, not once a
// member.
var (
	mirrorCountRowsFromSQL = `
		FROM tracker_cache_board_issues b
		CROSS JOIN tracker_cache_issues i INDEXED BY idx_tracker_cache_issues_status ON i.root_id = b.root_id AND i.issue_id = b.issue_id
		WHERE b.root_id = CAST(?1 AS INTEGER) AND b.board_id = ?2`
	mirrorCountSupportSQL = `iif(` + mirrorSupportSQL("?5", "?2", mirrorRowEstimate("?3")) + `, b.issue_id, NULL)`
	mirrorCountRowsSQL    = `SELECT ` + mirrorBacklog(mirrorInOpenSprintMembersSQL, mirrorNotDoneSQL) + `, i.status_id, ` + mirrorRowEstimate("?3") + `, ` +
		mirrorCountSupportSQL + mirrorCountRowsFromSQL
	mirrorRegroupedCountRowsSQL = `SELECT ` + mirrorBacklog(mirrorInOpenSprintMembersSQL, mirrorRegroupedNotDone("?4")) + `, i.status_id, ` +
		mirrorRowEstimate("?3") + `, iif(` + mirrorLeftOut("?4") + `, b.issue_id, NULL), ` + mirrorCountSupportSQL + mirrorCountRowsFromSQL
	mirrorCountSprintsRowsSQL = `SELECT sp.sprint_id, count(*), total(` + mirrorRowEstimate("?3") + `), count(*) - count(` + mirrorRowEstimate("?3") + `)
		  - sum(b.issue_id IN (SELECT value FROM json_each(?5)))
		  FROM tracker_cache_sprints sp
		  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = sp.root_id AND m.sprint_id = sp.sprint_id
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = sp.root_id AND b.issue_id = m.issue_id AND b.board_id = sp.board_id
		 WHERE sp.root_id = CAST(?1 AS INTEGER) AND sp.board_id = ?2`
	mirrorCountSprintsSQL        = mirrorCountSprintsRowsSQL + ` GROUP BY sp.sprint_id`
	mirrorCountSprintsLeftOutSQL = mirrorCountSprintsRowsSQL + ` AND b.issue_id NOT IN (SELECT value FROM json_each(?4)) GROUP BY sp.sprint_id`
	mirrorCountColumnStatusesSQL = `SELECT status_id, position FROM tracker_cache_board_column_statuses
		WHERE root_id = CAST(?1 AS INTEGER) AND board_id = ?2 LIMIT ` + strconv.Itoa(mirrorMaxColumnStatuses)
)

// CountMirrorSections totals the rows of each section of a board the
// mirror shows, by the section rules ListMirrorSection reads, in one
// snapshot with the board's revision. A board it does not show has none.
func (s *Store) CountMirrorSections(rootID int64, boardID string, regroup MirrorRegroup) (MirrorSectionCounts, error) {
	label := fmt.Sprintf("count tracker mirror %d board %s sections", rootID, boardID)
	return mirrorReadSnapshot(s, label, func(tx sqlQueryer) (MirrorSectionCounts, error) {
		board, shown, err := readMirrorShownBoardTx(tx, rootID, boardID)
		if err != nil {
			return MirrorSectionCounts{}, err
		}
		if !shown {
			return MirrorSectionCounts{Sprints: map[string]MirrorSectionTotal{}, Columns: map[int]MirrorSectionTotal{}}, nil
		}
		columns, err := readMirrorColumnPlacesTx(tx, rootID, boardID)
		if err != nil {
			return MirrorSectionCounts{}, err
		}
		return countMirrorSectionsTx(tx, rootID, boardID, board, regroup, columns)
	})
}

// readMirrorColumnPlacesTx reads the column position of each status a
// board's columns hold.
func readMirrorColumnPlacesTx(tx sqlQueryer, rootID int64, boardID string) (map[string]int, error) {
	columns := map[string]int{}
	err := scanMirrorRows(tx, mirrorCountColumnStatusesSQL, []any{rootID, boardID}, func(r rowScanner) error {
		var status string
		var position int
		err := r.Scan(&status, &position)
		columns[status] = position
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	return columns, nil
}

// countMirrorSectionsTx totals a shown board's sections in tx's snapshot,
// by the regrouping; columns places each status the board's columns hold.
// A row the regrouping leaves out of counts adds to no total, and a
// support row counts as a row with no estimate does, but not as one
// without an estimate.
func countMirrorSectionsTx(tx sqlQueryer, rootID int64, boardID string, board mirrorShownBoard, regroup MirrorRegroup,
	columns map[string]int) (MirrorSectionCounts, error) {
	counts := MirrorSectionCounts{Revision: board.revision, TotalsRevision: board.totalsRevision,
		Sprints: map[string]MirrorSectionTotal{}, Columns: map[int]MirrorSectionTotal{}}
	estimated := board.unit != MirrorEstimateNone
	var status, leftOut, support sql.RawBytes
	var estimate sql.NullFloat64
	add := func(t MirrorSectionTotal) MirrorSectionTotal {
		t.Rows++
		if estimated {
			t.Estimate += estimate.Float64
			if !estimate.Valid && support == nil {
				t.Unestimated++
			}
		}
		return t
	}
	var leftOutIssues, supportIssues []string
	var backlog bool
	query, args, dest := mirrorCountRowsSQL, []any{rootID, boardID, board.estimateResetAt, nil, regroup.supportArg()}, []any{&backlog, &status, &estimate}
	if regroup.statuses != "" {
		query, args[3], dest = mirrorRegroupedCountRowsSQL, regroup.statuses, append(dest, &leftOut)
	}
	dest = append(dest, &support)
	err := scanMirrorRows(tx, query, args, func(r rowScanner) error {
		if err := r.Scan(dest...); err != nil {
			return err
		}
		if leftOut != nil {
			leftOutIssues = append(leftOutIssues, string(leftOut))
			return nil
		}
		if support != nil && estimated {
			supportIssues = append(supportIssues, string(support))
		}
		counts.All = add(counts.All)
		if backlog {
			counts.Backlog = add(counts.Backlog)
		}
		if position, ok := columns[string(status)]; ok {
			counts.Columns[position] = add(counts.Columns[position])
		}
		return nil
	})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("rows: %w", err)
	}
	sprints, sprintArgs := mirrorCountSprintsSQL, []any{rootID, boardID, board.estimateResetAt, nil, nil}
	if len(leftOutIssues) > 0 {
		list, err := encodeMirrorIDs(leftOutIssues)
		if err != nil {
			return MirrorSectionCounts{}, err
		}
		sprints, sprintArgs[3] = mirrorCountSprintsLeftOutSQL, list
	}
	if len(supportIssues) > 0 {
		list, err := encodeMirrorIDs(supportIssues)
		if err != nil {
			return MirrorSectionCounts{}, err
		}
		sprintArgs[4] = list
	}
	err = scanMirrorRows(tx, sprints, sprintArgs, func(r rowScanner) error {
		var sprint string
		var total MirrorSectionTotal
		if err := r.Scan(&sprint, &total.Rows, &total.Estimate, &total.Unestimated); err != nil {
			return err
		}
		if !estimated {
			total.Estimate, total.Unestimated = 0, 0
		}
		counts.Sprints[sprint] = total
		return nil
	})
	if err != nil {
		return MirrorSectionCounts{}, fmt.Errorf("sprints: %w", err)
	}
	if board.plansSprints {
		if counts.Pace, err = readMirrorPaceTx(tx, rootID, boardID, board, regroup); err != nil {
			return MirrorSectionCounts{}, fmt.Errorf("pace: %w", err)
		}
	}
	return counts, nil
}
