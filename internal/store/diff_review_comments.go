package store

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
)

const (
	MaxDiffReviewCommentBodyBytes  = MaxProposedPlanCommentBodyBytes
	MaxDiffReviewSelectedTextBytes = 8 * 1024
	MaxDiffReviewCommentIDs        = MaxProposedPlanRevisionCommentIDs
	MaxDiffReviewFilePathBytes     = 1024
	MaxDiffReviewSourceKeyBytes    = 128
	MaxDiffReviewPromptBytes       = 256 * 1024
)

type DiffReviewScope string

const (
	DiffReviewScopeTurn      DiffReviewScope = "turn"
	DiffReviewScopeSession   DiffReviewScope = "session"
	DiffReviewScopeWorkspace DiffReviewScope = "workspace"
	DiffReviewScopeBranch    DiffReviewScope = "branch"
	DiffReviewScopePR        DiffReviewScope = "pr"
	DiffReviewScopeEdits     DiffReviewScope = "edits"
)

type DiffReviewComment struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	Scope        string `json:"scope"`
	SourceKey    string `json:"sourceKey"`
	CommitSHA    string `json:"commitSha,omitempty"`
	FilePath     string `json:"filePath"`
	Status       string `json:"status"`
	OldLine      int    `json:"oldLine,omitempty"`
	NewLine      int    `json:"newLine,omitempty"`
	Side         string `json:"side"`
	SelectedText string `json:"selectedText"`
	Body         string `json:"body"`
	SentAt       int64  `json:"sentAt,omitempty"`
	SentTurnID   string `json:"sentTurnId,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
	UpdatedAt    int64  `json:"updatedAt"`
}

type DiffReviewCommentInput struct {
	Scope        string `json:"scope"`
	SourceKey    string `json:"sourceKey"`
	CommitSHA    string `json:"commitSha,omitempty"`
	FilePath     string `json:"filePath"`
	OldLine      int    `json:"oldLine,omitempty"`
	NewLine      int    `json:"newLine,omitempty"`
	Side         string `json:"side"`
	SelectedText string `json:"selectedText"`
	Body         string `json:"body"`
}

type DiffReviewCommentUpdate struct {
	Body string `json:"body"`
}

type DiffReviewSourceRef struct {
	ThreadID  string               `json:"threadId,omitempty"`
	Scope     string               `json:"scope"`
	SourceKey string               `json:"sourceKey"`
	PR        *DiffReviewPRContext `json:"pr,omitempty"`
}

type DiffReviewPRContext struct {
	Number   int                        `json:"number"`
	URL      string                     `json:"url"`
	Comments []DiffReviewPRContextEntry `json:"comments"`
}

type DiffReviewPRContextEntry struct {
	CommentID   string `json:"commentId"`
	HunkExcerpt string `json:"hunkExcerpt"`
}

func NormalizeDiffReviewScope(scope string) (string, error) {
	switch DiffReviewScope(strings.TrimSpace(scope)) {
	case DiffReviewScopeTurn:
		return string(DiffReviewScopeTurn), nil
	case DiffReviewScopeSession:
		return string(DiffReviewScopeSession), nil
	case DiffReviewScopeWorkspace:
		return string(DiffReviewScopeWorkspace), nil
	case DiffReviewScopeBranch:
		return string(DiffReviewScopeBranch), nil
	case DiffReviewScopePR:
		return string(DiffReviewScopePR), nil
	case DiffReviewScopeEdits:
		return string(DiffReviewScopeEdits), nil
	default:
		return "", fmt.Errorf("diff review scope must be turn, session, workspace, branch, pr, or edits")
	}
}

func NormalizeDiffReviewSourceKey(sourceKey string) (string, error) {
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return "", fmt.Errorf("diff review source key is required")
	}
	if len(sourceKey) > MaxDiffReviewSourceKeyBytes || hasControlCharacter(sourceKey) {
		return "", fmt.Errorf("diff review source key is invalid")
	}
	return sourceKey, nil
}

func normalizeDiffReviewSide(side string, oldLine, newLine int) (string, error) {
	side = strings.TrimSpace(side)
	if side == "" {
		if oldLine == 0 && newLine == 0 {
			return "file", nil
		}
		if oldLine > 0 && newLine > 0 {
			return "context", nil
		}
		if oldLine > 0 {
			return "old", nil
		}
		return "new", nil
	}
	switch side {
	case "file":
		if oldLine != 0 || newLine != 0 {
			return "", fmt.Errorf("file comments must not include line numbers")
		}
	case "old":
		if oldLine <= 0 {
			return "", fmt.Errorf("old-line comments require oldLine")
		}
	case "new":
		if newLine <= 0 {
			return "", fmt.Errorf("new-line comments require newLine")
		}
	case "context":
		if oldLine <= 0 || newLine <= 0 {
			return "", fmt.Errorf("context comments require oldLine and newLine")
		}
	default:
		return "", fmt.Errorf("diff review side must be file, old, new, or context")
	}
	return side, nil
}

func validateDiffReviewAnchor(filePath string, oldLine, newLine int) (string, error) {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return "", fmt.Errorf("file path is required")
	}
	if len(filePath) > MaxDiffReviewFilePathBytes || hasControlCharacter(filePath) {
		return "", fmt.Errorf("file path is invalid")
	}
	clean := path.Clean(filePath)
	if clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("file path must be repo-relative")
	}
	if oldLine < 0 || newLine < 0 {
		return "", fmt.Errorf("line numbers must be positive")
	}
	return filePath, nil
}

func normalizeDiffReviewBody(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("comment body is required")
	}
	if len(trimmed) > MaxDiffReviewCommentBodyBytes {
		return "", fmt.Errorf("comment body is too large")
	}
	return trimmed, nil
}

func normalizeDiffReviewSelectedText(selected string) (string, error) {
	selected = strings.TrimSpace(selected)
	if len(selected) > MaxDiffReviewSelectedTextBytes {
		return "", fmt.Errorf("selected diff text is too large")
	}
	return selected, nil
}

func hasControlCharacter(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
}

func (s *Store) CreateDiffReviewComment(comment DiffReviewComment) (DiffReviewComment, error) {
	if comment.ID == "" || comment.ThreadID == "" {
		return DiffReviewComment{}, fmt.Errorf("store: diff review comment id and thread id are required")
	}
	scope, err := NormalizeDiffReviewScope(comment.Scope)
	if err != nil {
		return DiffReviewComment{}, err
	}
	sourceKey, err := NormalizeDiffReviewSourceKey(comment.SourceKey)
	if err != nil {
		return DiffReviewComment{}, err
	}
	filePath, err := validateDiffReviewAnchor(comment.FilePath, comment.OldLine, comment.NewLine)
	if err != nil {
		return DiffReviewComment{}, err
	}
	side, err := normalizeDiffReviewSide(comment.Side, comment.OldLine, comment.NewLine)
	if err != nil {
		return DiffReviewComment{}, err
	}
	selectedText, err := normalizeDiffReviewSelectedText(comment.SelectedText)
	if err != nil {
		return DiffReviewComment{}, err
	}
	body, err := normalizeDiffReviewBody(comment.Body)
	if err != nil {
		return DiffReviewComment{}, err
	}
	comment.Scope = scope
	comment.SourceKey = sourceKey
	comment.CommitSHA = strings.TrimSpace(comment.CommitSHA)
	comment.FilePath = filePath
	comment.Side = side
	comment.SelectedText = selectedText
	comment.Body = body
	comment.Status = "draft"
	if comment.CreatedAt == 0 {
		comment.CreatedAt = nowMillis()
	}
	if comment.UpdatedAt == 0 {
		comment.UpdatedAt = comment.CreatedAt
	}
	_, err = s.db.Exec(
		`INSERT INTO diff_review_comments (
			id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
			selected_text, body, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?, ?)`,
		comment.ID, comment.ThreadID, comment.Scope, comment.SourceKey, comment.CommitSHA, comment.FilePath, comment.OldLine, comment.NewLine,
		comment.Side, comment.SelectedText, comment.Body, comment.CreatedAt, comment.UpdatedAt,
	)
	if err != nil {
		return DiffReviewComment{}, fmt.Errorf("store: create diff review comment %s: %w", comment.ID, err)
	}
	return s.GetDiffReviewComment(comment.ThreadID, comment.ID)
}

func (s *Store) UpdateDiffReviewComment(threadID, commentID string, update DiffReviewCommentUpdate, now int64) (DiffReviewComment, error) {
	body, err := normalizeDiffReviewBody(update.Body)
	if err != nil {
		return DiffReviewComment{}, err
	}
	res, err := s.db.Exec(
		`UPDATE diff_review_comments
		    SET body = ?, status = 'draft', sent_at = 0, sent_turn_id = '', updated_at = ?
		  WHERE thread_id = ? AND id = ? AND status <> 'resolved'`,
		body, now, threadID, commentID,
	)
	if err != nil {
		return DiffReviewComment{}, fmt.Errorf("store: update diff review comment %s/%s: %w", threadID, commentID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return DiffReviewComment{}, fmt.Errorf("store: diff review comment %s not found or resolved", commentID)
	}
	return s.GetDiffReviewComment(threadID, commentID)
}

func (s *Store) DeleteOrResolveDiffReviewComment(threadID, commentID string, now int64) error {
	comment, err := s.GetDiffReviewComment(threadID, commentID)
	if err != nil {
		return err
	}
	if comment.Status == "draft" {
		if _, err := s.db.Exec(`DELETE FROM diff_review_comments WHERE thread_id = ? AND id = ?`, threadID, commentID); err != nil {
			return fmt.Errorf("store: delete draft diff review comment %s/%s: %w", threadID, commentID, err)
		}
		return nil
	}
	_, err = s.db.Exec(
		`UPDATE diff_review_comments
		    SET status = 'resolved', updated_at = ?
		  WHERE thread_id = ? AND id = ?`,
		now, threadID, commentID,
	)
	if err != nil {
		return fmt.Errorf("store: resolve diff review comment %s/%s: %w", threadID, commentID, err)
	}
	return nil
}

func (s *Store) MarkDiffReviewCommentsSent(threadID, scope, sourceKey string, commentIDs []string, sentAt int64, sentTurnID string) error {
	scope, err := NormalizeDiffReviewScope(scope)
	if err != nil {
		return err
	}
	sourceKey, err = NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return err
	}
	ids := uniqueNonEmptyStrings(commentIDs)
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > MaxDiffReviewCommentIDs {
		return fmt.Errorf("store: too many diff review comments selected")
	}
	query := `UPDATE diff_review_comments
	             SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?
	           WHERE thread_id = ? AND scope = ? AND source_key = ? AND status = 'draft' AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 6+len(ids))
	args = append(args, sentAt, sentTurnID, sentAt, threadID, scope, sourceKey)
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: mark diff review comments sent for %s/%s: %w", threadID, scope, err)
	}
	if n, err := res.RowsAffected(); err == nil && int(n) != len(ids) {
		ok, checkErr := s.diffReviewCommentsAlreadySent(threadID, scope, sourceKey, ids, sentTurnID)
		if checkErr != nil {
			return checkErr
		}
		if ok {
			return nil
		}
		return fmt.Errorf("store: marked %d diff review comments sent for %s/%s, want %d draft comments", n, threadID, scope, len(ids))
	}
	return nil
}

