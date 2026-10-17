package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The Agents segment's reads of the request ledger
// (docs/specs/standing-agents.md, "Agents segment"). Each is a number of
// statements that does not grow with the tree or the history: the listing
// or named rows of it (two), the tool rows' states and the callers a change
// frame names (one each).

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
	// Tokens, when set, keeps only those of the listing's rows, each
	// where the listing places it, wherever it falls against Limit.
	Tokens []string
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
// whose computer last reported open work (`nested_open`). A thread is
// walked once, under the first row in that order that reaches it, so a
// request is listed once; a later row that reaches it, or one at the depth
// bound, nests nothing. At most Limit rows, and whether there were more.
//
// Which thread a row enters depends on the rows kept before it, which no
// filter over the rows can express: one statement reads the tree's shape
// in order, agentTreeWalk keeps the rows the walk lists up to the limit,
// and a second statement reads those rows. Both cost what the listed rows
// cost, not the threads' history (agentRowArms); a read of named rows
// costs what the listed spawns cost (agentTreeShapeSQL).
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
	listed, more, err := s.walkThreadAgentTree(read, string(tokens))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	if read.Tokens != nil {
		named := make(map[string]bool, len(read.Tokens))
		for _, token := range read.Tokens {
			named[token] = true
		}
		listed = slices.DeleteFunc(listed, func(row agentTreeRow) bool { return !named[row.token] })
	}
	out, err := s.threadAgentRequests(listed, read.AnswerHead)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	return out, more, nil
}

// agentTreeRow is a row of the tree's shape: where it sits and what it
// reaches.
type agentTreeRow struct {
	token, sort    string
	depth          int
	kind, computer string
	target         string
	hidden         bool
}

