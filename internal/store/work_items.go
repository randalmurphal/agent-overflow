package store

import (
	"database/sql"
	"encoding/json"
	"errors"
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
	Seeds          json.RawMessage `json:"seeds,omitempty"`
	StepMode       bool            `json:"stepMode"`
	WorktreePath   string          `json:"worktreePath,omitempty"`
	Branch         string          `json:"branch,omitempty"`
	BaseBranch     string          `json:"baseBranch,omitempty"`
	Budget         json.RawMessage `json:"budget,omitempty"`
	Source         string          `json:"source"`
	SourceRef      string          `json:"sourceRef,omitempty"`
	TriageThreadID string          `json:"triageThreadId,omitempty"`
	OriginThreadID string          `json:"originThreadId,omitempty"`
	Disposition    json.RawMessage `json:"disposition,omitempty"`
	Digest         json.RawMessage `json:"digest,omitempty"`
	ParentItemID   string          `json:"parentItemId,omitempty"`
	ParentPhaseID  string          `json:"parentPhaseId,omitempty"`
	// ParentUnitID is set only when a call-bound fan-out unit created this run.
	// It is what distinguishes the children of one fan-out attempt from each
	// other; a `shape: call` phase makes one call per attempt and leaves it empty.
	ParentUnitID  string `json:"parentUnitId,omitempty"`
	ParentAttempt int    `json:"parentAttempt,omitempty"`
	CallDepth     int    `json:"callDepth,omitempty"`
	// SoftStop is a standing request to stop this run tree at its next call
	// boundary (D36). It is only ever set on a ROOT run — the tree is the unit a
	// human stops — and the engine consumes it: the boundary that fires clears
	// it, so arming is a one-shot rather than a mode a resume would re-trip on.
	SoftStop            bool   `json:"softStop,omitempty"`
	CreatedAt           int64  `json:"createdAt"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	EndedAt             int64  `json:"endedAt,omitempty"`
	CurrentPhaseID      string `json:"currentPhaseId,omitempty"`
	CurrentPhaseOrdinal int    `json:"currentPhaseOrdinal,omitempty"`
	PhaseCount          int    `json:"phaseCount,omitempty"`
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
	ProjectID string   `json:"projectId"`
	States    []string `json:"states,omitempty"`
	// ParentItemID restricts the listing to the runs one item called. Empty
	// matches every item regardless of linkage, so a plain project listing still
	// shows called runs alongside their callers.
	ParentItemID   string `json:"parentItemId,omitempty"`
	UnresolvedOnly bool   `json:"unresolvedOnly,omitempty"`
}

const unresolvedWorkItemsPredicate = `disposition = '' AND state IN ('running','needs-human','done','failed')`

func qualifiedUnresolvedWorkItemsPredicate(prefix string) string {
	return prefix + `disposition = '' AND ` + prefix + `state IN ('running','needs-human','done','failed')`
}

const workItemColumns = `id, project_id, goal, workflow_id, workflow_scope,
snapshot, state, reason, seeds, step_mode, worktree_path,
branch, base_branch, budget, source, source_ref, triage_thread_id, origin_thread_id,
disposition, digest, parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth,
soft_stop, created_at, started_at, ended_at`

const workItemSummaryListColumns = `w.id, w.project_id, w.goal, w.workflow_id, w.workflow_scope,
'', w.state, w.reason, '', w.step_mode, w.worktree_path,
w.branch, w.base_branch, '', w.source, w.source_ref, w.triage_thread_id, w.origin_thread_id,
w.disposition, '', w.parent_item_id, w.parent_phase_id, w.parent_unit_id, w.parent_attempt, w.call_depth,
w.soft_stop, w.created_at, w.started_at, w.ended_at`

const workItemSummaryProgressJoin = `
 LEFT JOIN work_item_phases AS current_phase ON current_phase.rowid = (
     SELECT latest.rowid FROM work_item_phases AS latest
      WHERE latest.item_id = w.id
      ORDER BY latest.started_at DESC, latest.phase_id DESC, latest.attempt DESC
      LIMIT 1
 )
 LEFT JOIN json_each(NULLIF(w.snapshot, ''), '$.workflow.phases') AS workflow_phase
   ON json_extract(workflow_phase.value, '$.id') = current_phase.phase_id`

const workItemSummaryProgressColumns = `,
COALESCE(current_phase.phase_id, ''),
COALESCE(CAST(workflow_phase.key AS INTEGER) + 1, 0),
COALESCE(json_array_length(NULLIF(w.snapshot, ''), '$.workflow.phases'), 0)`

func jsonText(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	return string(value)
}

func scanWorkItem(scanner interface{ Scan(...any) error }, includeProgress bool) (WorkItem, error) {
	var item WorkItem
	var snapshot, seeds, budget, disposition, digest string
	var stepMode, softStop int
	fields := []any{
		&item.ID, &item.ProjectID, &item.Goal, &item.WorkflowID, &item.WorkflowScope,
		&snapshot, &item.State, &item.Reason, &seeds, &stepMode,
		&item.WorktreePath, &item.Branch, &item.BaseBranch, &budget, &item.Source,
		&item.SourceRef, &item.TriageThreadID, &item.OriginThreadID, &disposition, &digest,
		&item.ParentItemID, &item.ParentPhaseID, &item.ParentUnitID, &item.ParentAttempt, &item.CallDepth,
		&softStop, &item.CreatedAt, &item.StartedAt, &item.EndedAt,
	}
	if includeProgress {
		fields = append(fields, &item.CurrentPhaseID, &item.CurrentPhaseOrdinal, &item.PhaseCount)
	}
	if err := scanner.Scan(fields...); err != nil {
		return WorkItem{}, err
	}
	item.Snapshot = json.RawMessage(snapshot)
	item.Seeds = json.RawMessage(seeds)
	item.Budget = json.RawMessage(budget)
	item.Disposition = json.RawMessage(disposition)
	item.Digest = json.RawMessage(digest)
	item.StepMode = stepMode != 0
	item.SoftStop = softStop != 0
	return item, nil
}

func (s *Store) CreateWorkItem(item WorkItem) error {
	_, err := s.db.Exec(
		`INSERT INTO work_items (`+workItemColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ProjectID, item.Goal, item.WorkflowID, item.WorkflowScope,
		jsonText(item.Snapshot), item.State, item.Reason,
		jsonText(item.Seeds), boolToInt(item.StepMode), item.WorktreePath, item.Branch,
		item.BaseBranch, jsonText(item.Budget), item.Source, item.SourceRef,
		item.TriageThreadID, item.OriginThreadID, jsonText(item.Disposition), jsonText(item.Digest),
		item.ParentItemID, item.ParentPhaseID, item.ParentUnitID, item.ParentAttempt, item.CallDepth,
		boolToInt(item.SoftStop), item.CreatedAt, item.StartedAt, item.EndedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create work item %s: %w", item.ID, err)
	}
	return nil
}

func (s *Store) GetWorkItem(id string) (WorkItem, error) {
	item, err := scanWorkItem(s.reader().QueryRow(
		`SELECT `+workItemColumns+` FROM work_items WHERE id = ?`, id,
	), false)
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
		w.id, w.project_id, w.goal, w.state, w.reason, w.worktree_path, w.digest, w.parent_item_id,
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
	if err := s.reader().QueryRow(query, id).Scan(
		&context.Item.ID, &context.Item.ProjectID, &context.Item.Goal,
		&context.Item.State, &context.Item.Reason, &context.Item.WorktreePath,
		&digest, &context.Item.ParentItemID, &context.PhaseID, &output, &context.Check,
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
	item, err := scanWorkItem(s.reader().QueryRow(
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE id = (
		     SELECT item_id FROM work_item_phases
		      WHERE thread_id = ?
		      ORDER BY started_at DESC, attempt DESC
		      LIMIT 1
		 )`,
		threadID,
	), false)
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: get work item for phase thread %s: %w", threadID, err)
	}
	return item, nil
}

