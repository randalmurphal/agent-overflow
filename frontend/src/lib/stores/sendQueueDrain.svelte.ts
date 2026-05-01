// Send-queue drain helper.
//
// Lives in `stores/` so `events.ts` (also in stores/) doesn't have to
// import from `components/composer/` — events.ts only depends on
// stores by convention. Pops the head-of-line queued message and
// dispatches it via `SendMessageWithOptions`. Triggered after every
// `provider:turn_completed` regardless of cause (success, error,
// abort) — that's the uniform drain rule both reference UIs follow
// (Claude Code's `commandQueue` + `useQueueProcessor`, Codex's
// `VecDeque<QueuedUserMessage>` + `maybe_send_next_queued_input`).
//
// Skips worktree-prep and draft-thread promotion because drain only
// fires post-first-send when the thread is fully established (there's
// no provider session before the first send, so `isTurnActive` can
// never be true on a draft thread, so the composer's enqueue branch
// never runs and this drain helper never sees a queued item without
// a real backend session to send to). Defensive bail when
// `getActiveTurn` is non-null guards against a fresh `turn_started`
// arriving between `projectTurnCompleted` and this listener firing.

import { SendMessageWithOptions } from './bindings';
import { getAllPanes } from './panes.svelte';
import {
  enqueueAtFront,
  popFront,
  type QueueItem,
} from './sendQueue.svelte';
import {
  clearPendingSend,
  getActiveTurn,
  projectSendStarted,
} from './threadStatuses.svelte';
import { clearRuntimeModeDraft } from './runtimeModeDraft.svelte';
import type { Thread } from '../types/models';
import { errString } from '../utils/errors';
import {
  buildSendOptions,
  type OutgoingSendOptions,
} from '../utils/sendOptions';

function toSendOptions(item: QueueItem): OutgoingSendOptions {
  return buildSendOptions({
    attachmentIds: item.attachments.map((attachment) => attachment.id),
    runtimeMode: item.runtimeMode,
    sourceProposedPlan: item.sourceProposedPlan,
    revisionSourceProposedPlan: item.revisionSourceProposedPlan,
    revisionSourceCommentIds: item.revisionSourceCommentIds,
  });
}

function reportThreadGeneralError(threadId: string, message: string): void {
  for (const pane of getAllPanes().values()) {
    if (pane.threadId !== threadId) continue;
    pane.setGeneralError(message);
  }
}

/**
 * Drain the head queued user message for `threadId`. On success the
 * backend's `provider:turn_started` clears the pendingSend flag via
 * `projectTurnStarted` and `clearRuntimeModeDraft` consumes the
 * staged runtime-mode override. On failure the helper restores the
 * popped item to the front so the user's work isn't lost, clears
 * pendingSend explicitly (since `projectTurnStarted` won't fire),
 * and surfaces the error through any matching pane's general-error
 * banner.
 */
export async function tryDrainNextQueued(threadId: string): Promise<void> {
  if (!threadId) return;
  if (getActiveTurn(threadId) !== null) return;
  const next = popFront(threadId);
  if (!next) return;
  projectSendStarted(threadId);
  try {
    (await SendMessageWithOptions(threadId, next.message, toSendOptions(next))) as Thread;
    // Success: `projectTurnStarted` will clear pendingSend when the
    // backend confirms the new round arms. The staged runtime-mode
    // override (if any) was just consumed by this send, so clear the
    // draft so the next idle dispatch doesn't re-apply it.
    clearRuntimeModeDraft(threadId);
  } catch (err) {
    console.error('Failed to drain queued message:', err);
    enqueueAtFront(threadId, next);
    clearPendingSend(threadId);
    reportThreadGeneralError(threadId, `Failed to send queued message: ${errString(err)}`);
  }
}
