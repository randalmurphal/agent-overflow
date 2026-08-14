package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// ListProposedPlanComments returns inline comments for one immutable plan
// version. Resolved comments stay persisted and visible on older versions so
// review history does not disappear when the agent proposes a revision.
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

func (a *App) CreateProposedPlanComment(threadID string, input store.ProposedPlanCommentInput) (store.ProposedPlanComment, error) {
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
	return created, nil
}

func (a *App) UpdateProposedPlanComment(threadID, commentID string, input store.ProposedPlanCommentUpdate) (store.ProposedPlanComment, error) {
	updated, err := a.store.UpdateProposedPlanComment(threadID, commentID, input, time.Now().UnixMilli())
	if err != nil {
		return store.ProposedPlanComment{}, fmt.Errorf("update proposed plan comment: %w", err)
	}
	if err := a.emitProposedPlanUpsert(threadID, updated.PlanItemID); err != nil {
		log.Printf("update proposed plan comment: emit plan upsert: %v", err)
	}
	return updated, nil
}

func (a *App) DeleteProposedPlanComment(threadID, commentID string) error {
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
	return nil
}

// SendPlanRevisionComments sends only the selected draft comments back to the
// agent. The same thread already contains the plan body, so this deliberately
// avoids stuffing the full plan text into the prompt.
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

	if _, err := a.sendMessageWithOptions(threadID, "", sendMessageOptions{
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
		a.emit("provider:item_event", triage.NewItemStreamUpsert(plan))
		return nil
	}
	return fmt.Errorf("proposed plan %s not found on thread %s", planItemID, threadID)
}
