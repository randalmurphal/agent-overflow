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

/**
 * Modes accepted by RevertToTurn. The four modes map to the backend
 * constants in app_checkpoint.go:
 *
 * - `fork`: create a new thread forked at this turn; leave the source
 *   thread and its worktree untouched.
 * - `revert-both`: in-place revert of both conversation history and
 *   working tree to the captured state.
 * - `revert-conversation`: in-place revert of conversation history only;
 *   worktree is untouched.
 * - `revert-code`: restore the worktree only; conversation history and
 *   provider session state are untouched.
 */
export type RevertMode = 'fork' | 'revert-both' | 'revert-conversation' | 'revert-code';

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
