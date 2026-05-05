package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/diffsummary"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

const (
	RevertModeConversationAndFiles = "conversation-and-files"
	RevertModeConversationOnly     = "conversation-only"
)

type CheckpointView struct {
	ID                    string             `json:"id"`
	ThreadID              string             `json:"threadId"`
	UserItemID            string             `json:"userItemId"`
	TurnIndex             int                `json:"turnIndex"`
	ProviderUserMessageID string             `json:"providerUserMessageId,omitempty"`
	Status                string             `json:"status"`
	Files                 []diffsummary.File `json:"files"`
	CapturedAt            int64              `json:"capturedAt"`
}

func (a *App) captureMessageCheckpoint(thread store.Thread, userItem store.Item) {
	cap := a.checkpointStore()
	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" {
		return
	}
	ctx := context.Background()
	if !cap.IsGitRepository(ctx, workspace) {
		a.emit("checkpoint:unavailable", map[string]any{
			"threadId": thread.ID,
			"reason":   "not-a-git-repo",
		})
		return
	}

	ref := checkpoint.ThreadRefPrefix(thread.ID) + "message/" + uuid.NewString()
	if err := cap.CaptureRef(ctx, workspace, ref); err != nil {
		a.emit("checkpoint:error", map[string]any{
			"threadId":   thread.ID,
			"userItemId": userItem.ID,
			"turnIndex":  userItem.TurnIndex,
			"error":      err.Error(),
		})
		return
	}

	var files []diffsummary.File
	if prev, ok, err := a.store.GetPreviousCheckpoint(thread.ID, userItem.TurnIndex); err != nil {
		log.Printf("checkpoint: previous checkpoint lookup failed thread=%s user_item=%s: %v", thread.ID, userItem.ID, err)
		a.emit("checkpoint:error", map[string]any{
			"threadId":   thread.ID,
			"userItemId": userItem.ID,
			"turnIndex":  userItem.TurnIndex,
			"error":      err.Error(),
		})
	} else if ok {
		if prev.WorkspacePath == workspace {
			var diffErr error
			files, diffErr = cap.DiffRefToRefSummary(ctx, workspace, prev.RefName, ref)
			if diffErr != nil {
				log.Printf("checkpoint: diff summary failed thread=%s user_item=%s: %v", thread.ID, userItem.ID, diffErr)
				a.emit("checkpoint:error", map[string]any{
					"threadId":   thread.ID,
					"userItemId": userItem.ID,
					"turnIndex":  userItem.TurnIndex,
					"error":      diffErr.Error(),
				})
				files = nil
			}
		}
	}

	now := time.Now().UnixMilli()
	record := store.Checkpoint{
		ID:            uuid.NewString(),
		ThreadID:      thread.ID,
		UserItemID:    userItem.ID,
		TurnIndex:     userItem.TurnIndex,
		RefName:       ref,
		Status:        "ready",
		Files:         files,
		CapturedAt:    now,
		WorkspacePath: workspace,
	}
	stale, err := a.store.ReplaceCheckpointByUserItemID(record)
	if err != nil {
		_ = cap.DeleteRef(ctx, workspace, ref)
		a.emit("checkpoint:error", map[string]any{
			"threadId":   thread.ID,
			"userItemId": userItem.ID,
			"turnIndex":  userItem.TurnIndex,
			"error":      err.Error(),
		})
		return
	}
	if stale.RefName != "" {
		if err := cap.DeleteRef(ctx, stale.WorkspacePath, stale.RefName); err != nil {
			log.Printf("checkpoint: delete stale ref failed thread=%s ref=%s: %v", thread.ID, stale.RefName, err)
			a.emit("checkpoint:error", map[string]any{
				"threadId":   thread.ID,
				"userItemId": userItem.ID,
				"turnIndex":  userItem.TurnIndex,
				"error":      err.Error(),
			})
		}
	}

	a.emit("checkpoint:captured", map[string]any{
		"threadId":   thread.ID,
		"userItemId": userItem.ID,
		"turnIndex":  userItem.TurnIndex,
		"capturedAt": now,
	})
}

