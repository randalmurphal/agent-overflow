package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// ErrInvalidTrackerBoards is returned by ReplaceProjectTrackerBoards for a
// list the table refuses: an empty connection or board ID, a board listed
// twice, more than one board that opens first, a name or tracker project
// that is not text or is longer than maxTrackerBoardTextRunes, or a
// connection that does not exist or has no boards (Tempo).
var ErrInvalidTrackerBoards = errors.New("store: invalid tracker board list")

// ErrTrackerBoardsChanged is returned by ReplaceProjectTrackerBoards when
// the list the caller built its change from is not the stored one: another
// save, or a connection's removal, changed the list after the caller read
// it.
var ErrTrackerBoardsChanged = errors.New("store: the project's tracker boards changed after they were read")

// TrackerBoardRef names one board: the tracker's own board ID within a
// connection.
type TrackerBoardRef struct {
	ConnectionID string
	BoardID      string
}

// ProjectTrackerBoard is one board an app project lists (migration v162).
// A project's list is ordered as its board picker shows it, and at most
// one board opens first. Name and TrackerProject are what the picker
// showed when the board was chosen (v166).
type ProjectTrackerBoard struct {
	ConnectionID   string
	BoardID        string
	OpensFirst     bool
	Name           string
	TrackerProject string
}

// HeldTrackerBoard is one board of the list a caller read and built its
// change from: what a project's list holds, less the names it stored.
type HeldTrackerBoard struct {
	ConnectionID string
	BoardID      string
	OpensFirst   bool
}

// heldTrackerBoards is boards as a caller holds them.
func heldTrackerBoards(boards []ProjectTrackerBoard) []HeldTrackerBoard {
	out := make([]HeldTrackerBoard, len(boards))
	for i, b := range boards {
		out[i] = HeldTrackerBoard{ConnectionID: b.ConnectionID, BoardID: b.BoardID, OpensFirst: b.OpensFirst}
	}
	return out
}

// maxTrackerBoardTextRunes bounds a listing's name and tracker project,
// as the columns' CHECKs do (migration v166).
const maxTrackerBoardTextRunes = 255

