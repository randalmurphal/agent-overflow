package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The pane's reads of the mirror. Each is bounded: by a literal LIMIT with
// a more flag, by the bound of what the writer accepts, or by refusing a
// longer list of IDs than mirrorMaxLookup. A detached root, a board the
// mirror does not show and an issue it does not keep read as absent.

const (
	// mirrorSectionPageSize is how many rows one section page holds.
	mirrorSectionPageSize = 100
	// mirrorMaxLookup bounds the IDs one read by ID takes.
	mirrorMaxLookup = 500
	// mirrorIssueRowsPage bounds one ListMirrorIssueRows read.
	mirrorIssueRowsPage = 100
	// mirrorLinkedPage bounds one ListMirrorLinkedIssues read.
	mirrorLinkedPage = 200
)

// mirrorCardColumns are a card's columns, of tracker_cache_issues aliased
// i, in the order scanMirrorCard reads them.
const mirrorCardColumns = `i.issue_id, i.issue_key, i.project_id, i.type_id, i.type_level, i.status_id, i.assignee_id,
	i.priority_id, i.parent_id, i.created_at, i.updated_at, i.resolved_at, i.status_since, i.data,
	(SELECT json_group_array(m.sprint_id) FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id)`

func scanMirrorCard(row rowScanner, extra ...any) (MirrorCard, error) {
	var c MirrorCard
	var sprints []byte
	dest := append([]any{&c.IssueID, &c.Key, &c.ProjectID, &c.TypeID, &c.TypeLevel, &c.StatusID, &c.AssigneeID,
		&c.PriorityID, &c.ParentID, &c.CreatedAt, &c.UpdatedAt, &c.ResolvedAt, &c.StatusSince, &c.Data, &sprints}, extra...)
	if err := row.Scan(dest...); err != nil {
		return MirrorCard{}, err
	}
	if len(sprints) > 2 {
		if err := json.Unmarshal(sprints, &c.Sprints); err != nil {
			return MirrorCard{}, fmt.Errorf("decode card %s sprints: %w", c.IssueID, err)
		}
		// Sorted here, not by the aggregate, which would sort in a
		// temporary b-tree per card.
		slices.Sort(c.Sprints)
	}
	return c, nil
}

