package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"agent-overflow/internal/entityid"
)

// The tracker mirror (migration v159, tracker_cache_*) is a cache of what
// a tracker connection's boards hold. A mirror is one tracker_cache_roots
// row; every accessor takes the root ID its caller captured when its work
// started, and every write fails with ErrTrackerCacheGone once that root
// is cleared, so a result that arrives late cannot fill a cleared mirror.
// The store does not read the data columns: the caller encodes them.
// Times are Unix milliseconds, zero for none.

// ErrTrackerCacheGone is returned by every mirror write whose root was
// cleared or never existed, and by GetOrCreateMirrorRoot for a connection
// that is turned off.
var ErrTrackerCacheGone = errors.New("store: tracker mirror was cleared")

// ErrInvalidTrackerCache is returned for a mirror write the schema would
// refuse or that names nothing: an empty ID, a value out of range, a
// record repeated, or a list longer than its bound. The mirror is left as
// it was.
var ErrInvalidTrackerCache = errors.New("store: invalid tracker mirror write")

// ErrTrackerCacheDrain is wrapped by the error of a connection removal
// that committed but could not drain the mirror it detached; the next
// ResumeMirrorClears finishes it.
var ErrTrackerCacheDrain = errors.New("store: tracker mirror detached but not drained")

// ErrTrackerCacheNoBoard is returned by a write of a board's rows or a
// reconcile of a board whose structure the mirror does not hold: the
// caller stores the structure first.
var ErrTrackerCacheNoBoard = errors.New("store: tracker mirror has no such board")

// clearMirrorChunk is how many rows one transaction of a clear deletes.
const clearMirrorChunk = 500

// clearMirrorTables are the mirror's tables in the order a clear drains
// them: every table before the tables it references, so no batch
// cascades into rows of another table. TestClearMirrorDrainsEveryTable
// holds it to the schema.
var clearMirrorTables = []string{
	"tracker_cache_issue_details",
	"tracker_cache_changes",
	"tracker_cache_issue_sprints",
	"tracker_cache_board_issues",
	"tracker_cache_board_column_statuses",
	"tracker_cache_board_columns",
	"tracker_cache_sprints",
	"tracker_cache_issues",
	"tracker_cache_boards",
	"tracker_cache_worklogs",
	"tracker_cache_sync",
	"tracker_cache_users",
	"tracker_cache_statuses",
	"tracker_cache_projects",
}

// clearMirrorChunkSQL deletes one batch of a detached root's rows from
// each table, one fixed statement per table.
var clearMirrorChunkSQL = func() map[string]string {
	out := make(map[string]string, len(clearMirrorTables))
	for _, table := range clearMirrorTables {
		out[table] = `DELETE FROM ` + table + ` WHERE rowid IN (SELECT rowid FROM ` + table +
			` WHERE root_id = ? LIMIT ` + strconv.Itoa(clearMirrorChunk) + `)`
	}
	return out
}()

// GetOrCreateMirrorRoot returns connectionID's mirror root, making one
// when it has none. A missing connection is sql.ErrNoRows, a Tempo
// connection (whose worklogs live under its Jira connection's root)
// ErrInvalidTrackerConnection, and a connection turned off
// ErrTrackerCacheGone.
func (s *Store) GetOrCreateMirrorRoot(connectionID string) (rootID string, created bool, err error) {
	action := fmt.Sprintf("store: get or create tracker mirror of connection %s", connectionID)
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var kind string
	var enabled bool
	if err := tx.QueryRow(`SELECT kind, enabled FROM tracker_connections WHERE id = ?`, connectionID).Scan(&kind, &enabled); err != nil {
		return "", false, fmt.Errorf("%s: read connection: %w", action, err)
	}
	switch {
	case kind == TrackerKindTempo:
		return "", false, fmt.Errorf("%w: a Tempo connection has no mirror of its own", ErrInvalidTrackerConnection)
	case !enabled:
		return "", false, fmt.Errorf("%s: the connection is turned off: %w", action, ErrTrackerCacheGone)
	}
	err = tx.QueryRow(`SELECT id FROM tracker_cache_roots WHERE connection_id = ?`, connectionID).Scan(&rootID)
	if err == nil {
		return rootID, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("%s: read root: %w", action, err)
	}
	rootID = entityid.New()
	if _, err := tx.Exec(`INSERT INTO tracker_cache_roots(id, connection_id, created_at) VALUES (?, ?, ?)`, rootID, connectionID, nowMillis()); err != nil {
		return "", false, fmt.Errorf("%s: insert root: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("%s: commit: %w", action, err)
	}
	return rootID, true, nil
}

// GetMirrorRoot returns connectionID's mirror root, if it has one.
func (s *Store) GetMirrorRoot(connectionID string) (rootID string, ok bool, err error) {
	err = s.reader().QueryRow(`SELECT id FROM tracker_cache_roots WHERE connection_id = ?`, connectionID).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read tracker mirror of connection %s: %w", connectionID, err)
	}
	return rootID, true, nil
}

