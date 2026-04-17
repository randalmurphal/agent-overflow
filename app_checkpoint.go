package main

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// GetTurnDiff returns the unified diff produced by a single turn.
//
// Semantics:
//   - Between the checkpoint captured at the START of turn N and the
//     checkpoint captured at the start of turn N+1 (if one exists), or
//   - Between the checkpoint at turn N and the current worktree if this is
//     the most recent captured turn.
//
// The frontend uses this to render "what did this turn change" diffs.
func (a *App) GetTurnDiff(threadID string, turnIndex int) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get turn diff: %w", err)
	}
	current, ok, err := a.store.GetCheckpoint(threadID, turnIndex)
	if err != nil {
		return "", fmt.Errorf("get turn diff: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("get turn diff: no checkpoint for thread %q turn %d", threadID, turnIndex)
	}

	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" {
		return "", errors.New("get turn diff: thread has no workspace path")
	}
	cpStore := a.checkpointStore()

	next, hasNext, err := a.store.GetCheckpoint(threadID, turnIndex+1)
	if err != nil {
		return "", fmt.Errorf("get turn diff: %w", err)
	}
	if hasNext {
		patch, err := cpStore.DiffRefToRef(context.Background(), workspace, current.RefName, next.RefName)
		if err != nil {
			return "", fmt.Errorf("get turn diff: %w", err)
		}
		return string(patch), nil
	}

	// This is the latest turn — diff against the current worktree so the UI
	// sees in-flight changes.
	patch, err := cpStore.DiffRefToWorktree(context.Background(), workspace, current.RefName)
	if err != nil {
		return "", fmt.Errorf("get turn diff: %w", err)
	}
	return string(patch), nil
}

// GetCheckpointToWorktreeDiff returns the diff between a specific checkpoint
// ref and the current worktree. Used by the three-source diff panel.
func (a *App) GetCheckpointToWorktreeDiff(threadID string, turnIndex int) (string, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return "", fmt.Errorf("get checkpoint diff: %w", err)
	}
	record, ok, err := a.store.GetCheckpoint(threadID, turnIndex)
	if err != nil {
		return "", fmt.Errorf("get checkpoint diff: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("get checkpoint diff: no checkpoint for thread %q turn %d", threadID, turnIndex)
	}
	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" {
		return "", errors.New("get checkpoint diff: thread has no workspace path")
	}
	patch, err := a.checkpointStore().DiffRefToWorktree(context.Background(), workspace, record.RefName)
	if err != nil {
		return "", fmt.Errorf("get checkpoint diff: %w", err)
	}
	return string(patch), nil
}

// RevertToTurn rolls the conversation back to a prior turn's checkpoint.
//
// Modes:
//   - "fork"    — create a new thread forked from the current thread at the
//                 checkpoint's turn. Workspace is left untouched. Always safe;
//                 the only safe option for Claude threads because Claude Code
//                 has no rollback primitive. Returns the new thread ID.
//   - "restore" — destructively restore the workspace from the checkpoint ref
//                 in-place. Requires a provider that supports conversation
//                 rollback (currently only Codex). Returns an empty string.
//
// Callers should have surfaced a warning to the user before invoking "restore";
// it will destroy untracked files added after the checkpoint.
func (a *App) RevertToTurn(threadID string, turnIndex int, mode string) (string, error) {
	switch mode {
	case "fork":
		return a.revertToTurnFork(threadID, turnIndex)
	case "restore":
		return "", a.revertToTurnRestore(threadID, turnIndex)
	default:
		return "", fmt.Errorf("revert to turn: unknown mode %q (want \"fork\" or \"restore\")", mode)
	}
}

func (a *App) revertToTurnFork(threadID string, turnIndex int) (string, error) {
	// Verify the checkpoint exists so we fail loudly before forking.
	if _, ok, err := a.store.GetCheckpoint(threadID, turnIndex); err != nil {
		return "", fmt.Errorf("revert to turn: %w", err)
	} else if !ok {
		return "", fmt.Errorf("revert to turn: no checkpoint for thread %q turn %d", threadID, turnIndex)
	}

	forked, err := a.ForkThread(threadID)
	if err != nil {
		return "", fmt.Errorf("revert to turn: %w", err)
	}
	return forked.ID, nil
}

func (a *App) revertToTurnRestore(threadID string, turnIndex int) error {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return fmt.Errorf("revert to turn: %w", err)
	}
	record, ok, err := a.store.GetCheckpoint(threadID, turnIndex)
	if err != nil {
		return fmt.Errorf("revert to turn: %w", err)
	}
	if !ok {
		return fmt.Errorf("revert to turn: no checkpoint for thread %q turn %d", threadID, turnIndex)
	}

	// Claude has no rollback primitive — require "fork" mode for Claude.
	if thread.Provider == string(provider.Claude) {
		return fmt.Errorf("revert to turn: Claude threads cannot be restored in place; use mode=\"fork\"")
	}

	workspace := checkpointWorkspaceForThread(thread)
	if workspace == "" {
		return errors.New("revert to turn: thread has no workspace path")
	}
	if err := a.checkpointStore().RestoreWorktree(context.Background(), workspace, record.RefName); err != nil {
		return fmt.Errorf("revert to turn: %w", err)
	}
	return nil
}

// ListThreadCheckpoints returns every persisted checkpoint row for a thread.
// Ordering is ascending by turn_index so the UI can render a turn-navigation
// strip without additional sorting.
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
	if t.WorktreePath != "" {
		return t.WorktreePath
	}
	return t.WorkspacePath
}
