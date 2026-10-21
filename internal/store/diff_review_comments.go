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

// UpdateDiffReviewComment saves a new body as a draft. The edit supersedes
// any message that carried the old text, so the comment's claim goes with it.
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
		    SET body = ?, status = 'draft', sent_at = 0, sent_turn_id = '', updated_at = ?,
		        claim_thread_id = '', claim_item_id = '', claim_settles = ''
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

// DeleteOrResolveDiffReviewComment deletes a draft and resolves any other
// comment. A resolved comment keeps no claim: no message's delivery can
// return it to a draft.
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
		    SET status = 'resolved', updated_at = ?, claim_thread_id = '', claim_item_id = '', claim_settles = ''
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
// the turn of the thread they were sent to, or published to the forge. It
// is a delivery of its own: the comments keep no claim of a message.
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
	             SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?,
	                 claim_thread_id = '', claim_item_id = '', claim_settles = ''
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
	ID   string `json:"id"`
	Body string `json:"body"`
}

// ErrDiffReviewCommentsSentElsewhere is a claim naming a draft another
// message already carried.
var ErrDiffReviewCommentsSentElsewhere = errors.New("a selected review comment was already sent in another message")

// DiffReviewDelivery is the message a claim marks drafts sent by: the turn
// they are sent in and the user row, ThreadID/ItemID, that carries them.
// Queued is a message the flush queue dispatches: its row can land before
// the provider has it, so its claim settles when the provider confirms
// consumption (settleDiffReviewClaimsTx), not when the row lands.
type DiffReviewDelivery struct {
	SentAt   int64
	TurnID   string
	ThreadID string
	ItemID   string
	Queued   bool
}

// What a claim waits for (claim_settles): nothing, its row's insert, or the
// provider's confirmation that it consumed the message.
const (
	diffReviewClaimSettled          = ""
	diffReviewClaimSettlesAtRow     = "row"
	diffReviewClaimSettlesAtConfirm = "confirm"
)

// ClaimDiffReviewCommentsSent marks sent in the turn delivery.TurnID each
// of the owner's drafts in snapshot whose body is still the one the message
// carries, in one transaction, and returns the ids it marked. A draft
// edited since stays a draft, a deleted one is skipped, and one already
// sent in that turn is left as it is. A draft sent in another turn, or
// resolved, fails the whole claim with ErrDiffReviewCommentsSentElsewhere
// and marks nothing: that text reached another message. The snapshot is
// claimed whole in one transaction, however many members' comments it joins.
//
// The claim names the message's row (claim_thread_id, claim_item_id) and
// waits for its delivery (claim_settles): a direct send or steer is
// delivered when its row lands, which settles the claim in the row's
// transaction (items_settle_diff_review_claims); a queued one when the
// provider confirms it (delivery.Queued, settleDiffReviewClaimsTx). A
// message retired undelivered returns its comments by the row
// (RetireUndeliveredUserRow), and a claim still unsettled at boot is
// released (ReleaseUnsettledDiffReviewClaims).
func (s *Store) ClaimDiffReviewCommentsSent(owner DiffReviewOwner, scope, sourceKey string, snapshot []DiffReviewCommentSnapshot, delivery DiffReviewDelivery) ([]string, error) {
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
	sentTurnID := delivery.TurnID
	if strings.TrimSpace(sentTurnID) == "" {
		return nil, fmt.Errorf("store: claim diff review comments: the turn is required")
	}
	if strings.TrimSpace(delivery.ThreadID) == "" || strings.TrimSpace(delivery.ItemID) == "" {
		return nil, fmt.Errorf("store: claim diff review comments: the message row is required")
	}
	// No selection limit here: the selection a message renders is bounded
	// where it is read (ListDraftDiffReviewCommentsByID), and a joined
	// message claims the comments of every member at once.
	column, value := owner.column()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: claim diff review comments for %s/%s: %w", owner, scope, err)
	}
	defer func() { _ = tx.Rollback() }()
	settles := diffReviewClaimSettlesAtConfirm
	if !delivery.Queued {
		// A row that has already landed delivers the message at once.
		var landed bool
		if err := tx.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM items WHERE thread_id = ? AND id = ?)`, delivery.ThreadID, delivery.ItemID,
		).Scan(&landed); err != nil {
			return nil, fmt.Errorf("store: claim diff review comments: read the message row: %w", err)
		}
		settles = diffReviewClaimSettlesAtRow
		if landed {
			settles = diffReviewClaimSettled
		}
	}
	sentAt := delivery.SentAt
	claimed := make([]string, 0, len(snapshot))
	for _, comment := range snapshot {
		res, err := tx.Exec(
			`UPDATE diff_review_comments
			    SET status = 'sent', sent_at = ?, sent_turn_id = ?, updated_at = ?, claim_thread_id = ?, claim_item_id = ?, claim_settles = ?
			  WHERE `+column+` = ? AND scope = ? AND source_key = ? AND id = ? AND status = 'draft' AND body = ?`,
			sentAt, sentTurnID, sentAt, delivery.ThreadID, delivery.ItemID, settles, value, scope, sourceKey, comment.ID, comment.Body,
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
	return s.moveClaimedDiffReviewComments(owner, scope, sourceKey, ids, sentTurnID, "", "")
}

// RetargetDiffReviewCommentsSent moves the comments ids a claim for
// fromTurnID marked to the turn toTurnID, when the message that claimed
// them is placed in another turn as the row toItemID. The claim names
// toItemID from then on and still waits for what it waited for.
func (s *Store) RetargetDiffReviewCommentsSent(owner DiffReviewOwner, scope, sourceKey string, ids []string, fromTurnID, toTurnID, toItemID string) error {
	if strings.TrimSpace(toTurnID) == "" || strings.TrimSpace(toItemID) == "" {
		return fmt.Errorf("store: retarget diff review comments: the turn and the message row are required")
	}
	return s.moveClaimedDiffReviewComments(owner, scope, sourceKey, ids, fromTurnID, toTurnID, toItemID)
}

// moveClaimedDiffReviewComments moves the comments ids sent in fromTurnID
// to toTurnID and the row toItemID, or back to drafts when toTurnID is
// empty.
func (s *Store) moveClaimedDiffReviewComments(owner DiffReviewOwner, scope, sourceKey string, ids []string, fromTurnID, toTurnID, toItemID string) error {
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
		        sent_turn_id = ?1,
		        claim_thread_id = CASE WHEN ?1 = '' OR claim_item_id = '' THEN '' ELSE claim_thread_id END,
		        claim_item_id = CASE WHEN ?1 = '' OR claim_item_id = '' THEN '' ELSE ?7 END,
		        claim_settles = CASE WHEN ?1 = '' OR claim_item_id = '' THEN '' ELSE claim_settles END
		  WHERE `+column+` = ?2 AND scope = ?3 AND source_key = ?4 AND status = 'sent' AND sent_turn_id = ?5
		    AND id IN (SELECT value FROM json_each(?6))`,
		toTurnID, value, scope, sourceKey, fromTurnID, string(encoded), toItemID,
	); err != nil {
		return fmt.Errorf("store: move diff review comments of %s from turn %s: %w", owner, fromTurnID, err)
	}
	return nil
}

