// Composer send / interrupt flow.
//
// Extracted from Composer.svelte to shrink the shell below the 300-line
// guideline. Owns nothing reactive — each call captures the current
// thread id / draft snapshot and delegates back to the pane + draft
// store for state changes. Keeping this pure functional makes the send
// path easy to trace from the click handler all the way to SendMessage.

import { SendMessageWithOptions } from '../../stores/bindings';
import type { Attachment } from '../../types/attachment';
import type { TerminalChip } from '../../types/draft';
import { addToast } from '../../stores/toast.svelte';
import { isUndeliveredSendError } from '../../stores/transportStatus.svelte';
import { confirmUnsentMessageRestore } from '../../stores/unsentMessageConfirmation.svelte';
import { userFacingError } from '../../utils/userFacingError';
import { syncThread } from '../../stores/panes.svelte';
import {
  projectSendResolved,
  projectSendStarted,
} from '../../stores/threadStatuses.svelte';
import type { SourceDiffReview, SourceProposedPlan, Thread } from '../../types/models';
import { buildSendOptions } from '../../utils/sendOptions';
import { getThreadById } from '../../stores/threads.svelte';
import { autoPinNewThread, shouldAutoPinFirstSend } from '../../stores/threadAutoPin';

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
  revisionSourceDiffReview?: SourceDiffReview;
  revisionSourceDiffCommentIds?: string[];
  /** Draft snapshot used to restore the composer on send failure. */
  snapshot: {
    content: string;
    attachments: Attachment[];
    terminalChips: TerminalChip[];
    sourceProposedPlan?: SourceProposedPlan | null;
  };
  restoreDraft: (threadId: string, snapshot: SendOptions['snapshot']) => Promise<void>;
  draftThreadId: () => string | null;
  reportError: (message: string) => void;
}

/**
 * Drive a send: dispatch SendMessage and restore the draft on failure.
 * Callers are expected to set the pane's local `sending` flag around this
 * call so the UI reflects in-flight state — this function intentionally
 * knows nothing about it.
 *
 * The staged branch/worktree intent is NOT applied here. The host first
 * materializes the draft, then binds its workspace through the thread-scoped
 * operation, and only then calls this function.
 */
export async function dispatchSend(opts: SendOptions): Promise<boolean> {
  let sendStarted = false;
  try {
    const autoPinAfterSend = shouldAutoPinFirstSend(getThreadById(opts.threadId));
    // Optimistically flip the sidebar pill to Working the moment the
    // user clicks Send. Provider sessions for brand-new threads take
    // seconds to cold-start before the backend emits `turn_started`;
    // without this, the row would sit idle for that whole window while
    // the agent is clearly "working" from the user's POV. Cleared by
    // applyTurnStarted (takeover on real backend signal) or by the
    // catch branch below if SendMessage itself rejects.
    projectSendStarted(opts.threadId);
    sendStarted = true;

    // Single source of truth for the wire payload — the queue's drain
    // path runs through `buildSendOptions` too, so the precedence rule
    // (revision wins over source-plan) stays aligned regardless of how
    // the message reaches the backend.
    const sendOptions = buildSendOptions({
      attachmentIds: opts.attachmentIds,
      sourceProposedPlan: opts.sourceProposedPlan,
      revisionSourceProposedPlan: opts.revisionSourceProposedPlan,
      revisionSourceCommentIds: opts.revisionSourceCommentIds,
      revisionSourceDiffReview: opts.revisionSourceDiffReview,
      revisionSourceDiffCommentIds: opts.revisionSourceDiffCommentIds,
    });
    let updated = (await SendMessageWithOptions(opts.threadId, opts.message, sendOptions)) as Thread;
    if (autoPinAfterSend) updated = await autoPinNewThread(updated);
    syncThread(updated);
    return true;
  } catch (err) {
    console.error('Failed to send message:', err);
    // Flip to error so the sidebar pill reads "Failed" — the user
    // should see the failure without having to open the thread.
    if (sendStarted) {
      projectSendResolved(opts.threadId, { error: true });
    }
    // A send whose socket broke is UNDECIDABLE here, and the transport has
    // already spent its one retry by the time this runs
    // (RETRY_ON_TRANSIENT_CLOSE). The message may be with the agent. Putting
    // it back silently is a guess whose wrong answer sends it twice, so ask —
    // and only for this class: a backend that answered with an error, or a
    // terminal disconnect, is a definite "nothing happened" and restores
    // exactly as it always did.
    //
    // "Leave it" discards the snapshot AND reports nothing further: the
    // person was shown the ambiguity and decided it, and a banner restating
    // the failure they just adjudicated would contradict their answer.
    if (isUndeliveredSendError(err) && !(await confirmUnsentMessageRestore())) {
      return false;
    }

    // restoreDraft always persists to the captured thread; it only touches
    // the local draft store if it's still on that thread. If the user has
    // moved on, surface a toast so the failed send is visible rather than
    // silent.
    //
    // Its own failure is a SECOND failure, not a replacement for the first:
    // it must neither swallow the send error's reporting below nor let the
    // "draft preserved" wording claim something that did not happen.
    let draftPreserved = true;
    try {
      await opts.restoreDraft(opts.threadId, opts.snapshot);
    } catch (restoreErr) {
      draftPreserved = false;
      console.error('Failed to restore the draft after a failed send:', restoreErr);
    }
    const readableError = userFacingError(err);
    if (opts.draftThreadId() !== opts.threadId) {
      addToast(
        'error',
        draftPreserved
          ? `Message to the previous thread failed to send; draft preserved: ${readableError}`
          : `Message to the previous thread failed to send, and its draft could not be saved: ${readableError}`,
      );
    } else {
      opts.reportError(
        draftPreserved
          ? `Failed to send message: ${readableError}`
          : `Failed to send message, and the draft could not be restored: ${readableError}`,
      );
    }
    return false;
  }
}
