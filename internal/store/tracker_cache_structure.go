package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Mirror enumerations, aligned with the CHECKs of migration v162.
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

// MirrorGroupNames are the status groups' names on the wire, by number:
// what a status view and a thread row's ticket link say.
var MirrorGroupNames = [...]string{
	MirrorGroupBacklog: "backlog", MirrorGroupToDo: "todo", MirrorGroupInProgress: "in_progress",
	MirrorGroupDone: "done", MirrorGroupCanceled: "canceled",
}

// mirrorGroupNameSQL is the name of the status group expr (an SQL
// expression of its number) by MirrorGroupNames, ” for none.
func mirrorGroupNameSQL(expr string) string {
	var b strings.Builder
	b.WriteString("CASE " + expr)
	for n, name := range MirrorGroupNames {
		if name != "" {
			fmt.Fprintf(&b, " WHEN %d THEN '%s'", n, name)
		}
	}
	b.WriteString(" ELSE '' END")
	return b.String()
}

// mirrorGroupNumber is the number of the status group named name, 0 for
// "" (no group); false for a name MirrorGroupNames does not hold.
func mirrorGroupNumber(name string) (int, bool) {
	if name == "" {
		return 0, true
	}
	for n, known := range MirrorGroupNames {
		if known != "" && known == name {
			return n, true
		}
	}
	return 0, false
}

