package store

import (
	"encoding/json"
	"fmt"
)

// Automation is a persisted workflow trigger definition.
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
}

const automationColumns = `id, project_id, workflow_id, workflow_scope, name,
enabled, trigger, condition, seeds, notes, created_at, updated_at`

func scanAutomation(scanner interface{ Scan(...any) error }) (Automation, error) {
	var automation Automation
	var enabled int
	var trigger, condition, seeds string
	if err := scanner.Scan(
		&automation.ID, &automation.ProjectID, &automation.WorkflowID,
		&automation.WorkflowScope, &automation.Name, &enabled, &trigger,
		&condition, &seeds, &automation.Notes, &automation.CreatedAt,
		&automation.UpdatedAt,
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
		`INSERT INTO automations (`+automationColumns+`)
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
