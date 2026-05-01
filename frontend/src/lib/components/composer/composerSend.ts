// Composer send / interrupt flow.
//
// Extracted from Composer.svelte to shrink the shell below the 300-line
// guideline. Owns nothing reactive — each call captures the current
// thread id / draft snapshot and delegates back to the pane + draft
// store for state changes. Keeping this pure functional makes the send
// path easy to trace from the click handler all the way to SendMessage.

import {
  InterruptTurn,
  PrepareThreadWorktree,
  SendMessageWithOptions,
} from '../../stores/bindings';
import type { Attachment } from '../../types/attachment';
import type { TerminalChip } from '../../types/draft';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import {
  findDraftProjectId,
  clearProjectDraft,
} from '../../stores/draftThreads.svelte';
import { getAllPanes } from '../../stores/panes.svelte';
import {
  enqueueAtFront,
  popFront,
  type QueueItem,
} from '../../stores/sendQueue.svelte';
import {
  getThreadById,
  prependThread,
  replaceThread,
} from '../../stores/threads.svelte';
import {
  clearPendingSend,
  getActiveTurn,
  projectSendResolved,
  projectSendStarted,
} from '../../stores/threadStatuses.svelte';
import type { SourceProposedPlan, Thread } from '../../types/models';
import {
  clearRuntimeModeDraft,
  hasRuntimeModeDraft,
  runtimeModeForThread,
} from '../../stores/runtimeModeDraft.svelte';
import {
  clearWorktreeIntent,
  worktreeIntentForThread,
} from '../../stores/worktreeIntent.svelte';

export interface SendOptions {
  threadId: string;
  message: string;
  attachmentIds: string[];
  /**
   * "This turn implements the named plan." Used by the
   * implement-in-new-thread flow: the draft carries the source-plan
   * reference, the send forwards it, the backend marks the original plan
   * Accepted. Distinct from revisionSourceProposedPlan, which means
   * "this turn is a revision based on the plan + comments."
   *
   * Precedence rule: if BOTH revisionSourceProposedPlan and
   * sourceProposedPlan are passed, the revision takes precedence and the
   * source-plan ref is dropped — a turn cannot simultaneously revise and
   * implement the same plan. Owned here (not at the call site) so every
   * caller of dispatchSend gets the same resolution.
   */
  sourceProposedPlan?: SourceProposedPlan;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: string[];
  /** Draft snapshot used to restore the composer on send failure. */
  snapshot: {
    content: string;
    attachments: Attachment[];
    terminalChips: TerminalChip[];
    sourceProposedPlan?: SourceProposedPlan | null;
  };
  /** Currently-focused Thread object — needed to promote a draft thread. */
  currentThread: Thread | null;
  replaceCurrentThread: (thread: Thread) => void;
  restoreDraft: (threadId: string, snapshot: SendOptions['snapshot']) => Promise<void>;
  draftThreadId: () => string | null;
  reportError: (message: string) => void;
  onWorktreePrepareStarted?: () => void;
  onWorktreePrepareFinished?: () => void;
}

/**
 * Drive a send: promote a draft-thread if needed, dispatch SendMessage,
 * and restore the draft on failure. Callers are expected to set the
 * pane's local `sending` flag around this call so the UI reflects in-
 * flight state — this function intentionally knows nothing about it.
 */
export async function dispatchSend(opts: SendOptions): Promise<void> {
  let sendStarted = false;
  try {
    let threadForSend = opts.currentThread;
    const worktreeIntent = worktreeIntentForThread(threadForSend);
    if (threadForSend && worktreeIntent.mode === 'new-worktree') {
      opts.onWorktreePrepareStarted?.();
      let updated: Thread;
      try {
        updated = (await PrepareThreadWorktree(
          opts.threadId,
          worktreeIntent.baseBranch,
          worktreeIntent.branchName,
        )) as Thread;
      } finally {
        opts.onWorktreePrepareFinished?.();
      }
      threadForSend = updated;
      opts.replaceCurrentThread(updated);
      replaceThread(updated);
      clearWorktreeIntent(opts.threadId);
    }

    // Promote a draft thread to the sidebar the moment the user hits send —
    // not after the backend confirms delivery. A failed send leaves the
    // thread visible. Clear the draft-thread pointer so a follow-up "New
    // Thread" for the same project spins up a fresh draft.
    const draftProjectId = findDraftProjectId(opts.threadId);
    if (draftProjectId) {
      if (threadForSend && !getThreadById(opts.threadId)) {
        prependThread(threadForSend);
      }
      clearProjectDraft(draftProjectId);
    }

    // Optimistically flip the sidebar pill to Working the moment the
    // user clicks Send. Provider sessions for brand-new threads take
    // seconds to cold-start before the backend emits `turn_started`;
    // without this, the row would sit idle for that whole window while
    // the agent is clearly "working" from the user's POV. Cleared by
    // applyTurnStarted (takeover on real backend signal) or by the
    // catch branch below if SendMessage itself rejects.
    projectSendStarted(opts.threadId);
    sendStarted = true;

    const runtimeMode = threadForSend?.id === opts.threadId && hasRuntimeModeDraft(threadForSend)
      ? runtimeModeForThread(threadForSend)
      : undefined;
    const sendOptions: {
      attachmentIds: string[];
      runtimeMode?: string;
      sourceProposedPlan?: SourceProposedPlan;
      revisionSourceProposedPlan?: SourceProposedPlan;
      revisionSourceCommentIds?: string[];
    } = {
      attachmentIds: opts.attachmentIds,
    };
    if (runtimeMode) sendOptions.runtimeMode = runtimeMode;
    // Apply the precedence rule documented on SendOptions: revision wins
    // over source-plan when both arrive, so a turn never simultaneously
    // revises and implements the same plan.
    if (opts.revisionSourceProposedPlan) {
      sendOptions.revisionSourceProposedPlan = opts.revisionSourceProposedPlan;
    } else if (opts.sourceProposedPlan) {
      sendOptions.sourceProposedPlan = opts.sourceProposedPlan;
    }
    if (opts.revisionSourceCommentIds && opts.revisionSourceCommentIds.length > 0) {
      sendOptions.revisionSourceCommentIds = opts.revisionSourceCommentIds;
    }
    const updated = (await SendMessageWithOptions(opts.threadId, opts.message, sendOptions)) as Thread;
    opts.replaceCurrentThread(updated);
    replaceThread(updated);
    clearRuntimeModeDraft(opts.threadId);
  } catch (err) {
    console.error('Failed to send message:', err);
    // Flip to error so the sidebar pill reads "Failed" — the user
    // should see the failure without having to open the thread.
    if (sendStarted) {
      projectSendResolved(opts.threadId, { error: true });
    }
    // restoreDraft always persists to the captured thread; it only touches
    // the local draft store if it's still on that thread. If the user has
    // moved on, surface a toast so the failed send is visible rather than
    // silent.
    await opts.restoreDraft(opts.threadId, opts.snapshot);
    if (opts.draftThreadId() !== opts.threadId) {
      addToast(
        'error',
        `Message to the previous thread failed to send; draft preserved (${errString(err)}).`,
      );
    } else {
      opts.reportError(`Failed to send message: ${errString(err)}`);
    }
  }
}

