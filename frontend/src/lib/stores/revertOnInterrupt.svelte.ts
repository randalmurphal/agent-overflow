import { conversationMutationReconciler } from './reconcileConversationMutation';
import { isTransportClassError } from './transportStatus.svelte';
import { getUndoableSend, retireUndoableSend } from './composerSendUndo';
import { parseUserMessageMeta } from '../utils/userMessageMeta';
// Early Stop restores presentation synchronously. The original send, background
// checks and provider rollback finish behind a thread-scoped Send gate. A refusal
// restores the transcript; a committed cut fences delayed item and turn events.

import type { Attachment } from '../types/attachment';
import type { TerminalChip } from '../types/draft';
import type { Item, ItemKind } from '../types/models';
import type { ComposerDraftSnapshot } from './composerDraftSnapshots';
import type { ErrorSurface, PaneSession, ThreadPaneIngest } from './threadPaneRoles';
import { isReaderAuthoredUserText } from '../utils/userMessageMeta';
import { restoredDraftSnapshotFromUserItem } from '../utils/userMessageDraftSnapshot';
import {
  getActiveTurn,
  isThreadWorking,
  projectTurnStopRequested,
  restoreRefusedTurnStop,
  settleTurnStopRequest,
  type ActiveTurn,
} from './threadStatuses.svelte';
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
import { refusedBackgroundAgents, type BackgroundKillAgent } from '../transport/backgroundKillRefusal';
import { confirmBackgroundKill } from './backgroundKillConfirmation.svelte';
import { hasLiveSubagentRunStates } from './subagentRunState.svelte';

/**
 * The only rows that may share a turn with the message an early Stop
 * un-sends: the model's unfinished reasoning and request-level retries or
 * errors. Everything else, including kinds added later, is content the
 * provider conversation holds. Mirrors the backend's
 * unsendTurnCompanionKinds.
 */
const UNSEND_TURN_COMPANION_KINDS: ReadonlySet<string> = new Set<string>([
  'thinking',
  'api_retry',
  'api_error',
  'error',
] satisfies ItemKind[]);

/**
 * Result of the frontend revert-eligibility predicate. Discriminated
 * on `canRevert` so callers don't have to defensively re-check
 * `userItem`: when the predicate says yes, the user row is guaranteed.
 *
 * `canRevert` is true only when ALL of the following hold:
 *   - The thread has an active turn.
 *   - The composer is empty (user hasn't started typing again).
 *   - The send queue is empty (no queued follow-up to drain).
 *   - The turn has never settled. A later round on a settled turn (the
 *     CLI answering a background task notification) does not make its
 *     message undoable again.
 *   - The turn holds exactly one reader-authored user_text and otherwise
 *     only UNSEND_TURN_COMPANION_KINDS rows.
 *
 * Mirrors the backend's evaluateInterruptRevertPredicate, which stays
 * authoritative; this copy only decides the optimistic presentation.
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
type InterruptPane = ThreadPaneIngest
  & Pick<ErrorSurface, 'setGeneralError'>
  & Pick<PaneSession, 'setSendInFlight'>;

/**
 * Whether a plain interrupt owns the pane's working presentation. 'clear'
 * flips the spinner, Stop button and mid-turn input gate to idle in the
 * same tick as the press (Claude Code's `resetLoadingState`, the Codex
 * TUI's spinner clear on `TurnAborted`); the real `provider:turn_completed`
 * re-runs the same path. 'keep' is the early un-send's fallback, whose
 * restored presentation already shows the turn running until it ends.
 */
