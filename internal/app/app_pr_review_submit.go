package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// --- PR review submit idempotency ---
//
// A connection that drops after the forge accepted a review looks, from
// the pane, like one that dropped before: the call never answers and the
// person submits again. So a submit carries a client-minted id the pane
// keeps for its draft set until an answer arrives
// (frontend/src/lib/stores/reviewPane.svelte.ts), and the review posts
// the drafts the store holds, never text the caller sends. When the forge
// accepts the review, the drafts it carried are marked sent and the answer
// is recorded under the id in one store transaction
// (store.RecordPRReviewSubmit); a repeat of the id answers that record and
// posts nothing. A submit the forge refused records nothing, so its retry
// posts once. Attempts with one id serialize, so a retry that arrives
// while the first is still posting waits for its record.

// SubmitPRReviewInput is one review submit.
type SubmitPRReviewInput struct {
	// SubmitID names this submit; a repeat answers its recorded outcome.
	SubmitID string `json:"submitId"`
	Verdict  string `json:"verdict"`
	Body     string `json:"body"`
	// Drafts are the stored drafts the review posts as its comments.
	Drafts SubmitPRReviewDrafts `json:"drafts"`
}

// SubmitPRReviewDrafts selects the drafts a review posts: a thread's own
// (ThreadID set) or the PR's (ThreadID empty), all of pr scope.
type SubmitPRReviewDrafts struct {
	ThreadID   string   `json:"threadId,omitempty"`
	Scope      string   `json:"scope"`
	SourceKey  string   `json:"sourceKey"`
	CommentIDs []string `json:"commentIds"`
	// HeadSHA is the PR head the drafts were written against; a thread's
	// sent drafts record it (`pr:<head>`).
	HeadSHA string `json:"headSha,omitempty"`
}

// SubmitPRReview posts a review of pr with the selected drafts as its
// comments, marks the drafts it posted sent and answers what landed. A
// repeat of input.SubmitID answers the first submit's outcome.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SubmitPRReview(ctx context.Context, pr gitops.PRReference, input SubmitPRReviewInput) (SubmitPRReviewResult, error) {
	if a.shuttingDown.Load() {
		return SubmitPRReviewResult{}, ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return SubmitPRReviewResult{}, err
	}
	if err := store.ValidatePRReviewSubmitID(input.SubmitID); err != nil {
		return SubmitPRReviewResult{}, err
	}
	owner := store.PRReviewOwner(pr.Key())
	sentTurnID := ""
	if input.Drafts.ThreadID != "" {
		if err := a.requireScope(ctx, transport.ScopeThreadsOperate, "SubmitPRReview marks a thread's drafts sent"); err != nil {
			return SubmitPRReviewResult{}, err
		}
		owner = store.ThreadReviewOwner(input.Drafts.ThreadID)
		sentTurnID = "pr:" + input.Drafts.HeadSHA
	}
	ids := store.UniqueNonEmptyStringsForApp(input.Drafts.CommentIDs)
	if len(ids) > 0 && input.Drafts.Scope != string(store.DiffReviewScopePR) {
		return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: drafts posted as a review are pr scope, not %q", input.Drafts.Scope)
	}

	unlock, err := a.reviewSubmitLocks().LockCtx(ctx, input.SubmitID)
	if err != nil {
		return SubmitPRReviewResult{}, err
	}
	defer unlock()
	if recorded, ok, err := a.store.GetPRReviewSubmit(input.SubmitID); err != nil {
		return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: %w", err)
	} else if ok {
		return recordedPRReviewSubmit(recorded, pr, owner)
	}

	drafts, err := a.store.ListDraftDiffReviewCommentsByID(owner, input.Drafts.Scope, input.Drafts.SourceKey, ids, store.DiffReviewMessageRows{})
	if err != nil {
		return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: %w", err)
	}
	review := gitops.SubmitReviewRequest{Verdict: input.Verdict, Body: input.Body, Comments: make([]gitops.ReviewLineComment, 0, len(drafts))}
	for _, draft := range drafts {
		if comment, ok := reviewLineCommentForDraft(draft); ok {
			review.Comments = append(review.Comments, comment)
		}
	}
	posted, err := a.gitCore().SubmitReview(forgeapi.WithInteractive(ctx), pr, review)
	mapped, err := mapSubmitPRReviewResult(pr, gitops.ActionReview, posted, err)
	if err != nil {
		return mapped, gitops.ExplainForgeError(pr, gitops.ActionReview, err)
	}
	result, err := json.Marshal(mapped)
	if err != nil {
		return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: encode the outcome: %w", err)
	}
	if err := a.store.RecordPRReviewSubmit(
		store.PRReviewSubmit{SubmitID: input.SubmitID, PRKey: pr.Key(), Owner: owner, Result: result, CreatedAt: time.Now().UnixMilli()},
		store.PRReviewSubmitSent{Scope: input.Drafts.Scope, SourceKey: input.Drafts.SourceKey, CommentIDs: postedDraftIDs(drafts, mapped), SentTurnID: sentTurnID},
	); err != nil {
		return SubmitPRReviewResult{}, fmt.Errorf("the review posted, but its drafts could not be marked sent: %w", err)
	}
	if owner.ThreadID != "" && len(drafts) > 0 {
		a.announceDiffCommentsChanged(owner.ThreadID, input.Drafts.Scope, input.Drafts.SourceKey)
	}
	return mapped, nil
}

// recordedPRReviewSubmit answers a repeated submit id from its record,
// refusing one recorded for another PR or owner.
func recordedPRReviewSubmit(recorded store.PRReviewSubmit, pr gitops.PRReference, owner store.DiffReviewOwner) (SubmitPRReviewResult, error) {
	if recorded.PRKey != pr.Key() || recorded.Owner != owner {
		return SubmitPRReviewResult{}, errors.New("submit PR review: the submit id names another review")
	}
	var out SubmitPRReviewResult
	if err := json.Unmarshal(recorded.Result, &out); err != nil {
		return SubmitPRReviewResult{}, fmt.Errorf("submit PR review: read the recorded outcome: %w", err)
	}
	return out, nil
}

// postedDraftIDs are the drafts a review that landed carried. File-level
// comments post one by one after the review and stop at the first
// failure, so only the first PostedFileComments of them made it up.
func postedDraftIDs(drafts []store.DiffReviewComment, result SubmitPRReviewResult) []string {
	fileLevel := 0
	ids := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		if draft.Side == "file" {
			fileLevel++
			if result.PartialFailurePath != "" && fileLevel > result.PostedFileComments {
				continue
			}
		}
		ids = append(ids, draft.ID)
	}
	return ids
}

// reviewLineCommentForDraft is a draft as the forge's review comment: a
// file comment, or a line on the old (left) or new (right) side. A draft
// with no line to anchor posts nothing and settles with the review.
func reviewLineCommentForDraft(draft store.DiffReviewComment) (gitops.ReviewLineComment, bool) {
	line := func(n int) *int { return &n }
	switch {
	case draft.Side == "file":
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Side: "file"}, true
	case draft.Side == "old" && draft.OldLine > 0:
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Line: line(draft.OldLine), Side: "left"}, true
	case (draft.Side == "new" || draft.Side == "context") && draft.NewLine > 0:
		return gitops.ReviewLineComment{Path: draft.FilePath, Body: draft.Body, Line: line(draft.NewLine), Side: "right"}, true
	}
	return gitops.ReviewLineComment{}, false
}

func (a *App) reviewSubmitLocks() *keyedlock.Registry {
	a.reviewSubmitsOnce.Do(func() { a.reviewSubmits = keyedlock.New() })
	return a.reviewSubmits
}
