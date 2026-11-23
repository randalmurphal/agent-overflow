package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The durable sources of a thread's rest, read for a set of threads at once
// (docs/specs/standing-agents.md, "Owned workers"): what a thread's own
// sources hold that starts a turn there without a person, and the open
// work it has outstanding below it. The app applies the rule to them
// (restWaits); a decision that cannot be taken back reads them for a
// whole subtree, once before its write and again inside the write's own
// transaction (RestGuard).

// ThreadRestSources is what the store holds of one thread's rest.
type ThreadRestSources struct {
	// TurnOpen: its latest turn row is open (GetActiveTurn), committed by
	// the turn's start whether or not its frame has been published.
	TurnOpen bool
	// Asking: a question it asked that nobody answered yet.
	Asking bool
	// AnswerDue: an answer to a request it made on its way to it
	// (threadAnswerDueSQL).
	AnswerDue bool
	// Background: background work of it running, as the background tray
	// lists it: a live background launch at any depth, or else a command it
	// runs on another computer. BackgroundDetail is the first launch's
	// summary or tool, or the command's label, with BackgroundComputer the
	// command's computer.
	Background         bool
	BackgroundDetail   string
	BackgroundComputer string
	// Workflow: a workflow run bound to it now that runs, or that rested
	// with its report owed (workflowRunHoldsSQL); WorkflowGoal is the
	// oldest's.
	Workflow     bool
	WorkflowGoal string
	// Wake: an armed or running loop or watch; WakeKind and WakeLabel are
	// the first by label.
	Wake      bool
	WakeKind  string
	WakeLabel string
	// Reminder: a reminder it set that has not fired; ReminderNote is the
	// soonest's note.
	Reminder     bool
	ReminderNote string
	// OpenWork: a request it made that is open work below it
	// (agentOpenWorkSQL): an open spawn, an open send or ask with its wake
	// armed, or a spawn on another computer that last reported open work
	// while a run of its is open there (spawnActiveSQL).
	OpenWork bool
	// Unarmed are its open sends and asks with no wake armed, by token:
	// open work while a call is parked on one.
	Unarmed []string
}

// SubtreeRest is one snapshot of the sources of rest of the threads reached
// from some roots.
type SubtreeRest struct {
	// Reached lists, for each root, the root and every thread a local spawn
	// link with a run of its caller's open reaches below it, at any depth,
	// settled and lineage links included, each once (subtreeReachedSQL). A
	// root that is no thread reaches nothing.
	Reached map[string][]string
	// Sources holds each reached thread's sources.
	Sources map[string]ThreadRestSources
}

// RestGuard is the precondition a write that cannot be taken back carries:
// the write happens only for the roots still at rest when its own
// transaction reads them. The store reads each root's subtree and its
// sources inside that transaction, on its connection (SubtreeRest), and
// AtRest names the roots that read at rest. A durable change by anyone else
// is so either read there or ordered after the write.
//
// AtRest runs with the write's transaction open: it takes no store
// connection and no lock that is held across a store write.
type RestGuard struct {
	ForgottenComputers  []string
	ReconciledComputers []string
	OwedRemoteTokens    []string
	Roots               []string
	AtRest              func(SubtreeRest) []string
}

// atRestTx reads the guard's roots inside tx, run the Store's, and names
// those AtRest says are at rest. A nil guard names none.
func (g *RestGuard) atRestTx(tx *sql.Tx, run string) ([]string, error) {
	if g == nil || len(g.Roots) == 0 {
		return nil, nil
	}
	subtree, err := readSubtreeRest(tx, run, g.Roots)
	if err != nil {
		return nil, fmt.Errorf("read the rest of %s: %w", strings.Join(g.Roots, ", "), err)
	}
	return g.AtRest(subtree), nil
}

