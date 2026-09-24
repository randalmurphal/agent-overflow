import type { Item } from '../types/models';
import type { ItemDeltaEvent, ItemMetaEvent, ItemPatchEvent } from '../types/events';
import type { UserMessageRevertedEvent } from '../types/messageRevert';
import type { BackendKey } from '../transport/backendKey';
import { registerWatchedScopeSource, registerWatchedThreadSource, refreshWatchedThreads } from './watchedThreads';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { errString } from '../utils/errors';
import { holdBackendRecovery } from './transportRecovery';

export type TimelineMutation =
  | { kind: 'upsert'; items: Item[] }
  | { kind: 'delta'; event: ItemDeltaEvent }
  | { kind: 'meta'; event: ItemMetaEvent }
  | { kind: 'patch'; event: ItemPatchEvent }
  | { kind: 'remove'; itemId: string }
  | { kind: 'revert'; event: UserMessageRevertedEvent };

export interface TimelineSurface {
  threadId: string;
  /**
   * The subagent scope roots whose rows this surface reads, within
   * `threadId`. The backend sends a child row only to a connection naming
   * its scope, so a scoped surface names every scope it admits rows from
   * while it is registered, and calls `refreshWatchedThreads` when that set
   * changes. Absent for a surface that reads root rows only.
   */
  scopeRootIds?(): Iterable<string>;
  backend(): BackendKey | undefined;
  apply(mutation: TimelineMutation): void;
  refresh(): Promise<void>;
}

const surfaces = new Set<TimelineSurface>();
registerWatchedThreadSource(() => Array.from(surfaces, surface => surface.threadId));
registerWatchedScopeSource(function* () {
  for (const surface of surfaces) {
    if (!surface.scopeRootIds) continue;
    for (const scopeRootId of surface.scopeRootIds()) yield { threadId: surface.threadId, scopeRootId };
  }
});

export function registerTimelineSurface(surface: TimelineSurface): () => void {
  surfaces.add(surface);
  refreshWatchedThreads();
  return () => { surfaces.delete(surface); refreshWatchedThreads(); };
}

export function applyTimelineMutation(threadId: string, mutation: TimelineMutation): void {
  for (const surface of surfaces) if (surface.threadId === threadId) surface.apply(mutation);
}

/** Refresh every surface, or only the surfaces showing `threads` when given. */
export function refreshTimelineSurfaces(threads?: ReadonlySet<string>): void {
  for (const surface of surfaces) {
    if (threads && !threads.has(surface.threadId)) continue;
    const owner = surface.backend();
    const work = surface.refresh();
    if (owner !== undefined) holdBackendRecovery(owner, work);
    void work.catch(error => reportFrontendDiagnostic('Timeline recovery failed', errString(error)));
  }
}
