package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidTrackerBoards is returned by ReplaceProjectTrackerBoards for a
// list the table refuses: an empty connection or board ID, a board listed
// twice, more than one board that opens first, or a connection that does
// not exist or has no boards (Tempo).
var ErrInvalidTrackerBoards = errors.New("store: invalid tracker board list")

// TrackerBoardRef names one board: the tracker's own board ID within a
// connection.
type TrackerBoardRef struct {
	ConnectionID string
	BoardID      string
}

// ProjectTrackerBoard is one board an app project lists (migration v159).
// A project's list is ordered as its board picker shows it, and at most
// one board opens first.
type ProjectTrackerBoard struct {
	ConnectionID string
	BoardID      string
	OpensFirst   bool
}

// trackerBoardRow is one board of ReplaceProjectTrackerBoards' JSON list;
// its index in the list is its position.
type trackerBoardRow struct {
	ConnectionID string `json:"c"`
	BoardID      string `json:"b"`
	OpensFirst   int    `json:"f"`
}

func validateProjectTrackerBoards(boards []ProjectTrackerBoard) error {
	seen := make(map[TrackerBoardRef]bool, len(boards))
	opensFirst := 0
	for _, board := range boards {
		ref := TrackerBoardRef{ConnectionID: board.ConnectionID, BoardID: board.BoardID}
		switch {
		case ref.ConnectionID == "" || ref.BoardID == "":
			return fmt.Errorf("%w: a board needs its connection and board ID", ErrInvalidTrackerBoards)
		case seen[ref]:
			return fmt.Errorf("%w: board %s of connection %s is listed twice", ErrInvalidTrackerBoards, ref.BoardID, ref.ConnectionID)
		}
		seen[ref] = true
		if board.OpensFirst {
			opensFirst++
		}
	}
	if opensFirst > 1 {
		return fmt.Errorf("%w: %d boards open first", ErrInvalidTrackerBoards, opensFirst)
	}
	return nil
}

// ReplaceProjectTrackerBoards makes boards, in order, the whole list of
// boards projectID lists, and reports whether the list changed. A missing
// project fails with sql.ErrNoRows and an invalid list with
// ErrInvalidTrackerBoards; either leaves the stored list as it was.
func (s *Store) ReplaceProjectTrackerBoards(projectID string, boards []ProjectTrackerBoard) (changed bool, err error) {
	if err := validateProjectTrackerBoards(boards); err != nil {
		return false, err
	}
	rows := make([]trackerBoardRow, 0, len(boards))
	connections := make([]string, 0, len(boards))
	for _, board := range boards {
		rows = append(rows, trackerBoardRow{ConnectionID: board.ConnectionID, BoardID: board.BoardID, OpensFirst: boolToInt(board.OpensFirst)})
		connections = append(connections, board.ConnectionID)
	}
	encodedRows, err := json.Marshal(rows)
	if err != nil {
		return false, fmt.Errorf("store: replace project %s tracker boards: encode: %w", projectID, err)
	}
	encodedConnections, err := json.Marshal(uniqueNonEmptyStrings(connections))
	if err != nil {
		return false, fmt.Errorf("store: replace project %s tracker boards: encode: %w", projectID, err)
	}

	action := fmt.Sprintf("store: replace project %s tracker boards", projectID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if err := tx.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(new(int)); err != nil {
		return false, fmt.Errorf("%s: read project: %w", action, err)
	}
	unknown, err := queryIDs(tx, `SELECT value FROM json_each(?)
		WHERE value NOT IN (SELECT id FROM tracker_connections WHERE kind <> 'tempo') ORDER BY value`, string(encodedConnections))
	if err != nil {
		return false, fmt.Errorf("%s: check connections: %w", action, err)
	}
	if len(unknown) > 0 {
		return false, fmt.Errorf("%w: connection %s does not exist or has no boards", ErrInvalidTrackerBoards, unknown[0])
	}
	current, err := listProjectTrackerBoards(tx, projectID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if slices.Equal(current, boards) {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM project_tracker_boards WHERE project_id = ?`, projectID); err != nil {
		return false, fmt.Errorf("%s: clear: %w", action, err)
	}
	if _, err := tx.Exec(`INSERT INTO project_tracker_boards(project_id, connection_id, board_id, position, opens_first)
		SELECT ?, json_extract(value, '$.c'), json_extract(value, '$.b'), key, json_extract(value, '$.f')
		  FROM json_each(?)`, projectID, string(encodedRows)); err != nil {
		return false, fmt.Errorf("%s: insert: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return true, nil
}

// ListProjectTrackerBoards lists the boards projectID lists, in picker
// order; none for a project that lists none or does not exist.
func (s *Store) ListProjectTrackerBoards(projectID string) ([]ProjectTrackerBoard, error) {
	boards, err := listProjectTrackerBoards(s.reader(), projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list project %s tracker boards: %w", projectID, err)
	}
	return boards, nil
}

func listProjectTrackerBoards(q sqlQueryer, projectID string) ([]ProjectTrackerBoard, error) {
	rows, err := q.Query(`SELECT connection_id, board_id, opens_first FROM project_tracker_boards
		WHERE project_id = ? ORDER BY position`, projectID)
	if err != nil {
		return nil, err
	}
	var out []ProjectTrackerBoard
	for rows.Next() {
		var board ProjectTrackerBoard
		if err := rows.Scan(&board.ConnectionID, &board.BoardID, &board.OpensFirst); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, board)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// ListTrackerBoardsListedByProjects lists every board some app project
// lists, once each, ordered by connection and board ID.
func (s *Store) ListTrackerBoardsListedByProjects() ([]TrackerBoardRef, error) {
	rows, err := s.reader().QueryContext(context.Background(), `SELECT DISTINCT connection_id, board_id
		FROM project_tracker_boards ORDER BY connection_id, board_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list listed tracker boards: %w", err)
	}
	var out []TrackerBoardRef
	for rows.Next() {
		var ref TrackerBoardRef
		if err := rows.Scan(&ref.ConnectionID, &ref.BoardID); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan listed tracker board: %w", err), rows.Close())
		}
		out = append(out, ref)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list listed tracker boards: %w", err)
	}
	return out, nil
}

// ListProjectsForTrackerBoard lists the projects that list a board, by
// project ID.
func (s *Store) ListProjectsForTrackerBoard(board TrackerBoardRef) ([]string, error) {
	ids, err := queryIDs(s.reader(), `SELECT project_id FROM project_tracker_boards
		WHERE connection_id = ? AND board_id = ? ORDER BY project_id`, board.ConnectionID, board.BoardID)
	if err != nil {
		return nil, fmt.Errorf("store: list projects listing board %s of connection %s: %w", board.BoardID, board.ConnectionID, err)
	}
	return ids, nil
}
