import { getBackendIdentity } from '../transport/backendIdentity';
import { addToast, removeToast } from './toast.svelte';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';
import { userFacingError } from '../utils/userFacingError';
import { GetConversationMutationState } from './bindings';
import { threadBackend } from '../transport/entityIndex';
import { withBackendTarget } from '../transport/backends';
import { whenTransportConnected } from './transportStatus.svelte';
import { fenceConversationSnapshot } from './eventsItemStream';
import { acknowledgeConversationRevision } from './eventsMessageRevert';
import type { ThreadPaneIngest } from './threadPaneRoles';

/** Capture the owner before the operation, and reconcile on that same owner. */
export function conversationMutationReconciler(pane: ThreadPaneIngest, threadId: string, userItemId: string, sendId = '') {
  const owner = threadBackend(threadId);
  const identity = getBackendIdentity(owner);
  const stillOwned = () => {
    const now = getBackendIdentity(owner);
    return threadBackend(threadId) === owner && now.backendId === identity.backendId && now.generation === identity.generation;
  };
  return async () => {
    const lifetime = new AbortController();
    let armed = false;
    const unsubscribe = onThreadHistoryInvalidated((owns) => {
      if (armed && owns(threadId)) lifetime.abort(new Error('Conversation ownership changed during recovery'));
    });
    armed = true;
    let retryToast: string | undefined;
    let retryNow: (() => void) | undefined;
    try {
      if (!stillOwned()) throw new Error('Conversation changed while restoring its state');
      for (;;) {
        try {
          await whenTransportConnected(owner, lifetime.signal);
          if (!stillOwned()) throw new Error('Conversation moved while restoring its state');
          const read = () => GetConversationMutationState(threadId, userItemId, sendId);
          const state = await (owner === undefined ? read() : withBackendTarget(owner, read));
          if (!stillOwned()) throw new Error('Conversation moved while restoring its state');
          fenceConversationSnapshot(threadId, state.itemEventSequence, state);
          if (pane.threadId === threadId) await pane.refreshFromBackend(true);
          acknowledgeConversationRevision(threadId, state.historyRev);
          return state;
        } catch (error) {
          if (lifetime.signal.aborted || !stillOwned()) throw error;
          if (!retryToast) retryToast = addToast('error', `Could not restore the conversation. Retrying; Send remains paused: ${userFacingError(error)}`, 0, {
            label: 'Retry now', run: () => retryNow?.(),
          });
          await new Promise<void>((resolve, reject) => {
            const clear = () => { clearTimeout(timer); retryNow = undefined; lifetime.signal.removeEventListener('abort', abort); };
            const abort = () => { clear(); reject(lifetime.signal.reason); };
            retryNow = () => { clear(); resolve(); };
            const timer = setTimeout(() => retryNow?.(), 5000);
            lifetime.signal.addEventListener('abort', abort, { once: true });
          });
        }
      }
    } finally {
      unsubscribe();
      if (retryToast) removeToast(retryToast);
    }
  };
}
