package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Mirror enumerations, aligned with the CHECKs of migration v159.
const (
	// Status groups.
	MirrorGroupBacklog    = 1
	MirrorGroupToDo       = 2
	MirrorGroupInProgress = 3
	MirrorGroupDone       = 4
	MirrorGroupCanceled   = 5

	// Sprint states.
	MirrorSprintFuture = 1
	MirrorSprintActive = 2
	MirrorSprintClosed = 3

	// Estimate units: none, points, time in seconds.
	MirrorEstimateNone   = 0
	MirrorEstimatePoints = 1
	MirrorEstimateTime   = 2
)

// Bounds of one board's structure, which bound the reads of it.
const (
	mirrorMaxColumns        = 100
	mirrorMaxColumnStatuses = 1000
	mirrorMaxSprints        = 500
)

// MirrorProject is a tracker project; Data is the rest of it.
type MirrorProject struct {
	ID, Key, Name string
	Data          []byte
}

// MirrorUser is a tracker user; Data is the rest of it.
type MirrorUser struct {
	ID, Name string
	Data     []byte
}

// MirrorStatus is a tracker status with its group, MirrorGroupBacklog
// through MirrorGroupCanceled.
type MirrorStatus struct {
	ID, Name string
	Group    int
}

// MirrorCatalog is the projects, users and statuses a read names. They
// belong to the root, not to a board: a write upserts them and never
// removes one.
type MirrorCatalog struct {
	Projects []MirrorProject
	Users    []MirrorUser
	Statuses []MirrorStatus
}

// MirrorBoard is a board: its project (empty when the tracker does not say),
// whether it plans in sprints, its estimate unit (MirrorEstimate*), and
// Data for the rest (its estimate choices).
type MirrorBoard struct {
	ID, ProjectID, Name string
	PlansSprints        bool
	EstimateUnit        int
	Data                []byte
}

// MirrorColumn is a board column and its statuses, in order. A status is
// in at most one column of a board.
type MirrorColumn struct {
	Name     string
	Statuses []string
}

// MirrorSprint is a sprint as one board shows it. OriginBoardID is the
// board it was made on; State is MirrorSprintFuture through
// MirrorSprintClosed; a time is zero when the sprint has not reached it.
type MirrorSprint struct {
	ID, OriginBoardID, Name, Goal string
	State                         int
	StartAt, EndAt, CompletedAt   int64
}

// MirrorStructure is one read of a board's structure.
type MirrorStructure struct {
	Board   MirrorBoard
	Columns []MirrorColumn
	Sprints []MirrorSprint
	Catalog MirrorCatalog
}

func (c MirrorCatalog) check() error {
	projects := make(map[string]bool, len(c.Projects))
	for _, p := range c.Projects {
		if p.ID == "" || projects[p.ID] {
			return fmt.Errorf("%w: project %q is empty or repeated", ErrInvalidTrackerCache, p.ID)
		}
		projects[p.ID] = true
	}
	users := make(map[string]bool, len(c.Users))
	for _, u := range c.Users {
		if u.ID == "" || users[u.ID] {
			return fmt.Errorf("%w: user %q is empty or repeated", ErrInvalidTrackerCache, u.ID)
		}
		users[u.ID] = true
	}
	statuses := make(map[string]bool, len(c.Statuses))
	for _, st := range c.Statuses {
		if st.ID == "" || statuses[st.ID] || st.Group < MirrorGroupBacklog || st.Group > MirrorGroupCanceled {
			return fmt.Errorf("%w: status %q is empty, repeated or has group %d", ErrInvalidTrackerCache, st.ID, st.Group)
		}
		statuses[st.ID] = true
	}
	return nil
}

func (st MirrorStructure) check() error {
	b := st.Board
	switch {
	case b.ID == "":
		return fmt.Errorf("%w: a board needs its ID", ErrInvalidTrackerCache)
	case b.EstimateUnit < MirrorEstimateNone || b.EstimateUnit > MirrorEstimateTime:
		return fmt.Errorf("%w: board %s has estimate unit %d", ErrInvalidTrackerCache, b.ID, b.EstimateUnit)
	case len(st.Columns) > mirrorMaxColumns || len(st.Sprints) > mirrorMaxSprints:
		return fmt.Errorf("%w: board %s has %d columns and %d sprints, over %d and %d",
			ErrInvalidTrackerCache, b.ID, len(st.Columns), len(st.Sprints), mirrorMaxColumns, mirrorMaxSprints)
	}
	placed := map[string]bool{}
	for _, column := range st.Columns {
		for _, status := range column.Statuses {
			if status == "" || placed[status] {
				return fmt.Errorf("%w: board %s places status %q in two columns or names none", ErrInvalidTrackerCache, b.ID, status)
			}
			placed[status] = true
		}
	}
	if len(placed) > mirrorMaxColumnStatuses {
		return fmt.Errorf("%w: board %s places %d statuses, over %d", ErrInvalidTrackerCache, b.ID, len(placed), mirrorMaxColumnStatuses)
	}
	sprints := map[string]bool{}
	for _, sprint := range st.Sprints {
		if sprint.ID == "" || sprints[sprint.ID] || sprint.State < MirrorSprintFuture || sprint.State > MirrorSprintClosed ||
			sprint.StartAt < 0 || sprint.EndAt < 0 || sprint.CompletedAt < 0 {
			return fmt.Errorf("%w: board %s sprint %q is empty, repeated or out of range", ErrInvalidTrackerCache, b.ID, sprint.ID)
		}
		sprints[sprint.ID] = true
	}
	return st.Catalog.check()
}

