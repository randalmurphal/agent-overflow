package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The predicates the Agents reads share: the arms a caller's listed rows are
// read through, the held work the marks do not count, a spawn's link into the
// tree, and the open work below a thread.

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

// agentRowArms are the requests of caller the listing keeps as six
// disjoint arms, each read through the index whose predicate it states:
// open (idx_thread_requests_open_created, in tree order); settled inside
// the window (idx_thread_requests_caller_settled); a local spawn settled
// before it with open work under it, stored (idx_thread_requests_open_below)
// or only held in memory, a parked request or a live thread (`held`, by
// token); a spawn on another computer that last reported open work
// (idx_thread_requests_nested_caller); a send whose run is open on a thread
// not at rest (idx_thread_requests_run_open). Each arm takes one request
// per run (runFirstUnsettledSQL, runLastSettledSQL), so a run of many
// requests takes one place in an arm's limit. None reads the caller's
// settled history, and only the open arm can hold many rows. The stored and
// remote arms are held to their index, and the held arm's other terms are
// kept off theirs: otherwise the planner reads them through
// idx_thread_requests_caller_settled from the history's settled_at range
// (TestThreadAgentTreeSearchesItsArms).
func agentRowArms(alias, caller string) []agentArm {
	a := alias + "."
	by := a + `caller_thread_id = ` + caller + ` AND `
	return []agentArm{
		{"idx_thread_requests_open_created", false, by + a + `lineage_only = 0 AND ` + a + `settled_at IS NULL AND ` + runFirstUnsettledSQL(alias)},
		{"idx_thread_requests_caller_settled", false, by + a + `lineage_only = 0 AND ` + a + `settled_at >= ` + agentWindowSQL + ` AND ` + runLastSettledSQL(alias)},
		{"idx_thread_requests_open_below", true, by + agentSpawnLinkSQL(alias) + ` AND ` + agentOpenBelowSQL(alias) + ` AND ` + a + `settled_at < ` + agentWindowSQL +
			` AND ` + spawnActiveSQL(alias)},
		{"sqlite_autoindex_thread_requests_1", false, a + `token IN (SELECT token FROM held WHERE thread = ` + caller + `) AND +` + a + `caller_thread_id = ` + caller +
			` AND NOT ` + agentOpenBelowSQL(alias) + ` AND +` + a + `settled_at < ` + agentWindowSQL + ` AND ` + spawnActiveSQL(alias)},
		{"idx_thread_requests_nested_caller", true, by + nestedRemoteSpawnSQL(alias) + ` AND ` + a + `settled_at < ` + agentWindowSQL + ` AND ` + spawnActiveSQL(alias)},
		{"idx_thread_requests_run_open", true, by + runOpenSQL(alias) + ` AND ` + a + `kind = 'send' AND ` + a + `lineage_only = 0 AND +` + a + `settled_at < ` + agentWindowSQL +
			` AND ` + sendTargetOpenSQL(alias) + ` AND ` + runLastSettledSQL(alias)},
	}
}

// runFirstUnsettledSQL matches an unsettled request alias that is its run's
// oldest unsettled request, or tracks no run: the one request the open arm
// reads of a run (idx_thread_requests_run_token).
func runFirstUnsettledSQL(alias string) string {
	a := alias + "."
	return `(` + a + `run_token IS NULL OR NOT EXISTS (SELECT 1 FROM thread_requests ru INDEXED BY idx_thread_requests_run_token
	          WHERE ru.run_token = ` + a + `run_token AND ru.settled_at IS NULL
	            AND (ru.created_at, ru.token) < (` + a + `created_at, ` + a + `token)))`
}

// runLastSettledSQL matches a settled request alias that stands for its
// run once every request of it settled: the newest settled, ties by token,
// with none unsettled; or one that tracks no run. Each term is one search
// of idx_thread_requests_run_token.
func runLastSettledSQL(alias string) string {
	a := alias + "."
	member := `SELECT 1 FROM thread_requests rs INDEXED BY idx_thread_requests_run_token WHERE rs.run_token = ` + a + `run_token AND `
	return `(` + a + `run_token IS NULL OR (NOT EXISTS (` + member + `rs.settled_at IS NULL)
	          AND NOT EXISTS (` + member + `rs.settled_at > ` + a + `settled_at)
	          AND NOT EXISTS (` + member + `rs.settled_at = ` + a + `settled_at AND rs.token > ` + a + `token)))`
}

