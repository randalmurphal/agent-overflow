package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// A request's run is the stretch of its worker's work that its Agents row
// counts (docs/specs/standing-agents.md, "Agents segment"): from when the
// worker left rest for it until the worker is at rest again. A spawn tracks
// one, and so does a send to a thread its caller spawned, the requests that
// wait for their worker's rest (migration v159):
//
//   - The run's base is the worker's tally (thread_tallies, with what the
//     running turn has done so far) when the request is written. A request
//     made while one of its caller's runs on that worker is still open
//     joins that run and takes its base: one row, one run, however many
//     requests went to the worker during it. A spawn's base is zero, its
//     thread new; a paired computer's worker's base is the figures that
//     computer last reported, which the app passes.
//   - The run ends when the worker is at rest after the request settled:
//     in the write that settles a request on its thread's rest
//     (SettleLocalThreadRequestAtRest), by the rest pass for one that settled
//     before its worker rested (EndThreadRequestRunsAtRest), or, for a
//     worker on a paired computer, by an answer of that computer that says
//     the run ended there (receipt_runs.go) or the thread is at rest
//     (EndRemoteThreadRequestRuns). Its totals are the tally then less the
//     base, and never change afterward.
//
// A row with no base tracks no run: one written before v159, or a request
// that waits for no worker's rest. A spawn that tracks none, written before
// v159, is open until it settles and closed for good once it has, whatever
// its worker does later; no other request without a base has a run. The
// rule is RunOpen and RunEnded in Go and ownRunOpenSQL in SQL; the upgrade
// rewrites no row for it.

// RunFigures is what a worker did: top-level tool calls, its own tokens and
// the time its turns ran.
type RunFigures struct {
	ToolCalls int64
	Tokens    int64
	TurnMs    int64
}

// RunEnded reports whether the request's run ended: its worker came to rest
// after it, and its totals are kept, or it is a spawn that tracks no run
// and has settled, which keeps no totals.
func (r ThreadRequest) RunEnded() bool {
	if r.RunBase == nil {
		return r.Kind == ThreadRequestSpawn && r.SettledAt != 0
	}
	return r.RunTotal != nil
}

// RunOpen reports whether the request's run is open: it tracks one that has
// not ended, or it is a spawn that tracks none and has not settled.
func (r ThreadRequest) RunOpen() bool {
	if r.RunBase == nil {
		return r.Kind == ThreadRequestSpawn && r.SettledAt == 0
	}
	return r.RunTotal == nil
}

// requestRunColumns is the run's eight columns in scan order.
const requestRunColumns = `run_base_tool_calls, run_base_tokens, run_base_turn_ms, run_tool_calls, run_tokens, run_turn_ms, COALESCE(run_token, ''), nudge`

// runFiguresOf is three scanned columns as figures, nil when the first is
// NULL.
func runFiguresOf(cols []sql.NullInt64) *RunFigures {
	if !cols[0].Valid {
		return nil
	}
	return &RunFigures{ToolCalls: cols[0].Int64, Tokens: cols[1].Int64, TurnMs: cols[2].Int64}
}

// runOpenSQL matches a request whose run is open, the predicate of
// idx_thread_requests_run_open.
func runOpenSQL(alias string) string {
	return alias + `.run_base_tool_calls IS NOT NULL AND ` + alias + `.run_tool_calls IS NULL`
}

// ownRunOpenSQL matches a spawn alias whose own run is open, as RunOpen
// reads it: its run has not ended, or it tracks none and has not settled.
// A spawn whose run ended, or that tracks none and settled, is closed,
// whatever its worker does later.
func ownRunOpenSQL(alias string) string {
	a := alias + "."
	return `((` + a + `run_base_tool_calls IS NULL AND ` + a + `settled_at IS NULL) OR (` +
		a + `run_base_tool_calls IS NOT NULL AND ` + a + `run_tool_calls IS NULL))`
}

