package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/diffreview"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// ReviewCommentsChangedEvent names the review-comment SET a write moved, and
// nothing else. It is a refetch nudge in the shape of the read RPC's
// arguments: PlanItemID names a proposed-plan set, Scope + SourceKey name a
// diff-review set, and exactly one of the two forms is filled.
//
// No comment rides it, because a "delete" is a DELETE-OR-RESOLVE depending on
// whether the comment was already sent — so a frame carrying one row could
// not say what the set now holds. Receivers re-read through
// ListProposedPlanComments / ListDiffReviewComments, which is also the grant
// the channel is gated on.
type ReviewCommentsChangedEvent struct {
	ThreadID   string `json:"threadId"`
	PlanItemID string `json:"planItemId,omitempty"`
	Scope      string `json:"scope,omitempty"`
	SourceKey  string `json:"sourceKey,omitempty"`
}

// announcePlanCommentsChanged tells every client holding one plan's comment
// set that it moved.
func (a *App) announcePlanCommentsChanged(threadID, planItemID string) {
	a.emit(eventchan.ReviewCommentsChanged, ReviewCommentsChangedEvent{
		ThreadID:   threadID,
		PlanItemID: planItemID,
	})
}

// announceDiffCommentsChanged is the same for one diff-review set, which is
// identified by the scope + source key pair its read RPC takes.
func (a *App) announceDiffCommentsChanged(threadID, scope, sourceKey string) {
	a.emit(eventchan.ReviewCommentsChanged, ReviewCommentsChangedEvent{
		ThreadID:  threadID,
		Scope:     scope,
		SourceKey: sourceKey,
	})
}

// ListProposedPlanComments returns inline comments for one immutable plan
// version. Resolved comments stay persisted and visible on older versions so
// review history does not disappear when the agent proposes a revision.
//
//ao:scope threads:read
func (a *App) ListProposedPlanComments(threadID, planItemID string) ([]store.ProposedPlanComment, error) {
	comments, err := a.store.ListProposedPlanComments(threadID, planItemID)
	if err != nil {
		return nil, fmt.Errorf("list proposed plan comments: %w", err)
	}
	if comments == nil {
		return []store.ProposedPlanComment{}, nil
	}
	return comments, nil
}

//ao:scope threads:operate
func (a *App) CreateProposedPlanComment(threadID string, input store.ProposedPlanCommentInput) (store.ProposedPlanComment, error) {
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return store.ProposedPlanComment{}, err
	}
	defer unlock()
	item, err := a.validateProposedPlanItem(threadID, input.PlanItemID)
	if err != nil {
		return store.ProposedPlanComment{}, err
	}
	selectedText, err := a.proposedPlanSelectedTextForRange(item, input.StartLine, input.EndLine)
	if err != nil {
		return store.ProposedPlanComment{}, err
	}
	now := time.Now().UnixMilli()
	comment := store.ProposedPlanComment{
		ID:           uuid.NewString(),
		ThreadID:     threadID,
		PlanItemID:   input.PlanItemID,
		StartLine:    input.StartLine,
		EndLine:      input.EndLine,
		SelectedText: selectedText,
		Body:         input.Body,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	created, err := a.store.CreateProposedPlanComment(comment)
	if err != nil {
		return store.ProposedPlanComment{}, fmt.Errorf("create proposed plan comment: %w", err)
	}
	if err := a.emitProposedPlanUpsert(threadID, input.PlanItemID); err != nil {
		// The comment is already persisted; failing to refresh aggregate chrome
		// should not make the edit look rejected.
		log.Printf("create proposed plan comment: emit plan upsert: %v", err)
	}
	a.announcePlanCommentsChanged(threadID, input.PlanItemID)
	return created, nil
}

//ao:scope threads:operate
func (a *App) UpdateProposedPlanComment(threadID, commentID string, input store.ProposedPlanCommentUpdate) (store.ProposedPlanComment, error) {
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return store.ProposedPlanComment{}, err
	}
	defer unlock()
	updated, err := a.store.UpdateProposedPlanComment(threadID, commentID, input, time.Now().UnixMilli())
	if err != nil {
		return store.ProposedPlanComment{}, fmt.Errorf("update proposed plan comment: %w", err)
	}
	if err := a.emitProposedPlanUpsert(threadID, updated.PlanItemID); err != nil {
		log.Printf("update proposed plan comment: emit plan upsert: %v", err)
	}
	a.announcePlanCommentsChanged(threadID, updated.PlanItemID)
	return updated, nil
}

