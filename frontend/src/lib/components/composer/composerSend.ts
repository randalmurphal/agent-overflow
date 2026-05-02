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
  SteerMessageWithOptions,
} from '../../stores/bindings';
import type { Attachment } from '../../types/attachment';
import type { TerminalChip } from '../../types/draft';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import {
  findDraftProjectId,
  clearProjectDraft,
} from '../../stores/draftThreads.svelte';
import {
  getThreadById,
  prependThread,
  replaceThread,
} from '../../stores/threads.svelte';
import {
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
import { buildSendOptions } from '../../utils/sendOptions';

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
    // Single source of truth for the wire payload — the queue's drain
    // path runs through `buildSendOptions` too, so the precedence rule
    // (revision wins over source-plan) and runtime-mode handling stay
    // aligned regardless of how the message reaches the backend.
    const sendOptions = buildSendOptions({
      attachmentIds: opts.attachmentIds,
      runtimeMode,
      sourceProposedPlan: opts.sourceProposedPlan,
      revisionSourceProposedPlan: opts.revisionSourceProposedPlan,
      revisionSourceCommentIds: opts.revisionSourceCommentIds,
    });
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

export interface SteerOptions {
  threadId: string;
  message: string;
  attachmentIds: string[];
  runtimeMode?: string;
  sourceProposedPlan?: SourceProposedPlan;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: string[];
  /**
   * Fallback when the backend reports the active turn ended before the
   * steer arrived (race window between the frontend reading
   * `getActiveTurn` and this RPC landing). The composer wires this to
   * an `enqueueQueuedMessage` call so the user's message survives the
   * race — the queued item drains on the next `provider:turn_completed`
   * just like a Claude mid-turn enqueue.
   */
  enqueueOnRace: () => void;
  reportError: (message: string) => void;
  /**
   * Restore the composer to the snapshot captured before clearing so a
   * non-race steer failure leaves the user's typed content visible
   * rather than silently dropped. The composer's idle-send path uses
   * the same pattern.
   */
  restoreDraft: () => Promise<void>;
}

/**
 * Heuristic match against the backend's "no active turn" error wires.
 * The Go side returns `codex.ErrNoActiveTurn` (`"codex: no active turn
 * to steer"`) when the local session has no in-flight turn id, and
 * Codex's app-server returns `NoActiveTurn` / `ExpectedTurnMismatch`
 * inside a `codex: turn/steer: ...` wrapper when the server has
 * already moved past the turn the frontend was tracking. Both shapes
 * mean "fall back to the queue" — losing the message because of a
 * timing race would be the worst possible UX.
 */
function isSteerRaceError(err: unknown): boolean {
  const message = errString(err).toLowerCase();
  if (!message) return false;
  return (
    message.includes('no active turn')
    || message.includes('noactiveturn')
    || message.includes('expectedturnmismatch')
    || message.includes('expected turn mismatch')
  );
}

/**
 * Drive a Codex `turn/steer` mid-turn injection. Mirrors the validation
 * shape of `dispatchSend` (worktree-prep / draft-thread promotion are
 * unnecessary mid-turn — the thread is fully established by definition)
 * and routes the wire payload through the shared `buildSendOptions`
 * helper so the precedence rule (revision wins over source-plan) and
 * runtime-mode handling stay aligned with the idle-send path.
 *
 * On a "no active turn" race, falls back to the per-thread queue so
 * the user's message lands on the next round.
 */
export async function dispatchSteer(opts: SteerOptions): Promise<void> {
  const sendOptions = buildSendOptions({
    attachmentIds: opts.attachmentIds,
    runtimeMode: opts.runtimeMode,
    sourceProposedPlan: opts.sourceProposedPlan,
    revisionSourceProposedPlan: opts.revisionSourceProposedPlan,
    revisionSourceCommentIds: opts.revisionSourceCommentIds,
  });
  try {
    await SteerMessageWithOptions(opts.threadId, opts.message, sendOptions);
  } catch (err) {
    if (isSteerRaceError(err)) {
      console.warn('Steer raced with turn end; falling back to queue:', err);
      opts.enqueueOnRace();
      return;
    }
    console.error('Failed to steer message:', err);
    await opts.restoreDraft();
    opts.reportError(`Failed to send mid-turn message: ${errString(err)}`);
  }
}
