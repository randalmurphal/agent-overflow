// The archived threads one done footer of an assistant's rows holds
// (docs/specs/standing-agents.md, "Sidebar"), read a page at a time while
// the footer is open: enough for the rows it shows, more as it shows more
// (ListArchivedSpawnedThreads, by the projects the footer's container
// counts and a cursor). A sidebar list holds the footers it has open;
// what no list holds is dropped, so a footer that closes forgets its rows
// and reads afresh when it opens again.
//
// An entry is for the container's count on the assistant's row
// (archivedChildCounts): the count moves with every archive, unarchive
// and delete of a thread under it, so rows read for one count are stale
// once the row says another, and the footer reads again from the newest.

import { SvelteMap } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { ListArchivedSpawnedThreads } from './bindings';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { THREAD_REVEAL_INCREMENT } from '../utils/sidebarThreadLimits';
import type { Thread } from '../types/models';
import { addErrorToast } from './toast.svelte';

/** The backend's bound on one page (store.MaxArchivedChildrenPage). */
const MAX_PAGE = 100;

/** What an open footer asks for. */
export interface ArchivedChildrenWant {
  /** The assistant's current thread, which the read is against. */
  agentThreadId: string;
  /** The projects the footer's container counts, '' for the card. */
  scopes: readonly string[];
  /** The container's archived count on the assistant's row. */
  count: number;
  /** How many of its archived threads the footer would show now. */
  needed: number;
}

/** The rows read for a footer: newest first, and whether more follow. */
export interface ArchivedChildrenRows {
  threads: readonly Thread[];
  more: boolean;
}

interface Entry {
  want: ArchivedChildrenWant;
  threads: readonly Thread[];
  more: boolean;
  /** A page has landed for this count. */
  loaded: boolean;
  /** The read in flight, by a ticket only it holds; null when none is. */
  reading: object | null;
}

// Entries are replaced whole, so a reader reruns only for its footer.
const entries = new SvelteMap<string, Entry>();
/** The footers each sidebar list holds open. */
const holders = new Map<object, ReadonlySet<string>>();

/** The key of an assistant's footer in one container. */
export function archivedChildrenKey(agentThreadId: string, containerKey: string): string {
  return `${agentThreadId}\u0000${containerKey}`;
}

/** The footer's rows, or null while no page for its current count has landed. */
export function getArchivedChildren(key: string): ArchivedChildrenRows | null {
  const entry = entries.get(key);
  return entry?.loaded ? { threads: entry.threads, more: entry.more } : null;
}

/**
 * The footers `holder` (one sidebar list) has open, with what each wants:
 * each reads what it needs, and a footer no list holds is dropped. Call
 * with an empty map, or releaseArchivedChildren, when the list goes.
 */
export function holdArchivedChildren(holder: object, wanted: ReadonlyMap<string, ArchivedChildrenWant>): void {
  untrack(() => {
    holders.set(holder, new Set(wanted.keys()));
    for (const [key, want] of wanted) ensure(key, want);
    dropUnheld();
  });
}

export function releaseArchivedChildren(holder: object): void {
  if (!holders.delete(holder)) return;
  untrack(dropUnheld);
}

function dropUnheld(): void {
  for (const key of [...entries.keys()]) {
    if (![...holders.values()].some((keys) => keys.has(key))) entries.delete(key);
  }
}

function sameWant(a: ArchivedChildrenWant, b: ArchivedChildrenWant): boolean {
  return a.agentThreadId === b.agentThreadId && a.count === b.count
    && a.scopes.length === b.scopes.length && a.scopes.every((scope, i) => scope === b.scopes[i]);
}

function ensure(key: string, want: ArchivedChildrenWant): void {
  let entry = entries.get(key);
  if (!entry || !sameWant(entry.want, want)) {
    // A new count or container: what was read no longer fits it, and a
    // read still out answers for the old one.
    entry = { want, threads: [], more: true, loaded: false, reading: null };
    entries.set(key, entry);
  } else if (entry.want.needed !== want.needed) {
    entry = { ...entry, want };
    entries.set(key, entry);
  }
  if (entry.reading || !entry.more || entry.threads.length >= want.needed) return;
  readPage(key, entry);
}

function readPage(key: string, entry: Entry): void {
  const ticket = {};
  entries.set(key, { ...entry, reading: ticket });
  const { agentThreadId, scopes } = entry.want;
  const last = entry.threads[entry.threads.length - 1];
  const after = last ? { updatedAt: last.updatedAt, id: last.id } : { updatedAt: 0, id: '' };
  const limit = Math.min(MAX_PAGE, Math.max(THREAD_REVEAL_INCREMENT, entry.want.needed - entry.threads.length));
  // A thread the index does not know is local, as everywhere else.
  const backend = threadBackend(agentThreadId) ?? HOME_BACKEND;
  void withBackendTarget(backend, () => ListArchivedSpawnedThreads(agentThreadId, [...scopes], after, limit)).then((page) => {
    const current = entries.get(key);
    if (current?.reading !== ticket) return;
    const next: Entry = {
      ...current,
      threads: [...current.threads, ...((page?.threads ?? []) as Thread[])],
      more: page?.more === true,
      loaded: true,
      reading: null,
    };
    entries.set(key, next);
    // Shown more while this page was out.
    if (next.more && next.threads.length < next.want.needed) readPage(key, next);
  }).catch((err: unknown) => {
    const current = entries.get(key);
    if (current?.reading !== ticket) return;
    // The next hold of the footer reads again.
    entries.delete(key);
    if (isPassiveConnectionFailure(err)) return;
    console.error(`Failed to load the archived threads under ${agentThreadId}:`, err);
    addErrorToast('Failed to load finished threads', err);
  });
}

/**
 * Drop what was read under a thread that stopped being an assistant's
 * current thread: its footers read against its successor.
 */
export function forgetArchivedChildren(agentThreadId: string): void {
  const prefix = `${agentThreadId}\u0000`;
  for (const key of [...entries.keys()]) if (key.startsWith(prefix)) entries.delete(key);
}

/** Test helper. */
export function resetArchivedChildrenForTest(): void {
  entries.clear();
  holders.clear();
}
