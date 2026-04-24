package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// Revert mode strings exposed to the frontend. See AGENTS.md for the design.
const (
	RevertModeConversationAndFiles = "conversation-and-files"
	RevertModeConversationOnly     = "conversation-only"
)

// GetCheckpointRangeDiff returns the unified diff between two finalized
// checkpoint turn counts. Turn count 0 is the baseline before the first turn;
// turn count N is the workspace after completed turn N.
func (a *App) GetCheckpointRangeDiff(threadID string, fromTurnCount int, toTurnCount int) (string, error) {
	if fromTurnCount < 0 || toTurnCount < 0 {
		return "", errors.New("get checkpoint range diff: turn counts must be non-negative")
	}
	if fromTurnCount > toTurnCount {
		return "", errors.New("get checkpoint range diff: from turn must be <= to turn")
	}
	if fromTurnCount == toTurnCount {
		return "", nil
	}

	if _, err := a.store.GetThread(threadID); err != nil {
		return "", fmt.Errorf("get checkpoint range diff: %w", err)
	}
	from, ok, err := a.store.GetCheckpointByTurnCount(threadID, fromTurnCount)
	if err != nil {
		return "", fmt.Errorf("get checkpoint range diff: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("get checkpoint range diff: no checkpoint for thread %q turn count %d", threadID, fromTurnCount)
	}
	to, ok, err := a.store.GetCheckpointByTurnCount(threadID, toTurnCount)
	if err != nil {
		return "", fmt.Errorf("get checkpoint range diff: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("get checkpoint range diff: no checkpoint for thread %q turn count %d", threadID, toTurnCount)
	}
	if err := validateCheckpointRecordForThread("get checkpoint range diff", threadID, from); err != nil {
		return "", err
	}
	if err := validateCheckpointRecordForThread("get checkpoint range diff", threadID, to); err != nil {
		return "", err
	}
	if from.WorkspacePath != to.WorkspacePath {
		return "", fmt.Errorf("get checkpoint range diff: checkpoint workspaces differ: %q != %q", from.WorkspacePath, to.WorkspacePath)
	}
	patch, err := a.checkpointStore().DiffRefToRef(context.Background(), from.WorkspacePath, from.RefName, to.RefName)
	if err != nil {
		return "", fmt.Errorf("get checkpoint range diff: %w", err)
	}
	return string(patch), nil
}

// RevertToCheckpoint rolls a thread back to a checkpoint turn count. The
// conversation-only mode keeps the current worktree and recaptures that kept
// state as the new checkpoint baseline.
func (a *App) RevertToCheckpoint(threadID string, checkpointTurnCount int, mode string) error {
	if checkpointTurnCount < 0 {
		return errors.New("revert checkpoint: turn count must be non-negative")
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
	record, ok, err := a.store.GetCheckpointByTurnCount(threadID, checkpointTurnCount)
	if err != nil {
		return fmt.Errorf("revert checkpoint: %w", err)
	}
	if !ok {
		return fmt.Errorf("revert checkpoint: no checkpoint for thread %q turn count %d", threadID, checkpointTurnCount)
	}
	if err := validateCheckpointRecordForThread("revert checkpoint", threadID, record); err != nil {
		return err
	}

	if err := a.stopSession(threadID); err != nil {
		return fmt.Errorf("revert checkpoint: stop session: %w", err)
	}

	if err := a.revertProviderConversation(thread, checkpointTurnCount); err != nil {
		return fmt.Errorf("revert checkpoint: %w", err)
	}

	if mode == RevertModeConversationAndFiles {
		workspace := checkpointWorkspaceForThread(thread)
		if workspace == "" {
			return errors.New("revert checkpoint: thread has no workspace path")
		}
		if record.WorkspacePath != workspace {
			return fmt.Errorf("revert checkpoint: checkpoint workspace %q does not match thread workspace %q", record.WorkspacePath, workspace)
		}
		if err := a.checkpointStore().RestoreWorktree(context.Background(), workspace, record.RefName); err != nil {
			return fmt.Errorf("revert checkpoint: restore worktree: %w", err)
		}
	}

	lastKeptTurnIndex := checkpointTurnCount - 1
	if _, err := a.store.DeleteItemsAfterTurn(threadID, lastKeptTurnIndex); err != nil {
		return fmt.Errorf("revert checkpoint: truncate items: %w", err)
	}
	refs, err := a.store.DeleteCheckpointsAfterTurn(threadID, checkpointTurnCount)
	if err != nil {
		return fmt.Errorf("revert checkpoint: truncate checkpoints: %w", err)
	}
	if err := a.deleteCheckpointRefs(context.Background(), threadID, "revert checkpoint", refs); err != nil {
		return err
	}
	if mode == RevertModeConversationOnly {
		if err := a.rebaseCheckpointToCurrentWorkspace(thread, record); err != nil {
			return err
		}
	}

	a.emit("checkpoint:reverted", map[string]any{
		"threadId":            threadID,
		"turnIndex":           checkpointTurnCount,
		"checkpointTurnCount": checkpointTurnCount,
		"mode":                mode,
	})
	return nil
}

// revertProviderConversation performs the provider-specific side of an
// in-place conversation rollback. The session is assumed stopped.
func (a *App) revertProviderConversation(thread store.Thread, checkpointTurnCount int) error {
	lastTurn, err := a.store.LastTurnIndex(thread.ID)
	if err != nil {
		return fmt.Errorf("determine last turn: %w", err)
	}
	lastKeptTurnIndex := checkpointTurnCount - 1
	numTurns := lastTurn - lastKeptTurnIndex
	if numTurns < 1 {
		// Nothing to roll back — target is at or past the tail.
		return nil
	}

	switch thread.Provider {
	case string(provider.Codex):
		return a.rollbackCodexThread(thread, numTurns)
	case string(provider.Claude):
		// Claude has no wire-level rollback. Clear the session ref so the
		// next turn starts a fresh session with no provider-side context.
		// The old session file on disk is left in place — callers can
		// surface it later via a "previous session" list if desired.
		thread.SessionRef = ""
		thread.PendingForkRef = ""
		thread.UpdatedAt = time.Now().UnixMilli()
		return a.store.UpdateThread(thread)
	default:
		return fmt.Errorf("unsupported provider %q", thread.Provider)
	}
}

func (a *App) rebaseCheckpointToCurrentWorkspace(thread store.Thread, previous store.Checkpoint) error {
	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" {
		return errors.New("revert checkpoint: thread has no workspace path")
	}
	ref := checkpoint.ThreadRefPrefix(thread.ID) + "rebase/" + uuid.NewString()
	if err := a.checkpointStore().CaptureRef(context.Background(), workspace, ref); err != nil {
		return fmt.Errorf("revert checkpoint: capture rebased checkpoint: %w", err)
	}
	now := time.Now().UnixMilli()
	record := previous
	record.ID = uuid.NewString()
	record.RefName = ref
	record.Status = "ready"
	record.Files = nil
	record.CapturedAt = now
	record.WorkspacePath = workspace
	staleRef, err := a.store.ReplaceCheckpointByTurnCount(record)
	if err != nil {
		_ = a.checkpointStore().DeleteRef(context.Background(), workspace, ref)
		return fmt.Errorf("revert checkpoint: persist rebased checkpoint: %w", err)
	}
	if staleRef.RefName != "" && staleRef.RefName != ref {
		if err := validateCheckpointRefForThread("revert checkpoint", thread.ID, staleRef); err != nil {
			return err
		}
		if err := a.checkpointStore().DeleteRef(context.Background(), staleRef.WorkspacePath, staleRef.RefName); err != nil {
			return fmt.Errorf("revert checkpoint: delete rebased ref %s: %w", staleRef.RefName, err)
		}
	}
	return nil
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

// rollbackCodexThread calls thread/rollback on the Codex session, either
// using the live session if one is still active or by resuming a short-lived
// temp session for the duration of the call. Mirrors the active-or-temp
// pattern used by forkCodexThread in app_thread_fork.go.
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

// ListThreadCheckpoints returns every persisted checkpoint row for a thread.
// Ordering is ascending by checkpoint turn count so the UI can render a
// turn-navigation strip without additional sorting.
func (a *App) ListThreadCheckpoints(threadID string) ([]store.Checkpoint, error) {
	list, err := a.store.ListCheckpoints(threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread checkpoints: %w", err)
	}
	return list, nil
}

// checkpointStore returns the App's checkpoint.Store, creating one on demand
// if the service wasn't initialized. The store is stateless (all state lives
// in Git + SQLite) so spinning up a fresh instance in tests is safe.
func (a *App) checkpointStore() *checkpoint.Store {
	if a.checkpoints != nil {
		return a.checkpoints
	}
	return checkpoint.NewStore()
}

// checkpointWorkspaceForThread picks the right directory to snapshot. Mirrors
// the helper in internal/triage so backend and binding code agree.
func checkpointWorkspaceForThread(t store.Thread) string {
	return t.WorkspacePath
}
