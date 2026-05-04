// Backend-only Git refs, workspace paths, and checkpoint bookkeeping stay
// server-side; the generated CheckpointView is the frontend-visible DTO.
export type { CheckpointView as Checkpoint } from '../../../bindings/agent-overflow/models';

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
