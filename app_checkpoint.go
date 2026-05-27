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
	"agent-overflow/internal/usermessage"

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

type RevertToMessageCheckpointOptions struct {
	KillRunningBackgroundTasks bool `json:"killRunningBackgroundTasks,omitempty"`
}

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
		ID:         uuid.NewString(),
		ThreadID:   thread.ID,
		UserItemID: userItem.ID,
		TurnIndex:  userItem.TurnIndex,
		// Mirror the row's provider_item_id onto the checkpoint at capture
		// time. For a direct send the row meta already carries the minted
		// send uuid (app_send.go stamps it before this call), so the
		// checkpoint is revert-ready before Claude's replay echo — the
		// revert path keys on this id. Empty when the meta has none yet
		// (eager-persist-on-interrupt rows, legacy sends); the echo then
		// fills it via UpdateCheckpointProviderIDs as before.
		ProviderUserMessageID: usermessage.ReadProviderItemID(userItem.Meta),
		RefName:               ref,
		Status:                "ready",
		Files:                 files,
		CapturedAt:            now,
		WorkspacePath:         workspace,
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
	return a.revertToMessageCheckpoint(threadID, userItemID, mode, RevertToMessageCheckpointOptions{})
}

func (a *App) RevertToMessageCheckpointWithOptions(
	threadID string,
	userItemID string,
	mode string,
	opts RevertToMessageCheckpointOptions,
) error {
	return a.revertToMessageCheckpoint(threadID, userItemID, mode, opts)
}

