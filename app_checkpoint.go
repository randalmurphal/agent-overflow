package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/composerdraft"
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

// CheckpointView is the wire shape returned by ListMessageCheckpoints
// and friends. Canonical declaration (plus the field-subset projection
// rule) lives in internal/checkpoint; main keeps the alias so the
// Wails binding generator still emits it under the agent-overflow
// namespace the frontend imports from.
type CheckpointView = checkpoint.View

func (a *App) captureMessageCheckpoint(thread store.Thread, userItem store.Item) {
	cap := a.checkpointStore()
	workspace := thread.WorkspacePath
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
		a.emitCheckpointError(thread.ID, userItem.ID, userItem.TurnIndex, err)
		return
	}

	var files []diffsummary.File
	if prev, ok, err := a.store.GetPreviousCheckpoint(thread.ID, userItem.TurnIndex); err != nil {
		log.Printf("checkpoint: previous checkpoint lookup failed thread=%s user_item=%s: %v", thread.ID, userItem.ID, err)
		a.emitCheckpointError(thread.ID, userItem.ID, userItem.TurnIndex, err)
	} else if ok {
		if prev.WorkspacePath == workspace {
			var diffErr error
			files, diffErr = cap.DiffRefToRefSummary(ctx, workspace, prev.RefName, ref)
			if diffErr != nil {
				log.Printf("checkpoint: diff summary failed thread=%s user_item=%s: %v", thread.ID, userItem.ID, diffErr)
				a.emitCheckpointError(thread.ID, userItem.ID, userItem.TurnIndex, diffErr)
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
		a.emitCheckpointError(thread.ID, userItem.ID, userItem.TurnIndex, err)
		return
	}
	if stale.RefName != "" {
		if err := cap.DeleteRef(ctx, stale.WorkspacePath, stale.RefName); err != nil {
			log.Printf("checkpoint: delete stale ref failed thread=%s ref=%s: %v", thread.ID, stale.RefName, err)
			a.emitCheckpointError(thread.ID, userItem.ID, userItem.TurnIndex, err)
		}
	}

	a.emit("checkpoint:captured", map[string]any{
		"threadId":   thread.ID,
		"userItemId": userItem.ID,
		"turnIndex":  userItem.TurnIndex,
		"capturedAt": now,
	})
}

// emitCheckpointError publishes a `checkpoint:error` event with the
// canonical {threadId, userItemId, turnIndex, error} payload the
// frontend's checkpoint banner consumes. Centralises the field names
// so a future schema tweak touches one site, not five.
func (a *App) emitCheckpointError(threadID, userItemID string, turnIndex int, err error) {
	a.emit("checkpoint:error", map[string]any{
		"threadId":   threadID,
		"userItemId": userItemID,
		"turnIndex":  turnIndex,
		"error":      err.Error(),
	})
}

func (a *App) GetMessageCheckpointDiff(threadID string, userItemID string) (string, error) {
	const action = "get message checkpoint diff"
	_, target, err := a.loadCheckpointForUserItem(action, threadID, userItemID)
	if err != nil {
		return "", err
	}
	prev, ok, err := a.store.GetPreviousCheckpoint(threadID, target.TurnIndex)
	if err != nil {
		return "", fmt.Errorf("%s: previous: %w", action, err)
	}
	if !ok {
		return "", nil
	}
	if err := checkpoint.ValidateRef(action, threadID, prev.RefName, prev.WorkspacePath); err != nil {
		return "", err
	}
	if prev.WorkspacePath != target.WorkspacePath {
		return "", fmt.Errorf("%s: checkpoint workspaces differ: %q != %q", action, prev.WorkspacePath, target.WorkspacePath)
	}
	patch, err := a.checkpointStore().DiffRefToRef(context.Background(), prev.WorkspacePath, prev.RefName, target.RefName)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

func (a *App) GetMessageCheckpointRevertDiff(threadID string, userItemID string) (string, error) {
	const action = "get message checkpoint revert diff"
	thread, target, err := a.loadCheckpointForUserItem(action, threadID, userItemID)
	if err != nil {
		return "", err
	}
	workspace, err := checkpoint.ValidateWorkspaceMatch(action, thread.WorkspacePath, target.WorkspacePath)
	if err != nil {
		return "", err
	}
	paths, err := a.store.ListTrackedFilesFromTurn(threadID, target.TurnIndex)
	if err != nil {
		return "", fmt.Errorf("%s: tracked files: %w", action, err)
	}
	patch, err := a.checkpointStore().DiffRefToWorktreeScoped(context.Background(), workspace, target.RefName, paths)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

func (a *App) GetSessionAgentDiff(threadID string) (string, error) {
	const action = "get session agent diff"
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	checkpoints, err := a.store.ListCheckpoints(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if len(checkpoints) == 0 {
		return "", nil
	}
	first := checkpoints[0]
	if err := checkpoint.ValidateRef(action, threadID, first.RefName, first.WorkspacePath); err != nil {
		return "", err
	}
	workspace, err := checkpoint.ValidateWorkspaceMatch(action, thread.WorkspacePath, first.WorkspacePath)
	if err != nil {
		return "", err
	}
	paths, err := a.store.ListTrackedFiles(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: tracked files: %w", action, err)
	}
	patch, err := a.checkpointStore().DiffRefToWorktreeScoped(context.Background(), workspace, first.RefName, paths)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	return string(patch), nil
}

// loadCheckpointForUserItem fetches the thread + the per-user-item
// checkpoint and runs the thread<->checkpoint validation shared by the
// diff getters keyed on a user message. Returns a wrapped error with the
// caller-supplied action prefix when the thread is missing, the
// checkpoint row is absent, or the record fails validation.
func (a *App) loadCheckpointForUserItem(action, threadID, userItemID string) (store.Thread, store.Checkpoint, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, store.Checkpoint{}, fmt.Errorf("%s: %w", action, err)
	}
	target, ok, err := a.store.GetCheckpointByUserItemID(threadID, userItemID)
	if err != nil {
		return store.Thread{}, store.Checkpoint{}, fmt.Errorf("%s: %w", action, err)
	}
	if !ok {
		return store.Thread{}, store.Checkpoint{}, fmt.Errorf("%s: no checkpoint for thread %q user item %q", action, threadID, userItemID)
	}
	if err := checkpoint.ValidateRef(action, threadID, target.RefName, target.WorkspacePath); err != nil {
		return store.Thread{}, store.Checkpoint{}, err
	}
	return thread, target, nil
}

func (a *App) GetWorkspaceCurrentDiff(threadID string) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get workspace current diff: %w", err)
	}
	workspace := thread.WorkspacePath
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

	unlock := a.threadLocks().Lock(threadID)
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
	if checkpoint.IsWireOnlyUserItem(userItem) {
		return fmt.Errorf("revert checkpoint: %q is provider-injected context, not a user message", userItemID)
	}
	record, ok, err := a.store.GetCheckpointByUserItemID(threadID, userItemID)
	if err != nil {
		return fmt.Errorf("revert checkpoint: %w", err)
	}
	if !ok {
		return fmt.Errorf("revert checkpoint: no checkpoint for thread %q user item %q", threadID, userItemID)
	}
	if err := checkpoint.ValidateRef("revert checkpoint", threadID, record.RefName, record.WorkspacePath); err != nil {
		return err
	}
	if record.TurnIndex != userItem.TurnIndex {
		return fmt.Errorf(
			"revert checkpoint: checkpoint turn index %d does not match user message turn index %d",
			record.TurnIndex,
			userItem.TurnIndex,
		)
	}
	promptDraft, err := composerdraft.FromUserItem(threadID, userItem, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("revert checkpoint: build prompt draft: %w", err)
	}

	if err := a.revertConversationLocked(revertConversationLockedArgs{
		thread:       thread,
		userItem:     userItem,
		record:       record,
		mode:         mode,
		promptDraft:  promptDraft,
		errorPrefix:  "revert checkpoint",
		markReverted: false,
	}); err != nil {
		return err
	}
	a.emit("checkpoint:reverted", map[string]any{
		"threadId":   threadID,
		"userItemId": userItemID,
		"turnIndex":  userItem.TurnIndex,
		"mode":       mode,
	})
	return nil
}

// revertConversationLockedArgs bundles the parameters for the shared
// revert tail. Callers prepare the thread, user item, checkpoint, and
// composer draft, then hand off to revertConversationLocked which owns
// the destructive sequence (stop session → provider rollback → SQLite
// truncate → draft upsert). Event emission stays with the caller so
// each surface (explicit revert vs revert-on-interrupt) can pick its
// own payload shape.
type revertConversationLockedArgs struct {
	thread      store.Thread
	userItem    store.Item
	record      store.Checkpoint
	mode        string
	promptDraft store.ThreadDraft
	// errorPrefix scopes wrapped errors so the calling surface (revert
	// vs revert-on-interrupt) is identifiable in logs and toasts.
	errorPrefix string
	// markReverted asks the triage router to flag the next
	// turn-completed emission as a revert (so the frontend can
	// suppress the Interrupted pill). Used only by the
	// revert-on-interrupt path, which interrupts a live turn — the
	// explicit revert path requires no active turn, so there is no
	// in-flight turn-complete to flag.
	markReverted bool
}

// revertConversationLocked is the destructive tail of any conversation
// revert. The caller is responsible for the per-thread action lock,
// loading the user item + checkpoint, projecting the composer draft
// via composerdraft.FromUserItem, AND emitting whatever post-revert
// event its surface needs once this returns nil.
//
// Sequence (in order — partial failures leave a clear cleanup point):
//
//  1. Optionally mark the triage router so the synthesized truncated
//     turn-complete fired by CleanupThread (during stopSession)
//     carries RevertedUserMessage=true. Skip when no active turn is
//     in flight (markReverted=false).
//  2. stopSession — closes the provider subprocess and runs
//     triage.CleanupThread which drains pending-send FIFO, wire-only
//     dedup, flush queue, and synthesizes a truncated turn-complete
//     for any open turn.
//  3. revertProviderConversationToMessage — Claude JSONL fork or
//     Codex thread/rollback. For Claude this REQUIRES the session
//     to be stopped (file rewrite).
//  4. Conversation-and-files mode only: ListTrackedFilesFromTurn +
//     RestoreWorktreePaths + workspaceFiles.Invalidate.
//  5. DeleteCheckpointsFromTurn + DeleteConversationFromTurn truncate
//     SQLite at the user-item's turnIndex (inclusive).
//  6. deleteCheckpointRefs removes the matching git refs.
//  7. UpsertThreadDraft restores the composer draft.
func (a *App) revertConversationLocked(args revertConversationLockedArgs) error {
	if args.markReverted && a.triage != nil {
		a.triage.MarkTurnReverted(args.thread.ID)
	}
	if err := a.stopSession(args.thread.ID); err != nil {
		return fmt.Errorf("%s: stop session: %w", args.errorPrefix, err)
	}
	if err := a.revertProviderConversationToMessage(args.thread, args.record); err != nil {
		return fmt.Errorf("%s: %w", args.errorPrefix, err)
	}

	workspace := args.thread.WorkspacePath
	if args.mode == RevertModeConversationAndFiles {
		if workspace == "" {
			return fmt.Errorf("%s: thread has no workspace path", args.errorPrefix)
		}
		if args.record.WorkspacePath != workspace {
			return fmt.Errorf("%s: checkpoint workspace %q does not match thread workspace %q", args.errorPrefix, args.record.WorkspacePath, workspace)
		}
		paths, err := a.store.ListTrackedFilesFromTurn(args.thread.ID, args.userItem.TurnIndex)
		if err != nil {
			return fmt.Errorf("%s: tracked files: %w", args.errorPrefix, err)
		}
		if err := a.checkpointStore().RestoreWorktreePaths(context.Background(), workspace, args.record.RefName, paths); err != nil {
			return fmt.Errorf("%s: restore paths: %w", args.errorPrefix, err)
		}
		if a.workspaceFiles != nil {
			a.workspaceFiles.Invalidate(workspace)
		}
	}

	refs, err := a.store.DeleteCheckpointsFromTurn(args.thread.ID, args.userItem.TurnIndex)
	if err != nil {
		return fmt.Errorf("%s: truncate checkpoints: %w", args.errorPrefix, err)
	}
	if _, err := a.store.DeleteConversationFromTurn(args.thread.ID, args.userItem.TurnIndex); err != nil {
		return fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
	}
	if err := a.deleteCheckpointRefs(context.Background(), args.thread.ID, args.errorPrefix, refs); err != nil {
		return err
	}
	if err := a.store.UpsertThreadDraft(args.promptDraft); err != nil {
		return fmt.Errorf("%s: restore prompt draft: %w", args.errorPrefix, err)
	}
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
		return a.store.UpdateThread(thread)
	}
	sourceSessionRef := thread.ResolvedSessionRef()
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
	if err := a.store.UpdateThread(thread); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("persist reverted claude state: %w", err)
	}
	return nil
}

func (a *App) deleteCheckpointRefs(ctx context.Context, threadID, action string, refs []store.CheckpointRef) error {
	for _, ref := range refs {
		if err := checkpoint.ValidateRef(action, threadID, ref.RefName, ref.WorkspacePath); err != nil {
			return err
		}
		if err := a.checkpointStore().DeleteRef(ctx, ref.WorkspacePath, ref.RefName); err != nil {
			return fmt.Errorf("%s: delete stale ref %s: %w", action, ref.RefName, err)
		}
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
		out = append(out, checkpoint.ViewFromStore(row))
	}
	return out, nil
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
