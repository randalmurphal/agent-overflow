package store

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/threadmode"
)

// Shown rows a thread writes (docs/architecture/sqlite-store.md#ownership).
//
// A row, payload or turn a fork shows never changes (fork_triggers.go). A
// thread that changes one of its own that a fork shows, a late provider
// report on a settled row for one, first gives the forks a copy: a holder
// takes a copy of the rows, which it hides behind it, and each fork that
// shows them reads the holder right before the thread, with the cut it
// reads the thread at (holdCopiesTx). No fork shows the thread's rows
// after that, so the write lands; the forks show the copies, which are the
// rows as they were. The mutable-row hooks run it: requireMutableItemTx,
// requireMutablePayloadTx and requireMutableTurnTx.
//
// A write that copies or moves rows to a holder reuses the holder that
// every reader of those rows reads right before the thread, when the
// thread's rows made it (reusableHolderTx), so a reader's lineage grows
// only when a reader of the rows does not read that holder.

// shownItemLevelsSQL lists the levels on ?1 through which a thread shows
// ?1's row ?2 at (?3, ?4): readerShowsItemSQL's lineage rows.
const shownItemLevelsSQL = `SELECT l.thread_id, l.depth, l.cut_turn_index, l.cut_item_index
  FROM thread_fork_lineage l
 WHERE l.ancestor_id = ?1 AND (l.cut_turn_index, l.cut_item_index) > (?3, ?4)
   AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ?2)
   AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                     JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ?2
                    WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)`

// shownTurnLevelsSQL lists the levels on ?1 through which a thread shows
// ?1's row of turn ?2: readerShowsTurnSQL's lineage rows.
const shownTurnLevelsSQL = `SELECT l.thread_id, l.depth, l.cut_turn_index, l.cut_item_index
  FROM thread_fork_lineage l
 WHERE l.ancestor_id = ?1 AND l.cut_turn_index > ?2
   AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = ?2)
   AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                     JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = ?2
                    WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
                      AND nearer.cut_turn_index > ?2)`

// threadReadSQL is true when a lineage row names ?.
const threadReadSQL = `SELECT EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = ?)`

// threadReadTx reports whether a lineage row names threadID: one probe of
// idx_thread_fork_lineage_ancestor, all a write to a thread no fork reads
// pays.
func threadReadTx(tx *sql.Tx, threadID string) (bool, error) {
	var read bool
	if err := tx.QueryRow(threadReadSQL, threadID).Scan(&read); err != nil {
		return false, fmt.Errorf("store: probe the forks of %s: %w", threadID, err)
	}
	return read, nil
}

// reownShownItemTx gives the forks that show threadID's own row itemID a
// copy of it before threadID writes it.
func reownShownItemTx(tx *sql.Tx, threadID, itemID string) error {
	read, err := threadReadTx(tx, threadID)
	if err != nil || !read {
		return err
	}
	row := inheritedRow{owner: threadID}
	var at timelineRow
	err = tx.QueryRow(`SELECT id, COALESCE(payload_id, ''), COALESCE(input_payload_id, ''), turn_index, item_index
		  FROM items WHERE thread_id = ? AND id = ?`, threadID, itemID).
		Scan(&row.id, &row.payloadID, &row.inputPayloadID, &at.turn, &at.item)
	if err != nil {
		return fmt.Errorf("store: read row %s/%s its forks may show: %w", threadID, itemID, err)
	}
	readers, err := queryLevels(tx, shownItemLevelsSQL, threadID, itemID, at.turn, at.item)
	if err != nil {
		return fmt.Errorf("store: list the forks that show %s/%s: %w", threadID, itemID, err)
	}
	if len(readers) == 0 {
		return nil
	}
	return holdCopiesTx(tx, threadID, []inheritedRow{row}, nil, readers)
}

