package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// A paired computer's owned spawn links its thread here through the spawn's
// receipt (thread_request_receipts.spawn_link, migration v157): while the
// link stands and the thread is owned, the thread belongs to the source
// thread on that computer (SpawnOwnerOf) as a local worker belongs to its
// spawner. A thread has at most one link
// (idx_thread_request_receipts_spawn_link). A link ends when its owner's
// device loses access (RevokeDevice, MergeOwnDevices), when its owner has
// been silent past the retention floor (DeleteThreadRequestsBefore), or when
// its owner releases it (ReleaseSpawnLink); the triggers of v157 then
// return an owned thread of a project to standalone.

// remoteSpawnLinkSQL is true when the expression target names a thread a
// paired computer's link reaches.
func remoteSpawnLinkSQL(target string) string {
	return `EXISTS (SELECT 1 FROM thread_request_receipts AS rlink
	          WHERE rlink.target_thread_id = ` + target + ` AND rlink.spawn_link = 1)`
}

// spawnedByComputerSQL is the computer whose link reaches the thread target
// names, "" for none (Thread.SpawnedByComputerID).
func spawnedByComputerSQL(target string) string {
	return `COALESCE((SELECT rlink.source_computer_id FROM thread_request_receipts AS rlink
	          WHERE rlink.target_thread_id = ` + target + ` AND rlink.spawn_link = 1), '')`
}

// spawnLinkTouchGap is how stale a link's owner_seen_at may be before a call
// from its owner writes it again: often enough for a 30-day floor, rare
// enough that a poll every few seconds does not write each time.
const spawnLinkTouchGap = int64(60 * 60 * 1000)

// staleSpawnLinkSQL is a link whose owner has not reached it since the
// retention floor (both parameters), and that is older than the floor.
const staleSpawnLinkSQL = `spawn_link = 1 AND owner_seen_at < ? AND created_at < ?`

// touchSpawnLinkOwner records that the link's owner reached it at now: its
// own device, for its own source thread. It writes at most once per
// spawnLinkTouchGap and reports whether it wrote.
func (s *Store) touchSpawnLinkOwner(token, deviceID, sourceThreadID string, now int64) (bool, error) {
	action := fmt.Sprintf("store: note the owner of spawn link %s", token)
	result, err := s.db.Exec(`UPDATE thread_request_receipts SET owner_seen_at = ?1
		 WHERE token = ?2 AND owner_device_id = ?3 AND source_thread_id = ?4 AND spawn_link = 1
		   AND owner_seen_at < ?1 - ?5`, now, token, deviceID, sourceThreadID, spawnLinkTouchGap)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return rowsChanged(result, action)
}

// SpawnReach is a paired computer's spawn receipt as the device that made
// it reaches it for its source thread (ReachSpawnLink).
type SpawnReach struct {
	// TargetThreadID is the thread the spawn made, "" before it is named or
	// after it was deleted or moved away.
	TargetThreadID string
	// State is the receipt's state; an open one can still name a thread.
	State string
	// Linked is whether the spawn's link stands (spawn_link).
	Linked bool
	// Owned is whether the link owns its thread: it stands and the thread
	// is owned, not given a sidebar row or a group. Only then do the
	// thread's prompts route to the spawner (SpawnOwnerOf).
	Owned bool
	// OwnerOpSeq is the newest owner operation the receipt admitted.
	OwnerOpSeq int64
}

// Open reports whether the receipt can still name a thread.
func (r SpawnReach) Open() bool {
	return slices.Contains(threadReceiptOpenStates, r.State)
}