//ao:scope threads:operate
func (a *App) DeleteProposedPlanComment(threadID, commentID string) error {
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return err
	}
	defer unlock()
	comment, err := a.store.GetProposedPlanComment(threadID, commentID)
	if err != nil {
		return fmt.Errorf("delete proposed plan comment: %w", err)
	}
	if err := a.store.DeleteOrResolveProposedPlanComment(threadID, commentID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("delete proposed plan comment: %w", err)
	}
	if err := a.emitProposedPlanUpsert(threadID, comment.PlanItemID); err != nil {
		log.Printf("delete proposed plan comment: emit plan upsert: %v", err)
	}
	a.announcePlanCommentsChanged(threadID, comment.PlanItemID)
	return nil
}

// SendPlanRevisionComments sends only the selected draft comments back to the
// agent. The same thread already contains the plan body, so this deliberately
// avoids stuffing the full plan text into the prompt.
//
//ao:scope threads:operate
func (a *App) SendPlanRevisionComments(threadID, planItemID string, commentIDs []string) (store.Thread, error) {
	if len(store.UniqueNonEmptyStringsForApp(commentIDs)) > store.MaxProposedPlanRevisionCommentIDs {
		return store.Thread{}, fmt.Errorf("send plan revision comments: too many comments selected")
	}
	if _, err := a.validateProposedPlanItem(threadID, planItemID); err != nil {
		return store.Thread{}, err
	}
	comments, err := a.store.ListDraftProposedPlanCommentsByID(threadID, planItemID, commentIDs)
	if err != nil {
		return store.Thread{}, fmt.Errorf("send plan revision comments: %w", err)
	}
	if len(comments) == 0 {
		return store.Thread{}, fmt.Errorf("send plan revision comments: no draft comments selected")
	}

	if thread, err := a.store.GetThread(threadID); err != nil {
		return store.Thread{}, fmt.Errorf("send plan revision comments: %w", err)
	} else if provider.NormalizeInteractionMode(thread.Mode) != provider.ModePlan {
		if _, err := a.UpdateThreadMode(threadID, string(provider.ModePlan)); err != nil {
			return store.Thread{}, fmt.Errorf("send plan revision comments: switch to plan mode: %w", err)
		}
	}

	if _, err := a.sendMessageWithOptions(context.Background(), threadID, "", sendMessageOptions{
		RevisionSourceProposedPlan: &SourceProposedPlan{
			ThreadID: threadID,
			ItemID:   planItemID,
		},
		RevisionSourceCommentIDs: commentIDs,
	}); err != nil {
		return store.Thread{}, err
	}
	if err := a.emitProposedPlanUpsert(threadID, planItemID); err != nil {
		log.Printf("send plan revision comments: emit plan upsert: %v", err)
	}
	return a.store.GetThread(threadID)
}

func (a *App) validateProposedPlanItem(threadID, planItemID string) (store.Item, error) {
	item, found, err := a.store.GetThreadItem(threadID, planItemID)
	if err != nil {
		return store.Item{}, fmt.Errorf("load proposed plan item: %w", err)
	}
	if !found {
		return store.Item{}, fmt.Errorf("proposed plan %s not found", planItemID)
	}
	if item.PayloadKind != "proposed_plan" || item.PayloadID == "" || item.Role != "assistant" {
		return store.Item{}, fmt.Errorf("item %s is not an assistant proposed plan", planItemID)
	}
	if _, err := a.store.EnsureProposedPlanState(threadID, planItemID, time.Now().UnixMilli()); err != nil {
		return store.Item{}, fmt.Errorf("ensure proposed plan state: %w", err)
	}
	// Same reason as resolveSourceProposedPlan: the ensure can create the
	// proposed_plans row hasActionableProposedPlan is derived from, and the
	// sidebar pill has no other source for it.
	a.broadcastThreadRowByID(threadID)
	return item, nil
}

func (a *App) proposedPlanSelectedTextForRange(item store.Item, startLine, endLine int) (string, error) {
	if err := store.ValidateProposedPlanCommentRangeForApp(startLine, endLine); err != nil {
		return "", err
	}
	data, err := a.store.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		return "", fmt.Errorf("load proposed plan payload: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if endLine > len(lines) {
		return "", fmt.Errorf("comment line range %d-%d exceeds plan length %d", startLine, endLine, len(lines))
	}
	selected := strings.TrimSpace(strings.Join(lines[startLine-1:endLine], "\n"))
	if len(selected) > store.MaxProposedPlanSelectedTextBytes {
		return "", fmt.Errorf("selected plan text is too large")
	}
	return selected, nil
}

