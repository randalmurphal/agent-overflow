import { createEntityStore } from './entityStore.svelte';
import type { BackendKey } from '../transport/backendKey';
import type { PagedItems, TimelineScopeContext } from '../../../bindings/agent-overflow/internal/store/models';
import type { Item } from '../types/models';
import type { TimelineMutation } from './timelineSurfaces';
import { holdBackendRecovery } from './transportRecovery';

export type WindowObservation = TimelineMutation
  | { kind: 'snapshot'; page?: PagedItems | null; scope?: TimelineScopeContext | null; gone: boolean; contextVersion: number; touched: ReadonlySet<string>; unheldItems: ReadonlyMap<string, Item> };
export interface WindowResource {
  backend(): BackendKey | undefined;
  read(signal: AbortSignal): Promise<WindowObservation | null>;
  apply(value: WindowObservation): void;
  endRead(signal: AbortSignal, completed: boolean): void;
}
const owners = new Map<string, WindowResource>();
const pending = new Map<string, Promise<void>>();
const resources = createEntityStore<WindowObservation, WindowResource>({
  name: 'timeline window',
  rawValue: true,
  backendForKey: key => owners.get(key)?.backend(),
  onApply: (key, value) => owners.get(key)?.apply(value),
  source: async ({ key, getCtx, apply, signal }) => {
    const owner = getCtx();
    let completed = true;
    const work = owner.read(signal).then(value => {
      completed = value !== null && !signal.aborted;
      if (completed && value) apply(value);
    }).finally(() => owner.endRead(signal, completed));
    pending.set(key, work);
    const backend = owner.backend();
    if (backend !== undefined) holdBackendRecovery(backend, work);
    try { await work; }
    finally { if (pending.get(key) === work) pending.delete(key); }
    return () => {};
  },
});

export function attachTimelineWindow(key: string, owner: WindowResource) {
  if (owners.has(key)) throw new Error(`Timeline surface already attached: ${key}`);
  owners.set(key, owner);
  const attachment = resources.attach(key, owner);
  let released = false;
  let requested = false;
  let refreshing: Promise<void> | null = null;
  return {
    get error() { return attachment.error; },
    apply(value: TimelineMutation) { resources.apply(key, value, { preserveError: true }); },
    refresh(): Promise<void> {
      if (released) return Promise.resolve();
      requested = true;
      if (refreshing) return refreshing;
      const run = async () => {
        // Let an already active snapshot land before revalidating changes
        // observed during it. Repeated live updates must not starve reads.
        await pending.get(key);
        while (requested && !released) {
          requested = false;
          resources.invalidate(key);
          await pending.get(key);
        }
      };
      refreshing = run().finally(() => { refreshing = null; });
      return refreshing;
    },
    release() {
      if (released) return;
      released = true;
      attachment.release(); owners.delete(key);
    },
  };
}
