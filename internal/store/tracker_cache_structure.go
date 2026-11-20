package store

import (
	"database/sql"
	"encoding/hex"
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

// Bounds of one board's structure and of one catalog. A structure's rows
// together stay within clearMirrorChunk, so a replace that removes all of
// them deletes at most a chunk.
const (
	mirrorMaxColumns        = 50
	mirrorMaxColumnStatuses = 200
	mirrorMaxSprints        = 250
	mirrorMaxCatalog        = 500
)

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

// MirrorCatalog is the users and statuses a read names. They belong to the
// root, not to a board: a write upserts them and never removes one. Each
// is replaced only by a read that began no earlier than the one that
// stored it; an earlier one is skipped without a report.
type MirrorCatalog struct {
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

// MirrorStructure is one read of a board's structure. ReadAt is when the
// read began; GetMirrorBoard leaves it and the catalog empty.
type MirrorStructure struct {
	Board   MirrorBoard
	Columns []MirrorColumn
	Sprints []MirrorSprint
	Catalog MirrorCatalog
	ReadAt  int64
}

func (c MirrorCatalog) check() error {
	if len(c.Users) > mirrorMaxCatalog || len(c.Statuses) > mirrorMaxCatalog {
		return fmt.Errorf("%w: a catalog names %d users and %d statuses, over %d", ErrInvalidTrackerCache, len(c.Users), len(c.Statuses), mirrorMaxCatalog)
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
	case b.ID == "" || st.ReadAt <= 0:
		return fmt.Errorf("%w: a board needs its ID and the time its read began", ErrInvalidTrackerCache)
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
// not in st.Sprints are removed from the board) and the catalog. The board
// must be listed (ErrTrackerCacheUnlisted). A read that began before the
// one that stored the board's structure is superseded and stores nothing.
func (s *Store) ReplaceMirrorStructure(rootID int64, st MirrorStructure) (result MirrorWrite, err error) {
	if err := st.check(); err != nil {
		return MirrorWrite{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %d board %s structure", rootID, st.Board.ID)
	err = s.mirrorBoardWrite(rootID, st.Board.ID, action, false, func(tx *sql.Tx, board mirrorBoard) error {
		if board.stored && st.ReadAt < board.readAt {
			result.Superseded = true
			return nil
		}
		var err error
		result.Changed, err = replaceMirrorStructureTx(tx, rootID, st)
		return err
	})
	if err != nil {
		return MirrorWrite{}, err
	}
	return result, nil
}

type mirrorStatusPlace struct {
	Status   string `json:"s"`
	Position int    `json:"p"`
	Rank     int    `json:"r"`
}

type mirrorSprintRow struct {
	ID          string `json:"i"`
	Origin      string `json:"o"`
	Name        string `json:"n"`
	Goal        string `json:"g"`
	State       int    `json:"t"`
	StartAt     int64  `json:"s"`
	EndAt       int64  `json:"e"`
	CompletedAt int64  `json:"c"`
}

// The structure's set statements: ?1 root, ?2 board, ?3 the JSON list.
const (
	mirrorColumnsUpsertSQL = `INSERT INTO tracker_cache_board_columns(root_id, board_id, position, name)
		SELECT ?1, ?2, c.key, c.value FROM json_each(?3) c WHERE true
		ON CONFLICT(root_id, board_id, position) DO UPDATE SET name = excluded.name WHERE name IS NOT excluded.name
		RETURNING position`
	mirrorColumnStatusesUpsertSQL = `INSERT INTO tracker_cache_board_column_statuses(root_id, board_id, position, status_id, rank)
		SELECT ?1, ?2, c.value ->> 'p', c.value ->> 's', c.value ->> 'r' FROM json_each(?3) c WHERE true
		ON CONFLICT(root_id, board_id, status_id) DO UPDATE SET position = excluded.position, rank = excluded.rank
		WHERE position IS NOT excluded.position OR rank IS NOT excluded.rank
		RETURNING status_id`
	mirrorColumnStatusesRemoveSQL = `DELETE FROM tracker_cache_board_column_statuses
		WHERE root_id = ?1 AND board_id = ?2 AND status_id NOT IN (SELECT c.value ->> 's' FROM json_each(?3) c)
		RETURNING status_id`
	mirrorSprintsUpsertSQL = `INSERT INTO tracker_cache_sprints(root_id, board_id, sprint_id, position, origin_board_id,
		        name, goal, state, start_at, end_at, completed_at)
		SELECT ?1, ?2, c.value ->> 'i', c.key, c.value ->> 'o', c.value ->> 'n', c.value ->> 'g', c.value ->> 't',
		       c.value ->> 's', c.value ->> 'e', c.value ->> 'c' FROM json_each(?3) c WHERE true
		ON CONFLICT(root_id, board_id, sprint_id) DO UPDATE SET position = excluded.position,
		    origin_board_id = excluded.origin_board_id, name = excluded.name, goal = excluded.goal, state = excluded.state,
		    start_at = excluded.start_at, end_at = excluded.end_at, completed_at = excluded.completed_at
		WHERE position IS NOT excluded.position OR origin_board_id IS NOT excluded.origin_board_id
		   OR name IS NOT excluded.name OR goal IS NOT excluded.goal OR state IS NOT excluded.state
		   OR start_at IS NOT excluded.start_at OR end_at IS NOT excluded.end_at OR completed_at IS NOT excluded.completed_at
		RETURNING sprint_id`
	mirrorSprintsRemoveSQL = `DELETE FROM tracker_cache_sprints
		WHERE root_id = ?1 AND board_id = ?2 AND sprint_id NOT IN (SELECT c.value ->> 'i' FROM json_each(?3) c)
		RETURNING sprint_id`
)

func replaceMirrorStructureTx(tx *sql.Tx, rootID int64, st MirrorStructure) (bool, error) {
	b := st.Board
	changed, err := upsertMirrorCatalogTx(tx, rootID, st.Catalog, st.ReadAt)
	if err != nil {
		return false, err
	}
	names := make([]string, len(st.Columns))
	places := []mirrorStatusPlace{}
	for position, column := range st.Columns {
		names[position] = column.Name
		for rank, status := range column.Statuses {
			places = append(places, mirrorStatusPlace{Status: status, Position: position, Rank: rank})
		}
	}
	sprints := make([]mirrorSprintRow, len(st.Sprints))
	for i, sp := range st.Sprints {
		sprints[i] = mirrorSprintRow{ID: sp.ID, Origin: sp.OriginBoardID, Name: sp.Name, Goal: sp.Goal, State: sp.State,
			StartAt: sp.StartAt, EndAt: sp.EndAt, CompletedAt: sp.CompletedAt}
	}
	lists := make([]string, 3)
	for i, v := range []any{names, places, sprints} {
		if lists[i], err = encodeMirrorJSON(v); err != nil {
			return false, err
		}
	}
	steps := []struct {
		what, query string
		args        []any
	}{
		{"upsert board", `INSERT INTO tracker_cache_boards(root_id, board_id, project_id, name, plans_sprints, estimate_unit, data, read_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(root_id, board_id) DO UPDATE SET project_id = excluded.project_id, name = excluded.name,
			    plans_sprints = excluded.plans_sprints, estimate_unit = excluded.estimate_unit, data = excluded.data
			WHERE project_id IS NOT excluded.project_id OR name IS NOT excluded.name OR plans_sprints IS NOT excluded.plans_sprints
			   OR estimate_unit IS NOT excluded.estimate_unit OR data IS NOT excluded.data
			RETURNING board_id`, []any{rootID, b.ID, b.ProjectID, b.Name, boolToInt(b.PlansSprints), b.EstimateUnit, mirrorData(b.Data), st.ReadAt}},
		{"upsert columns", mirrorColumnsUpsertSQL, []any{rootID, b.ID, lists[0]}},
		{"upsert column statuses", mirrorColumnStatusesUpsertSQL, []any{rootID, b.ID, lists[1]}},
		{"remove column statuses", mirrorColumnStatusesRemoveSQL, []any{rootID, b.ID, lists[1]}},
		{"remove columns", `DELETE FROM tracker_cache_board_columns WHERE root_id = ? AND board_id = ? AND position >= ?
			RETURNING position`, []any{rootID, b.ID, len(st.Columns)}},
		{"upsert sprints", mirrorSprintsUpsertSQL, []any{rootID, b.ID, lists[2]}},
		{"remove sprints", mirrorSprintsRemoveSQL, []any{rootID, b.ID, lists[2]}},
	}
	for _, step := range steps {
		stepChanged, err := execChangedTx(tx, step.query, step.args...)
		if err != nil {
			return false, fmt.Errorf("%s: %w", step.what, err)
		}
		changed = changed || stepChanged
	}
	if changed {
		if _, err := tx.Exec(`UPDATE tracker_cache_boards SET read_at = ?3 WHERE root_id = ?1 AND board_id = ?2 AND read_at < ?3`,
			rootID, b.ID, st.ReadAt); err != nil {
			return false, fmt.Errorf("record the structure's read: %w", err)
		}
	}
	return changed, nil
}

type mirrorUserRow struct {
	ID   string `json:"i"`
	Name string `json:"n"`
	Data string `json:"d"`
}

type mirrorStatusRow struct {
	ID    string `json:"i"`
	Name  string `json:"n"`
	Group int    `json:"g"`
}

// The catalog's set statements: ?1 root, ?2 the JSON list, ?3 when the
// read began. A row changes only for other content from a read that began
// no earlier, so its read_at moves with its content.
const (
	mirrorUsersUpsertSQL = `INSERT INTO tracker_cache_users(root_id, user_id, name, data, read_at)
		SELECT ?1, u.value ->> 'i', u.value ->> 'n', unhex(u.value ->> 'd'), ?3 FROM json_each(?2) u WHERE true
		ON CONFLICT(root_id, user_id) DO UPDATE SET name = excluded.name, data = excluded.data, read_at = excluded.read_at
		WHERE excluded.read_at >= read_at AND (name IS NOT excluded.name OR data IS NOT excluded.data)
		RETURNING user_id`
	mirrorStatusesUpsertSQL = `INSERT INTO tracker_cache_statuses(root_id, status_id, name, status_group, read_at)
		SELECT ?1, st.value ->> 'i', st.value ->> 'n', st.value ->> 'g', ?3 FROM json_each(?2) st WHERE true
		ON CONFLICT(root_id, status_id) DO UPDATE SET name = excluded.name, status_group = excluded.status_group,
		    read_at = excluded.read_at
		WHERE excluded.read_at >= read_at AND (name IS NOT excluded.name OR status_group IS NOT excluded.status_group)
		RETURNING status_id`
)

// upsertMirrorCatalogTx upserts what a read that began at readAt names and
// reports whether any row changed: one statement per kind it names.
func upsertMirrorCatalogTx(tx *sql.Tx, rootID int64, c MirrorCatalog, readAt int64) (bool, error) {
	changed := false
	if len(c.Users) > 0 {
		users := make([]mirrorUserRow, len(c.Users))
		for i, u := range c.Users {
			users[i] = mirrorUserRow{ID: u.ID, Name: u.Name, Data: hex.EncodeToString(u.Data)}
		}
		encoded, err := encodeMirrorJSON(users)
		if err != nil {
			return false, err
		}
		if changed, err = execChangedTx(tx, mirrorUsersUpsertSQL, rootID, encoded, readAt); err != nil {
			return false, fmt.Errorf("upsert users: %w", err)
		}
	}
	if len(c.Statuses) > 0 {
		statuses := make([]mirrorStatusRow, len(c.Statuses))
		for i, st := range c.Statuses {
			statuses[i] = mirrorStatusRow(st)
		}
		encoded, err := encodeMirrorJSON(statuses)
		if err != nil {
			return false, err
		}
		statusChanged, err := execChangedTx(tx, mirrorStatusesUpsertSQL, rootID, encoded, readAt)
		if err != nil {
			return false, fmt.Errorf("upsert statuses: %w", err)
		}
		changed = changed || statusChanged
	}
	return changed, nil
}
