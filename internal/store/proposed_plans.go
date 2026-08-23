package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrProposedPlanAlreadyImplemented = errors.New("store: proposed plan already implemented")

// ProposedPlanState is per-plan metadata layered over the existing
// payload-backed timeline item. The plan body remains in payloads.data.
//
// Every mutator in this file and in proposed_plan_comments.go advances
// the owning thread's history_rev, because decorateProposedPlanItems
// projects these rows onto `Item.Meta` at window-read time: a plan whose
// version, implemented stamp, or comment counts changed IS a changed
// window read, and a client comparing stamps would otherwise be told its
// stale copy is fresh. See internal/store/AGENTS.md
// "History invalidation contract" and docs/specs/thread-replica-sync.md
// §3.2. The thread id each mutator already carries is the enforcement —
// both tables reference `threads(id)` directly, so it is always the
// PLAN's thread, never the implementing one.
type ProposedPlanState struct {
	ItemID                string `json:"itemId"`
	ThreadID              string `json:"threadId"`
	RevisionParentItemID  string `json:"revisionParentItemId,omitempty"`
	Version               int    `json:"version"`
	ImplementedAt         int64  `json:"implementedAt,omitempty"`
	ImplementedByThreadID string `json:"implementedByThreadId,omitempty"`
	ImplementedByItemID   string `json:"implementedByItemId,omitempty"`
	CreatedAt             int64  `json:"createdAt"`
	UpdatedAt             int64  `json:"updatedAt"`
}

type ProposedPlanSourceRef struct {
	ThreadID  string `json:"threadId,omitempty"`
	ItemID    string `json:"itemId"`
	PayloadID string `json:"payloadId,omitempty"`
	Title     string `json:"title,omitempty"`
}

type proposedPlanUserMessageMeta struct {
	SourceProposedPlan           *ProposedPlanSourceRef `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *ProposedPlanSourceRef `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string               `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *DiffReviewSourceRef   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string               `json:"revisionSourceDiffCommentIds,omitempty"`
}

type proposedPlanCommentCounts struct {
	Draft    int `json:"draft"`
	Sent     int `json:"sent"`
	Resolved int `json:"resolved"`
}

// EnsureProposedPlanState creates a state row for a newly-persisted proposed
// plan item. Existing rows are left untouched, making provider replay/upsert
// idempotent.
func (s *Store) EnsureProposedPlanState(threadID, itemID string, now int64) (ProposedPlanState, error) {
	return s.EnsureProposedPlanStateWithParent(threadID, itemID, "", now)
}

func (s *Store) EnsureProposedPlanStateWithParent(threadID, itemID, explicitParentItemID string, now int64) (ProposedPlanState, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return ProposedPlanState{}, fmt.Errorf("store: begin ensure proposed plan tx: %w", err)
	}
	defer tx.Rollback()

	if state, found, err := getProposedPlanStateQ(tx, threadID, itemID); err != nil {
		return ProposedPlanState{}, err
	} else if found {
		// Nothing was written, so nothing to invalidate: the idempotent
		// replay path must not advance the contract.
		return state, tx.Commit()
	}

	var version int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(version), 0) + 1 FROM proposed_plans WHERE thread_id = ?`,
		threadID,
	).Scan(&version); err != nil {
		return ProposedPlanState{}, fmt.Errorf("store: next proposed plan version for %s: %w", threadID, err)
	}

	parent := strings.TrimSpace(explicitParentItemID)
	if parent != "" && parent != itemID {
		var exists int
		if err := tx.QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM proposed_plans WHERE thread_id = ? AND item_id = ?
			)`,
			threadID, parent,
		).Scan(&exists); err != nil {
			return ProposedPlanState{}, fmt.Errorf("store: validate explicit proposed plan parent for %s: %w", threadID, err)
		}
		if exists == 0 {
			parent = ""
		}
	} else {
		parent = ""
	}
	state := ProposedPlanState{
		ItemID:               itemID,
		ThreadID:             threadID,
		RevisionParentItemID: parent,
		Version:              version,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if _, err := tx.Exec(
		`INSERT INTO proposed_plans (
			item_id, thread_id, revision_parent_item_id, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		state.ItemID, state.ThreadID, state.RevisionParentItemID, state.Version, state.CreatedAt, state.UpdatedAt,
	); err != nil {
		return ProposedPlanState{}, fmt.Errorf("store: insert proposed plan state %s: %w", itemID, err)
	}
	if err := bumpHistoryRevTx(tx, threadID, fmt.Sprintf("store: insert proposed plan state %s", itemID)); err != nil {
		return ProposedPlanState{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProposedPlanState{}, fmt.Errorf("store: commit proposed plan state %s: %w", itemID, err)
	}
	return state, nil
}

