package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxPRReviewSubmitIDBytes bounds a client-minted submit id.
const MaxPRReviewSubmitIDBytes = 128

// prReviewSubmitRetention is how long a submit's outcome answers a retry.
// A retry comes from the client that lost the answer, within its session.
const prReviewSubmitRetention = 30 * 24 * time.Hour

// PRReviewSubmit is a PR review submit's recorded outcome.
type PRReviewSubmit struct {
	SubmitID string
	// PRKey is the PR the review was posted to (PRReference.Key).
	PRKey string
	// Owner owns the drafts the review posted.
	Owner DiffReviewOwner
	// Result is the submit's answer, JSON.
	Result    []byte
	CreatedAt int64
}

// PRReviewSubmitSent names the drafts a submit posted, marked sent with
// its record.
type PRReviewSubmitSent struct {
	Scope      string
	SourceKey  string
	CommentIDs []string
	SentTurnID string
}

// ValidatePRReviewSubmitID reports whether id can name a submit.
func ValidatePRReviewSubmitID(id string) error {
	if id == "" || len(id) > MaxPRReviewSubmitIDBytes || hasControlCharacter(id) {
		return fmt.Errorf("store: a PR review submit id is required, at most %d bytes of text", MaxPRReviewSubmitIDBytes)
	}
	return nil
}

// GetPRReviewSubmit reads the recorded outcome of submitID.
func (s *Store) GetPRReviewSubmit(submitID string) (PRReviewSubmit, bool, error) {
	if err := ValidatePRReviewSubmitID(submitID); err != nil {
		return PRReviewSubmit{}, false, err
	}
	var out PRReviewSubmit
	var result string
	err := s.reader().QueryRow(`SELECT submit_id, pr_key, owner_thread_id, owner_pr_key, result, created_at
	                              FROM pr_review_submits WHERE submit_id = ?`, submitID).
		Scan(&out.SubmitID, &out.PRKey, &out.Owner.ThreadID, &out.Owner.PRKey, &result, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PRReviewSubmit{}, false, nil
	}
	if err != nil {
		return PRReviewSubmit{}, false, fmt.Errorf("store: read PR review submit: %w", err)
	}
	out.Result = []byte(result)
	return out, true, nil
}

// RecordPRReviewSubmit marks the drafts a posted review carried sent and
// records its outcome in one transaction, and drops outcomes past their
// retention. A draft deleted or sent elsewhere since the review read it
// is left as it is: the forge holds the review either way. A submit id
// already recorded is refused.
func (s *Store) RecordPRReviewSubmit(submit PRReviewSubmit, sent PRReviewSubmitSent) error {
	if err := ValidatePRReviewSubmitID(submit.SubmitID); err != nil {
		return err
	}
	if err := submit.Owner.validate(); err != nil {
		return err
	}
	if submit.PRKey == "" || !json.Valid(submit.Result) || submit.CreatedAt <= 0 {
		return fmt.Errorf("store: a PR review submit needs its PR, a JSON result and its time")
	}
	ids := uniqueNonEmptyStrings(sent.CommentIDs)
	if len(ids) > MaxDiffReviewCommentIDs {
		return fmt.Errorf("store: too many diff review comments selected")
	}
	scope, sourceKey := "", ""
	if len(ids) > 0 {
		var err error
		if scope, err = NormalizeDiffReviewScope(sent.Scope); err != nil {
			return err
		}
		if sourceKey, err = NormalizeDiffReviewSourceKey(sent.SourceKey); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin PR review submit: %w", err)
	}
	// Rolls back only a write that already failed, or no-ops after Commit.
	defer func() { _ = tx.Rollback() }()
	if len(ids) > 0 {
		if _, err := markDiffReviewDraftsSentTx(tx, submit.Owner, scope, sourceKey, ids, submit.CreatedAt, sent.SentTurnID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO pr_review_submits(submit_id, pr_key, owner_thread_id, owner_pr_key, result, created_at)
	                      VALUES (?, ?, ?, ?, ?, ?)`,
		submit.SubmitID, submit.PRKey, submit.Owner.ThreadID, submit.Owner.PRKey, string(submit.Result), submit.CreatedAt); err != nil {
		return fmt.Errorf("store: record PR review submit: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM pr_review_submits WHERE created_at < ?`, submit.CreatedAt-prReviewSubmitRetention.Milliseconds()); err != nil {
		return fmt.Errorf("store: drop old PR review submits: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit PR review submit: %w", err)
	}
	return nil
}
