package store

import (
	"encoding/json"
	"fmt"
)

// spawnLineageSQL matches a spawn request whose thread may still exist: a
// local one whose thread does, or one on another computer that named its
// thread there. The row is the only record here of which thread started
// that one: a local one names a spawned thread's owner and where its
// prompts go, and either links its thread into its owner's Agents tree, so
// its lineage lives as long as the thread it made. Past the retention floor
// the sweep keeps only that lineage (clearLineagePayloadSQL). ON DELETE
// CASCADE on the caller takes it when the spawner goes. A local thread
// deleted on its own leaves a lineage row nothing reads, which the sweep
// deletes (orphanLineageSQL); a remote one goes when its computer refuses
// the thread as gone (DeleteRemoteSpawnLineage) or is forgotten
// (ForgetRemoteSpawnsOnComputer).
const spawnLineageSQL = `kind = 'spawn' AND target_thread_id <> ''
			AND (target_computer_id <> '' OR EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_requests.target_thread_id))`

// clearLineagePayloadSQL marks a lineage row lineage_only, empties
// everything it answered and disarms its wake, so past the floor the row
// names its caller, kind, target, state and times and nothing else: the
// ledger reads skip it, an old answer is not kept for the life of the
// thread, a remote spawn is no longer polled, and a wake it never handed
// over is not delivered empty. The
// delivery marks stay, because a cleared delivered_at would read as a wake
// still owed. The sweep appends its floor to the WHERE, which skips rows
// already marked.
const clearLineagePayloadSQL = `UPDATE thread_requests
	SET lineage_only = 1, answer = NULL, answer_kind = '', answer_cause = '', answer_cause_arg = '', late_reply = NULL, late_reply_at = NULL,
	    late_reply_expired = 0, answer_agent = 0, answer_agent_role = '', late_reply_agent = 0, late_reply_agent_role = '', notify = 0,
	    issue = '', wake_issue = '', expires_at = NULL, nested_open = 0, target_provider = '', polling = 0, ack_owed = 0
	WHERE lineage_only = 0`

// The retention floor's four sets, each stated as the partial predicate of
// the index that reads it: a source row past the floor
// (idx_thread_requests_age), a receipt past it
// (idx_thread_request_receipts_age), and a lineage row whose spawned thread
// is gone (idx_thread_requests_lineage, idx_thread_request_receipts_lineage). An open row is in neither age
// index: it is still work, and waits for its own settlement. A spawn whose
// thread is here waits the same way until that thread reads at rest: past
// the floor, a spawn, its request or its receipt, whose thread is here is
// cleared only when that thread is among ?2 (a JSON array, the threads the
// sweep's RestGuard read at rest inside its transaction), so the Agents
// segment keeps listing it with its answer and its worker can still reply
// on its token. A spawn on another computer that last reported open work there
// (nested_open) waits for that report to end: deleting it would take the
// open rows below it out of its caller's tree. A local spawn's lineage
// stays its target's link once cleared (agentSpawnLinkSQL). A spawn's
// receipt, once past the floor, is kept while its thread exists as its
// lineage (receiptLineageSQL): it is what lets the computer that made the
// spawn list and stop that thread's work, now or once it is continued
// (ThreadSpawnedForSource), and that computer keeps its own row for open
// work there through nested_open. Both read ?1 as the floor.
//
// The lineage set is the one the sweep's probe cannot search: "no thread
// has this id" is an anti-join, so the probe walks every lineage row on
// idx_thread_requests_lineage and looks each up in threads. Measured at
// 5 us with none and 1.8 ms with 5,000, once per sweep.
var (
	requestPastFloorSQL = `lineage_only = 0 AND state NOT IN ('unconfirmed','accepted','running')
	    AND COALESCE(settled_at, due_at, created_at) < ?1 AND NOT (` + nestedRemoteSpawnSQL("thread_requests") + `)
	    AND NOT (` + agentSpawnLinkSQL("thread_requests") + ` AND thread_requests.target_thread_id NOT IN (SELECT value FROM json_each(?2))
	             AND EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_requests.target_thread_id))`
	receiptPastFloorSQL = `lineage_only = 0 AND state NOT IN ('accepted','running') AND COALESCE(settled_at, created_at) < ?1
	    AND NOT (kind = 'spawn' AND COALESCE(thread_request_receipts.target_thread_id, '') NOT IN (SELECT value FROM json_each(?2))
	             AND EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_request_receipts.target_thread_id))`
)

