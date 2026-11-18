package store

import (
	"database/sql"
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

// threadAnswerDueV155SQL indexes the requests whose answer is on its way to
// their caller, by caller: the rows ThreadAnswerDue, ThreadsWithAnswerDue and
// the Agents tree's held work (agentHeldSQL) read.
//
// thread_requests.rest_unreported is ThreadRequest.RestUnreported.
var threadAnswerDueV155SQL = `
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

// workflowRunHoldsSQL matches a root run that keeps the thread it reports
// to from rest: one that runs, or one that rested with a rest's report its
// wake job has not yet delivered or dropped (reports_owed). It is
// idx_work_items_holding's predicate.
func workflowRunHoldsSQL(alias string) string {
	return alias + `.origin_thread_id <> '' AND (` + alias + `.state = 'running' OR ` + alias + `.reports_owed > 0)`
}

// ThreadsHeldByWorkflowRuns lists the threads a workflow run keeps from
// rest (workflowRunHoldsSQL).
func (s *Store) ThreadsHeldByWorkflowRuns() ([]string, error) {
	return s.queryThreadIDs("store: list the threads workflow runs keep from rest",
		`SELECT DISTINCT w.origin_thread_id FROM work_items w INDEXED BY idx_work_items_holding WHERE `+workflowRunHoldsSQL("w"))
}

// SettleWorkItemReport records that one rest's wake job of run itemID has
// ended, whatever it did with the report: one fewer report is owed. A run
// with none owed is left as it is.
func (s *Store) SettleWorkItemReport(itemID string) error {
	if _, err := s.db.Exec(`UPDATE work_items SET reports_owed = reports_owed - 1 WHERE id = ? AND reports_owed > 0`, itemID); err != nil {
		return fmt.Errorf("store: settle a report of workflow run %s: %w", itemID, err)
	}
	return nil
}

// ClearWorkItemReportsOwed forgets every report owed: the wake jobs that
// would deliver them were the process's that wrote them. The workflow
// engine's start calls it before it emits anything.
func (s *Store) ClearWorkItemReportsOwed() error {
	if _, err := s.db.Exec(`UPDATE work_items SET reports_owed = 0 WHERE reports_owed > 0`); err != nil {
		return fmt.Errorf("store: clear the workflow reports owed: %w", err)
	}
	return nil
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