// ReleaseUnsettledDiffReviewClaims returns to drafts every comment a claim
// marked sent whose message was never delivered, and reports how many. Run
// it only while no message can be in flight (App boot): a claim still
// unsettled then belongs to a message whose row the previous instance never
// persisted, or that no provider confirmed.
func (s *Store) ReleaseUnsettledDiffReviewClaims() (int, error) {
	res, err := s.db.Exec(
		`UPDATE diff_review_comments
		    SET status = CASE WHEN status = 'sent' THEN 'draft' ELSE status END,
		        sent_at = CASE WHEN status = 'sent' THEN 0 ELSE sent_at END,
		        sent_turn_id = CASE WHEN status = 'sent' THEN '' ELSE sent_turn_id END,
		        claim_thread_id = '', claim_item_id = '', claim_settles = ''
		  WHERE claim_item_id <> '' AND claim_settles <> ''`,
	)
	if err != nil {
		return 0, fmt.Errorf("store: release unsettled diff review claims: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: release unsettled diff review claims: %w", err)
	}
	return int(n), nil
}

// settleDiffReviewClaimsTx settles the claims of the message the row
// threadID/itemID carries: the provider confirmed it consumed the message.
// The confirmation writes run it in their transaction
// (PlaceUserItemsAfterBoundary, UpdateItemMetaAtBoundary).
func settleDiffReviewClaimsTx(tx sqlExecutor, threadID, itemID string) error {
	if _, err := tx.Exec(
		`UPDATE diff_review_comments SET claim_settles = ''
		  WHERE claim_item_id <> '' AND claim_thread_id = `+boundText+` AND claim_item_id = `+boundText+` AND claim_settles <> ''`,
		threadID, itemID,
	); err != nil {
		return fmt.Errorf("store: settle the review claims of %s/%s: %w", threadID, itemID, err)
	}
	return nil
}

// SettleDiffReviewClaims settles the claims of the message the row
// threadID/itemID carries, for a confirmation that writes nothing else.
func (s *Store) SettleDiffReviewClaims(threadID, itemID string) error {
	return settleDiffReviewClaimsTx(s.db, threadID, itemID)
}