// ownPayloadRowsSQL lists ?1's own rows, local and imported, that name
// payload ?2, with their positions.
var ownPayloadRowsSQL = `SELECT id, COALESCE(payload_id, ''), COALESCE(input_payload_id, ''), turn_index, item_index
  FROM items WHERE thread_id = ?1 AND payload_id = ?2
UNION ALL
SELECT id, COALESCE(payload_id, ''), COALESCE(input_payload_id, ''), turn_index, item_index
  FROM items WHERE thread_id = ?1 AND input_payload_id = ?2
UNION ALL
SELECT items.id, COALESCE(items.payload_id, ''), COALESCE(items.input_payload_id, ''), items.turn_index, items.item_index
  FROM import_history_payloads ref_payload
  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
  CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
 WHERE ref_payload.id = ?2 AND refs.thread_id = ?1
   AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
   AND ` + importedNotOverridden

// reownShownPayloadTx gives the forks that show a row of threadID's that
// renders payloadID a copy of every such row they show, which brings the
// payload, before threadID writes it.
func reownShownPayloadTx(tx *sql.Tx, threadID, payloadID string) error {
	read, err := threadReadTx(tx, threadID)
	if err != nil || !read {
		return err
	}
	rows, err := tx.Query(ownPayloadRowsSQL, threadID, payloadID)
	if err != nil {
		return fmt.Errorf("store: read the rows of %s that render payload %s: %w", threadID, payloadID, err)
	}
	type positioned struct {
		row inheritedRow
		at  timelineRow
	}
	var named []positioned
	seen := make(map[string]bool)
	for rows.Next() {
		p := positioned{row: inheritedRow{owner: threadID}}
		if err := rows.Scan(&p.row.id, &p.row.payloadID, &p.row.inputPayloadID, &p.at.turn, &p.at.item); err != nil {
			return errors.Join(fmt.Errorf("store: scan a row of %s that renders payload %s: %w", threadID, payloadID, err), rows.Close())
		}
		if !seen[p.row.id] {
			seen[p.row.id] = true
			named = append(named, p)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: read the rows of %s that render payload %s: %w", threadID, payloadID, err)
	}
	var shown []inheritedRow
	var readers []forkLevel
	reading := make(map[string]bool)
	for _, p := range named {
		levels, err := queryLevels(tx, shownItemLevelsSQL, threadID, p.row.id, p.at.turn, p.at.item)
		if err != nil {
			return fmt.Errorf("store: list the forks that show %s/%s: %w", threadID, p.row.id, err)
		}
		if len(levels) == 0 {
			continue
		}
		shown = append(shown, p.row)
		for _, level := range levels {
			if !reading[level.reader] {
				reading[level.reader] = true
				readers = append(readers, level)
			}
		}
	}
	if len(shown) == 0 {
		return nil
	}
	return holdCopiesTx(tx, threadID, shown, nil, readers)
}

// reownShownTurnTx gives the forks that show threadID's row of turn a copy
// of it before threadID writes it.
func reownShownTurnTx(tx *sql.Tx, threadID string, turn int) error {
	read, err := threadReadTx(tx, threadID)
	if err != nil || !read {
		return err
	}
	readers, err := queryLevels(tx, shownTurnLevelsSQL, threadID, turn)
	if err != nil {
		return fmt.Errorf("store: list the forks that show turn %d of %s: %w", turn, threadID, err)
	}
	if len(readers) == 0 {
		return nil
	}
	return holdCopiesTx(tx, threadID, nil, []int{turn}, readers)
}

// requireMutablePayloadTx prepares threadID to change the content of one
// payload it renders: threadID gets a payload row of its own
// (ensureLocalPayloadTx), and the forks that show a row rendering it get
// their copy first (reownShownPayloadTx).
func requireMutablePayloadTx(tx *sql.Tx, threadID, payloadID, label string) error {
	if err := ensureLocalPayloadTx(tx, threadID, payloadID, label); err != nil {
		return err
	}
	if err := reownShownPayloadTx(tx, threadID, payloadID); err != nil {
		return fmt.Errorf("%s give the forks of %s their payload %s: %w", label, threadID, payloadID, err)
	}
	return nil
}

// requireMutableTurnTx prepares the change of one turn row, named by its
// id: the forks that show it get their copy first (reownShownTurnTx). It
// reports false when no turn has the id.
func requireMutableTurnTx(tx *sql.Tx, turnID, label string) (bool, error) {
	var threadID string
	var turn int
	err := tx.QueryRow(`SELECT thread_id, turn_index FROM turns WHERE turn_id = ?`, turnID).Scan(&threadID, &turn)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s read turn %s: %w", label, turnID, err)
	}
	if err := reownShownTurnTx(tx, threadID, turn); err != nil {
		return false, fmt.Errorf("%s give the forks of %s their turn %d: %w", label, threadID, turn, err)
	}
	return true, nil
}

