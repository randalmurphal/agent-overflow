package store

import (
	"database/sql"
	"encoding/json"
	"errors"
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

// DiffReviewComment is one review comment on a diff. It belongs to a
// thread (ThreadID) or, for a PR the Pull requests page reviews with no
// thread, to that PR (PRKey, git.PRReference.Key); exactly one is set.
type DiffReviewComment struct {
	ID           string `json:"id"`
	ThreadID     string `json:"threadId"`
	PRKey        string `json:"prKey,omitempty"`
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

// DiffReviewSourceRef names the drafts a sent message carries: a scope and
// source key of the thread's own drafts (ThreadID), or of the drafts a PR
// owns (PRKey), which a send appends to another thread.
type DiffReviewSourceRef struct {
	ThreadID  string               `json:"threadId,omitempty"`
	PRKey     string               `json:"prKey,omitempty"`
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

// DiffReviewOwner is who diff review comments belong to: a thread, or a PR
// reviewed with no thread (its git.PRReference.Key). Exactly one is set;
// every accessor reads and writes only the rows of the owner it names, so
// a thread's paths never see a PR's drafts and the reverse.
type DiffReviewOwner struct {
	ThreadID string
	PRKey    string
}

// ThreadReviewOwner is the owner of a thread's own comments.
func ThreadReviewOwner(threadID string) DiffReviewOwner {
	return DiffReviewOwner{ThreadID: threadID}
}

// PRReviewOwner is the owner of the drafts on a PR with no thread.
func PRReviewOwner(prKey string) DiffReviewOwner {
	return DiffReviewOwner{PRKey: prKey}
}

func (o DiffReviewOwner) validate() error {
	switch {
	case (o.ThreadID == "") == (o.PRKey == ""):
		return fmt.Errorf("store: a diff review comment belongs to exactly one thread or PR")
	case o.PRKey != "" && (len(o.PRKey) > MaxDiffReviewSourceKeyBytes || hasControlCharacter(o.PRKey)):
		return fmt.Errorf("store: diff review PR key is invalid")
	}
	return nil
}

// column is the owner's column and the value it is matched with.
func (o DiffReviewOwner) column() (string, string) {
	if o.PRKey != "" {
		return "pr_key", o.PRKey
	}
	return "thread_id", o.ThreadID
}

func (o DiffReviewOwner) String() string {
	if o.PRKey != "" {
		return "PR " + o.PRKey
	}
	return "thread " + o.ThreadID
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

// CreateDiffReviewComment writes a draft owned by comment.ThreadID or
// comment.PRKey, exactly one of them. A PR's drafts are PR scope.
func (s *Store) CreateDiffReviewComment(comment DiffReviewComment) (DiffReviewComment, error) {
	owner := DiffReviewOwner{ThreadID: comment.ThreadID, PRKey: comment.PRKey}
	if comment.ID == "" {
		return DiffReviewComment{}, fmt.Errorf("store: diff review comment id is required")
	}
	if err := owner.validate(); err != nil {
		return DiffReviewComment{}, err
	}
	scope, err := NormalizeDiffReviewScope(comment.Scope)
	if err != nil {
		return DiffReviewComment{}, err
	}
	if owner.PRKey != "" && scope != string(DiffReviewScopePR) {
		return DiffReviewComment{}, fmt.Errorf("store: a PR's diff review comments are pr scope, not %s", scope)
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
			id, thread_id, pr_key, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
			selected_text, body, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?, ?, ?, ?, ?, ?)`,
		comment.ID, nullableText(owner.ThreadID), nullableText(owner.PRKey), comment.Scope, comment.SourceKey, comment.CommitSHA, comment.FilePath, comment.OldLine, comment.NewLine,
		comment.Side, comment.SelectedText, comment.Body, comment.CreatedAt, comment.UpdatedAt,
	)
	if err != nil {
		return DiffReviewComment{}, fmt.Errorf("store: create diff review comment %s: %w", comment.ID, err)
	}
	return s.GetDiffReviewComment(owner, comment.ID)
}

// nullableText is NULL for an empty string.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) UpdateDiffReviewComment(owner DiffReviewOwner, commentID string, update DiffReviewCommentUpdate, now int64) (DiffReviewComment, error) {
	if err := owner.validate(); err != nil {
		return DiffReviewComment{}, err
	}
	body, err := normalizeDiffReviewBody(update.Body)
	if err != nil {
		return DiffReviewComment{}, err
	}
	column, value := owner.column()
	res, err := s.db.Exec(
		`UPDATE diff_review_comments
		    SET body = ?, status = 'draft', sent_at = 0, sent_turn_id = '', updated_at = ?
		  WHERE `+column+` = ? AND id = ? AND status <> 'resolved'`,
		body, now, value, commentID,
	)
	if err != nil {
		return DiffReviewComment{}, fmt.Errorf("store: update diff review comment %s of %s: %w", commentID, owner, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return DiffReviewComment{}, fmt.Errorf("store: diff review comment %s not found or resolved", commentID)
	}
	return s.GetDiffReviewComment(owner, commentID)
}

func (s *Store) DeleteOrResolveDiffReviewComment(owner DiffReviewOwner, commentID string, now int64) error {
	comment, err := s.GetDiffReviewComment(owner, commentID)
	if err != nil {
		return err
	}
	column, value := owner.column()
	if comment.Status == "draft" {
		if _, err := s.db.Exec(`DELETE FROM diff_review_comments WHERE `+column+` = ? AND id = ?`, value, commentID); err != nil {
			return fmt.Errorf("store: delete draft diff review comment %s of %s: %w", commentID, owner, err)
		}
		return nil
	}
	_, err = s.db.Exec(
		`UPDATE diff_review_comments
		    SET status = 'resolved', updated_at = ?
		  WHERE `+column+` = ? AND id = ?`,
		now, value, commentID,
	)
	if err != nil {
		return fmt.Errorf("store: resolve diff review comment %s of %s: %w", commentID, owner, err)
	}
	return nil
}

// MarkDiffReviewCommentsSent marks the owner's drafts commentIDs sent in
// the turn sentTurnID: the owner's own thread's turn, or for a PR's drafts
// the turn of the thread they were sent to.
func (s *Store) MarkDiffReviewCommentsSent(owner DiffReviewOwner, scope, sourceKey string, commentIDs []string, sentAt int64, sentTurnID string) error {
	if err := owner.validate(); err != nil {
		return err
	}
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
	column, value := owner.column()
	query := `UPDATE diff_review_comments
	             SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?
	           WHERE ` + column + ` = ? AND scope = ? AND source_key = ? AND status = 'draft' AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 6+len(ids))
	args = append(args, sentAt, sentTurnID, sentAt, value, scope, sourceKey)
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: mark diff review comments sent for %s/%s: %w", owner, scope, err)
	}
	if n, err := res.RowsAffected(); err == nil && int(n) != len(ids) {
		ok, checkErr := s.diffReviewCommentsAlreadySent(owner, scope, sourceKey, ids, sentTurnID)
		if checkErr != nil {
			return checkErr
		}
		if ok {
			return nil
		}
		return fmt.Errorf("store: marked %d diff review comments sent for %s/%s, want %d draft comments", n, owner, scope, len(ids))
	}
	return nil
}

// DiffReviewCommentSnapshot is a draft as a send read it: its id and the
// body the message carries.
type DiffReviewCommentSnapshot struct {
	ID   string
	Body string
}

// ErrDiffReviewCommentsSentElsewhere is a claim naming a draft another
// message already carried.
var ErrDiffReviewCommentsSentElsewhere = errors.New("a selected review comment was already sent in another message")

// ClaimDiffReviewCommentsSent marks sent in the turn sentTurnID each of the
// owner's drafts in snapshot whose body is still the one the message
// carries, in one transaction, and returns the ids it marked. A draft
// edited since stays a draft, a deleted one is skipped, and one already
// sent in sentTurnID is left as it is. A draft sent in another turn, or
// resolved, fails the whole claim with ErrDiffReviewCommentsSentElsewhere
// and marks nothing: that text reached another message.
func (s *Store) ClaimDiffReviewCommentsSent(owner DiffReviewOwner, scope, sourceKey string, snapshot []DiffReviewCommentSnapshot, sentAt int64, sentTurnID string) ([]string, error) {
	if err := owner.validate(); err != nil {
		return nil, err
	}
	scope, err := NormalizeDiffReviewScope(scope)
	if err != nil {
		return nil, err
	}
	sourceKey, err = NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sentTurnID) == "" {
		return nil, fmt.Errorf("store: claim diff review comments: the turn is required")
	}
	if len(snapshot) > MaxDiffReviewCommentIDs {
		return nil, fmt.Errorf("store: too many diff review comments selected")
	}
	column, value := owner.column()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: claim diff review comments for %s/%s: %w", owner, scope, err)
	}
	defer func() { _ = tx.Rollback() }()
	claimed := make([]string, 0, len(snapshot))
	for _, comment := range snapshot {
		res, err := tx.Exec(
			`UPDATE diff_review_comments
			    SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?
			  WHERE `+column+` = ? AND scope = ? AND source_key = ? AND id = ? AND status = 'draft' AND body = ?`,
			sentAt, sentTurnID, sentAt, value, scope, sourceKey, comment.ID, comment.Body,
		)
		if err != nil {
			return nil, fmt.Errorf("store: claim diff review comment %s of %s: %w", comment.ID, owner, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("store: claim diff review comment %s of %s: %w", comment.ID, owner, err)
		}
		if n == 1 {
			claimed = append(claimed, comment.ID)
			continue
		}
		var status, turn string
		err = tx.QueryRow(
			`SELECT status, sent_turn_id FROM diff_review_comments
			  WHERE `+column+` = ? AND scope = ? AND source_key = ? AND id = ?`,
			value, scope, sourceKey, comment.ID,
		).Scan(&status, &turn)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("store: read diff review comment %s of %s: %w", comment.ID, owner, err)
		case status == "draft", turn == sentTurnID:
			continue
		default:
			return nil, ErrDiffReviewCommentsSentElsewhere
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: claim diff review comments for %s/%s: %w", owner, scope, err)
	}
	return claimed, nil
}

// ReleaseDiffReviewCommentsSent returns to drafts the comments ids a claim
// for sentTurnID marked, when the message that claimed them was not sent.
// A comment edited or claimed again since is left alone.
func (s *Store) ReleaseDiffReviewCommentsSent(owner DiffReviewOwner, scope, sourceKey string, ids []string, sentTurnID string) error {
	return s.moveClaimedDiffReviewComments(owner, scope, sourceKey, ids, sentTurnID, "")
}

// RetargetDiffReviewCommentsSent moves the comments ids a claim for
// fromTurnID marked to the turn toTurnID, when the message that claimed
// them landed in another turn.
func (s *Store) RetargetDiffReviewCommentsSent(owner DiffReviewOwner, scope, sourceKey string, ids []string, fromTurnID, toTurnID string) error {
	if strings.TrimSpace(toTurnID) == "" {
		return fmt.Errorf("store: retarget diff review comments: the turn is required")
	}
	return s.moveClaimedDiffReviewComments(owner, scope, sourceKey, ids, fromTurnID, toTurnID)
}

// moveClaimedDiffReviewComments moves the comments ids sent in fromTurnID
// to toTurnID, or back to drafts when toTurnID is empty.
func (s *Store) moveClaimedDiffReviewComments(owner DiffReviewOwner, scope, sourceKey string, ids []string, fromTurnID, toTurnID string) error {
	if err := owner.validate(); err != nil {
		return err
	}
	ids = uniqueNonEmptyStrings(ids)
	if len(ids) == 0 {
		return nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("store: encode diff review comment ids: %w", err)
	}
	column, value := owner.column()
	if _, err := s.db.Exec(
		`UPDATE diff_review_comments
		    SET status = CASE WHEN ?1 = '' THEN 'draft' ELSE 'sent' END,
		        sent_at = CASE WHEN ?1 = '' THEN 0 ELSE sent_at END,
		        sent_turn_id = ?1
		  WHERE `+column+` = ?2 AND scope = ?3 AND source_key = ?4 AND status = 'sent' AND sent_turn_id = ?5
		    AND id IN (SELECT value FROM json_each(?6))`,
		toTurnID, value, scope, sourceKey, fromTurnID, string(encoded),
	); err != nil {
		return fmt.Errorf("store: move diff review comments of %s from turn %s: %w", owner, fromTurnID, err)
	}
	return nil
}

func (s *Store) diffReviewCommentsAlreadySent(owner DiffReviewOwner, scope, sourceKey string, ids []string, sentTurnID string) (bool, error) {
	column, value := owner.column()
	query := `SELECT id
	            FROM diff_review_comments
	           WHERE ` + column + ` = ?
	             AND scope = ?
	             AND source_key = ?
	             AND status = 'sent'
	             AND sent_turn_id = ?
	             AND id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, 4+len(ids))
	args = append(args, value, scope, sourceKey, sentTurnID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return false, fmt.Errorf("store: check sent diff review comments for %s/%s: %w", owner, scope, err)
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

// diffReviewCommentColumns is a comment row as scanOwnedDiffReviewComment
// reads it.
const diffReviewCommentColumns = `id, thread_id, pr_key, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
		        selected_text, body, sent_at, sent_turn_id, created_at, updated_at`

func (s *Store) GetDiffReviewComment(owner DiffReviewOwner, commentID string) (DiffReviewComment, error) {
	if err := owner.validate(); err != nil {
		return DiffReviewComment{}, err
	}
	column, value := owner.column()
	row := s.reader().QueryRow(
		`SELECT `+diffReviewCommentColumns+`
		   FROM diff_review_comments
		  WHERE `+column+` = ? AND id = ?`,
		value, commentID,
	)
	comment, err := scanOwnedDiffReviewComment(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return DiffReviewComment{}, fmt.Errorf("store: diff review comment %s not found on %s", commentID, owner)
		}
		return DiffReviewComment{}, fmt.Errorf("store: get diff review comment %s of %s: %w", commentID, owner, err)
	}
	return comment, nil
}

func (s *Store) ListDiffReviewComments(owner DiffReviewOwner, scope, sourceKey string) ([]DiffReviewComment, error) {
	if err := owner.validate(); err != nil {
		return nil, err
	}
	scope, err := NormalizeDiffReviewScope(scope)
	if err != nil {
		return nil, err
	}
	sourceKey, err = NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return nil, err
	}
	column, value := owner.column()
	rows, err := s.reader().Query(
		`SELECT `+diffReviewCommentColumns+`
		   FROM diff_review_comments
		  WHERE `+column+` = ? AND scope = ? AND source_key = ? AND status <> 'resolved'
		  ORDER BY file_path ASC, old_line ASC, new_line ASC, created_at ASC`,
		value, scope, sourceKey,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list diff review comments %s/%s: %w", owner, scope, err)
	}
	defer rows.Close()

	out := []DiffReviewComment{}
	for rows.Next() {
		comment, err := scanOwnedDiffReviewComment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan diff review comment: %w", err)
		}
		out = append(out, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate diff review comments %s/%s: %w", owner, scope, err)
	}
	return out, nil
}

func (s *Store) ListDraftDiffReviewCommentsByID(owner DiffReviewOwner, scope, sourceKey string, ids []string) ([]DiffReviewComment, error) {
	if err := owner.validate(); err != nil {
		return nil, err
	}
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
	column, value := owner.column()
	query := `SELECT ` + diffReviewCommentColumns + `
	            FROM diff_review_comments
	           WHERE ` + column + ` = ?
	             AND scope = ?
	             AND source_key = ?
	             AND status = 'draft'
	             AND id IN (` + placeholders(len(wanted)) + `)
	           ORDER BY file_path ASC, old_line ASC, new_line ASC, created_at ASC`
	args := make([]any, 0, 3+len(wanted))
	args = append(args, value, scope, sourceKey)
	for _, id := range wanted {
		args = append(args, id)
	}
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list selected draft diff review comments %s/%s: %w", owner, scope, err)
	}
	defer rows.Close()

	out := make([]DiffReviewComment, 0, len(wanted))
	found := make(map[string]bool, len(wanted))
	for rows.Next() {
		comment, err := scanOwnedDiffReviewComment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan selected draft diff review comment: %w", err)
		}
		out = append(out, comment)
		found[comment.ID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate selected draft diff review comments %s/%s: %w", owner, scope, err)
	}
	if len(found) != len(wanted) {
		return nil, fmt.Errorf("store: selected diff review comments include stale or non-draft ids")
	}
	return out, nil
}

// scanOwnedDiffReviewComment reads a diffReviewCommentColumns row, whose
// owner is a thread or a PR.
func scanOwnedDiffReviewComment(scanner interface{ Scan(...any) error }) (DiffReviewComment, error) {
	var comment DiffReviewComment
	var threadID, prKey sql.NullString
	if err := scanner.Scan(
		&comment.ID, &threadID, &prKey, &comment.Scope, &comment.SourceKey, &comment.CommitSHA, &comment.FilePath, &comment.Status,
		&comment.OldLine, &comment.NewLine, &comment.Side, &comment.SelectedText, &comment.Body,
		&comment.SentAt, &comment.SentTurnID, &comment.CreatedAt, &comment.UpdatedAt,
	); err != nil {
		return DiffReviewComment{}, err
	}
	comment.ThreadID, comment.PRKey = threadID.String, prKey.String
	return comment, nil
}

// scanDiffReviewComment reads a thread-owned row without its pr_key, as
// a transfer exports it (transferDiffCommentColumns).
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