// DiffReviewCommentSource is one owner's scope and source key, whose review
// panes show a change.
type DiffReviewCommentSource struct {
	Owner     DiffReviewOwner
	Scope     string
	SourceKey string
}

// DiffReviewReturnedComments are the comments of one source that a retire
// returned to drafts, each with the body its message carried.
type DiffReviewReturnedComments struct {
	DiffReviewCommentSource
	Comments []DiffReviewCommentSnapshot
}

// RetireUndeliveredUserRow takes back a message the provider never
// consumed: in one transaction it returns to drafts the comments still
// marked sent by the message the row threadID/itemID carries, settled or
// not, and deletes the row if it exists (a deferred message has none). It
// returns the comments it returned, by source. A comment edited since its
// message claimed it is not among them: the edit cleared the claim. So
// each returned body is the one the message carries.
func (s *Store) RetireUndeliveredUserRow(threadID, itemID string) ([]DiffReviewReturnedComments, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		return nil, fmt.Errorf("store: retire undelivered user row: the thread and the row are required")
	}
	var sources []DiffReviewReturnedComments
	err := s.writeItems(threadID, nil, "retire undelivered user row "+threadID+"/"+itemID, func(tx *sql.Tx, w *cardWrite) error {
		sources = sources[:0]
		rows, err := tx.Query(
			`UPDATE diff_review_comments
			    SET status = CASE WHEN status = 'sent' THEN 'draft' ELSE status END,
			        sent_at = CASE WHEN status = 'sent' THEN 0 ELSE sent_at END,
			        sent_turn_id = CASE WHEN status = 'sent' THEN '' ELSE sent_turn_id END,
			        claim_thread_id = '', claim_item_id = '', claim_settles = ''
			  WHERE claim_item_id <> '' AND claim_thread_id = `+boundText+` AND claim_item_id = `+boundText+`
			RETURNING COALESCE(thread_id, ''), COALESCE(pr_key, ''), scope, source_key, id, body`,
			threadID, itemID,
		)
		if err != nil {
			return fmt.Errorf("store: return the review comments of %s/%s: %w", threadID, itemID, err)
		}
		index := map[DiffReviewCommentSource]int{}
		for rows.Next() {
			var source DiffReviewCommentSource
			var comment DiffReviewCommentSnapshot
			if err := rows.Scan(&source.Owner.ThreadID, &source.Owner.PRKey, &source.Scope, &source.SourceKey, &comment.ID, &comment.Body); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: return the review comments of %s/%s: %w", threadID, itemID, err)
			}
			i, seen := index[source]
			if !seen {
				i = len(sources)
				index[source] = i
				sources = append(sources, DiffReviewReturnedComments{DiffReviewCommentSource: source})
			}
			sources[i].Comments = append(sources[i].Comments, comment)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: return the review comments of %s/%s: %w", threadID, itemID, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: return the review comments of %s/%s: %w", threadID, itemID, err)
		}
		if err := deleteThreadItemTx(tx, w, threadID, itemID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sources, nil
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

// DiffReviewMessageRows are user rows of thread ThreadID that an earlier
// dispatch of one message landed as, and that are still to be retired.
type DiffReviewMessageRows struct {
	ThreadID string
	ItemIDs  []string
}

// ListDraftDiffReviewCommentsByID reads the owner's drafts ids for a
// message to send, failing unless every one is a draft. A comment the
// claim of one of heldBy's rows holds counts as a draft: it is the same
// message's, sent again, and the retire of that row returns it before the
// new claim takes it (RetireUndeliveredUserRow).
func (s *Store) ListDraftDiffReviewCommentsByID(owner DiffReviewOwner, scope, sourceKey string, ids []string, heldBy DiffReviewMessageRows) ([]DiffReviewComment, error) {
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
	held, err := json.Marshal(uniqueNonEmptyStrings(heldBy.ItemIDs))
	if err != nil {
		return nil, fmt.Errorf("store: encode the rows holding diff review comments: %w", err)
	}
	column, value := owner.column()
	query := `SELECT ` + diffReviewCommentColumns + `
	            FROM diff_review_comments
	           WHERE ` + column + ` = ?
	             AND scope = ?
	             AND source_key = ?
	             AND (status = 'draft'
	                  OR (status = 'sent' AND claim_item_id <> '' AND claim_thread_id = ` + boundText + `
	                      AND claim_item_id IN (SELECT value FROM json_each(?))))
	             AND id IN (` + placeholders(len(wanted)) + `)
	           ORDER BY file_path ASC, old_line ASC, new_line ASC, created_at ASC`
	args := make([]any, 0, 5+len(wanted))
	args = append(args, value, scope, sourceKey, heldBy.ThreadID, string(held))
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