// ClearMirror clears connectionID's mirror and reports whether it had one.
// Its first transaction detaches the root from the connection: from then
// on every write naming the root fails with ErrTrackerCacheGone, and
// GetOrCreateMirrorRoot makes a new root. It then deletes the root's rows
// clearMirrorChunk at a time, each batch its own transaction with pause
// between them, and the root last. A clear that stops part way leaves a
// detached root that ResumeMirrorClears finishes.
func (s *Store) ClearMirror(connectionID string, pause ChunkPause) (cleared bool, err error) {
	var rootID string
	err = s.db.QueryRow(`UPDATE tracker_cache_roots SET connection_id = NULL WHERE connection_id = ? RETURNING id`, connectionID).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: detach tracker mirror of connection %s: %w", connectionID, err)
	}
	return true, s.drainMirrorRoot(rootID, pause)
}

// ResumeMirrorClears finishes every drain that stopped part way, of a
// clear or of a connection's removal, and returns the roots it deleted.
// The app runs it once at start, before it syncs anything.
func (s *Store) ResumeMirrorClears(pause ChunkPause) ([]string, error) {
	roots, err := queryIDs(s.db, `SELECT id FROM tracker_cache_roots WHERE connection_id IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list detached tracker mirrors: %w", err)
	}
	for i, rootID := range roots {
		if err := s.drainMirrorRoot(rootID, pause); err != nil {
			return roots[:i], err
		}
	}
	return roots, nil
}

// mirrorRootsOfConnectionTx lists the mirror root of connectionID, at most
// one, which its removal detaches. The Tempo connection a removal takes
// with it has none (GetOrCreateMirrorRoot).
func mirrorRootsOfConnectionTx(tx *sql.Tx, connectionID string) ([]string, error) {
	return queryIDs(tx, `SELECT id FROM tracker_cache_roots WHERE connection_id = ?`, connectionID)
}

// drainMirrorRoot deletes a detached root's rows in batches, then the
// root. pause runs before each of its transactions, so it also separates
// them from the transaction that detached the root.
func (s *Store) drainMirrorRoot(rootID string, pause ChunkPause) error {
	between := func() {
		if pause != nil {
			pause()
		}
	}
	for _, table := range clearMirrorTables {
		for {
			between()
			result, err := s.db.Exec(clearMirrorChunkSQL[table], rootID)
			if err != nil {
				return fmt.Errorf("store: clear tracker mirror %s: %s: %w", rootID, table, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: clear tracker mirror %s: %s: %w", rootID, table, err)
			}
			if n < clearMirrorChunk {
				break
			}
		}
	}
	between()
	if _, err := s.db.Exec(`DELETE FROM tracker_cache_roots WHERE id = ? AND connection_id IS NULL`, rootID); err != nil {
		return fmt.Errorf("store: delete tracker mirror root %s: %w", rootID, err)
	}
	return nil
}

// mirrorWrite runs fn in one write transaction after checking that rootID
// is a live root, so every mirror writer refuses a cleared root with
// ErrTrackerCacheGone the same way. A root cannot be detached while the
// transaction holds the writer.
func (s *Store) mirrorWrite(rootID, action string, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var live int
	err = tx.QueryRow(`SELECT 1 FROM tracker_cache_roots WHERE id = ? AND connection_id IS NOT NULL`, rootID).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: root %s: %w", action, rootID, ErrTrackerCacheGone)
	}
	if err != nil {
		return fmt.Errorf("%s: read root: %w", action, err)
	}
	if err := fn(tx); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	return nil
}

// requireMirrorBoardTx fails with ErrTrackerCacheNoBoard when the mirror
// does not hold boardID's structure.
func requireMirrorBoardTx(tx *sql.Tx, rootID, boardID string) error {
	var found int
	err := tx.QueryRow(`SELECT 1 FROM tracker_cache_boards WHERE root_id = ? AND board_id = ?`, rootID, boardID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("board %s: %w", boardID, ErrTrackerCacheNoBoard)
	}
	if err != nil {
		return fmt.Errorf("read board %s: %w", boardID, err)
	}
	return nil
}

// mirrorData is a data column's value: never NULL.
func mirrorData(data []byte) []byte {
	if data == nil {
		return []byte{}
	}
	return data
}

// encodeMirrorIDs encodes ids as the JSON array a json_each statement
// reads.
func encodeMirrorIDs(ids []string) (string, error) {
	if ids == nil {
		ids = []string{}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("encode IDs: %w", err)
	}
	return string(encoded), nil
}
