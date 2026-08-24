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
//   - The tray preflight runs before any optimistic timeline mutation
//     because reverting a provider session would kill background work.
//     Backend success later confirms the durable draft; backend
//     decline/error clears the optimistic restore if the user has not
//     already edited it.
//
// References:
//   - claude-code-source-code/src/components/REPL.tsx — the TUI's
//     `messagesAfterAreOnlySynthetic` predicate; only assistant_text
//     and tool_use rows block the revert.
//   - app_revert_on_interrupt.go — the backend method this dispatches
//     to and the predicate it re-runs under the lock.

import type { Attachment } from '../types/attachment';
import type { TerminalChip } from '../types/draft';
import type { Item } from '../types/models';
import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import type { ThreadPane } from './thread.svelte';
import { isReaderAuthoredUserText } from '../utils/userMessageMeta';
import { restoredDraftSnapshotFromUserItem } from '../utils/userMessageDraftSnapshot';
import { getActiveTurn } from './threadStatuses.svelte';
import { getQueueForThread } from './sendQueue.svelte';
import { reportNonBenignInterruptError } from './interruptErrors';
import { applyUserMessageReverted } from './eventsMessageRevert';
import {
  beginThreadInterrupt,
  finishThreadInterrupt,
} from './threadInterruptState.svelte';
import {
  CountRunningBackgroundTasks,
  InterruptAndRevertIfClean,
  InterruptTurn,
} from './bindings';

/**
 * Result of the frontend revert-eligibility predicate. Discriminated
 * on `canRevert` so callers don't have to defensively re-check
 * `userItem`: when the predicate says yes, the user row is guaranteed.
 *
 * `canRevert` is true only when ALL of the following hold:
 *   - The thread has an active turn.
 *   - The composer is empty (user hasn't started typing again).
 *   - The send queue is empty (no queued follow-up to drain).
 *   - The latest turn contains exactly one revertable user_text row
 *     and no assistant_text / tool_call rows. Thinking blocks and
 *     synthetic error rows DO NOT block the revert (matches Claude
 *     Code's TUI semantics).
 */
export type RevertEligibility =
  | { canRevert: true; userItem: Item }
  | { canRevert: false; reason: string };

interface DraftSnapshotInputs {
  content: string;
  attachments: Attachment[] | { length: number };
  terminalChips: TerminalChip[] | { length: number };
  applyOptimisticRestoredDraft?: (threadId: string, snapshot: ComposerDraftSnapshot) => void;
  clearOptimisticRestoredDraft?: (threadId: string, snapshot: ComposerDraftSnapshot) => void;
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
    if (isReaderAuthoredUserText(item) && item.role === 'user') {
      // Reader-authored only. A subagent's own prompt is a user_text row
      // carrying the LAUNCH's turn index, so it sits in this turn and a
      // bare kind test would count it as a mid-round steer. Today the
      // launch's own tool_call returns above before that can happen; the
      // predicate is here so the answer stays right if that ever moves,
      // and because "did the reader write this" has one definition.
      userItem = item;
      userCount++;
    }
  }
  if (userCount === 0 || !userItem) return { canRevert: false, reason: 'no user_text item' };
  // Multiple user_text rows on one turn means the user steered mid-
  // round; reverting one would break ordering. Defer to plain interrupt.
  if (userCount > 1) return { canRevert: false, reason: 'turn has steered user messages' };

  return { canRevert: true, userItem };
}

