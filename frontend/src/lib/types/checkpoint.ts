// Mirrors internal/store.Checkpoint. Persisted alongside each turn's hidden
// Git ref; lets the diff panel resolve a (thread, turn) into a ref name.
export interface Checkpoint {
  id: string;
  threadId: string;
  turnIndex: number;
  checkpointTurnCount: number;
  turnId?: string;
  refName: string;
  baselineSha?: string;
  status: string;
  files: Array<{
    path: string;
    kind: string;
    additions: number;
    deletions: number;
  }>;
  /**
   * Workspace-relative paths the agent's file-mutating tools wrote during
   * the turn this checkpoint closes. Empty for the baseline (turn count 0)
   * and for any turn where the agent didn't run an Edit / Write /
   * MultiEdit / NotebookEdit (Claude) or a fileChange tool (Codex). Bash
   * side effects are intentionally not tracked.
   */
  toolPaths: string[];
  assistantMessageId?: string;
  completedAt?: number;
  capturedAt: number;
  workspacePath: string;
}

/** Diff panel view mode. */
export type DiffPanelTab = 'per-turn' | 'session' | 'workspace';

export type RevertMode = 'conversation-and-files' | 'conversation-only';

/**
 * Activity event emitted via `checkpoint:captured` when the triage layer
 * successfully snapshots a turn-start baseline.
 */
export interface CheckpointCapturedEvent {
  threadId: string;
  turnIndex: number;
  checkpointTurnCount: number;
  refName: string;
  capturedAt: number;
}

/**
 * Activity event emitted via `checkpoint:unavailable` when the workspace
 * isn't a git repo. The frontend uses this to hide diff/revert controls.
 */
export interface CheckpointUnavailableEvent {
  threadId: string;
  reason: string;
}

/**
 * Activity event emitted via `checkpoint:error` when capture failed. The
 * turn still proceeds; the frontend surfaces this as a warning.
 */
export interface CheckpointErrorEvent {
  threadId: string;
  turnIndex: number;
  checkpointTurnCount: number;
  error: string;
}

/**
 * Activity event emitted via `checkpoint:reverted` after a successful
 * revert (either mode). The chip strip needs to refresh because the
 * post-revert turn count has stale entries removed.
 */
export interface CheckpointRevertedEvent {
  threadId: string;
  turnIndex: number;
  checkpointTurnCount: number;
  mode: RevertMode;
}
