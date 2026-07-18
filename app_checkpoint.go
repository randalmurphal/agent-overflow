package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/diffsummary"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
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

	now := time.Now().UnixMilli()
	var files []diffsummary.File
	if prev, ok, err := a.store.GetPreviousCheckpoint(thread.ID, userItem.TurnIndex, userItem.ItemIndex); err != nil {
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

	record := store.Checkpoint{
		ID:         uuid.NewString(),
		ThreadID:   thread.ID,
		UserItemID: userItem.ID,
		TurnIndex:  userItem.TurnIndex,
		// Mirror the row's provider ids onto the checkpoint at capture
		// time. For a direct send the row meta already carries the minted
		// send uuid (app_send.go stamps it before this call), so the
		// checkpoint is revert-ready before Claude's replay echo — the
		// revert path keys on this id. A row confirmed by its echo also
		// carries the parent uuid (stamped in the same tx as the item id,
		// round-5 R5-8). Empty when the meta has none yet (eager-persist-
		// on-interrupt rows, legacy sends); the echo then fills both via
		// UpdateCheckpointProviderIDs as before.
		ProviderUserMessageID: usermessage.ReadProviderItemID(userItem.Meta),
		ProviderParentUUID:    usermessage.ReadProviderParentUUID(userItem.Meta),
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
	anchorItem, found, err := a.store.GetThreadItem(threadID, userItemID)
	if err != nil {
		return "", fmt.Errorf("%s: load anchor item: %w", action, err)
	}
	if !found {
		return "", fmt.Errorf("%s: anchor item %q not found", action, userItemID)
	}
	prev, ok, err := a.store.GetPreviousCheckpoint(threadID, anchorItem.TurnIndex, anchorItem.ItemIndex)
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
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", fmt.Errorf("get workspace current diff: %w", err)
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

// GetBranchBaseDiff returns the combined diff of the thread's workspace
// (committed work since merge-base plus uncommitted changes) against the
// merge base of baseBranch and the workspace HEAD — i.e. what a PR onto
// baseBranch would contain.
func (a *App) GetBranchBaseDiff(threadID string, baseBranch string) (string, error) {
	const action = "get branch base diff"
	if strings.TrimSpace(baseBranch) == "" {
		return "", fmt.Errorf("%s: base branch is required", action)
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	_, workspace, err := a.resolveGitPaths(thread)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
	}
	if !a.checkpointStore().IsGitRepository(context.Background(), workspace) {
		return "", nil
	}
	patch, err := a.checkpointStore().DiffBranchBaseToWorktree(context.Background(), workspace, baseBranch)
	if err != nil {
		return "", fmt.Errorf("%s: %w", action, err)
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
// the destructive sequence (provider cleanup/revert → SQLite truncate
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
	// thread-wide background-terminal clean RPC before the provider
	// fork when a live app-server session exists.
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
//     is terminated. This runs before the provider revert/truncation so
//     killed work does not stay advertised as running if a later step fails.
//  4. Provider revert:
//     - Codex stops the session first (closing the stopped-thread
//     gate), then forks its provider thread at the pre-revert anchor
//     turn (thread/fork lastTurnId) through a throwaway resume session
//     and repoints SessionRef at the fork.
//     - Claude stops the provider subprocess first, then writes a sliced
//     session file.
//  5. Conversation-and-files mode only: ListTrackedFilesFromTurn +
//     RestoreWorktreePaths + workspaceFiles.Invalidate.
//  6. SQLite truncation at the provider revert's granularity: Codex
//     deletes whole turns from the user-item's turnIndex (inclusive,
//     matching thread/fork's turn-boundary cut); Claude deletes from
//     the user item itself (DeleteConversationFromItem, matching the
//     session slice at the message uuid).
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
		if err := a.revertCodexThreadToMessage(args.thread, args.record); err != nil {
			return fmt.Errorf("%s: %w", args.errorPrefix, err)
		}
	} else if args.thread.Provider == string(provider.ClaudeTUI) {
		// The interactive TUI reverts the just-sent prompt natively when it
		// receives the Esc: the Esc aborts the in-flight /v1/messages and the
		// dropped turn does not re-enter the next request (LIVE-confirmed in
		// spike/claude-mitm/probe_hook_escrevert.py + probe_hook_revertcontext.py).
		// InterruptAndRevertIfClean already delivered that Esc via the provider
		// Interrupt above, so — unlike headless Claude — AO must NOT stop the
		// session (it stays live for the next turn) or rewrite a session file (the
		// TUI owns its own conversation; AO has no fork file to write). AO only
		// mirrors the native revert in its own timeline + draft below. claude-tui
		// Send clears the composer before its next paste so the prompt the TUI
		// restored can't fuse with the re-send. A files-revert has no native TUI
		// equivalent and is gated off in the UI (revert capability = false), so
		// refuse it here as defense-in-depth.
		if args.mode == RevertModeConversationAndFiles {
			return fmt.Errorf("%s: claude-tui cannot revert workspace files", args.errorPrefix)
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
		if err := a.revertProviderConversationToMessage(args.thread, args.record, args.userItem); err != nil {
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

	// The prompt draft is restored BEFORE the destructive truncation: the
	// provider slice above already removed the message from provider
	// history, so from here on the composer draft is the user's only copy.
	// If truncation then fails, the timeline still holds the rows and the
	// checkpoint, and a retry converges — the provider revert re-runs
	// against the already-cut transcript (the already-cut detector clones
	// it whole) and this upsert is idempotent. The old order deleted the
	// checkpoint (the retry key) first, so any later failure stranded the
	// message with no way to restore it (round-4 review, CT4-4).
	if err := a.store.UpsertThreadDraft(args.promptDraft); err != nil {
		return fmt.Errorf("%s: restore prompt draft: %w", args.errorPrefix, err)
	}

	// Truncation granularity must match the provider revert above. Codex
	// thread/fork cuts provider history at the turn boundary before the
	// checkpoint's turn, so SQLite drops the whole turn. Claude's session
	// slice (and the TUI's native Esc-revert) cut at the message itself, so
	// only the anchor row and what follows it go — a queued flush message
	// that shares its turn with an earlier prompt keeps that prompt and the
	// agent work that preceded the queued send. The Codex coarseness is an
	// app-server API limit, not permanent: see the granularity note on
	// codex.Session.ForkAt for what upstream already has and when this
	// branch can move to a message-granular cut.
	var refs []store.CheckpointRef
	var err error
	if args.thread.Provider == string(provider.Codex) {
		// One transaction for checkpoints + conversation: the old
		// two-call shape could commit the checkpoint delete and then
		// fail the conversation delete, stranding rows whose revert
		// anchors were already gone (round-5, R5-5).
		refs, _, err = a.store.DeleteConversationFromTurn(args.thread.ID, args.userItem.TurnIndex)
		if err != nil {
			return fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
		}
	} else {
		refs, err = a.store.DeleteConversationFromItem(args.thread.ID, args.userItem.ID)
		if err != nil {
			return fmt.Errorf("%s: truncate conversation: %w", args.errorPrefix, err)
		}
	}
	// Best-effort, after the point of no return: the checkpoint rows are
	// gone, so a ref-deletion failure can't be retried through this path
	// anyway — surfacing it as a hard error would only fail a revert that
	// has already fully happened. Orphaned refs are logged garbage;
	// they're swept when the thread is deleted.
	a.deleteCheckpointRefsBestEffort(args.thread.ID, args.errorPrefix, "", refs)
	return nil
}

func (a *App) revertProviderConversationToMessage(thread store.Thread, checkpoint store.Checkpoint, userItem store.Item) error {
	switch thread.Provider {
	case string(provider.Claude):
		return a.revertClaudeThreadToMessage(thread, checkpoint, userItem)
	default:
		return fmt.Errorf("unsupported provider %q", thread.Provider)
	}
}

// revertCodexThreadToMessage STOPS the session, then moves the
// thread's provider cursor to a `thread/fork` of its Codex thread cut
// at the last provider-backed turn before the reverted message. The
// stop is load-bearing, not cleanup: CleanupThread flips the stopped-
// thread gate (invariant 29) so straggler wire events from the old
// thread — late notifications, an interrupt-triggered turn completion
// — cannot land rows on the timeline the caller is about to truncate.
// (The old thread/rollback flow kept the session live through the
// revert, which is exactly the window the 2026-07 deprecation-notice-
// on-a-settled-turn race lived in.) The next send resumes on the fork.
//
// Reverting to turn 0 — or to a prefix with no provider-backed turns —
// needs no fork: SessionRef clears and the next send starts a fresh
// Codex thread, mirroring revertClaudeThreadToMessage's turn-0 branch.
func (a *App) revertCodexThreadToMessage(thread store.Thread, checkpoint store.Checkpoint) error {
	anchor := ""
	anchorFound := false
	if checkpoint.TurnIndex > 0 {
		var err error
		anchor, anchorFound, err = a.resolveCodexForkAnchor(thread.ID, checkpoint.TurnIndex-1)
		if err != nil {
			return fmt.Errorf("codex revert: %w", err)
		}
		// The thread reference is only required when a fork is actually
		// needed. A kept prefix of local-only failed sends resolves to
		// no anchor and takes the fresh-thread path below even on a
		// thread that never obtained a SessionRef.
		if anchorFound && thread.SessionRef == "" {
			return fmt.Errorf("codex revert: turn %d has provider-backed history but thread %s has no Codex thread reference", checkpoint.TurnIndex, thread.ID)
		}
	}
	// Stop BEFORE forking: the stopped-thread gate must be closed for
	// the whole mutation window. Forking through a still-live session
	// would leave its read loop delivering source-thread events during
	// the RPC. forkCodexThreadAt runs the fork through a throwaway
	// resume session whose events go nowhere.
	if err := a.stopSession(thread.ID); err != nil {
		return fmt.Errorf("codex revert: stop session: %w", err)
	}
	a.clearFlushDispatchForRollback(thread.ID)
	forkRef := ""
	if anchorFound {
		var err error
		forkRef, err = a.forkCodexThreadAt(thread, anchor)
		if err != nil {
			return fmt.Errorf("codex revert: fork at %s: %w", anchor, err)
		}
	}
	thread.SessionRef = forkRef
	thread.PendingForkRef = ""
	if err := a.store.UpdateThread(thread); err != nil {
		return fmt.Errorf("codex revert: persist reverted state: %w", err)
	}
	return nil
}

func (a *App) revertClaudeThreadToMessage(thread store.Thread, checkpoint store.Checkpoint, userItem store.Item) error {
	midTurn, err := claudeMidTurnAnchor(userItem)
	if err != nil {
		return fmt.Errorf("claude rollback: %w", err)
	}
	// A revert to the row that opens turn 0 keeps nothing: drop the session
	// reference and let the next send start fresh. An anchor deeper in turn
	// 0 — a flush message queued during the very first turn — keeps that
	// turn's prefix, so it needs the session slice like any later turn.
	if checkpoint.TurnIndex == 0 && !midTurn {
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
	newID, newPath, uuidMap, err := a.writeRevertedClaudeSession(srcPath, checkpoint, userItem, midTurn)
	if err != nil {
		return fmt.Errorf("write reverted session: %w", err)
	}
	// The slice reminted every uuid, so surviving items' provider_item_id
	// and surviving checkpoints' provider ids all point at the OLD
	// session file. Compute the rewrites BEFORE committing anything —
	// a failure here aborts with the thread untouched (the slice file is
	// an inert orphan) — then commit SessionRef + remap in ONE store
	// transaction. Committed separately, a crash between the two left
	// ids one fork generation stale, and a retried revert on top of that
	// lost the single-generation forkedFrom provenance the anchor
	// lookups heal through (round-6, R6-5). Rows at or past the revert
	// anchor are about to be truncated by the caller and are absent from
	// the map — unmapped ids are left untouched.
	itemUpdates, checkpointUpdates, err := a.computeClaudeProviderIDRemap(thread.ID, uuidMap)
	if err != nil {
		a.removeAbandonedSessionSlice(newPath)
		return fmt.Errorf("claude rollback: compute provider id remap: %w", err)
	}
	thread.SessionRef = newID
	thread.PendingForkRef = ""
	if err := a.store.UpdateThreadAndRemapProviderIDs(thread, itemUpdates, checkpointUpdates); err != nil {
		a.removeAbandonedSessionSlice(newPath)
		return fmt.Errorf("persist reverted claude state: %w", err)
	}
	return nil
}

// removeAbandonedSessionSlice deletes a Claude slice file whose revert
// aborted before committing any store state. The file is inert (no
// thread references it), so a failed delete only leaks disk — but a
// silent leak across repeated failed reverts is invisible, so log it
// (round-7, R7-6).
func (a *App) removeAbandonedSessionSlice(path string) {
	if err := os.Remove(path); err != nil {
		log.Printf("app: claude rollback: remove abandoned session slice %s: %v", path, err)
	}
}

// claudeMidTurnAnchor reports whether a revert/fork anchor row sits
// mid-turn in PROVIDER order — content the session slice must retain
// precedes it inside its own turn. Display position alone
// (ItemIndex > 0) undercounts: a promoted flush row healed at its
// dispatch-time index after a failed tail bump (round-10, R10-1) can
// sit at display index 0 while the interrupted round's tail —
// provider-order BEFORE the queued message — persists below it, and
// the ordinal whole-turn slice (or the turn-0 drop-SessionRef branch)
// would cut that retained prefix from the transcript while
// DeleteConversationFromItem's promoted predicate keeps it in SQLite
// (round-12, C12-1). The promotion marker is the durable record of
// that ordering. Head-healed deferred prompts (negative index,
// unmarked — round-7 R7-4 / round-8 R8-1) stay turn-initial. A
// malformed marker fails loudly per the corrupt-metadata posture
// (round-9, R9-4).
func claudeMidTurnAnchor(userItem store.Item) (bool, error) {
	state, err := itemmeta.DecodePromotionState(userItem.Meta)
	if err != nil {
		return false, fmt.Errorf("decode promotion state for %s/%s: %w", userItem.ThreadID, userItem.ID, err)
	}
	return userItem.ItemIndex > 0 || state.Promoted, nil
}

// writeRevertedClaudeSession is the revert-path call into
// writeClaudeSessionSlice. Returns the slice's uuidMap (old → new for
// every kept row) so the caller can refresh stored provider ids — the
// slice remints every uuid, exactly like a fork. midTurnAnchor comes
// from claudeMidTurnAnchor: a queued flush row sharing its turn with
// content that precedes it in provider order.
func (a *App) writeRevertedClaudeSession(srcPath string, checkpoint store.Checkpoint, userItem store.Item, midTurnAnchor bool) (string, string, map[string]string, error) {
	return writeClaudeSessionSlice(
		srcPath, claudeSliceAnchorUUIDs(checkpoint, userItem), claudeSliceParentUUIDs(checkpoint, userItem),
		checkpoint.TurnIndex-1, midTurnAnchor, "claude rollback",
	)
}

// claudeSliceParentUUIDs returns the anchor's transcript-parent uuid
// candidates for the already-cut retry, in the same trust order as
// claudeSliceAnchorUUIDs: the checkpoint's provider_parent_uuid, then
// the item row's meta stamp. The item copy is written atomically with
// the item id at the echo (round-5, R5-8), so a checkpoint whose
// follow-up update failed — previously the ONLY durable parent copy —
// no longer strands the retry without a slice-through point.
func claudeSliceParentUUIDs(checkpointRow store.Checkpoint, userItem store.Item) []string {
	var candidates []string
	if checkpointRow.ProviderParentUUID != "" {
		candidates = append(candidates, checkpointRow.ProviderParentUUID)
	}
	if p := usermessage.ReadProviderParentUUID(userItem.Meta); p != "" && p != checkpointRow.ProviderParentUUID {
		candidates = append(candidates, p)
	}
	return candidates
}

// claudeSliceAnchorUUIDs returns the wire uuid candidates keying a
// Claude slice at userItem, in trust order: the checkpoint's
// provider_user_message_id, then the item row's own durable meta stamp
// when it differs. The two copies are written at different moments —
// the item meta at the echo's stamp, the checkpoint by a follow-up
// UpdateCheckpointProviderIDs that can fail after the stamp committed
// (round-4 review, CT4-6) — and refreshed by different remap loops
// (remapClaudeProviderIDs updates items before checkpoints, each row
// autocommitting), so either copy can be a remap generation staler than
// the other. writeClaudeSessionSlice tries each candidate before
// declaring the anchor missing (round-5, R5-7); a checkpoint without
// any id does NOT mean the message never reached the provider — the
// item-meta candidate keeps a consumed mid-turn message on the exact
// UUID-keyed slice instead of misclassifying it into the unconsumed
// full-clone path, which would truncate the timeline while the provider
// transcript keeps the message.
func claudeSliceAnchorUUIDs(checkpointRow store.Checkpoint, userItem store.Item) []string {
	var candidates []string
	if checkpointRow.ProviderUserMessageID != "" {
		candidates = append(candidates, checkpointRow.ProviderUserMessageID)
	}
	if id := usermessage.ReadProviderItemID(userItem.Meta); id != "" && id != checkpointRow.ProviderUserMessageID {
		if len(candidates) == 0 {
			log.Printf("claude slice: checkpoint for %s/%s carries no provider uuid — using the item row's durable stamp %q", userItem.ThreadID, userItem.ID, id)
		}
		candidates = append(candidates, id)
	}
	return candidates
}

// writeClaudeSessionSlice tries the UUID-keyed fork-slice for each
// anchor candidate in order (checkpoint copy first, then the item
// row's meta stamp — see claudeSliceAnchorUUIDs; either can be a remap
// generation staler than the other, so a miss on the first is retried
// on the next before any fallback, round-5 R5-7). Only when EVERY
// candidate is ErrMessageNotFound — most often: the stored UUIDs are
// stale because the session was forked but the post-fork remap
// regressed — does it fall back to the ordinal walk at
// fallbackLastKeptTurn, so a known-imperfect slice still beats a
// hard error. Other errors from the UUID-keyed branch propagate
// verbatim. logCtx prefixes the fallback log so the operator can
// tell which entry point hit the stale id; a loud log here is
// deliberate because a wrong-source slice is worse than the
// ordinal walk's known synthetic-entry sensitivity.
//
// midTurnAnchor changes the no-UUID handling: the ordinal walk keeps
// whole turns, so for an anchor that does NOT open its turn (a queued
// flush row sharing turn N with an earlier prompt) it would slice at
// end-of-turn-N-1 and drop the shared turn's kept prefix from the
// provider session while SQLite retains it. A mid-turn anchor with an
// EMPTY uuid was never consumed — the checkpoint's provider id is
// stamped only by the consumption echo — so the transcript is already
// at the right cut and is cloned whole (the common case: revert of an
// interrupt-promoted row before its echo). A mid-turn anchor with a
// NON-EMPTY uuid that the transcript doesn't contain splits on
// anchorParentUUIDs (the checkpoint's provider_parent_uuid, then the
// item meta's copy — see claudeSliceParentUUIDs, round-5 R5-8):
//
//   - Parent PRESENT in the transcript: a prior slice already cut this
//     transcript exactly at the anchor — the post-slice remap refreshed
//     the surviving parent's id while the cut-away anchor's id had
//     nothing to map to. This is the retry of a revert whose later step
//     (files restore, SQLite truncation) failed after the provider
//     commit; re-slice keeping through the parent and let the caller
//     redo the remaining steps. Through-the-parent, not a whole clone:
//     anything appended after the failed revert (a resumed session's
//     rows) must not be resurrected into the retried cut (round-5,
//     R5-6).
//   - Parent ABSENT (or unknown): the stored ids went stale wholesale
//     (fork remap regression). Cloning the full transcript would resume
//     a session that still contains the reverted prompt and its
//     response; slicing ordinally would drop the shared turn's kept
//     prefix. Both silently diverge from the visible timeline, so the
//     operation FAILS — loud and recoverable beats a session whose
//     context contradicts what the user reverted.
//
// Returns (newSessionID, newPath, uuidMap, err). Fork and revert
// callers both thread the uuidMap into `remapClaudeProviderIDs` so
// stored ids track the reminted session.
func writeClaudeSessionSlice(
	srcPath string,
	anchorUUIDs []string,
	anchorParentUUIDs []string,
	fallbackLastKeptTurn int,
	midTurnAnchor bool,
	logCtx string,
) (string, string, map[string]string, error) {
	dedupNonEmpty := func(uuids []string) []string {
		var out []string
		for _, candidate := range uuids {
			if uuid := strings.TrimSpace(candidate); uuid != "" && !slices.Contains(out, uuid) {
				out = append(out, uuid)
			}
		}
		return out
	}
	candidates := dedupNonEmpty(anchorUUIDs)
	for i, uuid := range candidates {
		newID, newPath, uuidMap, err := sessionfork.WriteForkFileForUserMessageUUID(srcPath, uuid, "")
		if err == nil {
			if i > 0 {
				log.Printf("%s: anchor uuid %q missed but candidate %q matched session %s — the missed copy is a remap generation stale (round-5, R5-7)", logCtx, candidates[0], uuid, srcPath)
			}
			return newID, newPath, uuidMap, nil
		}
		if !errors.Is(err, sessionfork.ErrMessageNotFound) {
			return "", "", nil, err
		}
	}
	if len(candidates) > 0 {
		missed := strings.Join(candidates, ", ")
		if midTurnAnchor {
			for _, parent := range dedupNonEmpty(anchorParentUUIDs) {
				newID, newPath, uuidMap, parentErr := sessionfork.WriteForkFileThroughUUID(srcPath, parent, "")
				if parentErr == nil {
					log.Printf("%s: anchor uuids [%s] absent but the parent %q is present in session %s — a prior slice already cut this transcript at the anchor; re-slicing through the parent", logCtx, missed, parent, srcPath)
					return newID, newPath, uuidMap, nil
				}
				if !errors.Is(parentErr, sessionfork.ErrMessageNotFound) {
					return "", "", nil, parentErr
				}
			}
			return "", "", nil, fmt.Errorf(
				"%s: stored provider uuid %q is missing from session %s — the queued message was consumed but its stored id no longer matches the transcript (fork remap drift); refusing a mid-turn cut that would silently diverge from the timeline",
				logCtx, missed, srcPath,
			)
		}
		log.Printf("%s: stored provider uuids [%s] not in session %s — falling back to ordinal slice; check fork remap coverage", logCtx, missed, srcPath)
	}
	if midTurnAnchor {
		return sessionfork.WriteForkFileFullTranscript(srcPath, "")
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

func (a *App) deleteCheckpointRefsBestEffort(threadID, action, fallbackWorkspace string, refs []store.CheckpointRef) {
	ctx := context.Background()
	byWorkspace := make(map[string][]string)
	for _, ref := range refs {
		if err := checkpoint.ValidateRef(action, threadID, ref.RefName, ref.WorkspacePath); err != nil {
			log.Printf("%s: skip invalid checkpoint ref thread=%s ref=%q workspace=%q: %v", action, threadID, ref.RefName, ref.WorkspacePath, err)
			continue
		}
		byWorkspace[ref.WorkspacePath] = append(byWorkspace[ref.WorkspacePath], ref.RefName)
	}

	for workspace, workspaceRefs := range byWorkspace {
		if err := a.checkpointStore().DeleteRefs(ctx, workspace, workspaceRefs); err != nil {
			if fallbackWorkspace == "" || fallbackWorkspace == workspace {
				log.Printf("%s: delete %d checkpoint refs failed thread=%s workspace=%q: %v", action, len(workspaceRefs), threadID, workspace, err)
				continue
			}
			fallbackRefs := make([]string, 0, len(workspaceRefs))
			for _, ref := range workspaceRefs {
				if fallbackErr := checkpoint.ValidateRef(action, threadID, ref, fallbackWorkspace); fallbackErr != nil {
					log.Printf("%s: skip fallback checkpoint ref delete thread=%s ref=%q workspace=%q: primary=%v fallback=%v", action, threadID, ref, fallbackWorkspace, err, fallbackErr)
					continue
				}
				fallbackRefs = append(fallbackRefs, ref)
			}
			if len(fallbackRefs) == 0 {
				continue
			}
			if fallbackErr := a.checkpointStore().DeleteRefs(ctx, fallbackWorkspace, fallbackRefs); fallbackErr != nil {
				log.Printf("%s: delete %d checkpoint refs failed thread=%s workspace=%q fallback=%q: primary=%v fallback=%v", action, len(fallbackRefs), threadID, workspace, fallbackWorkspace, err, fallbackErr)
			}
		}
	}
}

func (a *App) updateThreadAndInvalidateCheckpointsForWorkspaceChange(thread store.Thread, action, fallbackWorkspace string) error {
	refs, err := a.store.UpdateThreadAndDeleteCheckpoints(thread)
	if err != nil {
		return fmt.Errorf("%s: update thread and drop checkpoint rows: %w", action, err)
	}
	a.deleteCheckpointRefsBestEffort(thread.ID, action, fallbackWorkspace, refs)
	return nil
}

// knownCodexProviderTurnCountBefore counts the distinct AO turn
// indexes below beforeTurnIndex whose user message provably reached
// the provider (a stamped provider_item_id on a non-wire-only user
// row). resolveCodexForkAnchor uses it as the cross-check that an
// anchor miss really means "empty provider prefix" and not a
// legacy-data hole.
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
