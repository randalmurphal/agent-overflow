package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The Agents segment's reads of the request ledger
// (docs/specs/standing-agents.md, "Agents segment"). Each is one statement
// whose cost follows the rows it lists and the tree's depth: the listing,
// named rows of it, the tool rows' states and the callers a change frame
// names.

// agentOpenDepth is how many spawns below a settled local spawn's target
// open work keeps it listed, the depth its marks count
// (open_below_0 to open_below_3, migration v155).
const agentOpenDepth = 4

// agentRootSQL and agentWindowSQL are the tree reads' root (?1) and settled
// window (?3) as expressions rather than bare parameters: with statistics
// the planner reads a bare parameter's bound value to weigh an index on the
// column it is compared with, and a plan that read one compiles again on
// every rebind (TestThreadAgentTreeCompilesOnceWithStatistics).
const (
	agentRootSQL   = `CAST(?1 AS TEXT)`
	agentWindowSQL = `CAST(?3 AS INTEGER)`
)

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
	// TargetOpen is whether the thread a spawn made has open work at any
	// depth below it, as agentTargetOpenSQL reads it: the one answer the
	// listing's membership, the row's state and Past share.
	TargetOpen bool
}

// ThreadAgentTreeRead is one read of a thread's Agents rows.
type ThreadAgentTreeRead struct {
	// Root is the listed thread.
	Root string
	// Waiting names the requests a tool call is parked on.
	Waiting []string
	// Live names the threads on this computer that are not at rest: a
	// local spawn that reaches one is open while it is.
	Live []string
	// SettledSince keeps a request settled at or after it.
	SettledSince int64
	// Depth bounds the nesting: rows at Depth-1 nest nothing further.
	Depth int
	// Limit bounds the rows; the read says whether there were more.
	Limit int
	// AnswerHead is how many bytes of the answer each row carries.
	AnswerHead int
	// Tokens, when set, reads only those of the listing's rows, each where
	// the listing places it, wherever it falls against Limit.
	Tokens []string
}

