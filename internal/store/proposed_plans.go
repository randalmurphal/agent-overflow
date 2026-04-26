package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrProposedPlanAlreadyImplemented = errors.New("store: proposed plan already implemented")

const (
	MaxProposedPlanCommentBodyBytes   = 16 * 1024
	MaxProposedPlanSelectedTextBytes  = 32 * 1024
	MaxProposedPlanCommentLineSpan    = 200
	MaxProposedPlanRevisionCommentIDs = 50
)

// ProposedPlanState is per-plan metadata layered over the existing
// payload-backed timeline item. The plan body remains in payloads.data.
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
	Version   int    `json:"version,omitempty"`
}

type proposedPlanUserMessageMeta struct {
	SourceProposedPlan         *ProposedPlanSourceRef `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan *ProposedPlanSourceRef `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs   []string               `json:"revisionSourceCommentIds,omitempty"`
}

type proposedPlanCommentCounts struct {
	Draft    int `json:"draft"`
	Sent     int `json:"sent"`
	Resolved int `json:"resolved"`
}

type proposedPlanItemMeta struct {
	PlanVersion               int                       `json:"planVersion"`
	PlanRevisionParentItemID  string                    `json:"planRevisionParentItemId,omitempty"`
	PlanImplementedAt         int64                     `json:"planImplementedAt,omitempty"`
	PlanImplementedByThreadID string                    `json:"planImplementedByThreadId,omitempty"`
	PlanImplementedByItemID   string                    `json:"planImplementedByItemId,omitempty"`
	PlanCommentCounts         proposedPlanCommentCounts `json:"planCommentCounts"`
}

// ProposedPlanComment is one inline review note anchored to a plan version.
type ProposedPlanComment struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	PlanItemID   string `json:"planItemId"`
	Status       string `json:"status"`
	StartLine    int    `json:"startLine"`
	EndLine      int    `json:"endLine"`
	SelectedText string `json:"selectedText"`
	Body         string `json:"body"`
	SentAt       int64  `json:"sentAt,omitempty"`
	SentTurnID   string `json:"sentTurnId,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

// ProposedPlanCommentInput carries the editable comment fields from the UI.
type ProposedPlanCommentInput struct {
	PlanItemID string `json:"planItemId"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	Body       string `json:"body"`
}

// ProposedPlanCommentUpdate carries the fields that can be changed after a
// comment exists. Editing a sent comment makes it draft again so it can be sent
// as a fresh revision request.
type ProposedPlanCommentUpdate struct {
	Body string `json:"body"`
}

func normalizeCommentBody(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("comment body is required")
	}
	if len(trimmed) > MaxProposedPlanCommentBodyBytes {
		return "", fmt.Errorf("comment body is too large")
	}
	return trimmed, nil
}

func validateCommentLines(startLine, endLine int) error {
	if startLine <= 0 || endLine <= 0 {
		return fmt.Errorf("comment line numbers must be positive")
	}
	if endLine < startLine {
		return fmt.Errorf("comment end line must be greater than or equal to start line")
	}
	if endLine-startLine+1 > MaxProposedPlanCommentLineSpan {
		return fmt.Errorf("comment line range is too large")
	}
	return nil
}