// agentNodeSQL is the token of the listing's row that request alias falls
// in: for a send to a thread its caller spawned that tracks a run of the
// caller's, or that tracks none (written before runs were kept) and is
// open, that spawn, here (idx_thread_requests_spawn_link) or on another
// computer (idx_thread_requests_remote_spawn_target), so a worker the
// caller spawned is one row in its spawn's place; else the request itself.
// A settled send that tracks no run is a row of its own.
func agentNodeSQL(alias string) string {
	a := alias + "."
	return `(CASE WHEN ` + a + `kind = 'send' AND (` + a + `run_token IS NOT NULL OR ` + a + `settled_at IS NULL) THEN COALESCE(CASE
	      WHEN ` + a + `target_computer_id = '' THEN (SELECT nl.token FROM thread_requests nl INDEXED BY idx_thread_requests_spawn_link
	            WHERE ` + agentSpawnLinkSQL("nl") + ` AND nl.target_thread_id = ` + a + `target_thread_id
	              AND nl.caller_thread_id = ` + a + `caller_thread_id)
	      ELSE (SELECT nr.token FROM thread_requests nr INDEXED BY idx_thread_requests_remote_spawn_target
	            WHERE nr.kind = 'spawn' AND nr.target_computer_id <> '' AND nr.target_computer_id = ` + a + `target_computer_id
	              AND nr.target_thread_id = ` + a + `target_thread_id AND nr.caller_thread_id = ` + a + `caller_thread_id
	            ORDER BY nr.created_at DESC LIMIT 1) END, ` + a + `token)
	    ELSE ` + a + `token END)`
}

// agentShownSQL is the token of the request the row of node, a request
// agentNodeSQL names, shows. A spawn's row shows its caller's newest run
// on its thread (idx_thread_requests_run_heads, a run's first request):
// that run's oldest unsettled request, else its first while it is open,
// else its newest settled (idx_thread_requests_run_token). When no run is
// recorded it shows the spawn while it is open, else the oldest open send
// of its caller's to its thread that tracks no run (untrackedOpenSendSQL),
// else the spawn. A spawn not yet given a thread, and any other row, shows
// its own request.
func agentShownSQL(node string) string {
	n := node + "."
	return `(CASE WHEN ` + n + `kind = 'spawn' AND ` + n + `target_thread_id <> '' THEN COALESCE((SELECT COALESCE(
	        (SELECT su.token FROM thread_requests su INDEXED BY idx_thread_requests_run_token
	          WHERE su.run_token = h.token AND su.settled_at IS NULL ORDER BY su.created_at, su.token LIMIT 1),
	        CASE WHEN h.run_tool_calls IS NULL THEN h.token END,
	        (SELECT ss.token FROM thread_requests ss INDEXED BY idx_thread_requests_run_token
	          WHERE ss.run_token = h.token AND ss.settled_at IS NOT NULL ORDER BY ss.settled_at DESC, ss.token DESC LIMIT 1),
	        h.token)
	      FROM thread_requests h INDEXED BY idx_thread_requests_run_heads
	     WHERE h.caller_thread_id = ` + n + `caller_thread_id AND h.target_computer_id = ` + n + `target_computer_id
	       AND h.target_thread_id = ` + n + `target_thread_id AND h.run_token = h.token
	     ORDER BY h.created_at DESC LIMIT 1),
	    CASE WHEN ` + n + `settled_at IS NULL THEN ` + n + `token END,
	    (SELECT uo.token FROM thread_requests uo INDEXED BY idx_thread_requests_open_created
	      WHERE ` + untrackedOpenSendSQL("uo", node) + ` ORDER BY uo.created_at, uo.token LIMIT 1),
	    ` + n + `token)
	    ELSE ` + n + `token END)`
}

