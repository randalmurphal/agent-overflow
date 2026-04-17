package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

// Revert mode strings exposed to the frontend. See AGENTS.md for the design.
const (
	RevertModeFork         = "fork"
	RevertModeBoth         = "revert-both"
	RevertModeConversation = "revert-conversation"
	RevertModeCode         = "revert-code"
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

// RevertToTurn rolls a thread back to a prior turn's captured state. The
// checkpoint at turnIndex records the state *before* turn turnIndex ran, so
// every revert mode drops turn turnIndex and everything after it.
//
// Modes:
//   - "fork": create a new thread forked from this one at the checkpoint,
//     leave the source thread untouched. Returns the new thread ID.
//   - "revert-both": in-place revert of both conversation history and
//     working tree.
//   - "revert-conversation": in-place revert of conversation history only
//     (working tree untouched). Codex uses its native thread/rollback;
//     Claude clears the session ref so the next message starts fresh.
//   - "revert-code": restore the working tree only, leave conversation
//     history and provider session state untouched.
//
// All in-place modes stop the active session first. Fork does not touch
// the source session.
func (a *App) RevertToTurn(threadID string, turnIndex int, mode string) (string, error) {
	switch mode {
	case RevertModeFork:
		return a.revertToTurnFork(threadID, turnIndex)
	case RevertModeBoth:
		return "", a.revertInPlace(threadID, turnIndex, true, true)
	case RevertModeConversation:
		return "", a.revertInPlace(threadID, turnIndex, true, false)
	case RevertModeCode:
		return "", a.revertInPlace(threadID, turnIndex, false, true)
	default:
		return "", fmt.Errorf("revert to turn: unknown mode %q (want %q, %q, %q, or %q)",
			mode, RevertModeFork, RevertModeBoth, RevertModeConversation, RevertModeCode)
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

// revertInPlace handles the three in-place revert modes in one path. The
// conversation and code booleans pick which side-effects fire; the rest of
// the procedure is shared.
func (a *App) revertInPlace(threadID string, turnIndex int, revertConversation, revertCode bool) error {
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

	// Always stop the active session before mutating provider or worktree
	// state. A running turn during revert produces undefined interleavings.
	if err := a.stopSession(threadID); err != nil {
		return fmt.Errorf("revert to turn: stop session: %w", err)
	}

	if revertConversation {
		if err := a.revertProviderConversation(thread, turnIndex); err != nil {
			return fmt.Errorf("revert to turn: %w", err)
		}
	}

	if revertCode {
		workspace := checkpointWorkspaceForThread(thread)
		if workspace == "" {
			return errors.New("revert to turn: thread has no workspace path")
		}
		if err := a.checkpointStore().RestoreWorktree(context.Background(), workspace, record.RefName); err != nil {
			return fmt.Errorf("revert to turn: restore worktree: %w", err)
		}
	}

	// Truncate items and forward checkpoints *only* when the conversation
	// side was reverted. Code-only revert leaves the timeline and provider
	// state intact so the user can keep iterating on the same turns against
	// the restored tree.
	if revertConversation {
		if _, err := a.store.DeleteItemsAfterTurn(threadID, turnIndex-1); err != nil {
			return fmt.Errorf("revert to turn: truncate items: %w", err)
		}
		if _, err := a.store.DeleteCheckpointsAfterTurn(threadID, turnIndex); err != nil {
			return fmt.Errorf("revert to turn: truncate checkpoints: %w", err)
		}
	}

	return nil
}

// revertProviderConversation performs the provider-specific side of an
// in-place conversation rollback. The session is assumed stopped.
func (a *App) revertProviderConversation(thread store.Thread, turnIndex int) error {
	lastTurn, err := a.store.LastTurnIndex(thread.ID)
	if err != nil {
		return fmt.Errorf("determine last turn: %w", err)
	}
	numTurns := lastTurn + 1 - turnIndex
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