func (a *App) emitProposedPlanUpsert(threadID, planItemID string) error {
	plan, found, err := a.store.GetThreadProposedPlanItem(threadID, planItemID)
	if err != nil {
		return err
	}
	if found {
		a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(plan))
		return nil
	}
	return fmt.Errorf("proposed plan %s not found on thread %s", planItemID, threadID)
}

//ao:scope threads:read
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

//ao:scope threads:operate
func (a *App) CreateDiffReviewComment(threadID string, input store.DiffReviewCommentInput) (store.DiffReviewComment, error) {
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return store.DiffReviewComment{}, err
	}
	defer unlock()
	now := time.Now().UnixMilli()
	comment := store.DiffReviewComment{
		ID:           uuid.NewString(),
		ThreadID:     threadID,
		Scope:        input.Scope,
		SourceKey:    input.SourceKey,
		CommitSHA:    input.CommitSHA,
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
	a.announceDiffCommentsChanged(threadID, created.Scope, created.SourceKey)
	return created, nil
}

//ao:scope threads:operate
func (a *App) UpdateDiffReviewComment(threadID, commentID string, input store.DiffReviewCommentUpdate) (store.DiffReviewComment, error) {
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return store.DiffReviewComment{}, err
	}
	defer unlock()
	updated, err := a.store.UpdateDiffReviewComment(threadID, commentID, input, time.Now().UnixMilli())
	if err != nil {
		return store.DiffReviewComment{}, fmt.Errorf("update diff review comment: %w", err)
	}
	a.announceDiffCommentsChanged(threadID, updated.Scope, updated.SourceKey)
	return updated, nil
}

//ao:scope threads:operate
func (a *App) DeleteDiffReviewComment(threadID, commentID string) error {
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return err
	}
	defer unlock()
	// Read the row before it moves: a delete names only the comment, and the
	// set a receiver has to re-read is identified by scope + source key.
	comment, err := a.store.GetDiffReviewComment(threadID, commentID)
	if err != nil {
		return fmt.Errorf("delete diff review comment: %w", err)
	}
	if err := a.store.DeleteOrResolveDiffReviewComment(threadID, commentID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("delete diff review comment: %w", err)
	}
	a.announceDiffCommentsChanged(threadID, comment.Scope, comment.SourceKey)
	return nil
}

//ao:scope threads:operate
func (a *App) MarkDiffReviewCommentsSent(threadID, scope, sourceKey string, commentIDs []string, sentTurnID string) error {
	if err := a.store.MarkDiffReviewCommentsSent(threadID, scope, sourceKey, commentIDs, time.Now().UnixMilli(), sentTurnID); err != nil {
		return fmt.Errorf("mark diff review comments sent: %w", err)
	}
	// Sending is a set change like any other: a draft that became sent renders
	// differently, and only a re-read can say which ones moved.
	a.announceDiffCommentsChanged(threadID, scope, sourceKey)
	return nil
}

type SendDiffReviewCommentsInput struct {
	PR *store.DiffReviewPRContext `json:"pr,omitempty"`
}

//ao:scope threads:operate
func (a *App) SendDiffReviewComments(threadID, scope, sourceKey string, commentIDs []string, input SendDiffReviewCommentsInput) (store.Thread, error) {
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
	if _, err := a.sendMessageWithOptions(context.Background(), threadID, "", sendMessageOptions{
		RevisionSourceDiffReview:     &SourceDiffReview{ThreadID: threadID, Scope: scope, SourceKey: sourceKey, PR: input.PR},
		RevisionSourceDiffCommentIDs: commentIDs,
	}); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(threadID)
}

func (a *App) appendDiffReviewCommentsToContent(threadID, content, scope, sourceKey string, commentIDs []string, pr *store.DiffReviewPRContext) (string, []string, error) {
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
	var prompt string
	if scope == string(store.DiffReviewScopePR) {
		prompt = diffreview.BuildPromptWithPRContext(comments, pr)
	} else {
		prompt = diffreview.BuildPrompt(comments)
	}
	if len(prompt) > store.MaxDiffReviewPromptBytes {
		return "", nil, fmt.Errorf("diff review comments are too large")
	}
	ids := diffreview.IDsOf(comments)
	if strings.TrimSpace(content) == "" {
		return prompt, ids, nil
	}
	return strings.TrimSpace(content) + "\n\n" + prompt, ids, nil
}
