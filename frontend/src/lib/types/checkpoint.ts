// Backend-only Git refs, workspace paths, and checkpoint bookkeeping stay
// server-side; the generated CheckpointView is the frontend-visible DTO.
export type { CheckpointView as Checkpoint } from '../../../bindings/agent-overflow/models';

/** Diff panel view mode. */
export type DiffPanelTab = 'messages' | 'workspace';

export type RevertMode = 'conversation-and-files' | 'conversation-only';

/**
 * Activity event emitted via `checkpoint:captured` when the app snapshots the
 * workspace immediately before a real user message is sent.
 */
export interface CheckpointCapturedEvent {
  threadId: string;
  userItemId: string;
  turnIndex: number;
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
 * message still proceeds; the frontend surfaces this as a warning.
 */
export interface CheckpointErrorEvent {
  threadId: string;
  userItemId?: string;
  turnIndex: number;
  error: string;
}

/**
 * Activity event emitted via `checkpoint:reverted` after a successful
 * revert (either mode). The chip strip needs to refresh because the
 * post-revert checkpoint list has stale entries removed.
 */
export interface CheckpointRevertedEvent {
  threadId: string;
  userItemId: string;
  turnIndex: number;
  mode: RevertMode;
}