// Bounds of one board's structure and of one catalog. A structure's rows
// together stay within clearMirrorChunk, so a replace that removes all of
// them deletes at most a chunk.
const (
	mirrorMaxColumns        = 50
	mirrorMaxColumnStatuses = 200
	mirrorMaxSprints        = 250
	// mirrorCatalogChunk is the most users or statuses one catalog
	// statement takes; a catalog names any number (upsertMirrorCatalogTx).
	mirrorCatalogChunk = 500
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
// whether it plans in sprints, its estimate definition, and Data for the
// rest (its estimate choices). The estimate definition is the unit
// (MirrorEstimate*) and EstimateField, the tracker's opaque identity of
// what the estimates are of (tracker.Estimate.Field): a change of either
// resets the board's estimates (MirrorSync.EstimateResetAt).
type MirrorBoard struct {
	ID, ProjectID, Name string
	PlansSprints        bool
	EstimateUnit        int
	EstimateField       string
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
	ReadAt   MirrorRead
	Lifetime int64
	Complete bool
}

func (c MirrorCatalog) check() error {
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

func (st MirrorStructure) check(read mirrorReadTimes) error {
	b := st.Board
	switch {
	case b.ID == "" || read.at <= 0:
		return fmt.Errorf("%w: a board needs its ID and the time its read began", ErrInvalidTrackerCache)
	case b.EstimateUnit < MirrorEstimateNone || b.EstimateUnit > MirrorEstimateTime:
		return fmt.Errorf("%w: board %s has estimate unit %d", ErrInvalidTrackerCache, b.ID, b.EstimateUnit)
	case b.EstimateUnit == MirrorEstimateNone && b.EstimateField != "":
		return fmt.Errorf("%w: board %s has no estimate but names estimate field %q", ErrInvalidTrackerCache, b.ID, b.EstimateField)
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

// MirrorStructureWrite is what ReplaceMirrorStructure did, and the
// board's estimate reset after it (MirrorSync.EstimateResetAt): a caller
// walking the board's rows that captured another value starts its walk
// over from the sync state. Catalog reports a changed user or status,
// which the root's every board shows, as MirrorChanges.Catalog does; it
// is part of Changed. Threads are the threads whose row shows a status
// the catalog renamed or regrouped (MirrorCatalogWrite.Threads).
type MirrorStructureWrite struct {
	MirrorWrite
	EstimateResetAt int64
	Catalog         bool
	Threads         []string
}

// ReplaceMirrorStructure stores one read of a board's structure under
// rootID: the board, its columns with their statuses, its sprints (those
// not in st.Sprints are removed from the board) and the catalog. The board
// must be listed in st.Lifetime (ErrTrackerCacheUnlisted). A read that
// began before the latest one admitted for the structure is superseded and
// stores nothing; an identical one that began later moves the structure's
// read time alone. The structure is one transaction. The board's marks of
// lacking references follow it in transactions of at most
// clearMirrorChunk marks: the marks of the sprints it supplies go, and a
// complete read makes the pending marks no later than it unexplained. A
// call whose marks fail after the structure committed returns, with the
// error, what the structure changed: the caller tells it whatever it does
// about the error, and the next structure read admitted, identical or
// not, settles the marks again. Meanwhile a mark left pending makes the
// board due only once a later row page names it again: it was named
// before the stored structure's read, which looked for it.
func (s *Store) ReplaceMirrorStructure(rootID int64, st MirrorStructure) (result MirrorStructureWrite, err error) {
	read, err := st.ReadAt.in(rootID)
	if err != nil {
		return MirrorStructureWrite{}, err
	}
	if err := st.check(read); err != nil {
		return MirrorStructureWrite{}, err
	}
	action := fmt.Sprintf("store: replace tracker mirror %d board %s structure", rootID, st.Board.ID)
	var before mirrorBoard
	err = s.mirrorBoardWrite(rootID, st.Board.ID, st.Lifetime, action, false, func(tx *sql.Tx, board mirrorBoard) error {
		before, result.EstimateResetAt = board, board.estimateResetAt
		if read.at < board.readAt {
			result.Superseded = true
			return nil
		}
		var err error
		var reset bool
		var catalog MirrorCatalogWrite
		result.Changed, catalog, reset, err = replaceMirrorStructureTx(tx, rootID, st, read, board)
		result.Catalog, result.Threads = catalog.Changed, catalog.Threads
		if reset {
			result.EstimateResetAt = read.at
		}
		return err
	})
	if err != nil {
		return MirrorStructureWrite{}, err
	}
	if result.Superseded {
		return result, nil
	}
	return result, s.settleMirrorMarks(rootID, st, read, before, action)
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

// replaceMirrorStructureTx writes an admitted structure and reports
// whether it changed, whether its catalog changed, and whether it reset
// the board's estimate definition.
func replaceMirrorStructureTx(tx *sql.Tx, rootID int64, st MirrorStructure, read mirrorReadTimes, board mirrorBoard) (changed bool, catalog MirrorCatalogWrite, reset bool, err error) {
	b := st.Board
	if catalog, err = upsertMirrorCatalogTx(tx, rootID, st.Catalog, read.at); err != nil {
		return false, MirrorCatalogWrite{}, false, err
	}
	changed = catalog.Changed
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
			return false, MirrorCatalogWrite{}, false, err
		}
	}
	boardChanged, reset, err := writeMirrorBoardTx(tx, rootID, st, read.at)
	if err != nil {
		return false, MirrorCatalogWrite{}, false, err
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
			return false, MirrorCatalogWrite{}, false, fmt.Errorf("%s: %w", step.what, err)
		}
		changed = changed || stepChanged
	}
	// The read is the structure's latest, changed or not, and covers the
	// pending marks no later than it: a narrow write of the board row,
	// which no index or trigger reads, with the wall time the structure's
	// interval is measured from (MirrorSync.StructureWall).
	if read.at > board.readAt {
		if _, err := tx.Exec(`UPDATE tracker_cache_boards SET read_at = ?3, structure_wall = ?4 WHERE root_id = ?1 AND board_id = ?2`,
			rootID, b.ID, read.at, read.wall); err != nil {
			return false, MirrorCatalogWrite{}, false, fmt.Errorf("record the structure's read: %w", err)
		}
	}
	return changed, catalog, reset, nil
}

// The mark statements: ?1 root, ?2 board.
var (
	// mirrorMarksSuppliedSQL removes the marks of the sprints ?3 (a JSON
	// list of mirrorSprintRow) supplies, probing each by key.
	mirrorMarksSuppliedSQL = `DELETE FROM tracker_cache_board_unexplained
		WHERE root_id = ?1 AND board_id = ?2 AND kind = 3 AND ref_id IN (SELECT c.value ->> 'i' FROM json_each(?3) c)`
	// mirrorMarksExplainSQL makes unexplained a chunk of the pending marks
	// named by reads no later than the complete structure read ?3, in the
	// pending index.
	mirrorMarksExplainSQL = `UPDATE tracker_cache_board_unexplained SET named_at = 0
		WHERE root_id = ?1 AND board_id = ?2 AND (kind, ref_id) IN (
		  SELECT kind, ref_id FROM tracker_cache_board_unexplained
		   WHERE root_id = ?1 AND board_id = ?2 AND named_at > 0 AND named_at <= ?3
		   LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)
		RETURNING kind`
)

// settleMirrorMarks keeps a board's marks after a structure write it
// admitted, given the board as that write read it: when the board held
// marks, one transaction removes those of the sprints st supplies (at most
// mirrorMaxSprints); when st is complete and the board held pending marks,
// transactions of clearMirrorChunk make unexplained those named no later
// than st, so a row naming them no longer makes the board due. A board
// with no marks runs nothing. Each transaction checks the lifetime again.
func (s *Store) settleMirrorMarks(rootID int64, st MirrorStructure, read mirrorReadTimes, board mirrorBoard, action string) error {
	if board.marked && len(st.Sprints) > 0 {
		sprints := make([]mirrorSprintRow, len(st.Sprints))
		for i, sp := range st.Sprints {
			sprints[i].ID = sp.ID
		}
		list, err := encodeMirrorJSON(sprints)
		if err != nil {
			return err
		}
		err = s.mirrorBoardWrite(rootID, st.Board.ID, st.Lifetime, action, false, func(tx *sql.Tx, _ mirrorBoard) error {
			if _, err := tx.Exec(mirrorMarksSuppliedSQL, rootID, st.Board.ID, list); err != nil {
				return fmt.Errorf("remove the marks of supplied sprints: %w", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if !st.Complete || board.pendingAt == 0 {
		return nil
	}
	for {
		var explained []string
		err := s.mirrorBoardWrite(rootID, st.Board.ID, st.Lifetime, action, false, func(tx *sql.Tx, _ mirrorBoard) error {
			var err error
			if explained, err = queryIDs(tx, mirrorMarksExplainSQL, rootID, st.Board.ID, read.at); err != nil {
				return fmt.Errorf("mark unexplained references: %w", err)
			}
			return nil
		})
		if err != nil || len(explained) < clearMirrorChunk {
			return err
		}
	}
}

// mirrorEstimateChangedSQL holds, in mirrorBoardWriteRowSQL, when the
// written estimate definition (unit ?6, field ?9) is not the stored one.
const mirrorEstimateChangedSQL = `(estimate_unit IS NOT ?6 OR estimate_field IS NOT ?9)`

// mirrorBoardWriteRowSQL writes a board's own fields. A changed estimate
// definition on a board whose structure is stored records the read as the
// board's estimate reset (estimate_reset_at), which RETURNING reports; any
// changed definition changes the estimate totals (totals_revision).
const mirrorBoardWriteRowSQL = `UPDATE tracker_cache_boards SET project_id = ?3, name = ?4, plans_sprints = ?5, estimate_unit = ?6,
	    estimate_field = ?9, data = ?7,
	    estimate_reset_at = iif(read_at > 0 AND ` + mirrorEstimateChangedSQL + `, ?8, estimate_reset_at),
	    totals_revision = totals_revision + ` + mirrorEstimateChangedSQL + `
	WHERE root_id = ?1 AND board_id = ?2 AND (read_at = 0 OR project_id IS NOT ?3 OR name IS NOT ?4
	   OR plans_sprints IS NOT ?5 OR ` + mirrorEstimateChangedSQL + ` OR data IS NOT ?7)
	RETURNING estimate_reset_at = ?8`

// writeMirrorBoardTx writes the board's own fields and reports whether
// they changed and whether the estimate definition changed. A changed
// definition also forgets the board's cursor, and a walk begun under the
// old one stores none (MirrorRowPage.EstimateResetAt), so the next walk
// reads every row again in the new definition: the rows read no later
// than it show their estimates as unknown until then.
func writeMirrorBoardTx(tx *sql.Tx, rootID int64, st MirrorStructure, readAt int64) (changed, reset bool, err error) {
	b := st.Board
	err = tx.QueryRow(mirrorBoardWriteRowSQL, rootID, b.ID, b.ProjectID, b.Name, boolToInt(b.PlansSprints), b.EstimateUnit,
		mirrorData(b.Data), readAt, b.EstimateField).Scan(&reset)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("write board: %w", err)
	case !reset:
		return true, false, nil
	}
	if _, err := tx.Exec(`UPDATE tracker_cache_sync SET cursor = NULL WHERE root_id = ?1 AND board_id = ?2`, rootID, b.ID); err != nil {
		return false, false, fmt.Errorf("forget the cursor: %w", err)
	}
	return true, true, nil
}