// GetMirrorBoard reads the structure of a board the mirror shows: the
// board, its columns with their statuses, its sprints and its lifetime.
// The catalog is read by ID (GetMirrorCatalog).
func (s *Store) GetMirrorBoard(rootID int64, boardID string) (st MirrorStructure, ok bool, err error) {
	action := fmt.Sprintf("store: read tracker mirror %d board %s", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorStructure{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	b := &st.Board
	err = tx.QueryRow(`SELECT b.board_id, b.project_id, b.name, b.plans_sprints, b.estimate_unit, b.data, b.lifetime FROM tracker_cache_boards b
		WHERE b.root_id = ?1 AND b.board_id = ?2 AND `+mirrorBoardShown("?1", "?2"), rootID, boardID).Scan(
		&b.ID, &b.ProjectID, &b.Name, &b.PlansSprints, &b.EstimateUnit, &b.Data, &st.Lifetime)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorStructure{}, false, nil
	}
	if err != nil {
		return MirrorStructure{}, false, fmt.Errorf("%s: %w", action, err)
	}
	if st.Columns, err = readMirrorColumnsTx(tx, rootID, boardID); err != nil {
		return MirrorStructure{}, false, fmt.Errorf("%s: %w", action, err)
	}
	if st.Sprints, err = readMirrorSprintsTx(tx, rootID, boardID); err != nil {
		return MirrorStructure{}, false, fmt.Errorf("%s: %w", action, err)
	}
	return st, true, tx.Commit()
}

func readMirrorColumnsTx(tx *sql.Tx, rootID int64, boardID string) ([]MirrorColumn, error) {
	names, err := queryIDs(tx, `SELECT name FROM tracker_cache_board_columns WHERE root_id = ? AND board_id = ?
		ORDER BY position LIMIT `+strconv.Itoa(mirrorMaxColumns), rootID, boardID)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	columns := make([]MirrorColumn, len(names))
	for i, name := range names {
		columns[i].Name = name
	}
	err = scanMirrorRows(tx, `SELECT position, status_id FROM tracker_cache_board_column_statuses WHERE root_id = ? AND board_id = ?
		ORDER BY position, rank LIMIT `+strconv.Itoa(mirrorMaxColumnStatuses), []any{rootID, boardID}, func(r rowScanner) error {
		var position int
		var status string
		if err := r.Scan(&position, &status); err != nil {
			return err
		}
		if position < len(columns) {
			columns[position].Statuses = append(columns[position].Statuses, status)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read column statuses: %w", err)
	}
	return columns, nil
}

func readMirrorSprintsTx(tx *sql.Tx, rootID int64, boardID string) ([]MirrorSprint, error) {
	var sprints []MirrorSprint
	err := scanMirrorRows(tx, `SELECT sprint_id, origin_board_id, name, goal, state, start_at, end_at, completed_at
		FROM tracker_cache_sprints WHERE root_id = ? AND board_id = ?
		ORDER BY position, sprint_id LIMIT `+strconv.Itoa(mirrorMaxSprints), []any{rootID, boardID}, func(r rowScanner) error {
		var sp MirrorSprint
		err := r.Scan(&sp.ID, &sp.OriginBoardID, &sp.Name, &sp.Goal, &sp.State, &sp.StartAt, &sp.EndAt, &sp.CompletedAt)
		sprints = append(sprints, sp)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read sprints: %w", err)
	}
	return sprints, nil
}

func encodeMirrorLookup(ids []string) (string, error) {
	if len(ids) > mirrorMaxLookup {
		return "", fmt.Errorf("%w: %d IDs, over %d", ErrInvalidTrackerCache, len(ids), mirrorMaxLookup)
	}
	return encodeMirrorIDs(ids)
}

// GetMirrorCatalog reads the users and statuses of rootID named by ID,
// each sorted by ID; an ID the mirror does not hold is left out.
func (s *Store) GetMirrorCatalog(rootID int64, users, statuses []string) (catalog MirrorCatalog, err error) {
	action := fmt.Sprintf("store: read tracker mirror %d catalog", rootID)
	userList, err := encodeMirrorLookup(users)
	if err != nil {
		return MirrorCatalog{}, err
	}
	statusList, err := encodeMirrorLookup(statuses)
	if err != nil {
		return MirrorCatalog{}, err
	}
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorCatalog{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if len(users) > 0 {
		err = scanMirrorRows(tx, `SELECT u.user_id, u.name, u.data FROM json_each(?2) l
			CROSS JOIN tracker_cache_users u ON u.root_id = ?1 AND u.user_id = l.value
			WHERE `+mirrorRootLiveSQL, []any{rootID, userList}, func(r rowScanner) error {
			var u MirrorUser
			err := r.Scan(&u.ID, &u.Name, &u.Data)
			catalog.Users = append(catalog.Users, u)
			return err
		})
		if err != nil {
			return MirrorCatalog{}, fmt.Errorf("%s: users: %w", action, err)
		}
	}
	if len(statuses) > 0 {
		err = scanMirrorRows(tx, `SELECT st.status_id, st.name, st.status_group FROM json_each(?2) l
			CROSS JOIN tracker_cache_statuses st ON st.root_id = ?1 AND st.status_id = l.value
			WHERE `+mirrorRootLiveSQL, []any{rootID, statusList}, func(r rowScanner) error {
			var st MirrorStatus
			err := r.Scan(&st.ID, &st.Name, &st.Group)
			catalog.Statuses = append(catalog.Statuses, st)
			return err
		})
		if err != nil {
			return MirrorCatalog{}, fmt.Errorf("%s: statuses: %w", action, err)
		}
	}
	// Sorted here, not by the statements: the ID list drives each, so an
	// ORDER BY would sort in a temporary b-tree.
	slices.SortFunc(catalog.Users, func(a, b MirrorUser) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(catalog.Statuses, func(a, b MirrorStatus) int { return strings.Compare(a.ID, b.ID) })
	return catalog, tx.Commit()
}

// scanMirrorRows runs query and calls scan for each row.
func scanMirrorRows(q sqlQueryer, query string, args []any, scan func(rowScanner) error) error {
	rows, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			return errors.Join(err, rows.Close())
		}
	}
	return errors.Join(rows.Err(), rows.Close())
}

// mirrorCardsSQL reads the cards of the kept issues among a JSON list of
// IDs, probing each by key.
var mirrorCardsSQL = `SELECT ` + mirrorCardColumns + ` FROM json_each(?2) l
	CROSS JOIN tracker_cache_issues i ON i.root_id = ?1 AND i.issue_id = l.value
	WHERE ` + mirrorIssueKept("i.root_id", "i.issue_id")

// ListMirrorCards reads the cards of issueIDs the mirror keeps, sorted by
// issue ID; any other issue is left out.
func (s *Store) ListMirrorCards(rootID int64, issueIDs []string) ([]MirrorCard, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	encoded, err := encodeMirrorLookup(issueIDs)
	if err != nil {
		return nil, err
	}
	cards := make([]MirrorCard, 0, len(issueIDs))
	err = scanMirrorRows(s.reader(), mirrorCardsSQL, []any{rootID, encoded}, func(r rowScanner) error {
		card, err := scanMirrorCard(r)
		cards = append(cards, card)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %d cards: %w", rootID, err)
	}
	slices.SortFunc(cards, func(a, b MirrorCard) int { return strings.Compare(a.IssueID, b.IssueID) })
	return cards, nil
}

// MirrorIssueRow is an issue's row on one board, without its card.
type MirrorIssueRow struct {
	BoardID, OrderKey string
	Estimate          *float64
}

// mirrorIssueRowsSQL reads a page of an issue's rows on the boards the
// mirror shows, by board ID after ?3, from the primary key.
var mirrorIssueRowsSQL = `SELECT b.board_id, b.order_key, b.estimate FROM tracker_cache_board_issues b
	WHERE b.root_id = ?1 AND b.issue_id = ?2 AND b.board_id > ?3 AND ` + mirrorBoardShown("b.root_id", "b.board_id") + `
	ORDER BY b.board_id LIMIT ` + strconv.Itoa(mirrorIssueRowsPage+1)

// ListMirrorIssueRows reads up to mirrorIssueRowsPage of one issue's rows
// on the boards of rootID the mirror shows, by board ID after afterBoard
// (empty for the first); more reports that rows follow the last.
func (s *Store) ListMirrorIssueRows(rootID int64, issueID, afterBoard string) (rows []MirrorIssueRow, more bool, err error) {
	err = scanMirrorRows(s.reader(), mirrorIssueRowsSQL, []any{rootID, issueID, afterBoard}, func(r rowScanner) error {
		var row MirrorIssueRow
		err := r.Scan(&row.BoardID, &row.OrderKey, &row.Estimate)
		rows = append(rows, row)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("store: read tracker mirror %d issue %s rows: %w", rootID, issueID, err)
	}
	if len(rows) > mirrorIssueRowsPage {
		return rows[:mirrorIssueRowsPage], true, nil
	}
	return rows, false, nil
}

// mirrorOffBoardLinkedSQL reads a page of the issues the links of a root
// name (mirrorLinksSQL, the keep rule's own) that have no row on a board
// the mirror shows, by issue ID after ?2: the tickets' issue index in
// order, and per ticket a probe of the issue's rows by key.
var mirrorOffBoardLinkedSQL = `SELECT DISTINCT link_ticket.issue_id FROM ` + mirrorLinksSQL("?1") + `
	AND link_ticket.issue_id > ?2
	AND NOT EXISTS (SELECT 1 FROM tracker_cache_board_issues on_board
	                 WHERE on_board.root_id = ?1 AND on_board.issue_id = link_ticket.issue_id
	                   AND ` + mirrorBoardShown("on_board.root_id", "on_board.board_id") + `)
	ORDER BY link_ticket.issue_id LIMIT ` + strconv.Itoa(mirrorLinkedPage+1)

// ListMirrorOffBoardLinkedIssues reads up to mirrorLinkedPage of the
// distinct issues that threads this computer owns link through rootID's
// connection and that no board the mirror shows has a row of, so no board
// read keeps their cards fresh; sorted, after after (empty for the
// first); more reports that issues follow the last. A detached root has
// none. A page reads the links in issue order until it has a page, so it
// reads at most every link of the connection.
func (s *Store) ListMirrorOffBoardLinkedIssues(rootID int64, after string) (issueIDs []string, more bool, err error) {
	issueIDs, err = queryIDs(s.reader(), mirrorOffBoardLinkedSQL, rootID, after)
	if err != nil {
		return nil, false, fmt.Errorf("store: list tracker mirror %d linked issues: %w", rootID, err)
	}
	if len(issueIDs) > mirrorLinkedPage {
		return issueIDs[:mirrorLinkedPage], true, nil
	}
	return issueIDs, false, nil
}
