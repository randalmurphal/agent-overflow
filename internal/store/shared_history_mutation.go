package store

import (
	"database/sql"
	"fmt"
)

// ensureLocalPayloadTx gives one thread a mutable payload row when the
// requested payload currently comes from immutable imported history or from
// a pointer fork's ancestor. Copying it is representation-only: the thread
// reads the same bytes before and after, so callers bump history_rev only
// for the mutation that follows. An inherited payload comes with the
// inherited rows that reference it (shadowInheritedPayloadTx), so those
// rows render the mutated payload too.
func ensureLocalPayloadTx(tx *sql.Tx, threadID, payloadID, label string) error {
	var local bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?)`, threadID, payloadID).Scan(&local); err != nil {
		return fmt.Errorf("%s inspect local payload: %w", label, err)
	}
	if local {
		return nil
	}

	// No local row exists, so the imported row is the whole logical payload.
	result, err := tx.Exec(
		`INSERT OR IGNORE INTO payloads (
		    thread_id, id, kind, meta, data, created_at, preview_spans, spans
		 )
		 SELECT refs.thread_id, p.id, p.kind, p.meta, p.data, p.created_at, p.preview_spans, p.spans
		   FROM import_history_payloads p
		   CROSS JOIN thread_import_chunks refs ON refs.chunk_id = p.chunk_id
		  WHERE p.id = ? AND refs.thread_id = ?`,
		payloadID, threadID,
	)
	if err != nil {
		return fmt.Errorf("%s copy imported payload %s/%s: %w", label, threadID, payloadID, err)
	}
	copied, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s count copied imported payload %s/%s: %w", label, threadID, payloadID, err)
	}
	if copied == 0 {
		if _, err := shadowInheritedPayloadTx(tx, threadID, payloadID); err != nil {
			return fmt.Errorf("%s copy inherited payload %s/%s: %w", label, threadID, payloadID, err)
		}
	}

	var exists int
	if err := tx.QueryRow(
		`SELECT 1 FROM payloads WHERE thread_id = ? AND id = ?`,
		threadID, payloadID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("%s payload %s/%s: %w", label, threadID, payloadID, err)
	}
	return nil
}

// localizeImportedItemTx copies one immutable imported item into the thread's
// mutable overlay. The explicit override is inserted first because the items
// trigger rejects accidental shadowing. Item INSERT history accounting is
// suppressed while the representation changes; the caller's subsequent
// UPDATE or DELETE advances the public stamp. The cards the move changes
// are recomputed here (recomputeLocalizedCardsTx).
func localizeImportedItemTx(tx *sql.Tx, threadID, itemID, label string) (bool, error) {
	var payloadID, inputPayloadID string
	err := tx.QueryRow(
		`SELECT COALESCE(imported.payload_id, ''), COALESCE(imported.input_payload_id, '')
		   FROM import_history_items imported
		   CROSS JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
		  WHERE imported.id = ? AND refs.thread_id = ?
		    AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides o
		      WHERE o.thread_id = refs.thread_id AND o.item_id = imported.id)`,
		itemID, threadID,
	).Scan(&payloadID, &inputPayloadID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s find imported item %s/%s: %w", label, threadID, itemID, err)
	}
	for _, id := range []string{payloadID, inputPayloadID} {
		if id != "" {
			if err := ensureLocalPayloadTx(tx, threadID, id, label); err != nil {
				return false, err
			}
		}
	}
	if err := setHistoryBulkLoadTx(tx, threadID, true, label); err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`INSERT INTO thread_import_item_overrides (thread_id, item_id) VALUES (?, ?)`,
		threadID, itemID,
	); err != nil {
		return false, fmt.Errorf("%s mark imported item override %s/%s: %w", label, threadID, itemID, err)
	}
	result, err := tx.Exec(
		`INSERT INTO items (
		    id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, input_payload_id, parent_id, is_background, completion_of,
		    tool_name, decision, meta, created_at, updated_at
		 )
		 SELECT imported.id, ?, imported.turn_index, imported.item_index,
		        imported.kind, imported.role, imported.status, imported.summary,
		        imported.payload_id, imported.input_payload_id, imported.parent_id,
		        imported.is_background, imported.completion_of, imported.tool_name,
		        imported.decision, imported.meta, imported.created_at, imported.updated_at
		   FROM import_history_items imported
		   CROSS JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
		  WHERE imported.id = ? AND refs.thread_id = ?`,
		threadID, itemID, threadID,
	)
	if err != nil {
		return false, fmt.Errorf("%s copy imported item %s/%s: %w", label, threadID, itemID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("%s copy imported item %s/%s", label, threadID, itemID)); err != nil {
		return false, err
	}
	// The override moves this item from the import arm to the item arm, so its
	// index row moves with it. The caller's mutation re-indexes the new text.
	if err := deleteThreadSearchItemsTx(tx, threadID, []string{itemID}); err != nil {
		return false, err
	}
	if err := indexItemByIDTx(tx, threadID, itemID); err != nil {
		return false, err
	}
	if err := setHistoryBulkLoadTx(tx, threadID, false, label); err != nil {
		return false, err
	}
	if err := recomputeLocalizedCardsTx(tx, threadID, []string{itemID}, label); err != nil {
		return false, err
	}
	return true, nil
}

// recomputeLocalizedCardsTx recomputes, with their families, the stamps
// that moving ids from a read-only arm (imported history, an ancestor's
// rows) into threadID's items changes. A read shows the same rows and the
// same cards, since the walk and the recompute read every arm, but a
// round whose prompt is not local is readTime, and only a local anchor
// holds a stamp. So the stamps are: the parent of a moved prompt, a moved
// carrier, a moved root a local carrier names, and a moved anchor with a
// visible child in any arm; a moved anchor with none stays unstamped, as
// a new anchor does. The thread stamp advances before the first stamp
// write, under history_bulk_load too. An accumulator a card holds for a
// member recomputes once a note reaches it (flushCardsTx checks the
// generation of every stamp a note reached).
func recomputeLocalizedCardsTx(tx *sql.Tx, threadID string, ids []string, label string) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	seeds, err := queryIDs(tx, localizedCardsSQL, threadID, list)
	if err != nil {
		return fmt.Errorf("%s find the cards of localized rows in %s: %w", label, threadID, err)
	}
	anchors, err := queryIDs(tx, `SELECT id FROM items
		 WHERE thread_id = ? AND id IN (SELECT value FROM json_each(?)) AND `+aggAnchorableSQL("items."),
		threadID, list)
	if err != nil {
		return fmt.Errorf("%s find localized subagent anchors in %s: %w", label, threadID, err)
	}
	if len(anchors) > 0 {
		if list, err = jsonList(anchors); err != nil {
			return err
		}
		children, args, err := timelineArms(tx, threadID, timelineSelection{
			Columns:   func(_, _ string) string { return "items.parent_id AS parent_id" },
			KeyFirst:  true,
			Where:     "items.parent_id IN (SELECT value FROM json_each(?)) AND items.parent_id <> '' AND " + visibleItemsFilterFor("items."),
			WhereArgs: []any{list},
		})
		if err != nil {
			return err
		}
		parents, err := queryIDs(tx, `SELECT DISTINCT parent_id FROM (`+children+`)`, args...)
		if err != nil {
			return fmt.Errorf("%s probe children of localized subagent anchors in %s: %w", label, threadID, err)
		}
		seeds = append(seeds, parents...)
	}
	if len(seeds) == 0 {
		return nil
	}
	bumped := false
	_, err = recomputeSubagentFamiliesTx(tx, threadID, seeds, func() error {
		if bumped {
			return nil
		}
		bumped = true
		return bumpHistoryRevTx(tx, threadID, label+" serve localized cards")
	})
	return err
}

// localizedCardsSQL selects, among the rows ?2 (a JSON array) moved into
// ?1's items, the parents of prompts, the carriers, and the roots a local
// carrier names. Each moved row is read by key; the carrier probe
// compares with `+m.id` so idx_items_transcript_root serves the value, as
// in stampedRowIDsFor.
var localizedCardsSQL = `SELECT m.parent_id FROM json_each(?2) AS moved
  CROSS JOIN items m ON m.thread_id = ?1 AND m.id = moved.value
 WHERE m.parent_id <> '' AND ` + aggPromptSQL("m.") + `
