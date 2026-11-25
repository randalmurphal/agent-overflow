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
// removes, whose subscriptions end with the save, its subscribers told
// Ended, and readies each board it lists again (SetProjectBoards).

var (
	// ErrProjectNotFound is a project that does not exist on this computer.
	ErrProjectNotFound = errors.New("trackerapp: no such project")
	// ErrInvalidBoards is a board list the store refuses: a board with no
	// connection or ID, a board listed twice, two boards that open first,
	// a name or tracker project longer than 255 characters, or a
	// connection that does not exist or has no boards (Tempo).
	ErrInvalidBoards = errors.New("trackerapp: invalid board list")
	// ErrBoardsChanged is a save built from a list that is no longer the
	// stored one: another save, or a connection's removal, changed it.
	ErrBoardsChanged = errors.New("trackerapp: the project's boards changed after they were read")
)

// ProjectBoard is one board of a list SetProjectBoards stores. Name and
// TrackerProject are what the board picker showed when the person chose
// the board (BoardInfoView.Name, and ProjectView.Key of its project), each
// at most 255 characters, so the board is named before it is read.
type ProjectBoard struct {
	Connection     string `json:"connection"`
	Board          string `json:"board"`
	OpensFirst     bool   `json:"opens_first,omitempty"`
	Name           string `json:"name,omitempty"`
	TrackerProject string `json:"tracker_project,omitempty"`
}

// BoardRef is one board of the list a save was built from, as
// ProjectBoards answered it: its connection, its board and whether it
// opens first.
type BoardRef struct {
	Connection string `json:"connection"`
	Board      string `json:"board"`
	OpensFirst bool   `json:"opens_first,omitempty"`
}

// BoardChoice is one board a project lists as its board picker shows it:
// the listing, its connection's kind, label, whether it is on and the
// account it was verified as, and the board's name, which is this
// computer's copy of the name once the board has been read and the name
// the listing stored until then (store ListTrackerBoardChoices).
type BoardChoice struct {
	Project           string `json:"project"`
	Connection        string `json:"connection"`
	ConnectionKind    string `json:"connection_kind"`
	ConnectionLabel   string `json:"connection_label"`
	ConnectionEnabled bool   `json:"connection_enabled"`
	VerifiedAccount   string `json:"verified_account"`
	Board             string `json:"board"`
	Name              string `json:"name"`
	TrackerProject    string `json:"tracker_project"`
	OpensFirst        bool   `json:"opens_first"`
}

// ProjectBoards lists the boards projectID lists, in picker order; none
// for a project that lists none. A project ID is required.
func (s *Service) ProjectBoards(projectID string) ([]BoardChoice, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: a project's boards name the project", ErrProjectNotFound)
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	return s.boardChoices(projectID)
}

// BoardChoices lists the boards every project of this computer lists, by
// project name, then project ID, then picker order: the Issues pane's
// board picker.
func (s *Service) BoardChoices() ([]BoardChoice, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	return s.boardChoices("")
}

// boardChoices reads projectID's board choices, or with projectID empty
// every project's. The caller is inside the service (enter).
func (s *Service) boardChoices(projectID string) ([]BoardChoice, error) {
	rows, err := s.store.ListTrackerBoardChoices(projectID)
	if err != nil {
		if projectID == "" {
			return nil, fmt.Errorf("trackerapp: read the boards every project lists: %w", err)
		}
		return nil, fmt.Errorf("trackerapp: read the boards of project %s: %w", projectID, err)
	}
	out := make([]BoardChoice, len(rows))
	for i, r := range rows {
		out[i] = BoardChoice{
			Project: r.ProjectID, Connection: r.ConnectionID, ConnectionKind: r.ConnectionKind,
			ConnectionLabel: r.ConnectionLabel, ConnectionEnabled: r.ConnectionEnabled, VerifiedAccount: r.ConnectionVerifiedAccount,
			Board: r.BoardID, Name: r.BoardName, TrackerProject: r.TrackerProject, OpensFirst: r.OpensFirst,
		}
	}
	return out, nil
}

// SetProjectBoards makes boards, in order, the whole list of boards
// projectID lists, and answers the list stored as ProjectBoards reads it.
// held is the list, in order, that boards was built from; when the stored
// list is not that one the save fails with ErrBoardsChanged. A missing
// project fails with ErrProjectNotFound and a list the store refuses with
// ErrInvalidBoards. Each failure leaves the stored list as it was. A
// board the list keeps keeps the name and tracker project stored when it
// was added.
//
// A list that changed takes effect before the answer, and the save does
// only what its own list needs. Each board the write left no project
// listing has its subscriptions ended, shown or hidden (its subscribers
// told Ended, its handles removed); its stored copy is the cleanup's to
// clear (FinishMirrorCleanup's callers). Each board the write added whose
// stored copy is retired and not yet cleared is given its next lifetime
// (store ReadyTrackerBoards), so a board removed and added back opens and
// reads at once: that drain of the one board is the only wait a save
// adds. A reorder or a move of the board that opens first does neither.
// A readying that fails leaves the list saved, and the next cleanup
// readies the board.
func (s *Service) SetProjectBoards(projectID string, held []BoardRef, boards []ProjectBoard) ([]BoardChoice, error) {
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	heldRows := make([]store.HeldTrackerBoard, len(held))
	for i, b := range held {
		heldRows[i] = store.HeldTrackerBoard{ConnectionID: b.Connection, BoardID: b.Board, OpensFirst: b.OpensFirst}
	}
	rows := make([]store.ProjectTrackerBoard, len(boards))
	for i, b := range boards {
		rows[i] = store.ProjectTrackerBoard{
			ConnectionID: b.Connection, BoardID: b.Board, OpensFirst: b.OpensFirst,
			Name: b.Name, TrackerProject: b.TrackerProject,
		}
	}
	_, listing, err := s.store.ReplaceProjectTrackerBoards(projectID, heldRows, rows)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s: %w", ErrProjectNotFound, projectID, err)
	case errors.Is(err, store.ErrTrackerBoardsChanged):
		return nil, fmt.Errorf("%w: %w", ErrBoardsChanged, err)
	case errors.Is(err, store.ErrInvalidTrackerBoards):
		return nil, fmt.Errorf("%w: %w", ErrInvalidBoards, err)
	case err != nil:
		return nil, fmt.Errorf("trackerapp: store the boards of project %s: %w", projectID, err)
	}
	s.endUnlistedBoards(listing.Unlisted)
	if len(listing.Relisted) > 0 {
		err := s.store.ReadyTrackerBoards(s.halting, listing.Relisted)
		if err != nil && s.halting.Err() != nil {
			err = errors.Join(ErrStopped, err)
		}
		if err != nil {
			return nil, fmt.Errorf("trackerapp: the boards of project %s are saved, and readying the boards it listed again failed, so they open after the next cleanup: %w", projectID, err)
		}
	}
	out, err := s.boardChoices(projectID)
	if err != nil {
		return nil, fmt.Errorf("trackerapp: the boards of project %s are saved, and reading them back failed, so read them again: %w", projectID, err)
	}
	return out, nil
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