// untrackedOpenSendSQL matches a request alias that is an open send of the
// caller of spawn node to its thread that tracks no run, one written before
// runs were kept, which a listing reads as a row (agentRowKindSQL). The
// spawn's row stands for it (agentNodeSQL). It states the terms of
// idx_thread_requests_open_created, the caller's open requests.
func untrackedOpenSendSQL(alias, node string) string {
	a, n := alias+".", node+"."
	return a + `caller_thread_id = ` + n + `caller_thread_id AND ` + a + `lineage_only = 0 AND ` + a + `settled_at IS NULL
	        AND ` + a + `kind = 'send' AND ` + a + `run_token IS NULL AND ` + a + `target_computer_id = ` + n + `target_computer_id
	        AND ` + a + `target_thread_id = ` + n + `target_thread_id AND ` + agentRowKindSQL(alias)
}

// agentNodeListedSQL is agentRowListedSQL for node, a request agentNodeSQL
// names: the request is listed, or, for a spawn, a send of its caller's
// newest run on its thread that matches member, alias lm, is
// (idx_thread_requests_run_heads). That run alone can hold an open
// request, and its requests settle after every earlier run's, so no
// earlier run's request is listed without one of it. A spawn is listed for
// an open send to its thread that tracks no run (untrackedOpenSendSQL) as
// well, alias lm too, which is always listed. Below a spawn that does not
// carry its worker the tree reads only done requests, which member states
// as doneRowSQL("lm").
func agentNodeListedSQL(node, member string) string {
	n := node + "."
	return `(` + agentRowListedSQL(node) + ` OR (` + n + `kind = 'spawn' AND ` + n + `target_thread_id <> '' AND EXISTS (
	    SELECT 1 FROM thread_requests lm INDEXED BY idx_thread_requests_open_created
	     WHERE ` + untrackedOpenSendSQL("lm", node) + ` AND ` + member + `)) OR (` + n + `kind = 'spawn' AND ` + n + `target_thread_id <> '' AND EXISTS (
	    SELECT 1 FROM thread_requests lm INDEXED BY idx_thread_requests_run_token
	     WHERE lm.run_token = (SELECT h.token FROM thread_requests h INDEXED BY idx_thread_requests_run_heads
	             WHERE h.caller_thread_id = ` + n + `caller_thread_id AND h.target_computer_id = ` + n + `target_computer_id
	               AND h.target_thread_id = ` + n + `target_thread_id AND h.run_token = h.token
	             ORDER BY h.created_at DESC LIMIT 1)
	       AND lm.kind = 'send' AND ` + agentRowKindSQL("lm") + ` AND ` + agentRowListedSQL("lm") + ` AND ` + member + `)))`
}

