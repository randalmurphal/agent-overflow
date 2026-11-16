package store

import (
	"fmt"
	"strings"
)

// Assistants turned off (docs/specs/standing-agents.md, "Turning assistants
// off"). While they are, nothing scheduled for a thread that lives under an
// assistant comes due: the due reads of the wake supervisor and the reminder
// clock leave those threads out, and so does the sweep's next-moment read,
// so it does not wake for them. The rows themselves are untouched, keeping
// their registration and next fire; turning assistants on makes them due
// again, and a fire that came due meanwhile is served once with its count.
//
// This is the one rule the pause has: a fire is decided by these reads, so
// no path fires a paused row without going through one.

// threadUnderAssistantSQL is true for a thread that lives under an
// assistant: its current thread, one of its past threads or one of its
// helpers. column names the thread id in the enclosing query.
func threadUnderAssistantSQL(column string) string {
	return strings.NewReplacer("{t}", column).Replace(`(EXISTS (SELECT 1 FROM agents WHERE agents.thread_id = {t})
		OR EXISTS (SELECT 1 FROM agent_past_threads WHERE agent_past_threads.thread_id = {t})
		OR EXISTS (SELECT 1 FROM agent_helpers WHERE agent_helpers.thread_id = {t}))`)
}

// PauseAssistants turns the pause on or off. The app sets it from the
// Assistants setting before the sweep's first pass and on every change.
func (s *Store) PauseAssistants(paused bool) {
	s.assistantsPaused.Store(paused)
}

// AssistantsPaused reports whether the due reads leave assistants' threads
// out.
func (s *Store) AssistantsPaused() bool {
	return s.assistantsPaused.Load()
}

// assistantPauseSQL is the filter a due read adds for the thread in column:
// nothing while assistants run, and while they are paused the threads that
// live under an assistant are not due.
func (s *Store) assistantPauseSQL(column string) string {
	if !s.AssistantsPaused() {
		return "1"
	}
	return "NOT " + threadUnderAssistantSQL(column)
}

// ThreadUnderAssistant reports whether threadID lives under an assistant
// (threadUnderAssistantSQL).
func (s *Store) ThreadUnderAssistant(threadID string) (bool, error) {
	var under bool
	if err := s.reader().QueryRow(`SELECT `+threadUnderAssistantSQL("?1"), threadID).Scan(&under); err != nil {
		return false, fmt.Errorf("store: read whether %s lives under an assistant: %w", threadID, err)
	}
	return under, nil
}

// ListThreadsUnderAssistants names every thread that lives under an
// assistant, current threads first.
func (s *Store) ListThreadsUnderAssistants() ([]string, error) {
	return s.queryThreadIDs("store: list the threads under assistants",
		`SELECT thread_id FROM agents
		 UNION ALL SELECT thread_id FROM agent_helpers
		 UNION ALL SELECT thread_id FROM agent_past_threads`)
}

// PauseAgentWakeRun records on a row whose run the pause stopped that it did
// not finish, and arms it again: an interval row is due once more at the
// fire the run was for, so turning assistants on serves that fire, and a
// long-running watch's process starts again then. A row no longer running
// is left alone.
func (s *Store) PauseAgentWakeRun(id, outcome string) (bool, error) {
	return s.changeAgentWake(fmt.Sprintf("store: pause agent wake run %s", id),
		`UPDATE agent_wakes SET state = 'armed', last_outcome = ?,
		        next_at = CASE WHEN long_running = 0 THEN COALESCE(last_run_at, next_at) ELSE next_at END
		  WHERE id = ? AND state = 'running'`, outcome, id)
}

// ListLiveAssistantAgentWakes lists the armed or running loops and watches
// of the threads that live under an assistant, oldest first: what turning
// assistants back on resumes.
func (s *Store) ListLiveAssistantAgentWakes() ([]AgentWake, error) {
	rows, err := s.reader().Query(`SELECT ` + agentWakeColumns + ` FROM agent_wakes
		WHERE state IN ('armed','running') AND ` + threadUnderAssistantSQL("agent_wakes.thread_id") + `
		ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list live assistant agent wakes: %w", err)
	}
	return collectAgentWakes(rows, "store: list live assistant agent wakes")
}