// walkThreadAgentTree reads the tree's shape and keeps, in order, the first
// Limit rows the walk lists, and whether it lists more; for named rows,
// every row the walk lists, the named among them and the spawns that place
// them.
func (s *Store) walkThreadAgentTree(read ThreadAgentTreeRead, waiting string) (_ []agentTreeRow, more bool, err error) {
	query, args := threadAgentTreeSQL, []any{read.Root, waiting, read.SettledSince, read.Depth, read.OpenDepth}
	if read.Tokens != nil {
		named, err := json.Marshal(read.Tokens)
		if err != nil {
			return nil, false, fmt.Errorf("encode tokens: %w", err)
		}
		query, args = threadAgentNamedTreeSQL, append(args, string(named))
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	walk := agentTreeWalk{depth: read.Depth, visited: map[string]bool{read.Root: true}, entered: map[string]bool{}}
	out := []agentTreeRow{}
	for rows.Next() {
		var row agentTreeRow
		var hidden int
		if err := rows.Scan(&row.token, &row.depth, &row.sort, &row.kind, &row.computer, &row.target, &hidden); err != nil {
			return nil, false, fmt.Errorf("scan: %w", err)
		}
		row.hidden = hidden != 0
		if !walk.keep(&row) {
			continue
		}
		if len(out) == read.Limit && read.Tokens == nil {
			return out, true, nil
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate: %w", err)
	}
	return out, false, nil
}

// threadAgentRequests reads the requests of listed, in its order, each at
// the place the walk gave it. One the ledger no longer holds is left out.
func (s *Store) threadAgentRequests(listed []agentTreeRow, answerHead int) (_ []ThreadAgentRequest, err error) {
	out := []ThreadAgentRequest{}
	if len(listed) == 0 {
		return out, nil
	}
	tokens := make([]string, len(listed))
	for i, row := range listed {
		tokens[i] = row.token
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("encode listed tokens: %w", err)
	}
	rows, err := s.reader().Query(threadAgentRequestsSQL, answerHead, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	at := 0
	for rows.Next() {
		var row ThreadAgentRequest
		var late int
		row.ThreadRequest, err = scanThreadRequestWith(rows, &row.AnswerHead, &late, &row.WakeItemID)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		for listed[at].token != row.Token {
			at++
		}
		row.HasLateReply, row.Depth, row.NestedHidden = late != 0, listed[at].depth, listed[at].hidden
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}

// agentTreeWalk keeps, in tree order, the rows of the threads it enters:
// a thread is entered by the first local spawn row that reaches it, unless
// that row sits at the depth bound. It drops the rows of a thread a later
// spawn row reaches again, and that row's mark of a thread at the bound.
type agentTreeWalk struct {
	depth   int
	visited map[string]bool
	// entered holds the sort keys of the spawn rows whose threads' rows
	// are kept.
	entered map[string]bool
}

func (w *agentTreeWalk) keep(row *agentTreeRow) bool {
	if row.depth > 0 {
		parent := strings.TrimSuffix(row.sort, "\x01")
		cut := strings.LastIndexByte(parent, 1)
		if cut < 0 || !w.entered[parent[:cut+1]] {
			return false
		}
	}
	if row.kind != ThreadRequestSpawn || row.computer != "" || row.target == "" {
		return true
	}
	if w.visited[row.target] {
		row.hidden = false
		return true
	}
	w.visited[row.target] = true
	if row.depth+1 < w.depth {
		w.entered[row.sort] = true
	}
	return true
}

// threadAgentTreeSQL and threadAgentNamedTreeSQL are the tree's shape for
// ListThreadAgentTree, the listing's and a read of named rows. ?1 is the
// root, ?2 the waiting tokens, ?3 the settled window, ?4 the depth, ?5 the
// open depth and ?6 the named tokens.
var (
	threadAgentTreeSQL      = agentTreeShapeSQL(false)
	threadAgentNamedTreeSQL = agentTreeShapeSQL(true)
)

// agentTreeShapeSQL reads the tree's shape in order. `open_callers` and
// `nested_callers` are the threads with work open in them, a request or a
// remote spawn that reported open work, one seek each
// (idx_thread_requests_open, _nested_caller); `hot` walks up from those,
// one step per local spawn, the open depth less one: a settled local spawn
// whose thread is in it is kept. Neither reads a spawn that
// settled with nothing open under it. `tree` walks the listed rows, its
// sort key each level's creation time and token, carrying the threads
// above each row so a cycle ends. A local spawn at the bound says whether
// its thread has rows of its own.
//
// Each read of a thread's rows is a union of arms searched by the caller
// (agentRowArms), so the cost follows the listed rows, not the thread's
// history. Only local spawns move the walk, so a read of named rows takes
// the named rows and the listed local spawns alone, the open, those
// settled inside the window (idx_thread_requests_spawn_settled) and those
// kept for open work under them: the walk places each row where the
// listing does, at the cost of the listed spawns.
func agentTreeShapeSQL(named bool) string {
	arms := func(alias, caller string) []agentArm {
		if !named {
			return agentRowArms(alias, caller)
		}
		a := alias + "."
		spawn := a + `caller_thread_id = ` + caller + ` AND ` + a + `kind = 'spawn' AND ` + a + `target_computer_id = ''
         AND ` + a + `lineage_only = 0 AND `
		return []agentArm{
			{"", a + `token = json_each.value AND ` + a + `caller_thread_id = ` + caller + `
         AND NOT (` + a + `kind = 'spawn' AND ` + a + `target_computer_id = '') AND ` + agentRowListedSQL(alias)},
			{"idx_thread_requests_spawn_settled", spawn + a + `settled_at IS NULL`},
			{"idx_thread_requests_spawn_settled", spawn + a + `settled_at >= ?3`},
			{"idx_thread_requests_spawn_caller", spawn + a + `settled_at < ?3 AND ` + a + `target_thread_id IN (SELECT thread_id FROM hot)`},
		}
	}
	from := func(arm agentArm, step bool) string {
		table := arm.from("r")
		if arm.index == "" {
			table = `json_each(?6) CROSS JOIN ` + table
		}
		if step {
			return `tree CROSS JOIN ` + table
		}
		return table
	}
	var seeds, steps []string
	for _, arm := range arms("r", "?1") {
		seeds = append(seeds, `SELECT r.token, 0, printf('%020d', r.created_at) || r.token || char(1), json_array(r.caller_thread_id),
           r.kind, r.target_computer_id, r.target_thread_id
      FROM `+from(arm, false)+`
     WHERE `+arm.where+` AND `+agentRowKindSQL("r"))
	}
	for _, arm := range arms("r", "tree.t_target") {
		steps = append(steps, `SELECT r.token, tree.t_depth + 1, tree.t_sort || printf('%020d', r.created_at) || r.token || char(1),
           json_insert(tree.t_path, '$[#]', tree.t_target), r.kind, r.target_computer_id, r.target_thread_id
      FROM `+from(arm, true)+`
     WHERE tree.t_kind = 'spawn' AND tree.t_computer = '' AND tree.t_target <> '' AND tree.t_depth + 1 < ?4
       AND NOT EXISTS (SELECT 1 FROM json_each(tree.t_path) WHERE json_each.value = tree.t_target)
       AND `+arm.where+` AND `+agentRowKindSQL("r"))
	}
	var nested []string
	for _, arm := range agentRowArms("c", "tree.t_target") {
		nested = append(nested, `EXISTS (SELECT 1 FROM `+arm.from("c")+` WHERE `+arm.where+` AND `+agentRowKindSQL("c")+`)`)
	}
	return `WITH RECURSIVE
  ` + agentCallersSQL("open_callers", "idx_thread_requests_open", openAgentRequestSQL("c")) + `,
  ` + agentCallersSQL("nested_callers", "idx_thread_requests_nested_caller", nestedRemoteSpawnSQL("c")) + `,
  hot(thread_id, dist) AS (
    SELECT thread_id, 0 FROM open_callers WHERE thread_id IS NOT NULL
    UNION
    SELECT thread_id, 0 FROM nested_callers WHERE thread_id IS NOT NULL
    UNION
    SELECT r.caller_thread_id, hot.dist + 1
      FROM hot CROSS JOIN thread_requests r INDEXED BY idx_thread_requests_spawn_target
     WHERE r.target_thread_id = hot.thread_id AND r.kind = 'spawn' AND r.target_computer_id = ''
       AND r.lineage_only = 0 AND hot.dist + 1 < ?5),
  tree(t_token, t_depth, t_sort, t_path, t_kind, t_computer, t_target) AS (
    ` + strings.Join(seeds, `
    UNION ALL
    `) + `
    UNION ALL
    ` + strings.Join(steps, `
    UNION ALL
    `) + `)
SELECT tree.t_token, tree.t_depth, tree.t_sort, tree.t_kind, tree.t_computer, tree.t_target,
       CASE WHEN tree.t_depth = ?4 - 1 AND tree.t_kind = 'spawn' AND tree.t_computer = '' AND tree.t_target <> ''
                 AND NOT EXISTS (SELECT 1 FROM json_each(tree.t_path) WHERE json_each.value = tree.t_target)
            THEN ` + strings.Join(nested, `
              OR `) + `
            ELSE 0 END
  FROM tree
 ORDER BY tree.t_sort`
}

// agentCallersSQL is the CTE name listing each caller of a request where
// holds once, read from index, whose predicate where states, one seek per
// caller (the next caller after the last), not one entry per request. The
// list ends in a NULL.
func agentCallersSQL(name, index, where string) string {
	next := `(SELECT MIN(c.caller_thread_id) FROM thread_requests c INDEXED BY ` + index + `
            WHERE ` + where + ` AND c.caller_thread_id > `
	return name + `(thread_id) AS (
    SELECT ` + next + `'')
    UNION ALL
    SELECT ` + next + name + `.thread_id) FROM ` + name + ` WHERE ` + name + `.thread_id IS NOT NULL)`
}

// agentArm is one arm of a read of a caller's rows: the index that reads
// it, none for a read by token, and its predicate, which states the
// index's.
type agentArm struct {
	index, where string
}

// from is the arm's table under alias, held to its index.
func (arm agentArm) from(alias string) string {
	if arm.index == "" {
		return `thread_requests ` + alias
	}
	return `thread_requests ` + alias + ` INDEXED BY ` + arm.index
}

// threadAgentRequestsSQL reads the requests ?2 names as the listing shows
// them, in that order, with ?1 bytes of each answer.
const threadAgentRequestsSQL = `SELECT ` + threadAgentRequestColumns + `
  FROM json_each(?2) CROSS JOIN thread_requests r
 WHERE r.token = json_each.value
 ORDER BY json_each.key`

// agentRowKindSQL matches a request kind the listing shows: every spawn,
// and a send or ask whose wake is armed or that a call is parked on (?2).
func agentRowKindSQL(alias string) string {
	a := alias + "."
	return a + `kind IN ('spawn','send','ask')
       AND (` + a + `kind = 'spawn' OR ` + a + `notify = 1 OR ` + a + `token IN (SELECT value FROM json_each(?2)))`
}

// agentRowArms are the requests of caller the listing keeps (see
// ListThreadAgentTree) as four disjoint arms, each held to the index whose
// predicate it states, so neither the statistics ANALYZE leaves nor a
// bound value changes the plan: open, and settled inside the window
// (idx_thread_requests_caller_settled); a local spawn
// settled before it with open work under it, read from `hot`
// (idx_thread_requests_spawn_caller); a spawn on another computer that last
// reported open work (idx_thread_requests_nested_caller). None reads the
// caller's settled history.
func agentRowArms(alias, caller string) []agentArm {
	a := alias + "."
	by := a + `caller_thread_id = ` + caller + ` AND `
	return []agentArm{
		{"idx_thread_requests_caller_settled", by + a + `lineage_only = 0 AND ` + a + `settled_at IS NULL`},
		{"idx_thread_requests_caller_settled", by + a + `lineage_only = 0 AND ` + a + `settled_at >= ?3`},
		{"idx_thread_requests_spawn_caller", by + a + `kind = 'spawn' AND ` + a + `target_computer_id = '' AND ` + a + `lineage_only = 0
         AND ` + a + `settled_at < ?3 AND ` + a + `target_thread_id IN (SELECT thread_id FROM hot)`},
		{"idx_thread_requests_nested_caller", by + nestedRemoteSpawnSQL(alias) + ` AND ` + a + `settled_at < ?3`},
	}
}

// agentRowListedSQL is agentRowArms as one predicate, for a row already
// found by its key.
func agentRowListedSQL(alias string) string {
	a := alias + "."
	return `(` + a + `lineage_only = 0 AND (` + a + `settled_at IS NULL OR ` + a + `settled_at >= ?3
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id = '' AND ` + a + `target_thread_id IN (SELECT thread_id FROM hot))
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1)))`
}

// threadAgentRequestColumns is threadRequestColumns for alias r, with the
// first ?1 bytes of the newer answer in place of the answer, whether a late
// reply arrived in place of it, and the wake row after them.
const threadAgentRequestColumns = `r.token, r.caller_thread_id, r.kind, COALESCE(r.due_at, 0),
    r.target_computer_id, r.target_thread_id, r.target_thread_title, r.origin_thread_id, r.notify, r.state,
    substr(COALESCE(r.late_reply, r.answer), 1, ?1), r.answer_kind, r.late_reply IS NOT NULL, COALESCE(r.late_reply_at, 0), r.revision,
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