// callerRunOpenSQL matches a spawn whose caller has a later run open on its
// worker: a send of the caller's to the same thread whose run is open. The
// spawn's link then still nests its worker's rows, under that run's row.
func callerRunOpenSQL(alias string) string {
	a := alias + "."
	return `EXISTS (SELECT 1 FROM thread_requests ro INDEXED BY idx_thread_requests_run_open
	          WHERE ro.caller_thread_id = ` + a + `caller_thread_id AND ` + runOpenSQL("ro") + `
	            AND ro.kind = 'send' AND ro.lineage_only = 0
	            AND ro.target_computer_id = ` + a + `target_computer_id AND ro.target_thread_id = ` + a + `target_thread_id)`
}

// spawnActiveSQL matches a spawn whose link carries its worker into its
// caller's listing: its own run is open (ownRunOpenSQL), or its caller has a
// later run open on the worker. A spawn whose run ended, its worker since
// continued by someone else, is not kept for that work and nests only done
// rows (doneRowSQL).
func spawnActiveSQL(alias string) string {
	return `(` + ownRunOpenSQL(alias) + ` OR ` + callerRunOpenSQL(alias) + `)`
}

// doneRowSQL matches a request alias that is done: settled, with no open
// work at or below its thread (agentTargetOpenSQL). A spawn whose run ended
// nests only done rows, at any depth: its run left nothing open when it
// ended, so an open row below it is work someone else started since.
func doneRowSQL(alias string) string {
	return `(` + alias + `.settled_at IS NOT NULL AND NOT ` + agentTargetOpenSQL(alias) + `)`
}

// tallyFiguresSQL is the three figures of the tally of the thread id names
// at now (milliseconds), as threadTallyColumnsSQL reads them.
func tallyFiguresSQL(id, now string) [3]string {
	open := `(SELECT started_at FROM turns WHERE thread_id = ` + id + ` AND completed_at IS NULL ORDER BY turn_index DESC LIMIT 1)`
	return [3]string{
		`COALESCE((SELECT tool_calls FROM thread_tallies WHERE thread_id = ` + id + `), 0)`,
		`(COALESCE((SELECT tokens FROM thread_tallies WHERE thread_id = ` + id + `), 0)
		  + COALESCE((SELECT SUM(` + usageSpendSQLPlain + `) FROM usage_pending WHERE thread_id = ` + id + `), 0))`,
		`(COALESCE((SELECT turn_ms FROM thread_tallies WHERE thread_id = ` + id + `), 0)
		  + COALESCE(MAX(0, ` + now + ` - ` + open + `), 0))`,
	}
}

// insertThreadRequestSQL writes a request with the start of its run,
// ?1 to ?20 its columns, ?21 to ?23 a given base, ?24 whether a local send
// tracks its run, ?25 now. The base is that of the caller's open run on
// the same worker when one is open (the newest), so the request joins it;
// else the given base; else, for a local send to a thread its caller
// spawned, the target's tally now; else none, no run. A request that joins
// a run records that run's first request (run_token); one that starts a
// run records its own token.
var insertThreadRequestSQL = func() string {
	tally := tallyFiguresSQL("?6", "?25")
	return `INSERT INTO thread_requests (
		    token, caller_thread_id, kind, due_at, target_computer_id, target_thread_id,
		    origin_thread_id, notify, state, answer, answer_kind, revision,
		    next_check, attempts, issue, lineage_only, created_at, updated_at, admit_until, target_computer_name,
		    run_base_tool_calls, run_base_tokens, run_base_turn_ms, run_token)
		 SELECT ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18, ?19, ?20,
		        base.tc, base.tk, base.ms, CASE WHEN base.tc IS NOT NULL THEN COALESCE(base.rt, ?1) END
		   FROM (SELECT 1) LEFT JOIN (
		        SELECT tc, tk, ms, rt FROM (
		            SELECT o.run_base_tool_calls AS tc, o.run_base_tokens AS tk, o.run_base_turn_ms AS ms, o.run_token AS rt, 0 AS pick, o.created_at AS at
		              FROM thread_requests o INDEXED BY idx_thread_requests_run_open
		             WHERE ?3 IN ('spawn', 'send') AND ?6 <> '' AND ?16 = 0
		               AND o.caller_thread_id = ?2 AND ` + runOpenSQL("o") + `
		               AND o.target_computer_id = ?5 AND o.target_thread_id = ?6 AND o.lineage_only = 0
		            UNION ALL
		            SELECT ?21, ?22, ?23, NULL, 1, 0 WHERE ?21 IS NOT NULL
		            UNION ALL
		            SELECT ` + tally[0] + `, ` + tally[1] + `, ` + tally[2] + `, NULL, 2, 0
		             WHERE ?21 IS NULL AND ?24 = 1 AND ?3 = 'send' AND ?5 = '' AND ?6 <> ''
		               AND ` + spawnerOfSQL("?6") + ` = ?2)
		         ORDER BY pick, at DESC LIMIT 1) AS base`
}()

