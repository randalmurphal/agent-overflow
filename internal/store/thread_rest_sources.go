package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The durable sources of a thread's rest the request ledger and the
// workflow runs hold (docs/specs/standing-agents.md, "At rest"): an answer
// on its way to the thread, and a workflow run that reports to it. Each is
// read per thread through an index that holds only the rows in that state,
// and listed whole at boot.

// threadAnswerDueSQL matches a request whose answer is owed to its caller
// as a message and has not reached a turn there yet: settled with its wake
// armed and nothing delivered, or queued and not yet echoed by the
// caller's provider (confirmed_at), and the same of a late reply. An
// inline delivery is confirmed when it is marked, an abandoned or
// withdrawn wake is disarmed, and a lineage row answers nothing.
func threadAnswerDueSQL(alias string) string {
	a := ""
	if alias != "" {
		a = alias + "."
	}
	return `(` + a + `lineage_only = 0 AND ` + a + `settled_at IS NOT NULL AND (
       (` + a + `notify = 1 AND ` + a + `delivered_at IS NULL AND ` + a + `late_reply IS NULL)
       OR (` + a + `delivered_at IS NOT NULL AND ` + a + `confirmed_at IS NULL)
       OR (` + a + `late_reply IS NOT NULL AND ((` + a + `notify = 1 AND ` + a + `late_delivered_at IS NULL)
           OR (` + a + `late_delivered_at IS NOT NULL AND ` + a + `late_confirmed_at IS NULL)))))`
}

// threadAnswerDueV154SQL indexes the requests whose answer is on its way to
// their caller, by caller: the rows ThreadAnswerDue, ThreadsWithAnswerDue and
// the Agents tree's held work (agentHeldSQL) read.
//
// thread_requests.rest_unreported is ThreadRequest.RestUnreported.
var threadAnswerDueV154SQL = `
CREATE INDEX idx_thread_requests_answer_due ON thread_requests(caller_thread_id)
 WHERE ` + threadAnswerDueSQL("") + `;
ALTER TABLE thread_requests ADD COLUMN rest_unreported INTEGER NOT NULL DEFAULT 0 CHECK(rest_unreported IN (0,1));
`

// ThreadAnswerDue reports whether an answer to a request threadID made is
// on its way to it (threadAnswerDueSQL): the thread will take a turn for
// it without a person.
func (s *Store) ThreadAnswerDue(threadID string) (bool, error) {
	var due bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_requests INDEXED BY idx_thread_requests_answer_due
		WHERE caller_thread_id = ? AND `+threadAnswerDueSQL("")+`)`, threadID).Scan(&due)
	if err != nil {
		return false, fmt.Errorf("store: read whether an answer is on its way to %s: %w", threadID, err)
	}
	return due, nil
}

// ThreadsWithAnswerDue lists the threads an answer is on its way to.
func (s *Store) ThreadsWithAnswerDue() ([]string, error) {
	return s.queryThreadIDs("store: list the threads an answer is on its way to",
		`SELECT DISTINCT caller_thread_id FROM thread_requests INDEXED BY idx_thread_requests_answer_due
		WHERE `+threadAnswerDueSQL(""))
}

// RunningWorkflowRunReportingTo names the oldest running workflow run that
// reports to threadID, by its goal, found false when none does. A run
// that waits on a person, or ended, starts no turn there by itself.
func (s *Store) RunningWorkflowRunReportingTo(threadID string) (goal string, found bool, err error) {
	err = s.reader().QueryRow(`SELECT goal FROM work_items
		WHERE origin_thread_id = ? AND origin_thread_id <> '' AND state = 'running'
		ORDER BY created_at ASC, id ASC LIMIT 1`, threadID).Scan(&goal)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read the running workflow run of %s: %w", threadID, err)
	}
	return goal, true, nil
}

// ThreadsWithRunningWorkflowRuns lists the threads a running workflow run
// reports to.
func (s *Store) ThreadsWithRunningWorkflowRuns() ([]string, error) {
	return s.queryThreadIDs("store: list the threads a running workflow run reports to",
		`SELECT DISTINCT origin_thread_id FROM work_items WHERE state = 'running' AND origin_thread_id <> ''`)
}

// WorkItemOriginThread names the thread a workflow run reports to, empty
// for a run that reports to none or is gone.
func (s *Store) WorkItemOriginThread(itemID string) (string, error) {
	var thread string
	err := s.reader().QueryRow(`SELECT origin_thread_id FROM work_items WHERE id = ?`, itemID).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read the thread workflow run %s reports to: %w", itemID, err)
	}
	return thread, nil
}

// ThreadRequestCallers names the caller of each of tokens this computer's
// ledger holds, a token it does not hold left out.
func (s *Store) ThreadRequestCallers(tokens []string) ([]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("store: encode request tokens: %w", err)
	}
	return s.queryThreadIDs("store: read the callers of changed requests",
		`SELECT DISTINCT r.caller_thread_id FROM json_each(?) j CROSS JOIN thread_requests r WHERE r.token = j.value`, string(encoded))
}

// CallerSpawnedRemote reports whether callerThreadID spawned threadID on
// the paired computer computerID: a send of it there waits for that
// thread's rest as a spawn does.
func (s *Store) CallerSpawnedRemote(callerThreadID, computerID, threadID string) (bool, error) {
	var spawned bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM thread_requests INDEXED BY idx_thread_requests_caller
		WHERE caller_thread_id = ? AND kind = 'spawn' AND target_computer_id = ? AND target_thread_id = ?)`,
		callerThreadID, computerID, threadID).Scan(&spawned)
	if err != nil {
		return false, fmt.Errorf("store: read whether %s spawned %s on %s: %w", callerThreadID, threadID, computerID, err)
	}
	return spawned, nil
}

// ThreadHasUnansweredAsyncQuestions reports whether threadID has a
// question still waiting for an answer: the thread waits on whoever
// answers it, and takes a turn for that answer. The same rows as
// UnansweredAsyncQuestions, on idx_async_questions_pending.
func (s *Store) ThreadHasUnansweredAsyncQuestions(threadID string) (bool, error) {
	var open bool
	err := s.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM async_questions q
		WHERE q.thread_id = ?1 AND q.state IN ('unanswered','submitted','restored') AND q.state = 'unanswered'
		AND EXISTS (SELECT 1 FROM owned_threads t WHERE t.id = ?1 AND t.archived = 0))`, threadID).Scan(&open)
	if err != nil {
		return false, fmt.Errorf("store: read whether %s has unanswered questions: %w", threadID, err)
	}
	return open, nil
}

// TurnsAfter lists threadID's turns past turnIndex, oldest first: the turns
// a request waiting for its thread's rest has not read yet. Each pass reads
// past the last turn it read, so a read is bounded by the turns run since.
func (s *Store) TurnsAfter(threadID string, turnIndex int) ([]Turn, error) {
	rows, err := s.reader().Query(`SELECT `+turnColumns+` FROM timeline_turns
		WHERE thread_id = ? AND turn_index > ? ORDER BY turn_index`, threadID, turnIndex)
	if err != nil {
		return nil, fmt.Errorf("store: list the turns of %s after %d: %w", threadID, turnIndex, err)
	}
	var out []Turn
	for rows.Next() {
		turn, err := scanTurnRow(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("store: list the turns of %s after %d: %w", threadID, turnIndex, err), rows.Close())
		}
		out = append(out, turn)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list the turns of %s after %d: %w", threadID, turnIndex, err)
	}
	return out, nil
}
