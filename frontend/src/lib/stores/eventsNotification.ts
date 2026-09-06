// OS-notification activation routing: `notification:activated` events land
// here and either open the target thread immediately or wait in the bounded
// pre-hydration queue until App.svelte reports the thread registry is
// loaded. The queue mechanics live in notificationActivationQueue.ts; this
// module only binds them to the real thread and pane stores.
import { getThreadById } from './threads.svelte';
import { GetThread, WorkflowGetItem } from './bindings';
import type { Thread } from '../types/models';
import { attachedBackends, backendById, withBackendTarget } from '../transport/backends';
import { currentThreadRow, resolveThreadBackend, noteThread, workflowItemBackend, noteWorkflowItem } from '../transport/entityIndex';
import { hasScope } from '../transport/scopes';
import { openThreadInPane } from './panes.svelte';
import { openWorkflowRunInOverlay } from './workflowsOverlay.svelte';
import { addToast, removeToast } from './toast.svelte';
import { isTemporarilyUnavailableError } from './transportStatus.svelte';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { errString } from '../utils/errors';
import {
  createNotificationActivationQueue,
  type NotificationTarget,
} from './notificationActivationQueue';


/** A notification may name an archived thread or arrive before the catalog.
 * Known ownership wins over an old notification's source after a move. */
export async function resolveNotificationThread(id: string, backendId?: string): Promise<Thread | undefined> {
  const cached = getThreadById(id);
  if (cached && currentThreadRow(cached)) return cached;
  const owner = resolveThreadBackend(id);
  const entry = owner !== undefined ? backendById(owner) : backendId ? backendById(backendId) : undefined;
  const targets = entry ? [entry] : owner !== undefined || backendId ? [] : attachedBackends();
  const results = await Promise.allSettled(targets.filter((target) => hasScope('threads:read', target.id)).map(async (target) => {
    const row = await withBackendTarget(target.id, () => GetThread(id)) as Thread | null;
    if (row?.id !== id || backendById(target.id) !== target) return undefined;
    noteThread(row.id, target.id, row.ownershipEpoch ?? 0);
    return { row, backend: target.id };
  }));
  for (const result of results) {
    if (result.status === 'fulfilled' && result.value && currentThreadRow(result.value.row, result.value.backend)) return result.value.row;
  }
  throwLookupFailure(results);
  return undefined;
}

// A failed read is not evidence that the target was deleted. Preserve the
// failure so the activation surface can offer a retry on the same computer.
function throwLookupFailure(results: PromiseSettledResult<unknown>[]): void {
  const failure = results.find((result) => result.status === 'rejected'
    && !(result.reason instanceof TransportError && result.reason.code === 'not_found'));
  if (failure?.status === 'rejected') throw failure.reason;
}

/** Verify an unindexed workflow target before teaching the normal RPC router
 * its owner. Workflow details then load through their ordinary entity route. */
export async function resolveNotificationWorkflow(id: string, backendId?: string): Promise<boolean> {
  const owner = workflowItemBackend(id);
  if (owner !== undefined && backendById(owner)) return true;
  const entry = owner !== undefined ? backendById(owner) : backendId ? backendById(backendId) : undefined;
  const targets = entry ? [entry] : owner !== undefined || backendId ? [] : attachedBackends();
  const results = await Promise.allSettled(targets.filter((target) => hasScope('threads:read', target.id)).map(async (target) => {
    const detail = await withBackendTarget(target.id, () => WorkflowGetItem(id));
    if (detail?.item?.id !== id || backendById(target.id) !== target) return false;
    noteWorkflowItem(id, target.id);
    return true;
  }));
  if (results.some((result) => result.status === 'fulfilled' && result.value)) return true;
  throwLookupFailure(results);
  return false;
}

export type { NotificationTarget } from './notificationActivationQueue';

let retryToast: string | undefined;
function clearRetryToast(): void {
  if (retryToast) removeToast(retryToast);
  retryToast = undefined;
}

// The store bindings are referenced inside closures, not at module-eval
// time: suites that partially vi.mock the pane/thread stores (e.g.
// TerminalView.test.ts) import this module transitively via events.ts, and
// vitest mock proxies throw on touching an export the mock factory omitted.
function createAppNotificationActivationQueue() {
  return createNotificationActivationQueue({
    getThreadById: resolveNotificationThread,
    openThread: (thread) => openThreadInPane(thread),
    openWorkflowRun: async (workItemId, backendId) => {
      if (await resolveNotificationWorkflow(workItemId, backendId)) openWorkflowRunInOverlay(workItemId);
      else throw new Error('This workflow run is no longer available on its computer.');
    },
    console,
    failed: (target, error) => {
      clearRetryToast();
      const reason = isTemporarilyUnavailableError(error) || (error instanceof DisconnectedError && !error.terminal)
        ? 'Its computer is unavailable. Reconnect, then try again.'
        : errString(error).slice(0, 500);
      retryToast = addToast('error', `Could not open notification. ${reason}`, 0, {
        label: 'Try again',
        run: () => applyNotificationActivated(target),
      });
    },
  });
}

let notificationActivationQueue = createAppNotificationActivationQueue();

export function applyNotificationActivated(target: NotificationTarget): void {
  clearRetryToast();
  notificationActivationQueue.receive(target);
}

export async function markNotificationHydrated(): Promise<void> {
  await notificationActivationQueue.markHydrated();
}

export function resetNotificationActivationForTest(): void {
  clearRetryToast();
  notificationActivationQueue = createAppNotificationActivationQueue();
}