// holdCopiesTx gives readers, the levels on threadID that show rows (each
// held by its owner, threadID or an ancestor) or threadID's rows of turns,
// copies of them in a holder: the holder hides the rows' ids behind it
// (snapshotRowsTx), and each reader reads it right before threadID with
// the cut it reads threadID at, so every reader shows the same rows and
// turns before and after, and threadID's are shown by none. The holder is
// the one readers already read there when it serves them all
// (reusableHolderTx), else a new one.
func holdCopiesTx(tx *sql.Tx, threadID string, rows []inheritedRow, turns []int, readers []forkLevel) error {
	holder, deepest, reused, err := holderForTx(tx, threadID, readers)
	if err != nil {
		return err
	}
	if err := withHistoryBulkLoadTx(tx, holder, func() error { return snapshotRowsTx(tx, holder, rows) }); err != nil {
		return err
	}
	if len(turns) > 0 {
		list, err := jsonIntList(turns)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id)
			 SELECT ?2 || ':' || turn_index, ?2, turn_index, started_at, completed_at,
			    stop_reason, assistant_message_id, token_usage_json, error_message, provider_turn_id
			   FROM turns WHERE thread_id = ?1 AND turn_index IN (SELECT value FROM json_each(?3))`,
			threadID, holder, list); err != nil {
			return fmt.Errorf("store: copy the turns of %s its forks show: %w", threadID, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM subagent_aggregates WHERE thread_id = ?`, holder); err != nil {
		return fmt.Errorf("store: drop the cards of the rows %s's holder copied: %w", threadID, err)
	}
	if reused {
		return nil
	}
	return insertLevelTx(tx, holder, readers, deepest)
}

// holderForTx returns the holder that takes rows of threadID for readers:
// the one they read right before threadID when it serves them all
// (reusableHolderTx), or a new one, which the caller gives each reader a
// level on (insertLevelTx) with deepest, the deepest level of the readers.
func holderForTx(tx *sql.Tx, threadID string, readers []forkLevel) (holder string, deepest int, reused bool, err error) {
	if holder, err = reusableHolderTx(tx, threadID, readers); err != nil || holder != "" {
		return holder, 0, holder != "", err
	}
	if deepest, err = requireLevelRoomTx(tx, threadID, readers); err != nil {
		return "", 0, false, err
	}
	holder, err = createHolderTx(tx, threadID)
	return holder, deepest, false, err
}