const orphanLineageSQL = `lineage_only = 1 AND target_computer_id = ''
	    AND NOT EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_requests.target_thread_id)`

const (
	// receiptLineageSQL matches a spawn's receipt whose thread still exists.
	receiptLineageSQL = `kind = 'spawn'
	    AND EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_request_receipts.target_thread_id)`
	orphanReceiptLineageSQL = `lineage_only = 1
	    AND NOT EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_request_receipts.target_thread_id)`
	// clearReceiptLineageSQL keeps a receipt past the floor as its lineage:
	// the source and target it names and its state and times, its answers
	// and their agents gone. The sweep appends its floor.
	clearReceiptLineageSQL = `UPDATE thread_request_receipts
	SET lineage_only = 1, answer = NULL, answer_kind = '', answer_cause = '', answer_cause_arg = '', late_reply = NULL, late_reply_at = NULL, expires_at = NULL,
	    answer_agent = 0, answer_agent_role = '', late_reply_agent = 0, late_reply_agent_role = ''
	WHERE `
)

// SpawnThreadsPastFloor lists the threads here of the spawns past the
// floor at cutoff, a spawn's request or its receipt not yet reduced to its
// lineage: the threads whose rest the sweep reads (RestGuard) before it
// clears those spawns (DeleteThreadRequestsBefore). One read through the
// age indexes, so it follows the rows past the floor.
func (s *Store) SpawnThreadsPastFloor(cutoff int64) ([]string, error) {
	return s.queryThreadIDs(fmt.Sprintf("store: list the threads of the spawns past %d", cutoff),
		`SELECT target_thread_id FROM thread_requests
		     WHERE lineage_only = 0 AND state NOT IN ('unconfirmed','accepted','running') AND COALESCE(settled_at, due_at, created_at) < ?1
		       AND `+agentSpawnLinkSQL("thread_requests")+`
		       AND EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_requests.target_thread_id)
		    UNION
		    SELECT target_thread_id FROM thread_request_receipts
		     WHERE lineage_only = 0 AND state NOT IN ('accepted','running') AND COALESCE(settled_at, created_at) < ?1
		       AND kind = 'spawn' AND EXISTS (SELECT 1 FROM threads WHERE threads.id = thread_request_receipts.target_thread_id)
		    ORDER BY 1`, cutoff)
}