/**
 * Unified Stop entry point shared by the Composer Stop button and the
 * `thread.interrupt` keybinding. Decides between revert-on-interrupt
 * and the plain-interrupt fallback, then dispatches the matching
 * backend RPC fire-and-forget. The plain-
 * interrupt branch matches the legacy `dispatchInterrupt` behavior;
 * the revert branch first checks the live background tray, then
 * truncates the active turn from the timeline (matching the backend's
 * `DeleteConversationFromTurn` — inclusive at turnIndex — so synthetic
 * siblings like thinking / api_retry / error rows go with the user row)
 * and rolls everything back on rollback / error.
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
  const interruptToken = beginThreadInterrupt(threadId);
  if (interruptToken === null) return;

  const eligibility = canRevertEarlyInterrupt(pane, draft);

  if (!eligibility.canRevert) {
    void runPlainInterrupt(pane, threadId, interruptToken);
    return;
  }

  void runInterruptOrRevertAfterBackgroundPreflight(
    pane,
    draft,
    threadId,
    eligibility.userItem,
    interruptToken,
  );
}

async function runPlainInterrupt(
  pane: ThreadPane,
  threadId: string,
  interruptToken: number,
): Promise<void> {
  try {
    await InterruptTurn(threadId);
  } catch (err) {
    reportNonBenignInterruptError(pane, err);
  } finally {
    finishThreadInterrupt(threadId, interruptToken);
  }
}

async function runInterruptOrRevertAfterBackgroundPreflight(
  pane: ThreadPane,
  draft: DraftSnapshotInputs,
  threadId: string,
  userItem: Item,
  interruptToken: number,
): Promise<void> {
  let backgroundCount = 0;
  try {
    backgroundCount = Number(await CountRunningBackgroundTasks(threadId));
  } catch (err) {
    reportNonBenignInterruptError(pane, err);
    await runPlainInterrupt(pane, threadId, interruptToken);
    return;
  }
  if (backgroundCount > 0) {
    await runPlainInterrupt(pane, threadId, interruptToken);
    return;
  }

  // Match the backend truncate: remove EVERY item on the active turn,
  // not just the user_text. Stranded thinking / api_retry / error rows
  // are the visible symptom of doing this piecewise. The rollback path
  // restores the full set via `upsertItems` when the backend refuses
  // the revert (predicate raced).
  const removedItems = pane.removeItemsFromTurn(userItem.turnIndex);
  const shouldRestoreDraft = Boolean(
    draft.applyOptimisticRestoredDraft || draft.clearOptimisticRestoredDraft,
  );
  const restoredDraft = shouldRestoreDraft
    ? restoredDraftSnapshotFromUserItem(userItem)
    : null;
  if (restoredDraft) {
    draft.applyOptimisticRestoredDraft?.(threadId, restoredDraft);
  }

  let result: Awaited<ReturnType<typeof InterruptAndRevertIfClean>>;
  try {
    result = await InterruptAndRevertIfClean(threadId);
  } catch (err) {
    if (removedItems.length > 0) pane.upsertItems(removedItems);
    if (restoredDraft) {
      draft.clearOptimisticRestoredDraft?.(threadId, restoredDraft);
    }
    finishThreadInterrupt(threadId, interruptToken);
    reportNonBenignInterruptError(pane, err);
    return;
  }

  if (!result.reverted) {
    if (removedItems.length > 0) pane.upsertItems(removedItems);
    if (restoredDraft) {
      draft.clearOptimisticRestoredDraft?.(threadId, restoredDraft);
    }
    finishThreadInterrupt(threadId, interruptToken);
    return;
  }

  // The event bus coalesces frames independently from RPC responses, so the
  // response can arrive first. Apply its identical authoritative cut before
  // releasing Send; eventsMessageRevert deduplicates the later event by its
  // post-cut history stamp. A missing cut is a server contract breach. Keep
  // Send closed because restoring or re-enabling here could race a cut that
  // has committed but has not reached this client.
  if (!result.userItemId
    || result.userItemId !== userItem.id
    || typeof result.turnIndex !== 'number'
    || result.turnIndex !== userItem.turnIndex
    || typeof result.historyEpoch !== 'number'
    || typeof result.historyRev !== 'number'
    || !Number.isFinite(result.historyEpoch)
    || !Number.isFinite(result.historyRev)
    || (result.historyEpoch <= 0 && result.historyRev <= 0)) {
    reportNonBenignInterruptError(
      pane,
      new Error('interrupt-and-revert completed without its authoritative cut fields'),
    );
    return;
  }
  applyUserMessageReverted({
    threadId,
    userItemId: result.userItemId,
    turnIndex: result.turnIndex,
    keptAnchorTurnItemIds: result.keptAnchorTurnItemIds,
    historyEpoch: result.historyEpoch,
    historyRev: result.historyRev,
  });
  // Caller-owned release, after its exact RPC cut has been applied. A revert
  // event from another client on the same thread must not release this
  // operation while its own RPC is still queued behind that client.
  finishThreadInterrupt(threadId, interruptToken);
}
