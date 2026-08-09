// Per-thread row-height priors: persistence of measured sizes across
// thread switches AND app restarts, and the live per-row estimate
// resolver the engine uses for unmeasured rows.
//
// MODEL: per-row signature → measured px, not a positional whole-window
// snapshot. The prior generation of this module kept ONE `sizes:
// number[]` array per thread, keyed against a whole-window
// `structureSig` — the newline-join of every loaded row's signature
// (utils/timelineStructureSignature.ts). That whole-window key made
// replay brittle exactly where it mattered most: a thread's loaded
// window grows to hundreds of rows over a session (streaming appends,
// loadOlder, prunes), but a fresh app boot always starts from a small
// initial slice — so the whole-window signature captured before restart
// almost never equals the signature computed after, and the persisted
// snapshot never replayed except on tiny threads. Keying each row
// independently by its OWN signature
// (utils/timelineStructureSignature.ts's `nodeSignature`) fixes both
// problems at once: the boot window's rows are a SUFFIX subset of a
// larger session window's rows, and each one resolves independently
// against the shared per-row map — window composition no longer has to
// match, only the individual rows that are still present.
//
// VALIDITY: two dimensions still gate an entire entry (a mismatch on
// either refuses every row in it, degrading the whole mount to the
// kind/flat estimate chain):
//
//   - width        : the wrap point — a narrower/wider pane changes every
//                    multi-line row's height, so it is a whole-entry miss.
//   - expansionSig : non-default row-UI expansion state
//                    (`pane.expansionSignature()`).
//
// The structure/content dimension is NOT a top-level key anymore — it is
// folded into the per-row map key itself (`nodeSignature` encodes id,
// status, summary length, updatedAt / group membership), so a row whose
// content changed simply has a different map key and misses on its own,
// without invalidating its still-valid siblings.
//
// PERSISTENCE: entries survive an app restart through the storage
// adapter seam below (`SizePriorsStorageAdapter`). This module stays
// DOM-free — no `localStorage` import here — so it has no opinion about
// where entries live at rest; `utils/virtual/priorsStorage.ts`
// implements the real localStorage-backed adapter and wires itself in
// via `setSizePriorsStorageAdapter`, installed at module scope of
// `components/chat/timelineSizePriors.svelte.ts` so it is active before
// any pane mounts. The in-memory `entries` Map here is an LRU WORKING SET
// over that persistent store, not the store itself:
//
//   - A memory miss in `getThreadSizePriors` falls through to
//     `adapter.load()`; a hit there is installed back into the LRU.
//   - A memory eviction past `MAX_ENTRIES` never calls `adapter.remove()`
//     — eviction is memory housekeeping (the thread's priors are still
//     on disk and rehydrate on its next visit), not a deletion.
//   - Only `clearThreadSizePriors` (a real thread deletion — see
//     `threads.svelte.ts removeThread` / `thread.svelte.ts` reswitch
//     eviction) calls `adapter.remove()`.
//
// `setThreadSizePriors` REPLACES a thread's entry wholesale on every
// capture rather than merging row-by-row. This is deliberate: streaming
// rows carry `updatedAt`/`summary.length` in their signature, so a row's
// key changes on every append — merging would accumulate an ever-growing
// tail of dead signatures from rows that no longer exist in that exact
// form. A wholesale replace self-cleans that churn for free. The consumer
// (timelineSizePriors.svelte.ts `maybePersistSizePriors`) builds each
// replacement by carrying forward the previous entry's sizes for
// signatures still live in the current window — so an early capture with
// few (or no) measured rows cannot destroy a settled one — but the store
// contract here stays a plain replace.
//
// Consumption is unchanged in spirit from the prior generation: the
// engine reads priors lazily per row through `RowEstimate` whenever a row
// is unmeasured. Estimates only ever PREDICT placement for unmeasured
// rows — they never decide what a row renders, and a measurement always
// overrides them in the size store (plan §2 Priors, §8 D2). Because
// per-row lookup is index-free (no positional snapshot to keep in sync
// with the row it describes), head splices need no remap step — contrast
// the deleted `RowEstimate.shiftBase`, which remapped the old positional
// snapshot's base index across a load-older prepend/removal.
//
// KNOWN RESIDUAL — display settings are NOT keyed. Four global settings
// change a timeline row's height at constant width/structure/expansion:
// `fontSize` (a root font-scale on <html>, so it rescales every row),
// `sansFont` and `monoFont` (typeface metrics shift line heights at a
// fixed width), and `collapseDiffPreviews` (the default expand/collapse
// of an un-overridden inline diff card — DiffFileBlock). Toggling one
// mid-session and then revisiting a thread can replay sizes measured
// under the old setting. This is deliberately tolerated rather than
// keyed, because it is benign and self-correcting: a stale replay feeds
// the engine wrong start heights, the per-row ResizeObserver corrects
// them, and MessageTimeline's warm-up visibility gate hides that
// cascade exactly as it hides a cold first visit — so the worst case
// degrades to first-visit behavior, never a crash or a stuck viewport.
// The benefit of keying them is therefore invisible (same masked
// cascade either way), while a partial display-settings key would
// silently regress the day a new height-affecting setting is added and
// not threaded through it. If it must become airtight, add ONE
// display-settings dimension — covering all four — here AND in
// timelineSizePriors.svelte.ts's validity check, not a subset.

import type { RowEstimate } from './types';

