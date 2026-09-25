package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/threadmode"
)

// Lineage levels (docs/architecture/sqlite-store.md#pointer-forks). A
// reader's levels are numbered from 1 without gaps, nearest first: a
// holder takes the depth of the thread it holds rows of, and a level that
// shows nothing goes, with the levels behind it moving up.

// levelList encodes levels as a JSON array of [reader, depth, cut turn,
// cut item].
func levelList(levels []forkLevel) (string, error) {
	rows := make([][4]any, len(levels))
	for i, l := range levels {
		rows[i] = [4]any{l.reader, l.depth, l.cut.turn, l.cut.item}
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return "", fmt.Errorf("store: encode lineage levels: %w", err)
	}
	return string(encoded), nil
}

// jsonIntList encodes values as one JSON array.
func jsonIntList(values []int) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("store: encode integer list: %w", err)
	}
	return string(encoded), nil
}

// requireLevelRoomTx refuses a new level for a reader that already reads
// through forkLineageMaxDepth levels, and returns the deepest level of the
// readers.
func requireLevelRoomTx(tx *sql.Tx, threadID string, readers []forkLevel) (int, error) {
	list, err := levelList(readers)
	if err != nil {
		return 0, err
	}
	var deepest int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(depth), 0) FROM thread_fork_lineage
		 WHERE thread_id IN (SELECT json_extract(value, '$[0]') FROM json_each(?))`, list).Scan(&deepest); err != nil {
		return 0, fmt.Errorf("store: read the depth of %s's forks: %w", threadID, err)
	}
	if deepest >= forkLineageMaxDepth {
		return 0, fmt.Errorf("%w: a fork of %s reads through %d levels", ErrForkChainTooDeep, threadID, deepest)
	}
	return deepest, nil
}

// insertLevelTx gives each reader a level for holder at the depth where it
// reads the thread the holder holds rows of, with that level's cut. The
// level there and the ones behind it move one deeper, deepest first, so no
// two levels of a reader share a depth.
func insertLevelTx(tx *sql.Tx, holder string, readers []forkLevel, deepest int) error {
	for depth := deepest; depth >= 1; depth-- {
		var ids []string
		for _, r := range readers {
			if r.depth <= depth {
				ids = append(ids, r.reader)
			}
		}
		if len(ids) == 0 {
			continue
		}
		list, err := jsonList(ids)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE thread_fork_lineage SET depth = depth + 1
			 WHERE depth = ? AND thread_id IN (SELECT value FROM json_each(?))`, depth, list); err != nil {
			return fmt.Errorf("store: make room for holder %s: %w", holder, err)
		}
	}
	list, err := levelList(readers)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO thread_fork_lineage (thread_id, depth, ancestor_id, cut_turn_index, cut_item_index)
		SELECT json_extract(value, '$[0]'), json_extract(value, '$[1]'), ?, json_extract(value, '$[2]'), json_extract(value, '$[3]')
		  FROM json_each(?)`, holder, list); err != nil {
		return fmt.Errorf("store: read holder %s: %w", holder, err)
	}
	return nil
}

// emptyLevelSQL is true for a lineage row `l` whose ancestor shows its
// reader nothing: no row or imported row the reader shows (below the cut
// and not hidden), no turn row below the cut, and no hide of a row a level
// behind it shows. It is an existence probe below a cut, so it runs only
// for a holder (dropEmptyHolderLevelsTx), whose rows never change once it
// holds them.
const emptyLevelSQL = `NOT EXISTS (SELECT 1 FROM items
	 WHERE items.thread_id = l.ancestor_id AND ` + inheritedItemVisibleSQL + `)
 AND NOT EXISTS (SELECT 1 FROM thread_import_chunks refs
	 CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id
	 WHERE refs.thread_id = l.ancestor_id AND refs.min_turn_index <= l.cut_turn_index
	   AND ` + inheritedItemVisibleSQL + `
	   AND ` + importedNotOverridden + `)
 AND NOT EXISTS (SELECT 1 FROM turns WHERE turns.thread_id = l.ancestor_id AND turns.turn_index < l.cut_turn_index)
 AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
	 CROSS JOIN thread_fork_lineage deeper
	 CROSS JOIN items ON items.thread_id = deeper.ancestor_id AND items.id = hidden.item_id
	 WHERE hidden.thread_id = l.ancestor_id AND deeper.thread_id = l.thread_id AND deeper.depth > l.depth
	   AND (items.turn_index, items.item_index) < (deeper.cut_turn_index, deeper.cut_item_index))
 AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
	 CROSS JOIN thread_fork_lineage deeper
	 CROSS JOIN import_history_items items ON items.id = hidden.item_id
	 CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id AND refs.thread_id = deeper.ancestor_id
	 WHERE hidden.thread_id = l.ancestor_id AND deeper.thread_id = l.thread_id AND deeper.depth > l.depth
	   AND (items.turn_index, items.item_index) < (deeper.cut_turn_index, deeper.cut_item_index)
	   AND ` + importedNotOverridden + `)`

// emptyHolderLevelSQL reports whether reader ?1's level on holder ?2 shows
// it nothing.
const emptyHolderLevelSQL = `SELECT EXISTS (SELECT 1 FROM thread_fork_lineage l
	 WHERE l.thread_id = ?1 AND l.ancestor_id = ?2 AND ` + emptyLevelSQL + `)`

// dropEmptyHolderLevelsTx removes reader's levels on holders that show it
// nothing after it lowered its cuts (retractInheritedTx) or hid a row
// (hideInheritedItemTx), so a holder a fork reverted or deleted away from
// is released when no other fork reads it. A level on any other thread
// stays: that thread's own writes keep it current.
func dropEmptyHolderLevelsTx(tx *sql.Tx, w *cardWrite, reader string) error {
	holders, err := queryIDs(tx, `SELECT l.ancestor_id FROM thread_fork_lineage l
		  JOIN threads h ON h.id = l.ancestor_id
		 WHERE l.thread_id = ? AND h.mode = ?`, reader, threadmode.ModeHolder)
	if err != nil {
		return fmt.Errorf("store: list the holders %s reads: %w", reader, err)
	}
	for _, holder := range holders {
		var empty bool
		if err := tx.QueryRow(emptyHolderLevelSQL, reader, holder).Scan(&empty); err != nil {
			return fmt.Errorf("store: probe %s's level on holder %s: %w", reader, holder, err)
		}
		if empty {
			if err := dropLevelsTx(tx, w, holder, []string{reader}); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropLevelsTx removes the level on ancestor of each of readers, all of
// which show nothing, and closes the gaps they leave, the levels behind
// each moving up one. A holder whose last level goes is marked for
// deletion (trg_thread_fork_lineage_release), so a removal marks w
// (cardWrite.levelsDropped).
func dropLevelsTx(tx *sql.Tx, w *cardWrite, ancestor string, readers []string) error {
	for _, reader := range readers {
		var depth int
		err := tx.QueryRow(`DELETE FROM thread_fork_lineage WHERE thread_id = ? AND ancestor_id = ? RETURNING depth`, reader, ancestor).Scan(&depth)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store: drop %s's level on %s: %w", reader, ancestor, err)
		}
		w.levelsDropped = true
		for next := depth + 1; next <= forkLineageMaxDepth; next++ {
			result, err := tx.Exec(`UPDATE thread_fork_lineage SET depth = depth - 1 WHERE thread_id = ? AND depth = ?`, reader, next)
			if err != nil {
				return fmt.Errorf("store: close a lineage gap of %s: %w", reader, err)
			}
			moved, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: close a lineage gap of %s: %w", reader, err)
			}
			if moved == 0 {
				break
			}
		}
	}
	return nil
}
