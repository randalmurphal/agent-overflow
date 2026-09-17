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
// VALIDITY: a thread's entry is a set of per-GEOMETRY BUCKETS, not one
// measurement set. A bucket's geometry is the two inputs that rescale
// every row at constant content: the scroll-surface content WIDTH (the
// wrap point, so a narrower/wider pane changes every multi-line row's
// height) and the TYPOGRAPHY signature (the display settings that change
// a row's height at a fixed wrap point — see
// `stores/settings.svelte.ts#typographySignature`). The same thread read
// full-pane, in a split, with the sidebar toggled, and at two font sizes
// replays at each of those geometries instead of refusing whichever one
// it was not last captured at. A geometry with no bucket is a miss, and
// so is a bucket whose remaining dimension disagrees. Either one refuses
// every row it would have supplied, degrading that mount to the kind/flat
// estimate chain:
//
//   - expansionSig : non-default row-UI expansion state
//                    (`pane.expansionSignature()`), per bucket.
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
// `setThreadSizePriors` REPLACES the captured geometry's BUCKET wholesale
// on every capture rather than merging row-by-row; the thread's other
// geometry buckets are left alone. The wholesale part is deliberate:
// streaming rows carry `updatedAt`/`summary.length` in their signature,
// so a row's key changes on every append — merging would accumulate an
// ever-growing tail of dead signatures from rows that no longer exist in
// that exact form. A wholesale replace self-cleans that churn for free.
// The consumer (timelineSizePriors.svelte.ts `maybePersistSizePriors`)
// builds each replacement by carrying forward that same geometry bucket's
// sizes for signatures still live in the current window — so an early
// capture with few (or no) measured rows cannot destroy a settled one —
// but the store contract here stays a plain per-bucket replace.
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
// The display settings that rescale rows are ONE dimension of the
// geometry key, never a subset: `typographySignature` in
// `stores/settings.svelte.ts` is the single place that names them, so a
// new height-affecting setting is added there and both the capture and
// the replay pick it up without a second list to keep in step.

import type { RowEstimate } from './types';

/** One geometry's measurements for a thread. */
export interface SizePriorsBucket {
  /** `pane.expansionSignature()` at capture. */
  expansionSig: string;
  /**
   * `nodeSignature(node, currentItem)` → last measured px, for every row
   * measured at capture.
   */
  rows: Map<string, number>;
}

/**
 * The geometry a bucket's rows were measured under: the scroll-surface
 * content width (the wrap point) and the typography signature (the
 * display settings that change a row's height at a fixed wrap point).
 * Both are inputs to every multi-line row's height, so they select the
 * bucket together — `sizePriorsGeometryKey` is the only way to build the
 * key they form.
 */
export interface SizePriorsGeometry {
  /** Scroll-surface CONTENT width in px; rounded into the key. */
  width: number;
  /** `typographySignature()` at capture. */
  typography: string;
}

export interface SizePriorsEntry {
  /**
   * `sizePriorsGeometryKey(geometry)` → that geometry's measurements, in
   * LRU order with the most recently captured geometry LAST. Bounded by
   * MAX_BUCKETS_PER_THREAD.
   */
  byGeometry: Map<string, SizePriorsBucket>;
}

/**
 * Separator between the two key segments. The width segment is digits
 * only, so the first separator always ends it and the typography segment
 * may contain anything non-empty, this character included.
 * `GEOMETRY_KEY_PATTERN` below spells the same separator; change both.
 */
const GEOMETRY_KEY_SEPARATOR = '|';

/**
 * `<rounded width>|<typography signature>`. Rounding lives here, not at
 * the call sites, so a capture and the replay that follows it cannot
 * disagree about which bucket a fractional width names.
 */
export function sizePriorsGeometryKey(geometry: SizePriorsGeometry): string {
  return `${Math.round(geometry.width)}${GEOMETRY_KEY_SEPARATOR}${geometry.typography}`;
}

/**
 * Whether a string from an untrusted source (storage, a hand-edited
 * profile) is a key this module could have written: a non-negative
 * integer width, the separator, then a non-empty typography segment. A
 * key that fails this is not merely a permanent miss occupying the
 * per-thread cap — it means the stored value was corrupted, so the
 * loader drops the whole entry.
 */
const GEOMETRY_KEY_PATTERN = /^(?:0|[1-9][0-9]*)\|[^]+$/;

