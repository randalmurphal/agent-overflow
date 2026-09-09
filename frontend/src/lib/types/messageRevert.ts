import type { Item } from './models';
/**
 * Event emitted via `user_message:reverted` after a successful
 * conversation revert — the Stop/Esc un-send and the edit-and-resend
 * saga. The backend has truncated SQLite; this
 * event tells the frontend exactly how to mirror that cut:
 *
 * - Every item with `turnIndex > turnIndex` is gone.
 * - Within the anchor turn itself, exactly `keptAnchorTurnItemIds`
 *   survive; everything else in that turn is removed — including
 *   pane-only rows that were never persisted (survivors are persisted
 *   rows by definition, so an enumerated kept-set is a complete
 *   removal instruction; an enumerated removed-set would not be).
 *
 * Absent/empty `keptAnchorTurnItemIds` (the common case) means the
 * whole anchor turn is gone: Codex cuts are turn-granular, and a
 * Claude anchor that opens its turn keeps nothing. Non-empty only for
 * Claude item-granular cuts to a mid-turn anchor (queued/steered
 * message sharing its turn with an earlier prompt), where the backend's
 * promoted-row predicate decides the survivor slice — carried here as
 * data so UI code never re-derives it. `userItemId` is retained for
 * telemetry / debugging.
 */
export interface UserMessageRevertedEvent {
  replacement?: Item | null;
  itemEventSequence?: number;
  turnStartedSequence?: number;
  turnCompletedSequence?: number;
  threadId: string;
  userItemId: string;
  turnIndex: number;
  keptAnchorTurnItemIds?: string[];
  /** Replacement operations preserve composer WIP and do not rehydrate it. */
  draftPendingResend?: boolean;
  /**
   * Post-cut history stamps, read inside the cut transaction
   * (docs/architecture/thread-replica-sync.md §4). In-memory adoption only, for
   * the same reason `provider:turn_completed`'s stamps are: they are not
   * a full attestation of any window this client holds. Zero means "no
   * stamp".
   */
  historyRev?: number;
  historyEpoch?: number;
  /**
   * The page load whose edit-and-resend saga produced this cut, set only
   * alongside `draftPendingResend`. Every client applies the cut; only
   * the one that started the saga records the marker its own failure
   * handler consumes (`stores/eventsMessageRevert.ts`). Absent means no
   * screen was behind the call, or a backend too old to stamp it.
   */
  connectionId?: string;
}
