package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Automation is a persisted workflow trigger definition. The trailing fields
// are the scheduler's fire record (migration v40): what the last trigger did,
// including the fires it deliberately did not turn into runs.
type Automation struct {
	ID            string          `json:"id"`
	ProjectID     string          `json:"projectId"`
	WorkflowID    string          `json:"workflowId"`
	WorkflowScope string          `json:"workflowScope"`
	Name          string          `json:"name"`
	Enabled       bool            `json:"enabled"`
	Trigger       json.RawMessage `json:"trigger"`
	Condition     json.RawMessage `json:"condition,omitempty"`
	Seeds         json.RawMessage `json:"seeds,omitempty"`
	Notes         string          `json:"notes"`
	CreatedAt     int64           `json:"createdAt"`
	UpdatedAt     int64           `json:"updatedAt"`
	// LastFiredAt / LastRunItemID record the last fire that actually started a
	// run. SkipCount / LastSkipAt / LastSkipReason record the fires that did
	// not, with the reason a human reads off the automation's row.
	LastFiredAt    int64  `json:"lastFiredAt"`
	LastRunItemID  string `json:"lastRunItemId"`
	SkipCount      int64  `json:"skipCount"`
	LastSkipAt     int64  `json:"lastSkipAt"`
	LastSkipReason string `json:"lastSkipReason"`
}

const automationColumns = `id, project_id, workflow_id, workflow_scope, name,
enabled, trigger, condition, seeds, notes, created_at, updated_at,
last_fired_at, last_run_item_id, skip_count, last_skip_at, last_skip_reason`

// automationInsertColumns omits the fire-record columns, which are owned by the
// scheduler's record calls and never supplied by a definition write.
const automationInsertColumns = `id, project_id, workflow_id, workflow_scope, name,
enabled, trigger, condition, seeds, notes, created_at, updated_at`

func scanAutomation(scanner interface{ Scan(...any) error }) (Automation, error) {
	var automation Automation
	var enabled int
	var trigger, condition, seeds string
	if err := scanner.Scan(
		&automation.ID, &automation.ProjectID, &automation.WorkflowID,
		&automation.WorkflowScope, &automation.Name, &enabled, &trigger,
		&condition, &seeds, &automation.Notes, &automation.CreatedAt,
		&automation.UpdatedAt, &automation.LastFiredAt, &automation.LastRunItemID,
		&automation.SkipCount, &automation.LastSkipAt, &automation.LastSkipReason,
	); err != nil {
		return Automation{}, err
	}
	automation.Enabled = enabled != 0
	automation.Trigger = json.RawMessage(trigger)
	automation.Condition = json.RawMessage(condition)
	automation.Seeds = json.RawMessage(seeds)
	return automation, nil
}

func (s *Store) CreateAutomation(automation Automation) error {
	_, err := s.db.Exec(
		`INSERT INTO automations (`+automationInsertColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		automation.ID, automation.ProjectID, automation.WorkflowID,
		automation.WorkflowScope, automation.Name, boolToInt(automation.Enabled),
		jsonText(automation.Trigger), jsonText(automation.Condition),
		jsonText(automation.Seeds), automation.Notes, automation.CreatedAt,
		automation.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create automation %s: %w", automation.ID, err)
	}
	return nil
}

func (s *Store) GetAutomation(id string) (Automation, error) {
	automation, err := scanAutomation(s.db.QueryRow(
		`SELECT `+automationColumns+` FROM automations WHERE id = ?`, id,
	))
	if err != nil {
		return Automation{}, fmt.Errorf("store: get automation %s: %w", id, err)
	}
	return automation, nil
}

func (s *Store) ListAutomations(projectID string) ([]Automation, error) {
	rows, err := s.db.Query(
		`SELECT `+automationColumns+` FROM automations
		 WHERE project_id = ? ORDER BY created_at ASC, id ASC`, projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list automations for project %s: %w", projectID, err)
	}
	defer rows.Close()
	automations := make([]Automation, 0)
	for rows.Next() {
		automation, err := scanAutomation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list automations for project %s: scan: %w", projectID, err)
		}
		automations = append(automations, automation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list automations for project %s: iterate: %w", projectID, err)
	}
	return automations, nil
}

