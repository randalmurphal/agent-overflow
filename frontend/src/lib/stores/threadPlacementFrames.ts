// The sidebar placement a thread's own computer announced in a
// thread:updated frame. Every window attached to that computer receives
// its frames, so a surface showing a thread another computer relays (an
// Agents tray row of a worker on a paired computer, whose spawner's
// computer reads that worker's computer only now and then) takes the
// placement from here instead. A leaf: events.ts publishes, a surface
// subscribes for the threads it shows.

import type { BackendKey } from '../transport/backendKey';

type Listener = (threadId: string, placement: string, backend: BackendKey) => void;

const listeners = new Set<Listener>();

/** Calls `listener` with each placement a thread's computer announces; returns a disposer. */
export function onThreadPlacementFrame(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** A frame from `backend`, the computer holding the thread, carried its row. */
export function publishThreadPlacementFrame(threadId: string, placement: string, backend: BackendKey): void {
  for (const listener of listeners) listener(threadId, placement, backend);
}
