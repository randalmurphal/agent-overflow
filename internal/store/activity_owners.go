package store

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type ActivityOwner struct {
	Device string
	Token  string
}

// ActivityOwners resolves changed requests and threads to the foreign
// receipts that read them, including the lineage of settled spawns.
func (s *Store) ActivityOwners(tokens, threads []string) (_ []ActivityOwner, err error) {
	if len(tokens)+len(threads) == 0 {
		return nil, nil
	}
	keys, err := json.Marshal(tokens)
	if err != nil {
		return nil, err
	}
	targets, err := json.Marshal(threads)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(activityOwnersSQL+` ORDER BY 1,2`, string(keys), string(targets))
	if err != nil {
		return nil, fmt.Errorf("store: find owners of changed activity: %w", err)
	}
	defer closeAgentWakeRows(rows, "find owners of changed activity", &err)
	var out []ActivityOwner
	for rows.Next() {
		var owner ActivityOwner
		if err := rows.Scan(&owner.Device, &owner.Token); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}

var activityOwnersSQL = `WITH RECURSIVE
	  changed(token) AS (SELECT value FROM json_each(?1) WHERE value IS NOT NULL),
	  direct(id) AS (
	    SELECT value FROM json_each(?2) WHERE value IS NOT NULL
	    UNION SELECT r.caller_thread_id FROM changed c JOIN thread_requests r ON r.token = c.token
	  ), ancestors(id) AS (
	    SELECT id FROM direct UNION
	    SELECT p.caller_thread_id FROM ancestors a JOIN thread_requests p ON p.target_thread_id = a.id
	     WHERE ` + agentSpawnLinkSQL("p") + `
	  )
	  SELECT r.owner_device_id AS device, r.token AS token FROM changed c JOIN thread_request_receipts r ON r.token = c.token
	    WHERE r.owner_device_id <> 'local'
	  UNION SELECT r.owner_device_id, r.token FROM ancestors a JOIN thread_request_receipts r ON r.target_thread_id = a.id
	    WHERE r.kind = 'spawn' AND r.owner_device_id <> 'local'
	  UNION SELECT r.owner_device_id, r.token FROM direct d JOIN thread_request_receipts r ON r.target_thread_id = d.id
	    WHERE r.kind IN ('send','ask') AND r.state IN ('accepted','running') AND r.owner_device_id <> 'local'
	  UNION SELECT c.owner_device_id,c.token FROM direct d JOIN spawn_revisions c ON c.target=d.id`

// RecoveryOwnerTokens pages one group of active threads by receipt token,
// including a busy target with more owners' requests than fit one reply.
func (s *Store) RecoveryOwnerTokens(device string, threads []string, after string, limit int) ([]string, error) {
	if limit < 1 {
		return nil, fmt.Errorf("positive owner page size required")
	}
	ids, err := json.Marshal(threads)
	if err != nil {
		return nil, err
	}
	return s.queryThreadIDs("page active owner tokens", `SELECT token FROM (`+activityOwnersSQL+`)
	 WHERE device=?3 AND token>?4 ORDER BY token LIMIT `+strconv.Itoa(limit), "[]", string(ids), device, after)
}