// reusableHolderTx returns the holder that every one of readers (levels on
// threadID) reads right before threadID, when a split or copy of
// threadID's rows made it, or "". A made holder's fork_source_thread_id
// names the thread whose rows it took; a retired thread holds its own
// history, whose rows sit where its source's do, and names none.
//
// Such a holder H takes rows for readers without a new level. Each level
// on H was made with the cut of the reader's level on threadID, and both
// move only together (a reader's own revert lowers every level of it) or
// the level on threadID alone, down to the end of a revert's kept rows
// (splitShownRowsTx). So for every reader r of H either both cuts are
// equal, or r's cut on threadID is the highest cut on threadID of H's
// readers. A row readers show sits below their cuts on threadID; for any
// other reader of H it sits at or past its cut there, which is then not
// the highest, so past its cut on H too. The rows H takes read the same
// for every reader of H before and after: readers show them from H and
// the others show none of them. A reader of H that reads another level
// between H and threadID reads that level for rows readers do not show,
// or they would read it right before threadID, so the hides H adds for
// the rows reach no row it shows. H's rows sit where threadID's rows were
// when H took them: a split takes rows at or past the kept end, where no
// reader reads threadID from then on, and a copy takes rows H then hides,
// so no row a reader shows is at one of H's positions, and a split leaves
// a row whose position or id H holds to the caller (holderHoldsSQL).
func reusableHolderTx(tx *sql.Tx, threadID string, readers []forkLevel) (string, error) {
	list, err := levelList(readers)
	if err != nil {
		return "", err
	}
	rows, err := tx.Query(`SELECT near.ancestor_id, COUNT(*) FROM json_each(?) r
		  CROSS JOIN thread_fork_lineage near
		    ON near.thread_id = json_extract(r.value, '$[0]') AND near.depth = json_extract(r.value, '$[1]') - 1
		 GROUP BY near.ancestor_id`, list)
	if err != nil {
		return "", fmt.Errorf("store: read the levels before %s: %w", threadID, err)
	}
	var holder string
	var groups, count int
	for rows.Next() {
		groups++
		if err := rows.Scan(&holder, &count); err != nil {
			return "", errors.Join(fmt.Errorf("store: scan a level before %s: %w", threadID, err), rows.Close())
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", fmt.Errorf("store: read the levels before %s: %w", threadID, err)
	}
	if groups != 1 || count != len(readers) {
		return "", nil
	}
	var made bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM threads WHERE id = ? AND mode = ? AND fork_source_thread_id = ?)`,
		holder, threadmode.ModeHolder, threadID).Scan(&made); err != nil {
		return "", fmt.Errorf("store: probe holder %s of %s: %w", holder, threadID, err)
	}
	if !made {
		return "", nil
	}
	return holder, nil
}

// holderHoldsSQL is true for a row (unqualified item columns of the
// `items` alias, local or imported) whose id or position holder ?
// already holds, locally or in its imported history, or whose id it
// hides. A split leaves such a row to the thread that reverts it, which
// removes it: the holder's row of that id or position is what the
// readers show there, and no reader of the holder shows the thread's.
// It binds the holder five times (holderHoldsArgs).
const holderHoldsSQL = `(EXISTS (SELECT 1 FROM items held WHERE held.thread_id = ? AND held.id = items.id)
   OR EXISTS (SELECT 1 FROM items held
               WHERE held.thread_id = ? AND held.turn_index = items.turn_index AND held.item_index = items.item_index)
   OR EXISTS (SELECT 1 FROM thread_fork_hidden held WHERE held.thread_id = ? AND held.item_id = items.id)
   OR EXISTS (SELECT 1 FROM import_history_items held
               CROSS JOIN thread_import_chunks held_refs ON held_refs.chunk_id = held.chunk_id AND held_refs.thread_id = ?
               WHERE held.id = items.id
                 AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o
                                  WHERE o.thread_id = held_refs.thread_id AND o.item_id = held.id))
   OR EXISTS (SELECT 1 FROM thread_import_chunks held_refs
               CROSS JOIN import_history_items held ON held.chunk_id = held_refs.chunk_id
                 AND held.turn_index = items.turn_index AND held.item_index = items.item_index
               WHERE held_refs.thread_id = ?
                 AND held_refs.min_turn_index <= items.turn_index AND held_refs.max_turn_index >= items.turn_index
                 AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o
                                  WHERE o.thread_id = held_refs.thread_id AND o.item_id = held.id)))`

// holderHoldsArgs binds holderHoldsSQL.
func holderHoldsArgs(holder string) []any {
	return []any{holder, holder, holder, holder, holder}
}