func (a *App) GetMessageCheckpointDiff(threadID string, userItemID string) (string, error) {
	if _, err := a.store.GetThread(threadID); err != nil {
		return "", fmt.Errorf("get message checkpoint diff: %w", err)
	}
	target, ok, err := a.store.GetCheckpointByUserItemID(threadID, userItemID)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint diff: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("get message checkpoint diff: no checkpoint for thread %q user item %q", threadID, userItemID)
	}
	if err := validateCheckpointRecordForThread("get message checkpoint diff", threadID, target); err != nil {
		return "", err
	}
	prev, ok, err := a.store.GetPreviousCheckpoint(threadID, target.TurnIndex)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint diff: previous: %w", err)
	}
	if !ok {
		return "", nil
	}
	if err := validateCheckpointRecordForThread("get message checkpoint diff", threadID, prev); err != nil {
		return "", err
	}
	if prev.WorkspacePath != target.WorkspacePath {
		return "", fmt.Errorf("get message checkpoint diff: checkpoint workspaces differ: %q != %q", prev.WorkspacePath, target.WorkspacePath)
	}
	patch, err := a.checkpointStore().DiffRefToRef(context.Background(), prev.WorkspacePath, prev.RefName, target.RefName)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint diff: %w", err)
	}
	return string(patch), nil
}

func (a *App) GetMessageCheckpointRevertDiff(threadID string, userItemID string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint revert diff: %w", err)
	}
	target, ok, err := a.store.GetCheckpointByUserItemID(threadID, userItemID)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint revert diff: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("get message checkpoint revert diff: no checkpoint for thread %q user item %q", threadID, userItemID)
	}
	if err := validateCheckpointRecordForThread("get message checkpoint revert diff", threadID, target); err != nil {
		return "", err
	}
	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" || workspace != target.WorkspacePath {
		return "", fmt.Errorf("get message checkpoint revert diff: checkpoint workspace %q does not match thread workspace %q", target.WorkspacePath, workspace)
	}
	paths, err := a.store.ListTrackedFilesFromTurn(threadID, target.TurnIndex)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint revert diff: tracked files: %w", err)
	}
	patch, err := a.checkpointStore().DiffRefToWorktreeScoped(context.Background(), workspace, target.RefName, paths)
	if err != nil {
		return "", fmt.Errorf("get message checkpoint revert diff: %w", err)
	}
	return string(patch), nil
}

func (a *App) GetSessionAgentDiff(threadID string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	checkpoints, err := a.store.ListCheckpoints(threadID)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	if len(checkpoints) == 0 {
		return "", nil
	}
	first := checkpoints[0]
	if err := validateCheckpointRecordForThread("get session agent diff", threadID, first); err != nil {
		return "", err
	}
	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" || workspace != first.WorkspacePath {
		return "", fmt.Errorf("get session agent diff: checkpoint workspace %q does not match thread workspace %q", first.WorkspacePath, workspace)
	}
	paths, err := a.store.ListTrackedFiles(threadID)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: tracked files: %w", err)
	}
	patch, err := a.checkpointStore().DiffRefToWorktreeScoped(context.Background(), first.WorkspacePath, first.RefName, paths)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	return string(patch), nil
}

func (a *App) GetWorkspaceCurrentDiff(threadID string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get workspace current diff: %w", err)
	}
	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" {
		return "", errors.New("get workspace current diff: thread has no workspace path")
	}
	if !a.checkpointStore().IsGitRepository(context.Background(), workspace) {
		return "", nil
	}
	patch, err := a.checkpointStore().DiffWorkspaceVsHead(context.Background(), workspace)
	if err != nil {
		return "", fmt.Errorf("get workspace current diff: %w", err)
	}
	return string(patch), nil
}

