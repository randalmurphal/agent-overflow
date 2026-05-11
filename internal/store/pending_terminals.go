package store

import (
	"database/sql"
	"encoding/json"
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

// ListPendingBackgroundCompletionsAsItems synthesizes tray-only
// `tool_completion` items from stash rows whose chat sibling hasn't
// been written yet. Mirrors the Codex tracker path
// (`triage.ListLiveCodexBackgroundTasks`) — these items are NOT
// persisted to `items`; they exist only so the tray can render
// "completed" the moment Claude's host process exits, without
// waiting for the agent-observation event that drives the real
// chat-side sibling write.
//
// The synthesized id MUST match triage's `nextToolCompletionID(launchID)`
// shape (`complete:<launchID>`) — that's the dedup key the frontend's
// `deriveTrayTasks` buckets by. The `NOT EXISTS` predicate in the
// query is the primary suppression mechanism once the real sibling
// lands; the frontend's `b.completion.createdAt < item.createdAt`
// check is a defensive backstop against any out-of-order arrival.
//
// `retentionCutoffMillis` matches the value passed to
// `ListLiveBackgroundTasks` so synthetic and real completions age
// out on the same clock.
func (s *Store) ListPendingBackgroundCompletionsAsItems(threadID string, retentionCutoffMillis int64) ([]Item, error) {
	if threadID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT
		    'complete:' || launch.id AS id,
		    launch.id AS launch_id,
		    launch.turn_index, launch.parent_id, launch.tool_name, launch.summary,
		    p.status, p.exit_code, p.output_file, p.task_id, p.created_at
		   FROM pending_background_task_terminals p
		   JOIN items launch
		     ON launch.thread_id = p.thread_id
		    AND launch.id = p.tool_use_id
		   WHERE p.thread_id = ?
		     AND p.created_at >= ?
		     AND launch.is_background = 1
		     AND launch.status = 'running'
		     AND NOT EXISTS (
		       SELECT 1 FROM items existing
		        WHERE existing.thread_id = p.thread_id
		          AND existing.completion_of = launch.id
		     )
		   ORDER BY p.created_at`,
		threadID, retentionCutoffMillis,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list pending background completions for %s: %w", threadID, err)
	}
	defer rows.Close()

	out := []Item{}
	for rows.Next() {
		var (
			id, launchID, parentID, toolName, launchSummary string
			turnIndex                                       int
			status, outputFile, taskID                      string
			exitCode                                        sql.NullInt64
			createdAt                                       int64
		)
		if err := rows.Scan(
			&id, &launchID, &turnIndex, &parentID, &toolName, &launchSummary,
			&status, &exitCode, &outputFile, &taskID, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan pending background completion: %w", err)
		}
		var exitCodePtr *int64
		if exitCode.Valid {
			exitCodePtr = &exitCode.Int64
		}
		out = append(out, Item{
			ID:           id,
			ThreadID:     threadID,
			TurnIndex:    turnIndex,
			ItemIndex:    0,
			Kind:         "tool_completion",
			Role:         "assistant",
			Status:       mapPendingTerminalStatus(status, exitCodePtr),
			Summary:      buildPendingTerminalSummary(launchSummary, status, exitCodePtr),
			ParentID:     parentID,
			IsBackground: true,
			CompletionOf: launchID,
			ToolName:     toolName,
			Meta:         buildPendingTerminalMeta(taskID, launchID, status, outputFile),
			CreatedAt:    createdAt,
			UpdatedAt:    createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pending background completions: %w", err)
	}
	return out, nil
}

// mapPendingTerminalStatus maps Claude wire `status` to the
// canonical `items.status` value the tray frontend's
// `completionStatusFor` expects. The precedence and arms mirror
// `triage.backgroundTerminalStatus` so synthetic completions render
// identically to persisted siblings; the contract is locked down by
// `TestPendingTerminalStatusParityWithTriage` in this package.
//
// Unknown statuses default to "errored" — never silently green-badge
// a state we don't recognise.
func mapPendingTerminalStatus(wireStatus string, exitCode *int64) string {
	if wireStatus == "killed" || wireStatus == "stopped" {
		return "killed"
	}
	if exitCode != nil && *exitCode != 0 {
		return "errored"
	}
	switch wireStatus {
	case "completed":
		return "completed"
	case "", "failed", "interrupted", "errored":
		if wireStatus == "" {
			return "completed"
		}
		return "errored"
	default:
		return "errored"
	}
}

// buildPendingTerminalSummary derives the tray summary. Mirrors
// `triage.buildBackgroundTerminalSummary` + `backgroundTerminalOutcome`
// so the synthetic and persisted rows produce the same "Bash -> done"
// / "Bash -> exit 1" shape. The contract is locked down by
// `TestPendingTerminalSummaryParityWithTriage` in this package.
//
// `stopped` is intentionally NOT mapped to "killed" here — triage's
// outcome arm only checks `Status == "killed"`. The wire-typed
// `stopped` variant routes through `task_updated{killed}` in
// triage.observeBackgroundTaskTerminal before reaching the sibling
// write, so a stash row never carries `stopped` in production.
func buildPendingTerminalSummary(launchSummary, wireStatus string, exitCode *int64) string {
	outcome := pendingTerminalOutcome(wireStatus, exitCode)
	if launchSummary == "" {
		if outcome == "" {
			return "done"
		}
		return outcome
	}
	if outcome == "" {
		return launchSummary
	}
	return launchSummary + " -> " + outcome
}

func pendingTerminalOutcome(wireStatus string, exitCode *int64) string {
	switch {
	case exitCode != nil:
		return fmt.Sprintf("exit %d", *exitCode)
	case wireStatus == "failed":
		return "failed"
	case wireStatus == "killed":
		return "killed"
	case wireStatus == "interrupted":
		return "interrupted"
	case wireStatus == "completed":
		return "done"
	default:
		return ""
	}
}

// buildPendingTerminalMeta produces the items.meta JSON blob,
// mirroring the shape `triage.backgroundCompletionItemMeta`
// produces for the persisted sibling: typed bools, not stringified.
// `synthetic: true` distinguishes this tray-only placeholder from
// the real sibling that lands once the agent observes the
// completion.
//
// Note: `task_id` is included for shape parity, not for the Stop
// button — `extractClaudeTaskID` is only called against the launch
// row in `ActivityRailBackgroundBody.svelte`.
func buildPendingTerminalMeta(taskID, toolUseID, wireStatus, outputFile string) string {
	fields := map[string]any{
		"task_id":       taskID,
		"tool_use_id":   toolUseID,
		"status":        wireStatus,
		"status_source": "task_updated",
		"synthetic":     true,
	}
	if outputFile != "" {
		fields["output_file"] = outputFile
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "{}"
	}
	return string(data)
}
