package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// ThreadAgentStopRead is one read of what a Stop of every Agents row of
// Root acts on, with ThreadAgentTreeRead's Root, Waiting, Live and
// SettledSince.
type ThreadAgentStopRead struct {
	Root         string
	Waiting      []string
	Live         []string
	SettledSince int64
	// Page bounds the rows each statement reads.
	Page int
}

// ListThreadAgentStopRows returns the requests a Stop of every Agents row
// of Root acts on: the rows ListThreadAgentTree lists at Root's own level,
// less a send or ask that already settled, which has nothing left to stop.
// A settled spawn stays, since its Stop reaches into the thread it made.
// Oldest first, each without its answer. The rows are read Page at a time,
// each page through the listing's arms from where the last one ended, all
// inside one read transaction: they are the ledger as the first page found
// it, and a request made meanwhile is in none of them. The tray's limit
// does not apply: every such row is returned.
func (s *Store) ListThreadAgentStopRows(read ThreadAgentStopRead) (out []ThreadRequest, err error) {
	action := "store: list what a stop of the agents of " + read.Root + " stops"
	if read.Page < 1 {
		return nil, fmt.Errorf("%s: page %d", action, read.Page)
	}
	held, err := agentHeldParam(read.Waiting, read.Live)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	query := threadAgentStopRowsSQL(read.Page)
	out = []ThreadRequest{}
	afterAt, afterToken := int64(math.MinInt64), ""
	for {
		page, err := s.threadAgentStopPage(tx, query, read, held, afterAt, afterToken)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		out = append(out, page...)
		if len(page) < read.Page {
			return out, nil
		}
		last := page[len(page)-1]
		afterAt, afterToken = last.CreatedAt, last.Token
		if hook := s.agentStopPageReadForTest; hook != nil {
			hook()
		}
	}
}