// ReplaceMirrorStructure stores one read of a board's structure under
// rootID: the board, its columns with their statuses, its sprints (those
// not in st.Sprints are removed from the board) and the catalog. It
// reports whether anything changed.
func (s *Store) ReplaceMirrorStructure(rootID string, st MirrorStructure) (changed bool, err error) {
	if err := st.check(); err != nil {
		return false, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %s board %s structure", rootID, st.Board.ID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		var err error
		changed, err = replaceMirrorStructureTx(tx, rootID, st)
		return err
	})
	return changed, err
}

func replaceMirrorStructureTx(tx *sql.Tx, rootID string, st MirrorStructure) (bool, error) {
	b := st.Board
	changed, err := upsertMirrorCatalogTx(tx, rootID, st.Catalog)
	if err != nil {
		return false, err
	}
	mark := func(rowChanged bool, err error) error {
		changed = changed || rowChanged
		return err
	}
	if err := mark(execChangedTx(tx, `INSERT INTO tracker_cache_boards(root_id, board_id, project_id, name, plans_sprints, estimate_unit, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_id, board_id) DO UPDATE SET project_id = excluded.project_id, name = excluded.name,
		    plans_sprints = excluded.plans_sprints, estimate_unit = excluded.estimate_unit, data = excluded.data
		WHERE project_id IS NOT excluded.project_id OR name IS NOT excluded.name OR plans_sprints IS NOT excluded.plans_sprints
		   OR estimate_unit IS NOT excluded.estimate_unit OR data IS NOT excluded.data
		RETURNING board_id`, rootID, b.ID, b.ProjectID, b.Name, boolToInt(b.PlansSprints), b.EstimateUnit, mirrorData(b.Data))); err != nil {
		return false, fmt.Errorf("upsert board: %w", err)
	}
	var placed []string
	for position, column := range st.Columns {
		if err := mark(execChangedTx(tx, `INSERT INTO tracker_cache_board_columns(root_id, board_id, position, name) VALUES (?, ?, ?, ?)
			ON CONFLICT(root_id, board_id, position) DO UPDATE SET name = excluded.name WHERE name IS NOT excluded.name
			RETURNING position`, rootID, b.ID, position, column.Name)); err != nil {
			return false, fmt.Errorf("upsert column %d: %w", position, err)
		}
		for rank, status := range column.Statuses {
			placed = append(placed, status)
			if err := mark(execChangedTx(tx, `INSERT INTO tracker_cache_board_column_statuses(root_id, board_id, position, status_id, rank)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(root_id, board_id, status_id) DO UPDATE SET position = excluded.position, rank = excluded.rank
				WHERE position IS NOT excluded.position OR rank IS NOT excluded.rank
				RETURNING status_id`, rootID, b.ID, position, status, rank)); err != nil {
				return false, fmt.Errorf("upsert column %d status %s: %w", position, status, err)
			}
		}
	}
	placedJSON, err := encodeMirrorIDs(placed)
	if err != nil {
		return false, err
	}
	if err := mark(execChangedTx(tx, `DELETE FROM tracker_cache_board_column_statuses
		WHERE root_id = ? AND board_id = ? AND status_id NOT IN (SELECT value FROM json_each(?))
		RETURNING status_id`, rootID, b.ID, placedJSON)); err != nil {
		return false, fmt.Errorf("remove column statuses: %w", err)
	}
	if err := mark(execChangedTx(tx, `DELETE FROM tracker_cache_board_columns WHERE root_id = ? AND board_id = ? AND position >= ?
		RETURNING position`, rootID, b.ID, len(st.Columns))); err != nil {
		return false, fmt.Errorf("remove columns: %w", err)
	}
	sprintChanged, err := replaceMirrorSprintsTx(tx, rootID, b.ID, st.Sprints)
	return changed || sprintChanged, err
}

