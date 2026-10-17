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
// (open_below_0 to open_below_3, migration v158).
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
// after SettledSince, or a spawn something under it is still open in: a
// local spawn with open work within agentOpenDepth threads below its
// target (its marks), or a remote spawn whose computer last reported open
// work (`nested_open`). A thread is reached by at most one local spawn
// (idx_thread_requests_spawn_link), so each request is listed once, under
// the chain of spawns above its caller. A local spawn at the depth bound
// nests nothing. At most Limit rows, and whether there were more.
func (s *Store) ListThreadAgentTree(read ThreadAgentTreeRead) (_ []ThreadAgentRequest, more bool, err error) {
	action := "store: list the agents of " + read.Root
	if read.Limit < 1 || read.Depth < 1 || read.AnswerHead < 0 {
		return nil, false, fmt.Errorf("%s: limit %d, depth %d and answer head %d", action, read.Limit, read.Depth, read.AnswerHead)
	}
	waiting := read.Waiting
	if waiting == nil {
		waiting = []string{}
	}
	tokens, err := json.Marshal(waiting)
	if err != nil {
		return nil, false, fmt.Errorf("%s: encode waiting tokens: %w", action, err)
	}
	query, args := threadAgentTreeSQL(read.Limit+1), []any{read.Root, string(tokens), read.SettledSince, read.Depth, read.AnswerHead}
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
	if read.Tokens == nil && len(out) > read.Limit {
		return out[:read.Limit], true, nil
	}
	return out, false, nil
}

// threadAgentTreeSQL is the listing's read of its first limit rows. ?1 is
// the root, ?2 the waiting tokens, ?3 the settled window, ?4 the depth and
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
         AND (` + agentRowsUnderSQL("tree.t_target") + `)
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
       up.depth = ?4 - 1 AND ` + agentSpawnLinkSQL("r") + ` AND (` + agentRowsUnderSQL("r.target_thread_id") + `)
  FROM up CROSS JOIN thread_requests r
 WHERE up.at = ` + agentRootSQL + ` AND r.token = up.token
 ORDER BY up.sort`

// agentArm is one arm of a read of a caller's rows: the index that reads
// it, whether the read is held to it, and its predicate, which states the
// index's.
type agentArm struct {
	index  string
	pinned bool
	where  string
}

// from is the arm's table under alias, held to its index when pinned.
func (arm agentArm) from(alias string) string {
	if arm.pinned {
		return `thread_requests ` + alias + ` INDEXED BY ` + arm.index
	}
	return `thread_requests ` + alias
}

// first is the arm's first limit rows in tree order, read through its
// index: open rows already in that order, the others sorted from the few
// the arm holds.
func (arm agentArm) first(limit string) string {
	return `SELECT a.token FROM ` + arm.from("a") + `
        WHERE ` + arm.where + ` AND ` + agentRowKindSQL("a") + `
        ORDER BY a.created_at, a.token LIMIT ` + limit
}

// agentRowArms are the requests of caller the listing keeps as four
// disjoint arms, each read through the index whose predicate it states:
// open (idx_thread_requests_open_created, in tree order); settled inside
// the window (idx_thread_requests_caller_settled); a local spawn settled
// before it with open work under it (idx_thread_requests_open_below); a
// spawn on another computer that last reported open work
// (idx_thread_requests_nested_caller). None reads the caller's settled
// history, and only the open arm can hold many rows. The last two are held
// to their index: unheld, the planner reads them through
// idx_thread_requests_caller_settled from the history's settled_at range
// (TestThreadAgentTreeSearchesItsArms).
func agentRowArms(alias, caller string) []agentArm {
	a := alias + "."
	by := a + `caller_thread_id = ` + caller + ` AND `
	return []agentArm{
		{"idx_thread_requests_open_created", false, by + a + `lineage_only = 0 AND ` + a + `settled_at IS NULL`},
		{"idx_thread_requests_caller_settled", false, by + a + `lineage_only = 0 AND ` + a + `settled_at >= ` + agentWindowSQL},
		{"idx_thread_requests_open_below", true, by + agentSpawnLinkSQL(alias) + ` AND ` + agentOpenBelowSQL(alias) + ` AND ` + a + `settled_at < ` + agentWindowSQL},
		{"idx_thread_requests_nested_caller", true, by + nestedRemoteSpawnSQL(alias) + ` AND ` + a + `settled_at < ` + agentWindowSQL},
	}
}

// agentRowsUnderSQL is whether thread has a row the listing would nest.
func agentRowsUnderSQL(thread string) string {
	var arms []string
	for _, arm := range agentRowArms("c", thread) {
		arms = append(arms, `EXISTS (SELECT 1 FROM `+arm.from("c")+`
              WHERE `+arm.where+` AND `+agentRowKindSQL("c")+`)`)
	}
	return strings.Join(arms, `
           OR `)
}

// agentSpawnLinkSQL matches the local spawn that reaches its target, the
// predicate of idx_thread_requests_spawn_link.
func agentSpawnLinkSQL(alias string) string {
	a := alias + "."
	return a + `kind = 'spawn' AND ` + a + `target_computer_id = '' AND ` + a + `lineage_only = 0 AND ` + a + `target_thread_id <> ''`
}

// agentOpenBelowSQL is whether a local spawn has open work under its
// target within agentOpenDepth, the term of idx_thread_requests_open_below.
func agentOpenBelowSQL(alias string) string {
	a := alias + "."
	return `(` + a + `open_below_0 + ` + a + `open_below_1 + ` + a + `open_below_2 + ` + a + `open_below_3) > 0`
}

// agentOpenWorkSQL matches a request that is open work under a spawn: an
// open request the listing counts (openAgentRequestSQL), or a spawn on
// another computer that last reported open work there. The marks count
// these (migration v158).
func agentOpenWorkSQL(alias string) string {
	a := alias + "."
	return `(` + a + `lineage_only = 0 AND ((` + a + `settled_at IS NULL AND (` + a + `kind = 'spawn' OR (` + a + `kind IN ('send','ask') AND ` + a + `notify = 1)))
        OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1)))`
}

// agentRowKindSQL matches a request kind the listing shows: every spawn,
// and a send or ask whose wake is armed or that a call is parked on (?2).
func agentRowKindSQL(alias string) string {
	a := alias + "."
	return a + `kind IN ('spawn','send','ask')
       AND (` + a + `kind = 'spawn' OR ` + a + `notify = 1 OR ` + a + `token IN (SELECT value FROM json_each(?2)))`
}

// agentRowListedSQL is agentRowArms as one predicate, for a row already
// found by its key.
func agentRowListedSQL(alias string) string {
	a := alias + "."
	return `(` + a + `lineage_only = 0 AND (` + a + `settled_at IS NULL OR ` + a + `settled_at >= ` + agentWindowSQL + `
         OR (` + agentSpawnLinkSQL(alias) + ` AND ` + agentOpenBelowSQL(alias) + `)
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1)))`
}

// threadAgentRequestColumns is threadRequestColumns for alias r, with the
// first ?5 bytes of the newer answer in place of the answer, whether a late
// reply arrived in place of it, and the wake row after them.
const threadAgentRequestColumns = `r.token, r.caller_thread_id, r.kind, COALESCE(r.due_at, 0),
    r.target_computer_id, r.target_thread_id, r.target_thread_title, r.origin_thread_id, r.notify, r.state,
    substr(COALESCE(r.late_reply, r.answer), 1, ?5), r.answer_kind, r.late_reply IS NOT NULL, COALESCE(r.late_reply_at, 0), r.revision,
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
