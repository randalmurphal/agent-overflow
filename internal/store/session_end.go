package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
)

// A worker is owned only while its owner's device can reach this computer:
// a device reaches it while it holds an unrevoked session here. So every
// store write that ends sessions, by revocation or by deletion, ends them
// through endSessionsTx, which releases in the same transaction the spawn
// links of every device left with no unrevoked session (the placement
// trigger makes their workers standalone) and returns the released threads
// for announcing. TestEverySessionEndGoesThroughEndSessionsTx holds every
// other store function to it, cascading deletes of a session's parent row
// included.
//
// Expiry alone ends no reach: an expired session renews through its refresh
// secret (ExtendSession). It ends when its refresh window closes, and the
// credential prune deletes it through endSessionsTx; a link of a device
// that stops calling is released by the owner_seen_at floor besides.

// sessionEnd is how endSessionsTx ends the sessions it selects.
type sessionEnd int

const (
	// revokeSessions stamps revoked_at on the unrevoked ones.
	revokeSessions sessionEnd = iota
	// deleteSessions deletes them, revoked or not.
	deleteSessions
)

// endSessionsTx ends, in tx, the sessions the condition where (over
// sessions, with args) selects: revoked at at, or deleted. It then releases
// the links of their devices, and of devices, that no unrevoked session
// pairs with this computer any longer. It returns the ended sessions and the
// released threads, sorted. The nudge runs kept for those devices' links,
// which no device left can take, go with them. where is a constant per call
// site, so each statement compiles once.
func endSessionsTx(tx *sql.Tx, end sessionEnd, at int64, devices []string, where string, args ...any) (ids, released []string, err error) {
	var rows *sql.Rows
	switch end {
	case revokeSessions:
		rows, err = tx.Query(`UPDATE sessions SET revoked_at = ? WHERE revoked_at IS NULL AND (`+where+`)
			 RETURNING id, device_id`, append([]any{at}, args...)...)
	case deleteSessions:
		rows, err = tx.Query(`DELETE FROM sessions WHERE `+where+`
			 RETURNING id, device_id`, args...)
	default:
		return nil, nil, fmt.Errorf("store: end sessions: unknown end %d", end)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: end sessions: %w", err)
	}
	ids, devices, err = scanEndedSessions(rows, devices)
	if err != nil {
		return nil, nil, err
	}
	if len(devices) == 0 {
		return ids, nil, nil
	}
	encoded, err := json.Marshal(devices)
	if err != nil {
		return nil, nil, fmt.Errorf("store: end sessions: %w", err)
	}
	if released, err = releaseSpawnLinksTx(tx, unreachableDeviceLinksSQL, string(encoded)); err != nil {
		return nil, nil, err
	}
	for _, statement := range []string{
		`UPDATE thread_request_receipts SET track_spawn = 0 WHERE track_spawn = 1 AND ` + unreachableDeviceLinksSQL,
		`DELETE FROM thread_receipt_nudges WHERE link_token IN (SELECT token FROM thread_request_receipts WHERE ` + unreachableDeviceLinksSQL + `)`,
		`DELETE FROM spawn_revisions WHERE owner_device_id IN (SELECT value FROM json_each(?))
		  AND NOT EXISTS (SELECT 1 FROM sessions WHERE device_id = spawn_revisions.owner_device_id AND revoked_at IS NULL)`,
	} {
		if _, err := tx.Exec(statement, string(encoded)); err != nil {
			return nil, nil, fmt.Errorf("store: forget spawn changes for ended sessions: %w", err)
		}
	}
	return ids, released, nil
}

// scanEndedSessions reads an ending statement's (id, device_id) rows and
// adds each device to devices once.
func scanEndedSessions(rows *sql.Rows, devices []string) (ids, _ []string, err error) {
	defer closeAgentWakeRows(rows, "store: end sessions", &err)
	ids = []string{}
	for rows.Next() {
		var id, device string
		if err := rows.Scan(&id, &device); err != nil {
			return nil, nil, fmt.Errorf("store: end sessions: scan: %w", err)
		}
		ids = append(ids, id)
		if !slices.Contains(devices, device) {
			devices = append(devices, device)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: end sessions: %w", err)
	}
	return ids, devices, nil
}

// unreachableDeviceLinksSQL selects, over thread_request_receipts, the links
// owned by the devices the JSON array lists that hold no unrevoked session.
const unreachableDeviceLinksSQL = `owner_device_id IN (SELECT value FROM json_each(?))
	AND NOT EXISTS (SELECT 1 FROM sessions live
	  WHERE live.device_id = thread_request_receipts.owner_device_id AND live.revoked_at IS NULL)`
