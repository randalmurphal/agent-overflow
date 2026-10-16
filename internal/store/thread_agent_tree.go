package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// The Agents segment's reads of the request ledger
// (docs/specs/standing-agents.md, "Agents segment"). Each is one statement
// whatever the size of the tree: the listing, one row of it, the tool rows'
// states and the callers a change frame names.

// ThreadAgentRequest is a request as the Agents segment lists it. Its
// ThreadRequest carries neither the answer nor the late reply, which a row
// never shows whole: AnswerHead is the start of the newer of the two.
type ThreadAgentRequest struct {
	ThreadRequest
	AnswerHead []byte
	// HasLateReply is whether a reply arrived after the request settled.
	HasLateReply bool
	// WakeItemID is the caller's timeline row that carried the answer, as
	// ConfirmThreadWake recorded it, while the caller's timeline still holds
	// it: the late reply's, else the first answer's. Empty for an answer a
	// call collected inline, which leaves no row.
	WakeItemID string
	// Depth is how many spawns down from the listed thread the request's
	// caller is.
	Depth int
	// NestedHidden marks a local spawn at the depth bound whose thread has
	// rows of its own the listing does not show.
	NestedHidden bool
}

// ThreadAgentTreeRead is one read of a thread's Agents rows.
type ThreadAgentTreeRead struct {
	// Root is the listed thread.
	Root string
	// Waiting names the requests a tool call is parked on.
	Waiting []string
	// SettledSince keeps a request settled at or after it.
	SettledSince int64
	// Depth bounds the nesting: rows at Depth-1 nest nothing further.
	Depth int
	// OpenDepth is how many levels below its caller open work keeps a
	// settled local spawn listed, whatever the listing's Depth.
	OpenDepth int
	// Limit bounds the rows; the read says whether there were more.
	Limit int
	// AnswerHead is how many bytes of the answer each row carries.
	AnswerHead int
	// Token, when set, reads only that one of Root's own rows.
	Token string
}

