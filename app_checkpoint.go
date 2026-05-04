package main

import (
	"context"
	"errors"
	"fmt"
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

// Revert mode strings exposed to the frontend. See AGENTS.md for the design.
const (
	RevertModeConversationAndFiles = "conversation-and-files"
	RevertModeConversationOnly     = "conversation-only"
)

// CheckpointView is the frontend-facing checkpoint row. The store row also
// carries backend-only Git ref/workspace fields; those stay server-side because
// the UI only needs turn ordering, status, file summaries, and tool-written
// paths.
type CheckpointView struct {
	ID                  string             `json:"id"`
	ThreadID            string             `json:"threadId"`
	CheckpointTurnCount int                `json:"checkpointTurnCount"`
	Status              string             `json:"status"`
	Files               []diffsummary.File `json:"files"`
	ToolPaths           []string           `json:"toolPaths"`
	CapturedAt          int64              `json:"capturedAt"`
}

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

// GetSessionAgentDiff returns the unified diff between the thread's
// baseline checkpoint (turn count 0) and the latest checkpoint, restricted
// to the cumulative set of paths the agent's file-mutating tools wrote
// across the session. Empty result when no checkpoints exist or no agent
// writes have been recorded.
func (a *App) GetSessionAgentDiff(threadID string) (string, error) {
	if _, err := a.store.GetThread(threadID); err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	checkpoints, err := a.store.ListCheckpoints(threadID)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	if len(checkpoints) == 0 {
		return "", nil
	}
	baseline := checkpoints[0]
	latest := checkpoints[len(checkpoints)-1]
	if baseline.CheckpointTurnCount == latest.CheckpointTurnCount {
		// Only the baseline exists — nothing to diff against.
		return "", nil
	}
	if err := validateCheckpointRecordForThread("get session agent diff", threadID, baseline); err != nil {
		return "", err
	}
	if err := validateCheckpointRecordForThread("get session agent diff", threadID, latest); err != nil {
		return "", err
	}
	if baseline.WorkspacePath != latest.WorkspacePath {
		return "", fmt.Errorf("get session agent diff: checkpoint workspaces differ: %q != %q", baseline.WorkspacePath, latest.WorkspacePath)
	}
	paths, err := a.store.GetCumulativeToolPaths(threadID, baseline.CheckpointTurnCount)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	if len(paths) == 0 {
		return "", nil
	}
	patch, err := a.checkpointStore().DiffRefToRefScoped(
		context.Background(), baseline.WorkspacePath, baseline.RefName, latest.RefName, paths,
	)
	if err != nil {
		return "", fmt.Errorf("get session agent diff: %w", err)
	}
	return string(patch), nil
}

// GetWorkspaceCurrentDiff returns the full uncommitted diff in the
// thread's workspace — `git diff HEAD` plus untracked-not-ignored files —
// without filtering by tool_paths. Surfaces manual user edits alongside
// any post-checkpoint agent activity.
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
		// Path-scoped restore: only the paths the agent's tools wrote
		// after this checkpoint get rolled back. Manual edits to other
		// files survive. The cumulative set is the union of every
		// post-checkpoint row's tool_paths (deduped + sorted).
		cumulative, err := a.store.GetCumulativeToolPaths(threadID, checkpointTurnCount)
		if err != nil {
			return fmt.Errorf("revert checkpoint: cumulative paths: %w", err)
		}
		if err := a.checkpointStore().RestoreWorktreePaths(context.Background(), workspace, record.RefName, cumulative); err != nil {
			return fmt.Errorf("revert checkpoint: restore paths: %w", err)
		}
		// Bust the workspace file cache so the @-mention picker reflects
		// the post-restore tree on the next composer interaction.
		// Mirrors t3-code's `workspaceEntries.invalidate(cwd)` after
		// checkpoint restore (CheckpointReactor.ts:670). Skipped on the
		// conversation-only path because files are unchanged.
		if a.workspaceFiles != nil {
			a.workspaceFiles.Invalidate(workspace)
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
		return a.revertClaudeThread(thread, lastKeptTurnIndex)
	default:
		return fmt.Errorf("unsupported provider %q", thread.Provider)
	}
}

// revertClaudeThread truncates a Claude session in place by slicing the
// source JSONL at the end of lastKeptTurnIndex's turn and pointing the
// thread's SessionRef at the new <newID>.jsonl. The next session start
// resumes from the truncated transcript with full prior context intact —
// matching how t3-code's adapter does in-memory truncation, just at the
// JSONL level since we use the CLI subprocess and don't own the
// conversation array.
//
// lastKeptTurnIndex < 0 (i.e. revert all the way to baseline) clears the
// session refs entirely; the next turn starts a fresh session.
//
// The old JSONL is left on disk so the discarded conversation is
// recoverable via Claude's own session picker. Disk growth is bounded
// by user behavior; no GC is needed for v1.
func (a *App) revertClaudeThread(thread store.Thread, lastKeptTurnIndex int) error {
	if lastKeptTurnIndex < 0 {
		// Nothing to keep — fresh session on next turn.
		thread.SessionRef = ""
		thread.PendingForkRef = ""
		thread.UpdatedAt = time.Now().UnixMilli()
		return a.store.UpdateThread(thread)
	}

	if thread.SessionRef == "" {
		// No session to truncate; treat as already-cleared.
		return nil
	}

	srcPath, err := sessionfork.LocateSessionFile(thread.SessionRef, thread.WorkspacePath)
	if err != nil {
		// Session file isn't on disk (deleted, moved, or workspace
		// rearranged). Fall back to the old "clear refs" behavior so the
		// timeline-truncation half of the revert still succeeds — the
		// next turn just won't have prior context, which matches what
		// the user got before this code path landed.
		if errors.Is(err, sessionfork.ErrSessionFileNotFound) {
			thread.SessionRef = ""
			thread.PendingForkRef = ""
			thread.UpdatedAt = time.Now().UnixMilli()
			return a.store.UpdateThread(thread)
		}
		return fmt.Errorf("locate claude session: %w", err)
	}

	// Single-pass: open the source JSONL once and combine slice-point
	// computation with the write. Avoids the double-open TOCTOU
	// window. lastKeptTurnIndex < 0 (revert all the way to baseline)
	// would have been short-circuited above to the clear-refs branch.
	newID, newPath, err := sessionfork.WriteForkFileForLastKeptTurn(srcPath, lastKeptTurnIndex, "")
	if err != nil {
		return fmt.Errorf("write reverted session: %w", err)
	}

	thread.SessionRef = newID
	thread.PendingForkRef = ""
	thread.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.UpdateThread(thread); err != nil {
		// Roll back the JSONL we just wrote so disk and SQLite stay
		// in lockstep — either both reflect the new session, or neither.
		_ = os.Remove(newPath)
		return fmt.Errorf("persist reverted claude state: %w", err)
	}
	return nil
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

// ListThreadCheckpoints returns every frontend-visible checkpoint view for a thread.
// Ordering is ascending by checkpoint turn count so the UI can render a
// turn-navigation strip without additional sorting.
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
		ID:                  row.ID,
		ThreadID:            row.ThreadID,
		CheckpointTurnCount: row.CheckpointTurnCount,
		Status:              row.Status,
		Files:               row.Files,
		ToolPaths:           row.ToolPaths,
		CapturedAt:          row.CapturedAt,
	}
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