func (s *Store) threadAgentStopPage(tx *sql.Tx, query string, read ThreadAgentStopRead, held string, afterAt int64, afterToken string) (_ []ThreadRequest, err error) {
	rows, err := tx.Query(query, read.Root, held, read.SettledSince, nil, 0, afterAt, afterToken)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	page := []ThreadRequest{}
	for rows.Next() {
		var head []byte
		var late int
		var wake string
		row, err := scanThreadRequestWith(rows, &head, &late, &wake)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return page, nil
}

// threadAgentStopRowsSQL reads one page of ListThreadAgentStopRows: the
// first limit rows after the cursor (?6, ?7) in (created_at, token) order
// from each arm of agentRowArms, read through the arm's index, with the
// parameters of threadAgentTreeSQL (?4 unused, ?5 an answer head of 0).
// The open arm's index is in that order and seeks to the cursor. The other
// arms hold few rows, and their cursor term is kept off every index: on
// idx_thread_requests_caller it would read the caller's history from the
// cursor on (TestThreadAgentTreeSearchesItsArms).
func threadAgentStopRowsSQL(limit int) string {
	n := strconv.Itoa(limit)
	var arms []string
	for _, arm := range agentRowArms("a", agentRootSQL) {
		cursor := `(+a.created_at, +a.token) > (?6, ?7)`
		if arm.index == "idx_thread_requests_open_created" {
			cursor = `(a.created_at, a.token) > (?6, ?7)`
		}
		arms = append(arms, `SELECT token FROM (SELECT a.token FROM `+arm.from("a")+`
        WHERE `+arm.where+` AND `+agentRowKindSQL("a")+`
          AND (a.settled_at IS NULL OR a.kind = 'spawn') AND `+cursor+`
        ORDER BY a.created_at, a.token LIMIT `+n+`)`)
	}
	return `WITH RECURSIVE
  ` + agentHeldSQL + `
SELECT ` + threadAgentRequestColumns + `
  FROM thread_requests r
 WHERE r.token IN (` + strings.Join(arms, `
    UNION ALL
    `) + `)
 ORDER BY r.created_at, r.token LIMIT ` + n
}

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

// agentRowArms are the requests of caller the listing keeps as five
// disjoint arms, each read through the index whose predicate it states:
// open (idx_thread_requests_open_created, in tree order); settled inside
// the window (idx_thread_requests_caller_settled); a local spawn settled
// before it with open work under it, stored (idx_thread_requests_open_below)
// or only held in memory, a parked request or a live thread (`held`, by
// token); a spawn on another computer that last reported open work
// (idx_thread_requests_nested_caller). None reads the caller's settled
// history, and only the open arm can hold many rows. The stored and remote
// arms are held to their index, and the held arm's other terms are kept
// off theirs: otherwise the planner reads them through
// idx_thread_requests_caller_settled from the history's settled_at range
// (TestThreadAgentTreeSearchesItsArms).
func agentRowArms(alias, caller string) []agentArm {
	a := alias + "."
	by := a + `caller_thread_id = ` + caller + ` AND `
	return []agentArm{
		{"idx_thread_requests_open_created", false, by + a + `lineage_only = 0 AND ` + a + `settled_at IS NULL`},
		{"idx_thread_requests_caller_settled", false, by + a + `lineage_only = 0 AND ` + a + `settled_at >= ` + agentWindowSQL},
		{"idx_thread_requests_open_below", true, by + agentSpawnLinkSQL(alias) + ` AND ` + agentOpenBelowSQL(alias) + ` AND ` + a + `settled_at < ` + agentWindowSQL},
		{"sqlite_autoindex_thread_requests_1", false, a + `token IN (SELECT token FROM held WHERE thread = ` + caller + `) AND +` + a + `caller_thread_id = ` + caller +
			` AND NOT ` + agentOpenBelowSQL(alias) + ` AND +` + a + `settled_at < ` + agentWindowSQL},
		{"idx_thread_requests_nested_caller", true, by + nestedRemoteSpawnSQL(alias) + ` AND ` + a + `settled_at < ` + agentWindowSQL},
	}
}

// agentHeldSQL is the `held` table of the tree reads: open work the marks
// do not count, each row a token and the thread it keeps from rest. That is
// work the app holds in memory (?2, agentHeldParam): a send or ask a call is
// parked on (only an armed wake is stored, agentOpenWorkSQL, and a parked
// call arms its wake once its wait ends), held by its own caller, the work
// that caller started; or a live thread, whose own spawn is its link, the
// first spawn above it, held by that spawn's caller. And it is stored work
// deeper than the marks reach: a spawn whose open_below_3 counts work
// agentOpenDepth-1 spawns below its target has it agentOpenDepth below its
// caller's link, which no mark counts (idx_thread_requests_open_deep, which
// holds only such spawns). `held` climbs one spawn a step from each,
// keyed by the queued thread (idx_thread_requests_spawn_link), up to the
// root, each spawn held by its caller. Each link has one caller, so a link
// enters it once however many seeds lie below it, and a cycle ends: its
// cost is one search per parked request, live thread and deep spawn, and
// one per distinct link above them.
var agentHeldSQL = `held(token, thread) AS (
    SELECT pr.token, pr.caller_thread_id
      FROM json_each(?2, '$.waiting') CROSS JOIN thread_requests pr
     WHERE pr.token = json_each.value AND pr.kind IN ('send','ask') AND pr.lineage_only = 0 AND pr.settled_at IS NULL
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM json_each(?2, '$.live') CROSS JOIN thread_requests ps
     WHERE ps.target_thread_id = json_each.value AND ` + agentSpawnLinkSQL("ps") + `
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM thread_requests f INDEXED BY idx_thread_requests_open_deep CROSS JOIN thread_requests ps
     WHERE ` + agentSpawnLinkSQL("f") + ` AND f.open_below_3 > 0
       AND ps.target_thread_id = f.caller_thread_id AND ` + agentSpawnLinkSQL("ps") + `
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM thread_requests d INDEXED BY idx_thread_requests_answer_due CROSS JOIN thread_requests ps
     WHERE ` + threadAnswerDueSQL("d") + `
       AND ps.target_thread_id = d.caller_thread_id AND ` + agentSpawnLinkSQL("ps") + `
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM held CROSS JOIN thread_requests ps
     WHERE ps.target_thread_id = held.thread AND ` + agentSpawnLinkSQL("ps") + `)`

// agentTargetOpenSQL is whether request alias is a spawn whose thread is
// not at rest as the ledger and the held work know it: a local spawn's link
// with open work below its target, counted by its marks or held
// (agentHeldSQL), its own thread's liveness included, or a remote spawn
// whose computer last reported open work or its thread live. The tree
// reads keep a settled spawn for it, its row shows its worker for it, and
// Past leaves it out for it.
func agentTargetOpenSQL(alias string) string {
	a := alias + "."
	return `((` + agentSpawnLinkSQL(alias) + ` AND (` + agentOpenBelowSQL(alias) + ` OR ` + a + `token IN (SELECT token FROM held)))
         OR (` + nestedRemoteSpawnSQL(alias) + `))`
}

// agentHeldParam encodes the tree reads' ?2, the open work the app holds in
// memory: the requests a tool call is parked on, by token, and the threads
// that are not at rest, by id.
func agentHeldParam(waiting, live []string) (string, error) {
	if waiting == nil {
		waiting = []string{}
	}
	if live == nil {
		live = []string{}
	}
	encoded, err := json.Marshal(struct {
		Waiting []string `json:"waiting"`
		Live    []string `json:"live"`
	}{waiting, live})
	if err != nil {
		return "", fmt.Errorf("encode the held work: %w", err)
	}
	return string(encoded), nil
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
// predicate of idx_thread_requests_spawn_link: its link into the tree,
// whether or not the retention floor cleared the spawn's payload
// (lineage_only), so new work its target opens after the sweep still nests
// under the spawn's caller.
func agentSpawnLinkSQL(alias string) string {
	a := alias + "."
	return a + `kind = 'spawn' AND ` + a + `target_computer_id = '' AND ` + a + `superseded = 0 AND ` + a + `target_thread_id <> ''`
}

// spawnerOfSQL is the caller of the link of the thread the expression
// target names, NULL for a thread no local spawn reaches: the one owner
// every read of a thread's spawner names, its row, its prompts' route, its
// owner's archive and an answer's read mark alike. A superseded spawn is
// no owner.
func spawnerOfSQL(target string) string {
	return `(SELECT link.caller_thread_id FROM thread_requests AS link
	          WHERE ` + agentSpawnLinkSQL("link") + ` AND link.target_thread_id = ` + target + `)`
}

// SpawnLinkOf names the local spawn that reaches threadID, its link, by
// token, in one search of idx_thread_requests_spawn_link. Found is false
// for a thread no local spawn made. A link the retention floor reduced to
// its lineage is still found.
func (s *Store) SpawnLinkOf(threadID string) (token string, found bool, err error) {
	err = s.reader().QueryRow(spawnLinkOfSQL, threadID).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read the spawn that made %s: %w", threadID, err)
	}
	return token, true, nil
}

var spawnLinkOfSQL = `SELECT link.token FROM thread_requests AS link
	WHERE ` + agentSpawnLinkSQL("link") + ` AND link.target_thread_id = ?1`

// agentOpenBelowSQL is whether a local spawn has open work under its
// target within agentOpenDepth, the term of idx_thread_requests_open_below.
func agentOpenBelowSQL(alias string) string {
	a := alias + "."
	return `(` + a + `open_below_0 + ` + a + `open_below_1 + ` + a + `open_below_2 + ` + a + `open_below_3) > 0`
}

// agentOpenWorkSQL matches a request that is open work under a spawn: an
// open request the listing counts (a spawn, or a send or ask whose wake is
// armed), or a spawn on another computer that last reported open work
// there. The marks count these (migration v155).
func agentOpenWorkSQL(alias string) string {
	a := alias + "."
	return `((` + a + `lineage_only = 0 AND ` + a + `settled_at IS NULL AND (` + a + `kind = 'spawn' OR (` + a + `kind IN ('send','ask') AND ` + a + `notify = 1)))
        OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1))`
}

// agentOpenUnderSQL is whether thread, or a thread up to agentOpenDepth
// spawns below it, made open work: a request thread made that is open work
// (agentOpenWorkSQL), or a local spawn it made with open work under that
// spawn's target (agentOpenBelowSQL). Each arm is read through its index.
func agentOpenUnderSQL(thread string) string {
	return `(EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_open_created
              WHERE o.caller_thread_id = ` + thread + ` AND o.lineage_only = 0 AND o.settled_at IS NULL AND ` + agentOpenWorkSQL("o") + `)
           OR EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_nested_caller
              WHERE o.caller_thread_id = ` + thread + ` AND ` + nestedRemoteSpawnSQL("o") + `)
           OR EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_open_below
              WHERE o.caller_thread_id = ` + thread + ` AND ` + agentSpawnLinkSQL("o") + ` AND ` + agentOpenBelowSQL("o") + `))`
}

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

// agentOpenBelowThreadSQL is whether thread has open work at any depth
// below it, as agentTargetOpenSQL reads it for a spawn of it: a request it
// made that is open work, or a local spawn it made whose marks count open
// work, within agentOpenDepth (agentOpenUnderSQL), or held work it keeps
// (agentHeldSQL), deeper or only in memory. The statement defines `held`.
func agentOpenBelowThreadSQL(thread string) string {
	return `(` + agentOpenUnderSQL(thread) + ` OR EXISTS (SELECT 1 FROM held WHERE held.thread = ` + thread + `))`
}

// agentRowKindSQL matches a request kind the listing shows: every spawn,
// and a send or ask whose wake is armed or that a call is parked on (?2).
func agentRowKindSQL(alias string) string {
	a := alias + "."
	return a + `kind IN ('spawn','send','ask')
       AND (` + a + `kind = 'spawn' OR ` + a + `notify = 1 OR ` + a + `token IN (SELECT value FROM json_each(?2, '$.waiting')))`
}

// agentRowListedSQL is agentRowArms as one predicate, for a row already
// found by its key.
func agentRowListedSQL(alias string) string {
	a := alias + "."
	return `((` + a + `lineage_only = 0 AND (` + a + `settled_at IS NULL OR ` + a + `settled_at >= ` + agentWindowSQL + `))
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1)
         OR (` + agentSpawnLinkSQL(alias) + ` AND (` + agentOpenBelowSQL(alias) + ` OR ` + a + `token IN (SELECT token FROM held))))`
}

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

// MaxPastAgentRowsPage bounds one page of ListThreadPastAgentRequests.
const MaxPastAgentRowsPage = 50

// ErrPastAgentRowsPage is a page size outside 1 to MaxPastAgentRowsPage.
var ErrPastAgentRowsPage = fmt.Errorf("store: a page of past agent rows holds 1 to %d rows", MaxPastAgentRowsPage)

// PastAgentRowsCursor is where a page of past agent rows starts: after the
// row it names, in page order. The zero cursor starts at the newest.
type PastAgentRowsCursor struct {
	SettledAt int64  `json:"settledAt"`
	Token     string `json:"token"`
}

// PastAgentRequests is one page of a thread's settled agent rows.
type PastAgentRequests struct {
	Requests []ThreadAgentRequest
	// Total is how many settled rows the thread has, this page's included.
	Total int
	// Next starts the following page; nil on the last.
	Next *PastAgentRowsCursor
}

// pastAgentRowSQL is the request kinds a thread's past agent rows hold:
// every spawn and ask, and a send whose wake was armed, less the spawns
// whose thread is not at rest (agentTargetOpenSQL, with ?2 the held work).
// A lineage row the retention sweep kept is one of them, with what the
// sweep cleared gone.
var pastAgentRowSQL = `r.caller_thread_id = ?1 AND r.settled_at IS NOT NULL
	   AND (r.kind IN ('spawn','ask') OR (r.kind = 'send' AND r.notify = 1))
	   AND NOT ` + agentTargetOpenSQL("r")

// ListThreadPastAgentRequests lists one page of the requests callerThreadID
// made that its Agents segment lists as past: settled, newest settled
// first, ties by token descending, from after the cursor, at most limit (1
// to MaxPastAgentRowsPage), with the first answerHead bytes of each answer
// and the count of them all. A settled spawn whose thread is not at rest
// is still open, and left out: waiting and live are the held work, as
// ThreadAgentTreeRead's. The page and the count are one read, through
// idx_thread_requests_caller_settled, so the page's cost follows its rows;
// the count's follows the caller's settled history, which the retention
// floor bounds to a month of requests and the lineage of the spawns past
// it.
func (s *Store) ListThreadPastAgentRequests(callerThreadID string, after PastAgentRowsCursor, limit, answerHead int, waiting, live []string) (page PastAgentRequests, err error) {
	if limit < 1 || limit > MaxPastAgentRowsPage {
		return PastAgentRequests{}, ErrPastAgentRowsPage
	}
	action := "store: list the past agents of " + callerThreadID
	if answerHead < 0 {
		return PastAgentRequests{}, fmt.Errorf("%s: answer head %d", action, answerHead)
	}
	settledBefore, tokenBefore := after.SettledAt, after.Token
	if after == (PastAgentRowsCursor{}) {
		settledBefore = math.MaxInt64
	}
	held, err := agentHeldParam(waiting, live)
	if err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: %w", action, err)
	}
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	if err := tx.QueryRow(`WITH RECURSIVE `+agentHeldSQL+` SELECT count(*) FROM thread_requests r WHERE `+pastAgentRowSQL, callerThreadID, held).Scan(&page.Total); err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: count: %w", action, err)
	}
	page.Requests = []ThreadAgentRequest{}
	if page.Total == 0 {
		return page, nil
	}
	rows, err := tx.Query(pastAgentRowsSQL(limit+1), callerThreadID, held, settledBefore, tokenBefore, answerHead)
	if err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var row ThreadAgentRequest
		var late int
		row.ThreadRequest, err = scanThreadRequestWith(rows, &row.AnswerHead, &late, &row.WakeItemID)
		if err != nil {
			return PastAgentRequests{}, fmt.Errorf("%s: scan: %w", action, err)
		}
		row.HasLateReply = late != 0
		page.Requests = append(page.Requests, row)
	}
	if err := rows.Err(); err != nil {
		return PastAgentRequests{}, fmt.Errorf("%s: iterate: %w", action, err)
	}
	if len(page.Requests) > limit {
		page.Requests = page.Requests[:limit]
		last := page.Requests[limit-1]
		page.Next = &PastAgentRowsCursor{SettledAt: last.SettledAt, Token: last.Token}
	}
	return page, nil
}

// pastAgentRowsSQL reads a page of past agent rows: ?1 is the caller, ?2
// the held work, ?3 and ?4 the cursor's settled time and token, ?5 the
// answer head (where the tree reads put it).
func pastAgentRowsSQL(limit int) string {
	return `WITH RECURSIVE ` + agentHeldSQL + `
SELECT ` + threadAgentRequestColumns + `
  FROM thread_requests r INDEXED BY idx_thread_requests_caller_settled
 WHERE ` + pastAgentRowSQL + ` AND (r.settled_at, r.token) < (?3, ?4)
 ORDER BY r.settled_at DESC, r.token DESC LIMIT ` + strconv.Itoa(limit)
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
    r.late_reply_agent, r.late_reply_agent_role, r.rest_unreported, ` + wakeItemSQL

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
