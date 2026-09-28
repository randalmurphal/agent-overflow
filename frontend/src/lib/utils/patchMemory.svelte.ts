import { isCompactLayout } from '../stores/layoutMode.svelte';

// The budget for patch text the review surfaces hold, across every open
// diff. A diff that fits holds all of its text and releases its handle. A
// diff that passes the budget keeps its handle open and gives up chunks,
// which read again from the handle when a row or an exact reader needs
// them. Line indexes stay resident either way, so rows, counts and anchors
// never wait for text.
//
// Text nobody has read goes first, the latest to arrive first: a diff
// larger than the budget keeps its beginning, where the reader starts,
// and a read in progress is never interleaved with reads of text it just
// gave up. Text that has been read goes after, least recently read first.
// Pinned text (a line still arriving, a range a reader holds) stays and is
// not counted: the budget is met from the rest, so a pin never pushes out
// the text that would otherwise stay.

export const PATCH_TEXT_BUDGET_BYTES = 256 * 1024 * 1024;
export const COMPACT_PATCH_TEXT_BUDGET_BYTES = 64 * 1024 * 1024;

/** A store whose chunks can be evicted and read again. */
export interface EvictableText {
  readonly id: number;
  /** Whether the chunk must stay: an open line is being joined across it,
   * a reader pinned it, or it is being read again. */
  pinned(chunk: number): boolean;
  /** Drops the chunk's text. Returns the bytes freed. */
  evict(chunk: number): number;
}

interface Entry {
  store: EvictableText;
  chunk: number;
  /** Not read since it arrived; listed in `unread`. */
  unread: boolean;
}

let heldBytes = 0;
let pinnedBytes = 0;
let budgetForTest: number | null = null;
// Every evictable resident chunk.
const entries = new Map<string, Entry>();
// Chunks read since they arrived, least recently read first.
const read = new Map<string, Entry>();
// Chunks not read since they arrived, in arrival order. Entries that were
// read or withdrawn since stay until the list is compacted.
let unread: Entry[] = [];
let staleUnread = 0;

// Bumped when evicted text is read again. Readers that met evicted text
// read this, so they run again once the text is back.
let version = $state(0);

/** Reactive: changes whenever evicted patch text becomes resident again. */
export function patchTextVersion(): number {
  return version;
}

function budget(): number {
  if (budgetForTest !== null) return budgetForTest;
  return isCompactLayout() ? COMPACT_PATCH_TEXT_BUDGET_BYTES : PATCH_TEXT_BUDGET_BYTES;
}

function key(store: EvictableText, chunk: number): string {
  return `${store.id}:${chunk}`;
}

function leaveUnread(entry: Entry): void {
  entry.unread = false;
  staleUnread += 1;
  if (staleUnread > 64 && staleUnread * 2 > unread.length) {
    unread = unread.filter((candidate) => candidate.unread);
    staleUnread = 0;
  }
}

function drop(id: string, entry: Entry): void {
  entries.delete(id);
  if (entry.unread) leaveUnread(entry);
  else read.delete(id);
}

export const patchMemory = {
  /** Counts text a store now holds. */
  add(bytes: number): void {
    heldBytes += bytes;
  },

  /** Stops counting text a store no longer holds. */
  remove(bytes: number): void {
    heldBytes -= bytes;
  },

  /** Changes the bytes of pinned text, which the budget leaves out. */
  pin(delta: number): void {
    pinnedBytes += delta;
  },

  /** Makes a resident chunk evictable: as unread text when it just
   * arrived, else as the most recently read. */
  offer(store: EvictableText, chunk: number, arrived: boolean): void {
    const id = key(store, chunk);
    const existing = entries.get(id);
    if (existing) drop(id, existing);
    const entry: Entry = { store, chunk, unread: arrived };
    entries.set(id, entry);
    if (arrived) unread.push(entry);
    else read.set(id, entry);
  },

  /** Marks an evictable chunk as just read. */
  touch(store: EvictableText, chunk: number): void {
    const id = key(store, chunk);
    const entry = entries.get(id);
    if (!entry) return;
    if (entry.unread) leaveUnread(entry);
    else read.delete(id);
    read.set(id, entry);
  },

  /** Makes a chunk no longer evictable. */
  withdraw(store: EvictableText, chunk: number): void {
    const id = key(store, chunk);
    const entry = entries.get(id);
    if (entry) drop(id, entry);
  },

  /** Evicts chunks until the held text fits. */
  enforce(): void {
    const limit = budget() + pinnedBytes;
    if (heldBytes <= limit) return;
    const evict = (id: string, entry: Entry): void => {
      drop(id, entry);
      heldBytes -= entry.store.evict(entry.chunk);
    };
    // Evicting can compact `unread`; the entries' flags stay current.
    const arrived = unread;
    for (let index = arrived.length - 1; index >= 0 && heldBytes > limit; index -= 1) {
      const entry = arrived[index];
      if (!entry.unread || entry.store.pinned(entry.chunk)) continue;
      evict(key(entry.store, entry.chunk), entry);
    }
    for (const [id, entry] of read) {
      if (heldBytes <= limit) break;
      if (!entry.store.pinned(entry.chunk)) evict(id, entry);
    }
  },

  /** Reports that evicted text, or something built from it, is back. */
  restored(): void {
    version += 1;
  },
};

/** Held text in bytes, for tests and measurement. */
export function patchTextHeldBytes(): number {
  return heldBytes;
}

export function setPatchTextBudgetForTest(bytes: number | null): void {
  budgetForTest = bytes;
}