// GetWorkItemBySourceRef resolves a run by its provenance key. For agent-source
// runs that key is unique (idx_work_items_agent_source_ref), which makes this
// the durable backstop behind the workflow effect ledger: a run that committed
// before its ledger entry did is still found here, so re-entering the phase
// surfaces the original rather than starting a second one. Absence is the
// ordinary first-run answer and is reported as found=false, not as an error.
func (s *Store) GetWorkItemBySourceRef(source, sourceRef string) (WorkItem, bool, error) {
	item, err := scanWorkItem(s.reader().QueryRow(
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE source = ? AND source_ref = ? AND source_ref <> ''
		 ORDER BY created_at ASC, id ASC LIMIT 1`,
		source, sourceRef,
	), false)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkItem{}, false, nil
	}
	if err != nil {
		return WorkItem{}, false, fmt.Errorf("store: get work item by source ref %s/%s: %w", source, sourceRef, err)
	}
	return item, true, nil
}

// ListWorkItemChildren returns the runs one item called, oldest-first. Children
// are ordinary run records linked by parent id (§3a); the run tree is read
// through this one edge rather than through a denormalized tree column.
func (s *Store) ListWorkItemChildren(parentItemID string) ([]WorkItem, error) {
	if parentItemID == "" {
		return nil, fmt.Errorf("store: list work item children: empty parent id")
	}
	return s.queryWorkItems(
		// The `<> ''` is not redundant: it is what lets SQLite use the partial
		// index, since a bound parameter alone cannot prove the predicate holds.
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE parent_item_id = ? AND parent_item_id <> ''
		 ORDER BY created_at ASC, id ASC`,
		"list work item children", parentItemID,
	)
}

// ListWorkItemCallChildren narrows ListWorkItemChildren to the runs one
// `shape: call` phase *attempt* created. A rerun of a call phase is a new
// attempt with a new child, so the attempt — not the phase — is what identifies
// the invocation a parent is waiting on.
//
// Unit-call children are excluded structurally rather than by the caller
// happening to ask about a call phase: a fan-out attempt's children share this
// (item, phase, attempt) key and are told apart only by `parent_unit_id`.
func (s *Store) ListWorkItemCallChildren(parentItemID, parentPhaseID string, parentAttempt int) ([]WorkItem, error) {
	if parentItemID == "" || parentPhaseID == "" {
		return nil, fmt.Errorf("store: list work item call children: parent id and phase id are required")
	}
	return s.queryWorkItems(
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE parent_item_id = ? AND parent_item_id <> ''
		   AND parent_phase_id = ? AND parent_attempt = ? AND parent_unit_id = ''
		 ORDER BY created_at ASC, id ASC`,
		"list work item call children", parentItemID, parentPhaseID, parentAttempt,
	)
}

// ListWorkItemUnitCallChildren is the fan-out counterpart: the runs one
// call-bound unit of one phase attempt created, oldest-first. A retried unit
// makes a new child on the same key, so more than one row is possible and the
// LAST one is the invocation the unit is waiting on — which makes the order a
// correctness property here rather than a display choice.
//
// That is why the tiebreak is `rowid` and not `id`: two invocations can land in
// the same millisecond, and a random uuid would then decide which child the
// engine believes it is waiting on. `rowid` is insertion order, so the last row
// is the last invocation whatever the clock did.
func (s *Store) ListWorkItemUnitCallChildren(parentItemID, parentPhaseID string, parentAttempt int, parentUnitID string) ([]WorkItem, error) {
	if parentItemID == "" || parentPhaseID == "" || parentUnitID == "" {
		return nil, fmt.Errorf("store: list work item unit call children: parent id, phase id, and unit id are required")
	}
	return s.queryWorkItems(
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE parent_item_id = ? AND parent_item_id <> ''
		   AND parent_phase_id = ? AND parent_attempt = ? AND parent_unit_id = ?
		 ORDER BY created_at ASC, rowid ASC`,
		"list work item unit call children", parentItemID, parentPhaseID, parentAttempt, parentUnitID,
	)
}

func (s *Store) queryWorkItems(query, label string, args ...any) ([]WorkItem, error) {
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", label, err)
	}
	defer rows.Close()
	items := make([]WorkItem, 0)
	for rows.Next() {
		item, err := scanWorkItem(rows, false)
		if err != nil {
			return nil, fmt.Errorf("store: %s: scan: %w", label, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: iterate: %w", label, err)
	}
	return items, nil
}

// ListWorkItems returns matching items oldest-first. An empty ProjectID lists
// every project; an empty States slice includes every state.
func (s *Store) ListWorkItems(filter WorkItemListFilter) ([]WorkItem, error) {
	return s.listWorkItems(filter, workItemColumns, false)
}

// ListWorkItemSummaries returns matching items without loading snapshot,
// seeds, or budget payloads. GetWorkItem is the detail path for those fields.
func (s *Store) ListWorkItemSummaries(filter WorkItemListFilter) ([]WorkItem, error) {
	return s.listWorkItems(filter, workItemSummaryListColumns, true)
}

// GetWorkItemSummary is the single-row form of ListWorkItemSummaries: the same
// projection including phase progress, and the same omission of the snapshot,
// seeds, and budget payloads. `ao run status` polls this, so it must stay a
// one-row read that never drags a run's snapshot along.
func (s *Store) GetWorkItemSummary(id string) (WorkItem, error) {
	item, err := scanWorkItem(s.reader().QueryRow(
		`SELECT `+workItemSummaryListColumns+workItemSummaryProgressColumns+
			` FROM work_items AS w`+workItemSummaryProgressJoin+` WHERE w.id = ?`, id,
	), true)
	if err != nil {
		return WorkItem{}, fmt.Errorf("store: get work item summary %s: %w", id, err)
	}
	return item, nil
}

func (s *Store) listWorkItems(filter WorkItemListFilter, columns string, includeProgress bool) ([]WorkItem, error) {
	table := `work_items`
	prefix := ``
	progressColumns := ``
	if includeProgress {
		table = `work_items AS w` + workItemSummaryProgressJoin
		prefix = `w.`
		progressColumns = workItemSummaryProgressColumns
	}
	query := `SELECT ` + columns + progressColumns + ` FROM ` + table
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 2+len(filter.States))
	if filter.ProjectID != "" {
		conditions = append(conditions, prefix+`project_id = ?`)
		args = append(args, filter.ProjectID)
	}
	if len(filter.States) > 0 {
		conditions = append(conditions, prefix+`state IN (`+strings.TrimRight(strings.Repeat("?,", len(filter.States)), ",")+`)`)
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	if filter.ParentItemID != "" {
		// The second predicate is what makes the partial parent index usable; a
		// bound parameter on its own does not prove `parent_item_id <> ''`.
		conditions = append(conditions, prefix+`parent_item_id = ? AND `+prefix+`parent_item_id <> ''`)
		args = append(args, filter.ParentItemID)
	}
	if filter.UnresolvedOnly {
		conditions = append(conditions, qualifiedUnresolvedWorkItemsPredicate(prefix))
	}
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query += ` ORDER BY ` + prefix + `created_at ASC, ` + prefix + `id ASC`

	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list work items: %w", err)
	}
	defer rows.Close()

	items := make([]WorkItem, 0)
	for rows.Next() {
		item, err := scanWorkItem(rows, includeProgress)
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

// UpdateWorkItemSeeds replaces the run's frozen inputs. The engine's variable
// context reads this column at every phase entry, so the write IS the durable
// record of an amendment — there is no second place for one to live, and a
// reader of the row always sees the values the run will next render.
//
// Which values a run may be given, and when a run may be given any, are the
// engine's rules; the store enforces only that the column stays one JSON
// object.
func (s *Store) UpdateWorkItemSeeds(id string, seeds json.RawMessage) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET seeds = ? WHERE id = ?`, jsonText(seeds), id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item seeds %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item seeds %s", id))
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
	if err := s.reader().QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count work items in states: %w", err)
	}
	return count, nil
}

// CountProjectWorkItemsInStates returns a project-scoped lifecycle count
// without materializing work item rows.
func (s *Store) CountProjectWorkItemsInStates(projectID string, states ...string) (int, error) {
	if len(states) == 0 {
		return 0, nil
	}
	query := `SELECT COUNT(*) FROM work_items WHERE project_id = ? AND state IN (` +
		strings.TrimRight(strings.Repeat("?,", len(states)), ",") + `)`
	args := make([]any, 0, len(states)+1)
	args = append(args, projectID)
	for _, state := range states {
		args = append(args, state)
	}
	var count int
	if err := s.reader().QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count project %s work items in states: %w", projectID, err)
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

// UpdateWorkItemOriginThread binds (or, with an empty threadID, unbinds) the
// chat thread a root run wakes on every resting transition (§5, D17). The
// schema refuses a binding on a called run, so "child runs never bind" cannot
// be defeated by a caller that forgets to check.
//
// Thread lifetime is deliberately independent, like triage_thread_id: a deleted
// thread leaves a stale id that the wake path clears when it next resolves it,
// falling back to the unbound surface rather than losing the wake.
func (s *Store) UpdateWorkItemOriginThread(id, threadID string) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET origin_thread_id = ? WHERE id = ?`, threadID, id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item origin thread %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item origin thread %s", id))
}

// WorkItemWakeSignature reads the signature of the last wake delivered into
// this run's bound thread (migration v52). An empty answer means nothing has
// been delivered yet, or somebody has acted on the run since — both of which
// make the next wake a new one.
//
// It is a read of its own rather than a column on `workItemColumns` because it
// is wake bookkeeping: no listing, overlay, or CLI projection has any use for
// it, and every row those reads carry would pay for it.
func (s *Store) WorkItemWakeSignature(id string) (string, error) {
	var signature string
	err := s.reader().QueryRow(`SELECT wake_signature FROM work_items WHERE id = ?`, id).Scan(&signature)
	if err != nil {
		return "", fmt.Errorf("store: read work item wake signature %s: %w", id, err)
	}
	return signature, nil
}

// UpdateWorkItemWakeSignature records what was just delivered, or clears the
// record with an empty signature when an action on the run makes the next wake
// new whatever it says.
func (s *Store) UpdateWorkItemWakeSignature(id, signature string) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET wake_signature = ? WHERE id = ?`, signature, id,
	)
	if err != nil {
		return fmt.Errorf("store: update work item wake signature %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: update work item wake signature %s", id))
}

// WorkItemPendingGuidance reads the operator guidance waiting for this run's
// next fresh phase entry (migration v53). An empty answer means nothing is
// pending — either none was ever left, or the entry that delivered it cleared
// the slot.
//
// The content is an engine-written JSON array; this layer stores and returns it
// whole, because what an entry IS is the engine's contract with the prompt that
// renders it, not the store's.
//
// It is a read of its own rather than a column on `workItemColumns` for the
// reason `wake_signature` is: it is read by the phase entry that delivers it and
// by the two verbs that report it, and every listed row would otherwise pay for
// text nothing on that surface prints.
func (s *Store) WorkItemPendingGuidance(id string) (json.RawMessage, error) {
	var guidance string
	err := s.reader().QueryRow(`SELECT pending_guidance FROM work_items WHERE id = ?`, id).Scan(&guidance)
	if err != nil {
		return nil, fmt.Errorf("store: read work item pending guidance %s: %w", id, err)
	}
	return json.RawMessage(guidance), nil
}

// SetWorkItemPendingGuidance replaces the slot wholesale: with the appended
// array when a `run guide` adds an entry, and with nothing when the phase entry
// that delivered it clears it. It is an assignment rather than an append because
// the read-modify-write happens on the engine's command goroutine, which is what
// makes "append" a serialized operation rather than a racy one — and because the
// clear needs the same write either way.
func (s *Store) SetWorkItemPendingGuidance(id string, guidance json.RawMessage) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET pending_guidance = ? WHERE id = ?`, jsonText(guidance), id,
	)
	if err != nil {
		return fmt.Errorf("store: set work item pending guidance %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set work item pending guidance %s", id))
}