func (s *Store) diffReviewCommentsAlreadySent(threadID, scope, sourceKey string, ids []string, sentTurnID string) (bool, error) {
	query := `SELECT id
	            FROM diff_review_comments
	           WHERE thread_id = ?
	             AND scope = ?
	             AND source_key = ?
	             AND status = 'sent'
	             AND sent_turn_id = ?
	             AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 4+len(ids))
	args = append(args, threadID, scope, sourceKey, sentTurnID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return false, fmt.Errorf("store: check sent diff review comments for %s/%s: %w", threadID, scope, err)
	}
	defer rows.Close()
	found := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, fmt.Errorf("store: scan sent diff review comment id: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: iterate sent diff review comment ids: %w", err)
	}
	return len(found) == len(ids), nil
}

func (s *Store) GetDiffReviewComment(threadID, commentID string) (DiffReviewComment, error) {
	row := s.reader().QueryRow(
		`SELECT id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
		        selected_text, body, sent_at, sent_turn_id, created_at, updated_at
		   FROM diff_review_comments
		  WHERE thread_id = ? AND id = ?`,
		threadID, commentID,
	)
	comment, err := scanDiffReviewComment(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return DiffReviewComment{}, fmt.Errorf("store: diff review comment %s not found on thread %s", commentID, threadID)
		}
		return DiffReviewComment{}, fmt.Errorf("store: get diff review comment %s/%s: %w", threadID, commentID, err)
	}
	return comment, nil
}

func (s *Store) ListDiffReviewComments(threadID, scope, sourceKey string) ([]DiffReviewComment, error) {
	scope, err := NormalizeDiffReviewScope(scope)
	if err != nil {
		return nil, err
	}
	sourceKey, err = NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader().Query(
		`SELECT id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
		        selected_text, body, sent_at, sent_turn_id, created_at, updated_at
		   FROM diff_review_comments
		  WHERE thread_id = ? AND scope = ? AND source_key = ? AND status <> 'resolved'
		  ORDER BY file_path ASC, old_line ASC, new_line ASC, created_at ASC`,
		threadID, scope, sourceKey,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list diff review comments %s/%s: %w", threadID, scope, err)
	}
	defer rows.Close()

	out := []DiffReviewComment{}
	for rows.Next() {
		comment, err := scanDiffReviewComment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan diff review comment: %w", err)
		}
		out = append(out, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate diff review comments %s/%s: %w", threadID, scope, err)
	}
	return out, nil
}

func (s *Store) ListDraftDiffReviewCommentsByID(threadID, scope, sourceKey string, ids []string) ([]DiffReviewComment, error) {
	scope, err := NormalizeDiffReviewScope(scope)
	if err != nil {
		return nil, err
	}
	sourceKey, err = NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return nil, err
	}
	wanted := uniqueNonEmptyStrings(ids)
	if len(wanted) == 0 {
		return []DiffReviewComment{}, nil
	}
	if len(wanted) > MaxDiffReviewCommentIDs {
		return nil, fmt.Errorf("store: too many diff review comments selected")
	}
	query := `SELECT id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	                 selected_text, body, sent_at, sent_turn_id, created_at, updated_at
	            FROM diff_review_comments
	           WHERE thread_id = ?
	             AND scope = ?
	             AND source_key = ?
	             AND status = 'draft'
	             AND id IN (` + placeholders(len(wanted)) + `)
	           ORDER BY file_path ASC, old_line ASC, new_line ASC, created_at ASC`
	args := make([]any, 0, 3+len(wanted))
	args = append(args, threadID, scope, sourceKey)
	for _, id := range wanted {
		args = append(args, id)
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list selected draft diff review comments %s/%s: %w", threadID, scope, err)
	}
	defer rows.Close()

	out := make([]DiffReviewComment, 0, len(wanted))
	found := make(map[string]bool, len(wanted))
	for rows.Next() {
		comment, err := scanDiffReviewComment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan selected draft diff review comment: %w", err)
		}
		out = append(out, comment)
		found[comment.ID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate selected draft diff review comments %s/%s: %w", threadID, scope, err)
	}
	if len(found) != len(wanted) {
		return nil, fmt.Errorf("store: selected diff review comments include stale or non-draft ids")
	}
	return out, nil
}

func scanDiffReviewComment(scanner interface{ Scan(...any) error }) (DiffReviewComment, error) {
	var comment DiffReviewComment
	if err := scanner.Scan(
		&comment.ID, &comment.ThreadID, &comment.Scope, &comment.SourceKey, &comment.CommitSHA, &comment.FilePath, &comment.Status,
		&comment.OldLine, &comment.NewLine, &comment.Side, &comment.SelectedText, &comment.Body,
		&comment.SentAt, &comment.SentTurnID, &comment.CreatedAt, &comment.UpdatedAt,
	); err != nil {
		return DiffReviewComment{}, err
	}
	return comment, nil
}
