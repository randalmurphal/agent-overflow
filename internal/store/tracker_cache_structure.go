package store

import (
	"database/sql"
	"encoding/hex"
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
// is written only by a read that began no earlier than the latest one
// admitted for it, identical or not; an earlier one is skipped without a
// report.
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
// read began, and Lifetime the board's lifetime its caller captured before
// the read (MirrorSync.Lifetime); GetMirrorBoard leaves ReadAt and the
// catalog empty. Complete reports that the tracker said the read listed
// everything the board's rows can name: a status, user or sprint they name
// that it did not supply is then marked unexplained, and no longer makes
// the board due a structure read (MirrorSync.StructureDueAt).
type MirrorStructure struct {
	Board    MirrorBoard
	Columns  []MirrorColumn
	Sprints  []MirrorSprint
	Catalog  MirrorCatalog
	ReadAt   int64
	Lifetime int64
	Complete bool
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
// must be listed in st.Lifetime (ErrTrackerCacheUnlisted). A read that
// began before the latest one admitted for the structure is superseded and
// stores nothing; an identical one that began later moves the structure's
// read time alone.
func (s *Store) ReplaceMirrorStructure(rootID int64, st MirrorStructure) (result MirrorWrite, err error) {
	if err := st.check(); err != nil {
		return MirrorWrite{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %d board %s structure", rootID, st.Board.ID)
	err = s.mirrorBoardWrite(rootID, st.Board.ID, st.Lifetime, action, false, func(tx *sql.Tx, board mirrorBoard) error {
		if st.ReadAt < board.readAt {
			result.Superseded = true
			return nil
		}
		var err error
		result.Changed, err = replaceMirrorStructureTx(tx, rootID, st, board)
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

func replaceMirrorStructureTx(tx *sql.Tx, rootID int64, st MirrorStructure, board mirrorBoard) (bool, error) {
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
	boardChanged, err := writeMirrorBoardTx(tx, rootID, st)
	if err != nil {
		return false, err
	}
	changed = changed || boardChanged
	steps := []struct {
		what, query string
		args        []any
	}{
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
	// The read is the structure's latest, changed or not, and covers a due
	// state no later than it: a narrow write of the board row, which no
	// index or trigger reads.
	clearsDue := board.structureDueAt > 0 && st.ReadAt >= board.structureDueAt
	if st.ReadAt > board.readAt || clearsDue {
		if _, err := tx.Exec(`UPDATE tracker_cache_boards SET read_at = max(read_at, ?3),
			structure_due_at = iif(structure_due_at <= ?3, 0, structure_due_at) WHERE root_id = ?1 AND board_id = ?2`,
			rootID, b.ID, st.ReadAt); err != nil {
			return false, fmt.Errorf("record the structure's read: %w", err)
		}
	}
	return changed, markMirrorUnexplainedTx(tx, rootID, b.ID, board.marked, clearsDue && st.Complete)
}

// The unexplained-reference statements: ?1 root, ?2 board.
const (
	// mirrorUnexplainedSuppliedSQL removes the marks of references the
	// mirror now holds.
	mirrorUnexplainedSuppliedSQL = `DELETE FROM tracker_cache_board_unexplained WHERE root_id = ?1 AND board_id = ?2 AND CASE kind
	  WHEN 1 THEN EXISTS (SELECT 1 FROM tracker_cache_statuses st WHERE st.root_id = ?1 AND st.status_id = ref_id)
	  WHEN 2 THEN EXISTS (SELECT 1 FROM tracker_cache_users us WHERE us.root_id = ?1 AND us.user_id = ref_id)
	  ELSE EXISTS (SELECT 1 FROM tracker_cache_sprints sp WHERE sp.root_id = ?1 AND sp.board_id = ?2 AND sp.sprint_id = ref_id) END`
	// mirrorUnexplainedMarkSQL marks every reference the board's rows name
	// that the mirror lacks, walking the board's rows once a kind.
	mirrorUnexplainedMarkSQL = `INSERT INTO tracker_cache_board_unexplained(root_id, board_id, kind, ref_id)
	SELECT ?1, ?2, 1, i.status_id FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE b.root_id = ?1 AND b.board_id = ?2 AND i.status_id <> ''
	   AND NOT EXISTS (SELECT 1 FROM tracker_cache_statuses st WHERE st.root_id = ?1 AND st.status_id = i.status_id)
	UNION ALL
	SELECT ?1, ?2, 2, i.assignee_id FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issues i ON i.root_id = b.root_id AND i.issue_id = b.issue_id
	 WHERE b.root_id = ?1 AND b.board_id = ?2 AND i.assignee_id <> ''
	   AND NOT EXISTS (SELECT 1 FROM tracker_cache_users us WHERE us.root_id = ?1 AND us.user_id = i.assignee_id)
	UNION ALL
	SELECT ?1, ?2, 3, m.sprint_id FROM tracker_cache_board_issues b
	  CROSS JOIN tracker_cache_issue_sprints m ON m.root_id = b.root_id AND m.issue_id = b.issue_id
	 WHERE b.root_id = ?1 AND b.board_id = ?2
	   AND NOT EXISTS (SELECT 1 FROM tracker_cache_sprints sp WHERE sp.root_id = ?1 AND sp.board_id = ?2 AND sp.sprint_id = m.sprint_id)
	ON CONFLICT DO NOTHING`
)

// markMirrorUnexplainedTx keeps a board's unexplained references after a
// structure write: a board with marks loses those the mirror now holds,
// and a complete read that clears the board's due state replaces them with
// what the board's rows name that the mirror lacks. A board with no marks
// and no due state runs nothing.
func markMirrorUnexplainedTx(tx *sql.Tx, rootID int64, boardID string, marked, mark bool) error {
	switch {
	case mark:
		if _, err := tx.Exec(`DELETE FROM tracker_cache_board_unexplained WHERE root_id = ?1 AND board_id = ?2`, rootID, boardID); err != nil {
			return fmt.Errorf("clear unexplained references: %w", err)
		}
		if _, err := tx.Exec(mirrorUnexplainedMarkSQL, rootID, boardID); err != nil {
			return fmt.Errorf("mark unexplained references: %w", err)
		}
	case marked:
		if _, err := tx.Exec(mirrorUnexplainedSuppliedSQL, rootID, boardID); err != nil {
			return fmt.Errorf("remove supplied references: %w", err)
		}
	}
	return nil
}

// mirrorBoardWriteRowSQL writes a board's own fields. A changed estimate
// unit on a board whose structure is stored records the read as the
// board's estimate reset (estimate_reset_at), which RETURNING reports.
const mirrorBoardWriteRowSQL = `UPDATE tracker_cache_boards SET project_id = ?3, name = ?4, plans_sprints = ?5, estimate_unit = ?6, data = ?7,
	    estimate_reset_at = iif(read_at > 0 AND estimate_unit IS NOT ?6, ?8, estimate_reset_at)
	WHERE root_id = ?1 AND board_id = ?2 AND (read_at = 0 OR project_id IS NOT ?3 OR name IS NOT ?4
	   OR plans_sprints IS NOT ?5 OR estimate_unit IS NOT ?6 OR data IS NOT ?7)
	RETURNING estimate_reset_at = ?8`

// writeMirrorBoardTx writes the board's own fields and reports whether
// they changed. A changed estimate definition also forgets the board's
// cursor and fences every cursor read no later than the structure, so the
// next walk reads every row again in the new definition: the rows read no
// later than it show their estimates as unknown until then.
func writeMirrorBoardTx(tx *sql.Tx, rootID int64, st MirrorStructure) (changed bool, err error) {
	b := st.Board
	var reset bool
	err = tx.QueryRow(mirrorBoardWriteRowSQL, rootID, b.ID, b.ProjectID, b.Name, boolToInt(b.PlansSprints), b.EstimateUnit,
		mirrorData(b.Data), st.ReadAt).Scan(&reset)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("write board: %w", err)
	case !reset:
		return true, nil
	}
	// A cursor is admitted from a read no earlier than cursor_read_at; one
	// past the structure's read refuses those of its own millisecond too.
	if _, err := tx.Exec(`UPDATE tracker_cache_sync SET cursor = NULL, cursor_read_at = max(cursor_read_at, ?3 + 1)
		WHERE root_id = ?1 AND board_id = ?2`, rootID, b.ID, st.ReadAt); err != nil {
		return false, fmt.Errorf("forget the cursor: %w", err)
	}
	return true, nil
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
// read began. A row is written by a read that began later than its
// read_at, which moves read_at whether or not the content differs, or by
// other content from the same read; changed_at moves only with the
// content, so RETURNING reports a change. A status's done_changed_at moves
// when it enters or leaves the done and canceled groups, the one change of
// a status that moves rows between sections.
const (
	mirrorUsersUpsertSQL = `INSERT INTO tracker_cache_users(root_id, user_id, name, data, read_at, changed_at)
		SELECT ?1, u.value ->> 'i', u.value ->> 'n', unhex(u.value ->> 'd'), ?3, ?3 FROM json_each(?2) u WHERE true
		ON CONFLICT(root_id, user_id) DO UPDATE SET name = excluded.name, data = excluded.data, read_at = excluded.read_at,
		    changed_at = iif(name IS NOT excluded.name OR data IS NOT excluded.data, excluded.read_at, changed_at)
		WHERE excluded.read_at > read_at OR (excluded.read_at = read_at AND (name IS NOT excluded.name OR data IS NOT excluded.data))
		RETURNING user_id, changed_at = ?3`
	mirrorStatusesUpsertSQL = `INSERT INTO tracker_cache_statuses(root_id, status_id, name, status_group, read_at, changed_at, done_changed_at)
		SELECT ?1, st.value ->> 'i', st.value ->> 'n', st.value ->> 'g', ?3, ?3, iif(st.value ->> 'g' IN (4, 5), ?3, 0)
		  FROM json_each(?2) st WHERE true
		ON CONFLICT(root_id, status_id) DO UPDATE SET name = excluded.name, status_group = excluded.status_group,
		    read_at = excluded.read_at,
		    changed_at = iif(name IS NOT excluded.name OR status_group IS NOT excluded.status_group, excluded.read_at, changed_at),
		    done_changed_at = iif((status_group IN (4, 5)) IS NOT (excluded.status_group IN (4, 5)), excluded.read_at, done_changed_at)
		WHERE excluded.read_at > read_at
		   OR (excluded.read_at = read_at AND (name IS NOT excluded.name OR status_group IS NOT excluded.status_group))
		RETURNING status_id, changed_at = ?3, done_changed_at = ?3`
	// mirrorStatusGroupRevisionSQL advances, once each, the revision of the
	// boards with a row whose issue has a status in ?2. A status moves
	// between groups rarely, so this walks the root's issues rather than
	// keep an index by status that every card write would maintain.
	mirrorStatusGroupRevisionSQL = `UPDATE tracker_cache_boards SET revision = revision + 1 WHERE root_id = ?1 AND board_id IN (
		SELECT b.board_id FROM tracker_cache_issues i
		  CROSS JOIN tracker_cache_board_issues b ON b.root_id = i.root_id AND b.issue_id = i.issue_id
		 WHERE i.root_id = ?1 AND i.status_id IN (SELECT value FROM json_each(?2)))`
)

// upsertMirrorCatalogTx upserts what a read that began at readAt names and
// reports whether any content changed: one statement per kind it names,
// and one more when a status entered or left the done groups, which
// advances the revision of the boards whose rows have it.
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
		err = scanMirrorRows(tx, mirrorUsersUpsertSQL, []any{rootID, encoded, readAt}, func(r rowScanner) error {
			var id string
			var userChanged bool
			err := r.Scan(&id, &userChanged)
			changed = changed || userChanged
			return err
		})
		if err != nil {
			return false, fmt.Errorf("upsert users: %w", err)
		}
	}
	if len(c.Statuses) == 0 {
		return changed, nil
	}
	statuses := make([]mirrorStatusRow, len(c.Statuses))
	for i, st := range c.Statuses {
		statuses[i] = mirrorStatusRow(st)
	}
	encoded, err := encodeMirrorJSON(statuses)
	if err != nil {
		return false, err
	}
	var moved []string
	err = scanMirrorRows(tx, mirrorStatusesUpsertSQL, []any{rootID, encoded, readAt}, func(r rowScanner) error {
		var id string
		var statusChanged, doneChanged bool
		err := r.Scan(&id, &statusChanged, &doneChanged)
		changed = changed || statusChanged
		if doneChanged {
			moved = append(moved, id)
		}
		return err
	})
	if err != nil {
		return false, fmt.Errorf("upsert statuses: %w", err)
	}
	if len(moved) == 0 {
		return changed, nil
	}
	list, err := encodeMirrorIDs(moved)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(mirrorStatusGroupRevisionSQL, rootID, list); err != nil {
		return false, fmt.Errorf("advance the revision of boards with a regrouped status: %w", err)
	}
	return changed, nil
}
