/**
 * Event emitted via `user_message:reverted` after a successful
 * revert-on-interrupt (the "Stop before agent responds" flow). The
 * backend has truncated SQLite at `turnIndex` (inclusive — every
 * sibling row at that turn is gone, not just the user message). The
 * frontend handler mirrors that truncate against `pane.items` and
 * reloads the composer draft so the user's text reappears in the
 * input. `userItemId` is retained for telemetry / debugging; the
 * `turnIndex` is the source of truth for what to remove.
 */
export interface UserMessageRevertedEvent {
  threadId: string;
  userItemId: string;
  turnIndex: number;
}