func (a *App) revertToMessageCheckpoint(
	threadID string,
	userItemID string,
	mode string,
	opts RevertToMessageCheckpointOptions,
) error {
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
	if running, err := a.hasRunningBackgroundTasks(threadID); err != nil {
		return fmt.Errorf("revert checkpoint: check background tasks: %w", err)
	} else if running && !opts.KillRunningBackgroundTasks {
		return errors.New("revert checkpoint: running background tasks must be killed before reverting")
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
		thread:                      thread,
		userItem:                    userItem,
		record:                      record,
		mode:                        mode,
		promptDraft:                 promptDraft,
		errorPrefix:                 "revert checkpoint",
		markReverted:                false,
		clearRunningBackgroundTasks: opts.KillRunningBackgroundTasks,
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
// the destructive sequence (provider cleanup/rollback → SQLite truncate
// → draft upsert). Event emission stays with the caller so each surface
// (explicit revert vs revert-on-interrupt) can pick its own payload shape.
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
	// clearRunningBackgroundTasks hides any still-running background
	// tray rows after provider-owned work has been terminated. Claude
	// relies on stopSession's process-group close; Codex uses its
	// thread-wide background-terminal clean RPC before rollback when a
	// live app-server session exists.
	clearRunningBackgroundTasks bool
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
//  2. Optional background cleanup, when the user explicitly confirmed it.
//  3. Optional tray-state cleanup immediately after provider-owned work
//     is terminated. This runs before rollback/truncation so killed work
//     does not stay advertised as running if a later step fails.
//  4. Provider rollback:
//     - Codex uses a live/resumed app-server session and validates the
//     thread/rollback response before local state changes.
//     - Claude stops the provider subprocess first, then writes a sliced
//     session file.
//  5. Conversation-and-files mode only: ListTrackedFilesFromTurn +
//     RestoreWorktreePaths + workspaceFiles.Invalidate.
//  6. DeleteCheckpointsFromTurn + DeleteConversationFromTurn truncate
//     SQLite at the user-item's turnIndex (inclusive).
//  7. deleteCheckpointRefs removes the matching git refs.
//  8. UpsertThreadDraft restores the composer draft.
func (a *App) revertConversationLocked(args revertConversationLockedArgs) error {
	if args.markReverted && a.triage != nil {
		a.triage.MarkTurnReverted(args.thread.ID)
	}

	if args.clearRunningBackgroundTasks {
		if err := a.cleanRunningBackgroundTasksBeforeProviderRevert(args.thread, args.errorPrefix); err != nil {
			return err
		}
	}
	if args.thread.Provider == string(provider.Codex) {
		if args.clearRunningBackgroundTasks {
			if err := a.markConfirmedBackgroundTasksInactiveAfterProviderCleanup(args.thread.ID, args.errorPrefix); err != nil {
				return err
			}
		}
		result, err := a.revertCodexConversationToMessage(args.thread, args.record, args.userItem.TurnIndex, args.markReverted)
		if err != nil {
			return fmt.Errorf("%s: %w", args.errorPrefix, err)
		}
		if result != nil {
			a.resetLiveCodexRollbackState(args.thread.ID)
		}
	} else {
		if err := a.stopSession(args.thread.ID); err != nil {
			return fmt.Errorf("%s: stop session: %w", args.errorPrefix, err)
		}
		if args.clearRunningBackgroundTasks {
			if err := a.markConfirmedBackgroundTasksInactiveAfterProviderCleanup(args.thread.ID, args.errorPrefix); err != nil {
				return err
			}
		}
		if err := a.revertProviderConversationToMessage(args.thread, args.record); err != nil {
			return fmt.Errorf("%s: %w", args.errorPrefix, err)
		}
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
		return fmt.Errorf("codex rollback requires a live or resumed app-server session")
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
	// Prefer UUID-keyed slicing when the checkpoint carries a wire
	// id — it is immune to synthetic-entry ordinal drift (e.g.
	// /compact-summary rows or `[Request interrupted by user]`
	// markers). Fall back to the ordinal walk when the checkpoint has
	// no stamped id (the user_text row pre-dates triage's
	// `provider_item_id` stamping path, or the synthesized at-send
	// record found nothing on the item meta — the fast send→escape
	// race lands here); `findmessage.isRealUserPrompt` filters the same
	// synthetic entries (boolean flags, content sentinels, injected XML)
	// so the fallback is correct as long as the wire shape stays in its
	// documented set.
	newID, newPath, err := a.writeRevertedClaudeSession(srcPath, checkpoint)
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

// writeRevertedClaudeSession is the revert-path call into
// writeClaudeSessionSlice. The revert path discards the uuidMap
// because nothing downstream remaps stored ids — the active thread's
// `items.meta.provider_item_id` still points at the source session's
// UUIDs, which is correct because the source-message AO row remains
// the slice anchor for any subsequent revert.
func (a *App) writeRevertedClaudeSession(srcPath string, checkpoint store.Checkpoint) (string, string, error) {
	newID, newPath, _, err := writeClaudeSessionSlice(
		srcPath, checkpoint.ProviderUserMessageID, checkpoint.TurnIndex-1, "claude rollback",
	)
	return newID, newPath, err
}

// writeClaudeSessionSlice tries the UUID-keyed fork-slice when
// anchorUUID is non-empty. On ErrMessageNotFound — most often: the
// stored UUID is stale because the session was forked but the
// post-fork remap regressed — it falls back to the ordinal walk at
// fallbackLastKeptTurn so a known-imperfect slice still beats a
// hard error. Other errors from the UUID-keyed branch propagate
// verbatim. logCtx prefixes the fallback log so the operator can
// tell which entry point hit the stale id; a loud log here is
// deliberate because a wrong-source slice is worse than the
// ordinal walk's known synthetic-entry sensitivity.
//
// Returns (newSessionID, newPath, uuidMap, err). The fork callers
// thread the uuidMap into `remapForkedClaudeUUIDs`; the revert
// caller discards it.
func writeClaudeSessionSlice(
	srcPath, anchorUUID string,
	fallbackLastKeptTurn int,
	logCtx string,
) (string, string, map[string]string, error) {
	if uuid := strings.TrimSpace(anchorUUID); uuid != "" {
		newID, newPath, uuidMap, err := sessionfork.WriteForkFileForUserMessageUUID(srcPath, uuid, "")
		if err == nil {
			return newID, newPath, uuidMap, nil
		}
		if !errors.Is(err, sessionfork.ErrMessageNotFound) {
			return "", "", nil, err
		}
		log.Printf("%s: stored provider_user_message_id %q not in session %s — falling back to ordinal slice; check fork remap coverage", logCtx, uuid, srcPath)
	}
	newID, newPath, uuidMap, err := sessionfork.WriteForkFileForLastKeptTurn(srcPath, fallbackLastKeptTurn, "")
	if err == nil {
		return newID, newPath, uuidMap, nil
	}
	if errors.Is(err, sessionfork.ErrUserTurnAtTranscriptEnd) {
		// Slice anchor lands one past the last persisted user prompt:
		// AO recorded the user_text row but the Claude CLI died before
		// writing that prompt to the JSONL. Clone the JSONL as-is — the
		// file is already at the right cut point from Claude's side
		// (it never saw the missing prompt), and AO's composer
		// rehydration restores the missing message from the DB so the
		// user can re-edit and resend.
		log.Printf("%s: revert anchor past JSONL end (%v) — cloning full transcript", logCtx, err)
		return sessionfork.WriteForkFileFullTranscript(srcPath, "")
	}
	return "", "", nil, err
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

func (a *App) revertCodexConversationToMessage(thread store.Thread, checkpoint store.Checkpoint, targetTurnIndex int, waitForIdle bool) (*codex.ThreadRollbackResult, error) {
	lastTurn, err := a.store.LastTurnIndex(thread.ID)
	if err != nil {
		return nil, fmt.Errorf("determine last turn: %w", err)
	}
	numTurns := lastTurn - checkpoint.TurnIndex + 1
	if numTurns < 1 {
		return nil, nil
	}
	if waitForIdle {
		if err := a.waitForNoActiveTurn(thread.ID, 5*time.Second); err != nil {
			return nil, fmt.Errorf("codex rollback: wait for active turn: %w", err)
		}
	}
	result, err := a.rollbackCodexThread(thread, numTurns)
	if err != nil {
		return nil, err
	}
	if err := a.validateCodexRollbackSurvivors(thread.ID, "codex rollback", result, targetTurnIndex); err != nil {
		return nil, err
	}
	return &result, nil
}

func (a *App) validateCodexRollbackSurvivors(threadID, action string, result codex.ThreadRollbackResult, maxSurvivingTurns int) error {
	if result.TurnCount > maxSurvivingTurns {
		return fmt.Errorf("%s returned %d surviving turns, expected at most %d", action, result.TurnCount, maxSurvivingTurns)
	}
	minKnownSurvivors, err := a.knownCodexProviderTurnCountBefore(threadID, maxSurvivingTurns)
	if err != nil {
		return fmt.Errorf("%s: count provider-backed turns: %w", action, err)
	}
	if result.TurnCount < minKnownSurvivors {
		return fmt.Errorf("%s returned %d surviving turns, expected at least %d known provider-backed turns", action, result.TurnCount, minKnownSurvivors)
	}
	return nil
}

func (a *App) knownCodexProviderTurnCountBefore(threadID string, beforeTurnIndex int) (int, error) {
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return 0, err
	}
	turns := make(map[int]struct{})
	for _, item := range items {
		if item.TurnIndex >= beforeTurnIndex {
			continue
		}
		if item.Kind != "user_text" || item.Role != "user" || checkpoint.IsWireOnlyUserItem(item) {
			continue
		}
		if usermessage.ReadProviderItemID(item.Meta) == "" {
			continue
		}
		turns[item.TurnIndex] = struct{}{}
	}
	return len(turns), nil
}

func (a *App) rollbackCodexThread(thread store.Thread, numTurns int) (codex.ThreadRollbackResult, error) {
	if active, ok := a.activeCodexSession(thread.ID); ok {
		return active.Rollback(context.Background(), numTurns)
	}
	if thread.SessionRef == "" {
		return codex.ThreadRollbackResult{}, fmt.Errorf("codex rollback: thread %q is missing a Codex thread reference", thread.ID)
	}
	if err := a.startSession(thread.ID); err != nil {
		return codex.ThreadRollbackResult{}, fmt.Errorf("codex rollback: resume session: %w", err)
	}
	active, ok := a.activeCodexSession(thread.ID)
	if !ok {
		return codex.ThreadRollbackResult{}, fmt.Errorf("codex rollback: resumed session unavailable for thread %q", thread.ID)
	}
	return active.Rollback(context.Background(), numTurns)
}

func (a *App) resetLiveCodexRollbackState(threadID string) {
	if a.triage != nil {
		a.triage.ResetThreadForRollback(threadID)
	}
	a.clearFlushDispatchForRollback(threadID)
}

func (a *App) waitForNoActiveTurn(threadID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, active, err := a.store.GetActiveTurn(threadID); err != nil {
			return err
		} else if !active {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
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
