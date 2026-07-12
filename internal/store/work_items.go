package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WorkItem is one persisted workflow run record.
type WorkItem struct {
	ID            string          `json:"id"`
	ProjectID     string          `json:"projectId"`
	Goal          string          `json:"goal"`
	WorkflowID    string          `json:"workflowId"`
	WorkflowScope string          `json:"workflowScope"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	State         string          `json:"state"`
	Reason        string          `json:"reason,omitempty"`
	SortPosition  int             `json:"sortPosition"`
	Seeds         json.RawMessage `json:"seeds,omitempty"`
	StepMode      bool            `json:"stepMode"`
	WorktreePath  string          `json:"worktreePath,omitempty"`
	Branch        string          `json:"branch,omitempty"`
	BaseBranch    string          `json:"baseBranch,omitempty"`
	Budget        json.RawMessage `json:"budget,omitempty"`
	Source        string          `json:"source"`
	SourceRef     string          `json:"sourceRef,omitempty"`
	CreatedAt     int64           `json:"createdAt"`
	StartedAt     int64           `json:"startedAt,omitempty"`
	EndedAt       int64           `json:"endedAt,omitempty"`
}

// WorkItemListFilter narrows ListWorkItems by project and optional states.
type WorkItemListFilter struct {
	ProjectID string   `json:"projectId"`
	States    []string `json:"states,omitempty"`
}

const workItemColumns = `id, project_id, goal, workflow_id, workflow_scope,
snapshot, state, reason, sort_position, seeds, step_mode, worktree_path,
branch, base_branch, budget, source, source_ref, created_at, started_at, ended_at`

func jsonText(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	return string(value)
}

func scanWorkItem(scanner interface{ Scan(...any) error }) (WorkItem, error) {
	var item WorkItem
	var snapshot, seeds, budget string
	var stepMode int
	if err := scanner.Scan(
		&item.ID, &item.ProjectID, &item.Goal, &item.WorkflowID, &item.WorkflowScope,
		&snapshot, &item.State, &item.Reason, &item.SortPosition, &seeds, &stepMode,
		&item.WorktreePath, &item.Branch, &item.BaseBranch, &budget, &item.Source,
		&item.SourceRef, &item.CreatedAt, &item.StartedAt, &item.EndedAt,
	); err != nil {
		return WorkItem{}, err
	}
	item.Snapshot = json.RawMessage(snapshot)
	item.Seeds = json.RawMessage(seeds)
	item.Budget = json.RawMessage(budget)
	item.StepMode = stepMode != 0
	return item, nil
}

func (s *Store) CreateWorkItem(item WorkItem) error {
	_, err := s.db.Exec(
		`INSERT INTO work_items (`+workItemColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ProjectID, item.Goal, item.WorkflowID, item.WorkflowScope,
		jsonText(item.Snapshot), item.State, item.Reason, item.SortPosition,
		jsonText(item.Seeds), boolToInt(item.StepMode), item.WorktreePath, item.Branch,
		item.BaseBranch, jsonText(item.Budget), item.Source, item.SourceRef,
		item.CreatedAt, item.StartedAt, item.EndedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create work item %s: %w", item.ID, err)
	}
	return nil
}

func (s *Store) GetWorkItem(id string) (WorkItem, error) {
	item, err := scanWorkItem(s.db.QueryRow(
		`SELECT `+workItemColumns+` FROM work_items WHERE id = ?`, id,
	))
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: get work item %s: %w", id, err)
	}
	return item, nil
}

// ListWorkItems returns matching items in queue order. ProjectID is required;
// an empty States slice includes every state.
func (s *Store) ListWorkItems(filter WorkItemListFilter) ([]WorkItem, error) {
	query := `SELECT ` + workItemColumns + ` FROM work_items WHERE project_id = ?`
	args := []any{filter.ProjectID}
	if len(filter.States) > 0 {
		query += ` AND state IN (` + strings.TrimRight(strings.Repeat("?,", len(filter.States)), ",") + `)`
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	query += ` ORDER BY sort_position ASC, created_at ASC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list work items: %w", err)
	}
	defer rows.Close()

	items := make([]WorkItem, 0)
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list work items: scan: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list work items: iterate: %w", err)
	}
	return items, nil
}

// UpdateWorkItemState writes the transition result atomically. Transition
// validity belongs to the workflow engine; the store enforces only schema
// constraints.
func (s *Store) UpdateWorkItemState(id, state, reason string, endedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET state = ?, reason = ?, ended_at = ? WHERE id = ?`,
		state, reason, endedAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item state %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item state %s", id))
}

// UpdateWorkItemRunStart freezes the resolved workflow and workspace fields in
// one write immediately before execution begins.
func (s *Store) UpdateWorkItemRunStart(id string, snapshot json.RawMessage, worktreePath, branch, baseBranch string, startedAt int64) error {
	result, err := s.db.Exec(
		`UPDATE work_items
		 SET snapshot = ?, worktree_path = ?, branch = ?, base_branch = ?, started_at = ?
		 WHERE id = ?`,
		jsonText(snapshot), worktreePath, branch, baseBranch, startedAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item run start %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item run start %s", id))
}

// ReorderQueuedWorkItems assigns dense positions to the project's complete
// queued set. Rejecting partial or duplicate sets prevents ambiguous positions.
func (s *Store) ReorderQueuedWorkItems(projectID string, orderedIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: reorder queued work items: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT id FROM work_items WHERE project_id = ? AND state = 'queued'`, projectID,
	)
	if err != nil {
		return fmt.Errorf("store: reorder queued work items: list queued set: %w", err)
	}
	queued := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: reorder queued work items: scan queued set: %w", err)
		}
		queued[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: reorder queued work items: iterate queued set: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: reorder queued work items: close queued set: %w", err)
	}
	if len(orderedIDs) != len(queued) {
		return fmt.Errorf("store: reorder queued work items: got %d ids for %d queued items", len(orderedIDs), len(queued))
	}
	seen := make(map[string]struct{}, len(orderedIDs))
	for _, id := range orderedIDs {
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("store: reorder queued work items: duplicate id %s", id)
		}
		seen[id] = struct{}{}
		if _, exists := queued[id]; !exists {
			return fmt.Errorf("store: reorder queued work items: id %s is not queued in project %s", id, projectID)
		}
	}
	if len(orderedIDs) == 0 {
		return nil
	}

	stmt, err := tx.Prepare(
		`UPDATE work_items SET sort_position = ? WHERE id = ? AND project_id = ? AND state = 'queued'`,
	)
	if err != nil {
		return fmt.Errorf("store: reorder queued work items: prepare: %w", err)
	}
	defer stmt.Close()
	for position, id := range orderedIDs {
		result, err := stmt.Exec(position, id, projectID)
		if err != nil {
			return fmt.Errorf("store: reorder queued work item %s: %w", id, err)
		}
		if err := requireRowsAffected(result, fmt.Sprintf("store: reorder queued work item %s", id)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: reorder queued work items: commit: %w", err)
	}
	return nil
}
