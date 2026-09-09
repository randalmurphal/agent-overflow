import { conversationMutationReconciler } from './reconcileConversationMutation';
import { isTransportClassError } from './transportStatus.svelte';
import { getUndoableSend, retireUndoableSend } from './composerSendUndo';
import { parseUserMessageMeta } from '../utils/userMessageMeta';
// Early Stop restores presentation synchronously. The original send, background
// checks and provider rollback finish behind a thread-scoped Send gate. A refusal
// restores the transcript; a committed cut fences delayed item and turn events.

import type { Attachment } from '../types/attachment';
import type { TerminalChip } from '../types/draft';
import type { Item } from '../types/models';
import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import type { ErrorSurface, ThreadPaneIngest } from './threadPaneRoles';
import { isReaderAuthoredUserText } from '../utils/userMessageMeta';
import { restoredDraftSnapshotFromUserItem } from '../utils/userMessageDraftSnapshot';
import { getActiveTurn } from './threadStatuses.svelte';
import { getQueueForThread } from './sendQueue.svelte';
import { reportNonBenignInterruptError } from './interruptErrors';
import { applyUserMessageReverted } from './eventsMessageRevert';
import {
  beginThreadInterrupt,
  isThreadInterruptCurrent,
  finishThreadInterrupt,
  presentThreadInterruptAsRestored,
  restoreThreadInterruptPresentation,
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

/**
 * What this flow reads off a pane: the ingest surface (timeline reads,
 * the optimistic truncate/restore pair) plus the error banner writer the
 * shared interrupt error filter lands on. Callers pass a whole
 * `ThreadPane`; the type names the slice actually used, so a new member
 * use here fails to compile until threadPaneRoles.ts lists it.
 */
type InterruptPane = ThreadPaneIngest & Pick<ErrorSurface, 'setGeneralError'>;

interface DraftSnapshotInputs {
  content: string;
  attachments: Attachment[] | { length: number };
  terminalChips: TerminalChip[] | { length: number };
  applyOptimisticRestoredDraft?: (threadId: string, snapshot: ComposerDraftSnapshot) => void;
  settleOptimisticRestoredDraft?: (threadId: string) => Promise<void>;
  clearOptimisticRestoredDraft?: (threadId: string, snapshot: ComposerDraftSnapshot) => void;
}

export function canRevertEarlyInterrupt(
  pane: InterruptPane,
  draft: DraftSnapshotInputs,
): RevertEligibility {
  const threadId = pane.threadId;
  if (!threadId) return { canRevert: false, reason: 'no thread' };
  const active = getActiveTurn(threadId);
  const pending = getUndoableSend(threadId);
  if (!active && !pending) return { canRevert: false, reason: 'no active turn' };

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
  const turnIndex = active?.turnIndex ?? pane.items.find((item) =>
    parseUserMessageMeta(item.meta).sendId === pending?.sendId)?.turnIndex;
  if (turnIndex === undefined) return { canRevert: false, reason: 'no pending user message' };
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

/** Returns true when the early un-send owns the local status projection. */
export function runInterruptOrRevert(
  pane: InterruptPane,
  draft: DraftSnapshotInputs,
): boolean {
  const threadId = pane.threadId;
  if (!threadId) return false;
  const interruptToken = beginThreadInterrupt(threadId);
  if (interruptToken === null) return true;

  const eligibility = canRevertEarlyInterrupt(pane, draft);

  if (!eligibility.canRevert) {
    void runPlainInterrupt(pane, threadId, interruptToken);
    return false;
  }

  void runEarlyInterrupt(
    pane,
    draft,
    threadId,
    eligibility.userItem,
    interruptToken,
  );
  return true;
}

async function runPlainInterrupt(
  pane: InterruptPane,
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

async function runEarlyInterrupt(
  pane: InterruptPane,
  draft: DraftSnapshotInputs,
  threadId: string,
  userItem: Item,
  interruptToken: number,
): Promise<void> {
  // Match the backend truncate: remove EVERY item on the active turn,
  // not just the user_text. Stranded thinking / api_retry / error rows
  // are the visible symptom of doing this piecewise. The rollback path
  // restores the full set via `upsertItems` when the backend refuses
  // the revert (predicate raced).
  const removedItems = pane.removeItemsFromTurn(userItem.turnIndex, threadId);
  const shouldRestoreDraft = Boolean(
    draft.applyOptimisticRestoredDraft || draft.clearOptimisticRestoredDraft,
  );
  const undo = getUndoableSend(threadId);
  const matchesSend = undo && parseUserMessageMeta(userItem.meta).sendId === undo.sendId;
  if (matchesSend) undo.undoRequested = true;
  const restoredDraft = shouldRestoreDraft
    ? (matchesSend ? undo.snapshot : restoredDraftSnapshotFromUserItem(userItem))
    : null;
  if (restoredDraft) {
    draft.applyOptimisticRestoredDraft?.(threadId, restoredDraft);
  }

  presentThreadInterruptAsRestored(threadId, interruptToken, userItem.turnIndex);
  const current = () => isThreadInterruptCurrent(threadId, interruptToken);
  const reconcile = conversationMutationReconciler(pane, threadId, userItem.id, matchesSend ? undo.sendId : '');
  const restore = () => {
    if (pane.threadId === threadId && removedItems.length > 0) pane.upsertItems(removedItems);
    if (restoredDraft) draft.clearOptimisticRestoredDraft?.(threadId, restoredDraft);
    return restoreThreadInterruptPresentation(threadId, interruptToken);
  };

  if (matchesSend) {
    const outcome = await undo.completion;
    if (!current()) return;
    if (outcome === 'cancelled' || outcome === 'failed') {
      try { await draft.settleOptimisticRestoredDraft?.(threadId); }
      catch (err) { reportNonBenignInterruptError(pane, err); }
      finally {
        retireUndoableSend(threadId, undo);
        finishThreadInterrupt(threadId, interruptToken);
      }
      return;
    }
  }

  let backgroundCount = 0;
  try {
    backgroundCount = Number(await CountRunningBackgroundTasks(threadId));
    if (!current()) return;
  } catch (err) {
    if (!current()) return;
    const refreshNeeded = restore();
    reportNonBenignInterruptError(pane, err);
    await runPlainInterrupt(pane, threadId, interruptToken);
    if (refreshNeeded && pane.threadId === threadId) await pane.refreshFromBackend();
    return;
  }
  if (backgroundCount > 0) {
    const refreshNeeded = restore();
    await runPlainInterrupt(pane, threadId, interruptToken);
    if (refreshNeeded && pane.threadId === threadId) await pane.refreshFromBackend();
    return;
  }

  let result: Awaited<ReturnType<typeof InterruptAndRevertIfClean>>;
  try {
    result = await InterruptAndRevertIfClean(threadId, {
      expectedSendId: matchesSend ? undo.sendId : '',
      draft: matchesSend ? {
        content: undo.snapshot.content,
        attachmentIds: undo.snapshot.attachments.map((a) => a.id),
        terminalChips: undo.snapshot.terminalChips,
        sourceProposedPlan: undo.snapshot.sourceProposedPlan,
      } : undefined,
    });
    if (!current()) return;
  } catch (err) {
    if (!current()) return;
    reportNonBenignInterruptError(pane, err);
    if (isTransportClassError(err)) {
      try {
        const state = await reconcile();
        if (matchesSend ? state.sendAccepted : state.userItemExists) {
          if (restoredDraft) draft.clearOptimisticRestoredDraft?.(threadId, restoredDraft);
        } else await draft.settleOptimisticRestoredDraft?.(threadId);
        finishThreadInterrupt(threadId, interruptToken);
      } catch (recoveryError) {
        reportNonBenignInterruptError(pane, recoveryError);
      }
    } else {
      const refreshNeeded = restore();
      finishThreadInterrupt(threadId, interruptToken);
      if (refreshNeeded && pane.threadId === threadId) await pane.refreshFromBackend();
    }
    return;
  }

  if (!result.reverted) {
    if (result.reason === 'sent message is no longer present' || result.reason === 'latest message changed') {
      try {
        const state = await reconcile();
        if (state.sendAccepted) restore();
        else await draft.settleOptimisticRestoredDraft?.(threadId);
      } catch (err) { reportNonBenignInterruptError(pane, err); }
      finishThreadInterrupt(threadId, interruptToken);
      return;
    }
    const refreshNeeded = restore();
    finishThreadInterrupt(threadId, interruptToken);
    if (refreshNeeded && pane.threadId === threadId) await pane.refreshFromBackend();
    return;
  }

  // The event bus coalesces frames independently from RPC responses, so the
  // response can arrive first. Apply its identical authoritative cut before
  // releasing Send; eventsMessageRevert deduplicates the later event by its
  // post-cut history stamp. A missing cut is a server contract breach. Keep
  // Send closed because restoring or re-enabling here could race a cut that
  // has committed but has not reached this client.
  if (!result.userItemId
    || (!matchesSend && result.userItemId !== userItem.id)
    || typeof result.turnIndex !== 'number'
    || (!matchesSend && result.turnIndex !== userItem.turnIndex)
    || typeof result.historyEpoch !== 'number'
    || typeof result.historyRev !== 'number'
    || !Number.isFinite(result.historyEpoch)
    || !Number.isFinite(result.historyRev)
    || (result.historyEpoch <= 0 && result.historyRev <= 0)) {
    reportNonBenignInterruptError(
      pane,
      new Error('interrupt-and-revert completed without its authoritative cut fields'),
    );
    try {
      const state = await reconcile();
      if (matchesSend ? state.sendAccepted : state.userItemExists) restore();
      else await draft.settleOptimisticRestoredDraft?.(threadId);
      finishThreadInterrupt(threadId, interruptToken);
    } catch (err) { reportNonBenignInterruptError(pane, err); }
    return;
  }
  applyUserMessageReverted({
    threadId,
    itemEventSequence: result.itemEventSequence,
    turnStartedSequence: result.turnStartedSequence,
    turnCompletedSequence: result.turnCompletedSequence,
    userItemId: result.userItemId,
    turnIndex: result.turnIndex,
    keptAnchorTurnItemIds: result.keptAnchorTurnItemIds,
    historyEpoch: result.historyEpoch,
    historyRev: result.historyRev,
  });
  // Caller-owned release, after its exact RPC cut has been applied. A revert
  // event from another client on the same thread must not release this
  // operation while its own RPC is still queued behind that client.
  try {
    await draft.settleOptimisticRestoredDraft?.(threadId);
  } catch (err) {
    reportNonBenignInterruptError(pane, err);
  } finally {
    if (matchesSend) retireUndoableSend(threadId, undo);
    finishThreadInterrupt(threadId, interruptToken);
  }
}
