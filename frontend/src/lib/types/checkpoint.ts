// Mirrors internal/store.Checkpoint. Persisted alongside each turn's hidden
// Git ref; lets the diff panel resolve a (thread, turn) into a ref name.
export interface Checkpoint {
  id: string;
  threadId: string;
  turnIndex: number;
  refName: string;
  baselineSha?: string;
  capturedAt: number;
  workspacePath: string;
}

/** Modes accepted by RevertToTurn. */
export type RevertMode = 'fork' | 'restore';

/**
 * Activity event emitted via `checkpoint:captured` when the triage layer
 * successfully snapshots a turn-start baseline.
 */
export interface CheckpointCapturedEvent {
  threadId: string;
  turnIndex: number;
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
  error: string;
}