export interface SizePriorsEntry {
  /** Math.round(scroll-surface content width) at capture. */
  width: number;
  /** `pane.expansionSignature()` at capture. */
  expansionSig: string;
  /** nodeSignature(node) → last measured px, for every row measured at capture. */
  rows: Map<string, number>;
}

/** Persists priors past the in-memory LRU (utils/virtual/priorsStorage.ts). */
export interface SizePriorsStorageAdapter {
  load(threadId: string): SizePriorsEntry | undefined;
  /** The adapter owns its own write debouncing. */
  persist(threadId: string, entry: SizePriorsEntry): void;
  remove(threadId: string): void;
}

let storageAdapter: SizePriorsStorageAdapter | undefined;

export function setSizePriorsStorageAdapter(adapter: SizePriorsStorageAdapter | undefined): void {
  storageAdapter = adapter;
}

// Each entry holds ~one float per loaded row plus its signature string;
// both scale with the live window and are bounded here. 50 recently
// visited threads is a generous working set; older threads fall back to
// the adapter (if the thread is still stored) or kind estimates, which is
// correct, just not pixel-exact until the next capture.
const MAX_ENTRIES = 50;

const entries = new Map<string, SizePriorsEntry>();

function evictOverCap(): void {
  // Memory-only eviction: the store below is the persistent copy, so
  // dropping a thread from this LRU never calls adapter.remove().
  while (entries.size > MAX_ENTRIES) {
    const oldest = entries.keys().next().value;
    if (oldest === undefined) break;
    entries.delete(oldest);
  }
}

export function setThreadSizePriors(threadId: string, entry: SizePriorsEntry): void {
  // Re-insert to bump LRU recency.
  if (entries.has(threadId)) {
    entries.delete(threadId);
  }
  entries.set(threadId, entry);
  evictOverCap();
  storageAdapter?.persist(threadId, entry);
}

/**
 * The stored entry for a thread — a memory hit bumps LRU recency; a
 * memory miss falls through to the storage adapter (if installed) and,
 * on a storage hit, installs the result into the in-memory LRU before
 * returning it. Validity checking (width/expansionSig, per-row signature
 * lookup) is the consumer's job — see
 * `components/chat/timelineSizePriors.svelte.ts`.
 */
export function getThreadSizePriors(threadId: string): SizePriorsEntry | undefined {
  const hit = entries.get(threadId);
  if (hit) {
    entries.delete(threadId);
    entries.set(threadId, hit);
    return hit;
  }
  const loaded = storageAdapter?.load(threadId);
  if (!loaded) return undefined;
  entries.set(threadId, loaded);
  evictOverCap();
  return loaded;
}

/**
 * Whether the thread's entry is already in the in-memory LRU — WITHOUT
 * bumping recency or falling through to the adapter. Lets the consumer
 * distinguish a memory hit from a storage hydration for the cold-load
 * trace (`replayStats` in timelineSizePriors.svelte.ts); call it BEFORE
 * `getThreadSizePriors`, which installs a storage hit into memory.
 */
export function hasThreadSizePriorsInMemory(threadId: string): boolean {
  return entries.has(threadId);
}

export function clearThreadSizePriors(threadId: string): void {
  entries.delete(threadId);
  storageAdapter?.remove(threadId);
}

export function clearAllThreadSizePriorsForTest(): void {
  entries.clear();
}

/** Test-only: raw stored entry without the validity gate, memory only. */
export function peekThreadSizePriorsForTest(threadId: string): SizePriorsEntry | undefined {
  return entries.get(threadId);
}

/** Diagnostic accounting (memoryReport). */
export function sizePriorsStats(): { threads: number; rows: number } {
  let rows = 0;
  for (const entry of entries.values()) rows += entry.rows.size;
  return { threads: entries.size, rows };
}

export interface RowEstimateOptions {
  /** Per-row prior lookup (already validity-gated by the caller), if any. */
  rowPrior?: (index: number) => number | undefined;
  /**
   * Computed estimate for rows whose height is a function of live state
   * rather than of kind — an activity run is a ~24px chip collapsed and up
   * to its cap expanded, and one kind-table entry would be wrong by ~20×
   * in whichever state it did not describe.
   */
  structuralSize?: (index: number) => number | undefined;
  /** Maps a row index to its kind for the kind-height table. */
  kindOf?: (index: number) => string | undefined;
  /** Kind → typical height px. Static table, tuned against real data. */
  kindHeights?: Readonly<Record<string, number>>;
  /** Final fallback when no prior and no kind height applies. */
  defaultSize: number;
}

/**
 * The live estimate resolver: per-row prior → structural size → kind
 * height → flat default.
 * Estimates only ever predict placement for unmeasured rows — they never
 * decide what a row renders, and a measurement always overrides them in
 * the size store.
 *
 * `rowPrior` and `kindOf` both resolve against the row currently at
 * `index` (live data), so there is nothing positional to remap across a
 * head splice — unlike the deleted snapshot+`shiftBase` design, this
 * resolver carries no index-keyed state at all.
 */
export function createRowEstimate(options: RowEstimateOptions): RowEstimate {
  return {
    at(index: number): number {
      const prior = options.rowPrior?.(index);
      if (prior !== undefined) return prior;
      const structural = options.structuralSize?.(index);
      if (structural !== undefined) return structural;
      const kind = options.kindOf?.(index);
      if (kind !== undefined) {
        const kindHeight = options.kindHeights?.[kind];
        if (kindHeight !== undefined) return kindHeight;
      }
      return options.defaultSize;
    },
  };
}
