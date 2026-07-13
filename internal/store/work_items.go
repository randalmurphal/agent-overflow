package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/threadmode"
)

// WorkItem is one persisted workflow run record.
type WorkItem struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"projectId"`
	Goal           string          `json:"goal"`
	WorkflowID     string          `json:"workflowId"`
	WorkflowScope  string          `json:"workflowScope"`
	Snapshot       json.RawMessage `json:"snapshot,omitempty"`
	State          string          `json:"state"`
	Reason         string          `json:"reason,omitempty"`
	SortPosition   int             `json:"sortPosition"`
	Seeds          json.RawMessage `json:"seeds,omitempty"`
	StepMode       bool            `json:"stepMode"`
	WorktreePath   string          `json:"worktreePath,omitempty"`
	Branch         string          `json:"branch,omitempty"`
	BaseBranch     string          `json:"baseBranch,omitempty"`
	Budget         json.RawMessage `json:"budget,omitempty"`
	Source         string          `json:"source"`
	SourceRef      string          `json:"sourceRef,omitempty"`
	TriageThreadID string          `json:"triageThreadId,omitempty"`
	Disposition    json.RawMessage `json:"disposition,omitempty"`
	Digest         json.RawMessage `json:"digest,omitempty"`
	CreatedAt      int64           `json:"createdAt"`
	StartedAt      int64           `json:"startedAt,omitempty"`
	EndedAt        int64           `json:"endedAt,omitempty"`
}

// WorkItemAttentionContext is the narrow item/phase projection used while an
// attention transition is synchronously preparing its digest. It deliberately
// excludes the frozen workflow snapshot and phase fields the digest does not
// consume.
type WorkItemAttentionContext struct {
	Item           WorkItem
	PhaseID        string
	OutputEnvelope json.RawMessage
	Check          string
}

// WorkItemListFilter narrows ListWorkItems by optional project and states.
// An empty ProjectID matches every project. UnresolvedOnly excludes cancelled
// items and every item with a disposition receipt, regardless of state.
type WorkItemListFilter struct {
	ProjectID      string   `json:"projectId"`
	States         []string `json:"states,omitempty"`
	UnresolvedOnly bool     `json:"unresolvedOnly,omitempty"`
}

const unresolvedWorkItemsPredicate = `disposition = '' AND state IN ('queued','running','needs-human','done','failed')`

const workItemColumns = `id, project_id, goal, workflow_id, workflow_scope,
snapshot, state, reason, sort_position, seeds, step_mode, worktree_path,
branch, base_branch, budget, source, source_ref, triage_thread_id,
disposition, digest, created_at, started_at, ended_at`

const workItemSummaryColumns = `id, project_id, goal, workflow_id, workflow_scope,
'', state, reason, sort_position, '', step_mode, worktree_path,
branch, base_branch, '', source, source_ref, triage_thread_id,
disposition, '', created_at, started_at, ended_at`

func jsonText(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	return string(value)
}

func scanWorkItem(scanner interface{ Scan(...any) error }) (WorkItem, error) {
	var item WorkItem
	var snapshot, seeds, budget, disposition, digest string
	var stepMode int
	if err := scanner.Scan(
		&item.ID, &item.ProjectID, &item.Goal, &item.WorkflowID, &item.WorkflowScope,
		&snapshot, &item.State, &item.Reason, &item.SortPosition, &seeds, &stepMode,
		&item.WorktreePath, &item.Branch, &item.BaseBranch, &budget, &item.Source,
		&item.SourceRef, &item.TriageThreadID, &disposition, &digest,
		&item.CreatedAt, &item.StartedAt, &item.EndedAt,
	); err != nil {
		return WorkItem{}, err
	}
	item.Snapshot = json.RawMessage(snapshot)
	item.Seeds = json.RawMessage(seeds)
	item.Budget = json.RawMessage(budget)
	item.Disposition = json.RawMessage(disposition)
	item.Digest = json.RawMessage(digest)
	item.StepMode = stepMode != 0
	return item, nil
}

func (s *Store) CreateWorkItem(item WorkItem) error {
	_, err := s.db.Exec(
		`INSERT INTO work_items (`+workItemColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ProjectID, item.Goal, item.WorkflowID, item.WorkflowScope,
		jsonText(item.Snapshot), item.State, item.Reason, item.SortPosition,
		jsonText(item.Seeds), boolToInt(item.StepMode), item.WorktreePath, item.Branch,
		item.BaseBranch, jsonText(item.Budget), item.Source, item.SourceRef,
		item.TriageThreadID, jsonText(item.Disposition), jsonText(item.Digest),
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

// GetWorkItemAttentionContext loads only the fields needed for an attention
// digest and notification. SQLite extracts the current phase's check binding
// from the snapshot so the multi-megabyte snapshot never crosses into Go on
// the engine owner's synchronous event path.
func (s *Store) GetWorkItemAttentionContext(id string) (WorkItemAttentionContext, error) {
	const query = `SELECT
		w.id, w.project_id, w.goal, w.state, w.reason, w.worktree_path, w.digest,
		COALESCE(p.phase_id, ''), COALESCE(p.output_envelope, ''),
		COALESCE((
			SELECT json_extract(phase.value, '$.check')
			FROM json_each(NULLIF(w.snapshot, ''), '$.workflow.phases') AS phase
			WHERE json_extract(phase.value, '$.id') = p.phase_id
			LIMIT 1
		), '')
	FROM work_items AS w
	LEFT JOIN work_item_phases AS p ON p.rowid = (
		SELECT latest.rowid FROM work_item_phases AS latest
		WHERE latest.item_id = w.id
		ORDER BY latest.started_at DESC, latest.phase_id DESC, latest.attempt DESC
		LIMIT 1
	)
	WHERE w.id = ?`
	var context WorkItemAttentionContext
	var digest, output string
	if err := s.db.QueryRow(query, id).Scan(
		&context.Item.ID, &context.Item.ProjectID, &context.Item.Goal,
		&context.Item.State, &context.Item.Reason, &context.Item.WorktreePath,
		&digest, &context.PhaseID, &output, &context.Check,
	); err != nil {
		return WorkItemAttentionContext{}, fmt.Errorf("store: get work item attention context %s: %w", id, err)
	}
	context.Item.Digest = json.RawMessage(digest)
	context.OutputEnvelope = json.RawMessage(output)
	return context, nil
}

// GetWorkItemByPhaseThread resolves the workflow item that owns a phase
// thread. A phase thread may be reused by later attempts, so the newest phase
// row is only the lookup anchor; ownership remains item-scoped.
func (s *Store) GetWorkItemByPhaseThread(threadID string) (WorkItem, error) {
	item, err := scanWorkItem(s.db.QueryRow(
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE id = (
		     SELECT item_id FROM work_item_phases
		      WHERE thread_id = ?
		      ORDER BY started_at DESC, attempt DESC
		      LIMIT 1
		 )`,
		threadID,
	))
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: get work item for phase thread %s: %w", threadID, err)
	}
	return item, nil
}