// ListThreadAgentTree returns the spawns, sends and asks Root made that its
// activity rail lists as agents, then under each local spawn those its
// thread made, in tree order, oldest first among siblings: every spawn, and
// a send or ask whose answer its caller is still owed a wake for (`notify`)
// or is parked waiting on (one of Waiting). Each is open, settled at or
// after SettledSince, or a spawn whose thread is not at rest
// (agentTargetOpenSQL): a local spawn with open work at any depth below its
// target, its own thread included (its marks, a parked request, a thread
// among Live, or stored work deeper than the marks count), or a remote
// spawn whose computer last reported open work or its thread live
// (`nested_open`). A thread is reached by at most one local spawn
// (idx_thread_requests_spawn_link), so each request is listed once, under
// the chain of spawns above its caller. A local spawn at the depth bound
// nests nothing. At most Limit rows, and whether there were more.
func (s *Store) ListThreadAgentTree(read ThreadAgentTreeRead) (_ []ThreadAgentRequest, more bool, err error) {
	action := "store: list the agents of " + read.Root
	if read.Limit < 1 || read.Depth < 1 || read.AnswerHead < 0 {
		return nil, false, fmt.Errorf("%s: limit %d, depth %d and answer head %d", action, read.Limit, read.Depth, read.AnswerHead)
	}
	held, err := agentHeldParam(read.Waiting, read.Live)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	query, args := threadAgentTreeSQL(read.Limit+1), []any{read.Root, held, read.SettledSince, read.Depth, read.AnswerHead}
	if read.Tokens != nil {
		named, err := json.Marshal(read.Tokens)
		if err != nil {
			return nil, false, fmt.Errorf("%s: encode tokens: %w", action, err)
		}
		query, args = threadAgentNamedRowsSQL, append(args, string(named))
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out := []ThreadAgentRequest{}
	for rows.Next() {
		var row ThreadAgentRequest
		var late, hidden, open int
		row.ThreadRequest, err = scanThreadRequestWith(rows, &row.AnswerHead, &late, &row.WakeItemID, &row.Depth, &hidden, &open)
		if err != nil {
			return nil, false, fmt.Errorf("%s: scan: %w", action, err)
		}
		row.HasLateReply, row.NestedHidden, row.TargetOpen = late != 0, hidden != 0, open != 0
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("%s: iterate: %w", action, err)
	}
	if read.Tokens == nil && len(out) > read.Limit {
		return out[:read.Limit], true, nil
	}
	return out, false, nil
}

// threadAgentTreeSQL is the listing's read of its first limit rows. ?1 is
// the root, ?2 the held work (agentHeldParam), ?3 the settled window, ?4 the depth and
// ?5 the answer head.
//
// `tree` walks the listed rows in tree order, its sort key each level's
// creation time and token, carrying the threads above each row so a cycle
// ends. Its queue is taken in sort order and stops at limit rows
// (ORDER BY and LIMIT on the recursion), so it reads a row's children only
// when the row is reached, and each read of a thread's rows takes, from
// each arm, the first limit rows in that order (agentRowArms): what the
// listing reads follows the rows it returns and its depth, not the
// threads' history or how many rows lie past the limit.
func threadAgentTreeSQL(limit int) string {
	n := strconv.Itoa(limit)
	var seeds, steps []string
	for _, arm := range agentRowArms("a", agentRootSQL) {
		seeds = append(seeds, `SELECT r.token, 0, printf('%020d', r.created_at) || r.token || char(1), json_array(r.caller_thread_id),
           r.kind, r.target_computer_id, r.target_thread_id
      FROM thread_requests r
     WHERE r.token IN (`+arm.first(n)+`)`)
	}
	for _, arm := range agentRowArms("a", "tree.t_target") {
		steps = append(steps, `SELECT r.token, tree.t_depth + 1, tree.t_sort || printf('%020d', r.created_at) || r.token || char(1),
           json_insert(tree.t_path, '$[#]', tree.t_target), r.kind, r.target_computer_id, r.target_thread_id
      FROM tree CROSS JOIN thread_requests r
     WHERE tree.t_kind = 'spawn' AND tree.t_computer = '' AND tree.t_target <> '' AND tree.t_depth + 1 < ?4
       AND NOT EXISTS (SELECT 1 FROM json_each(tree.t_path) WHERE json_each.value = tree.t_target)
       AND r.token IN (`+arm.first(n)+`)`)
	}
	return `WITH RECURSIVE
  ` + agentHeldSQL + `,
  tree(t_token, t_depth, t_sort, t_path, t_kind, t_computer, t_target) AS (
    ` + strings.Join(seeds, `
    UNION ALL
    `) + `
    UNION ALL
    ` + strings.Join(steps, `
    UNION ALL
    `) + `
    ORDER BY 3 LIMIT ` + n + `)
SELECT ` + threadAgentRequestColumns + `, tree.t_depth,
       tree.t_depth = ?4 - 1 AND tree.t_kind = 'spawn' AND tree.t_computer = '' AND tree.t_target <> ''
         AND NOT EXISTS (SELECT 1 FROM json_each(tree.t_path) WHERE json_each.value = tree.t_target)
         AND (` + agentRowsUnderSQL("tree.t_target") + `),
       ` + agentTargetOpenSQL("r") + `
  FROM tree CROSS JOIN thread_requests r
 WHERE r.token = tree.t_token
 ORDER BY tree.t_sort`
}

// threadAgentNamedRowsSQL reads the listing's rows among ?6, each at the
// place the listing gives it, with the parameters of threadAgentTreeSQL.
// `up` climbs from each named row's caller to the root, one spawn a step
// (idx_thread_requests_spawn_link), prefixing each spawn's part of the
// listing's sort key: the row is listed when the row and every spawn above
// it are, within the depth. The cost is the depth per named row.
var threadAgentNamedRowsSQL = `WITH RECURSIVE
  ` + agentHeldSQL + `,
  up(token, at, depth, sort) AS (
    SELECT r.token, r.caller_thread_id, 0, printf('%020d', r.created_at) || r.token || char(1)
      FROM json_each(?6) CROSS JOIN thread_requests r
     WHERE r.token = json_each.value AND ` + agentRowListedSQL("r") + ` AND ` + agentRowKindSQL("r") + `
    UNION ALL
    SELECT up.token, sp.caller_thread_id, up.depth + 1, printf('%020d', sp.created_at) || sp.token || char(1) || up.sort
      FROM up CROSS JOIN thread_requests sp
     WHERE up.at <> ` + agentRootSQL + ` AND up.depth + 1 < ?4 AND ` + agentSpawnLinkSQL("sp") + ` AND sp.target_thread_id = up.at
       AND ` + agentRowListedSQL("sp") + `)
SELECT ` + threadAgentRequestColumns + `, up.depth,
       up.depth = ?4 - 1 AND ` + agentSpawnLinkSQL("r") + ` AND (` + agentRowsUnderSQL("r.target_thread_id") + `),
       ` + agentTargetOpenSQL("r") + `
  FROM up CROSS JOIN thread_requests r
 WHERE up.at = ` + agentRootSQL + ` AND r.token = up.token
 ORDER BY up.sort`

// ThreadAgentStateRead is one read of what a thread's own Agents listing
// says of it, with ThreadAgentTreeRead's Waiting, Live and SettledSince.
type ThreadAgentStateRead struct {
	Thread       string
	Waiting      []string
	Live         []string
	SettledSince int64
}

// ThreadAgentState is what a thread's Agents listing says of the thread
// whatever rows it has room for.
type ThreadAgentState struct {
	// OpenBelow is whether the thread has open work at any depth below it,
	// as agentTargetOpenSQL reads it for a spawn of it: a request it made
	// that is open work, or a local spawn it made whose marks count open
	// work, within agentOpenDepth (agentOpenUnderSQL), or a spawn it made
	// above held work (agentHeldSQL), deeper or only in memory.
	OpenBelow bool
	// Rows is whether its listing has a row.
	Rows bool
}

// ReadThreadAgentState reads what a thread's Agents listing says of it,
// in one statement: a search of each index agentOpenUnderSQL and
// agentRowArms name, and the held work's climb.
func (s *Store) ReadThreadAgentState(read ThreadAgentStateRead) (ThreadAgentState, error) {
	action := "store: read the agents state of " + read.Thread
	held, err := agentHeldParam(read.Waiting, read.Live)
	if err != nil {
		return ThreadAgentState{}, fmt.Errorf("%s: %w", action, err)
	}
	var state ThreadAgentState
	if err := s.reader().QueryRow(threadAgentStateSQL, read.Thread, held, read.SettledSince).Scan(&state.OpenBelow, &state.Rows); err != nil {
		return ThreadAgentState{}, fmt.Errorf("%s: %w", action, err)
	}
	return state, nil
}

// threadAgentStateSQL is ReadThreadAgentState's read: ?1 the thread, ?2 the
// held work (agentHeldParam), ?3 the settled window.
var threadAgentStateSQL = `WITH RECURSIVE ` + agentHeldSQL + `
SELECT ` + agentOpenBelowThreadSQL(agentRootSQL) + `,
       (` + agentRowsUnderSQL(agentRootSQL) + `)`

// ThreadsWithOpenAgentRows lists, by id, the threads after `after` in id
// order whose Agents listing may hold an open row, at most limit of them,
// and whether more follow: a thread that made an open request the listing
// shows (a spawn, or a send or ask with its wake armed or one of waiting
// parked on it), one with a local spawn whose marks count open work below
// it, or that reaches a live thread (one of live) or sits above a parked
// request or a live thread (`held`), or a spawn on another computer that
// last reported open work or its thread live. Each arm reads a partial
// index that holds only open work (idx_thread_requests_open_created,
// idx_thread_requests_open_below, idx_thread_requests_nested_caller) from
// the cursor on, so a page follows the open work on this computer, not the
// ledger's settled rows.
func (s *Store) ThreadsWithOpenAgentRows(waiting, live []string, after string, limit int) (_ []string, more bool, err error) {
	action := "store: list the threads with open agent rows"
	if limit < 1 {
		return nil, false, fmt.Errorf("%s: limit %d", action, limit)
	}
	held, err := agentHeldParam(waiting, live)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	rows, err := s.reader().Query(threadsWithOpenAgentRowsSQL(limit+1), after, held)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("%s: iterate: %w", action, err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// threadsWithOpenAgentRowsSQL is ThreadsWithOpenAgentRows' read of its first
// limit threads: ?1 is the cursor, ?2 the held work (agentHeldParam).
func threadsWithOpenAgentRowsSQL(limit int) string {
	return `WITH RECURSIVE ` + agentHeldSQL + `
		SELECT a.caller_thread_id FROM thread_requests a INDEXED BY idx_thread_requests_open_created
		 WHERE a.caller_thread_id > ?1 AND a.lineage_only = 0 AND a.settled_at IS NULL AND ` + agentRowKindSQL("a") + `
		UNION
		SELECT thread FROM held WHERE thread > ?1
		UNION
		SELECT a.caller_thread_id FROM thread_requests a INDEXED BY idx_thread_requests_open_below
		 WHERE a.caller_thread_id > ?1 AND ` + agentSpawnLinkSQL("a") + ` AND ` + agentOpenBelowSQL("a") + `
		UNION
		SELECT a.caller_thread_id FROM thread_requests a INDEXED BY idx_thread_requests_nested_caller
		 WHERE a.caller_thread_id > ?1 AND ` + nestedRemoteSpawnSQL("a") + `
		ORDER BY 1 LIMIT ` + strconv.Itoa(limit)
}

// threadAgentRequestColumns is threadRequestColumns for alias r, with the
// first ?5 bytes of the newer answer in place of the answer, whether a late
// reply arrived in place of it, and the wake row after them.
const threadAgentRequestColumns = `r.token, r.caller_thread_id, r.kind, COALESCE(r.due_at, 0),
    r.target_computer_id, r.target_thread_id, r.target_thread_title, r.origin_thread_id, r.notify, r.state,
    substr(COALESCE(r.late_reply, r.answer), 1, ?5), r.answer_kind, r.late_reply IS NOT NULL, COALESCE(r.late_reply_at, 0), r.revision,
    COALESCE(r.settled_at, 0), COALESCE(r.delivered_at, 0), r.delivered_how, r.delivered_with,
    COALESCE(r.late_delivered_at, 0), COALESCE(r.expires_at, 0),
    r.polling, r.next_check, r.attempts, r.issue,
    r.wake_attempts, r.wake_next_check, r.wake_issue, r.lineage_only, r.created_at, r.updated_at,
    COALESCE(r.confirmed_at, 0), COALESCE(r.late_confirmed_at, 0), r.answer_agent, r.answer_agent_role,
    r.target_provider, r.nested_open, r.late_delivered_how, r.made_in_thread_id,
    r.late_reply_agent, r.late_reply_agent_role, r.rest_unreported, r.answer_cause, r.answer_cause_arg, r.admit_until,
    r.late_reply_expired, r.ack_owed, r.target_computer_name, r.link_end, ` + wakeItemSQL

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
