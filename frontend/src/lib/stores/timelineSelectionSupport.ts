import type { TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { getPinnedBackend, requireEntityBackend } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { getTransportHelloFor } from './transportStatus.svelte';

export function requireTimelineSelectionSupport(threadId: string, selection?: TimelineSelection): void {
  if (!selection?.scopeRootId && !selection?.tools) return;
  const backend = requireEntityBackend(getPinnedBackend() ?? threadBackend(threadId));
  if (!getTransportHelloFor(backend)?.capabilities.includes('timeline.scopes.v1')) {
    throw new Error('Update the computer hosting this thread to load its agent history.');
  }
}
