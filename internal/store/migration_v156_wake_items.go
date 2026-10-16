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
// lookup every read ran until this version; one it finds nothing for keeps
// none, as the read found none.
const wakeItemsMigrationVersion = 156

const wakeItemsV156SQL = `
ALTER TABLE thread_requests ADD COLUMN wake_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE thread_requests ADD COLUMN late_wake_item_id TEXT NOT NULL DEFAULT '';
`

// recordConfirmedWakeItems fills the two columns for the queued answers and
// late replies already confirmed, by the send ids their wakes carried.
func recordConfirmedWakeItems(tx *sql.Tx) error {
	for _, wake := range []struct{ column, route, confirmed, sendID string }{
		{"wake_item_id", "delivered_how", "confirmed_at", "thread-wake:"},
		{"late_wake_item_id", "late_delivered_how", "late_confirmed_at", "thread-wake-late:"},
	} {
		type confirmed struct{ token, caller string }
		var found []confirmed
		rows, err := tx.Query(`SELECT token, caller_thread_id FROM thread_requests
			WHERE lineage_only = 0 AND ` + wake.route + ` = 'queued' AND ` + wake.confirmed + ` IS NOT NULL`)
		if err != nil {
			return fmt.Errorf("list the confirmed wakes: %w", err)
		}
		for rows.Next() {
			var row confirmed
			if err := rows.Scan(&row.token, &row.caller); err != nil {
				return errors.Join(fmt.Errorf("scan a confirmed wake: %w", err), rows.Close())
			}
			found = append(found, row)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("list the confirmed wakes: %w", err)
		}
		for _, row := range found {
			id, err := wakeItemBySendID(tx, row.caller, wake.sendID+row.token)
			if err != nil {
				return err
			}
			if id == "" {
				continue
			}
			if _, err := tx.Exec(`UPDATE thread_requests SET `+wake.column+` = ? WHERE token = ?`, id, row.token); err != nil {
				return fmt.Errorf("record the wake row of %s: %w", row.token, err)
			}
		}
	}
	return nil
}

// wakeItemBySendID is the user row of threadID that carries sendID, alone
// or joined with other messages, or "".
func wakeItemBySendID(tx *sql.Tx, threadID, sendID string) (string, error) {
	for _, lookup := range []func(sqlQueryer, string, string) (string, []any, error){sendIdentityQuery, joinedSendIdentityQuery} {
		query, args, err := lookup(tx, threadID, sendID)
		if err != nil {
			return "", err
		}
		var id string
		err = tx.QueryRow(query, args...).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("find the row of %s in %s: %w", sendID, threadID, err)
		}
	}
	return "", nil
}
