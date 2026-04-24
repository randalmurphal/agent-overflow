// Composer send / interrupt flow.
//
// Extracted from Composer.svelte to shrink the shell below the 300-line
// guideline. Owns nothing reactive — each call captures the current
// thread id / draft snapshot and delegates back to the pane + draft
// store for state changes. Keeping this pure functional makes the send
// path easy to trace from the click handler all the way to SendMessage.

import { InterruptTurn, SendMessage } from '../../stores/bindings';
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
} from '../../stores/threads.svelte';
import {
  projectSendResolved,
  projectSendStarted,
} from '../../stores/threadStatuses.svelte';
import type { Thread } from '../../types/models';

export interface SendOptions {
  threadId: string;
  message: string;
  attachmentIds: string[];
  /** Draft snapshot used to restore the composer on send failure. */
  snapshot: {
    content: string;
    attachments: Attachment[];
    terminalChips: TerminalChip[];
  };
  /** Currently-focused Thread object — needed to promote a draft thread. */
  currentThread: Thread | null;
  restoreDraft: (threadId: string, snapshot: SendOptions['snapshot']) => Promise<void>;
  draftThreadId: () => string | null;
  reportError: (message: string) => void;
}

/**
 * Drive a send: promote a draft-thread if needed, dispatch SendMessage,
 * and restore the draft on failure. Callers are expected to set the
 * pane's local `sending` flag around this call so the UI reflects in-
 * flight state — this function intentionally knows nothing about it.
 */
export async function dispatchSend(opts: SendOptions): Promise<void> {
  // Promote a draft thread to the sidebar the moment the user hits send —
  // not after the backend confirms delivery. A failed send leaves the
  // thread visible. Clear the draft-thread pointer so a follow-up "New
  // Thread" for the same project spins up a fresh draft.
  const draftProjectId = findDraftProjectId(opts.threadId);
  if (draftProjectId) {
    if (opts.currentThread && !getThreadById(opts.threadId)) {
      prependThread(opts.currentThread);
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

  try {
    await SendMessage(opts.threadId, opts.message, opts.attachmentIds);
  } catch (err) {
    console.error('Failed to send message:', err);
    // Flip to error so the sidebar pill reads "Error" — the user
    // should see the failure without having to open the thread.
    projectSendResolved(opts.threadId, { error: true });
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
