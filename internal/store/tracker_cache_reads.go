package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// The pane's reads of the mirror. Each is bounded: by a literal LIMIT, by
// the bound of what the writer accepts, or by refusing a longer list of
// IDs than mirrorMaxLookup.

// mirrorSectionPageSize is how many rows one section page holds.
const mirrorSectionPageSize = 100

// mirrorMaxLookup bounds the IDs one read by ID takes.
const mirrorMaxLookup = 500

// mirrorMaxIssueBoards bounds the boards one issue's rows are read from.
const mirrorMaxIssueBoards = 500

// mirrorCardColumns are a card's columns, of tracker_cache_issues aliased
// i, in the order scanMirrorCard reads them.
const mirrorCardColumns = `i.issue_id, i.issue_key, i.project_id, i.type_id, i.type_level, i.status_id, i.assignee_id,
	i.priority_id, i.parent_id, i.created_at, i.updated_at, i.resolved_at, i.data,
	(SELECT json_group_array(m.sprint_id) FROM tracker_cache_issue_sprints m WHERE m.root_id = i.root_id AND m.issue_id = i.issue_id)`

func scanMirrorCard(row rowScanner, extra ...any) (MirrorCard, error) {
	var c MirrorCard
	var sprints string
	dest := append([]any{&c.IssueID, &c.Key, &c.ProjectID, &c.TypeID, &c.TypeLevel, &c.StatusID, &c.AssigneeID,
		&c.PriorityID, &c.ParentID, &c.CreatedAt, &c.UpdatedAt, &c.ResolvedAt, &c.Data, &sprints}, extra...)
	if err := row.Scan(dest...); err != nil {
		return MirrorCard{}, err
	}
	if err := json.Unmarshal([]byte(sprints), &c.Sprints); err != nil {
		return MirrorCard{}, fmt.Errorf("decode card %s sprints: %w", c.IssueID, err)
	}
	if len(c.Sprints) == 0 {
		c.Sprints = nil
	}
	slices.Sort(c.Sprints)
	return c, nil
}