// ListWorkItems returns matching items in queue order. An empty ProjectID
// lists every project; an empty States slice includes every state.
func (s *Store) ListWorkItems(filter WorkItemListFilter) ([]WorkItem, error) {
	return s.listWorkItems(filter, workItemColumns)
}

// ListWorkItemSummaries returns matching items without loading snapshot,
// seeds, or budget payloads. GetWorkItem is the detail path for those fields.
func (s *Store) ListWorkItemSummaries(filter WorkItemListFilter) ([]WorkItem, error) {
	return s.listWorkItems(filter, workItemSummaryColumns)
}

func (s *Store) listWorkItems(filter WorkItemListFilter, columns string) ([]WorkItem, error) {
	query := `SELECT ` + columns + ` FROM work_items`
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 1+len(filter.States))
	if filter.ProjectID != "" {
		conditions = append(conditions, `project_id = ?`)
		args = append(args, filter.ProjectID)
	}
	if len(filter.States) > 0 {
		conditions = append(conditions, `state IN (`+strings.TrimRight(strings.Repeat("?,", len(filter.States)), ",")+`)`)
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	if filter.UnresolvedOnly {
		conditions = append(conditions, unresolvedWorkItemsPredicate)
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
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

// NextWorkItemSortPosition returns the queue position after the project's
// current maximum without materializing any work items.
func (s *Store) NextWorkItemSortPosition(projectID string) (int, error) {
	var position int
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(sort_position), -1) + 1 FROM work_items WHERE project_id = ?`,
		projectID,
	).Scan(&position); err != nil {
		return 0, fmt.Errorf("store: next work item sort position: %w", err)
	}
	return position, nil
}

// PredictWorkItemQueuePosition returns the one-based global insertion rank for
// a new project item using the scheduler's sort-position and creation-time
// ordering. Existing items created in the same millisecond sort first; the
// final UUID tie-break is unknowable until enqueue allocates the item ID.
func (s *Store) PredictWorkItemQueuePosition(projectID string, createdAt int64) (int, error) {
	sortPosition, err := s.NextWorkItemSortPosition(projectID)
	if err != nil {
		return 0, err
	}
	var before int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM work_items
		 WHERE state = 'queued'
		   AND (sort_position < ? OR (sort_position = ? AND created_at <= ?))`,
		sortPosition, sortPosition, createdAt,
	).Scan(&before); err != nil {
		return 0, fmt.Errorf("store: predict work item queue position: %w", err)
	}
	return before + 1, nil
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

// ReenqueueFailedWorkItem atomically returns a failed item to the queue tail.
// Transition validity belongs to the workflow engine.
func (s *Store) ReenqueueFailedWorkItem(id string, sortPosition int) error {
	result, err := s.db.Exec(
		`UPDATE work_items
		 SET state = 'queued', reason = '', ended_at = 0, sort_position = ?
		 WHERE id = ?`,
		sortPosition, id,
	)
	if err != nil {
		return fmt.Errorf("store: re-enqueue failed work item %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: re-enqueue failed work item %s", id))
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

// UpdateWorkItemWorkspace records a successfully provisioned workspace. The
// runner calls this only after worktree creation and setup hooks complete.
func (s *Store) UpdateWorkItemWorkspace(id, worktreePath, branch, baseBranch string) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET worktree_path = ?, branch = ?, base_branch = ? WHERE id = ?`,
		worktreePath, branch, baseBranch, id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item workspace %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item workspace %s", id))
}

// UpdateWorkItemDisposition persists the durable receipt for a completed
// disposition action. The schema validates the JSON representation.
func (s *Store) UpdateWorkItemDisposition(id string, disposition json.RawMessage) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET disposition = ? WHERE id = ?`, jsonText(disposition), id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item disposition %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item disposition %s", id))
}

// UpdateWorkItemDigest replaces the human-facing run digest. Template and
// generated upgrades share this one persistence path.
func (s *Store) UpdateWorkItemDigest(id string, digest json.RawMessage) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET digest = ? WHERE id = ?`, jsonText(digest), id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item digest %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item digest %s", id))
}

// CountWorkItemsInStates returns the global count for the supplied lifecycle
// states without materializing run records.
func (s *Store) CountWorkItemsInStates(states ...string) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	query := `SELECT COUNT(*) FROM work_items WHERE state IN (` +
		strings.TrimRight(strings.Repeat("?,", len(states)), ",") + `)`
	args := make([]any, len(states))
	for index, state := range states {
		args[index] = state
	}
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count work items in states: %w", err)
	}
	return count, nil
}

// UpdateWorkItemTriageThread persists the open-or-return association used by
// item hand-off triage threads. Thread lifetime is deliberately independent,
// so deletion leaves the id as a stale pointer that the app replaces on the
// next open.
func (s *Store) UpdateWorkItemTriageThread(id, threadID string) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET triage_thread_id = ? WHERE id = ?`, threadID, id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item triage thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item triage thread %s", id))
}

// CreateWorkItemTriageThread makes the hand-off thread and its durable item
// association visible in one transaction. The project-level triage singleton
// is identified by the absence of this link, so a partially linked hand-off
// thread must never be observable after a crash.
func (s *Store) CreateWorkItemTriageThread(itemID string, thread Thread) error {
	if thread.Mode != threadmode.ModeWorkflowTriage {
		return fmt.Errorf("store: create work item triage thread: mode is %q", thread.Mode)
	}
	prepared, lastReadAtArg, err := prepareThreadForCreate(thread)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: create work item triage thread: begin: %w", err)
	}
	defer tx.Rollback()
	if err := insertThread(tx, prepared, lastReadAtArg); err != nil {
		return fmt.Errorf("store: create work item triage thread: insert thread: %w", err)
	}
	result, err := tx.Exec(`UPDATE work_items SET triage_thread_id = ? WHERE id = ?`, prepared.ID, itemID)
	if err != nil {
		return fmt.Errorf("store: create work item triage thread: link item %s: %w", itemID, err)
	}
	if err := requireRowsAffected(result, fmt.Sprintf("store: create work item triage thread: link item %s", itemID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: create work item triage thread: commit: %w", err)
	}
	return nil
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