/**
 * Fire InterruptTurn for the current thread. Errors are surfaced via the
 * supplied reporter so the composer can paint them in its error row.
 */
export async function dispatchInterrupt(
  threadId: string,
  reportError: (message: string) => void,
): Promise<void> {
  try {
    await InterruptTurn(threadId);
  } catch (err) {
    console.error('Failed to interrupt turn:', err);
    reportError(`Failed to interrupt: ${errString(err)}`);
  }
}

interface DrainSendOptions {
  attachmentIds: string[];
  sourceProposedPlan?: SourceProposedPlan;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: string[];
}

/**
 * Translate a queued item into the SendMessageWithOptions payload. Same
 * precedence rule as `dispatchSend`: a revision wins over a plain
 * source-plan ref, so a turn never simultaneously revises and
 * implements the same plan.
 */
function toSendOptions(item: QueueItem): DrainSendOptions {
  const options: DrainSendOptions = {
    attachmentIds: item.attachments.map((attachment) => attachment.id),
  };
  if (item.revisionSourceProposedPlan) {
    options.revisionSourceProposedPlan = item.revisionSourceProposedPlan;
  } else if (item.sourceProposedPlan) {
    options.sourceProposedPlan = item.sourceProposedPlan;
  }
  if (item.revisionSourceCommentIds && item.revisionSourceCommentIds.length > 0) {
    options.revisionSourceCommentIds = [...item.revisionSourceCommentIds];
  }
  return options;
}

function reportThreadGeneralError(threadId: string, message: string): void {
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== threadId) continue;
    pane.setGeneralError(message);
  }
}

/**
 * Pop the head-of-line queued message and dispatch it via
 * SendMessageWithOptions. Triggered after every `provider:turn_completed`
 * regardless of cause (success, error, abort) — that's the uniform rule
 * both reference UIs follow. Skips worktree-prep and draft-thread
 * promotion because drain only fires post-first-send when the thread is
 * fully established (there's no provider session before the first send,
 * so `isTurnActive` can never be true on a draft thread, so the
 * composer's enqueue branch never runs and this drain helper never sees
 * a queued item without a real backend session to send to).
 *
 * On failure, restores the item at the front so the user's queued work
 * isn't lost, clears the pendingSend flag (which `projectTurnStarted`
 * would otherwise have cleared on success), and surfaces the error
 * through any matching pane's general-error banner.
 *
 * Defensive bail when `getActiveTurn` is non-null: the caller is
 * `applyTurnCompleted`, which has already cleared the registry, so this
 * path is normally a no-op. It only matters if a fresh `turn_started`
 * for a follow-up round arrives between `projectTurnCompleted` and
 * this listener firing.
 */
export async function tryDrainNextQueued(threadId: string): Promise<void> {
  if (!threadId) return;
  if (getActiveTurn(threadId) !== null) return;
  const next = popFront(threadId);
  if (!next) return;
  projectSendStarted(threadId);
  try {
    await SendMessageWithOptions(threadId, next.message, toSendOptions(next));
    // Success: `projectTurnStarted` clears the pendingSend flag when
    // the backend confirms the new round arms. Nothing else to do here.
  } catch (err) {
    console.error('Failed to drain queued message:', err);
    enqueueAtFront(threadId, next);
    clearPendingSend(threadId);
    reportThreadGeneralError(threadId, `Failed to send queued message: ${errString(err)}`);
  }
}