// ReachSpawnLink is the one reader that authenticates a paired computer's
// call about its spawn: the receipt of the spawn token names, made by
// deviceID for its thread sourceThreadID, the lineage the retention floor
// kept included, so a link past the ledger's floor still answers its owner.
// Found is false for any other token, device or source. A call that reaches
// a standing link is the owner's contact, whatever its thread's placement
// (touchSpawnLinkOwner).
func (s *Store) ReachSpawnLink(token, deviceID, sourceThreadID string, now int64) (SpawnReach, bool, error) {
	var reach SpawnReach
	err := s.reader().QueryRow(`SELECT COALESCE(r.target_thread_id, ''), r.state, r.spawn_link, r.owner_op_seq,
		    r.spawn_link = 1 AND EXISTS (SELECT 1 FROM threads WHERE threads.id = r.target_thread_id AND threads.placement = 'owned')
		  FROM thread_request_receipts AS r
		 WHERE r.token = ? AND r.owner_device_id = ? AND r.source_thread_id = ? AND r.kind = ?`,
		token, deviceID, sourceThreadID, ThreadRequestSpawn).Scan(&reach.TargetThreadID, &reach.State, &reach.Linked, &reach.OwnerOpSeq, &reach.Owned)
	if errors.Is(err, sql.ErrNoRows) {
		return SpawnReach{}, false, nil
	}
	if err != nil {
		return SpawnReach{}, false, fmt.Errorf("store: read spawn %s for its owner: %w", token, err)
	}
	if reach.Linked {
		if _, err := s.touchSpawnLinkOwner(token, deviceID, sourceThreadID, now); err != nil {
			return SpawnReach{}, false, err
		}
	}
	return reach, true, nil
}

// releaseSpawnLinksTx ends, in tx, every link the condition where (over
// thread_request_receipts, with args) selects, and returns the threads they
// named, sorted: the trigger has made each owned one standalone, so the
// caller announces their placement.
func releaseSpawnLinksTx(tx *sql.Tx, where string, args ...any) (released []string, err error) {
	rows, err := tx.Query(`UPDATE thread_request_receipts SET spawn_link = 0, updated_at = ?
		 WHERE spawn_link = 1 AND `+where+`
		 RETURNING COALESCE(target_thread_id, '')`, append([]any{nowMillis()}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store: release spawn links: %w", err)
	}
	defer closeAgentWakeRows(rows, "store: release spawn links", &err)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: release spawn links: scan: %w", err)
		}
		if id != "" && !slices.Contains(released, id) {
			released = append(released, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: release spawn links: %w", err)
	}
	slices.Sort(released)
	return released, nil
}

// ReleaseSpawnLink ends one link for its owner: its own device, for its own
// source thread. It returns the thread it named, none when nothing matched.
func (s *Store) ReleaseSpawnLink(token, deviceID, sourceThreadID string) (released []string, err error) {
	action := fmt.Sprintf("store: release spawn link %s", token)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	if released, err = releaseSpawnLinksTx(tx, `token = ? AND owner_device_id = ? AND source_thread_id = ?`,
		token, deviceID, sourceThreadID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: commit: %w", action, err)
	}
	return released, nil
}

// SupersedeLink records that the link's owner has sent operation seq, so
// an operation it sent before arrives as already applied. The sequence
// only rises. It reports whether the owner's link was found.
func (s *Store) SupersedeLink(token, deviceID, sourceThreadID string, seq int64) (bool, error) {
	action := fmt.Sprintf("store: supersede spawn link %s", token)
	var found bool
	err := s.db.QueryRow(`UPDATE thread_request_receipts SET owner_op_seq = MAX(owner_op_seq, ?1)
		 WHERE token = ?2 AND owner_device_id = ?3 AND source_thread_id = ?4 AND spawn_link = 1
		 RETURNING 1`, seq, token, deviceID, sourceThreadID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	return true, nil
}

// LinkFence names a link by its receipt's token and owning device: a delete
// admitted through it claims the thread only while that link still owns it
// (claimThreadDeleteTx).
type LinkFence struct {
	Token    string
	DeviceID string
}

// linkedBySQL holds for the threads row that the link the two parameters
// name (token, device) owns.
const linkedBySQL = `threads.placement = 'owned' AND EXISTS (SELECT 1 FROM thread_request_receipts AS rlink
	  WHERE rlink.target_thread_id = threads.id AND rlink.spawn_link = 1 AND rlink.token = ? AND rlink.owner_device_id = ?)`
