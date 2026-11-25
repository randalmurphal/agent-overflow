// What the always-loaded stores tell the Issues stores, which load with the
// Issues pane: a store there registers here when it loads, and until one
// does nothing is held that the news could change, so it is dropped.

import type { BackendKey } from '../transport/backendKey';

const listsChanged = new Set<(backend: BackendKey) => void>();

/** Runs `listener` whenever the boards some project of a computer lists may
 * have changed; the returned function stops it. */
export function onTrackerBoardListsChanged(listener: (backend: BackendKey) => void): () => void {
  listsChanged.add(listener);
  return () => { listsChanged.delete(listener); };
}

/** The boards some project of `backend` lists may have changed: its
 * connections changed, or this window saved a project's boards. */
export function trackerBoardListsChanged(backend: BackendKey): void {
  for (const listener of listsChanged) listener(backend);
}
