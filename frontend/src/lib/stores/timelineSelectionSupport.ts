import type { TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { attachedBackendCount, attachedBackends, getPinnedBackend, requireEntityBackend, type BackendKey } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { getTransportHelloFor } from './transportStatus.svelte';

export function requireTimelineSelectionSupport(threadId: string, selection?: TimelineSelection): void {
  if (!selection?.scopeRootId && !selection?.tools && !selection?.digestItemId) return;
  const backend = requireEntityBackend(getPinnedBackend() ?? threadBackend(threadId));
  const capability = selection?.digestItemId ? 'timeline.digests.v1' : 'timeline.scopes.v1';
  if (!getTransportHelloFor(backend)?.capabilities.includes(capability)) {
    throw new Error('Update the computer hosting this thread to load its agent history.');
  }
}

/** The find bar's search contract: scoped SearchTimelineItems, main-only SearchThreadItems. */
export const THREAD_FIND_CAPABILITY = 'thread-find.v1';

/**
 * The backend a thread's find reads, or null when its owner is unknown or
 * does not serve the find contract. Reactive on ownership and hello.
 */
export function threadFindBackend(threadId: string): BackendKey | null {
  const backends = attachedBackends();
  const owner = threadBackend(threadId) ?? (attachedBackendCount() === 1 ? backends[0]?.id : undefined);
  if (owner === undefined) return null;
  return getTransportHelloFor(owner)?.capabilities.includes(THREAD_FIND_CAPABILITY) ? owner : null;
}

export function requireThreadFindSupport(threadId: string): void {
  if (threadFindBackend(threadId) === null) {
    throw new Error('Update the computer hosting this thread to find in it.');
  }
}
