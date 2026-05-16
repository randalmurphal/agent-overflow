// Composer send / interrupt flow.
//
// Extracted from Composer.svelte to shrink the shell below the 300-line
// guideline. Owns nothing reactive — each call captures the current
// thread id / draft snapshot and delegates back to the pane + draft
// store for state changes. Keeping this pure functional makes the send
// path easy to trace from the click handler all the way to SendMessage.

import {
  AttachThreadWorktree,
  GitCreateBranchFrom,
  PrepareThreadWorktree,
  SendMessageWithOptions,
} from '../../stores/bindings';
import type { Attachment } from '../../types/attachment';
import type { TerminalChip } from '../../types/draft';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import {
  findDraftEntry,
  clearProjectDraft,
} from '../../stores/draftThreads.svelte';
import {
  getThreadById,
  prependThread,
} from '../../stores/threads.svelte';
import { syncThread } from '../../stores/panes.svelte';
import {
  projectSendResolved,
  projectSendStarted,
} from '../../stores/threadStatuses.svelte';
import type { SourceDiffReview, SourceProposedPlan, Thread } from '../../types/models';
import {
  clearRuntimeModeDraft,
  hasRuntimeModeDraft,
  runtimeModeForThread,
} from '../../stores/runtimeModeDraft.svelte';
import {
  clearWorktreeIntent,
  resolveBaseForWire,
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
  revisionSourceDiffReview?: SourceDiffReview;
  revisionSourceDiffCommentIds?: string[];
  /** Draft snapshot used to restore the composer on send failure. */
  snapshot: {
    content: string;
    attachments: Attachment[];
    terminalChips: TerminalChip[];
    sourceProposedPlan?: SourceProposedPlan | null;
  };
  /** Currently-focused Thread object — needed to promote a draft thread. */
  currentThread: Thread | null;
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
    if (threadForSend) {
      const needsWorkspaceWork =
        worktreeIntent.mode === 'new-worktree' || worktreeIntent.creatingBranch;
      if (needsWorkspaceWork) {
        opts.onWorktreePrepareStarted?.();
        let updated: Thread | null = null;
        try {
          if (worktreeIntent.mode === 'new-worktree' && worktreeIntent.creatingBranch) {
            // New worktree off a brand-new branch (today's existing
            // path). LOCAL sentinel resolves via resolveBaseForWire to
            // (currentBranch, carry=true) so the backend stash-carry
            // path engages.
            const wire = resolveBaseForWire(
              worktreeIntent.newBranchBase,
              threadForSend.branch ?? '',
            );
            updated = (await PrepareThreadWorktree(
              opts.threadId,
              wire.baseBranch,
              worktreeIntent.newBranchName,
              wire.carryLocalChanges,
            )) as Thread;
          } else if (
            worktreeIntent.mode === 'new-worktree' &&
            !worktreeIntent.creatingBranch
          ) {
            // New worktree pointing at an existing branch. The
            // BranchPicker's dedup gate already caught
            // already-checked-out branches; if the attach still fails,
            // git's invariant kicks in and the error surfaces below.
            const branch = worktreeIntent.attachBranch || (threadForSend.branch ?? '');
            updated = (await AttachThreadWorktree(opts.threadId, branch)) as Thread;
          } else if (
            worktreeIntent.mode === 'local' &&
            worktreeIntent.creatingBranch
          ) {
            // Stay in the current workspace, create a new branch off
            // the picked base, and check it out.
            const wire = resolveBaseForWire(
              worktreeIntent.newBranchBase,
              threadForSend.branch ?? '',
            );
            updated = (await GitCreateBranchFrom(
              opts.threadId,
              worktreeIntent.newBranchName,
              wire.baseBranch,
              wire.carryLocalChanges,
            )) as Thread;
          }
        } finally {
          opts.onWorktreePrepareFinished?.();
        }
        if (updated) {
          threadForSend = updated;
          syncThread(updated);
        }
        clearWorktreeIntent(opts.threadId);
      }
    }

    // Promote a draft thread to the sidebar the moment the user hits send —
    // not after the backend confirms delivery. A failed send leaves the
    // thread visible. Clear the draft-thread pointer so a follow-up "+
    // New" for the same (project, mode) spins up a fresh draft.
    const draftEntry = findDraftEntry(opts.threadId);
    if (draftEntry) {
      if (threadForSend && !getThreadById(opts.threadId)) {
        prependThread(threadForSend);
      }
      clearProjectDraft(draftEntry.projectId, draftEntry.mode);
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
      revisionSourceDiffReview: opts.revisionSourceDiffReview,
      revisionSourceDiffCommentIds: opts.revisionSourceDiffCommentIds,
    });
    const updated = (await SendMessageWithOptions(opts.threadId, opts.message, sendOptions)) as Thread;
    syncThread(updated);
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

