package app

import (
	"context"
	"fmt"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// Review drafts a PR owns: the line comments the Pull requests page's
// review pane writes on a PR opened with no thread
// (store.PRReviewOwner, keyed by the computer-neutral PRReference.Key).
// They are pr scope, never a thread's, so a thread's transfer, fork and
// deletion never see them. SubmitPRReview posts them, after which
// MarkPRDiffReviewCommentsSent settles them; SendPRDiffReviewComments
// hands them to a thread instead.

// ListPRDiffReviewComments lists pr's drafts for one source.
//
//ao:scope git:operate
//ao:route selected
func (a *App) ListPRDiffReviewComments(pr gitops.PRReference, scope, sourceKey string) ([]store.DiffReviewComment, error) {
	owner, err := prReviewOwner(pr)
	if err != nil {
		return nil, err
	}
	comments, err := a.store.ListDiffReviewComments(owner, scope, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("list PR diff review comments: %w", err)
	}
	if comments == nil {
		return []store.DiffReviewComment{}, nil
	}
	return comments, nil
}

//ao:scope git:operate
//ao:route selected
func (a *App) CreatePRDiffReviewComment(pr gitops.PRReference, input store.DiffReviewCommentInput) (store.DiffReviewComment, error) {
	owner, err := prReviewOwner(pr)
	if err != nil {
		return store.DiffReviewComment{}, err
	}
	created, err := a.store.CreateDiffReviewComment(newDiffReviewComment(owner, input))
	if err != nil {
		return store.DiffReviewComment{}, fmt.Errorf("create PR diff review comment: %w", err)
	}
	return created, nil
}

//ao:scope git:operate
//ao:route selected
func (a *App) UpdatePRDiffReviewComment(pr gitops.PRReference, commentID string, update store.DiffReviewCommentUpdate) (store.DiffReviewComment, error) {
	owner, err := prReviewOwner(pr)
	if err != nil {
		return store.DiffReviewComment{}, err
	}
	updated, err := a.store.UpdateDiffReviewComment(owner, commentID, update, time.Now().UnixMilli())
	if err != nil {
		return store.DiffReviewComment{}, fmt.Errorf("update PR diff review comment: %w", err)
	}
	return updated, nil
}

// DeletePRDiffReviewComment deletes a draft, or resolves one already sent,
// as DeleteDiffReviewComment does.
//
//ao:scope git:operate
//ao:route selected
func (a *App) DeletePRDiffReviewComment(pr gitops.PRReference, commentID string) error {
	owner, err := prReviewOwner(pr)
	if err != nil {
		return err
	}
	if err := a.store.DeleteOrResolveDiffReviewComment(owner, commentID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("delete PR diff review comment: %w", err)
	}
	return nil
}

// MarkPRDiffReviewCommentsSent settles drafts a SubmitPRReview posted to
// the forge; no turn carried them.
//
//ao:scope git:operate
//ao:route selected
func (a *App) MarkPRDiffReviewCommentsSent(pr gitops.PRReference, scope, sourceKey string, commentIDs []string) error {
	owner, err := prReviewOwner(pr)
	if err != nil {
		return err
	}
	if err := a.store.MarkDiffReviewCommentsSent(owner, scope, sourceKey, commentIDs, time.Now().UnixMilli(), ""); err != nil {
		return fmt.Errorf("mark PR diff review comments sent: %w", err)
	}
	return nil
}

// SendPRDiffReviewComments appends pr's drafts commentIDs to threadID as
// SendDiffReviewComments appends a thread's own (the same prompt, with
// input's PR context) and marks them sent on the turn that carried them.
// Every id must be a draft of pr's for scope and sourceKey.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SendPRDiffReviewComments(ctx context.Context, pr gitops.PRReference, threadID, scope, sourceKey string, commentIDs []string, input SendDiffReviewCommentsInput) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsOperate, "SendPRDiffReviewComments sends to a thread"); err != nil {
		return store.Thread{}, err
	}
	if err := a.requireAutonomyForThread(ctx, threadID, ""); err != nil {
		return store.Thread{}, err
	}
	owner, err := prReviewOwner(pr)
	if err != nil {
		return store.Thread{}, err
	}
	scope, err = store.NormalizeDiffReviewScope(scope)
	if err != nil {
		return store.Thread{}, err
	}
	sourceKey, err = store.NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return store.Thread{}, err
	}
	ids := store.UniqueNonEmptyStringsForApp(commentIDs)
	if len(ids) == 0 {
		return store.Thread{}, fmt.Errorf("send PR diff review comments: no comments selected")
	}
	if len(ids) > store.MaxDiffReviewCommentIDs {
		return store.Thread{}, fmt.Errorf("send PR diff review comments: too many comments selected")
	}
	// Refuses an id that is not a draft of pr's for this source.
	if _, err := a.store.ListDraftDiffReviewCommentsByID(owner, scope, sourceKey, ids); err != nil {
		return store.Thread{}, fmt.Errorf("send PR diff review comments of %s: %w", pr.Key(), err)
	}
	// The send runs on ctx's values, not its cancel, as SendMessageWithOptions.
	if _, err := a.sendMessageWithOptions(context.WithoutCancel(ctx), threadID, "", sendMessageOptions{
		RevisionSourceDiffReview:     &SourceDiffReview{PRKey: pr.Key(), Scope: scope, SourceKey: sourceKey, PR: input.PR},
		RevisionSourceDiffCommentIDs: ids,
	}); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(threadID)
}

func prReviewOwner(pr gitops.PRReference) (store.DiffReviewOwner, error) {
	if err := pr.Validate(); err != nil {
		return store.DiffReviewOwner{}, err
	}
	return store.PRReviewOwner(pr.Key()), nil
}
