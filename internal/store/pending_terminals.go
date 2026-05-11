package store

import (
	"database/sql"
	"fmt"
)

// PendingBackgroundTaskTerminal stashes the host-side terminal state of
// a backgrounded Claude tool whose completion the agent has not yet
// observed. The triage layer inserts a row on system/task_updated and
// removes it when an observation event (system/task_notification or
// TaskOutput tool_result) writes the tool_completion sibling.
//
// The tray query joins against this table to hide launches whose
// process has exited but whose chat sibling has not been written yet —
// "tray = process state, chat = agent state" decoupling.
//
// ToolUseID may be empty when the launching tool_use_id is unknown
// (rare: parser map lost across reconnect with no items.meta.task_id
// fallback). Drain callers re-resolve the launch by task_id when the
// stored ToolUseID is empty.
//
// The only source that writes here is "task_updated" — Claude's
// host-side process exit. The startup-recovery path
// (Router.RecoverOrphanedBackgroundTasks) writes its
// `tool_completion` sibling directly without staging a stash row,
// so a crash mid-sweep leaves the launch re-discoverable on next
// boot.
type PendingBackgroundTaskTerminal struct {
	ThreadID   string
	TaskID     string
	ToolUseID  string
	Status     string
	ExitCode   *int64
	OutputFile string
	EndTime    *int64
	Source     string
	CreatedAt  int64
}

const pendingTerminalColumns = `
	thread_id, task_id, tool_use_id, status, exit_code,
	output_file, end_time, source, created_at`

// UpsertPendingBackgroundTerminal writes the stash row, replacing any
// existing entry for the same (thread_id, task_id). Used by triage on
// system/task_updated; the INSERT OR REPLACE shape is what makes
// reconnect-replay idempotent (the CLI re-emits historical task_updated
// envelopes on session resume).
func (s *Store) UpsertPendingBackgroundTerminal(t PendingBackgroundTaskTerminal) error {
	if t.ThreadID == "" || t.TaskID == "" {
		return fmt.Errorf("store: pending terminal requires thread_id and task_id")
	}
	if t.Status == "" || t.Source == "" {
		return fmt.Errorf("store: pending terminal requires status and source")
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO pending_background_task_terminals (`+pendingTerminalColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ThreadID, t.TaskID, t.ToolUseID, t.Status, nullableInt64(t.ExitCode),
		t.OutputFile, nullableInt64(t.EndTime), t.Source, t.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: upsert pending terminal %s/%s: %w", t.ThreadID, t.TaskID, err)
	}
	return nil
}

// GetPendingBackgroundTerminal reads a stash row without removing it.
// Used by the tray query path where the row stays until the agent
// observation drain.
func (s *Store) GetPendingBackgroundTerminal(threadID, taskID string) (PendingBackgroundTaskTerminal, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+pendingTerminalColumns+`
		   FROM pending_background_task_terminals
		  WHERE thread_id = ? AND task_id = ?`,
		threadID, taskID,
	)
	t, err := scanPendingTerminalRow(row)
	if err == sql.ErrNoRows {
		return PendingBackgroundTaskTerminal{}, false, nil
	}
	if err != nil {
		return PendingBackgroundTaskTerminal{}, false, fmt.Errorf("store: get pending terminal %s/%s: %w", threadID, taskID, err)
	}
	return t, true, nil
}

// TakePendingBackgroundTerminal atomically reads and deletes the stash
// row in a single transaction. Used by triage on the agent-observation
// event (task_notification / TaskOutput tool_result). Returns
// (zero, false, nil) when no row exists — observation arriving before
// task_updated, or for foreground-stall task_notifications that have no
// stash by design.
func (s *Store) TakePendingBackgroundTerminal(threadID, taskID string) (PendingBackgroundTaskTerminal, bool, error) {
	if threadID == "" || taskID == "" {
		return PendingBackgroundTaskTerminal{}, false, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return PendingBackgroundTaskTerminal{}, false, fmt.Errorf("store: begin take pending terminal %s/%s: %w", threadID, taskID, err)
	}
	defer tx.Rollback()

	row := tx.QueryRow(
		`SELECT `+pendingTerminalColumns+`
		   FROM pending_background_task_terminals
		  WHERE thread_id = ? AND task_id = ?`,
		threadID, taskID,
	)
	t, err := scanPendingTerminalRow(row)
	if err == sql.ErrNoRows {
		return PendingBackgroundTaskTerminal{}, false, nil
	}
	if err != nil {
		return PendingBackgroundTaskTerminal{}, false, fmt.Errorf("store: read pending terminal %s/%s: %w", threadID, taskID, err)
	}

	if _, err := tx.Exec(
		`DELETE FROM pending_background_task_terminals
		  WHERE thread_id = ? AND task_id = ?`,
		threadID, taskID,
	); err != nil {
		return PendingBackgroundTaskTerminal{}, false, fmt.Errorf("store: delete pending terminal %s/%s: %w", threadID, taskID, err)
	}
	if err := tx.Commit(); err != nil {
		return PendingBackgroundTaskTerminal{}, false, fmt.Errorf("store: commit take pending terminal %s/%s: %w", threadID, taskID, err)
	}
	return t, true, nil
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanPendingTerminalRow(row rowScanner) (PendingBackgroundTaskTerminal, error) {
	var (
		t        PendingBackgroundTaskTerminal
		exitCode sql.NullInt64
		endTime  sql.NullInt64
	)
	if err := row.Scan(
		&t.ThreadID, &t.TaskID, &t.ToolUseID, &t.Status, &exitCode,
		&t.OutputFile, &endTime, &t.Source, &t.CreatedAt,
	); err != nil {
		return PendingBackgroundTaskTerminal{}, err
	}
	if exitCode.Valid {
		t.ExitCode = &exitCode.Int64
	}
	if endTime.Valid {
		t.EndTime = &endTime.Int64
	}
	return t, nil
}

func nullableInt64(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

