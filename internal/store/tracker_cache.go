package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The tracker mirror (migration v159, tracker_cache_*) is a cache of what
// a tracker connection's boards hold. A mirror is one tracker_cache_roots
// row; every accessor takes the root ID its caller captured when its work
// started, and every write fails with ErrTrackerCacheGone once that root
// is detached, so a result that arrives late cannot fill a cleared mirror.
// Inside a live root each record carries its own order: a write of a read
// that began before the latest read admitted for the record is superseded
// and changes nothing (see MirrorWrite); an identical write from a later
// read moves the record's read time alone, and one from the same read
// changes no row. A board has lifetimes inside a root (MirrorSync.Lifetime):
// each board write names the lifetime its caller captured, so a write
// captured before the board was unlisted and listed again is refused. The store does
// not read the data columns: the caller encodes them. Times are Unix
// milliseconds, zero for none.

// ErrTrackerCacheGone is returned by every mirror write whose root was
// detached or never existed, and by GetOrCreateMirrorRoot for a connection
// that is turned off.
var ErrTrackerCacheGone = errors.New("store: tracker mirror was cleared")

// ErrInvalidTrackerCache is returned for a mirror write the schema would
// refuse or that names nothing: an empty ID, a value out of range, a
// record repeated, or a list longer than its bound. The mirror is left as
// it was.
var ErrInvalidTrackerCache = errors.New("store: invalid tracker mirror write")

// ErrTrackerCacheNoBoard is returned by a write of a board's rows or a
// reconcile of a board whose structure the mirror does not hold: the
// caller stores the structure first.
var ErrTrackerCacheNoBoard = errors.New("store: tracker mirror has no such board")

// ErrTrackerCacheUnlisted is returned by a write naming a board no project
// lists through the root's connection, one whose last listing went and
// whose rows FinishMirrorCleanup has yet to remove, or a lifetime of the
// board other than its current one. The caller stops syncing the board; a
// board listed again is synced again, under its new lifetime, once the
// cleanup has removed it.
var ErrTrackerCacheUnlisted = errors.New("store: no project lists the tracker board")

// ErrTrackerCacheSwept is returned by a write of content read outside a
// board (UpsertMirrorCards, StoreMirrorDetail, ReplaceMirrorHistories)
// naming a root sweep earlier than the root's: a sweep deleted issues
// since the caller captured MirrorSync.Sweep, so the read may describe an
// issue's earlier lifetime. The caller reads the sync state and the
// content again.
var ErrTrackerCacheSwept = errors.New("store: tracker mirror swept issues since the read began; read it again")

// ErrTrackerCacheStale is returned by ListMirrorSection for a continuation
// read at another revision of the board than the page it continues: the
// caller reads the section again from its start.
var ErrTrackerCacheStale = errors.New("store: tracker board changed since the page was read; read it again from the start")

// MirrorWrite is what a replace did: Changed reports a stored change;
// Superseded reports a write refused whole because the stored record came
// from a read that began later.
type MirrorWrite struct {
	Changed, Superseded bool
}