// ReadSubtreeRest reads, in one read transaction, the threads reached from
// each of roots and the sources of each (ThreadRestSources). The walk
// probes each reached thread's spawn links by caller
// (idx_thread_requests_spawn_caller), and every source is one indexed
// search per reached thread, so the read's cost follows the subtree, not
// the computer's other threads or a reached thread's settled history.
func (s *Store) ReadSubtreeRest(roots []string) (_ SubtreeRest, err error) {
	action := "store: read the rest below " + strings.Join(roots, ", ")
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return SubtreeRest{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	out, err := readSubtreeRest(tx, s.run, roots)
	if err != nil {
		return SubtreeRest{}, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

// readSubtreeRest walks from roots down the local spawn links and reads the
// sources of every thread reached, on q; run is the Store's.
func readSubtreeRest(q sqlQueryer, run string, roots []string) (SubtreeRest, error) {
	encodedRoots, err := json.Marshal(roots)
	if err != nil {
		return SubtreeRest{}, fmt.Errorf("encode the roots: %w", err)
	}
	out := SubtreeRest{Reached: map[string][]string{}}
	threads, err := func() (_ []string, err error) {
		rows, err := q.Query(subtreeReachedSQL, string(encodedRoots))
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, rows.Close()) }()
		seen := map[string]bool{}
		var threads []string
		for rows.Next() {
			var root, id string
			if err := rows.Scan(&root, &id); err != nil {
				return nil, err
			}
			out.Reached[root] = append(out.Reached[root], id)
			if !seen[id] {
				seen[id] = true
				threads = append(threads, id)
			}
		}
		return threads, rows.Err()
	}()
	if err != nil {
		return SubtreeRest{}, fmt.Errorf("walk the spawn links: %w", err)
	}
	if out.Sources, err = readRestSources(q, run, threads); err != nil {
		return SubtreeRest{}, err
	}
	return out, nil
}

// ReadThreadRestSources reads one thread's sources of rest
// (ThreadRestSources).
func (s *Store) ReadThreadRestSources(threadID string) (ThreadRestSources, error) {
	sources, err := readRestSources(s.reader(), s.run, []string{threadID})
	if err != nil {
		return ThreadRestSources{}, fmt.Errorf("store: read the rest of %s: %w", threadID, err)
	}
	return sources[threadID], nil
}

// readRestSources reads the sources of each of threads in one statement;
// run is the Store's.
func readRestSources(q sqlQueryer, run string, threads []string) (_ map[string]ThreadRestSources, err error) {
	out := make(map[string]ThreadRestSources, len(threads))
	if len(threads) == 0 {
		return out, nil
	}
	encodedThreads, err := json.Marshal(threads)
	if err != nil {
		return nil, fmt.Errorf("encode the threads: %w", err)
	}
	rows, err := q.Query(threadRestSourcesSQL, string(encodedThreads), run)
	if err != nil {
		return nil, fmt.Errorf("read the sources: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id string
		var src ThreadRestSources
		var background, workflow, wake, reminder sql.NullString
		var unarmed string
		if err := rows.Scan(&id, &src.TurnOpen, &src.Asking, &src.AnswerDue, &background, &workflow, &wake, &reminder, &src.OpenWork, &unarmed); err != nil {
			return nil, fmt.Errorf("scan the sources: %w", err)
		}
		if background.Valid {
			src.Background = true
			if err := decodeRestFields(background.String, &src.BackgroundDetail, &src.BackgroundComputer); err != nil {
				return nil, fmt.Errorf("the background work of %s: %w", id, err)
			}
		}
		if workflow.Valid {
			src.Workflow = true
			if err := decodeRestFields(workflow.String, &src.WorkflowGoal); err != nil {
				return nil, fmt.Errorf("the workflow run reporting to %s: %w", id, err)
			}
		}
		if wake.Valid {
			src.Wake = true
			if err := decodeRestFields(wake.String, &src.WakeKind, &src.WakeLabel); err != nil {
				return nil, fmt.Errorf("the loop or watch of %s: %w", id, err)
			}
		}
		if reminder.Valid {
			src.Reminder = true
			if err := decodeRestFields(reminder.String, &src.ReminderNote); err != nil {
				return nil, fmt.Errorf("the reminder of %s: %w", id, err)
			}
		}
		if err := json.Unmarshal([]byte(unarmed), &src.Unarmed); err != nil {
			return nil, fmt.Errorf("the open sends and asks of %s: %w", id, err)
		}
		out[id] = src
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the sources: %w", err)
	}
	return out, nil
}

// decodeRestFields reads a source's JSON array of strings into fields.
func decodeRestFields(encoded string, fields ...*string) error {
	var values []string
	if err := json.Unmarshal([]byte(encoded), &values); err != nil {
		return err
	}
	if len(values) != len(fields) {
		return fmt.Errorf("%d fields, want %d", len(values), len(fields))
	}
	for i, value := range values {
		*fields[i] = value
	}
	return nil
}

// subtreeReachedSQL walks from each root (?1, a JSON array) down the local
// spawn links with a run of their caller's open (spawnActiveSQL), one link
// a step, keyed by the queued thread (idx_thread_requests_spawn_caller holds
// only links, so a thread's sends, asks and reminders are never read): a
// worker whose spawner's runs on it all ended is at rest for that spawner,
// whoever runs it since. A thread is reached once per root, so a cycle
// ends. Only threads that exist are listed.
var subtreeReachedSQL = `WITH RECURSIVE reached(root, id) AS (
    SELECT value, value FROM json_each(?1)
    UNION
    SELECT reached.root, link.target_thread_id
      FROM reached CROSS JOIN thread_requests AS link INDEXED BY idx_thread_requests_spawn_caller
     WHERE link.caller_thread_id = reached.id AND ` + agentSpawnLinkSQL("link") + ` AND ` + spawnActiveSQL("link") + `)
SELECT reached.root, reached.id FROM reached CROSS JOIN threads ON threads.id = reached.id`

// remoteCommandRunningSQL matches a remote command watch the background
// tray lists as running: not dismissed, and with no receipt of its end.
func remoteCommandRunningSQL(alias string) string {
	return alias + `.notification <> 'dismissed' AND (COALESCE(json_extract(` + alias + `.receipt, '$.id'), '') = ''
	         OR json_extract(` + alias + `.receipt, '$.state') = 'running')`
}

// threadRestSourcesSQL reads each thread of ?1 (a JSON array), ?2 the
// Store's run, with its sources: one search per source and thread, each through the index that
// holds only that source's live rows (idx_async_questions_pending,
// idx_thread_requests_answer_due, idx_items_running_bg_tool_calls,
// idx_agent_wakes_thread, idx_thread_requests_open_created,
// idx_thread_requests_nested_caller, idx_work_items_holding) or the
// thread's own rows (idx_remote_watches_thread), and its latest turn, one
// probe of turns_thread_index. An accepted reminder is open (settled_at IS NULL),
// so it is read among the open rows, not the caller's history.
var threadRestSourcesSQL = `SELECT j.value,
       COALESCE(` + newestTurnSQL("j.value", turnOpenSQL) + `, 0),
       EXISTS (SELECT 1 FROM async_questions q INDEXED BY idx_async_questions_pending
                WHERE q.thread_id = j.value AND q.state IN ('unanswered','submitted','restored') AND q.state = 'unanswered'
                  AND ` + asyncQuestionHoldsSQL("q", "?2") + `)
         AND EXISTS (SELECT 1 FROM owned_threads t WHERE t.id = j.value AND t.archived = 0),
       EXISTS (SELECT 1 FROM thread_requests d INDEXED BY idx_thread_requests_answer_due
                WHERE d.caller_thread_id = j.value AND ` + threadAnswerDueSQL("d") + `),
       COALESCE(
         (SELECT json_array(COALESCE(NULLIF(items.summary, ''), items.tool_name), '') FROM items INDEXED BY idx_items_running_bg_tool_calls
           WHERE items.thread_id = j.value AND items.kind = 'tool_call' AND items.status = 'running' AND items.is_background = 1
             AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0
             AND ` + noCompletionSiblingIndexedSQL + `
           ORDER BY items.turn_index, items.item_index LIMIT 1),
         (SELECT json_array(rw.label, rw.computer_id) FROM remote_watches rw INDEXED BY idx_remote_watches_thread
           WHERE rw.thread_id = j.value AND ` + remoteCommandRunningSQL("rw") + `
           ORDER BY rw.created_at DESC LIMIT 1)),
       (SELECT json_array(w.goal) FROM work_items w INDEXED BY idx_work_items_holding
         WHERE w.origin_thread_id = j.value AND ` + workflowRunHoldsSQL("w") + `
         ORDER BY w.created_at, w.id LIMIT 1),
       (SELECT json_array(a.kind, a.label) FROM agent_wakes a INDEXED BY idx_agent_wakes_thread
         WHERE a.thread_id = j.value AND a.state IN ('armed','running') ORDER BY a.label LIMIT 1),
       (SELECT json_array(CAST(COALESCE(r.answer, '') AS TEXT)) FROM thread_requests r INDEXED BY idx_thread_requests_open_created
         WHERE r.caller_thread_id = j.value AND r.lineage_only = 0 AND r.settled_at IS NULL
           AND r.kind = 'remind' AND r.state = 'accepted'
         ORDER BY r.due_at, r.token LIMIT 1),
       EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_open_created
                WHERE o.caller_thread_id = j.value AND o.lineage_only = 0 AND o.settled_at IS NULL AND ` + agentOpenWorkSQL("o") + `)
         OR EXISTS (SELECT 1 FROM thread_requests o INDEXED BY idx_thread_requests_nested_caller
                     WHERE o.caller_thread_id = j.value AND ` + nestedRemoteSpawnSQL("o") + ` AND ` + spawnActiveSQL("o") + `),
       (SELECT json_group_array(u.token) FROM thread_requests u INDEXED BY idx_thread_requests_open_created
         WHERE u.caller_thread_id = j.value AND u.lineage_only = 0 AND u.settled_at IS NULL
           AND u.kind IN ('send','ask') AND u.notify = 0)
  FROM json_each(?1) j`