// endedRun is a request whose run a write ended.
type endedRun struct {
	token, caller string
}

// endThreadRunsTx ends the open runs of the settled requests to thread on
// this computer, their totals the tally now less their bases, and those of
// the settled receipts of paired computers' requests to it
// (endReceiptRunsTx).
func endThreadRunsTx(tx *sql.Tx, threadID string) ([]endedRun, error) {
	now := nowMillis()
	tally := tallyFiguresSQL("?1", "?2")
	runs, err := endRunsTx(tx, `target_computer_id = '' AND target_thread_id = ?1`,
		[3]string{tally[0], tally[1], tally[2]}, threadID, now)
	if err != nil {
		return nil, err
	}
	if err := endReceiptRunsTx(tx, threadID, now); err != nil {
		return nil, err
	}
	return runs, nil
}

// endRunsTx ends the open runs of the settled requests where matches, with
// totals the figures less each one's base.
func endRunsTx(tx *sql.Tx, where string, figures [3]string, args ...any) (_ []endedRun, err error) {
	rows, err := tx.Query(`UPDATE thread_requests
		    SET run_tool_calls = MAX(0, `+figures[0]+` - run_base_tool_calls),
		        run_tokens = MAX(0, `+figures[1]+` - run_base_tokens),
		        run_turn_ms = MAX(0, `+figures[2]+` - run_base_turn_ms)
		  WHERE `+where+` AND `+runOpenSQL("thread_requests")+` AND settled_at IS NOT NULL
		  RETURNING token, caller_thread_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []endedRun
	for rows.Next() {
		var run endedRun
		if err := rows.Scan(&run.token, &run.caller); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// A nudge is a message the person or a thread other than its spawner sends
// an owned worker outside every run of its spawner's: an owned worker is its
// spawner's sub-agent always, so the work it starts is a run of the
// spawner's row (docs/specs/standing-agents.md, "Owned workers"). The run
// is recorded on a row of its own, `nudge` = 1: a send of the spawner's to
// the worker that nobody made, settled `finished` when written, owed
// nothing and delivering nothing (notify 0), its base the worker's tally
// then. It ends at the worker's rest as any settled request's run does, or
// when the worker gets a sidebar row (endNudgeRunsTx). A request of the
// spawner's made while it is open joins it, as any request joins its
// caller's open run on the worker.

// StartNudgeRun records the run a nudge of threadID by byThreadID (empty
// for the person) starts, as token, when threadID is an owned worker whose
// spawner here is not archived, is not byThreadID, and has no run open on
// it: the nudge then joins that run. A worker a paired computer's spawn
// owns has its nudge run recorded on that spawn's link by the same rule
// (startPeerNudgeRun). It reports whether it recorded one. A message that
// is no nudge, the common case, costs one read and no write.
func (s *Store) StartNudgeRun(token, threadID, byThreadID string) (bool, error) {
	var nudges, peer bool
	if err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 `+nudgeLinkSQL("?1", "?2")+`),
		    EXISTS (SELECT 1 `+peerNudgeLinkSQL("?1", "?2")+`)`, threadID, byThreadID).Scan(&nudges, &peer); err != nil {
		return false, fmt.Errorf("store: read whether a message to %s is a nudge: %w", threadID, err)
	}
	if peer {
		return s.startPeerNudgeRun(token, threadID, byThreadID)
	}
	if !nudges {
		return false, nil
	}
	now := nowMillis()
	tally := tallyFiguresSQL("?2", "?3")
	// The write states the read's terms again, so a change between them
	// records nothing.
	rows, err := s.db.Query(`INSERT INTO thread_requests (
		    token, caller_thread_id, kind, target_computer_id, target_thread_id, notify, state, answer_kind,
		    created_at, updated_at, settled_at, run_base_tool_calls, run_base_tokens, run_base_turn_ms, run_token, nudge)
		 SELECT ?1, link.caller_thread_id, 'send', '', ?2, 0, 'finished', '', ?3, ?3, ?3,
		        `+tally[0]+`, `+tally[1]+`, `+tally[2]+`, ?1, 1
		 `+nudgeLinkSQL("?2", "?4")+`
		 RETURNING caller_thread_id`, token, threadID, now, byThreadID)
	if err != nil {
		return false, fmt.Errorf("store: record the run a nudge of %s starts: %w", threadID, err)
	}
	var caller string
	started := rows.Next()
	if started {
		err = rows.Scan(&caller)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return false, fmt.Errorf("store: record the run a nudge of %s starts: %w", threadID, err)
	}
	if started {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token, CallerThreadID: caller})
	}
	return started, nil
}

