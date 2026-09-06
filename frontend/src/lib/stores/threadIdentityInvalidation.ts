// A history reset affects the threads owned by that computer. A display-name
// change and another computer's first connection do not invalidate them.
import { onBackendIdentity } from '../transport/backendIdentity';
import { onBackendDetached } from '../transport/backends';
import { HOME_BACKEND, onThreadOwnershipChanged, threadBackend } from '../transport/entityIndex';

type Invalidator = (owns: (threadId: string) => boolean) => void;
export function onThreadHistoryInvalidated(invalidate: Invalidator): () => void {
  const ownership = onThreadOwnershipChanged((id) => invalidate((threadId) => threadId === id));
  const identities = new Map<string, string>();
  const identity = onBackendIdentity((next, backend) => {
    const key = `${next.backendId}/${next.generation}`;
    if (identities.get(backend) === key) return;
    identities.set(backend, key);
    invalidate((threadId) => (threadBackend(threadId) ?? HOME_BACKEND) === backend);
  });
  const detach = onBackendDetached(({ backendId, threadIds }) => {
    identities.delete(backendId);
    const gone = new Set(threadIds);
    invalidate((threadId) => gone.has(threadId));
  });
  return () => { identity(); detach(); ownership(); };
}