const proposedPlanStateColumns = `item_id, thread_id, revision_parent_item_id, version,
        implemented_at, implemented_by_thread_id, implemented_by_item_id,
        created_at, updated_at`

func getProposedPlanStateQ(q sqlQueryer, threadID, itemID string) (ProposedPlanState, bool, error) {
	row := q.QueryRow(
		`SELECT `+proposedPlanStateColumns+`
		   FROM proposed_plans
		  WHERE thread_id = ? AND item_id = ?`,
		threadID, itemID,
	)
	state, err := scanProposedPlanState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ProposedPlanState{}, false, nil
	}
	if err != nil {
		return ProposedPlanState{}, false, fmt.Errorf("store: get proposed plan state %s/%s: %w", threadID, itemID, err)
	}
	return state, true, nil
}

func (s *Store) GetProposedPlanState(threadID, itemID string) (ProposedPlanState, bool, error) {
	return getProposedPlanStateQ(s.reader(), threadID, itemID)
}

func scanProposedPlanState(scanner interface{ Scan(...any) error }) (ProposedPlanState, error) {
	var state ProposedPlanState
	if err := scanner.Scan(
		&state.ItemID, &state.ThreadID, &state.RevisionParentItemID, &state.Version,
		&state.ImplementedAt, &state.ImplementedByThreadID, &state.ImplementedByItemID,
		&state.CreatedAt, &state.UpdatedAt,
	); err != nil {
		return ProposedPlanState{}, err
	}
	return state, nil
}

