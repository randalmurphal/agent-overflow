// Wire replay ends before queued item mutations and gap snapshots necessarily
// settle. Include both before publishing completion to mounted timelines.
// Recovery remains per backend; no event payloads are retained here.
import { attachedBackends, onBackendsChanged } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { pendingItemEventsSettled } from './itemEventSettlement';
import { DisconnectedError } from '../transport/wsClient';

export type RecoveryPhase = 'start' | 'complete' | 'cancel';
type Listener = (backend: BackendKey, phase: RecoveryPhase) => void;
const listeners = new Set<Listener>();
const subscriptions = new Map<BackendKey, () => void>();
interface Recovery {
  pending: Set<Promise<unknown>>;
  replay: Promise<void>;
  resolveReplay(): void;
  rejectReplay(error: Error): void;
  replayPending: boolean;
}
const active = new Map<BackendKey, Recovery>();

function cancel(backend: BackendKey): void {
  const recovery = active.get(backend);
  active.delete(backend);
  recovery?.rejectReplay(new DisconnectedError('connection changed during conversation recovery'));
}

function publish(backend: BackendKey, phase: RecoveryPhase): void {
  for (const listener of listeners) {
    try { listener(backend, phase); }
    catch (err) { console.warn('transportRecovery: listener threw', err); }
  }
}

async function complete(backend: BackendKey, recovery: Recovery): Promise<void> {
  const mutations = pendingItemEventsSettled();
  if (mutations) await mutations;
  if (active.get(backend) !== recovery) return;
  recovery.replayPending = false;
  recovery.resolveReplay();
  while (active.get(backend) === recovery && recovery.pending.size) {
    await Promise.allSettled([...recovery.pending]);
  }
  if (active.get(backend) !== recovery) return;
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
        cancel(id);
        let resolveReplay!: () => void;
        let rejectReplay!: (error: Error) => void;
        const replay = new Promise<void>((resolve, reject) => { resolveReplay = resolve; rejectReplay = reject; });
        // A connection can close before any snapshot has joined the fence.
        void replay.catch(() => {});
        active.set(id, { pending: new Set(), replay, resolveReplay, rejectReplay, replayPending: true });
        publish(id, phase);
      } else if (phase === 'complete') {
        const pending = active.get(id);
        if (pending) void complete(id, pending);
      } else {
        cancel(id);
        publish(id, phase);
      }
    });
    // A disconnect can also land while snapshot reads outlive the replay.
    const offStatus = client.onStatusChange((state) => {
      if (state.status !== 'connected') {
        cancel(id);
        publish(id, 'cancel');
      }
    });
    subscriptions.set(id, () => { offReplay(); offStatus(); });
  }
  for (const [id, off] of subscriptions) {
    if (live.has(id)) continue;
    off();
    subscriptions.delete(id);
    cancel(id);
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
  const pending = active.get(backend)?.pending;
  if (!pending) return;
  pending.add(work);
  void work.then(() => pending.delete(work), () => pending.delete(work));
}

/** Snapshot guards must be captured after old replay mutations have applied.
 * This fence excludes snapshot work itself, so a held recovery read can join it. */
export function pendingBackendReplay(backend: BackendKey): Promise<void> | undefined {
  const recovery = active.get(backend);
  return recovery?.replayPending ? recovery.replay : undefined;
}
