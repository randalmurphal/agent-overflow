package trackerapp

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A project's boards: which boards an app project lists, in its picker's
// order, and which opens first (store project_tracker_boards). A board
// subscribe is refused for a board its project does not list
// (SubscribeBoard), and the mirror keeps only the boards some project
// lists: replacing a list retires each board whose last listing it
// removes, and that board's subscriptions end with the save, its
// subscribers told Ended (SetProjectBoards).

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
//
// A list that changed takes effect before the answer. Each board the
// write left no project listing has its subscriptions ended, shown or
// hidden (its subscribers told Ended, its handles removed), and the
// mirror cleanup then drains the boards retired and starts the next
// lifetime of a board listed again, so a board removed and added back
// opens and reads at once. The drain is the save's cost: 1.06 s for a
// 20,050-issue board. A cleanup that fails leaves the list saved, and the
// next cleanup finishes it.
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
	changed, unlisted, err := s.store.ReplaceProjectTrackerBoards(projectID, rows)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s: %w", ErrProjectNotFound, projectID, err)
	case errors.Is(err, store.ErrInvalidTrackerBoards):
		return nil, fmt.Errorf("%w: %w", ErrInvalidBoards, err)
	case err != nil:
		return nil, fmt.Errorf("trackerapp: store the boards of project %s: %w", projectID, err)
	case !changed:
		return boards, nil
	}
	s.endUnlistedBoards(unlisted)
	if err := s.clean(); err != nil {
		return nil, fmt.Errorf("trackerapp: the boards of project %s are saved, and clearing the copies of the boards it dropped failed: %w", projectID, err)
	}
	return boards, nil
}

// endUnlistedBoards ends the subscriptions of the boards no project lists
// any more.
func (s *Service) endUnlistedBoards(unlisted []store.TrackerBoardRef) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ref := range unlisted {
		if p := s.subs.boards[Scope{Connection: ref.ConnectionID, Board: tracker.BoardID(ref.BoardID)}]; p != nil {
			s.endBoardLocked(p)
		}
	}
}

func projectBoardsOf(rows []store.ProjectTrackerBoard) []ProjectBoard {
	out := make([]ProjectBoard, len(rows))
	for i, r := range rows {
		out[i] = ProjectBoard{Connection: r.ConnectionID, Board: r.BoardID, OpensFirst: r.OpensFirst}
	}
	return out
}