// ListThreadAgentTree returns the spawns, sends and asks Root made that its
// activity rail lists as agents, then under each local spawn those its
// thread made, in tree order, oldest first among siblings: every spawn, and
// a send or ask whose answer its caller is still owed a wake for (`notify`)
// or is parked waiting on (one of Waiting). Each is open, settled at or
// after SettledSince, or a spawn something under it is still open in: a
// local spawn whose thread, or a thread under it through local spawns
// within OpenDepth levels of the spawn's caller, has a request open or a
// remote spawn whose computer last reported open work; or a remote spawn
// whose computer last reported open work (`nested_open`). A thread already
// on a row's path is not walked again. At most Limit rows, and whether
// there were more.
func (s *Store) ListThreadAgentTree(read ThreadAgentTreeRead) (_ []ThreadAgentRequest, more bool, err error) {
	action := "store: list the agents of " + read.Root
	if read.Limit < 1 || read.Depth < 1 || read.OpenDepth < 1 || read.AnswerHead < 0 {
		return nil, false, fmt.Errorf("%s: limit %d, depth %d, open depth %d and answer head %d",
			action, read.Limit, read.Depth, read.OpenDepth, read.AnswerHead)
	}
	waiting := read.Waiting
	if waiting == nil {
		waiting = []string{}
	}
	tokens, err := json.Marshal(waiting)
	if err != nil {
		return nil, false, fmt.Errorf("%s: encode waiting tokens: %w", action, err)
	}
	query := threadAgentTreeSQL(false, read.Limit+1)
	args := []any{read.Root, string(tokens), read.SettledSince, read.Depth, read.OpenDepth, read.AnswerHead}
	if read.Token != "" {
		query = threadAgentTreeSQL(true, read.Limit+1)
		args = append(args, read.Token)
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out := []ThreadAgentRequest{}
	for rows.Next() {
		var row ThreadAgentRequest
		var late, hidden int
		row.ThreadRequest, err = scanThreadRequestWith(rows, &row.AnswerHead, &late, &row.WakeItemID, &row.Depth, &hidden)
		if err != nil {
			return nil, false, fmt.Errorf("%s: scan: %w", action, err)
		}
		row.HasLateReply, row.NestedHidden = late != 0, hidden != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("%s: iterate: %w", action, err)
	}
	if len(out) > read.Limit {
		return out[:read.Limit], true, nil
	}
	return out, false, nil
}

// threadAgentTreeSQL is ListThreadAgentTree's read. ?1 is the root, ?2 the
// waiting tokens, ?3 the settled window, ?4 the depth, ?5 the open depth, ?6
// the answer head and ?7, for one row, its token.
//
// `sub` walks the root's local spawns as deep as any listed row's open work
// can sit; `busy` is each thread there with work open in it; `hot` walks up
// from those, one step per spawn, the open depth less one: a settled local
// spawn whose thread is in it is kept. `tree` walks the listed rows in
// order, its sort key each level's creation time and token, carrying the
// threads above each row so a cycle ends. Only the rows inside the limit
// are projected: a row's answer head is read for those alone.
func threadAgentTreeSQL(oneToken bool, limit int) string {
	token := ""
	if oneToken {
		token = " AND r.token = ?7"
	}
	return `WITH RECURSIVE
  sub(thread_id, depth) AS (
    SELECT ?1, 0
    UNION
    SELECT r.target_thread_id, sub.depth + 1
      FROM sub CROSS JOIN thread_requests r
     WHERE r.caller_thread_id = sub.thread_id AND r.kind = 'spawn' AND r.target_computer_id = ''
       AND r.target_thread_id <> '' AND r.lineage_only = 0 AND sub.depth < ?4 + ?5),
  busy(thread_id) AS (
    SELECT DISTINCT sub.thread_id FROM sub
     WHERE EXISTS (SELECT 1 FROM thread_requests c
                    WHERE c.caller_thread_id = sub.thread_id AND (` + openAgentRequestSQL("c") + `
                      OR (c.kind = 'spawn' AND c.target_computer_id <> '' AND c.lineage_only = 0 AND c.nested_open = 1)))),
  hot(thread_id, dist) AS (
    SELECT thread_id, 0 FROM busy
    UNION
    SELECT r.caller_thread_id, hot.dist + 1
      FROM hot CROSS JOIN thread_requests r
     WHERE r.target_thread_id = hot.thread_id AND r.kind = 'spawn' AND r.target_computer_id = ''
       AND r.lineage_only = 0 AND hot.dist + 1 < ?5),
  tree(t_token, t_depth, t_sort, t_path, t_kind, t_computer, t_target) AS (
    SELECT r.token, 0, printf('%020d', r.created_at) || r.token || char(1), json_array(?1),
           r.kind, r.target_computer_id, r.target_thread_id
      FROM thread_requests r
     WHERE r.caller_thread_id = ?1` + token + ` AND ` + listedAgentRequestSQL("r") + `
    UNION ALL
    SELECT r.token, tree.t_depth + 1, tree.t_sort || printf('%020d', r.created_at) || r.token || char(1),
           json_insert(tree.t_path, '$[#]', tree.t_target), r.kind, r.target_computer_id, r.target_thread_id
      FROM tree CROSS JOIN thread_requests r
     WHERE tree.t_kind = 'spawn' AND tree.t_computer = '' AND tree.t_target <> '' AND tree.t_depth + 1 < ?4
       AND NOT EXISTS (SELECT 1 FROM json_each(tree.t_path) WHERE json_each.value = tree.t_target)
       AND r.caller_thread_id = tree.t_target AND ` + listedAgentRequestSQL("r") + `)
SELECT ` + threadAgentRequestColumns + `, tree.t_depth,
       CASE WHEN tree.t_depth = ?4 - 1 AND tree.t_kind = 'spawn' AND tree.t_computer = '' AND tree.t_target <> ''
                 AND NOT EXISTS (SELECT 1 FROM json_each(tree.t_path) WHERE json_each.value = tree.t_target)
            THEN EXISTS (SELECT 1 FROM thread_requests c
                          WHERE c.caller_thread_id = tree.t_target AND ` + listedAgentRequestSQL("c") + `)
            ELSE 0 END
  FROM (SELECT * FROM tree ORDER BY t_sort LIMIT ` + strconv.Itoa(limit) + `) tree CROSS JOIN thread_requests r
 WHERE r.token = tree.t_token
 ORDER BY tree.t_sort`
}

// listedAgentRequestSQL matches a request of the listing (see
// ListThreadAgentTree), reading ?2, ?3 and the `hot` walk.
func listedAgentRequestSQL(alias string) string {
	a := alias + "."
	return `(` + a + `lineage_only = 0 AND ` + a + `kind IN ('spawn','send','ask')
       AND (` + a + `kind = 'spawn' OR ` + a + `notify = 1 OR ` + a + `token IN (SELECT value FROM json_each(?2)))
       AND (` + a + `settled_at IS NULL OR ` + a + `settled_at >= ?3
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id = '' AND ` + a + `target_thread_id IN (SELECT thread_id FROM hot))
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1)))`
}

// threadAgentRequestColumns is threadRequestColumns for alias r, with the
// first ?6 bytes of the newer answer in place of the answer, whether a late
// reply arrived in place of it, and the wake row after them.
const threadAgentRequestColumns = `r.token, r.caller_thread_id, r.kind, COALESCE(r.due_at, 0),
    r.target_computer_id, r.target_thread_id, r.target_thread_title, r.origin_thread_id, r.notify, r.state,
    substr(COALESCE(r.late_reply, r.answer), 1, ?6), r.answer_kind, r.late_reply IS NOT NULL, COALESCE(r.late_reply_at, 0), r.revision,
    COALESCE(r.settled_at, 0), COALESCE(r.delivered_at, 0), r.delivered_how,
    COALESCE(r.late_delivered_at, 0), COALESCE(r.expires_at, 0),
    r.polling, r.next_check, r.attempts, r.issue,
    r.wake_attempts, r.wake_next_check, r.wake_issue, r.lineage_only, r.created_at, r.updated_at,
    COALESCE(r.confirmed_at, 0), COALESCE(r.late_confirmed_at, 0), r.answer_agent, r.answer_agent_role,
    r.target_provider, r.nested_open, r.late_delivered_how, r.made_in_thread_id, ` + wakeItemSQL

// wakeItemSQL is the caller's row that carried request r's answer: the late
// reply's confirmed wake, else the first answer's, each while the caller's
// own rows still hold it. A revert that removed the row, or New Thread,
// Same Assistant moving the request to a thread the row is not in, leaves
// none.
const wakeItemSQL = `CASE
      WHEN r.late_delivered_how = 'queued' AND r.late_confirmed_at IS NOT NULL AND r.late_wake_item_id <> ''
       AND EXISTS (SELECT 1 FROM items w WHERE w.thread_id = r.caller_thread_id AND w.id = r.late_wake_item_id)
      THEN r.late_wake_item_id
      WHEN r.delivered_how = 'queued' AND r.confirmed_at IS NOT NULL AND r.wake_item_id <> ''
       AND EXISTS (SELECT 1 FROM items w WHERE w.thread_id = r.caller_thread_id AND w.id = r.wake_item_id)
      THEN r.wake_item_id
      ELSE '' END`

// ThreadRequestDelivery is how a request's answer reached its caller.
type ThreadRequestDelivery struct {
	Token            string
	State            string
	DeliveredHow     string
	DeliveredAt      int64
	LateDeliveredHow string
	LateDeliveredAt  int64
	// WakeItemID is as ThreadAgentRequest's.
	WakeItemID string
}

// ThreadRequestDeliveries reads how the answers of the requests among
// tokens that callerThreadID made reached it, in the order of tokens. A
// token it did not make, or one past the retention floor, is left out.
func (s *Store) ThreadRequestDeliveries(callerThreadID string, tokens []string) (_ []ThreadRequestDelivery, err error) {
	action := "store: read the deliveries of " + callerThreadID
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("%s: encode tokens: %w", action, err)
	}
	rows, err := s.reader().Query(`SELECT r.token, r.state, r.delivered_how, COALESCE(r.delivered_at, 0),
		       r.late_delivered_how, COALESCE(r.late_delivered_at, 0), `+wakeItemSQL+`
		  FROM json_each(?1) j CROSS JOIN thread_requests r
		 WHERE r.token = j.value AND r.caller_thread_id = ?2 AND r.lineage_only = 0
		 ORDER BY j.key`, string(encoded), callerThreadID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out := []ThreadRequestDelivery{}
	for rows.Next() {
		var d ThreadRequestDelivery
		if err := rows.Scan(&d.Token, &d.State, &d.DeliveredHow, &d.DeliveredAt, &d.LateDeliveredHow, &d.LateDeliveredAt, &d.WakeItemID); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", action, err)
	}
	return out, nil
}

// ThreadAgentRowCaller is a request a change frame names, and its caller.
type ThreadAgentRowCaller struct {
	Token          string
	CallerThreadID string
}

// ThreadAgentRowCallers names the caller of each request among tokens, and
// of each request this computer's threads made (the receipts owner names)
// that is accepted or running in one of targets. Reminders and requests
// past the retention floor are left out.
func (s *Store) ThreadAgentRowCallers(tokens, targets []string, owner string) (_ []ThreadAgentRowCaller, err error) {
	action := "store: name the callers of changed agent rows"
	if tokens == nil {
		tokens = []string{}
	}
	if targets == nil {
		targets = []string{}
	}
	encodedTokens, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("%s: encode tokens: %w", action, err)
	}
	encodedTargets, err := json.Marshal(targets)
	if err != nil {
		return nil, fmt.Errorf("%s: encode targets: %w", action, err)
	}
	rows, err := s.reader().Query(`SELECT r.token, r.caller_thread_id FROM thread_requests r
		 WHERE r.token IN (SELECT value FROM json_each(?1)
		                   UNION SELECT rc.token FROM json_each(?2) t CROSS JOIN thread_request_receipts rc
		                          WHERE rc.target_thread_id = t.value AND rc.owner_device_id = ?3
		                            AND rc.state IN ('accepted','running'))
		   AND r.lineage_only = 0 AND r.kind <> 'remind'`, string(encodedTokens), string(encodedTargets), owner)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out := []ThreadAgentRowCaller{}
	for rows.Next() {
		var c ThreadAgentRowCaller
		if err := rows.Scan(&c.Token, &c.CallerThreadID); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", action, err)
	}
	return out, nil
}
