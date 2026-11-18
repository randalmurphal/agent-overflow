package store

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ThreadAgentRowFrame is one change frame: the request Token, or every
// request when Token is empty, of a thread ThreadID lists.
type ThreadAgentRowFrame struct {
	Token    string
	ThreadID string
}

// ThreadAgentRowFrames names the frames a batch of changes sends, in one
// statement: each request among tokens, each request this computer's
// threads made (the receipts owner names) that is accepted or running in
// one of targets, and the spawn link of each of targets
// (idx_thread_requests_spawn_link), whose row shows that thread's live
// state, framed for its caller; each of callers framed for itself with no
// token; every link above each of those up to the root, framed for its
// caller, since whether its thread is at rest follows the work below it;
// and each of those frames again for every thread up the local spawns
// above it whose listing nests it, fewer than depth spawns up. Reminders
// and requests past the retention floor are left out; a link the floor
// reduced to its lineage frames its caller with no token.
//
// The same statement names rest: the caller of each of tokens that is a
// worker, spawned here (its spawn link) or by a paired computer (a spawn
// receipt with no request here). An answer on its way to such a caller
// keeps it from rest (ThreadAnswerDue), so a changed request is a change
// to its caller's durable sources.
func (s *Store) ThreadAgentRowFrames(tokens, targets, callers []string, owner string, depth int) (_ []ThreadAgentRowFrame, rest []string, err error) {
	action := "store: name the frames of changed agent rows"
	encoded := make([]string, 0, 3)
	for _, list := range [][]string{tokens, targets, callers} {
		if list == nil {
			list = []string{}
		}
		value, err := json.Marshal(list)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: encode: %w", action, err)
		}
		encoded = append(encoded, string(value))
	}
	rows, err := s.reader().Query(threadAgentRowFramesSQL, encoded[0], encoded[1], owner, encoded[2], depth)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out := []ThreadAgentRowFrame{}
	for rows.Next() {
		var f ThreadAgentRowFrame
		var isRest bool
		if err := rows.Scan(&f.Token, &f.ThreadID, &isRest); err != nil {
			return nil, nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		if isRest {
			rest = append(rest, f.ThreadID)
			continue
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s: iterate: %w", action, err)
	}
	return out, rest, nil
}

// threadAgentRowFramesSQL is ThreadAgentRowFrames' read: ?1 the tokens, ?2
// the targets, ?3 the receipts' owner, ?4 the callers, ?5 the depth.
// `chain` climbs one local spawn a step from each named frame's thread to
// the root, keyed by the queued thread; a link has one caller, so each
// enters once and a cycle ends. `up` frames each again up to the depth.
// The cost is one search per link above the named threads, and the depth
// per link. The last arm names rest, one indexed probe per token's caller
// (idx_thread_requests_spawn_link, then the receipts by target).
var threadAgentRowFramesSQL = `WITH RECURSIVE
  named(token, thread) AS (
    SELECT r.token, r.caller_thread_id FROM thread_requests r
     WHERE r.token IN (SELECT value FROM json_each(?1)
                       UNION SELECT rc.token FROM json_each(?2) CROSS JOIN thread_request_receipts rc
                              WHERE rc.target_thread_id = json_each.value AND rc.owner_device_id = ?3
                                AND rc.state IN ('accepted','running'))
       AND r.lineage_only = 0 AND r.kind <> 'remind'
    UNION SELECT CASE WHEN link.lineage_only = 0 THEN link.token ELSE '' END, link.caller_thread_id
      FROM json_each(?2) CROSS JOIN thread_requests link
     WHERE link.target_thread_id = json_each.value AND ` + agentSpawnLinkSQL("link") + `
    UNION SELECT '', value FROM json_each(?4)),
  chain(token, thread) AS (
    SELECT token, thread FROM named
    UNION
    SELECT CASE WHEN link.lineage_only = 0 THEN link.token ELSE '' END, link.caller_thread_id
      FROM chain CROSS JOIN thread_requests link
     WHERE link.target_thread_id = chain.thread AND ` + agentSpawnLinkSQL("link") + `),
  up(token, thread, depth) AS (
    SELECT token, thread, 0 FROM chain
    UNION
    SELECT up.token, sp.caller_thread_id, up.depth + 1
      FROM up CROSS JOIN thread_requests sp
     WHERE up.depth + 1 < ?5 AND ` + agentSpawnLinkSQL("sp") + ` AND sp.target_thread_id = up.thread)
SELECT DISTINCT token, thread, 0 FROM up
UNION ALL
SELECT DISTINCT '', r.caller_thread_id, 1 FROM json_each(?1) CROSS JOIN thread_requests r
 WHERE r.token = json_each.value AND r.lineage_only = 0
   AND (EXISTS (SELECT 1 FROM thread_requests link
                 WHERE link.target_thread_id = r.caller_thread_id AND ` + agentSpawnLinkSQL("link") + `)
        OR EXISTS (SELECT 1 FROM thread_request_receipts rc
                    WHERE rc.target_thread_id = r.caller_thread_id AND rc.kind = 'spawn'
                      AND NOT EXISTS (SELECT 1 FROM thread_requests own WHERE own.token = rc.token)))`