type InterruptPresentation = 'clear' | 'keep';

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

  const turnIndex = active?.turnIndex ?? pane.items.find((item) =>
    parseUserMessageMeta(item.meta).sendId === pending?.sendId)?.turnIndex;
  if (turnIndex === undefined) return { canRevert: false, reason: 'no pending user message' };
  const settled = pane.latestSettledTurn;
  if (settled && settled.turnIndex >= turnIndex) {
    return { canRevert: false, reason: 'turn already settled' };
  }
  let userItem: Item | undefined;
  let userCount = 0;
  for (const item of pane.items) {
    if (item.turnIndex !== turnIndex) continue;
    if (isReaderAuthoredUserText(item) && item.role === 'user') {
      userItem = item;
      userCount++;
      continue;
    }
    if (!UNSEND_TURN_COMPANION_KINDS.has(item.kind)) {
      return { canRevert: false, reason: `turn holds ${item.kind}` };
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
    void runPlainInterrupt(pane, threadId, interruptToken, 'clear');
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

/**
 * A person's Stop that keeps the sent message: the mid-turn cancel paths
 * (an approval or user-input prompt declined by Esc) and any Stop the
 * un-send predicate turns down. Owns the optimistic clear and the
 * background-agent confirmation like runInterruptOrRevert's plain arm.
 */
export function runInterrupt(pane: InterruptPane): void {
  const threadId = pane.threadId;
  if (!threadId) return;
  const interruptToken = beginThreadInterrupt(threadId);
  if (interruptToken === null) return;
  void runPlainInterrupt(pane, threadId, interruptToken, 'clear');
}

/**
 * The Stop's optimistic clear. Returns the handle the answer to its
 * interrupt RPC takes: a refusal puts the turn back (restoreRefusedTurnStop),
 * anything else settles it.
 */
function clearWorkingPresentation(pane: InterruptPane, threadId: string): ActiveTurn | null {
  const stopped = projectTurnStopRequested(threadId);
  if (pane.threadId === threadId) pane.setSendInFlight(false);
  return stopped;
}

/** InterruptTurn for a Stop whose optimistic clear is `stopped`. */
async function interruptTurnForStop(
  pane: InterruptPane,
  threadId: string,
  confirmed: boolean,
  stopped: ActiveTurn | null,
): Promise<BackgroundKillAgent[] | null> {
  try {
    await InterruptTurn(threadId, confirmed);
  } catch (err) {
    const agents = refusedBackgroundAgents(err);
    if (agents !== null) return agents;
    reportNonBenignInterruptError(pane, err);
  }
  settleTurnStopRequest(threadId, stopped);
  return null;
}

/**
 * A plain interrupt. A Claude interrupt kills every live async agent the
 * session holds, so the backend refuses with `background_agents_running`
 * and the agents until the caller confirms
 * (transport/backgroundKillRefusal.ts); the person answers through the
 * app-root dialog, and "keep them" leaves the turn running. The working
 * presentation is cleared in the press's tick when no listed agent is
 * live, else after the confirmation. The backend, not the registry, is the
 * authority: a refusal on a stale registry (an agent launched since the
 * tray's last read) puts the cleared turn back before asking.
 */
async function runPlainInterrupt(
  pane: InterruptPane,
  threadId: string,
  interruptToken: number,
  presentation: InterruptPresentation,
): Promise<void> {
  const stopped = presentation === 'clear' && !hasLiveSubagentRunStates(threadId)
    ? clearWorkingPresentation(pane, threadId)
    : null;
  let agents: BackgroundKillAgent[] | null = null;
  try {
    agents = await interruptTurnForStop(pane, threadId, false, stopped);
    if (agents !== null) restoreRefusedTurnStop(threadId, stopped);
  } finally {
    finishThreadInterrupt(threadId, interruptToken);
  }
  if (agents !== null) await interruptAfterConfirmation(pane, threadId, agents);
}

/**
 * The refused Stop's second half: ask, and on "stop everything" run a Stop
 * of its own with the confirmation set. The refused Stop's transaction is
 * already closed: the question is the person's, not an interrupt in
 * flight, so the composer keeps showing the running turn and its Stop
 * behind the dialog. Nothing is asked when the turn ended before the
 * refusal landed: there is nothing left to stop.
 */
async function interruptAfterConfirmation(
  pane: InterruptPane,
  threadId: string,
  agents: readonly BackgroundKillAgent[],
): Promise<void> {
  if (!isThreadWorking(threadId)) return;
  const stop = await confirmBackgroundKill(threadId, agents);
  if (!stop) return;
  const interruptToken = beginThreadInterrupt(threadId);
  if (interruptToken === null) return;
  try {
    const stopped = clearWorkingPresentation(pane, threadId);
    const refusedAgain = await interruptTurnForStop(pane, threadId, true, stopped);
    if (refusedAgain !== null) {
      // A confirmed Stop is never refused; a backend that does is reporting
      // a contract breach, and the turn it left running stays on screen.
      restoreRefusedTurnStop(threadId, stopped);
      reportNonBenignInterruptError(pane, new Error('confirmed Stop was refused for background agents'));
    }
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
    await runPlainInterrupt(pane, threadId, interruptToken, 'keep');
    if (refreshNeeded && pane.threadId === threadId) await pane.refreshFromBackend();
    return;
  }
  if (backgroundCount > 0) {
    // The un-send is not offered while background work runs (a Claude
    // interrupt kills the agents; a plain Stop keeps the message). The
    // plain interrupt asks about the agents itself.
    const refreshNeeded = restore();
    await runPlainInterrupt(pane, threadId, interruptToken, 'keep');
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
    }, false);
    if (!current()) return;
  } catch (err) {
    if (!current()) return;
    const agents = refusedBackgroundAgents(err);
    if (agents !== null) {
      // An agent launched after the count above: the un-send is off the
      // table (the message stays), and the Stop asks like a plain one.
      const refreshNeeded = restore();
      finishThreadInterrupt(threadId, interruptToken);
      if (refreshNeeded && pane.threadId === threadId) await pane.refreshFromBackend();
      await interruptAfterConfirmation(pane, threadId, agents);
      return;
    }
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