UNION
SELECT m.id FROM json_each(?2) AS moved
  CROSS JOIN items m ON m.thread_id = ?1 AND m.id = moved.value
 WHERE ` + aggAnchorableSQL("m.") + `
   AND (` + aggCarrierSQL("m.") + ` OR EXISTS (SELECT 1 FROM items WHERE thread_id = ?1 AND ` + transcriptRootExpr + ` = +m.id))`

func setHistoryBulkLoadTx(tx *sql.Tx, threadID string, enabled bool, label string) error {
	from, to := 0, 1
	if !enabled {
		from, to = 1, 0
	}
	result, err := tx.Exec(
		`UPDATE threads SET history_bulk_load = ? WHERE id = ? AND history_bulk_load = ?`,
		to, threadID, from,
	)
	if err != nil {
		return fmt.Errorf("%s set history materialization flag for %s: %w", label, threadID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("%s set history materialization flag for %s", label, threadID))
}

// requireMutableItemTx prepares threadID to change one row of its timeline:
// threadID gets a row it may write, localized from its imported history or
// copied from an ancestor when it does not own one, and the forks that
// show that row get their copy first (reownShownItemTx). It returns a
// wrapped sql.ErrNoRows when threadID does not show the row.
func requireMutableItemTx(tx *sql.Tx, threadID, itemID, label string) error {
	_, err := readMutableSubagentRowTx(tx, threadID, itemID, label)
	return err
}

// ownShownItemTx gives threadID its own copy of a row it shows but does not
// own: localized from its imported history, or copied from the ancestor a
// pointer fork reads it from. It returns a wrapped sql.ErrNoRows when
// threadID shows no such row.
func ownShownItemTx(tx *sql.Tx, threadID, itemID, label string) error {
	localized, err := localizeImportedItemTx(tx, threadID, itemID, label)
	if err != nil || localized {
		return err
	}
	shadowed, err := shadowInheritedItemTx(tx, threadID, itemID)
	if err != nil {
		return fmt.Errorf("%s copy inherited item %s/%s: %w", label, threadID, itemID, err)
	}
	if !shadowed {
		return fmt.Errorf("%s %s/%s: %w", label, threadID, itemID, sql.ErrNoRows)
	}
	return nil
}
