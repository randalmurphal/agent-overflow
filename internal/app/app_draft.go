package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/usermessage"
)

// TerminalChip is the frontend-owned shape of a "terminal context" snippet
// captured from the terminal drawer. The Go side treats these as opaque —
// we store and echo them back intact.
type TerminalChip struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Preview   string `json:"preview"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
}

// Draft is the composer draft state. AttachmentIDs reference rows the
// attachment upload route inserted; terminal chips are snippets captured
// from the terminal drawer. SourceProposedPlan, when non-nil, links the draft to a
// proposed plan in another thread — set by "Implement plan in new thread"
// so the eventual send carries the linkage that marks the original plan
// Accepted.
type Draft struct {
	ThreadID           string              `json:"threadId"`
	Content            string              `json:"content"`
	AttachmentIDs      []string            `json:"attachmentIds"`
	TerminalChips      []TerminalChip      `json:"terminalChips"`
	SourceProposedPlan *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	UpdatedAt          int64               `json:"updatedAt"`
}

// SaveDraft replaces the draft row for a thread.
//
// ctx carries the calling screen's identity so the broadcast can name it and
// so that screen can recognize the echo of its own save. The generated TS
// bindings strip a leading ctx parameter, so the wire signature is unchanged.
//
//ao:scope threads:operate
func (a *App) SaveDraft(ctx context.Context, threadID string, content string, attachmentIDs []string, terminalChips []TerminalChip, sourceProposedPlan *SourceProposedPlan) error {
	if a.store == nil {
		return fmt.Errorf("draft store not initialized")
	}
	attachmentsJSON, err := json.Marshal(slicesx.OrEmpty(attachmentIDs))
	if err != nil {
		return fmt.Errorf("save draft: encode attachment ids: %w", err)
	}
	chipsJSON, err := json.Marshal(slicesx.OrEmpty(terminalChips))
	if err != nil {
		return fmt.Errorf("save draft: encode terminal chips: %w", err)
	}
	sourcePlanJSON, err := usermessage.EncodeDraftSource(sourceProposedPlan)
	if err != nil {
		return fmt.Errorf("save draft: encode source proposed plan: %w", err)
	}
	return a.writeThreadDraft(clientOf(ctx), store.ThreadDraft{
		ThreadID:                  threadID,
		Content:                   content,
		Attachments:               string(attachmentsJSON),
		TerminalChips:             string(chipsJSON),
		PendingPlanImplementation: sourcePlanJSON,
		UpdatedAt:                 time.Now().UnixMilli(),
	})
}

// GetDraft returns the draft for a thread. Missing drafts return a zero
// Draft (with empty arrays) rather than an error.
//
//ao:scope threads:operate
func (a *App) GetDraft(threadID string) (Draft, error) {
	if a.store == nil {
		return Draft{}, fmt.Errorf("draft store not initialized")
	}
	row, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return Draft{}, err
	}

	draft := Draft{
		ThreadID:      row.ThreadID,
		Content:       row.Content,
		AttachmentIDs: []string{},
		TerminalChips: []TerminalChip{},
		UpdatedAt:     row.UpdatedAt,
	}
	if row.Attachments != "" {
		if err := json.Unmarshal([]byte(row.Attachments), &draft.AttachmentIDs); err != nil {
			return Draft{}, fmt.Errorf("get draft: decode attachment ids: %w", err)
		}
	}
	if draft.AttachmentIDs == nil {
		draft.AttachmentIDs = []string{}
	}
	if row.TerminalChips != "" {
		if err := json.Unmarshal([]byte(row.TerminalChips), &draft.TerminalChips); err != nil {
			return Draft{}, fmt.Errorf("get draft: decode terminal chips: %w", err)
		}
	}
	if draft.TerminalChips == nil {
		draft.TerminalChips = []TerminalChip{}
	}
	if row.PendingPlanImplementation != "" {
		var src SourceProposedPlan
		if err := json.Unmarshal([]byte(row.PendingPlanImplementation), &src); err != nil {
			return Draft{}, fmt.Errorf("get draft: decode source proposed plan: %w", err)
		}
		draft.SourceProposedPlan = &src
	}
	return draft, nil
}

// ClearDraft deletes any stored draft for a thread. Missing rows are not
// treated as an error because the caller just wants the thread to have no
// draft.
//
//ao:scope threads:operate
func (a *App) ClearDraft(ctx context.Context, threadID string) error {
	if a.store == nil {
		return fmt.Errorf("draft store not initialized")
	}
	return a.removeThreadDraft(clientOf(ctx), threadID)
}

// DeleteEmptyDraftThread removes a materialized chat/plan draft row after the
// composer is cleared. It returns false when the thread has gained durable
// state or is currently active.
//
//ao:scope threads:operate
func (a *App) DeleteEmptyDraftThread(threadID string) (bool, error) {
	if a.store == nil {
		return false, fmt.Errorf("draft store not initialized")
	}
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if a.hasActiveSession(threadID) {
		return false, nil
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("delete empty draft thread %s: load thread: %w", threadID, err)
	}
	empty, err := a.store.IsEmptyDraftThread(threadID)
	if err != nil {
		return false, err
	}
	if !empty {
		return false, nil
	}

	var errs []error
	if err := a.stopSession(threadID); err != nil {
		errs = append(errs, fmt.Errorf("stop session: %w", err))
	}
	if a.terminals != nil {
		if err := a.terminals.CloseThread(threadID); err != nil {
			errs = append(errs, fmt.Errorf("close terminals: %w", err))
		}
	}
	a.clearThreadSystemPrompt(threadID)
	a.removeDeliberation(thread)
	a.clearAutoReconnectAttempted(threadID)
	if err := a.cleanupThreadAttachmentFiles(threadID); err != nil {
		errs = append(errs, fmt.Errorf("cleanup attachments: %w", err))
	}
	if a.replay != nil {
		if err := a.replay.RemoveThreadLog(threadID); err != nil {
			errs = append(errs, fmt.Errorf("cleanup replay log: %w", err))
		}
	}
	if len(errs) > 0 {
		return false, fmt.Errorf("delete empty draft thread %s: %w", threadID, errors.Join(errs...))
	}

	deleted, err := a.store.DeleteEmptyDraftThread(threadID)
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// stagedThreadDraft is the before/after pair one merge-and-upsert wrote.
// A caller parking transient saga state in the draft row
// (RevertConversationAndResendMessage) needs both: prior to put the
// user's untouched work-in-progress back afterwards, and merged to
// prove the row it is about to overwrite is still its own staged copy
// and not a composer save that landed while the saga ran.
type stagedThreadDraft struct {
	merged store.ThreadDraft
	// prior is the row as it was BEFORE the merge; priorExisted
	// distinguishes an empty composer from a persisted empty draft, which
	// settle back differently (delete vs restore).
	prior        store.ThreadDraft
	priorExisted bool
}

// mergeAndUpsertThreadDraft merges parts AHEAD of the thread's current
// composer draft and persists the result. The order is chronological —
// the parts were typed before whatever is sitting in the composer now —
// and composerdraft.MergeParts carries the current row's terminal chips
// and pending-plan link through untouched, so a merge round-trip never
// costs the user composer context.
//
// Callers that only ever ADD to the draft (the flush-queue restore
// paths) discard the result.
//
// The write is attributed to nobody: every caller is a backend saga (a revert
// parking the user's work, a flush-queue restore putting an undelivered
// message back), so there is no screen to credit and every client applies the
// frame.
func (a *App) mergeAndUpsertThreadDraft(threadID string, parts []composerdraft.Part) (stagedThreadDraft, error) {
	current, existed, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return stagedThreadDraft{}, err
	}
	merged, err := composerdraft.MergeParts(threadID, current, parts, time.Now().UnixMilli())
	if err != nil {
		return stagedThreadDraft{}, err
	}
	if err := a.writeThreadDraft(transport.ClientIdentity{}, merged); err != nil {
		return stagedThreadDraft{}, err
	}
	return stagedThreadDraft{merged: merged, prior: current, priorExisted: existed}, nil
}

func (a *App) composerDraftFromUserItemWithClonedAttachments(threadID string, userItem store.Item, updatedAt int64) (store.ThreadDraft, error) {
	meta, err := usermessage.FromItem(userItem)
	if err != nil {
		return store.ThreadDraft{}, err
	}
	attachmentIDs, err := a.cloneUserMessageAttachmentsForDraft(threadID, userItem.ThreadID, meta.Attachments, updatedAt)
	if err != nil {
		return store.ThreadDraft{}, err
	}
	return composerdraft.FromParts(threadID, userItem.Summary, attachmentIDs, meta.SourceProposedPlan, updatedAt)
}

func (a *App) cloneUserMessageAttachmentsForDraft(
	targetThreadID string,
	sourceThreadID string,
	attachments []userMessageAttachmentMeta,
	createdAt int64,
) ([]string, error) {
	if len(attachments) == 0 {
		return []string{}, nil
	}
	if a.attachments == nil {
		return nil, fmt.Errorf("attachment store not initialized")
	}
	if strings.TrimSpace(sourceThreadID) == "" {
		sourceThreadID = targetThreadID
	}
	clonedIDs := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		sourceAttachmentID := strings.TrimSpace(attachment.ID)
		if sourceAttachmentID == "" {
			continue
		}
		if sourceThreadID == targetThreadID {
			clonedIDs = append(clonedIDs, sourceAttachmentID)
			continue
		}
		// Copied on disk, not round-tripped through base64 Upload: the
		// store already accepted these bytes, so re-validating them would
		// re-DERIVE a kind the original write settled — and a 50 MiB file
		// would sit in memory twice on its way to the same place.
		cloned, err := a.attachments.CopyToThread(sourceThreadID, targetThreadID, sourceAttachmentID, createdAt)
		if err != nil {
			return nil, fmt.Errorf("clone draft attachment %s: %w", sourceAttachmentID, err)
		}
		clonedIDs = append(clonedIDs, cloned.ID)
	}
	return clonedIDs, nil
}