// GetOrCreateMirrorRoot returns connectionID's mirror root, making one
// when it has none. A missing connection is sql.ErrNoRows, a Tempo
// connection (whose worklogs live under its Jira connection's root)
// ErrInvalidTrackerConnection, and a connection turned off
// ErrTrackerCacheGone.
func (s *Store) GetOrCreateMirrorRoot(connectionID string) (rootID int64, created bool, err error) {
	action := fmt.Sprintf("store: get or create tracker mirror of connection %s", connectionID)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var kind string
	var enabled bool
	var existing sql.NullInt64
	if err := tx.QueryRow(`SELECT c.kind, c.enabled, r.id FROM tracker_connections c
		LEFT JOIN tracker_cache_roots r ON r.connection_id = c.id WHERE c.id = ?`, connectionID).Scan(&kind, &enabled, &existing); err != nil {
		return 0, false, fmt.Errorf("%s: read connection: %w", action, err)
	}
	switch {
	case kind == TrackerKindTempo:
		return 0, false, fmt.Errorf("%w: a Tempo connection has no mirror of its own", ErrInvalidTrackerConnection)
	case !enabled:
		return 0, false, fmt.Errorf("%s: the connection is turned off: %w", action, ErrTrackerCacheGone)
	case existing.Valid:
		return existing.Int64, false, nil
	}
	if err := tx.QueryRow(`INSERT INTO tracker_cache_roots(connection_id, created_at) VALUES (?, ?) RETURNING id`,
		connectionID, nowMillis()).Scan(&rootID); err != nil {
		return 0, false, fmt.Errorf("%s: insert root: %w", action, err)
	}
	// Each board listed now starts its lifetime with the root; one listed
	// later starts its own (trg_project_tracker_boards_list).
	if _, err := tx.Exec(`INSERT INTO tracker_cache_boards(root_id, board_id)
		SELECT ?1, board_id FROM project_tracker_boards WHERE connection_id = ?2 ON CONFLICT DO NOTHING`, rootID, connectionID); err != nil {
		return 0, false, fmt.Errorf("%s: start board lifetimes: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return rootID, true, nil
}

// GetMirrorRoot returns connectionID's mirror root, if it has one.
func (s *Store) GetMirrorRoot(connectionID string) (rootID int64, ok bool, err error) {
	err = s.reader().QueryRow(`SELECT id FROM tracker_cache_roots WHERE connection_id = ?`, connectionID).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: read tracker mirror of connection %s: %w", connectionID, err)
	}
	return rootID, true, nil
}

// DetachMirror detaches connectionID's mirror root in one statement and
// reports whether it had one: from then on every write naming the root
// fails with ErrTrackerCacheGone, every read of it is empty, and
// GetOrCreateMirrorRoot makes a new root. FinishMirrorCleanup deletes the
// detached root's rows. A connection's removal detaches its root the same
// way, through the schema.
func (s *Store) DetachMirror(connectionID string) (detached bool, err error) {
	detached, err = execChangedTx(s.db, `UPDATE tracker_cache_roots SET connection_id = NULL WHERE connection_id = ? RETURNING id`, connectionID)
	if err != nil {
		return false, fmt.Errorf("store: detach tracker mirror of connection %s: %w", connectionID, err)
	}
	return detached, nil
}

// mirrorWrite runs fn in one write transaction after checking that rootID
// is a live root, so every mirror writer refuses a detached root with
// ErrTrackerCacheGone the same way. A root cannot be detached while the
// transaction holds the writer.
func (s *Store) mirrorWrite(rootID int64, action string, fn func(tx *sql.Tx) error) error {
	return s.mirrorSweptWrite(rootID, 0, action, fn)
}

// mirrorSweptWrite is mirrorWrite for a write of content read outside a
// board: sweep is the root's sweep its caller captured before the read
// (MirrorSync.Sweep), and the write is refused with ErrTrackerCacheSwept
// when a sweep has deleted issues since, in the same statement as the
// root's check. Zero checks no sweep, for the mirror's own writes.
func (s *Store) mirrorSweptWrite(rootID, sweep int64, action string, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var live bool
	var current int64
	err = tx.QueryRow(`SELECT connection_id IS NOT NULL, sweep FROM tracker_cache_roots WHERE id = ?`, rootID).Scan(&live, &current)
	switch {
	case errors.Is(err, sql.ErrNoRows) || err == nil && !live:
		return fmt.Errorf("%s: root %d: %w", action, rootID, ErrTrackerCacheGone)
	case err != nil:
		return fmt.Errorf("%s: read root: %w", action, err)
	case sweep > current:
		return fmt.Errorf("%s: %w: sweep %d was never reached (the root is at %d)", action, ErrInvalidTrackerCache, sweep, current)
	case sweep > 0 && sweep < current:
		return fmt.Errorf("%s: sweep %d, root at %d: %w", action, sweep, current, ErrTrackerCacheSwept)
	}
	if err := fn(tx); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// mirrorBoard is a board's state as a write of it reads it.
type mirrorBoard struct {
	// stored reports that the mirror holds the board's structure.
	stored bool
	// readAt is when the latest read of the structure admitted began.
	readAt int64
	// rowsRemovedAt is when the latest read that found rows gone began.
	rowsRemovedAt int64
}

// mirrorBoardWriteSQL reads, in one statement, whether a root is live,
// whether some project lists the board through its connection, and the
// state of the board's lifetime ?3.
const mirrorBoardWriteSQL = `SELECT r.connection_id IS NOT NULL,
	  EXISTS (SELECT 1 FROM project_tracker_boards p WHERE p.connection_id = r.connection_id AND p.board_id = ?2),
	  coalesce(b.retired, 1), coalesce(b.read_at, 0), coalesce(b.rows_removed_at, 0)
	FROM tracker_cache_roots r LEFT JOIN tracker_cache_boards b ON b.root_id = r.id AND b.board_id = ?2 AND b.lifetime = ?3
	WHERE r.id = ?1`

// mirrorBoardWrite runs fn in one write transaction for a write naming
// lifetime of boardID: the root must be live (ErrTrackerCacheGone), some
// project must list the board through the root's connection, and lifetime
// must be the board's current one, not retired (ErrTrackerCacheUnlisted).
// With needStructure, the mirror must hold the board's structure
// (ErrTrackerCacheNoBoard).
func (s *Store) mirrorBoardWrite(rootID int64, boardID string, lifetime int64, action string, needStructure bool,
	fn func(tx *sql.Tx, board mirrorBoard) error) (err error) {
	if boardID == "" || lifetime <= 0 {
		return fmt.Errorf("%s: %w: a board write names the board and the lifetime its caller captured", action, ErrInvalidTrackerCache)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var live, listed, retired bool
	var board mirrorBoard
	err = tx.QueryRow(mirrorBoardWriteSQL, rootID, boardID, lifetime).Scan(&live, &listed, &retired, &board.readAt, &board.rowsRemovedAt)
	board.stored = board.readAt > 0
	switch {
	case errors.Is(err, sql.ErrNoRows) || err == nil && !live:
		return fmt.Errorf("%s: root %d: %w", action, rootID, ErrTrackerCacheGone)
	case err != nil:
		return fmt.Errorf("%s: read root and board: %w", action, err)
	case !listed || retired:
		return fmt.Errorf("%s: %w", action, ErrTrackerCacheUnlisted)
	case needStructure && !board.stored:
		return fmt.Errorf("%s: %w", action, ErrTrackerCacheNoBoard)
	}
	if err := fn(tx, board); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// mirrorBoardShown is the rule for which boards of a root the mirror
// shows, in one place: a board whose structure is stored, not retired,
// that some project lists through the root's live connection. root and
// board are SQL expressions naming it.
func mirrorBoardShown(root, board string) string {
	return `EXISTS (SELECT 1 FROM tracker_cache_boards shown_board
	  JOIN tracker_cache_roots shown_root ON shown_root.id = shown_board.root_id
	  JOIN project_tracker_boards shown_listing ON shown_listing.connection_id = shown_root.connection_id
	   AND shown_listing.board_id = shown_board.board_id
	 WHERE shown_board.root_id = ` + root + ` AND shown_board.board_id = ` + board + `
	   AND shown_board.retired = 0 AND shown_board.read_at > 0)`
}

// mirrorLinksSQL is the FROM and WHERE of the links that keep issues in a
// root's mirror: a thread this computer owns whose ticket names the root's
// live connection's kind and site. link_ticket.issue_id is the issue.
func mirrorLinksSQL(root string) string {
	return `tracker_cache_roots link_root
	  JOIN tracker_connections link_conn ON link_conn.id = link_root.connection_id
	  JOIN thread_tickets link_ticket ON link_ticket.kind = link_conn.kind AND link_ticket.site = link_conn.site
	  JOIN owned_threads link_thread ON link_thread.id = link_ticket.thread_id
	 WHERE link_root.id = ` + root
}

// mirrorIssueKept is the rule for which issues a mirror keeps, in one
// place: an issue with a row on a board the mirror shows
// (mirrorBoardShown), or one a link names (mirrorLinksSQL). An issue the
// rule does not keep is absent to every read and write and the cleanup
// sweep removes it. root and issue are SQL expressions naming it.
func mirrorIssueKept(root, issue string) string {
	return `(EXISTS (SELECT 1 FROM tracker_cache_board_issues kept_row
	          WHERE kept_row.root_id = ` + root + ` AND kept_row.issue_id = ` + issue + `
	            AND ` + mirrorBoardShown("kept_row.root_id", "kept_row.board_id") + `)
	  OR EXISTS (SELECT 1 FROM ` + mirrorLinksSQL(root) + ` AND link_ticket.issue_id = ` + issue + `))`
}

// mirrorData is a data column's value: never NULL.
func mirrorData(data []byte) []byte {
	if data == nil {
		return []byte{}
	}
	return data
}

// encodeMirrorJSON encodes v as the JSON a json_each statement reads.
func encodeMirrorJSON(v any) (string, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode for json_each: %w", err)
	}
	return string(encoded), nil
}

// encodeMirrorIDs encodes ids as the JSON array a json_each statement
// reads.
func encodeMirrorIDs(ids []string) (string, error) {
	if ids == nil {
		ids = []string{}
	}
	return encodeMirrorJSON(ids)
}

// execChangedTx runs a write whose RETURNING yields a row for each row it
// changed, and reports whether it yielded any.
func execChangedTx(q sqlQueryer, query string, args ...any) (bool, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return false, err
	}
	changed := false
	for rows.Next() {
		changed = true
	}
	return changed, errors.Join(rows.Err(), rows.Close())
}
