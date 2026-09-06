import { DisconnectedError } from '../transport/wsClient';
import { advanceCatalogRevision, catalogRevision } from './computerCatalogRevision';
// A failed list read is not an empty computer. Read each computer's share
// independently and retain only that computer's cached rows on failure.
import { attachedBackends, withBackendTarget, type BackendEntry } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { getReplicaCatalog, putReplicaCatalog, replicaCatalogStamp } from '../replica/session';
import type { CatalogKind, CatalogRows } from '../replica/catalog';
import { getBackendIdentity } from '../transport/backendIdentity';
import { readBeforeDeadline } from '../utils/readBeforeDeadline';
import { untrack } from 'svelte';

interface ComputerCatalog<T> {
  read(backend: BackendKey): Promise<T[] | null>;
  write(backend: BackendKey, rows: T[]): Promise<void>;
  hasRows(backend: BackendKey): boolean;
  begin(backend: BackendKey): () => boolean;
  applyLate?(result: ComputerRows<T>): void;
}

export function computerCatalog<K extends CatalogKind>(
  kind: K, previous: () => readonly CatalogRows[K][], owner: (row: CatalogRows[K]) => BackendKey | undefined,
  applyLate?: (result: ComputerRows<CatalogRows[K]>) => void,
): ComputerCatalog<CatalogRows[K]> {
  // Reading the fallback must never subscribe a mount loader to its own
  // result. Accept a getter so callers cannot accidentally read it first.
  const populated = untrack(() => new Set(previous().map((row) => owner(row) ?? HOME_BACKEND)));
  const stamps = new Map<BackendKey, string | null>();
  return {
    applyLate,
    begin(backend) {
      const revision = advanceCatalogRevision(backend, kind);
      const stamp = replicaCatalogStamp(backend, kind);
      stamps.set(backend, stamp);
      return () => revision === catalogRevision(backend, kind)
        && (!stamp || stamp === replicaCatalogStamp(backend, kind));
    },
    read: (backend) => getReplicaCatalog(backend, kind),
    write: (backend, rows) => putReplicaCatalog(backend, kind, rows, stamps.get(backend) ?? null),
    hasRows: (backend) => populated.has(backend),
  };
}

export interface ComputerRows<T> {
  rows: T[];
  answered: ReadonlySet<BackendKey>;
  attached: ReadonlySet<BackendKey>;
}

export async function readComputerRows<T>(
  read: () => PromiseLike<T[] | null>,
  note: (row: T, backend: BackendKey) => void,
  cache?: ComputerCatalog<T>,
  admit?: (row: T, backend: BackendKey) => boolean,
  applyLate?: (result: ComputerRows<T>) => void,
): Promise<ComputerRows<T> | null> {
  const targets = attachedBackends().slice();
  const results = await Promise.all(targets.map(async (target) => {
    const identity = getBackendIdentity(target.id);
    const currentRead = cache?.begin(target.id) ?? (() => true);
    function stillCurrent(): boolean {
      const current = getBackendIdentity(target.id);
      return currentRead() && attachedBackends().includes(target)
        && (!identity.backendId || (identity.backendId === current.backendId && identity.generation === current.generation));
    }
    let error: unknown;
    try {
      // Every saved computer gets its initial dial, independently and under
      // the same deadline. A phone need not have a legacy HOME slot at all;
      // rejecting its first non-home read would leave boot/notification
      // hydration incomplete even after the sidebar later reconnects.
      const initialDial = !target.client.getHello?.();
      if (!initialDial && target.status.status !== 'connected') throw new DisconnectedError('Computer is offline.');
      const rows = await readBeforeDeadline(withBackendTarget(target.id, read), 2500, (late) => {
        const apply = cache?.applyLate ?? applyLate;
        if (!apply || !stillCurrent()) return;
        const arrived = late ?? [];
        for (const row of arrived) note(row, target.id);
        void cache?.write(target.id, arrived);
        apply({ rows: admit ? arrived.filter((row) => admit(row, target.id)) : arrived, answered: new Set([target.id]),
          attached: new Set(attachedBackends().map((entry) => entry.id)) });
      }) ?? [];
      const current = getBackendIdentity(target.id);
      if (identity.backendId && (identity.backendId !== current.backendId || identity.generation !== current.generation)) {
        throw new Error('Computer history changed during the read.');
      }
      return { target, rows, answered: true, identity: current, currentRead, cached: false };
    } catch (reason) { error = reason; }
    const cachedIdentity = getBackendIdentity(target.id);
    const previous = cache?.hasRows(target.id) ?? false;
    const rows = cache && !previous ? await cache.read(target.id) : null;
    return { target, rows: rows ?? [], answered: false, error, identity: cachedIdentity, currentRead, cached: previous || rows !== null };
  }));
  const live = new Set<BackendEntry>(attachedBackends());
  const answered = new Set<BackendKey>();
  const rows: T[] = [];
  let error: unknown;
  let hasCache = false;
  let currentResults = 0;
  for (const result of results) {
    const target = result.target;
    // One final membership check covers both RPC and IndexedDB awaits.
    if (!live.has(target) || !result.currentRead()) continue;
    const current = getBackendIdentity(target.id);
    if (result.identity.backendId !== current.backendId || result.identity.generation !== current.generation) continue;
    currentResults++;
    error ??= result.error;
    hasCache ||= result.cached;
    if (result.answered) {
      answered.add(target.id);
      if (cache) void cache.write(target.id, result.rows);
    }
    for (const row of result.rows) note(row, target.id);
  }
  // All origins must be indexed before admission: arrival/attachment order
  // cannot decide ownership when an offline catalog predates a move.
  for (const result of results) {
    const current = getBackendIdentity(result.target.id);
    if (!live.has(result.target) || !result.currentRead()
      || result.identity.backendId !== current.backendId || result.identity.generation !== current.generation) continue;
    for (const row of result.rows) if (!admit || admit(row, result.target.id)) rows.push(row);
  }
  // Superseded reads have no authority and no failure to report. In
  // particular, boot and a connection's first hello can overlap. Keep
  // cancellation distinct from both an authoritative empty list and an
  // actual failed dial, so callers cannot mark an unknown catalog loaded.
  if (currentResults === 0) return null;
  // An asleep saved computer is ordinary state. Its connection banner
  // owns the explanation; cached catalogs remain usable.
  // No answer and no cache is unknown, never an authoritative empty list:
  // treating it as empty would erase saved panes on a cold offline startup.
  if (answered.size === 0 && !hasCache) throw error ?? new Error('No computer could be reached.');
  return { rows, answered, attached: new Set([...live].map((entry) => entry.id)) };
}

export function retainUnavailableComputerRows<T>(
  previous: readonly T[], result: ComputerRows<T>, owner: (row: T) => BackendKey | undefined,
): T[] {
  const retained = previous.filter((row) => {
    const backend = owner(row) ?? HOME_BACKEND;
    return result.attached.has(backend) && !result.answered.has(backend);
  });
  return retained.length ? [...result.rows, ...retained] : result.rows;
}
