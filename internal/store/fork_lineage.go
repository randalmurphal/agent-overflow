package store

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
)

// Pointer-fork row ownership (docs/architecture/sqlite-store.md#pointer-forks).
//
// A fork reads the rows before its cut from the thread that owns them. It
// takes its own copy of an inherited row only when it must own it:
//
//   - copy-on-write, when the fork mutates an inherited row or payload
//     (requireMutableItemTx, requireMutablePayloadTx);
//   - hand-off, before a thread changes, moves, deletes or hides a row
//     another fork reads through it (handOffIDsTx, handOffOwnRowsTx), so
//     that fork's history stays what it was when it was made;
//   - materialization, when the fork's history leaves this database
//     (MaterializeForkHistory, run by the transfer export).
//
// Every copy keeps the row's id, position and content and hides the
// ancestor's row from the fork, so a read returns the same timeline before
// and after. Copies insert under history_bulk_load: the row takes the
// thread's current stamp and the caller advances history_rev once.

// forkCopyBatch bounds how many ids one copy statement names.
const forkCopyBatch = 200

// forkDividerToolName marks the divider row a fork holds at its cut.
const forkDividerToolName = "fork_origin"

// forkDividerID is the divider's item id. One per fork, so every write that
// moves or updates it finds it by primary key.
func forkDividerID(threadID string) string {
	return "fork-origin-" + threadID
}

// inheritedRow is one row a fork reads from an ancestor, the owner.
type inheritedRow struct {
	id             string
	payloadID      string
	inputPayloadID string
	owner          string
}

// Lineage level filters for the inherited-row reads, predicates on the
// lineage row `l`. allLevels reads every level; levelAt reads the rows one
// ancestor owns; levelsFrom reads the rows a viewer reads from an ancestor
// or through it.
const allLevels = ""

func levelAt(depth int) string { return " AND l.depth = " + strconv.Itoa(depth) }

func levelsFrom(depth int) string { return " AND l.depth >= " + strconv.Itoa(depth) }

// inheritedRowColumns is the lineage arms' projection of an inheritedRow.
func inheritedRowColumns(string, string) string {
	return `items.id, COALESCE(items.payload_id, ''), COALESCE(items.input_payload_id, ''), l.ancestor_id`
}

// queryInheritedRows reads the inherited rows viewer shows that sel
// selects, on the lineage levels `levels` selects. sel's Columns are
// inheritedRowColumns; a lookup sets KeyFirst or Turn as timelineArms
// requires, so the read costs the rows it names.
func queryInheritedRows(q sqlQueryer, viewer, levels string, sel timelineSelection) ([]inheritedRow, error) {
	sel.Columns = inheritedRowColumns
	query, args := inheritedTimelineArms(viewer, levels, sel)
	return scanInheritedRows(q, viewer, query, args)
}

// scanInheritedRows runs an inherited-row read, keeping the first row of
// each id: a payload read can name one row twice.
func scanInheritedRows(q sqlQueryer, viewer, query string, args []any) ([]inheritedRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read inherited rows of %s: %w", viewer, err)
	}
	var out []inheritedRow
	seen := make(map[string]bool)
	for rows.Next() {
		var row inheritedRow
		if err := rows.Scan(&row.id, &row.payloadID, &row.inputPayloadID, &row.owner); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan inherited row of %s: %w", viewer, err), rows.Close())
		}
		if !seen[row.id] {
			seen[row.id] = true
			out = append(out, row)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate inherited rows of %s: %w", viewer, err)
	}
	return out, nil
}

// readerRowColumns is inheritedRowColumns plus the reader, the thread whose
// lineage row `l` is.
func readerRowColumns(string, string) string {
	return inheritedRowColumns("", "") + `, l.thread_id`
}

