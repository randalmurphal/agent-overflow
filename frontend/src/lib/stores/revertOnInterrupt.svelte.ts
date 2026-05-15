// Stop-button revert-on-interrupt entry point.
//
// When the user clicks Stop (or hits Esc) before the agent has produced
// any visible response, the user's message should be removed from the
// timeline and put back into the composer as a draft — matching Claude
// Code's TUI behavior. This module owns the FRONTEND predicate
// (canRevertEarlyInterrupt) and the unified flow that decides between
// the revert path and the plain-interrupt fallback.
//
// Architecture:
//   - Frontend predicate runs synchronously so the click handler can
//     paint instant optimistic state.
//   - Backend re-checks under the per-thread lock (SQLite + flush
//     queue) so a Send→Stop race resolves correctly. Result.reverted
//     tells the frontend which path the backend actually took.
//   - Backend emits `user_message:reverted` on success; events.ts
//     refreshes the composer draft so the user sees their text reappear
//     in the input.
//
// References:
//   - claude-code-source-code/src/components/REPL.tsx — the TUI's
//     `messagesAfterAreOnlySynthetic` predicate; only assistant_text
//     and tool_use rows block the revert.
//   - app_revert_on_interrupt.go — the backend method this dispatches
//     to and the predicate it re-runs under the lock.

import type { Item } from '../types/models';
import type { ThreadPane } from './thread.svelte';
import { getActiveTurn } from './threadStatuses.svelte';
import { getQueueForThread } from './sendQueue.svelte';
import { reportNonBenignInterruptError } from './interruptErrors';
import {
  InterruptAndRevertIfClean,
  InterruptTurn,
} from './bindings';

/**
 * Result of the frontend revert-eligibility predicate. `canRevert` is
 * true only when ALL of the following hold:
 *   - The thread has an active turn.
 *   - The composer is empty (user hasn't started typing again).
 *   - The send queue is empty (no queued follow-up to drain).
 *   - The latest turn contains exactly one revertable user_text row
 *     and no assistant_text / tool_call rows. Thinking blocks and
 *     synthetic error rows DO NOT block the revert (matches Claude
 *     Code's TUI semantics).
 *
 * When false, `reason` carries a short tag for debugging / telemetry.
 * When true, `userItem` is the row that will be removed optimistically.
 */
export interface RevertEligibility {
  canRevert: boolean;
  userItem?: Item;
  reason?: string;
}

interface DraftSnapshotInputs {
  content: string;
  attachments: { length: number };
  terminalChips: { length: number };
}

export function canRevertEarlyInterrupt(
  pane: ThreadPane,
  draft: DraftSnapshotInputs,
): RevertEligibility {
  const threadId = pane.threadId;
  if (!threadId) return { canRevert: false, reason: 'no thread' };
  const active = getActiveTurn(threadId);
  if (!active) return { canRevert: false, reason: 'no active turn' };

  // Composer not empty → user has started typing new text or carries
  // attachments / terminal chips from prior actions. Preserve their work
  // by falling back to the plain-interrupt path.
  if (draft.content !== '' || draft.attachments.length > 0 || draft.terminalChips.length > 0) {
    return { canRevert: false, reason: 'composer not empty' };
  }

  // A queued mid-round message represents user intent to keep sending
  // (steer / follow-up). Stop should let the queue drain through the
  // existing interrupt flow rather than discard everything.
  if (getQueueForThread(threadId).length > 0) {
    return { canRevert: false, reason: 'queue has pending items' };
  }

  // Scan items on the active turn. Only one user_text allowed; any
  // assistant_text or tool_call means the agent has produced visible
  // output and the revert would discard real work.
  const turnIndex = active.turnIndex;
  let userItem: Item | undefined;
  let userCount = 0;
  for (const item of pane.items) {
    if (item.turnIndex !== turnIndex) continue;
    if (item.kind === 'assistant_text' || item.kind === 'tool_call') {
      return { canRevert: false, reason: 'agent has responded' };
    }
    if (item.kind === 'user_text' && item.role === 'user') {
      userItem = item;
      userCount++;
    }
  }
  if (userCount === 0) return { canRevert: false, reason: 'no user_text item' };
  // Multiple user_text rows on one turn means the user steered mid-
  // round; reverting one would break ordering. Defer to plain interrupt.
  if (userCount > 1) return { canRevert: false, reason: 'turn has steered user messages' };

  return { canRevert: true, userItem };
}

/**
 * Unified Stop entry point shared by the Composer Stop button and the
 * `thread.interrupt` keybinding. Decides between revert-on-interrupt
 * and the plain-interrupt fallback, paints optimistic UI immediately,
 * and dispatches the matching backend RPC fire-and-forget. The plain-
 * interrupt branch matches the legacy `dispatchInterrupt` behavior;
 * the revert branch additionally removes the user row from the timeline
 * and rolls it back on rollback / error.
 *
 * The pane's active-turn and send-in-flight flags ARE NOT cleared here.
 * Callers (builtin command, Composer.interrupt) own that decision so
 * they can sequence it with their own per-call cleanup (user-input /
 * approval cancellations, etc).
 */
export function runInterruptOrRevert(
  pane: ThreadPane,
  draft: DraftSnapshotInputs,
): void {
  const threadId = pane.threadId;
  if (!threadId) return;

  const eligibility = canRevertEarlyInterrupt(pane, draft);

  if (!eligibility.canRevert) {
    void InterruptTurn(threadId).catch((err) =>
      reportNonBenignInterruptError(pane, err),
    );
    return;
  }

  const removedItem = eligibility.userItem
    ? pane.removeItemById(eligibility.userItem.id)
    : null;

  void InterruptAndRevertIfClean(threadId)
    .then((result) => {
      if (!result.reverted) {
        if (removedItem) pane.upsertItem(removedItem);
      }
      // Reverted=true: backend will emit `user_message:reverted` which
      // confirms the removal (idempotent) and refreshes the composer
      // draft. No additional client-side work required.
    })
    .catch((err) => {
      if (removedItem) pane.upsertItem(removedItem);
      reportNonBenignInterruptError(pane, err);
    });
}