func (a *App) RevertToMessageCheckpoint(threadID string, userItemID string, mode string) error {
	if strings.TrimSpace(userItemID) == "" {
		return errors.New("revert checkpoint: user item id is required")
	}
	if mode != RevertModeConversationAndFiles && mode != RevertModeConversationOnly {
		return fmt.Errorf("revert checkpoint: unknown mode %q", mode)
	}

	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	if _, active, err := a.store.GetActiveTurn(threadID); err != nil {
		return fmt.Errorf("revert checkpoint: active turn check: %w", err)
	} else if active {
		return errors.New("revert checkpoint: interrupt the current turn before reverting")
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("revert checkpoint: %w", err)
	}
	userItem, found, err := a.store.GetThreadItem(threadID, userItemID)
	if err != nil {
		return fmt.Errorf("revert checkpoint: load user item: %w", err)
	}
	if !found || userItem.Kind != "user_text" || userItem.Role != "user" {
		return fmt.Errorf("revert checkpoint: %q is not a revertable user message", userItemID)
	}
	if isWireOnlyUserItem(userItem) {
		return fmt.Errorf("revert checkpoint: %q is provider-injected context, not a user message", userItemID)
	}
	record, ok, err := a.store.GetCheckpointByUserItemID(threadID, userItemID)
	if err != nil {
		return fmt.Errorf("revert checkpoint: %w", err)
	}
	if !ok {
		return fmt.Errorf("revert checkpoint: no checkpoint for thread %q user item %q", threadID, userItemID)
	}
	if err := validateCheckpointRecordForThread("revert checkpoint", threadID, record); err != nil {
		return err
	}
	if record.TurnIndex != userItem.TurnIndex {
		return fmt.Errorf(
			"revert checkpoint: checkpoint turn index %d does not match user message turn index %d",
			record.TurnIndex,
			userItem.TurnIndex,
		)
	}
	promptDraft, err := composerDraftFromUserItem(threadID, userItem, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("revert checkpoint: build prompt draft: %w", err)
	}

	if err := a.stopSession(threadID); err != nil {
		return fmt.Errorf("revert checkpoint: stop session: %w", err)
	}
	if err := a.revertProviderConversationToMessage(thread, record); err != nil {
		return fmt.Errorf("revert checkpoint: %w", err)
	}

	workspace := checkpointWorkspaceForThread(thread)
	if mode == RevertModeConversationAndFiles {
		if workspace == "" {
			return errors.New("revert checkpoint: thread has no workspace path")
		}
		if record.WorkspacePath != workspace {
			return fmt.Errorf("revert checkpoint: checkpoint workspace %q does not match thread workspace %q", record.WorkspacePath, workspace)
		}
		paths, err := a.store.ListTrackedFilesFromTurn(threadID, userItem.TurnIndex)
		if err != nil {
			return fmt.Errorf("revert checkpoint: tracked files: %w", err)
		}
		if err := a.checkpointStore().RestoreWorktreePaths(context.Background(), workspace, record.RefName, paths); err != nil {
			return fmt.Errorf("revert checkpoint: restore paths: %w", err)
		}
		if a.workspaceFiles != nil {
			a.workspaceFiles.Invalidate(workspace)
		}
	}

	refs, err := a.store.DeleteCheckpointsFromTurn(threadID, userItem.TurnIndex)
	if err != nil {
		return fmt.Errorf("revert checkpoint: truncate checkpoints: %w", err)
	}
	if _, err := a.store.DeleteConversationFromTurn(threadID, userItem.TurnIndex); err != nil {
		return fmt.Errorf("revert checkpoint: truncate conversation: %w", err)
	}
	if err := a.deleteCheckpointRefs(context.Background(), threadID, "revert checkpoint", refs); err != nil {
		return err
	}
	if err := a.store.UpsertThreadDraft(promptDraft); err != nil {
		return fmt.Errorf("revert checkpoint: restore prompt draft: %w", err)
	}

	a.emit("checkpoint:reverted", map[string]any{
		"threadId":   threadID,
		"userItemId": userItemID,
		"turnIndex":  userItem.TurnIndex,
		"mode":       mode,
	})
	return nil
}

func (a *App) revertProviderConversationToMessage(thread store.Thread, checkpoint store.Checkpoint) error {
	switch thread.Provider {
	case string(provider.Codex):
		lastTurn, err := a.store.LastTurnIndex(thread.ID)
		if err != nil {
			return fmt.Errorf("determine last turn: %w", err)
		}
		numTurns := lastTurn - checkpoint.TurnIndex + 1
		if numTurns < 1 {
			return nil
		}
		return a.rollbackCodexThread(thread, numTurns)
	case string(provider.Claude):
		return a.revertClaudeThreadToMessage(thread, checkpoint)
	default:
		return fmt.Errorf("unsupported provider %q", thread.Provider)
	}
}

func (a *App) revertClaudeThreadToMessage(thread store.Thread, checkpoint store.Checkpoint) error {
	if checkpoint.TurnIndex == 0 {
		thread.SessionRef = ""
		thread.PendingForkRef = ""
		thread.UpdatedAt = time.Now().UnixMilli()
		return a.store.UpdateThread(thread)
	}
	sourceSessionRef := claudeSourceSessionRef(thread)
	if sourceSessionRef == "" {
		return fmt.Errorf("claude rollback: checkpoint for turn %d requires Claude session reference", checkpoint.TurnIndex)
	}
	srcPath, err := sessionfork.LocateSessionFile(sourceSessionRef, thread.WorkspacePath)
	if err != nil {
		return fmt.Errorf("locate claude session: %w", err)
	}
	newID, newPath, err := sessionfork.WriteForkFileForLastKeptTurn(srcPath, checkpoint.TurnIndex-1, "")
	if err != nil {
		return fmt.Errorf("write reverted session: %w", err)
	}
	thread.SessionRef = newID
	thread.PendingForkRef = ""
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("persist reverted claude state: %w", err)
	}
	return nil
}

