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
  assistantMessageId?: string;
  completedAt?: number;
  capturedAt: number;
  workspacePath: string;
}

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
