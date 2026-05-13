package main

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/diffreview"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

func (a *App) ListDiffReviewComments(threadID, scope, sourceKey string) ([]store.DiffReviewComment, error) {
	comments, err := a.store.ListDiffReviewComments(threadID, scope, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("list diff review comments: %w", err)
	}
	if comments == nil {
		return []store.DiffReviewComment{}, nil
	}
	return comments, nil
}

func (a *App) CreateDiffReviewComment(threadID string, input store.DiffReviewCommentInput) (store.DiffReviewComment, error) {
	now := time.Now().UnixMilli()
	comment := store.DiffReviewComment{
		ID:           uuid.NewString(),
		ThreadID:     threadID,
		Scope:        input.Scope,
		SourceKey:    input.SourceKey,
		FilePath:     input.FilePath,
		OldLine:      input.OldLine,
		NewLine:      input.NewLine,
		Side:         input.Side,
		SelectedText: input.SelectedText,
		Body:         input.Body,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	created, err := a.store.CreateDiffReviewComment(comment)
	if err != nil {
		return store.DiffReviewComment{}, fmt.Errorf("create diff review comment: %w", err)
	}
	return created, nil
}

func (a *App) UpdateDiffReviewComment(threadID, commentID string, input store.DiffReviewCommentUpdate) (store.DiffReviewComment, error) {
	updated, err := a.store.UpdateDiffReviewComment(threadID, commentID, input, time.Now().UnixMilli())
	if err != nil {
		return store.DiffReviewComment{}, fmt.Errorf("update diff review comment: %w", err)
	}
	return updated, nil
}

func (a *App) DeleteDiffReviewComment(threadID, commentID string) error {
	if err := a.store.DeleteOrResolveDiffReviewComment(threadID, commentID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("delete diff review comment: %w", err)
	}
	return nil
}

func (a *App) SendDiffReviewComments(threadID, scope, sourceKey string, commentIDs []string) (store.Thread, error) {
	scope, err := store.NormalizeDiffReviewScope(scope)
	if err != nil {
		return store.Thread{}, err
	}
	sourceKey, err = store.NormalizeDiffReviewSourceKey(sourceKey)
	if err != nil {
		return store.Thread{}, err
	}
	if len(store.UniqueNonEmptyStringsForApp(commentIDs)) > store.MaxDiffReviewCommentIDs {
		return store.Thread{}, fmt.Errorf("send diff review comments: too many comments selected")
	}
	if _, err := a.sendMessageWithOptions(threadID, "", sendMessageOptions{
		RevisionSourceDiffReview:     &SourceDiffReview{ThreadID: threadID, Scope: scope, SourceKey: sourceKey},
		RevisionSourceDiffCommentIDs: commentIDs,
	}); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(threadID)
}

func (a *App) appendDiffReviewCommentsToContent(threadID, content, scope, sourceKey string, commentIDs []string) (string, []string, error) {
	if len(store.UniqueNonEmptyStringsForApp(commentIDs)) > store.MaxDiffReviewCommentIDs {
		return "", nil, fmt.Errorf("too many diff review comments selected")
	}
	comments, err := a.store.ListDraftDiffReviewCommentsByID(threadID, scope, sourceKey, commentIDs)
	if err != nil {
		return "", nil, err
	}
	if len(comments) == 0 {
		return "", nil, fmt.Errorf("no draft diff review comments selected")
	}
	prompt := diffreview.BuildPrompt(comments)
	if len(prompt) > store.MaxDiffReviewPromptBytes {
		return "", nil, fmt.Errorf("diff review comments are too large")
	}
	ids := diffreview.IDsOf(comments)
	if strings.TrimSpace(content) == "" {
		return prompt, ids, nil
	}
	return strings.TrimSpace(content) + "\n\n" + prompt, ids, nil
}