// readersInheritedRowsByID is inheritedRowsByID for every thread that reads
// through threadID at depth with its cut after from, in one statement per
// id batch: the readers are the lineage rows `r`, each the viewer of its own
// lineage arms. The result maps each such reader to the rows it shows.
func readersInheritedRowsByID(q sqlQueryer, threadID string, depth int, from timelineRow, ids []string) (map[string][]inheritedRow, error) {
	out := make(map[string][]inheritedRow)
	seen := make(map[[2]string]bool)
	for start := 0; start < len(ids); start += forkCopyBatch {
		clause, args := inClause("items.id", ids[start:min(start+forkCopyBatch, len(ids))])
		query, binds := inheritedTimelineArms("", levelsFrom(depth), timelineSelection{
			Columns:  readerRowColumns,
			Source:   "thread_fork_lineage r",
			Thread:   "r.thread_id",
			KeyFirst: true,
			Where: "r.ancestor_id = ? AND r.depth = ? AND (r.cut_turn_index, r.cut_item_index) > (?, ?)" +
				"\n		   AND " + clause,
			WhereArgs: append([]any{threadID, depth, from.turn, from.item}, args...),
		})
		rows, err := q.Query(query, binds...)
		if err != nil {
			return nil, fmt.Errorf("store: read rows forks read through %s: %w", threadID, err)
		}
		for rows.Next() {
			var row inheritedRow
			var reader string
			if err := rows.Scan(&row.id, &row.payloadID, &row.inputPayloadID, &row.owner, &reader); err != nil {
				return nil, errors.Join(fmt.Errorf("store: scan a row a fork reads through %s: %w", threadID, err), rows.Close())
			}
			if key := [2]string{reader, row.id}; !seen[key] {
				seen[key] = true
				out[reader] = append(out[reader], row)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("store: iterate rows forks read through %s: %w", threadID, err)
		}
	}
	return out, nil
}

// inheritedRowsByID resolves ids to the inherited rows the viewer shows.
// Ids the viewer owns or does not show are absent from the result.
func inheritedRowsByID(q sqlQueryer, viewer string, ids []string, levels string) ([]inheritedRow, error) {
	var out []inheritedRow
	for start := 0; start < len(ids); start += forkCopyBatch {
		clause, args := inClause("items.id", ids[start:min(start+forkCopyBatch, len(ids))])
		rows, err := queryInheritedRows(q, viewer, levels, timelineSelection{KeyFirst: true, Where: clause, WhereArgs: args})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// inheritedPayloadRows reads the inherited rows the viewer shows that name
// payloadID as their payload or input payload.
func inheritedPayloadRows(q sqlQueryer, viewer, payloadID string) ([]inheritedRow, error) {
	return scanInheritedRows(q, viewer, inheritedPayloadRowArms(inheritedRowColumns("", ""), allLevels),
		repeatArgs(payloadRowArmCount, []any{viewer, payloadID}))
}

// withHistoryBulkLoadTx runs body with threadID's stamp triggers frozen,
// restoring the prior flag. The caller writes the aggregate stamp.
func withHistoryBulkLoadTx(tx *sql.Tx, threadID string, body func() error) error {
	var prior int
	if err := tx.QueryRow(`SELECT history_bulk_load FROM threads WHERE id = ?`, threadID).Scan(&prior); err != nil {
		return fmt.Errorf("store: read history load flag of %s: %w", threadID, err)
	}
	if prior == 0 {
		if _, err := tx.Exec(`UPDATE threads SET history_bulk_load = 1 WHERE id = ?`, threadID); err != nil {
			return fmt.Errorf("store: set history load flag of %s: %w", threadID, err)
		}
	}
	if err := body(); err != nil {
		return err
	}
	if prior == 0 {
		if _, err := tx.Exec(`UPDATE threads SET history_bulk_load = 0 WHERE id = ?`, threadID); err != nil {
			return fmt.Errorf("store: clear history load flag of %s: %w", threadID, err)
		}
	}
	return nil
}

// copyInheritedRowsTx gives threadID its own copy of rows it reads from an
// ancestor: payloads first, because items reference them by foreign key,
// then the hide that lets a copy sit below the fork's cut, then the rows,
// their search index rows, the cards the copies change
// (recomputeLocalizedCardsTx) and the ownership of the attachments they
// show. The caller holds history_bulk_load and accounts for the stamp.
func copyInheritedRowsTx(tx *sql.Tx, threadID string, rows []inheritedRow) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		for _, payloadID := range []string{row.payloadID, row.inputPayloadID} {
			if payloadID == "" {
				continue
			}
			if err := copyPayloadFromTx(tx, threadID, row.owner, payloadID); err != nil {
				return err
			}
		}
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.id
	}
	if err := hideForkRowsTx(tx, threadID, ids); err != nil {
		return err
	}
	groups := make(map[string][]string)
	var owners []string
	for _, row := range rows {
		if _, seen := groups[row.owner]; !seen {
			owners = append(owners, row.owner)
		}
		groups[row.owner] = append(groups[row.owner], row.id)
	}
	// An owner shows each id from its items or, when it has no local row,
	// from its imported history; both are read by id.
	const copyColumns = `SELECT items.id, ?, items.turn_index, items.item_index, items.kind, items.role,
				       items.status, items.summary, items.payload_id, items.input_payload_id,
				       items.parent_id, items.is_background, items.completion_of, items.tool_name,
				       items.decision, items.meta, items.created_at, items.updated_at`
	for _, owner := range owners {
		group := groups[owner]
		for start := 0; start < len(group); start += forkCopyBatch {
			batch := group[start:min(start+forkCopyBatch, len(group))]
			clause, args := inClause("items.id", batch)
			binds := append([]any{threadID, owner}, args...)
			binds = append(append(binds, threadID, owner), args...)
			result, err := tx.Exec(itemInsertPrefix+`
				`+copyColumns+`
				  FROM items WHERE items.thread_id = ? AND `+clause+`
				UNION ALL
				`+copyColumns+`
				  FROM import_history_items items
				  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = items.chunk_id
				 WHERE refs.thread_id = ? AND `+clause+` AND `+importedNotOverridden, binds...)
			if err != nil {
				return fmt.Errorf("store: copy inherited rows into %s: %w", threadID, err)
			}
			copied, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: count inherited rows copied into %s: %w", threadID, err)
			}
			if copied != int64(len(batch)) {
				return fmt.Errorf("store: copy inherited rows into %s: copied %d of %d", threadID, copied, len(batch))
			}
		}
	}
	for _, id := range ids {
		if err := indexItemByIDTx(tx, threadID, id); err != nil {
			return err
		}
	}
	if err := recomputeLocalizedCardsTx(tx, threadID, ids, "store: copy inherited rows:"); err != nil {
		return err
	}
	return ownCopiedAttachmentsTx(tx, threadID, rows)
}