// GetMirrorBoard reads a board's structure: the board, its columns with
// their statuses and its sprints. The catalog is read by ID
// (GetMirrorCatalog).
func (s *Store) GetMirrorBoard(rootID, boardID string) (st MirrorStructure, ok bool, err error) {
	action := fmt.Sprintf("store: read tracker mirror %s board %s", rootID, boardID)
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorStructure{}, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	b := &st.Board
	err = tx.QueryRow(`SELECT board_id, project_id, name, plans_sprints, estimate_unit, data FROM tracker_cache_boards
		WHERE root_id = ? AND board_id = ?`, rootID, boardID).Scan(&b.ID, &b.ProjectID, &b.Name, &b.PlansSprints, &b.EstimateUnit, &b.Data)
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

func readMirrorColumnsTx(tx *sql.Tx, rootID, boardID string) ([]MirrorColumn, error) {
	names, err := queryIDs(tx, `SELECT name FROM tracker_cache_board_columns WHERE root_id = ? AND board_id = ?
		ORDER BY position LIMIT `+strconv.Itoa(mirrorMaxColumns), rootID, boardID)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	columns := make([]MirrorColumn, len(names))
	for i, name := range names {
		columns[i].Name = name
	}
	rows, err := tx.Query(`SELECT position, status_id FROM tracker_cache_board_column_statuses WHERE root_id = ? AND board_id = ?
		ORDER BY position, rank LIMIT `+strconv.Itoa(mirrorMaxColumnStatuses), rootID, boardID)
	if err != nil {
		return nil, fmt.Errorf("read column statuses: %w", err)
	}
	for rows.Next() {
		var position int
		var status string
		if err := rows.Scan(&position, &status); err != nil {
			return nil, errors.Join(fmt.Errorf("scan column status: %w", err), rows.Close())
		}
		if position < len(columns) {
			columns[position].Statuses = append(columns[position].Statuses, status)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read column statuses: %w", err)
	}
	return columns, nil
}

func readMirrorSprintsTx(tx *sql.Tx, rootID, boardID string) ([]MirrorSprint, error) {
	rows, err := tx.Query(`SELECT sprint_id, origin_board_id, name, goal, state, start_at, end_at, completed_at
		FROM tracker_cache_sprints WHERE root_id = ? AND board_id = ?
		ORDER BY position, sprint_id LIMIT `+strconv.Itoa(mirrorMaxSprints), rootID, boardID)
	if err != nil {
		return nil, fmt.Errorf("read sprints: %w", err)
	}
	var sprints []MirrorSprint
	for rows.Next() {
		var sp MirrorSprint
		if err := rows.Scan(&sp.ID, &sp.OriginBoardID, &sp.Name, &sp.Goal, &sp.State, &sp.StartAt, &sp.EndAt, &sp.CompletedAt); err != nil {
			return nil, errors.Join(fmt.Errorf("scan sprint: %w", err), rows.Close())
		}
		sprints = append(sprints, sp)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
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

// GetMirrorCatalog reads the projects, users and statuses of rootID named
// by ID, each sorted by ID; an ID the mirror does not hold is left out.
func (s *Store) GetMirrorCatalog(rootID string, projects, users, statuses []string) (catalog MirrorCatalog, err error) {
	action := fmt.Sprintf("store: read tracker mirror %s catalog", rootID)
	lists := make([]string, 3)
	for i, ids := range [][]string{projects, users, statuses} {
		if lists[i], err = encodeMirrorLookup(ids); err != nil {
			return MirrorCatalog{}, err
		}
	}
	tx, err := s.reader().Begin()
	if err != nil {
		return MirrorCatalog{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	err = scanMirrorRows(tx, `SELECT project_id, project_key, name, data FROM tracker_cache_projects
		WHERE root_id = ? AND project_id IN (SELECT value FROM json_each(?)) ORDER BY project_id`, []any{rootID, lists[0]},
		func(r rowScanner) error {
			var p MirrorProject
			err := r.Scan(&p.ID, &p.Key, &p.Name, &p.Data)
			catalog.Projects = append(catalog.Projects, p)
			return err
		})
	if err != nil {
		return MirrorCatalog{}, fmt.Errorf("%s: projects: %w", action, err)
	}
	err = scanMirrorRows(tx, `SELECT user_id, name, data FROM tracker_cache_users
		WHERE root_id = ? AND user_id IN (SELECT value FROM json_each(?)) ORDER BY user_id`, []any{rootID, lists[1]},
		func(r rowScanner) error {
			var u MirrorUser
			err := r.Scan(&u.ID, &u.Name, &u.Data)
			catalog.Users = append(catalog.Users, u)
			return err
		})
	if err != nil {
		return MirrorCatalog{}, fmt.Errorf("%s: users: %w", action, err)
	}
	err = scanMirrorRows(tx, `SELECT status_id, name, status_group FROM tracker_cache_statuses
		WHERE root_id = ? AND status_id IN (SELECT value FROM json_each(?)) ORDER BY status_id`, []any{rootID, lists[2]},
		func(r rowScanner) error {
			var st MirrorStatus
			err := r.Scan(&st.ID, &st.Name, &st.Group)
			catalog.Statuses = append(catalog.Statuses, st)
			return err
		})
	if err != nil {
		return MirrorCatalog{}, fmt.Errorf("%s: statuses: %w", action, err)
	}
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

// ListMirrorCards reads the cards of issueIDs, sorted by issue ID; an
// issue the mirror does not hold is left out.
func (s *Store) ListMirrorCards(rootID string, issueIDs []string) ([]MirrorCard, error) {
	encoded, err := encodeMirrorLookup(issueIDs)
	if err != nil {
		return nil, err
	}
	var cards []MirrorCard
	err = scanMirrorRows(s.reader(), `SELECT `+mirrorCardColumns+` FROM tracker_cache_issues i
		WHERE i.root_id = ? AND i.issue_id IN (SELECT value FROM json_each(?)) ORDER BY i.issue_id`, []any{rootID, encoded},
		func(r rowScanner) error {
			card, err := scanMirrorCard(r)
			cards = append(cards, card)
			return err
		})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %s cards: %w", rootID, err)
	}
	return cards, nil
}

// MirrorIssueRow is an issue's row on one board, without its card.
type MirrorIssueRow struct {
	BoardID, OrderKey string
	Estimate          *float64
}

// ListMirrorIssueRows reads one issue's rows on every board of rootID that
// has it, by board ID.
func (s *Store) ListMirrorIssueRows(rootID, issueID string) ([]MirrorIssueRow, error) {
	var out []MirrorIssueRow
	err := scanMirrorRows(s.reader(), `SELECT board_id, order_key, estimate FROM tracker_cache_board_issues
		WHERE root_id = ? AND issue_id = ? ORDER BY board_id LIMIT `+strconv.Itoa(mirrorMaxIssueBoards), []any{rootID, issueID},
		func(r rowScanner) error {
			var row MirrorIssueRow
			err := r.Scan(&row.BoardID, &row.OrderKey, &row.Estimate)
			out = append(out, row)
			return err
		})
	if err != nil {
		return nil, fmt.Errorf("store: read tracker mirror %s issue %s rows: %w", rootID, issueID, err)
	}
	return out, nil
}
