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
}

let heldBytes = 0;
let pinnedBytes = 0;
let budgetForTest: number | null = null;
// Every evictable resident chunk is in exactly one of these two lists and
// leaves it the moment it stops being evictable: an entry keeps its store,
// and so all of that store's text, alive.
// Chunks not read since they arrived, in arrival order.
const unread = new Map<string, Entry>();
// Chunks read since they arrived, least recently read first.
const read = new Map<string, Entry>();

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

/** Takes a chunk off whichever list holds it. */
function drop(id: string): Entry | undefined {
  const entry = unread.get(id) ?? read.get(id);
  unread.delete(id);
  read.delete(id);
  return entry;
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

  /** Changes the bytes of text both held and pinned, which the budget
   * leaves out. */
  pin(delta: number): void {
    pinnedBytes += delta;
  },

  /** Makes a resident chunk evictable: as unread text when it just
   * arrived, else as the most recently read. */
  offer(store: EvictableText, chunk: number, arrived: boolean): void {
    const id = key(store, chunk);
    drop(id);
    (arrived ? unread : read).set(id, { store, chunk });
  },

  /** Marks an evictable chunk as just read. */
  touch(store: EvictableText, chunk: number): void {
    const id = key(store, chunk);
    const entry = drop(id);
    if (entry) read.set(id, entry);
  },

  /** Makes a chunk no longer evictable. */
  withdraw(store: EvictableText, chunk: number): void {
    drop(key(store, chunk));
  },

  /** Evicts chunks until the held text fits. */
  enforce(): void {
    const limit = budget() + pinnedBytes;
    if (heldBytes <= limit) return;
    // Unread text goes latest first; a Map only walks forward.
    const arrived = [...unread];
    for (let index = arrived.length - 1; index >= 0 && heldBytes > limit; index -= 1) {
      const [id, entry] = arrived[index];
      if (entry.store.pinned(entry.chunk)) continue;
      unread.delete(id);
      heldBytes -= entry.store.evict(entry.chunk);
    }
    for (const [id, entry] of read) {
      if (heldBytes <= limit) break;
      if (entry.store.pinned(entry.chunk)) continue;
      read.delete(id);
      heldBytes -= entry.store.evict(entry.chunk);
    }
  },

  /** Reports that evicted text, or something built from it, is back. */
  restored(): void {
    version += 1;
  },
};

/** Stored characters of text a reader walking a patch may read back and
 * hold pinned at once: an eighth of the budget, so what a walk holds past
 * the budget stays a fraction of it. A single line longer than this is
 * still held whole while it is read. */
export function patchTextReadAheadBytes(): number {
  return Math.floor(budget() / 8);
}

/** Held text in bytes: what tests read to see the budget kept. */
export function patchTextHeldBytes(): number {
  return heldBytes;
}

/** Held and pinned text in bytes, which the budget leaves out: what tests
 * read to see a reader's pins stay bounded. */
export function patchTextPinnedBytes(): number {
  return pinnedBytes;
}

/** Chunks the budget can still reach: what tests read to see a store let
 * go, since each one keeps its store, and so all of that store's text,
 * alive. */
export function patchTextEvictableChunks(): number {
  return unread.size + read.size;
}

export function setPatchTextBudgetForTest(bytes: number | null): void {
  budgetForTest = bytes;
}
