/**
 * The question a Stop asks when it would kill background agents.
 *
 * A Claude interrupt kills every live async agent the session holds, so
 * the backend refuses a person's Stop while such agents are live until the
 * caller confirms (transport/backgroundKillRefusal.ts). The askers are the
 * Stop flows in stores/revertOnInterrupt.svelte.ts and the thread.interrupt
 * builtin: plain async functions with no place to render, so this is the
 * promise-shaped door unsentMessageConfirmation.svelte.ts is, and
 * components/composer/BackgroundKillConfirmationHost.svelte mounts once at
 * the app root and settles whatever is pending.
 *
 * One question at a time. A second ask while one is open settles the first
 * as "keep them" and takes its place: the newer press is the person's
 * current intent, and the older question's agent list may be stale. The
 * question also settles as "keep them" when there is nothing left to stop:
 * the turn it was about completes or reverts (threadStatuses.svelte.ts),
 * its thread is torn down, or the thread's history is invalidated.
 */

import type { BackgroundKillAgent } from '../transport/backgroundKillRefusal';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';

export interface PendingBackgroundKill {
  readonly threadId: string;
  /** The agents the refused Stop named; empty when the list was unreadable. */
  readonly agents: readonly BackgroundKillAgent[];
}

interface PendingAsk extends PendingBackgroundKill {
  resolve: (stop: boolean) => void;
}

let pending: PendingAsk | null = $state.raw(null);

/** The open question, or null. Tracked. */
export function pendingBackgroundKillConfirmation(): PendingBackgroundKill | null {
  return pending;
}

/**
 * Ask whether to stop the turn and the background agents with it. Resolves
 * true for "stop everything", false for "keep them running".
 */
export function confirmBackgroundKill(
  threadId: string,
  agents: readonly BackgroundKillAgent[],
): Promise<boolean> {
  const previous = pending;
  const promise = new Promise<boolean>((resolve) => {
    pending = { threadId, agents, resolve };
  });
  previous?.resolve(false);
  return promise;
}

/** Settle the open question. No-op when nothing is pending. */
export function resolveBackgroundKillConfirmation(stop: boolean): void {
  const current = pending;
  pending = null;
  current?.resolve(stop);
}

/**
 * Nothing on the thread is left to stop (its turn ended, it is being torn
 * down, or its history moved): the open question, if it is this thread's,
 * is "keep them".
 */
export function cancelBackgroundKillConfirmationForThread(threadId: string): void {
  if (pending?.threadId !== threadId) return;
  resolveBackgroundKillConfirmation(false);
}

export function resetForTest(): void {
  resolveBackgroundKillConfirmation(false);
}

onThreadHistoryInvalidated((owns) => {
  if (pending && owns(pending.threadId)) resolveBackgroundKillConfirmation(false);
});