export function isSizePriorsGeometryKey(value: unknown): value is string {
  return typeof value === 'string' && GEOMETRY_KEY_PATTERN.test(value);
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

// Each entry holds ~one float per loaded row plus its signature string,
// times its geometry buckets (MAX_BUCKETS_PER_THREAD below); all of that
// scales with the live window and is bounded here. 50 recently
// visited threads is a generous working set; older threads fall back to
// the adapter (if the thread is still stored) or kind estimates, which is
// correct, just not pixel-exact until the next capture.
const MAX_ENTRIES = 50;

/**
 * Geometry buckets kept per thread. The geometries a reader actually
 * cycles through are the full pane, a split, and a sidebar-toggled
 * variant, so three covers the recurring set; a drag-resize or a run of
 * font-size steps instead produces one-off geometries, and the LRU order
 * keeps the latest of that run rather than the stalest. Storage cost per
 * thread is therefore bounded at this many row maps.
 */
export const MAX_BUCKETS_PER_THREAD = 3;

/**
 * Rows one bucket may hold. Equal to
 * `ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS` in
 * `stores/threadPaneShared.ts`, the pane's retention ceiling and so the
 * most rows a capture can ever measure. The value is duplicated rather
 * than imported because `utils/` must not depend on `stores/`; the priors
 * test asserts the two stay equal.
 */
export const MAX_ROWS_PER_BUCKET = 2400;

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

function evictBucketsOverCap(byGeometry: Map<string, SizePriorsBucket>): void {
  while (byGeometry.size > MAX_BUCKETS_PER_THREAD) {
    const oldest = byGeometry.keys().next().value;
    if (oldest === undefined) break;
    byGeometry.delete(oldest);
  }
}

/**
 * Installs `bucket` as this thread's measurements at `geometry`,
 * replacing whatever that geometry held and leaving its other geometries
 * alone. The whole thread entry (every bucket) is what reaches the
 * adapter, so the adapter contract stays one persisted value per thread.
 *
 * The thread's existing buckets come from the in-memory LRU alone. A
 * memory miss means no stored buckets either: `getThreadSizePriors`
 * installs a storage hit into the LRU, and the capture path reads
 * through it before every set, so a memory-evicted thread is rehydrated
 * before it is written back rather than truncated to this one geometry.
 */
export function setThreadSizePriors(
  threadId: string,
  geometry: SizePriorsGeometry,
  bucket: SizePriorsBucket,
): void {
  const entry = entries.get(threadId) ?? { byGeometry: new Map() };
  const key = sizePriorsGeometryKey(geometry);
  // Re-insert to bump the bucket to LRU-last.
  entry.byGeometry.delete(key);
  entry.byGeometry.set(key, bucket);
  evictBucketsOverCap(entry.byGeometry);
  // Re-insert to bump LRU recency.
  entries.delete(threadId);
  entries.set(threadId, entry);
  evictOverCap();
  storageAdapter?.persist(threadId, entry);
}

/** This thread's measurements at `geometry`, if it has any. */
export function sizePriorsAtGeometry(
  entry: SizePriorsEntry,
  geometry: SizePriorsGeometry,
): SizePriorsBucket | undefined {
  return entry.byGeometry.get(sizePriorsGeometryKey(geometry));
}

/**
 * The most recently captured bucket, the best guess when the surface has
 * not reported a width yet (see `buildRowEstimate` in
 * components/chat/timelineSizePriors.svelte.ts).
 */
export function latestSizePriors(entry: SizePriorsEntry): SizePriorsBucket | undefined {
  let latest: SizePriorsBucket | undefined;
  for (const bucket of entry.byGeometry.values()) latest = bucket;
  return latest;
}

/**
 * The stored entry for a thread — a memory hit bumps LRU recency; a
 * memory miss falls through to the storage adapter (if installed) and,
 * on a storage hit, installs the result into the in-memory LRU before
 * returning it. Validity checking (bucket selection by geometry,
 * expansionSig, per-row signature lookup) is the consumer's job — see
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
export function sizePriorsStats(): { threads: number; buckets: number; rows: number } {
  let buckets = 0;
  let rows = 0;
  for (const entry of entries.values()) {
    buckets += entry.byGeometry.size;
    for (const bucket of entry.byGeometry.values()) rows += bucket.rows.size;
  }
  return { threads: entries.size, buckets, rows };
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