// MarkProposedPlanImplemented records that a plan was turned into work.
//
// threadID is the PLAN's thread; implementationThreadID is where the work
// started, and the two differ whenever a plan is implemented from another
// thread (app_send.go passes the source plan's own thread id for exactly
// that reason). Only the plan's thread takes the history bump: the
// implemented stamp is projected onto the PLAN item's meta, and nothing
// on the implementing thread's window changes.
//
// Idempotent for a repeat of the same implementation, which writes
// nothing and therefore bumps nothing.
func (s *Store) MarkProposedPlanImplemented(threadID, itemID, implementationThreadID, implementationItemID string, now int64) error {
	if itemID == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin mark proposed plan implemented %s/%s: %w", threadID, itemID, err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE proposed_plans
		    SET implemented_at = ?,
		        implemented_by_thread_id = ?,
		        implemented_by_item_id = ?,
		        updated_at = ?
		  WHERE thread_id = ? AND item_id = ? AND implemented_at = 0`,
		now, implementationThreadID, implementationItemID, now, threadID, itemID,
	)
	if err != nil {
		return fmt.Errorf("store: mark proposed plan implemented %s/%s: %w", threadID, itemID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark proposed plan implemented %s/%s: rows affected: %w", threadID, itemID, err)
	}
	if n == 0 {
		// Read the losing row inside the same transaction so the
		// already-implemented verdict describes the state the UPDATE
		// actually raced against.
		state, found, getErr := getProposedPlanStateQ(tx, threadID, itemID)
		if getErr != nil {
			return getErr
		}
		if found && state.ImplementedAt > 0 {
			if state.ImplementedByThreadID == implementationThreadID && state.ImplementedByItemID == implementationItemID {
				return tx.Commit()
			}
			return fmt.Errorf("%w: %s on thread %s", ErrProposedPlanAlreadyImplemented, itemID, threadID)
		}
		return fmt.Errorf("store: proposed plan %s not found on thread %s", itemID, threadID)
	}
	if err := bumpHistoryRevTx(tx, threadID, fmt.Sprintf("store: mark proposed plan implemented %s", itemID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit mark proposed plan implemented %s/%s: %w", threadID, itemID, err)
	}
	return nil
}

// RevisionSourceProposedPlanForTurn loads the revision source plan
// reference stamped onto a turn's user_text meta. Used by triage's
// payload writer to link a newly-emitted plan to its parent (the plan
// the user just sent revision comments against), so the new plan can
// claim the right RevisionParentItemID.
func (s *Store) RevisionSourceProposedPlanForTurn(threadID string, turnIndex int) (ProposedPlanSourceRef, bool, error) {
	item, found, err := s.FindTurnItem(threadID, turnIndex, "user_text")
	if err != nil {
		return ProposedPlanSourceRef{}, false, err
	}
	if !found {
		return ProposedPlanSourceRef{}, false, nil
	}
	meta, ok := parseProposedPlanUserMessageMeta(item.Meta)
	if !ok || meta.RevisionSourceProposedPlan == nil {
		return ProposedPlanSourceRef{}, false, nil
	}
	source := *meta.RevisionSourceProposedPlan
	if strings.TrimSpace(source.ItemID) == "" {
		return ProposedPlanSourceRef{}, false, nil
	}
	if strings.TrimSpace(source.ThreadID) == "" {
		source.ThreadID = threadID
	}
	return source, true, nil
}

func parseProposedPlanUserMessageMeta(metaJSON string) (proposedPlanUserMessageMeta, bool) {
	if strings.TrimSpace(metaJSON) == "" {
		return proposedPlanUserMessageMeta{}, false
	}
	var meta proposedPlanUserMessageMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return proposedPlanUserMessageMeta{}, false
	}
	return meta, true
}

func mergePlanItemMeta(itemMeta string, state ProposedPlanState, counts proposedPlanCommentCounts) string {
	merged := map[string]any{}
	if strings.TrimSpace(itemMeta) != "" && itemMeta != "{}" {
		_ = json.Unmarshal([]byte(itemMeta), &merged)
	}
	merged["planVersion"] = state.Version
	if state.RevisionParentItemID != "" {
		merged["planRevisionParentItemId"] = state.RevisionParentItemID
	}
	if state.ImplementedAt > 0 {
		merged["planImplementedAt"] = state.ImplementedAt
		merged["planImplementedByThreadId"] = state.ImplementedByThreadID
		merged["planImplementedByItemId"] = state.ImplementedByItemID
	}
	merged["planCommentCounts"] = counts
	data, err := json.Marshal(merged)
	if err != nil {
		return itemMeta
	}
	return string(data)
}

// decorateProposedPlanItems rewrites plan items' Meta from the
// `proposed_plans` / `proposed_plan_comments` rows. It runs inside
// decoratePagedItems on EVERY window read, SyncThreadWindow included,
// which is why both those tables' writers advance history_rev: this
// projection is the reason their state is window-visible at all.
func (s *Store) decorateProposedPlanItems(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	if len(items) == 0 {
		return items, nil
	}
	itemIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Role == "assistant" && item.PayloadKind == "proposed_plan" && strings.TrimSpace(item.ID) != "" {
			itemIDs = append(itemIDs, item.ID)
		}
	}
	if len(itemIDs) == 0 {
		return items, nil
	}
	states, err := s.proposedPlanStatesByItemID(q, threadID, itemIDs)
	if err != nil {
		return nil, err
	}
	counts, err := s.proposedPlanCommentCountsByItemID(q, threadID, itemIDs)
	if err != nil {
		return nil, err
	}
	for i := range items {
		state, ok := states[items[i].ID]
		if !ok {
			continue
		}
		items[i].Meta = mergePlanItemMeta(items[i].Meta, state, counts[items[i].ID])
	}
	return items, nil
}

func (s *Store) proposedPlanStatesByItemID(q sqlQueryer, threadID string, itemIDs []string) (map[string]ProposedPlanState, error) {
	query := `SELECT ` + proposedPlanStateColumns + `
	            FROM proposed_plans
	           WHERE thread_id = ? AND item_id IN (` + placeholders(len(itemIDs)) + `)`
	args := make([]any, 0, 1+len(itemIDs))
	args = append(args, threadID)
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list proposed plan states: %w", err)
	}
	defer rows.Close()

	out := make(map[string]ProposedPlanState, len(itemIDs))
	for rows.Next() {
		state, err := scanProposedPlanState(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proposed plan state: %w", err)
		}
		out[state.ItemID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate proposed plan states: %w", err)
	}
	return out, nil
}
