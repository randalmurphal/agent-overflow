package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Migration v156 records the caller's timeline row that carried a queued
// answer, and the late reply's apart from it, when the provider's echo
// confirms it (ConfirmThreadWake). The Agents segment and the tool rows read
// it from the request instead of finding the row by its send id on every
// read.
//
// A row confirmed before it gets the row the send-id lookup finds now, the
// lookup every read ran until this version: the caller's own user row
// carrying the wake's send id, alone or joined with other messages, in one
// statement per wake. Only the caller's own rows are looked in, which are
// the only rows the reads accept (wakeItemSQL): a request never moves to a
// fork, so no inherited row is its caller's, and imported history carries
// no send ids. One the lookup finds nothing for keeps none, as the read
// found none.
const wakeItemsMigrationVersion = 156

const wakeItemsV156SQL = `
ALTER TABLE thread_requests ADD COLUMN wake_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN late_wake_item_id TEXT NOT NULL DEFAULT '';
`

// recordConfirmedWakeItems fills the two columns for the queued answers and
// late replies already confirmed, by the send ids their wakes carried: each
// request probes its caller's row by the send id (idx_items_send_id), then
// every joined row is read once and names the requests it carries
// (idx_items_joined_send_ids), which a probe per request would read once
// per request of its thread.
func recordConfirmedWakeItems(tx *sql.Tx) error {
	for _, wake := range []struct{ column, route, confirmed, sendID string }{
		{"wake_item_id", "delivered_how", "confirmed_at", "thread-wake:"},
		{"late_wake_item_id", "late_delivered_how", "late_confirmed_at", "thread-wake-late:"},
	} {
		prefix := sqlTextLiteral(wake.sendID)
		confirmed := `r.lineage_only = 0 AND r.` + wake.route + ` = 'queued' AND r.` + wake.confirmed + ` IS NOT NULL`
		if _, err := tx.Exec(`UPDATE thread_requests AS r SET ` + wake.column + ` = COALESCE((
			SELECT i.id FROM items i
			 WHERE i.thread_id = r.caller_thread_id AND i.kind = 'user_text' AND i.parent_id = '' AND json_valid(i.meta)
			   AND json_extract(i.meta, '$.sendId') IS NOT NULL AND json_extract(i.meta, '$.sendId') = ` + prefix + ` || r.token
			   AND COALESCE(json_extract(i.meta, '$.wire_only'), 0) != 1
			 LIMIT 1), '')
			WHERE ` + confirmed); err != nil {
			return fmt.Errorf("record the rows of the confirmed wakes: %w", err)
		}
		if err := recordJoinedWakeItems(tx, wake.column, prefix, confirmed); err != nil {
			return err
		}
	}
	return nil
}

// recordJoinedWakeItems names, for each joined row, the confirmed requests
// still without a row whose wakes it carries: a read of the joined rows,
// then one keyed write per wake found.
func recordJoinedWakeItems(tx *sql.Tx, column, prefix, confirmed string) (err error) {
	type carried struct{ id, threadID, token string }
	rows, err := tx.Query(`SELECT i.id, i.thread_id, substr(j.value, length(` + prefix + `) + 1)
		  FROM items i CROSS JOIN json_each(i.meta, '$.joinedSendIds') j
		 WHERE i.kind = 'user_text' AND i.parent_id = '' AND json_valid(i.meta)
		   AND json_extract(i.meta, '$.joinedSendIds') IS NOT NULL
		   AND COALESCE(json_extract(i.meta, '$.wire_only'), 0) != 1
		   AND substr(j.value, 1, length(` + prefix + `)) = ` + prefix)
	if err != nil {
		return fmt.Errorf("list the joined rows of the confirmed wakes: %w", err)
	}
	var found []carried
	for rows.Next() {
		var c carried
		if err := rows.Scan(&c.id, &c.threadID, &c.token); err != nil {
			return errors.Join(fmt.Errorf("scan a joined row: %w", err), rows.Close())
		}
		found = append(found, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("list the joined rows of the confirmed wakes: %w", err)
	}
	update, err := tx.Prepare(`UPDATE thread_requests AS r SET ` + column + ` = ?1
		WHERE r.token = ?2 AND r.caller_thread_id = ?3 AND r.` + column + ` = '' AND ` + confirmed)
	if err != nil {
		return fmt.Errorf("prepare the joined wake write: %w", err)
	}
	defer func() { err = errors.Join(err, update.Close()) }()
	for _, c := range found {
		if _, err := update.Exec(c.id, c.token, c.threadID); err != nil {
			return fmt.Errorf("record the joined row of %s: %w", c.token, err)
		}
	}
	return nil
}