func claudeSourceSessionRef(thread store.Thread) string {
	if thread.SessionRef != "" {
		return thread.SessionRef
	}
	return thread.PendingForkRef
}

func isWireOnlyUserItem(item store.Item) bool {
	var meta map[string]any
	if json.Unmarshal([]byte(item.Meta), &meta) != nil {
		return false
	}
	wireOnly, _ := meta["wire_only"].(bool)
	return wireOnly
}

func (a *App) deleteCheckpointRefs(ctx context.Context, threadID, action string, refs []store.CheckpointRef) error {
	for _, ref := range refs {
		if err := validateCheckpointRefForThread(action, threadID, ref); err != nil {
			return err
		}
		if err := a.checkpointStore().DeleteRef(ctx, ref.WorkspacePath, ref.RefName); err != nil {
			return fmt.Errorf("%s: delete stale ref %s: %w", action, ref.RefName, err)
		}
	}
	return nil
}

func validateCheckpointRecordForThread(action, threadID string, record store.Checkpoint) error {
	return validateCheckpointRefForThread(action, threadID, store.CheckpointRef{
		RefName:       record.RefName,
		WorkspacePath: record.WorkspacePath,
	})
}

func validateCheckpointRefForThread(action, threadID string, ref store.CheckpointRef) error {
	if strings.TrimSpace(ref.RefName) == "" {
		return fmt.Errorf("%s: checkpoint ref is empty", action)
	}
	if !checkpoint.IsThreadRef(ref.RefName, threadID) {
		return fmt.Errorf("%s: checkpoint ref %q is outside thread %q namespace", action, ref.RefName, threadID)
	}
	if strings.TrimSpace(ref.WorkspacePath) == "" {
		return fmt.Errorf("%s: checkpoint workspace is empty for ref %q", action, ref.RefName)
	}
	return nil
}

func (a *App) rollbackCodexThread(thread store.Thread, numTurns int) error {
	if active, ok := a.activeCodexSession(thread.ID); ok {
		return active.Rollback(context.Background(), numTurns)
	}
	if thread.SessionRef == "" {
		return fmt.Errorf("codex rollback: thread %q is missing a Codex thread reference", thread.ID)
	}
	tempSession, err := codex.NewSession(context.Background(), thread.ID, codex.Config{
		Binary:         a.providerBinaryPath(thread.Provider),
		Model:          thread.Model,
		WorkDir:        thread.WorkspacePath,
		ResumeThreadID: thread.SessionRef,
		EventLogger:    a.logger,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		return fmt.Errorf("codex rollback: resume session: %w", err)
	}
	defer tempSession.Close()
	return tempSession.Rollback(context.Background(), numTurns)
}

func (a *App) ListThreadCheckpoints(threadID string) ([]CheckpointView, error) {
	list, err := a.store.ListCheckpoints(threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread checkpoints: %w", err)
	}
	out := make([]CheckpointView, 0, len(list))
	for _, row := range list {
		out = append(out, checkpointViewFromStore(row))
	}
	return out, nil
}

func checkpointViewFromStore(row store.Checkpoint) CheckpointView {
	return CheckpointView{
		ID:                    row.ID,
		ThreadID:              row.ThreadID,
		UserItemID:            row.UserItemID,
		TurnIndex:             row.TurnIndex,
		ProviderUserMessageID: row.ProviderUserMessageID,
		Status:                row.Status,
		Files:                 row.Files,
		CapturedAt:            row.CapturedAt,
	}
}

func (a *App) checkpointStore() *checkpoint.Store {
	if a.checkpoints != nil {
		return a.checkpoints
	}
	return checkpoint.NewStore()
}

func (a *App) cleanupLegacyCheckpointRefs(st *store.Store) {
	refs, err := st.ListThreadWorkspaceRefs()
	if err != nil {
		log.Printf("checkpoint: list workspaces for legacy cleanup: %v", err)
		return
	}
	for _, ref := range refs {
		workspace := ref.WorkspacePath
		if workspace == "" {
			workspace = ref.WorktreePath
		}
		if workspace == "" || !a.checkpointStore().IsGitRepository(context.Background(), workspace) {
			continue
		}
		if err := a.checkpointStore().CleanupLegacyTurnRefs(context.Background(), workspace, ref.ID); err != nil {
			log.Printf("checkpoint: cleanup legacy refs thread=%s workspace=%s: %v", ref.ID, workspace, err)
		}
	}
}

func checkpointWorkspaceForThread(t store.Thread) string {
	return t.WorkspacePath
}
