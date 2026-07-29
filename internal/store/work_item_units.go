package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Work item unit kinds and statuses. A unit row exists from the moment its
// phase attempt expands, so `pending` is the birth state and every later state
// is a transition the engine persists as it happens.
const (
	WorkItemUnitKindUnit = "unit"
	WorkItemUnitKindJoin = "join"

	WorkItemUnitPending   = "pending"
	WorkItemUnitRunning   = "running"
	WorkItemUnitDone      = "done"
	WorkItemUnitFailed    = "failed"
	WorkItemUnitDropped   = "dropped"
	WorkItemUnitTakenOver = "taken-over"
)

// WorkItemUnit is one fan-out unit (or the join) of one phase attempt.
//
// Branch and worktree path are registered as soon as the runner provisions
// them, not when the unit finishes: a crash between provisioning and completion
// must still leave the sub-worktree discoverable.
type WorkItemUnit struct {
	ItemID        string          `json:"itemId"`
	PhaseID       string          `json:"phaseId"`
	Attempt       int             `json:"attempt"`
	UnitID        string          `json:"unitId"`
	UnitIndex     int             `json:"unitIndex"`
	Kind          string          `json:"kind"`
	Provider      string          `json:"provider,omitempty"`
	Model         string          `json:"model,omitempty"`
	ThreadID      string          `json:"threadId,omitempty"`
	Branch        string          `json:"branch,omitempty"`
	WorktreePath  string          `json:"worktreePath,omitempty"`
	NarrativePath string          `json:"narrativePath,omitempty"`
	Status        string          `json:"status"`
	UnitAttempt   int             `json:"unitAttempt"`
	Envelope      json.RawMessage `json:"envelope,omitempty"`
	Feedback      string          `json:"feedback,omitempty"`
	StartedAt     int64           `json:"startedAt,omitempty"`
	EndedAt       int64           `json:"endedAt,omitempty"`
}

const workItemUnitColumns = `item_id, phase_id, attempt, unit_id, unit_index, kind,
provider, model, thread_id, branch, worktree_path, narrative_path, status,
unit_attempt, envelope, feedback, started_at, ended_at`

func scanWorkItemUnit(scanner interface{ Scan(...any) error }) (WorkItemUnit, error) {
	var unit WorkItemUnit
	var envelope string
	if err := scanner.Scan(
		&unit.ItemID, &unit.PhaseID, &unit.Attempt, &unit.UnitID, &unit.UnitIndex, &unit.Kind,
		&unit.Provider, &unit.Model, &unit.ThreadID, &unit.Branch, &unit.WorktreePath,
		&unit.NarrativePath, &unit.Status, &unit.UnitAttempt, &envelope, &unit.Feedback,
		&unit.StartedAt, &unit.EndedAt,
	); err != nil {
		return WorkItemUnit{}, err
	}
	unit.Envelope = json.RawMessage(envelope)
	return unit, nil
}

