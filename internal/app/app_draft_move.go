package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"agent-overflow/internal/store"
)

// MoveDraftToThread moves a saved composer into an empty destination on this
// computer. The source's workspace and ordinary empty-draft policy are retained.
//
//ao:scope threads:operate
func (a *App) MoveDraftToThread(ctx context.Context, threadID, destinationThreadID string, snapshot DraftSnapshot) (result Draft, err error) {
	if threadID == destinationThreadID || threadID == "" || destinationThreadID == "" {
		return result, errors.New("Choose a different draft destination.")
	}
	ids := []string{threadID, destinationThreadID}
	slices.Sort(ids)
	for _, id := range ids {
		unlock, lockErr := a.threadLocks().LockCtx(ctx, id)
		if lockErr != nil {
			return result, lockErr
		}
		defer unlock()
	}
	for _, id := range ids {
		unlock, lockErr := a.threadApplication().LockMutable(ctx, id)
		if lockErr != nil {
			return result, lockErr
		}
		defer unlock()
		if a.hasActiveSession(id) {
			return result, errors.New("Wait for the draft's current operation to finish.")
		}
		if err := a.store.CheckUnsentDraft(id); err != nil {
			return result, err
		}
	}
	sourceThread, err := a.store.GetThread(threadID)
	if err != nil {
		return result, err
	}
	targetThread, err := a.store.GetThread(destinationThreadID)
	if err != nil {
		return result, err
	}
	if sourceThread.Provider != targetThread.Provider || sourceThread.Model != targetThread.Model ||
		sourceThread.Mode != targetThread.Mode || sourceThread.ReasoningEffort != targetThread.ReasoningEffort ||
		sourceThread.FastMode != targetThread.FastMode || sourceThread.ContextWindow != targetThread.ContextWindow ||
		sourceThread.RuntimeMode != targetThread.RuntimeMode ||
		sourceThread.AutoCompactStandardPercent != targetThread.AutoCompactStandardPercent ||
		sourceThread.AutoCompactExtendedPercent != targetThread.AutoCompactExtendedPercent {
		return result, errors.New("The draft's settings changed while switching projects. Try again.")
	}
	expected, err := encodeThreadDraft(threadID, snapshot)
	if err != nil {
		return result, err
	}
	current, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return result, err
	}
	current.UpdatedAt, expected.UpdatedAt = 0, 0
	if current != expected {
		return result, errors.New("The draft changed while switching projects. Try again.")
	}
	cloned := make([]string, 0, len(snapshot.AttachmentIDs))
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, id := range cloned {
			if cleanupErr := a.attachments.Delete(destinationThreadID, id); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("clean up copied attachment: %w", cleanupErr))
			}
		}
	}()
	for _, id := range snapshot.AttachmentIDs {
		if a.attachments == nil {
			return result, errors.New("Attachment storage is unavailable.")
		}
		attachment, copyErr := a.attachments.CopyToThread(threadID, destinationThreadID, id, time.Now().UnixMilli())
		if copyErr != nil {
			return result, copyErr
		}
		cloned = append(cloned, attachment.ID)
	}
	originalAttachments := snapshot.AttachmentIDs
	snapshot.AttachmentIDs = cloned
	destination, err := encodeThreadDraft(destinationThreadID, snapshot)
	if err != nil {
		return result, err
	}
	if err = a.writeMovedThreadDraft(clientOf(ctx), expected, destination); err != nil {
		return result, err
	}
	committed = true
	for _, id := range originalAttachments {
		if cleanupErr := a.attachments.Delete(threadID, id); cleanupErr != nil {
			log.Printf("draft move: clean up source attachment %s: %v", id, cleanupErr)
		}
	}
	return a.GetDraft(destinationThreadID)
}

// A completed remote move may race with new work in the emptied source.
// Release only attachments that still belong to an unsent, unreferencing draft.
func (a *App) cleanupMovedDraftAttachments(moved store.ThreadDraft) error {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), moved.ThreadID)
	if err != nil {
		return err
	}
	defer unlock()
	unlockMutations, err := a.threadApplication().LockMutable(a.lifeCtx(), moved.ThreadID)
	if err != nil {
		return err
	}
	defer unlockMutations()
	if err := a.store.CheckUnsentDraft(moved.ThreadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotUnsentDraft) {
			return nil
		}
		return err
	}
	current, _, err := a.store.GetThreadDraft(moved.ThreadID)
	if err != nil {
		return err
	}
	var previous, retained []string
	if err := json.Unmarshal([]byte(moved.Attachments), &previous); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(current.Attachments), &retained); err != nil {
		return err
	}
	if len(previous) == 0 {
		return nil
	}
	if a.attachments == nil {
		return errors.New("Attachment storage is unavailable.")
	}
	owned, err := a.attachments.List(moved.ThreadID)
	if err != nil {
		return err
	}
	for _, attachment := range owned {
		if slices.Contains(previous, attachment.ID) && !slices.Contains(retained, attachment.ID) {
			if err := a.attachments.Delete(moved.ThreadID, attachment.ID); err != nil {
				return fmt.Errorf("clean up moved draft attachment: %w", err)
			}
		}
	}
	return nil
}