func ValidateProposedPlanCommentRangeForApp(startLine, endLine int) error {
	return validateCommentLines(startLine, endLine)
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

	if state, found, err := getProposedPlanStateTx(tx, threadID, itemID); err != nil {
		return ProposedPlanState{}, err
	} else if found {
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
	if err := tx.Commit(); err != nil {
		return ProposedPlanState{}, fmt.Errorf("store: commit proposed plan state %s: %w", itemID, err)
	}
	return state, nil
}

func getProposedPlanStateTx(tx *sql.Tx, threadID, itemID string) (ProposedPlanState, bool, error) {
	row := tx.QueryRow(
		`SELECT item_id, thread_id, revision_parent_item_id, version,
		        implemented_at, implemented_by_thread_id, implemented_by_item_id,
		        created_at, updated_at
		   FROM proposed_plans
		  WHERE thread_id = ? AND item_id = ?`,
		threadID, itemID,
	)
	state, err := scanProposedPlanState(row)
	if err == sql.ErrNoRows {
		return ProposedPlanState{}, false, nil
	}
	if err != nil {
		return ProposedPlanState{}, false, fmt.Errorf("store: get proposed plan state %s/%s: %w", threadID, itemID, err)
	}
	return state, true, nil
}

func (s *Store) GetProposedPlanState(threadID, itemID string) (ProposedPlanState, bool, error) {
	row := s.db.QueryRow(
		`SELECT item_id, thread_id, revision_parent_item_id, version,
		        implemented_at, implemented_by_thread_id, implemented_by_item_id,
		        created_at, updated_at
		   FROM proposed_plans
		  WHERE thread_id = ? AND item_id = ?`,
		threadID, itemID,
	)
	state, err := scanProposedPlanState(row)
	if err == sql.ErrNoRows {
		return ProposedPlanState{}, false, nil
	}
	if err != nil {
		return ProposedPlanState{}, false, fmt.Errorf("store: get proposed plan state %s/%s: %w", threadID, itemID, err)
	}
	return state, true, nil
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

func (s *Store) MarkProposedPlanImplemented(threadID, itemID, implementationThreadID, implementationItemID string, now int64) error {
	if itemID == "" {
		return nil
	}
	res, err := s.db.Exec(
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
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		state, found, getErr := s.GetProposedPlanState(threadID, itemID)
		if getErr != nil {
			return getErr
		}
		if found && state.ImplementedAt > 0 {
			if state.ImplementedByThreadID == implementationThreadID && state.ImplementedByItemID == implementationItemID {
				return nil
			}
			return fmt.Errorf("%w: %s on thread %s", ErrProposedPlanAlreadyImplemented, itemID, threadID)
		}
		return fmt.Errorf("store: proposed plan %s not found on thread %s", itemID, threadID)
	}
	return nil
}

func (s *Store) ReconcileProposedPlanStateFromAcceptedTurns(now int64) error {
	rows, err := s.db.Query(
		`SELECT items.id, items.thread_id, items.meta, turns.turn_id, turns.started_at
		   FROM items
		   JOIN turns
		     ON turns.thread_id = items.thread_id
		    AND turns.turn_index = items.turn_index
		  WHERE items.kind = 'user_text'
		    AND (
		      items.meta LIKE '%"sourceProposedPlan"%'
		      OR items.meta LIKE '%"revisionSourceCommentIds"%'
		    )`,
	)
	if err != nil {
		return fmt.Errorf("store: reconcile proposed plan state: %w", err)
	}
	defer rows.Close()

	type acceptedTurn struct {
		userItemID string
		threadID   string
		turnID     string
		startedAt  int64
		meta       proposedPlanUserMessageMeta
	}
	var turns []acceptedTurn
	for rows.Next() {
		var userItemID string
		var implementationThreadID string
		var metaJSON string
		var turnID string
		var startedAt int64
		if err := rows.Scan(&userItemID, &implementationThreadID, &metaJSON, &turnID, &startedAt); err != nil {
			return fmt.Errorf("store: scan proposed plan accepted turn source: %w", err)
		}
		meta, ok := parseProposedPlanUserMessageMeta(metaJSON)
		if !ok {
			continue
		}
		turns = append(turns, acceptedTurn{
			userItemID: userItemID,
			threadID:   implementationThreadID,
			turnID:     turnID,
			startedAt:  startedAt,
			meta:       meta,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate proposed plan accepted turn sources: %w", err)
	}

	for _, accepted := range turns {
		acceptedAt := accepted.startedAt
		if acceptedAt == 0 {
			acceptedAt = now
		}
		if sourceRef := accepted.meta.SourceProposedPlan; sourceRef != nil {
			source := normalizeProposedPlanSourceRef(*sourceRef, accepted.threadID)
			if err := s.markAcceptedProposedPlanImplemented(accepted.threadID, accepted.userItemID, source, acceptedAt); err != nil {
				return err
			}
		}
		if sourceRef := accepted.meta.RevisionSourceProposedPlan; sourceRef != nil && len(accepted.meta.RevisionSourceCommentIDs) > 0 {
			source := normalizeProposedPlanSourceRef(*sourceRef, accepted.threadID)
			ids := limitStringIDs(uniqueNonEmptyStrings(accepted.meta.RevisionSourceCommentIDs), MaxProposedPlanRevisionCommentIDs)
			if err := s.markAcceptedProposedPlanCommentsSent(accepted.threadID, source, ids, acceptedAt, accepted.turnID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReconcileProposedPlanImplementationsFromAcceptedTurns is kept as a narrow
// compatibility wrapper for older call sites. New code should call
// ReconcileProposedPlanStateFromAcceptedTurns so revision comments recover too.
func (s *Store) ReconcileProposedPlanImplementationsFromAcceptedTurns(now int64) error {
	return s.ReconcileProposedPlanStateFromAcceptedTurns(now)
}

func normalizeProposedPlanSourceRef(source ProposedPlanSourceRef, fallbackThreadID string) ProposedPlanSourceRef {
	source.ThreadID = strings.TrimSpace(source.ThreadID)
	if source.ThreadID == "" {
		source.ThreadID = fallbackThreadID
	}
	source.ItemID = strings.TrimSpace(source.ItemID)
	return source
}

func (s *Store) markAcceptedProposedPlanImplemented(implementationThreadID, implementationItemID string, source ProposedPlanSourceRef, implementedAt int64) error {
	if source.ItemID == "" {
		return nil
	}
	if source.ThreadID != implementationThreadID {
		return nil
	}
	item, found, err := s.GetThreadItem(source.ThreadID, source.ItemID)
	if err != nil {
		return fmt.Errorf("store: validate accepted proposed plan %s/%s: %w", source.ThreadID, source.ItemID, err)
	}
	if !found || item.Role != "assistant" || item.PayloadKind != "proposed_plan" {
		return nil
	}
	err = s.MarkProposedPlanImplemented(source.ThreadID, source.ItemID, implementationThreadID, implementationItemID, implementedAt)
	if err == nil || errors.Is(err, ErrProposedPlanAlreadyImplemented) {
		return nil
	}
	return err
}

func (s *Store) markAcceptedProposedPlanCommentsSent(threadID string, source ProposedPlanSourceRef, commentIDs []string, sentAt int64, sentTurnID string) error {
	if source.ThreadID != threadID || source.ItemID == "" || len(commentIDs) == 0 {
		return nil
	}
	item, found, err := s.GetThreadItem(source.ThreadID, source.ItemID)
	if err != nil {
		return fmt.Errorf("store: validate accepted revision source plan %s/%s: %w", source.ThreadID, source.ItemID, err)
	}
	if !found || item.Role != "assistant" || item.PayloadKind != "proposed_plan" {
		return nil
	}
	return s.MarkProposedPlanCommentsSent(threadID, source.ItemID, commentIDs, sentAt, sentTurnID)
}

func (s *Store) RevisionSourceProposedPlanForTurn(threadID string, turnIndex int) (ProposedPlanSourceRef, bool, error) {
	return s.proposedPlanSourceForTurn(threadID, turnIndex, true)
}

func (s *Store) RevisionSourceCommentIDsForTurn(threadID string, turnIndex int) ([]string, bool, error) {
	item, found, err := s.FindTurnItem(threadID, turnIndex, "user_text")
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	meta, ok := parseProposedPlanUserMessageMeta(item.Meta)
	if !ok || len(meta.RevisionSourceCommentIDs) == 0 {
		return nil, false, nil
	}
	ids := uniqueNonEmptyStrings(meta.RevisionSourceCommentIDs)
	if len(ids) == 0 {
		return nil, false, nil
	}
	return ids, true, nil
}

func (s *Store) SourceProposedPlanForTurn(threadID string, turnIndex int) (ProposedPlanSourceRef, bool, error) {
	return s.proposedPlanSourceForTurn(threadID, turnIndex, false)
}

func (s *Store) proposedPlanSourceForTurn(threadID string, turnIndex int, revision bool) (ProposedPlanSourceRef, bool, error) {
	item, found, err := s.FindTurnItem(threadID, turnIndex, "user_text")
	if err != nil {
		return ProposedPlanSourceRef{}, false, err
	}
	if !found {
		return ProposedPlanSourceRef{}, false, nil
	}
	meta, ok := parseProposedPlanUserMessageMeta(item.Meta)
	if !ok {
		return ProposedPlanSourceRef{}, false, nil
	}
	sourceRef := meta.SourceProposedPlan
	if revision {
		sourceRef = meta.RevisionSourceProposedPlan
	}
	if sourceRef == nil || strings.TrimSpace(sourceRef.ItemID) == "" {
		return ProposedPlanSourceRef{}, false, nil
	}
	source := *sourceRef
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

func (s *Store) CreateProposedPlanComment(comment ProposedPlanComment) (ProposedPlanComment, error) {
	if comment.ID == "" || comment.ThreadID == "" || comment.PlanItemID == "" {
		return ProposedPlanComment{}, fmt.Errorf("store: proposed plan comment id, thread id, and plan item id are required")
	}
	if err := validateCommentLines(comment.StartLine, comment.EndLine); err != nil {
		return ProposedPlanComment{}, err
	}
	body, err := normalizeCommentBody(comment.Body)
	if err != nil {
		return ProposedPlanComment{}, err
	}
	comment.Body = body
	comment.Status = "draft"
	if comment.CreatedAt == 0 {
		comment.CreatedAt = nowMillis()
	}
	if comment.UpdatedAt == 0 {
		comment.UpdatedAt = comment.CreatedAt
	}

	_, err = s.db.Exec(
		`INSERT INTO proposed_plan_comments (
			id, thread_id, plan_item_id, status, start_line, end_line,
			selected_text, body, created_at, updated_at
		) VALUES (?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?)`,
		comment.ID, comment.ThreadID, comment.PlanItemID, comment.StartLine, comment.EndLine,
		comment.SelectedText, comment.Body, comment.CreatedAt, comment.UpdatedAt,
	)
	if err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: create proposed plan comment %s: %w", comment.ID, err)
	}
	return s.GetProposedPlanComment(comment.ThreadID, comment.ID)
}

func (s *Store) UpdateProposedPlanComment(threadID, commentID string, update ProposedPlanCommentUpdate, now int64) (ProposedPlanComment, error) {
	body, err := normalizeCommentBody(update.Body)
	if err != nil {
		return ProposedPlanComment{}, err
	}
	res, err := s.db.Exec(
		`UPDATE proposed_plan_comments
		    SET body = ?, status = 'draft', sent_at = 0, sent_turn_id = '', updated_at = ?
		  WHERE thread_id = ? AND id = ? AND status <> 'resolved'`,
		body, now, threadID, commentID,
	)
	if err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: update proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ProposedPlanComment{}, fmt.Errorf("store: proposed plan comment %s not found or resolved", commentID)
	}
	return s.GetProposedPlanComment(threadID, commentID)
}

func (s *Store) DeleteOrResolveProposedPlanComment(threadID, commentID string, now int64) error {
	comment, err := s.GetProposedPlanComment(threadID, commentID)
	if err != nil {
		return err
	}
	if comment.Status == "draft" {
		if _, err := s.db.Exec(`DELETE FROM proposed_plan_comments WHERE thread_id = ? AND id = ?`, threadID, commentID); err != nil {
			return fmt.Errorf("store: delete draft proposed plan comment %s/%s: %w", threadID, commentID, err)
		}
		return nil
	}
	_, err = s.db.Exec(
		`UPDATE proposed_plan_comments
		    SET status = 'resolved', updated_at = ?
		  WHERE thread_id = ? AND id = ?`,
		now, threadID, commentID,
	)
	if err != nil {
		return fmt.Errorf("store: resolve proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	return nil
}

func (s *Store) MarkProposedPlanCommentsSent(threadID, planItemID string, commentIDs []string, sentAt int64, sentTurnID string) error {
	ids := uniqueNonEmptyStrings(commentIDs)
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > MaxProposedPlanRevisionCommentIDs {
		return fmt.Errorf("store: too many proposed plan comments selected")
	}
	query := `UPDATE proposed_plan_comments
	             SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?
	           WHERE thread_id = ? AND plan_item_id = ? AND status = 'draft' AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 5+len(ids))
	args = append(args, sentAt, sentTurnID, sentAt, threadID, planItemID)
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: mark proposed plan comments sent for %s: %w", threadID, err)
	}
	if n, err := res.RowsAffected(); err == nil && int(n) != len(ids) {
		if ok, checkErr := s.proposedPlanCommentsAlreadySent(threadID, planItemID, ids, sentTurnID); checkErr != nil {
			return checkErr
		} else if ok {
			return nil
		}
		return fmt.Errorf("store: marked %d proposed plan comments sent for %s, want %d draft comments", n, threadID, len(ids))
	}
	return nil
}

func (s *Store) proposedPlanCommentsAlreadySent(threadID, planItemID string, ids []string, sentTurnID string) (bool, error) {
	query := `SELECT COUNT(*)
	            FROM proposed_plan_comments
	           WHERE thread_id = ?
	             AND plan_item_id = ?
	             AND status = 'sent'
	             AND sent_turn_id = ?
	             AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 3+len(ids))
	args = append(args, threadID, planItemID, sentTurnID)
	for _, id := range ids {
		args = append(args, id)
	}
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check proposed plan comments sent for %s: %w", threadID, err)
	}
	return count == len(ids), nil
}

func (s *Store) GetProposedPlanComment(threadID, commentID string) (ProposedPlanComment, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, plan_item_id, status, start_line, end_line,
		        selected_text, body, sent_at, sent_turn_id, created_at, updated_at
		   FROM proposed_plan_comments
		  WHERE thread_id = ? AND id = ?`,
		threadID, commentID,
	)
	comment, err := scanProposedPlanComment(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return ProposedPlanComment{}, fmt.Errorf("store: proposed plan comment %s not found on thread %s", commentID, threadID)
		}
		return ProposedPlanComment{}, fmt.Errorf("store: get proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	return comment, nil
}

func (s *Store) ListProposedPlanComments(threadID, planItemID string) ([]ProposedPlanComment, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, plan_item_id, status, start_line, end_line,
		        selected_text, body, sent_at, sent_turn_id, created_at, updated_at
		   FROM proposed_plan_comments
		  WHERE thread_id = ? AND plan_item_id = ?
		  ORDER BY start_line ASC, end_line ASC, created_at ASC`,
		threadID, planItemID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list proposed plan comments %s/%s: %w", threadID, planItemID, err)
	}
	defer rows.Close()

	out := []ProposedPlanComment{}
	for rows.Next() {
		comment, err := scanProposedPlanComment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proposed plan comment: %w", err)
		}
		out = append(out, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate proposed plan comments %s/%s: %w", threadID, planItemID, err)
	}
	return out, nil
}

func (s *Store) ListDraftProposedPlanCommentsByID(threadID, planItemID string, ids []string) ([]ProposedPlanComment, error) {
	wanted := uniqueNonEmptyStrings(ids)
	if len(wanted) == 0 {
		return []ProposedPlanComment{}, nil
	}
	if len(wanted) > MaxProposedPlanRevisionCommentIDs {
		return nil, fmt.Errorf("store: too many proposed plan comments selected")
	}
	query := `SELECT id, thread_id, plan_item_id, status, start_line, end_line,
	                 selected_text, body, sent_at, sent_turn_id, created_at, updated_at
	            FROM proposed_plan_comments
	           WHERE thread_id = ?
	             AND plan_item_id = ?
	             AND status = 'draft'
	             AND id IN (` + placeholders(len(wanted)) + `)
	           ORDER BY start_line ASC, end_line ASC, created_at ASC`
	args := make([]any, 0, 2+len(wanted))
	args = append(args, threadID, planItemID)
	for _, id := range wanted {
		args = append(args, id)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list selected draft proposed plan comments %s/%s: %w", threadID, planItemID, err)
	}
	defer rows.Close()

	out := make([]ProposedPlanComment, 0, len(wanted))
	for rows.Next() {
		comment, err := scanProposedPlanComment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan selected draft proposed plan comment: %w", err)
		}
		out = append(out, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate selected draft proposed plan comments %s/%s: %w", threadID, planItemID, err)
	}
	return out, nil
}

func scanProposedPlanComment(scanner interface{ Scan(...any) error }) (ProposedPlanComment, error) {
	var comment ProposedPlanComment
	if err := scanner.Scan(
		&comment.ID, &comment.ThreadID, &comment.PlanItemID, &comment.Status,
		&comment.StartLine, &comment.EndLine, &comment.SelectedText, &comment.Body,
		&comment.SentAt, &comment.SentTurnID, &comment.CreatedAt, &comment.UpdatedAt,
	); err != nil {
		return ProposedPlanComment{}, err
	}
	return comment, nil
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

func (s *Store) decorateProposedPlanItems(threadID string, items []Item) ([]Item, error) {
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
	states, err := s.proposedPlanStatesByItemID(threadID, itemIDs)
	if err != nil {
		return nil, err
	}
	counts, err := s.proposedPlanCommentCountsByItemID(threadID, itemIDs)
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

func (s *Store) proposedPlanStatesByItemID(threadID string, itemIDs []string) (map[string]ProposedPlanState, error) {
	query := `SELECT item_id, thread_id, revision_parent_item_id, version,
	                 implemented_at, implemented_by_thread_id, implemented_by_item_id,
	                 created_at, updated_at
	            FROM proposed_plans
	           WHERE thread_id = ? AND item_id IN (` + placeholders(len(itemIDs)) + `)`
	args := make([]any, 0, 1+len(itemIDs))
	args = append(args, threadID)
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(query, args...)
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

func (s *Store) proposedPlanCommentCountsByItemID(threadID string, itemIDs []string) (map[string]proposedPlanCommentCounts, error) {
	query := `SELECT plan_item_id, status, COUNT(*)
	            FROM proposed_plan_comments
	           WHERE thread_id = ? AND plan_item_id IN (` + placeholders(len(itemIDs)) + `)
	           GROUP BY plan_item_id, status`
	args := make([]any, 0, 1+len(itemIDs))
	args = append(args, threadID)
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list proposed plan comment counts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]proposedPlanCommentCounts, len(itemIDs))
	for rows.Next() {
		var itemID string
		var status string
		var count int
		if err := rows.Scan(&itemID, &status, &count); err != nil {
			return nil, fmt.Errorf("store: scan proposed plan comment count: %w", err)
		}
		counts := out[itemID]
		switch status {
		case "draft":
			counts.Draft = count
		case "sent":
			counts.Sent = count
		case "resolved":
			counts.Resolved = count
		}
		out[itemID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate proposed plan comment counts: %w", err)
	}
	return out, nil
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func UniqueNonEmptyStringsForApp(values []string) []string {
	return uniqueNonEmptyStrings(values)
}

func limitStringIDs(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return values[:max]
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}