// ListEnabledAutomations returns every enabled automation across every project,
// which is the set the scheduler computes its next fire from. Disabled rows are
// excluded here rather than filtered by the caller: a disabled automation has no
// schedule, and leaving it in the list would make "next fire" a lie.
func (s *Store) ListEnabledAutomations() ([]Automation, error) {
	rows, err := s.db.Query(
		`SELECT ` + automationColumns + ` FROM automations
		 WHERE enabled = 1 ORDER BY created_at ASC, id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled automations: %w", err)
	}
	defer rows.Close()
	automations := make([]Automation, 0)
	for rows.Next() {
		automation, err := scanAutomation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list enabled automations: scan: %w", err)
		}
		automations = append(automations, automation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list enabled automations: iterate: %w", err)
	}
	return automations, nil
}

// RecordAutomationFire stamps a fire that started a run. It deliberately does
// not touch updated_at: a fire is not an edit of the definition, and the
// scheduler recomputes its schedule from the definition alone.
func (s *Store) RecordAutomationFire(id string, firedAt int64, itemID string) error {
	result, err := s.db.Exec(
		`UPDATE automations SET last_fired_at = ?, last_run_item_id = ? WHERE id = ?`,
		firedAt, itemID, id,
	)
	if err != nil {
		return fmt.Errorf("store: record automation fire %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: record automation fire %s", id))
}

// RecordAutomationSkip stamps a fire that did not start a run, with the reason.
// The counter is monotonic per automation so the row shows both the latest skip
// and how often skipping has happened at all.
func (s *Store) RecordAutomationSkip(id string, at int64, reason string) error {
	result, err := s.db.Exec(
		`UPDATE automations
		 SET skip_count = skip_count + 1, last_skip_at = ?, last_skip_reason = ?
		 WHERE id = ?`,
		at, reason, id,
	)
	if err != nil {
		return fmt.Errorf("store: record automation skip %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: record automation skip %s", id))
}

// UpdateAutomation replaces the editable automation definition fields.
func (s *Store) UpdateAutomation(automation Automation) error {
	result, err := s.db.Exec(
		`UPDATE automations SET project_id = ?, workflow_id = ?, workflow_scope = ?,
		 name = ?, enabled = ?, trigger = ?, condition = ?, seeds = ?, updated_at = ?
		 WHERE id = ?`,
		automation.ProjectID, automation.WorkflowID, automation.WorkflowScope,
		automation.Name, boolToInt(automation.Enabled), jsonText(automation.Trigger),
		jsonText(automation.Condition), jsonText(automation.Seeds),
		automation.UpdatedAt, automation.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update automation %s: %w", automation.ID, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update automation %s", automation.ID))
}

func (s *Store) DeleteAutomation(id string) error {
	result, err := s.db.Exec(`DELETE FROM automations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete automation %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: delete automation %s", id))
}

func (s *Store) SetAutomationEnabled(id string, enabled bool, updatedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE automations SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), updatedAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: set automation enabled %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set automation enabled %s", id))
}

func (s *Store) GetAutomationNotes(id string) (string, error) {
	var notes string
	if err := s.db.QueryRow(`SELECT notes FROM automations WHERE id = ?`, id).Scan(&notes); err != nil {
		return "", fmt.Errorf("store: get automation notes %s: %w", id, err)
	}
	return notes, nil
}

func (s *Store) SetAutomationNotes(id, notes string, updatedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE automations SET notes = ?, updated_at = ? WHERE id = ?`, notes, updatedAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: set automation notes %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set automation notes %s", id))
}

// AutomationRun identifies one run an automation started. It is a work_items
// read that lives here because it exists for exactly one caller: §11's
// skip-if-running overlap policy, which asks "does this automation still have a
// run that has not finished".
type AutomationRun struct {
	ItemID string `json:"itemId"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// ActiveAutomationRun returns the newest non-terminal run this automation
// started, if any.
//
// Non-terminal is `running` OR `needs-human`: a parked run is unfinished work
// the next fire would overlap, and a scheduled job silently stacking runs behind
// a park is exactly the failure the recorded skip is meant to surface. The
// second source_ref predicate is what lets the partial v40 index apply — a
// bound parameter alone cannot prove the index's non-empty-source_ref clause.
func (s *Store) ActiveAutomationRun(automationID string) (AutomationRun, bool, error) {
	var run AutomationRun
	err := s.db.QueryRow(
		`SELECT id, state, reason FROM work_items
		 WHERE source = 'automation' AND source_ref = ? AND source_ref <> ''
		   AND state IN ('running','needs-human')
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		automationID,
	).Scan(&run.ItemID, &run.State, &run.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationRun{}, false, nil
	}
	if err != nil {
		return AutomationRun{}, false, fmt.Errorf("store: active automation run %s: %w", automationID, err)
	}
	return run, true, nil
}

// AutomationCursor is the last processed position for one trigger source.
type AutomationCursor struct {
	AutomationID string `json:"automationId"`
	SourceKey    string `json:"sourceKey"`
	Cursor       string `json:"cursor"`
	UpdatedAt    int64  `json:"updatedAt"`
}

func (s *Store) GetAutomationCursor(automationID, sourceKey string) (AutomationCursor, error) {
	var cursor AutomationCursor
	err := s.db.QueryRow(
		`SELECT automation_id, source_key, cursor, updated_at
		 FROM automation_cursors WHERE automation_id = ? AND source_key = ?`,
		automationID, sourceKey,
	).Scan(&cursor.AutomationID, &cursor.SourceKey, &cursor.Cursor, &cursor.UpdatedAt)
	if err != nil {
		return AutomationCursor{}, fmt.Errorf("store: get automation cursor %s/%s: %w", automationID, sourceKey, err)
	}
	return cursor, nil
}

func (s *Store) SetAutomationCursor(cursor AutomationCursor) error {
	_, err := s.db.Exec(
		`INSERT INTO automation_cursors (automation_id, source_key, cursor, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(automation_id, source_key) DO UPDATE SET
		 cursor = excluded.cursor, updated_at = excluded.updated_at`,
		cursor.AutomationID, cursor.SourceKey, cursor.Cursor, cursor.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: set automation cursor %s/%s: %w", cursor.AutomationID, cursor.SourceKey, err)
	}
	return nil
}
