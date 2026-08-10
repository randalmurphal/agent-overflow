package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	MaxProposedPlanCommentBodyBytes   = 16 * 1024
	MaxProposedPlanSelectedTextBytes  = 32 * 1024
	MaxProposedPlanCommentLineSpan    = 200
	MaxProposedPlanRevisionCommentIDs = 50
)

// ProposedPlanComment is one inline review note anchored to a proposed plan.
//
// `thread_id` is a FK to `threads(id)` and the table's compound FK
// `(thread_id, plan_item_id)` points at `proposed_plans(thread_id,
// item_id)`, so a comment's thread is structurally the PLAN's thread —
// which is what makes it the right target for the history bump every
// mutator here performs. See ProposedPlanState's doc for why they bump at
// all.
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

const proposedPlanCommentColumns = `id, thread_id, plan_item_id, status, start_line, end_line,
        selected_text, body, sent_at, sent_turn_id, created_at, updated_at`

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

	tx, err := s.db.Begin()
	if err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: begin create proposed plan comment %s: %w", comment.ID, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO proposed_plan_comments (
			id, thread_id, plan_item_id, status, start_line, end_line,
			selected_text, body, created_at, updated_at
		) VALUES (?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?)`,
		comment.ID, comment.ThreadID, comment.PlanItemID, comment.StartLine, comment.EndLine,
		comment.SelectedText, comment.Body, comment.CreatedAt, comment.UpdatedAt,
	); err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: create proposed plan comment %s: %w", comment.ID, err)
	}
	if err := bumpHistoryRevTx(tx, comment.ThreadID, fmt.Sprintf("store: create proposed plan comment %s", comment.ID)); err != nil {
		return ProposedPlanComment{}, err
	}
	stored, err := getProposedPlanCommentQ(tx, comment.ThreadID, comment.ID)
	if err != nil {
		return ProposedPlanComment{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: commit create proposed plan comment %s: %w", comment.ID, err)
	}
	return stored, nil
}

func (s *Store) UpdateProposedPlanComment(threadID, commentID string, update ProposedPlanCommentUpdate, now int64) (ProposedPlanComment, error) {
	body, err := normalizeCommentBody(update.Body)
	if err != nil {
		return ProposedPlanComment{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: begin update proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE proposed_plan_comments
		    SET body = ?, status = 'draft', sent_at = 0, sent_turn_id = '', updated_at = ?
		  WHERE thread_id = ? AND id = ? AND status <> 'resolved'`,
		body, now, threadID, commentID,
	)
	if err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: update proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: update proposed plan comment %s/%s: rows affected: %w", threadID, commentID, err)
	} else if n == 0 {
		return ProposedPlanComment{}, fmt.Errorf("store: proposed plan comment %s not found or resolved", commentID)
	}
	if err := bumpHistoryRevTx(tx, threadID, fmt.Sprintf("store: update proposed plan comment %s", commentID)); err != nil {
		return ProposedPlanComment{}, err
	}
	stored, err := getProposedPlanCommentQ(tx, threadID, commentID)
	if err != nil {
		return ProposedPlanComment{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProposedPlanComment{}, fmt.Errorf("store: commit update proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	return stored, nil
}

// DeleteOrResolveProposedPlanComment drops a draft outright and resolves
// anything already sent. The status read happens inside the write
// transaction so a concurrent send cannot make the branch decision stale
// between the read and the write it chose.
func (s *Store) DeleteOrResolveProposedPlanComment(threadID, commentID string, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin delete-or-resolve proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	defer tx.Rollback()

	comment, err := getProposedPlanCommentQ(tx, threadID, commentID)
	if err != nil {
		return err
	}
	if comment.Status == "draft" {
		if _, err := tx.Exec(`DELETE FROM proposed_plan_comments WHERE thread_id = ? AND id = ?`, threadID, commentID); err != nil {
			return fmt.Errorf("store: delete draft proposed plan comment %s/%s: %w", threadID, commentID, err)
		}
	} else if _, err := tx.Exec(
		`UPDATE proposed_plan_comments
		    SET status = 'resolved', updated_at = ?
		  WHERE thread_id = ? AND id = ?`,
		now, threadID, commentID,
	); err != nil {
		return fmt.Errorf("store: resolve proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	if err := bumpHistoryRevTx(tx, threadID, fmt.Sprintf("store: delete-or-resolve proposed plan comment %s", commentID)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete-or-resolve proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	return nil
}

// MarkProposedPlanCommentsSent flips the selected drafts to `sent` under
// one turn id. Idempotent for a replayed send: when the UPDATE matches
// nothing because this very turn already sent them, it commits without a
// bump, since nothing changed.
func (s *Store) MarkProposedPlanCommentsSent(threadID, planItemID string, commentIDs []string, sentAt int64, sentTurnID string) error {
	ids := uniqueNonEmptyStrings(commentIDs)
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > MaxProposedPlanRevisionCommentIDs {
		return fmt.Errorf("store: too many proposed plan comments selected")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin mark proposed plan comments sent for %s: %w", threadID, err)
	}
	defer tx.Rollback()

	query := `UPDATE proposed_plan_comments
	             SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?
	           WHERE thread_id = ? AND plan_item_id = ? AND status = 'draft' AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 5+len(ids))
	args = append(args, sentAt, sentTurnID, sentAt, threadID, planItemID)
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: mark proposed plan comments sent for %s: %w", threadID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark proposed plan comments sent for %s: rows affected: %w", threadID, err)
	}
	if int(n) != len(ids) {
		ok, checkErr := proposedPlanCommentsAlreadySent(tx, threadID, planItemID, ids, sentTurnID)
		if checkErr != nil {
			return checkErr
		}
		if !ok {
			return fmt.Errorf("store: marked %d proposed plan comments sent for %s, want %d draft comments", n, threadID, len(ids))
		}
	}
	if n > 0 {
		if err := bumpHistoryRevTx(tx, threadID, fmt.Sprintf("store: mark proposed plan comments sent for %s", planItemID)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit mark proposed plan comments sent for %s: %w", threadID, err)
	}
	return nil
}

func proposedPlanCommentsAlreadySent(q sqlQueryer, threadID, planItemID string, ids []string, sentTurnID string) (bool, error) {
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
	if err := q.QueryRow(query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("store: check proposed plan comments sent for %s: %w", threadID, err)
	}
	return count == len(ids), nil
}

func getProposedPlanCommentQ(q sqlQueryer, threadID, commentID string) (ProposedPlanComment, error) {
	row := q.QueryRow(
		`SELECT `+proposedPlanCommentColumns+`
		   FROM proposed_plan_comments
		  WHERE thread_id = ? AND id = ?`,
		threadID, commentID,
	)
	comment, err := scanProposedPlanComment(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProposedPlanComment{}, fmt.Errorf("store: proposed plan comment %s not found on thread %s", commentID, threadID)
		}
		return ProposedPlanComment{}, fmt.Errorf("store: get proposed plan comment %s/%s: %w", threadID, commentID, err)
	}
	return comment, nil
}

func (s *Store) GetProposedPlanComment(threadID, commentID string) (ProposedPlanComment, error) {
	return getProposedPlanCommentQ(s.reader(), threadID, commentID)
}

func (s *Store) ListProposedPlanComments(threadID, planItemID string) ([]ProposedPlanComment, error) {
	rows, err := s.reader().Query(
		`SELECT `+proposedPlanCommentColumns+`
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
	query := `SELECT ` + proposedPlanCommentColumns + `
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
	rows, err := s.reader().Query(query, args...)
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

func (s *Store) proposedPlanCommentCountsByItemID(q sqlQueryer, threadID string, itemIDs []string) (map[string]proposedPlanCommentCounts, error) {
	query := `SELECT plan_item_id, status, COUNT(*)
	            FROM proposed_plan_comments
	           WHERE thread_id = ? AND plan_item_id IN (` + placeholders(len(itemIDs)) + `)
	           GROUP BY plan_item_id, status`
	args := make([]any, 0, 1+len(itemIDs))
	args = append(args, threadID)
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := q.Query(query, args...)
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