// SetWorkItemSoftStop arms or disarms the standing request to stop this run
// tree at its next call boundary (D36). It is a plain assignment rather than a
// toggle, so setting the state it is already in is a no-op that still succeeds
// — a human pressing the button twice and an agent re-issuing the verb after a
// dropped response must not disagree about what the row says.
//
// Only a root run's row is ever written; the engine enforces that, because
// "which run may hold this" is a fact about the tree rather than about the
// column.
func (s *Store) SetWorkItemSoftStop(id string, armed bool) error {
	result, err := s.db.Exec(
		`UPDATE work_items SET soft_stop = ? WHERE id = ?`, boolToInt(armed), id,
	)
	if err != nil {
		return fmt.Errorf("store: set work item soft stop %s: %w", id, err)
	}
	return requireRowsAffected(result, fmt.Sprintf("store: set work item soft stop %s", id))
}

// ClearWorkItemOriginThreads unbinds every run pointing at a thread that no
// longer exists. Returns how many rows it cleared so the caller can report the
// convergence rather than silently repairing state.
func (s *Store) ClearWorkItemOriginThreads(threadID string) (int64, error) {
	if threadID == "" {
		return 0, nil
	}
	result, err := s.db.Exec(
		`UPDATE work_items SET origin_thread_id = '' WHERE origin_thread_id = ? AND origin_thread_id <> ''`,
		threadID,
	)
	if err != nil {
		return 0, fmt.Errorf("store: clear work item origin threads for %s: %w", threadID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: clear work item origin threads for %s: %w", threadID, err)
	}
	return affected, nil
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

// DeleteProjectWorkflowRecords removes every workflow row a project owns: its
// runs, their phase/unit/effect records, and its automations (whose cursors
// cascade). One transaction, because a half-deleted run is a run the engine's
// startup rebuild would try to resume.
//
// App.DeleteProject calls this on every project deletion (decision D25): there
// is no foreign key from these tables to `projects` to cascade through, so a
// project deletion that skipped it would leave rows carrying a project id that
// resolves to nothing — unreachable from every project-scoped query and
// invisible in the UI. The app layer owns the disk side, which under D25 is
// cleanup and not destruction: it removes the run worktrees the app created and
// deletes no branch. This call is only the rows. The harness reset invokes it
// directly ahead of the deletion, because it removes the generated workspaces
// wholesale rather than through git.
func (s *Store) DeleteProjectWorkflowRecords(projectID string) error {
	if projectID == "" {
		return fmt.Errorf("store: delete project workflow records: project id is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: delete project %s workflow records: begin: %w", projectID, err)
	}
	defer func() { _ = tx.Rollback() }()
	const owned = `SELECT id FROM work_items WHERE project_id = ?`
	for _, statement := range []string{
		`DELETE FROM work_item_units WHERE item_id IN (` + owned + `)`,
		`DELETE FROM work_item_phases WHERE item_id IN (` + owned + `)`,
		`DELETE FROM work_item_effects WHERE item_id IN (` + owned + `)`,
		`DELETE FROM work_items WHERE project_id = ?`,
		`DELETE FROM automations WHERE project_id = ?`,
	} {
		if _, err := tx.Exec(statement, projectID); err != nil {
			return fmt.Errorf("store: delete project %s workflow records: %w", projectID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete project %s workflow records: commit: %w", projectID, err)
	}
	return nil
}