// CreateWorkItemUnits writes one phase attempt's expanded units in a single
// transaction. An attempt's unit set is decided at once, so it is persisted at
// once: a partially written expansion would make the attempt's width ambiguous
// after a crash.
func (s *Store) CreateWorkItemUnits(units []WorkItemUnit) error {
	if len(units) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: create work item units: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.Prepare(
		`INSERT INTO work_item_units (` + workItemUnitColumns + `)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("store: create work item units: prepare: %w", err)
	}
	defer statement.Close()
	for _, unit := range units {
		if unit.UnitAttempt < 1 {
			unit.UnitAttempt = 1
		}
		if _, err := statement.Exec(
			unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, unit.UnitIndex, unit.Kind,
			unit.Provider, unit.Model, unit.ThreadID, unit.Branch, unit.WorktreePath,
			unit.NarrativePath, unit.Status, unit.UnitAttempt, jsonText(unit.Envelope),
			unit.Feedback, unit.StartedAt, unit.EndedAt,
		); err != nil {
			return fmt.Errorf("store: create work item unit %s/%s/%d/%s: %w", unit.ItemID, unit.PhaseID, unit.Attempt, unit.UnitID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create work item units: commit: %w", err)
	}
	return nil
}

// StartWorkItemUnit moves a unit to running for a given per-unit attempt. The
// envelope is cleared because the new attempt has not produced one yet; the
// feedback the attempt carries is written by the same call, so a retry's input
// is visible in the record.
func (s *Store) StartWorkItemUnit(itemID, phaseID string, attempt int, unitID string, unitAttempt int, feedback string, startedAt int64) error {
	if unitAttempt < 1 {
		return fmt.Errorf("store: start work item unit %s/%s/%d/%s: unit attempt must be >= 1", itemID, phaseID, attempt, unitID)
	}
	result, err := s.db.Exec(
		`UPDATE work_item_units
		 SET status = 'running', unit_attempt = ?, feedback = ?, envelope = '',
		     started_at = ?, ended_at = 0
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND unit_id = ?`,
		unitAttempt, feedback, startedAt, itemID, phaseID, attempt, unitID,
	)
	if err != nil {
		return fmt.Errorf("store: start work item unit %s/%s/%d/%s: %w", itemID, phaseID, attempt, unitID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: start work item unit %s/%s/%d/%s", itemID, phaseID, attempt, unitID))
}

// RetryWorkItemUnit returns a settled unit to `pending` so its attempt can
// launch it again. The row is reused rather than superseded — a unit's identity
// inside a phase attempt is its id, and `unit_attempt` counts the tries — so the
// previous try's envelope and timestamps are cleared.
//
// The reopen writes the try the unit is now ON and the feedback that try will
// carry, rather than leaving both to the eventual start. A repair that leaves
// the run parked (one unit retried while another still rests failed, a
// retry-all with a taken-over unit) evicts the item from memory, so anything
// held only in memory is lost and the unit's next start would inherit the
// previous FAILURE note as its input and a try count one behind the truth. The
// row is the only thing that survives the park, so the row is what carries it.
//
// `unitAttempt` is the value StartWorkItemUnit will later write again for the
// same try. That is not a double bump: the caller bumps its counter once and
// both writes persist the same number, so a start that never happens (the run
// stays parked, the process dies) leaves the record exactly where the reopen
// put it.
func (s *Store) RetryWorkItemUnit(itemID, phaseID string, attempt int, unitID string, unitAttempt int, feedback string) error {
	if unitAttempt < 1 {
		return fmt.Errorf("store: retry work item unit %s/%s/%d/%s: unit attempt must be >= 1", itemID, phaseID, attempt, unitID)
	}
	result, err := s.db.Exec(
		`UPDATE work_item_units
		 SET status = 'pending', unit_attempt = ?, feedback = ?, envelope = '',
		     started_at = 0, ended_at = 0
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND unit_id = ?`,
		unitAttempt, feedback, itemID, phaseID, attempt, unitID,
	)
	if err != nil {
		return fmt.Errorf("store: retry work item unit %s/%s/%d/%s: %w", itemID, phaseID, attempt, unitID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: retry work item unit %s/%s/%d/%s", itemID, phaseID, attempt, unitID))
}

// AttachWorkItemUnitWorkspace records the branch and sub-worktree a unit was
// provisioned with. It is deliberately separate from AttachWorkItemUnitRun:
// isolation exists before the session does, and the row has to say so from the
// moment the worktree is on disk — a crash in between is exactly the case that
// would otherwise strand it. Clearing the worktree path (empty string) while
// keeping the branch is how consumed sub-worktrees are retired.
func (s *Store) AttachWorkItemUnitWorkspace(itemID, phaseID string, attempt int, unitID, branch, worktreePath string) error {
	result, err := s.db.Exec(
		`UPDATE work_item_units SET branch = ?, worktree_path = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND unit_id = ?`,
		branch, worktreePath, itemID, phaseID, attempt, unitID,
	)
	if err != nil {
		return fmt.Errorf("store: attach work item unit workspace %s/%s/%d/%s: %w", itemID, phaseID, attempt, unitID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: attach work item unit workspace %s/%s/%d/%s", itemID, phaseID, attempt, unitID))
}

// AttachWorkItemUnitRun records the AO thread and system-owned narrative path
// the runner created for a unit. It leaves branch and worktree alone — those
// were registered earlier by AttachWorkItemUnitWorkspace.
func (s *Store) AttachWorkItemUnitRun(itemID, phaseID string, attempt int, unitID, threadID, narrativePath string) error {
	result, err := s.db.Exec(
		`UPDATE work_item_units SET thread_id = ?, narrative_path = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND unit_id = ?`,
		threadID, narrativePath, itemID, phaseID, attempt, unitID,
	)
	if err != nil {
		return fmt.Errorf("store: attach work item unit run %s/%s/%d/%s: %w", itemID, phaseID, attempt, unitID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: attach work item unit run %s/%s/%d/%s", itemID, phaseID, attempt, unitID))
}

// CompleteWorkItemUnit records a unit's terminal status for its current
// attempt, with whatever envelope it produced and the note that explains a
// non-success.
func (s *Store) CompleteWorkItemUnit(itemID, phaseID string, attempt int, unitID, status string, envelope json.RawMessage, feedback string, endedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE work_item_units
		 SET status = ?, envelope = ?, feedback = ?, ended_at = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND unit_id = ?`,
		status, jsonText(envelope), feedback, endedAt, itemID, phaseID, attempt, unitID,
	)
	if err != nil {
		return fmt.Errorf("store: complete work item unit %s/%s/%d/%s: %w", itemID, phaseID, attempt, unitID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: complete work item unit %s/%s/%d/%s", itemID, phaseID, attempt, unitID))
}

// FailRunningWorkItemUnits marks every still-running unit of one phase attempt
// failed. It backs the teardown contract: a stopped attempt can leave no unit
// row claiming to be running, including after a crash where no in-memory state
// survived to be torn down. Pending units are deliberately untouched — they
// never started, so they stay launchable.
func (s *Store) FailRunningWorkItemUnits(itemID, phaseID string, attempt int, feedback string, endedAt int64) (int64, error) {
	result, err := s.db.Exec(
		`UPDATE work_item_units
		 SET status = 'failed', feedback = ?, ended_at = ?
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND status = 'running'`,
		feedback, endedAt, itemID, phaseID, attempt,
	)
	if err != nil {
		return 0, fmt.Errorf("store: fail running work item units %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: fail running work item units %s/%s/%d: rows affected: %w", itemID, phaseID, attempt, err)
	}
	return affected, nil
}

// ListWorkItemUnits returns every persisted unit of a run in attempt order.
func (s *Store) ListWorkItemUnits(itemID string) ([]WorkItemUnit, error) {
	rows, err := s.db.Query(
		`SELECT `+workItemUnitColumns+` FROM work_item_units
		 WHERE item_id = ?
		 ORDER BY phase_id ASC, attempt ASC, unit_index ASC, unit_id ASC`, itemID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item units %s: %w", itemID, err)
	}
	return collectWorkItemUnits(rows, fmt.Sprintf("store: list work item units %s", itemID))
}

// ListWorkItemPhaseUnits returns one phase attempt's units in launch order.
func (s *Store) ListWorkItemPhaseUnits(itemID, phaseID string, attempt int) ([]WorkItemUnit, error) {
	rows, err := s.db.Query(
		`SELECT `+workItemUnitColumns+` FROM work_item_units
		 WHERE item_id = ? AND phase_id = ? AND attempt = ?
		 ORDER BY unit_index ASC, unit_id ASC`, itemID, phaseID, attempt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list work item phase units %s/%s/%d: %w", itemID, phaseID, attempt, err)
	}
	return collectWorkItemUnits(rows, fmt.Sprintf("store: list work item phase units %s/%s/%d", itemID, phaseID, attempt))
}

// GetWorkItemUnitByThread resolves the unit row that owns an AO thread. A unit
// thread belongs to exactly one unit of one attempt, so this is the inverse of
// AttachWorkItemUnitRun and the lookup every thread-first entry point (a human
// steering a taken-over unit) starts from. The newest row wins if a thread was
// somehow reused, matching how phase threads resolve.
func (s *Store) GetWorkItemUnitByThread(threadID string) (WorkItemUnit, bool, error) {
	if threadID == "" {
		return WorkItemUnit{}, false, nil
	}
	unit, err := scanWorkItemUnit(s.db.QueryRow(
		`SELECT `+workItemUnitColumns+` FROM work_item_units
		 WHERE thread_id = ?
		 ORDER BY started_at DESC, attempt DESC
		 LIMIT 1`,
		threadID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemUnit{}, false, nil
	}
	if err != nil {
		return WorkItemUnit{}, false, fmt.Errorf("store: get work item unit for thread %s: %w", threadID, err)
	}
	return unit, true, nil
}

// GetWorkItemUnit returns one unit row.
func (s *Store) GetWorkItemUnit(itemID, phaseID string, attempt int, unitID string) (WorkItemUnit, bool, error) {
	unit, err := scanWorkItemUnit(s.db.QueryRow(
		`SELECT `+workItemUnitColumns+` FROM work_item_units
		 WHERE item_id = ? AND phase_id = ? AND attempt = ? AND unit_id = ?`,
		itemID, phaseID, attempt, unitID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItemUnit{}, false, nil
	}
	if err != nil {
		return WorkItemUnit{}, false, fmt.Errorf("store: get work item unit %s/%s/%d/%s: %w", itemID, phaseID, attempt, unitID, err)
	}
	return unit, true, nil
}

func collectWorkItemUnits(rows *sql.Rows, context string) ([]WorkItemUnit, error) {
	defer rows.Close()
	units := make([]WorkItemUnit, 0)
	for rows.Next() {
		unit, err := scanWorkItemUnit(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: scan: %w", context, err)
		}
		units = append(units, unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate: %w", context, err)
	}
	return units, nil
}
