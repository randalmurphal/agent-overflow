// Wire replay ends before queued item mutations and gap snapshots necessarily
// settle. Include both before publishing completion to mounted timelines.
// Recovery remains per backend; no event payloads are retained here.
import { attachedBackends, onBackendsChanged } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { pendingItemEventsSettled } from './itemEventSettlement';

export type RecoveryPhase = 'start' | 'complete' | 'cancel';
type Listener = (backend: BackendKey, phase: RecoveryPhase) => void;
const listeners = new Set<Listener>();
const subscriptions = new Map<BackendKey, () => void>();
const active = new Map<BackendKey, Set<Promise<unknown>>>();

function publish(backend: BackendKey, phase: RecoveryPhase): void {
  for (const listener of listeners) {
    try { listener(backend, phase); }
    catch (err) { console.warn('transportRecovery: listener threw', err); }
  }
}

async function complete(backend: BackendKey, pending: Set<Promise<unknown>>): Promise<void> {
  const mutations = pendingItemEventsSettled();
  if (mutations) await mutations;
  while (active.get(backend) === pending && pending.size) {
    await Promise.allSettled([...pending]);
  }
  if (active.get(backend) !== pending) return;
  active.delete(backend);
  publish(backend, 'complete');
}

function sync(): void {
  const live = new Set<BackendKey>();
  for (const { id, client } of attachedBackends()) {
    live.add(id);
    if (subscriptions.has(id)) continue;
    const offReplay = client.onReplay((phase) => {
      if (phase === 'start') {
        active.set(id, new Set());
        publish(id, phase);
      } else if (phase === 'complete') {
        const pending = active.get(id);
        if (pending) void complete(id, pending);
      } else {
        active.delete(id);
        publish(id, phase);
      }
    });
    // A disconnect can also land while snapshot reads outlive the replay.
    const offStatus = client.onStatusChange((state) => {
      if (state.status !== 'connected') {
        active.delete(id);
        publish(id, 'cancel');
      }
    });
    subscriptions.set(id, () => { offReplay(); offStatus(); });
  }
  for (const [id, off] of subscriptions) {
    if (live.has(id)) continue;
    off();
    subscriptions.delete(id);
    active.delete(id);
    publish(id, 'cancel');
  }
}

onBackendsChanged(sync);
sync();

export function onBackendRecovery(listener: Listener): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function isBackendRecovering(backend: BackendKey): boolean {
  return active.has(backend);
}

export function holdBackendRecovery(backend: BackendKey, work: Promise<unknown>): void {
  const pending = active.get(backend);
  if (!pending) return;
  pending.add(work);
  void work.then(() => pending.delete(work), () => pending.delete(work));
}