// copyInheritedRowsStampedTx is copyInheritedRowsTx for a copy no mutation
// follows: it advances threadID's stamp once for the whole copy.
func copyInheritedRowsStampedTx(tx *sql.Tx, threadID string, rows []inheritedRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return copyInheritedRowsTx(tx, threadID, rows)
	}); err != nil {
		return err
	}
	return bumpHistoryRevTx(tx, threadID, "store: stamp copied fork rows")
}

// copyPayloadFromTx copies one payload from the thread that holds it, with
// its append chunks and edit snapshots. A payload threadID already holds is
// left alone.
func copyPayloadFromTx(tx *sql.Tx, threadID, owner, payloadID string) error {
	var held bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?)`, threadID, payloadID).Scan(&held); err != nil {
		return fmt.Errorf("store: inspect payload %s/%s: %w", threadID, payloadID, err)
	}
	if held {
		return nil
	}
	result, err := tx.Exec(`INSERT INTO payloads (thread_id, id, kind, meta, data, created_at, preview_spans, spans)
		SELECT ?, id, kind, meta, data, created_at, preview_spans, spans FROM payloads WHERE thread_id = ? AND id = ?`,
		threadID, owner, payloadID)
	if err != nil {
		return fmt.Errorf("store: copy payload %s into %s: %w", payloadID, threadID, err)
	}
	copied, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count payload %s copied into %s: %w", payloadID, threadID, err)
	}
	if copied == 1 {
		if _, err := tx.Exec(`INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, start_offset, data, created_at)
			SELECT ?, payload_id, chunk_index, start_offset, data, created_at FROM payload_chunks WHERE thread_id = ? AND payload_id = ?`,
			threadID, owner, payloadID); err != nil {
			return fmt.Errorf("store: copy payload %s chunks into %s: %w", payloadID, threadID, err)
		}
		if _, err := tx.Exec(`INSERT INTO edit_file_snapshots (thread_id, payload_id, path, content, created_at)
			SELECT ?, payload_id, path, content, created_at FROM edit_file_snapshots WHERE thread_id = ? AND payload_id = ?`,
			threadID, owner, payloadID); err != nil {
			return fmt.Errorf("store: copy payload %s edit snapshots into %s: %w", payloadID, threadID, err)
		}
		return nil
	}
	result, err = tx.Exec(`INSERT INTO payloads (thread_id, id, kind, meta, data, created_at, preview_spans, spans)
		SELECT ?, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans
		  FROM import_history_payloads p
		  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id
		 WHERE refs.thread_id = ? AND p.id = ?
		 LIMIT 1`, threadID, owner, payloadID)
	if err != nil {
		return fmt.Errorf("store: copy imported payload %s into %s: %w", payloadID, threadID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: copy payload %s of %s into %s", payloadID, owner, threadID)); err != nil {
		return err
	}
	return nil
}

func hideForkRowsTx(tx *sql.Tx, threadID string, ids []string) error {
	for start := 0; start < len(ids); start += forkCopyBatch {
		batch := ids[start:min(start+forkCopyBatch, len(ids))]
		args := make([]any, 0, 2*len(batch))
		for _, id := range batch {
			args = append(args, threadID, id)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO thread_fork_hidden (thread_id, item_id) VALUES `+
			repeatPlaceholders("(?,?)", len(batch)), args...); err != nil {
			return fmt.Errorf("store: hide inherited rows in %s: %w", threadID, err)
		}
	}
	return nil
}