// nudgeLinkSQL is the FROM and WHERE of the spawn link a message to thread
// from by is a nudge through (StartNudgeRun), as `link`: the worker is
// owned, its spawner here is not archived and is not by, and no run of the
// spawner's is open on it (idx_thread_requests_spawn_link).
func nudgeLinkSQL(thread, by string) string {
	return `FROM thread_requests AS link INDEXED BY idx_thread_requests_spawn_link
		   CROSS JOIN threads AS w ON w.id = link.target_thread_id
		   CROSS JOIN threads AS sp ON sp.id = link.caller_thread_id
		  WHERE ` + agentSpawnLinkSQL("link") + ` AND link.target_thread_id = ` + thread + ` AND w.placement = 'owned'
		    AND sp.archived = 0 AND link.caller_thread_id <> ` + by + ` AND NOT ` + spawnActiveSQL("link")
}

// DropNudgeRun deletes the run StartNudgeRun recorded as token, for a
// nudge that started no work, unless a request of the spawner's joined it
// since. A nudge run on a paired computer's link is cleared from it
// (dropPeerNudgeRun).
func (s *Store) DropNudgeRun(token string) error {
	if err := s.dropPeerNudgeRun(token); err != nil {
		return err
	}
	rows, err := s.db.Query(`DELETE FROM thread_requests
		 WHERE token = ?1 AND nudge = 1 AND run_tool_calls IS NULL
		   AND NOT EXISTS (SELECT 1 FROM thread_requests j INDEXED BY idx_thread_requests_run_token
		                    WHERE j.run_token = ?1 AND j.token <> ?1)
		 RETURNING caller_thread_id`, token)
	if err != nil {
		return fmt.Errorf("store: drop the nudge run %s: %w", token, err)
	}
	var caller string
	dropped := rows.Next()
	if dropped {
		err = rows.Scan(&caller)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("store: drop the nudge run %s: %w", token, err)
	}
	if dropped {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: token, CallerThreadID: caller})
	}
	return nil
}

// endNudgeRunsTx ends the open nudge runs on threadID now, whatever its
// rest, when it is no owned worker: a worker given a sidebar row is no
// longer its spawner's outside the runs the spawner started. That covers
// the nudge run a paired computer's link here keeps for it.
func endNudgeRunsTx(tx *sql.Tx, threadID string) ([]endedRun, error) {
	now := nowMillis()
	const unowned = `NOT EXISTS (SELECT 1 FROM threads nw WHERE nw.id = ?1 AND nw.placement = 'owned')`
	if err := endReceiptNudgesTx(tx, `target_thread_id = ?1 AND `+unowned, threadID, now); err != nil {
		return nil, err
	}
	tally := tallyFiguresSQL("?1", "?2")
	return endRunsTx(tx, `target_computer_id = '' AND target_thread_id = ?1 AND nudge = 1 AND `+unowned,
		[3]string{tally[0], tally[1], tally[2]}, threadID, now)
}

// noteRunsEnded reports each request whose run ended.
func (s *Store) noteRunsEnded(runs []endedRun) {
	for _, run := range runs {
		s.noteThreadRequestChanged(ThreadRequestChange{Token: run.token, CallerThreadID: run.caller})
	}
}

