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
// (workerUnrest); a decision that cannot be taken back reads them for a
// whole subtree in one snapshot (ReadSubtreeRest).

// ThreadRestSources is what the store holds of one thread's rest.
type ThreadRestSources struct {
	// Asking: a question it asked that nobody answered yet.
	Asking bool
	// AnswerDue: an answer to a request it made on its way to it
	// (threadAnswerDueSQL).
	AnswerDue bool
	// Workflow: a workflow run that reports to it and runs, or is one of
	// the owed reports the read was given; WorkflowGoal is the oldest's.
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
	// armed, or a spawn on another computer that last reported open work.
	OpenWork bool
	// Unarmed are its open sends and asks with no wake armed, by token:
	// open work while a call is parked on one.
	Unarmed []string
}

// SubtreeRest is one snapshot of the sources of rest of the threads reached
// from some roots.
type SubtreeRest struct {
	// Reached lists, for each root, the root and every thread a local spawn
	// link reaches below it, at any depth, settled and lineage links
	// included, each once. A root that is no thread reaches nothing.
	Reached map[string][]string
	// Sources holds each reached thread's sources.
	Sources map[string]ThreadRestSources
}

// ReadSubtreeRest reads, in one read transaction, the threads reached from
// each of roots and the sources of each (ThreadRestSources). owedRuns are
// the workflow runs that rested and still owe their report, which only the
// workflow owner knows; a run that runs is read from the store. The walk
// probes each reached thread's spawn links by caller
// (idx_thread_requests_spawn_caller), and every source is one indexed
// search per reached thread, so the read's cost follows the subtree,
// not the computer's other threads or a reached thread's settled history.
func (s *Store) ReadSubtreeRest(roots, owedRuns []string) (_ SubtreeRest, err error) {
	action := "store: read the rest below " + strings.Join(roots, ", ")
	encodedRoots, err := json.Marshal(roots)
	if err != nil {
		return SubtreeRest{}, fmt.Errorf("%s: encode: %w", action, err)
	}
	tx, err := s.reader().BeginTx(context.Background(), nil)
	if err != nil {
		return SubtreeRest{}, fmt.Errorf("%s: begin: %w", action, err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && err == nil {
			err = fmt.Errorf("%s: end the read: %w", action, rollbackErr)
		}
	}()
	out := SubtreeRest{Reached: map[string][]string{}}
	threads, err := func() (_ []string, err error) {
		rows, err := tx.Query(subtreeReachedSQL, string(encodedRoots))
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
		return SubtreeRest{}, fmt.Errorf("%s: walk the spawn links: %w", action, err)
	}
	if out.Sources, err = readRestSources(tx, threads, owedRuns); err != nil {
		return SubtreeRest{}, fmt.Errorf("%s: %w", action, err)
	}
	return out, nil
}

// ReadThreadRestSources reads one thread's sources of rest
// (ThreadRestSources), with ReadSubtreeRest's owedRuns.
func (s *Store) ReadThreadRestSources(threadID string, owedRuns []string) (ThreadRestSources, error) {
	sources, err := readRestSources(s.reader(), []string{threadID}, owedRuns)
	if err != nil {
		return ThreadRestSources{}, fmt.Errorf("store: read the rest of %s: %w", threadID, err)
	}
	return sources[threadID], nil
}

// readRestSources reads the sources of each of threads in one statement.
func readRestSources(q sqlQueryer, threads, owedRuns []string) (_ map[string]ThreadRestSources, err error) {
	out := make(map[string]ThreadRestSources, len(threads))
	if len(threads) == 0 {
		return out, nil
	}
	if owedRuns == nil {
		owedRuns = []string{}
	}
	encodedThreads, err := json.Marshal(threads)
	if err != nil {
		return nil, fmt.Errorf("encode the threads: %w", err)
	}
	encodedRuns, err := json.Marshal(owedRuns)
	if err != nil {
		return nil, fmt.Errorf("encode the owed reports: %w", err)
	}
	rows, err := q.Query(threadRestSourcesSQL, string(encodedThreads), string(encodedRuns))
	if err != nil {
		return nil, fmt.Errorf("read the sources: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id string
		var src ThreadRestSources
		var workflow, wake, reminder sql.NullString
		var unarmed string
		if err := rows.Scan(&id, &src.Asking, &src.AnswerDue, &workflow, &wake, &reminder, &src.OpenWork, &unarmed); err != nil {
			return nil, fmt.Errorf("scan the sources: %w", err)
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
// spawn links, one link a step, keyed by the queued thread
// (idx_thread_requests_spawn_caller holds only links, so a thread's
// sends, asks and reminders are never read). A thread is reached once per
// root, so a cycle ends. Only threads that exist are listed.
var subtreeReachedSQL = `WITH RECURSIVE reached(root, id) AS (
    SELECT value, value FROM json_each(?1)
    UNION
    SELECT reached.root, link.target_thread_id
      FROM reached CROSS JOIN thread_requests AS link INDEXED BY idx_thread_requests_spawn_caller
     WHERE link.caller_thread_id = reached.id AND ` + agentSpawnLinkSQL("link") + `)
SELECT reached.root, reached.id FROM reached CROSS JOIN threads ON threads.id = reached.id`

// threadRestSourcesSQL reads each thread of ?1 (a JSON array) with its
// sources, ?2 the owed reports (a JSON array): one search per source and
// thread, each through the index that holds only that source's live rows
// (idx_async_questions_pending, idx_thread_requests_answer_due,
// idx_agent_wakes_thread, idx_thread_requests_open_created,
// idx_thread_requests_nested_caller) or, for the workflow runs, the
// thread's bound runs (idx_work_items_origin_thread). An accepted reminder
// is open (settled_at IS NULL), so it is read among the open rows, not the
// caller's history.
var threadRestSourcesSQL = `SELECT j.value,
       EXISTS (SELECT 1 FROM async_questions q INDEXED BY idx_async_questions_pending
                WHERE q.thread_id = j.value AND q.state IN ('unanswered','submitted','restored') AND q.state = 'unanswered')
         AND EXISTS (SELECT 1 FROM owned_threads t WHERE t.id = j.value AND t.archived = 0),
       EXISTS (SELECT 1 FROM thread_requests d INDEXED BY idx_thread_requests_answer_due
                WHERE d.caller_thread_id = j.value AND ` + threadAnswerDueSQL("d") + `),
       (SELECT json_array(w.goal) FROM work_items w INDEXED BY idx_work_items_origin_thread
         WHERE w.origin_thread_id = j.value AND w.origin_thread_id <> ''
           AND (w.state = 'running' OR w.id IN (SELECT value FROM json_each(?2)))
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
                     WHERE o.caller_thread_id = j.value AND ` + nestedRemoteSpawnSQL("o") + `),
       (SELECT json_group_array(u.token) FROM thread_requests u INDEXED BY idx_thread_requests_open_created
         WHERE u.caller_thread_id = j.value AND u.lineage_only = 0 AND u.settled_at IS NULL
           AND u.kind IN ('send','ask') AND u.notify = 0)
  FROM json_each(?1) j`

// ThreadsOfRestChanges names the threads whose rest a change to the
// requests among tokens, or to the workflow runs among runs, may have
// moved: each request's caller and its thread on this computer, the
// thread of each receipt, and the thread each run reports to. One read,
// by key; a token or run that is gone names nothing.
func (s *Store) ThreadsOfRestChanges(tokens, runs []string) (_ []string, err error) {
	if len(tokens)+len(runs) == 0 {
		return nil, nil
	}
	action := "store: read the threads of changed requests and workflow runs"
	if tokens == nil {
		tokens = []string{}
	}
	if runs == nil {
		runs = []string{}
	}
	encodedTokens, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", action, err)
	}
	encodedRuns, err := json.Marshal(runs)
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", action, err)
	}
	rows, err := s.reader().Query(`SELECT r.caller_thread_id FROM json_each(?1) j CROSS JOIN thread_requests r WHERE r.token = j.value
		UNION SELECT r.target_thread_id FROM json_each(?1) j CROSS JOIN thread_requests r
		 WHERE r.token = j.value AND r.target_computer_id = '' AND r.target_thread_id <> ''
		UNION SELECT rc.target_thread_id FROM json_each(?1) j CROSS JOIN thread_request_receipts rc
		 WHERE rc.token = j.value AND rc.target_thread_id IS NOT NULL
		UNION SELECT w.origin_thread_id FROM json_each(?2) j CROSS JOIN work_items w
		 WHERE w.id = j.value AND w.origin_thread_id <> ''`, string(encodedTokens), string(encodedRuns))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", action, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", action, err)
	}
	return out, nil
}