func repeatPlaceholders(group string, count int) string {
	out := make([]byte, 0, count*(len(group)+1))
	for i := range count {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, group...)
	}
	return string(out)
}

// forkReader is a thread that reads another thread's rows through its
// lineage, with the depth at which that thread sits in its chain.
type forkReader struct {
	threadID string
	depth    int
}

// forkReadExistsSQL asks whether any thread reads through a thread. It runs
// before every write that changes a row another thread may read, so it is
// one probe of idx_thread_fork_lineage_ancestor.
const forkReadExistsSQL = `SELECT EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = ?)`

// forkReadersSQL lists the threads that read through a thread at or after a
// timeline position, nearest first: a reader whose cut at that thread's
// level is at or before the position reads nothing there. It is one range
// probe of idx_thread_fork_lineage_ancestor.
const forkReadersSQL = `SELECT thread_id, depth FROM thread_fork_lineage
 WHERE ancestor_id = ? AND (cut_turn_index, cut_item_index) > (?, ?)
 ORDER BY depth, thread_id`

// forkReaderDepthsSQL is forkReadersSQL's depths. The same range probe finds
// none for a write after every reader's cut, the source's hot path.
const forkReaderDepthsSQL = `SELECT DISTINCT depth FROM thread_fork_lineage
 WHERE ancestor_id = ? AND (cut_turn_index, cut_item_index) > (?, ?)
 ORDER BY depth`

// readThroughTx reports whether any thread reads through threadID.
func readThroughTx(q sqlQueryer, threadID string) (bool, error) {
	var read bool
	if err := q.QueryRow(forkReadExistsSQL, threadID).Scan(&read); err != nil {
		return false, fmt.Errorf("store: probe fork readers of %s: %w", threadID, err)
	}
	return read, nil
}

// queryForkLevels scans (thread_id, depth) lineage rows.
func queryForkLevels(q sqlQueryer, query string, args ...any) ([]forkReader, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var readers []forkReader
	for rows.Next() {
		var reader forkReader
		if err := rows.Scan(&reader.threadID, &reader.depth); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		readers = append(readers, reader)
	}
	return readers, errors.Join(rows.Err(), rows.Close())
}

// forkReadersTx lists every thread that reads through threadID at or after
// from, nearest first. A nearer reader's copy replaces the row for the
// readers that read through it, so handing off in this order copies a row
// once per branch; a farther reader copies only what the nearer one no
// longer reads, such as rows below a cut the nearer reader lowered.
func forkReadersTx(q sqlQueryer, threadID string, from timelineRow) ([]forkReader, error) {
	readers, err := queryForkLevels(q, forkReadersSQL, threadID, from.turn, from.item)
	if err != nil {
		return nil, fmt.Errorf("store: list fork readers of %s: %w", threadID, err)
	}
	return readers, nil
}

// handOff copies into every reader of threadID at or after from the rows
// read selects for it.
func handOff(tx *sql.Tx, threadID string, from timelineRow, read func(reader forkReader) ([]inheritedRow, error)) error {
	readers, err := forkReadersTx(tx, threadID, from)
	if err != nil {
		return err
	}
	for _, reader := range readers {
		rows, err := read(reader)
		if err != nil {
			return err
		}
		if err := copyInheritedRowsStampedTx(tx, reader.threadID, rows); err != nil {
			return err
		}
	}
	return nil
}

// handOffIDsTx runs before threadID changes, moves, deletes or hides the
// rows ids names in its timeline, whether it owns them or inherits them.
// Every thread that reads one of them through threadID gets its own copy
// first, so a fork's history does not change because a thread it reads
// from rewrote that thread's own history.
func handOffIDsTx(tx *sql.Tx, threadID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if read, err := readThroughTx(tx, threadID); err != nil || !read {
		return err
	}
	return handOffReadIDsTx(tx, threadID, ids)
}