func replaceMirrorSprintsTx(tx *sql.Tx, rootID, boardID string, sprints []MirrorSprint) (bool, error) {
	changed := false
	kept := make([]string, 0, len(sprints))
	for position, sp := range sprints {
		kept = append(kept, sp.ID)
		rowChanged, err := execChangedTx(tx, `INSERT INTO tracker_cache_sprints(root_id, board_id, sprint_id, position, origin_board_id,
		        name, goal, state, start_at, end_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(root_id, board_id, sprint_id) DO UPDATE SET position = excluded.position,
			    origin_board_id = excluded.origin_board_id, name = excluded.name, goal = excluded.goal, state = excluded.state,
			    start_at = excluded.start_at, end_at = excluded.end_at, completed_at = excluded.completed_at
			WHERE position IS NOT excluded.position OR origin_board_id IS NOT excluded.origin_board_id
			   OR name IS NOT excluded.name OR goal IS NOT excluded.goal OR state IS NOT excluded.state
			   OR start_at IS NOT excluded.start_at OR end_at IS NOT excluded.end_at OR completed_at IS NOT excluded.completed_at
			RETURNING sprint_id`, rootID, boardID, sp.ID, position, sp.OriginBoardID, sp.Name, sp.Goal, sp.State,
			sp.StartAt, sp.EndAt, sp.CompletedAt)
		if err != nil {
			return false, fmt.Errorf("upsert sprint %s: %w", sp.ID, err)
		}
		changed = changed || rowChanged
	}
	keptJSON, err := encodeMirrorIDs(kept)
	if err != nil {
		return false, err
	}
	removed, err := execChangedTx(tx, `DELETE FROM tracker_cache_sprints
		WHERE root_id = ? AND board_id = ? AND sprint_id NOT IN (SELECT value FROM json_each(?))
		RETURNING sprint_id`, rootID, boardID, keptJSON)
	if err != nil {
		return false, fmt.Errorf("remove sprints: %w", err)
	}
	return changed || removed, nil
}

// upsertMirrorCatalogTx upserts what a read names and reports whether any
// row changed.
func upsertMirrorCatalogTx(tx *sql.Tx, rootID string, c MirrorCatalog) (bool, error) {
	changed := false
	for _, p := range c.Projects {
		rowChanged, err := execChangedTx(tx, `INSERT INTO tracker_cache_projects(root_id, project_id, project_key, name, data)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(root_id, project_id) DO UPDATE SET project_key = excluded.project_key, name = excluded.name, data = excluded.data
			WHERE project_key IS NOT excluded.project_key OR name IS NOT excluded.name OR data IS NOT excluded.data
			RETURNING project_id`, rootID, p.ID, p.Key, p.Name, mirrorData(p.Data))
		if err != nil {
			return false, fmt.Errorf("upsert project %s: %w", p.ID, err)
		}
		changed = changed || rowChanged
	}
	for _, u := range c.Users {
		rowChanged, err := execChangedTx(tx, `INSERT INTO tracker_cache_users(root_id, user_id, name, data) VALUES (?, ?, ?, ?)
			ON CONFLICT(root_id, user_id) DO UPDATE SET name = excluded.name, data = excluded.data
			WHERE name IS NOT excluded.name OR data IS NOT excluded.data
			RETURNING user_id`, rootID, u.ID, u.Name, mirrorData(u.Data))
		if err != nil {
			return false, fmt.Errorf("upsert user %s: %w", u.ID, err)
		}
		changed = changed || rowChanged
	}
	for _, st := range c.Statuses {
		rowChanged, err := execChangedTx(tx, `INSERT INTO tracker_cache_statuses(root_id, status_id, name, status_group) VALUES (?, ?, ?, ?)
			ON CONFLICT(root_id, status_id) DO UPDATE SET name = excluded.name, status_group = excluded.status_group
			WHERE name IS NOT excluded.name OR status_group IS NOT excluded.status_group
			RETURNING status_id`, rootID, st.ID, st.Name, st.Group)
		if err != nil {
			return false, fmt.Errorf("upsert status %s: %w", st.ID, err)
		}
		changed = changed || rowChanged
	}
	return changed, nil
}

// MirrorBoardRemoval is what DeleteMirrorBoard removed: whether the board
// was there, and the issues that left the mirror with it.
type MirrorBoardRemoval struct {
	Board  bool
	Issues []string
}

// DeleteMirrorBoard removes a board no project lists any more from rootID's
// mirror: its structure, sprints, rows and sync state, and every issue
// that leaves the mirror with it (pruneMirrorIssuesTx).
func (s *Store) DeleteMirrorBoard(rootID, boardID string) (removal MirrorBoardRemoval, err error) {
	action := fmt.Sprintf("store: delete tracker mirror %s board %s", rootID, boardID)
	err = s.mirrorWrite(rootID, action, func(tx *sql.Tx) error {
		issues, err := queryIDs(tx, `SELECT issue_id FROM tracker_cache_board_issues WHERE root_id = ? AND board_id = ?`, rootID, boardID)
		if err != nil {
			return fmt.Errorf("list rows: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM tracker_cache_sync WHERE root_id = ? AND board_id = ?`, rootID, boardID); err != nil {
			return fmt.Errorf("delete sync state: %w", err)
		}
		removal.Board, err = execChangedTx(tx, `DELETE FROM tracker_cache_boards WHERE root_id = ? AND board_id = ? RETURNING board_id`, rootID, boardID)
		if err != nil {
			return fmt.Errorf("delete board: %w", err)
		}
		removal.Issues, err = pruneMirrorIssuesTx(tx, rootID, issues)
		return err
	})
	return removal, err
}

// execChangedTx runs a write whose RETURNING yields a row for each row it
// changed, and reports whether it yielded any.
func execChangedTx(tx *sql.Tx, query string, args ...any) (bool, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return false, err
	}
	changed := false
	for rows.Next() {
		changed = true
	}
	return changed, errors.Join(rows.Err(), rows.Close())
}