// trackerBoardRow is one board of ReplaceProjectTrackerBoards' JSON list;
// its index in the list is its position.
type trackerBoardRow struct {
	ConnectionID   string `json:"c"`
	BoardID        string `json:"b"`
	OpensFirst     int    `json:"f"`
	Name           string `json:"n,omitempty"`
	TrackerProject string `json:"p,omitempty"`
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
		for _, text := range [...]struct{ what, value string }{{"name", board.Name}, {"tracker project", board.TrackerProject}} {
			if !utf8.ValidString(text.value) || utf8.RuneCountInString(text.value) > maxTrackerBoardTextRunes {
				return fmt.Errorf("%w: the %s of board %s is not text of at most %d characters", ErrInvalidTrackerBoards, text.what, ref.BoardID, maxTrackerBoardTextRunes)
			}
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

// TrackerBoardListing is what a replace of a project's board list did to
// the boards' stored copies, decided in its transaction. Unlisted are the
// boards whose last listing it removed: each is retired in its mirror
// (trg_project_tracker_boards_retire). Relisted are the boards it added
// whose stored copy is retired and not yet cleared, which open only once
// ReadyTrackerBoards gives them their next lifetime.
type TrackerBoardListing struct {
	Unlisted, Relisted []TrackerBoardRef
}

// ReplaceProjectTrackerBoards makes boards, in order, the whole list of
// boards projectID lists, and reports whether the list changed and what
// that did to the boards' stored copies. held is the list the caller read
// and built boards from, in order; when the stored list is not that one
// (another save, or a connection's removal, changed it since), the save
// fails with ErrTrackerBoardsChanged. A missing project fails with
// sql.ErrNoRows and an invalid list with ErrInvalidTrackerBoards. Each
// failure leaves the stored list as it was. A listing that stays keeps the
// name and tracker project it stored; only a listing the save adds takes
// them from boards. Only the listings it removes are deleted, so a board
// still listed elsewhere is not retired. A reorder or a move of the board
// that opens first removes and adds none, and reads nothing more.
func (s *Store) ReplaceProjectTrackerBoards(projectID string, held []HeldTrackerBoard, boards []ProjectTrackerBoard) (changed bool, listing TrackerBoardListing, err error) {
	if err := validateProjectTrackerBoards(boards); err != nil {
		return false, listing, err
	}
	rows := make([]trackerBoardRow, 0, len(boards))
	connections := make([]string, 0, len(boards))
	for _, board := range boards {
		rows = append(rows, trackerBoardRow{
			ConnectionID: board.ConnectionID, BoardID: board.BoardID, OpensFirst: boolToInt(board.OpensFirst),
			Name: board.Name, TrackerProject: board.TrackerProject,
		})
		connections = append(connections, board.ConnectionID)
	}
	encodedRows, err := json.Marshal(rows)
	if err != nil {
		return false, listing, fmt.Errorf("store: replace project %s tracker boards: encode: %w", projectID, err)
	}
	encodedConnections, err := json.Marshal(uniqueNonEmptyStrings(connections))
	if err != nil {
		return false, listing, fmt.Errorf("store: replace project %s tracker boards: encode: %w", projectID, err)
	}

	action := fmt.Sprintf("store: replace project %s tracker boards", projectID)
	tx, err := s.db.Begin()
	if err != nil {
		return false, listing, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if err := tx.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(new(int)); err != nil {
		return false, listing, fmt.Errorf("%s: read project: %w", action, err)
	}
	// Before the connections are checked: a list built before a
	// connection's removal names it, and the removal is the cause.
	current, err := listProjectTrackerBoards(tx, projectID)
	if err != nil {
		return false, listing, fmt.Errorf("%s: %w", action, err)
	}
	stored := heldTrackerBoards(current)
	if !slices.Equal(stored, held) {
		return false, listing, fmt.Errorf("%w: project %s lists %d boards, not the %d the change was built from or not in their order", ErrTrackerBoardsChanged, projectID, len(stored), len(held))
	}
	unknown, err := queryIDs(tx, `SELECT value FROM json_each(?)
		WHERE value NOT IN (SELECT id FROM tracker_connections WHERE kind <> 'tempo') ORDER BY value`, string(encodedConnections))
	if err != nil {
		return false, listing, fmt.Errorf("%s: check connections: %w", action, err)
	}
	if len(unknown) > 0 {
		return false, listing, fmt.Errorf("%w: connection %s does not exist or has no boards", ErrInvalidTrackerBoards, unknown[0])
	}
	if slices.Equal(stored, heldTrackerBoards(boards)) {
		return false, listing, nil
	}
	removed, err := queryTrackerBoardRefs(tx, `DELETE FROM project_tracker_boards WHERE project_id = ?1 AND (connection_id, board_id) NOT IN (
		SELECT value ->> 'c', value ->> 'b' FROM json_each(?2)) RETURNING connection_id, board_id`, projectID, string(encodedRows))
	if err != nil {
		return false, listing, fmt.Errorf("%s: remove: %w", action, err)
	}
	// Kept listings move past every new position first, so the upsert
	// never meets a position or an opens-first another row still holds.
	// They move by more than the largest stored position, so no row lands
	// on one another row still holds: a connection's removal deletes its
	// listings and leaves gaps, which a count of the rows does not cover.
	var past int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(position), -1) + 1 FROM project_tracker_boards WHERE project_id = ?`, projectID).Scan(&past); err != nil {
		return false, listing, fmt.Errorf("%s: read the last position: %w", action, err)
	}
	for _, step := range []struct {
		what, query string
		args        []any
	}{
		{"move aside", `UPDATE project_tracker_boards SET position = position + ?2, opens_first = 0 WHERE project_id = ?1`,
			[]any{projectID, past + len(boards)}},
		{"place", `INSERT INTO project_tracker_boards(project_id, connection_id, board_id, position, opens_first, name, tracker_project)
			SELECT ?1, value ->> 'c', value ->> 'b', key, value ->> 'f', COALESCE(value ->> 'n', ''), COALESCE(value ->> 'p', '')
			FROM json_each(?2) WHERE true
			ON CONFLICT(project_id, connection_id, board_id) DO UPDATE SET position = excluded.position, opens_first = excluded.opens_first`,
			[]any{projectID, string(encodedRows)}},
	} {
		if _, err := tx.Exec(step.query, step.args...); err != nil {
			return false, listing, fmt.Errorf("%s: %s: %w", action, step.what, err)
		}
	}
	if listing.Unlisted, err = trackerBoardsIn(tx, removed, unlistedTrackerBoardSQL); err != nil {
		return false, listing, fmt.Errorf("%s: read the boards no project lists: %w", action, err)
	}
	if listing.Relisted, err = trackerBoardsIn(tx, addedTrackerBoards(current, boards), retiredTrackerBoardSQL); err != nil {
		return false, listing, fmt.Errorf("%s: read the boards listed again: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, listing, fmt.Errorf("%s: commit: %w", action, err)
	}
	return true, listing, nil
}

// unlistedTrackerBoardSQL keeps the boards no project lists: the test of
// trg_project_tracker_boards_retire, which a migration would have to
// change to share.
const unlistedTrackerBoardSQL = `SELECT value ->> 'c', value ->> 'b' FROM json_each(?1) WHERE NOT EXISTS (
	SELECT 1 FROM project_tracker_boards WHERE connection_id = value ->> 'c' AND board_id = value ->> 'b')`

// retiredTrackerBoardSQL keeps the boards whose stored copy is retired.
const retiredTrackerBoardSQL = `SELECT value ->> 'c', value ->> 'b' FROM json_each(?1) WHERE EXISTS (
	SELECT 1 FROM tracker_cache_roots r JOIN tracker_cache_boards b ON b.root_id = r.id
	 WHERE r.connection_id = value ->> 'c' AND b.board_id = value ->> 'b' AND b.retired = 1)`

// trackerBoardsIn is the boards of refs that query, over their JSON list,
// keeps; none, with no statement, for none.
func trackerBoardsIn(tx *sql.Tx, refs []TrackerBoardRef, query string) ([]TrackerBoardRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	encoded := make([]trackerBoardRow, len(refs))
	for i, ref := range refs {
		encoded[i] = trackerBoardRow{ConnectionID: ref.ConnectionID, BoardID: ref.BoardID}
	}
	list, err := json.Marshal(encoded)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return queryTrackerBoardRefs(tx, query, string(list))
}

// addedTrackerBoards is the boards of now that was does not list.
func addedTrackerBoards(was, now []ProjectTrackerBoard) []TrackerBoardRef {
	listed := make(map[TrackerBoardRef]bool, len(was))
	for _, b := range was {
		listed[TrackerBoardRef{ConnectionID: b.ConnectionID, BoardID: b.BoardID}] = true
	}
	var out []TrackerBoardRef
	for _, b := range now {
		if ref := (TrackerBoardRef{ConnectionID: b.ConnectionID, BoardID: b.BoardID}); !listed[ref] {
			out = append(out, ref)
		}
	}
	return out
}

// queryTrackerBoardRefs reads the (connection, board) pairs query answers.
func queryTrackerBoardRefs(tx *sql.Tx, query string, args ...any) ([]TrackerBoardRef, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []TrackerBoardRef
	for rows.Next() {
		var ref TrackerBoardRef
		if err := rows.Scan(&ref.ConnectionID, &ref.BoardID); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, ref)
	}
	return out, errors.Join(rows.Err(), rows.Close())
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

// ProjectTrackerBoardListed is the slug of projectID when it lists
// connectionID's board boardID, by the listing's key; listed is false
// when it does not, or the project does not exist.
func (s *Store) ProjectTrackerBoardListed(projectID, connectionID, boardID string) (slug string, listed bool, err error) {
	err = s.reader().QueryRow(`SELECT p.slug FROM project_tracker_boards t JOIN projects p ON p.id = t.project_id
		WHERE t.project_id = ? AND t.connection_id = ? AND t.board_id = ?`, projectID, connectionID, boardID).Scan(&slug)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("store: read project %s's listing of board %s of %s: %w", projectID, boardID, connectionID, err)
	}
	return slug, true, nil
}

func listProjectTrackerBoards(q sqlQueryer, projectID string) ([]ProjectTrackerBoard, error) {
	rows, err := q.Query(`SELECT connection_id, board_id, opens_first, name, tracker_project FROM project_tracker_boards
		WHERE project_id = ? ORDER BY position`, projectID)
	if err != nil {
		return nil, err
	}
	var out []ProjectTrackerBoard
	for rows.Next() {
		var board ProjectTrackerBoard
		if err := rows.Scan(&board.ConnectionID, &board.BoardID, &board.OpensFirst, &board.Name, &board.TrackerProject); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, board)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// TrackerBoardChoice is one board a project lists, as a board picker shows
// it: the listing, whether its project is archived, its connection's kind,
// label, state and verified account, and the board's name, which is this
// computer's copy of the board's name once the board has been read and the
// name the listing stored until then (or when the copy is detached or
// retired).
type TrackerBoardChoice struct {
	ProjectID                 string
	ProjectArchived           bool
	ConnectionID              string
	ConnectionKind            string
	ConnectionLabel           string
	ConnectionEnabled         bool
	ConnectionVerifiedAccount string
	BoardID                   string
	BoardName                 string
	TrackerProject            string
	OpensFirst                bool
}

// trackerBoardChoicesSQL reads listings with their connection and the
// name of the board's live copy in the connection's mirror: a detached
// root names no connection and a retired copy is skipped, so neither
// names a board. The UNIQUE (root_id, board_id) of tracker_cache_boards
// and the UNIQUE connection_id of tracker_cache_roots make each listing
// one row.
const trackerBoardChoicesSQL = `SELECT p.project_id, j.archived, p.connection_id, c.kind, c.label, c.enabled, c.verified_account_id,
	p.board_id, COALESCE(NULLIF(b.name, ''), p.name), p.tracker_project, p.opens_first
	FROM project_tracker_boards p
	JOIN projects j ON j.id = p.project_id
	JOIN tracker_connections c ON c.id = p.connection_id
	LEFT JOIN tracker_cache_roots r ON r.connection_id = p.connection_id
	LEFT JOIN tracker_cache_boards b ON b.root_id = r.id AND b.board_id = p.board_id AND b.retired = 0`

// ListTrackerBoardChoices lists the boards projectID lists in picker
// order, or with projectID empty the boards every project lists, by
// project name, then project ID, then picker order; none for a project
// that lists none or does not exist.
func (s *Store) ListTrackerBoardChoices(projectID string) ([]TrackerBoardChoice, error) {
	query, args := trackerBoardChoicesSQL+` WHERE p.project_id = ? ORDER BY p.position`, []any{projectID}
	if projectID == "" {
		query, args = trackerBoardChoicesSQL+` ORDER BY j.name, j.id, p.position`, nil
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tracker board choices of project %q: %w", projectID, err)
	}
	var out []TrackerBoardChoice
	for rows.Next() {
		var c TrackerBoardChoice
		if err := rows.Scan(&c.ProjectID, &c.ProjectArchived, &c.ConnectionID, &c.ConnectionKind, &c.ConnectionLabel, &c.ConnectionEnabled,
			&c.ConnectionVerifiedAccount, &c.BoardID, &c.BoardName, &c.TrackerProject, &c.OpensFirst); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan tracker board choice: %w", err), rows.Close())
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list tracker board choices of project %q: %w", projectID, err)
	}
	return out, nil
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

// ConnectionProjectBoard is a board of a connection one project lists,
// with the project's slug.
type ConnectionProjectBoard struct {
	ProjectID, ProjectSlug, BoardID string
}

// ListConnectionProjectBoards lists, by board and project, each board of
// connectionID with each project that lists it, through the connection's
// index: a read whose cost is the connection's listings alone.
func (s *Store) ListConnectionProjectBoards(connectionID string) ([]ConnectionProjectBoard, error) {
	rows, err := s.reader().QueryContext(context.Background(), `SELECT t.project_id, p.slug, t.board_id FROM project_tracker_boards t
		JOIN projects p ON p.id = t.project_id WHERE t.connection_id = ? ORDER BY t.board_id, t.project_id`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("store: list the project boards of connection %s: %w", connectionID, err)
	}
	var out []ConnectionProjectBoard
	for rows.Next() {
		var b ConnectionProjectBoard
		if err := rows.Scan(&b.ProjectID, &b.ProjectSlug, &b.BoardID); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan a project board of connection %s: %w", connectionID, err), rows.Close())
		}
		out = append(out, b)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list the project boards of connection %s: %w", connectionID, err)
	}
	return out, nil
}