// DeleteThreadRequestsBefore removes both sides of every request that has
// been settled longer than the retention floor, and reports how many rows
// it deleted or cleared. Past it a token stops answering, so a late reply
// gets `thread_request_unknown` rather than resurrecting a month-old
// exchange.
//
// Two rows survive the floor whatever their age. An OPEN row is still work:
// deleting it would leave a receipt nobody can settle or cancel, so the row
// waits for its own settlement first. And a row's age is measured from the
// moment it finished, not from when it was made: a reminder armed for next
// month is due in the future and older than the floor at the same time, and
// an answer written yesterday on a request made last year has been readable
// for a day. A local spawn request whose thread exists keeps its lineage
// past the floor, marked lineage_only with its payload cleared
// (spawnLineageSQL), and so does a spawn's receipt (receiptLineageSQL).
// A spawn whose thread is here is cleared only when its thread is among the
// roots rest reads at rest inside the sweep's transaction: a nil rest, or
// one whose roots leave it out, clears no such spawn.
//
// The same transaction releases each paired computer's link whose owner has
// not reached it since the floor (staleSpawnLinkSQL), one link at a time
// whatever device owns it, and returns the threads those links named
// (releaseSpawnLinksTx), apart from the count.
//
// The sweep runs every few minutes and almost never finds anything, so it
// asks first, on the read pool, through the indexes the sets have, and
// takes the writer only when something is past the floor.
func (s *Store) DeleteThreadRequestsBefore(cutoff int64, rest *RestGuard) (swept int64, released []string, err error) {
	var roots []string
	if rest != nil {
		roots = rest.Roots
	}
	encoded := func(threads []string) (string, error) {
		if threads == nil {
			threads = []string{}
		}
		out, err := json.Marshal(threads)
		return string(out), err
	}
	// The roots read at rest before the sweep: what may be due.
	threads, err := encoded(roots)
	if err != nil {
		return 0, nil, fmt.Errorf("store: sweep thread requests past %d: encode: %w", cutoff, err)
	}
	var due bool
	// staleSpawnLinkSQL's two bare parameters follow ?1 and ?2: ?3 and ?4.
	if err := s.reader().QueryRow(`SELECT
	    EXISTS (SELECT 1 FROM thread_requests WHERE `+requestPastFloorSQL+`)
	    OR EXISTS (SELECT 1 FROM thread_request_receipts WHERE `+receiptPastFloorSQL+`)
	    OR EXISTS (SELECT 1 FROM thread_requests WHERE `+orphanLineageSQL+`)
	    OR EXISTS (SELECT 1 FROM thread_request_receipts WHERE `+orphanReceiptLineageSQL+`)
	    OR EXISTS (SELECT 1 FROM thread_request_receipts WHERE `+staleSpawnLinkSQL+`)`,
		cutoff, threads, cutoff, cutoff).Scan(&due); err != nil {
		return 0, nil, fmt.Errorf("store: read whether thread requests are past %d: %w", cutoff, err)
	}
	if !due {
		return 0, nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, fmt.Errorf("store: begin thread request retention sweep: %w", err)
	}
	defer rollbackTx(tx, "store: thread request retention sweep", &err)
	// The roots read at rest again with the writer held: a thread that took
	// work since keeps its spawns whole.
	rested, err := rest.atRestTx(tx)
	if err != nil {
		return 0, nil, fmt.Errorf("store: sweep thread requests past %d: %w", cutoff, err)
	}
	if threads, err = encoded(rested); err != nil {
		return 0, nil, fmt.Errorf("store: sweep thread requests past %d: encode: %w", cutoff, err)
	}
	if released, err = releaseSpawnLinksTx(tx, staleSpawnLinkSQL, cutoff, cutoff); err != nil {
		return 0, nil, fmt.Errorf("store: release the spawn links silent since %d: %w", cutoff, err)
	}
	floor := []any{cutoff, threads}
	steps := []struct {
		what, query string
		args        []any
	}{
		{"clear kept thread_requests", clearLineagePayloadSQL + ` AND ` + requestPastFloorSQL + ` AND ` + spawnLineageSQL, floor},
		{"sweep thread_requests", `DELETE FROM thread_requests WHERE ` + requestPastFloorSQL + ` AND NOT (` + spawnLineageSQL + `)`, floor},
		{"sweep lineage thread_requests", `DELETE FROM thread_requests WHERE ` + orphanLineageSQL, nil},
		{"clear kept thread_request_receipts", clearReceiptLineageSQL + receiptPastFloorSQL + ` AND ` + receiptLineageSQL, floor},
		// The step before kept the lineage, which the floor's set leaves out.
		{"sweep thread_request_receipts", `DELETE FROM thread_request_receipts WHERE ` + receiptPastFloorSQL, floor},
		{"sweep lineage thread_request_receipts", `DELETE FROM thread_request_receipts WHERE ` + orphanReceiptLineageSQL, nil},
	}
	for _, step := range steps {
		result, err := tx.Exec(step.query, step.args...)
		if err != nil {
			return 0, nil, fmt.Errorf("store: %s before %d: %w", step.what, cutoff, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, nil, fmt.Errorf("store: count %s rows: %w", step.what, err)
		}
		swept += affected
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("store: commit thread request retention sweep: %w", err)
	}
	return swept, released, nil
}