// EndThreadRequestRunsAtRest ends the open runs of the settled requests to
// threadID, provided rest, a guard over threadID, reads it at rest inside
// the write's transaction. It reports whether the guard held.
func (s *Store) EndThreadRequestRunsAtRest(threadID string, rest *RestGuard) (_ bool, err error) {
	action := "store: end the runs on " + threadID
	if rest == nil {
		return false, fmt.Errorf("%s: a run ends only on its worker's rest", action)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	rested, err := rest.atRestTx(tx, s.run)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if len(rested) == 0 {
		return false, nil
	}
	runs, err := endThreadRunsTx(tx, threadID)
	if err != nil {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteRunsEnded(runs)
	return true, nil
}

// OpenRunThreads lists the threads on this computer that requests with an
// open run went to, this computer's or, on their receipts, a paired
// computer's, with one of those requests settled, for the rest pass: a
// settled one's run ends on its worker's rest. A nudge's open run on a
// paired computer's link counts as settled. Any is false when no run is
// open on this computer at all.
func (s *Store) OpenRunThreads() (settled []string, any bool, err error) {
	rows, err := s.reader().Query(`SELECT thread, MAX(settled) FROM (
		  SELECT target_thread_id AS thread, settled_at IS NOT NULL AS settled
		    FROM thread_requests INDEXED BY idx_thread_requests_run_open
		   WHERE ` + runOpenSQL("thread_requests") + ` AND target_computer_id = '' AND target_thread_id <> ''
		  UNION ALL
		  SELECT target_thread_id, settled_at IS NOT NULL
		    FROM thread_request_receipts INDEXED BY idx_thread_request_receipts_run_open
		   WHERE run_open = 1 AND target_thread_id IS NOT NULL
		  UNION ALL
		  SELECT target_thread_id, 1
		    FROM thread_request_receipts INDEXED BY idx_thread_request_receipts_nudge_open
		   WHERE nudge_open = 1 AND target_thread_id IS NOT NULL)
		 GROUP BY thread`)
	if err != nil {
		return nil, false, fmt.Errorf("store: list the open runs: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var thread string
		var anySettled bool
		if err := rows.Scan(&thread, &anySettled); err != nil {
			return nil, false, fmt.Errorf("store: scan the open runs: %w", err)
		}
		any = true
		if anySettled {
			settled = append(settled, thread)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: list the open runs: %w", err)
	}
	return settled, any, nil
}

// CallerRunOpen reports whether callerThreadID has a run open on threadID
// on computerID (empty for this computer), in one search of
// idx_thread_requests_run_open.
func (s *Store) CallerRunOpen(callerThreadID, computerID, threadID string) (bool, error) {
	var open bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_requests ro INDEXED BY idx_thread_requests_run_open
		 WHERE ro.caller_thread_id = ?1 AND `+runOpenSQL("ro")+`
		   AND ro.target_computer_id = `+textParam("?2")+` AND ro.target_thread_id = ?3 AND ro.lineage_only = 0)`,
		callerThreadID, computerID, threadID).Scan(&open)
	if err != nil {
		return false, fmt.Errorf("store: read whether %s has a run open on %s: %w", callerThreadID, threadID, err)
	}
	return open, nil
}

// EndRemoteThreadRequestRuns ends the open runs of the settled requests to
// threadID on computerID, whose computer answered that the thread is at
// rest having done figures over its life. A nudge's run there ends by that
// computer's word on the nudge alone (RecordRemoteNudgeRun).
func (s *Store) EndRemoteThreadRequestRuns(computerID, threadID string, figures RunFigures) (err error) {
	action := "store: end the runs on " + threadID + " on " + computerID
	if computerID == "" || threadID == "" {
		return nil
	}
	// Most answers find no run open on the thread: one read of the open
	// runs' index spares them the write.
	var open bool
	if err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_requests INDEXED BY idx_thread_requests_run_open
		 WHERE `+runOpenSQL("thread_requests")+` AND target_computer_id = `+textParam("?1")+` AND target_thread_id = ?2 AND nudge = 0)`,
		computerID, threadID).Scan(&open); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !open {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	runs, err := endRunsTx(tx, `target_computer_id = `+textParam("?1")+` AND target_thread_id = ?2 AND nudge = 0`,
		[3]string{"?3", "?4", "?5"}, computerID, threadID, figures.ToolCalls, figures.Tokens, figures.TurnMs)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteRunsEnded(runs)
	return nil
}

// RecordRemoteNudgeRun records the nudge run the paired computer that holds
// the thread of spawnToken, a spawn from here, reported on the spawn's link
// there (ReceiptRunOf): the row StartNudgeRun writes for a worker here, of
// the spawn's caller, its base what the worker had done when the run began
// there. A nudge already recorded only has its run ended, once nudge.End
// says it ended, with those figures less its base, as a run ends at the
// worker's rest. Nothing is recorded for an archived caller, or for a nudge
// that began before the retention floor, which the sweep would remove
// again. It reports whether it wrote. Most answers repeat a nudge already
// recorded: one read by key spares them the write.
func (s *Store) RecordRemoteNudgeRun(spawnToken string, nudge NudgeRun) (_ bool, err error) {
	action := "store: record the nudge run " + nudge.Token + " of spawn " + spawnToken
	if nudge.Token == "" {
		return false, nil
	}
	var open bool
	err = s.reader().QueryRow(`SELECT run_tool_calls IS NULL FROM thread_requests WHERE token = ?1 AND nudge = 1`, nudge.Token).Scan(&open)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%s: %w", action, err)
	}
	if found && (!open || nudge.End == nil) {
		return false, nil
	}
	now := nowMillis()
	if !found && nudge.StartedAt < now-int64(ThreadRequestRetentionDays)*24*60*60*1000 {
		return false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer rollbackTx(tx, action, &err)
	var runs []endedRun
	if found {
		runs, err = endRunsTx(tx, `token = ?1 AND nudge = 1`, [3]string{"?2", "?3", "?4"},
			nudge.Token, nudge.End.ToolCalls, nudge.End.Tokens, nudge.End.TurnMs)
		if err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
	} else {
		var end []any
		if e := nudge.End; e != nil {
			end = []any{max(0, e.ToolCalls-nudge.Base.ToolCalls), max(0, e.Tokens-nudge.Base.Tokens), max(0, e.TurnMs-nudge.Base.TurnMs)}
		} else {
			end = []any{nil, nil, nil}
		}
		at := min(nudge.StartedAt, now)
		rows, err := tx.Query(`INSERT INTO thread_requests (
			    token, caller_thread_id, kind, target_computer_id, target_thread_id, target_thread_title, target_provider,
			    target_computer_name, notify, state, answer_kind, created_at, updated_at, settled_at,
			    run_base_tool_calls, run_base_tokens, run_base_turn_ms, run_tool_calls, run_tokens, run_turn_ms, run_token, nudge)
			 SELECT ?1, sp.caller_thread_id, 'send', sp.target_computer_id, sp.target_thread_id, sp.target_thread_title, sp.target_provider,
			        sp.target_computer_name, 0, 'finished', '', ?3, ?4, ?3, ?5, ?6, ?7, ?8, ?9, ?10, ?1, 1
			   FROM thread_requests sp CROSS JOIN threads c ON c.id = sp.caller_thread_id
			  WHERE sp.token = ?2 AND sp.kind = 'spawn' AND sp.target_computer_id <> '' AND sp.target_thread_id <> '' AND c.archived = 0
			 ON CONFLICT(token) DO NOTHING
			 RETURNING token, caller_thread_id`,
			nudge.Token, spawnToken, at, now, nudge.Base.ToolCalls, nudge.Base.Tokens, nudge.Base.TurnMs, end[0], end[1], end[2])
		if err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
		for rows.Next() {
			var run endedRun
			if err := rows.Scan(&run.token, &run.caller); err != nil {
				_ = rows.Close()
				return false, fmt.Errorf("%s: scan: %w", action, err)
			}
			runs = append(runs, run)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return false, fmt.Errorf("%s: %w", action, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", action, err)
	}
	s.noteRunsEnded(runs)
	return len(runs) > 0, nil
}