// handOffReadIDsTx is handOffIDsTx once some thread is known to read
// through threadID.
func handOffReadIDsTx(tx *sql.Tx, threadID string, ids []string) error {
	return forEachReaderShowingTx(tx, threadID, ids, func(reader string, rows []inheritedRow) error {
		return copyInheritedRowsStampedTx(tx, reader, rows)
	})
}

// forEachReaderShowingTx calls visit with every thread that shows some of
// ids through threadID and the rows it shows. Only the readers whose cut
// follows the first of the rows are asked which rows they read, and all the
// readers at one depth are asked in one statement: the rows a source writes
// while it runs are after every fork's cut or, for a fork made mid-turn,
// rows it hides because it took interrupted copies of them, so the write
// costs the same however many forks there are. Depths run nearest first and
// each is asked after the nearer depths were visited, for the reason
// handOff gives.
func forEachReaderShowingTx(tx *sql.Tx, threadID string, ids []string, visit func(reader string, rows []inheritedRow) error) error {
	from, found, err := firstRowOfTx(tx, threadID, ids)
	if err != nil || !found {
		return err
	}
	depths, err := forkReaderDepthsTx(tx, threadID, from)
	if err != nil {
		return err
	}
	for _, depth := range depths {
		byReader, err := readersInheritedRowsByID(tx, threadID, depth, from, ids)
		if err != nil {
			return err
		}
		for _, reader := range slices.Sorted(maps.Keys(byReader)) {
			if err := visit(reader, byReader[reader]); err != nil {
				return err
			}
		}
	}
	return nil
}

// forkReaderDepthsTx lists the depths of forkReaderDepthsSQL.
func forkReaderDepthsTx(q sqlQueryer, threadID string, from timelineRow) ([]int, error) {
	rows, err := q.Query(forkReaderDepthsSQL, threadID, from.turn, from.item)
	if err != nil {
		return nil, fmt.Errorf("store: list fork reader depths of %s: %w", threadID, err)
	}
	var depths []int
	for rows.Next() {
		var depth int
		if err := rows.Scan(&depth); err != nil {
			return nil, errors.Join(fmt.Errorf("store: scan a fork reader depth of %s: %w", threadID, err), rows.Close())
		}
		depths = append(depths, depth)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: iterate fork reader depths of %s: %w", threadID, err)
	}
	return depths, nil
}