// agentDisplayedSQL is the token of the request the row of node shows
// where free says whether the row may show open work: every spawn above it
// carries its worker into the listing (spawnActiveSQL). Below a spawn that
// does not, every row is done (doneRowSQL), and a row whose shown request
// is not shows node, which the tree read found done.
func agentDisplayedSQL(node, free string) string {
	shown := agentShownSQL(node)
	return `(CASE WHEN ` + free + ` OR EXISTS (SELECT 1 FROM thread_requests sh WHERE sh.token = ` + shown + ` AND ` + doneRowSQL("sh") + `)
	    THEN ` + shown + ` ELSE ` + node + `.token END)`
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
// root, each spawn held by its caller, through a spawn only while a run of
// its caller's is open on its thread (spawnActiveSQL): work in a run its
// spawner did not start holds no thread above it. Each link has one caller, so a link
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
     WHERE ps.target_thread_id = json_each.value AND ` + agentSpawnLinkSQL("ps") + ` AND ` + spawnActiveSQL("ps") + `
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM thread_requests f INDEXED BY idx_thread_requests_open_deep CROSS JOIN thread_requests ps
     WHERE ` + agentSpawnLinkSQL("f") + ` AND f.open_below_3 > 0
       AND ps.target_thread_id = f.caller_thread_id AND ` + agentSpawnLinkSQL("ps") + ` AND ` + spawnActiveSQL("ps") + `
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM thread_requests d INDEXED BY idx_thread_requests_answer_due CROSS JOIN thread_requests ps
     WHERE ` + threadAnswerDueSQL("d") + `
       AND ps.target_thread_id = d.caller_thread_id AND ` + agentSpawnLinkSQL("ps") + ` AND ` + spawnActiveSQL("ps") + `
    UNION
    SELECT ps.token, ps.caller_thread_id
      FROM held CROSS JOIN thread_requests ps
     WHERE ps.target_thread_id = held.thread AND ` + agentSpawnLinkSQL("ps") + ` AND ` + spawnActiveSQL("ps") + `)`

// agentTargetOpenSQL is whether request alias has a run open
// (ownRunOpenSQL, runOpenSQL) on a thread not at rest as the ledger and the held
// work know it: a local spawn's link with open work below its target,
// counted by its marks or held (agentHeldSQL), its own thread's liveness
// included, a remote spawn whose computer last reported open work or its
// thread live, or a send whose run is open on such a thread. The tree
// reads keep a settled row for it, its row shows its worker for it, and
// Past leaves it out for it.
func agentTargetOpenSQL(alias string) string {
	a := alias + "."
	return `((` + agentSpawnLinkSQL(alias) + ` AND ` + ownRunOpenSQL(alias) + ` AND (` + agentOpenBelowSQL(alias) + ` OR ` + a + `token IN (SELECT token FROM held)))
         OR (` + nestedRemoteSpawnSQL(alias) + ` AND ` + ownRunOpenSQL(alias) + `)
         OR (` + a + `kind = 'send' AND ` + runOpenSQL(alias) + ` AND ` + sendTargetOpenSQL(alias) + `))`
}

// sendTargetOpenSQL is whether the thread send alias went to is not at rest
// as agentTargetOpenSQL reads it for that thread's own spawn: its local
// link counts open work below it or is held, or its remote spawn's
// computer last reported open work.
func sendTargetOpenSQL(alias string) string {
	a := alias + "."
	return `((` + a + `target_computer_id = '' AND EXISTS (SELECT 1 FROM thread_requests sl
	           WHERE ` + agentSpawnLinkSQL("sl") + ` AND sl.target_thread_id = ` + a + `target_thread_id
	             AND (` + agentOpenBelowSQL("sl") + ` OR sl.token IN (SELECT token FROM held))))
         OR (` + a + `target_computer_id <> '' AND EXISTS (SELECT 1 FROM thread_requests sl
	           WHERE ` + nestedRemoteSpawnSQL("sl") + ` AND sl.target_computer_id = ` + a + `target_computer_id
	             AND sl.target_thread_id = ` + a + `target_thread_id)))`
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
// spawn's target (agentOpenBelowSQL). A spawn counts only while a run of
// thread's is open on its target (spawnActiveSQL). Each arm is read through
// its index.
func agentOpenUnderSQL(thread string) string {
	return `(EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_open_created
              WHERE o.caller_thread_id = ` + thread + ` AND o.lineage_only = 0 AND o.settled_at IS NULL AND ` + agentOpenWorkSQL("o") + `)
           OR EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_nested_caller
              WHERE o.caller_thread_id = ` + thread + ` AND ` + nestedRemoteSpawnSQL("o") + ` AND ` + spawnActiveSQL("o") + `)
           OR EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_open_below
              WHERE o.caller_thread_id = ` + thread + ` AND ` + agentSpawnLinkSQL("o") + ` AND ` + agentOpenBelowSQL("o") + `
                AND ` + spawnActiveSQL("o") + `))`
}

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
       AND (` + a + `kind = 'spawn' OR ` + a + `notify = 1 OR ` + a + `run_base_tool_calls IS NOT NULL
            OR ` + a + `token IN (SELECT value FROM json_each(?2, '$.waiting')))`
}

// agentRowListedSQL is agentRowArms as one predicate, for a row already
// found by its key.
func agentRowListedSQL(alias string) string {
	a := alias + "."
	return `((` + a + `lineage_only = 0 AND (` + a + `settled_at IS NULL OR ` + a + `settled_at >= ` + agentWindowSQL + `))
         OR (` + a + `kind = 'spawn' AND ` + a + `target_computer_id <> '' AND ` + a + `nested_open = 1 AND ` + spawnActiveSQL(alias) + `)
         OR (` + agentSpawnLinkSQL(alias) + ` AND (` + agentOpenBelowSQL(alias) + ` OR ` + a + `token IN (SELECT token FROM held)) AND ` + spawnActiveSQL(alias) + `)
         OR (` + a + `kind = 'send' AND ` + a + `lineage_only = 0 AND ` + runOpenSQL(alias) + ` AND ` + sendTargetOpenSQL(alias) + `))`
}
