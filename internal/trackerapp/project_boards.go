package trackerapp

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
)

// A project's boards: which boards an app project lists, in its picker's
// order, and which opens first (store project_tracker_boards). A board
// subscribe is refused for a board its project does not list
// (SubscribeBoard), and the mirror keeps only the boards some project
// lists: replacing a list retires each board whose last listing it
// removes, and that board's pump ends at its next tick, telling its
// subscribers (Ended).

var (
	// ErrProjectNotFound is a project that does not exist on this computer.
	ErrProjectNotFound = errors.New("trackerapp: no such project")
	// ErrInvalidBoards is a board list the store refuses: a board with no
	// connection or ID, a board listed twice, two boards that open first,
	// or a connection that does not exist or has no boards (Tempo).
	ErrInvalidBoards = errors.New("trackerapp: invalid board list")
)

// ProjectBoard is one board a project lists.
type ProjectBoard struct {
	Connection string `json:"connection"`
	Board      string `json:"board"`
	OpensFirst bool   `json:"opens_first,omitempty"`
}

// ProjectBoards lists the boards projectID lists, in picker order; none
// for a project that lists none.
func (s *Service) ProjectBoards(projectID string) ([]ProjectBoard, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	rows, err := s.store.ListProjectTrackerBoards(projectID)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: read the boards of project %s: %w", projectID, err)
	}
	return projectBoardsOf(rows), nil
}

// SetProjectBoards makes boards, in order, the whole list of boards
// projectID lists, and answers the list stored. A missing project fails
// with ErrProjectNotFound and a list the store refuses with
// ErrInvalidBoards; either leaves the stored list as it was.
func (s *Service) SetProjectBoards(projectID string, boards []ProjectBoard) ([]ProjectBoard, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	if boards == nil {
		boards = []ProjectBoard{}
	}
	rows := make([]store.ProjectTrackerBoard, len(boards))
	for i, b := range boards {
		rows[i] = store.ProjectTrackerBoard{ConnectionID: b.Connection, BoardID: b.Board, OpensFirst: b.OpensFirst}
	}
	_, err := s.store.ReplaceProjectTrackerBoards(projectID, rows)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s: %w", ErrProjectNotFound, projectID, err)
	case errors.Is(err, store.ErrInvalidTrackerBoards):
		return nil, fmt.Errorf("%w: %w", ErrInvalidBoards, err)
	case err != nil:
		return nil, fmt.Errorf("trackerapp: store the boards of project %s: %w", projectID, err)
	}
	return boards, nil
}

func projectBoardsOf(rows []store.ProjectTrackerBoard) []ProjectBoard {
	out := make([]ProjectBoard, len(rows))
	for i, r := range rows {
		out[i] = ProjectBoard{Connection: r.ConnectionID, Board: r.BoardID, OpensFirst: r.OpensFirst}
	}
	return out
}