// firstRowOfTx is the earliest position of ids in threadID's timeline.
// found is false when the timeline shows none of them.
func firstRowOfTx(tx *sql.Tx, threadID string, ids []string) (timelineRow, bool, error) {
	var first timelineRow
	found := false
	for start := 0; start < len(ids); start += forkCopyBatch {
		clause, args := inClause("items.id", ids[start:min(start+forkCopyBatch, len(ids))])
		query, binds, err := timelineArms(tx, threadID, timelineSelection{
			Columns: timelineIDColumns, KeyFirst: true, Where: clause, WhereArgs: args,
		})
		if err != nil {
			return timelineRow{}, false, err
		}
		rows, err := tx.Query(query, binds...)
		if err != nil {
			return timelineRow{}, false, fmt.Errorf("store: read positions of %s rows: %w", threadID, err)
		}
		for rows.Next() {
			var row timelineRow
			if err := rows.Scan(&row.id, &row.turn, &row.item); err != nil {
				return timelineRow{}, false, errors.Join(fmt.Errorf("store: scan position of a %s row: %w", threadID, err), rows.Close())
			}
			if !found || row.turn < first.turn || (row.turn == first.turn && row.item < first.item) {
				first, found = row, true
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return timelineRow{}, false, fmt.Errorf("store: iterate positions of %s rows: %w", threadID, err)
		}
	}
	return first, found, nil
}

// handOffOwnRowsTx is handOffIDsTx for threadID's own rows at or after
// fromTurn that match where (unqualified or `items.` columns, or ""),
// before threadID deletes them. A reader whose cut precedes fromTurn reads
// none of them.
func handOffOwnRowsTx(tx *sql.Tx, threadID string, fromTurn int, where string, args ...any) error {
	sel := timelineSelection{Turn: "?", TurnArgs: []any{fromTurn}, FromTurn: true}
	if where != "" {
		sel.Where, sel.WhereArgs = "("+where+")", args
	}
	return handOff(tx, threadID, timelineRow{turn: fromTurn, item: math.MinInt}, func(reader forkReader) ([]inheritedRow, error) {
		return queryInheritedRows(tx, reader.threadID, levelAt(reader.depth), sel)
	})
}

// handOffPayloadTx is handOffIDsTx for the rows of threadID's timeline that
// reference payloadID, before threadID rewrites the payload's content. The
// rows are found only when some thread reads through threadID.
func handOffPayloadTx(tx *sql.Tx, threadID, payloadID string) error {
	if read, err := readThroughTx(tx, threadID); err != nil || !read {
		return err
	}
	ids, err := payloadRowIDsTx(tx, threadID, payloadID)
	if err != nil || len(ids) == 0 {
		return err
	}
	return handOffReadIDsTx(tx, threadID, ids)
}

// bumpPayloadReadersTx advances the stamp of every thread that shows
// payloadID's rows through holder, for a write to the holder's payload row
// that no hand-off precedes (UpdatePayloadSpans). The readers are found as
// handOffPayloadTx finds them, so a payload whose rows follow every reader's
// cut asks no reader.
func bumpPayloadReadersTx(tx *sql.Tx, holder, payloadID, label string) error {
	if read, err := readThroughTx(tx, holder); err != nil || !read {
		return err
	}
	ids, err := payloadRowIDsTx(tx, holder, payloadID)
	if err != nil || len(ids) == 0 {
		return err
	}
	return forEachReaderShowingTx(tx, holder, ids, func(reader string, _ []inheritedRow) error {
		return bumpHistoryRevTx(tx, reader, label)
	})
}

// payloadRowIDsSQL reads the ids of the rows of a thread's timeline that
// reference a payload, own or inherited, through the payload keys. It binds
// (thread, payload) once per arm, 2*payloadRowArmCount times.
var payloadRowIDsSQL = ownPayloadRowArms("items.id") + "\n		UNION ALL\n		" + inheritedPayloadRowArms("items.id", allLevels)

// payloadRowIDsTx lists the rows of threadID's timeline that reference
// payloadID.
func payloadRowIDsTx(tx *sql.Tx, threadID, payloadID string) ([]string, error) {
	ids, err := queryIDs(tx, payloadRowIDsSQL, repeatArgs(2*payloadRowArmCount, []any{threadID, payloadID})...)
	if err != nil {
		return nil, fmt.Errorf("store: read rows of payload %s/%s: %w", threadID, payloadID, err)
	}
	return ids, nil
}

func queryIDs(q sqlQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(rows.Err(), rows.Close())
}

// shadowInheritedItemTx is copy-on-write for one inherited row. It reports
// false when threadID does not show itemID as an inherited row. The caller's
// mutation of the copy advances the stamp.
func shadowInheritedItemTx(tx *sql.Tx, threadID, itemID string) (bool, error) {
	rows, err := inheritedRowsByID(tx, threadID, []string{itemID}, allLevels)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return copyInheritedRowsTx(tx, threadID, rows)
	}); err != nil {
		return false, err
	}
	return true, nil
}

// shadowInheritedPayloadTx is copy-on-write for an inherited payload: it
// copies every inherited row that references it, which brings the payload.
// A payload row is only ever read beside the item that references it, so a
// payload copied without its rows would diverge from what they render.
func shadowInheritedPayloadTx(tx *sql.Tx, threadID, payloadID string) (bool, error) {
	rows, err := inheritedPayloadRows(tx, threadID, payloadID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	if err := withHistoryBulkLoadTx(tx, threadID, func() error {
		return copyInheritedRowsTx(tx, threadID, rows)
	}); err != nil {
		return false, err
	}
	return true, nil
}

// payloadHolderTx names the thread whose row holds a payload threadID reads:
// threadID itself, or the nearest ancestor that holds it. A cache written to
// the holder (UpdatePayloadSpans) is seen by every fork that reads it.
func payloadHolderTx(tx *sql.Tx, threadID, payloadID string) (string, error) {
	var holder string
	var depth int
	err := tx.QueryRow(`SELECT ?, 0 WHERE `+payloadHeldBySQL("?", "?")+`
		UNION ALL
		SELECT l.ancestor_id, l.depth FROM thread_fork_lineage l
		 WHERE l.thread_id = ? AND `+payloadHeldBySQL("l.ancestor_id", "?")+`
		ORDER BY 2
		LIMIT 1`,
		threadID, threadID, payloadID, threadID, payloadID, threadID, payloadID, payloadID,
	).Scan(&holder, &depth)
	if errors.Is(err, sql.ErrNoRows) {
		return threadID, nil
	}
	if err != nil {
		return "", fmt.Errorf("store: resolve payload %s holder for %s: %w", payloadID, threadID, err)
	}
	return holder, nil
}
