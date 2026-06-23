// Per-thread virtua measured-size cache. Persists virtua's row-size
// measurements across thread switches inside a single session so a revisited
// thread can mount its <Virtualizer> at the already-measured total height
// instead of re-running the estimate→measure cascade (mount every row at the
// flat `itemSize` estimate, then grow `scrollHeight` over a burst of
// ResizeObserver corrections — the "lands right, flickers, lands right again"
// thread-switch flicker; see docs/architecture/frontend-scroll.md
// "estimate→measure cascade").
//
// WHY THIS IS SAFE TO REPLAY (it was previously prohibited): a restored
// snapshot is only sound when the rows re-render at the SAME heights they
// were measured at. virtua itself no-ops a re-measure that matches the
// restored size — its resize handler filters out every item whose new size
// equals the cached size and bumps no state version (verified against the
// installed virtua 0.49.1 core: `lib/core/index.js` action-3 handler,
// `s.filter(([i, sz]) => J.t[i] !== sz); if (!filtered.length) break`). So a
// matched replay produces zero re-render and zero scroll jump. The whole job
// of this module is to refuse the replay whenever the heights would NOT match.
//
// Row height is keyed on three inputs that change it sharply, so each entry
// stamps all three and `getReplayableVirtuaCache` refuses the snapshot on any
// mismatch. (A fourth class — display settings — also shifts height but is a
// deliberately-unkeyed, benign residual documented at the end of this block.)
// On refusal virtua falls back to the flat `itemSize` estimate — i.e. the
// pre-cache behavior, never worse:
//
//   - width    : the scroll-pane content width is the wrap point, so it
//                changes every multi-line row's height.
//   - structureSig : a signature over the rendered node sequence + per-leaf
//                content (`utils/timelineStructureSignature.ts`). It is
//                REPRODUCIBLE — revisiting a settled thread yields the same
//                string, so the sizes replay — and content-aware, so a
//                background change (streaming append, tool fill) yields a
//                different string and the stale sizes are refused. It
//                superseded an earlier version of this key that read
//                `pane.timelineRevision`, a monotonic counter that is never
//                restored on a cache-hit re-entry: a revisit always computed a
//                strictly-greater revision than capture, so the replay never
//                matched and the whole cache was inert. (`timelineRevision`
//                itself is unchanged and still drives timeline-derivation
//                reactivity — it was simply the wrong input to key replay on.)
//   - expansionSig : the thread's non-default row-UI expansion state
//                (expanded diffs, subagent groups, payloads). Row-UI state is
//                reset to default on every thread switch
//                (`rowUiState.clear()` in thread.svelte.ts
//                `installCacheOrFreshState`), so at restore time the signature
//                is the default one — which means a snapshot captured while
//                something was expanded (taller rows) is correctly refused,
//                and the common all-collapsed idle thread replays cleanly.
//
// KNOWN RESIDUAL — display settings are NOT keyed. Four global settings change a
// timeline row's height at constant width/structure/expansion: `fontSize` (a
// root font-scale on <html>, so it rescales every row), `sansFont` and
// `monoFont` (typeface metrics shift line heights at a fixed width), and
// `collapseDiffPreviews` (the default expand/collapse of an un-overridden inline
// diff card — DiffFileBlock). Toggling one mid-session and then revisiting a
// thread can replay sizes measured under the old setting. This is deliberately
// tolerated rather than keyed, because it is benign and self-correcting: a stale
// replay feeds virtua wrong start heights, the per-row ResizeObserver cascade
// corrects them, and MessageTimeline's warm-up visibility gate hides that
// cascade exactly as it hides a cold first visit — so the worst case degrades to
// first-visit behavior, never a crash or a stuck viewport. The benefit of keying
// them is therefore invisible (same masked cascade either way), while a partial
// display-settings key would silently regress the day a new height-affecting
// setting is added and not threaded through it. If it must become airtight, add
// ONE display-settings dimension here AND in MessageTimeline's
// currentVirtuaSizeKey — covering all four — not a subset.

import type { VirtualizerProps } from 'virtua/svelte';

// virtua's opaque measured-size snapshot (the return type of
// `VirtualizerHandle.getCache()` and the input to the `cache` prop). Derived
// from the public Svelte prop type so we don't import virtua's
// `unstable_core` path into app code.
export type VirtuaCacheSnapshot = NonNullable<VirtualizerProps<unknown>['cache']>;

// The validity stamp captured alongside a snapshot, compared at replay.
export interface VirtuaSizeCacheKey {
  width: number;
  structureSig: string;
  expansionSig: string;
}

export interface VirtuaSizeCacheEntry extends VirtuaSizeCacheKey {
  snapshot: VirtuaCacheSnapshot;
}

// Each entry holds a measured-size array (~one float per loaded item, capped
// by the live window — typically a few hundred, at most ~1000) plus its
// validity stamp. Now that the key holds a structureSig string rather than the
// old revision number, that string is the dominant per-entry allocation, not
// the snapshot: roughly
// ~30 chars/node × N nodes (tens of KB at the upper end) vs the snapshot's ~N
// floats. Both scale with the live window and are bounded by MAX_ENTRIES below.
// 50 recently visited threads is a generous working set; older threads fall
// back to the estimate, which is correct, just not instant. Bounds session
// growth the same way threadScrollSnapshots does.
const MAX_ENTRIES = 50;

const entries = new Map<string, VirtuaSizeCacheEntry>();

// Compare EVERY field of VirtuaSizeCacheKey. TypeScript forces the key
// builder (MessageTimeline's currentVirtuaSizeKey) to set a newly-added field
// but will NOT flag a field missed here, so a newly-keyed height input must be
// added in both places. (Not every height input is keyed — see the display-
// settings residual in the header block. The three that ARE keyed must each
// match exactly, or a snapshot replays at the wrong height and reintroduces the
// cascade this cache exists to kill.)
function keysMatch(a: VirtuaSizeCacheKey, b: VirtuaSizeCacheKey): boolean {
  return a.width === b.width && a.structureSig === b.structureSig && a.expansionSig === b.expansionSig;
}

export function setThreadVirtuaSizeCache(threadId: string, entry: VirtuaSizeCacheEntry): void {
  // Re-insert to bump LRU recency.
  if (entries.has(threadId)) {
    entries.delete(threadId);
  }
  entries.set(threadId, entry);
  while (entries.size > MAX_ENTRIES) {
    const oldest = entries.keys().next().value;
    if (oldest === undefined) break;
    entries.delete(oldest);
  }
}

// Returns the stored snapshot ONLY when its captured key matches the current
// mount's key — otherwise undefined, so the caller mounts on the flat
// estimate. A successful match bumps LRU recency.
export function getReplayableVirtuaCache(
  threadId: string,
  key: VirtuaSizeCacheKey,
): VirtuaCacheSnapshot | undefined {
  const entry = entries.get(threadId);
  if (!entry || !keysMatch(entry, key)) return undefined;
  entries.delete(threadId);
  entries.set(threadId, entry);
  return entry.snapshot;
}

export function clearThreadVirtuaSizeCache(threadId: string): void {
  entries.delete(threadId);
}

export function clearThreadVirtuaSizeCacheForTest(): void {
  entries.clear();
}

// Test-only: read the raw stored entry (snapshot + captured key) without the
// validity gate, so a component test can assert what was persisted without
// depending on happy-dom's (zero) geometry for the width dimension.
export function peekThreadVirtuaSizeCacheForTest(
  threadId: string,
): VirtuaSizeCacheEntry | undefined {
  return entries.get(threadId);
}
