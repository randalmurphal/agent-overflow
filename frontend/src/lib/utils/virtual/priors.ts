// Per-thread row-height priors: persistence of measured sizes across
// thread switches, and the live per-row estimate resolver the engine uses
// for unmeasured rows.
//
// Persistence carries the proven design of utils/threadVirtuaSizeCache.ts
// (which phase V3 deletes): an LRU of measured-size snapshots keyed by
// {width, structureSig, expansionSig}, where a mismatch on ANY dimension
// refuses the snapshot. The key is why priors can never mis-place rows
// from stale data:
//
//   - width        : the wrap point — a narrower/wider pane changes every
//                    multi-line row's height, so it is a key miss.
//   - structureSig : node sequence + per-leaf content
//                    (utils/timelineStructureSignature.ts) — a tool call
//                    that errored, streamed text that grew, anything that
//                    changes what a row renders is a key miss.
//   - expansionSig : non-default row-UI expansion state.
//
// What DID change from threadVirtuaSizeCache: consumption. Instead of the
// constructor-once `cache` prop replay (all-or-nothing, resolved in an
// $effect.pre before a keyed remount), the engine reads priors lazily per
// row through RowEstimate whenever a row is unmeasured. A key miss
// degrades per-row to the kind estimate or flat default — and measured
// sizes always win over any prior (plan §2 Priors, §8 D2).
//
// The known residual is inherited unchanged: display settings (fontSize,
// sansFont, monoFont, collapseDiffPreviews) are deliberately NOT keyed —
// a stale replay degrades to the measure-and-correct path that the warm
// gate already masks, exactly like a cold first visit. If keying them
// ever becomes necessary, add ONE display-settings dimension covering all
// four, not a subset (see the threadVirtuaSizeCache.ts header for the
// full rationale until V3 folds it in here).

import { UNMEASURED } from './sizes';
import type { RowEstimate } from './types';

export interface SizePriorsKey {
  width: number;
  structureSig: string;
  expansionSig: string;
}

export interface SizePriorsEntry extends SizePriorsKey {
  /** Measured px per row index; UNMEASURED where the row never measured. */
  sizes: number[];
}

// Each entry holds ~one float per loaded row plus the signature strings;
// both scale with the live window and are bounded here. 50 recently
// visited threads is a generous working set; older threads fall back to
// kind estimates, which is correct, just not pixel-exact.
const MAX_ENTRIES = 50;

const entries = new Map<string, SizePriorsEntry>();

// Compare EVERY field of SizePriorsKey. TypeScript forces the key builder
// (MessageTimeline's key derivation) to set a newly-added field but will
// NOT flag a field missed here — a newly-keyed height input must be added
// in both places.
function keysMatch(a: SizePriorsKey, b: SizePriorsKey): boolean {
  return (
    a.width === b.width &&
    a.structureSig === b.structureSig &&
    a.expansionSig === b.expansionSig
  );
}

export function setThreadSizePriors(threadId: string, entry: SizePriorsEntry): void {
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

/**
 * The stored sizes ONLY when the captured key matches the current mount's
 * key — otherwise undefined and every row falls back to its kind estimate.
 * A successful match bumps LRU recency.
 */
export function getReplayableSizePriors(
  threadId: string,
  key: SizePriorsKey,
): number[] | undefined {
  const entry = entries.get(threadId);
  if (!entry || !keysMatch(entry, key)) return undefined;
  entries.delete(threadId);
  entries.set(threadId, entry);
  return entry.sizes;
}

export function clearThreadSizePriors(threadId: string): void {
  entries.delete(threadId);
}

export function clearAllThreadSizePriorsForTest(): void {
  entries.clear();
}

/** Test-only: raw stored entry without the validity gate. */
export function peekThreadSizePriorsForTest(threadId: string): SizePriorsEntry | undefined {
  return entries.get(threadId);
}

export interface RowEstimateOptions {
  /** Replayable priors for this mount (already key-validated), if any. */
  snapshot?: readonly number[];
  /** Maps a row index to its kind for the kind-height table. */
  kindOf?: (index: number) => string | undefined;
  /** Kind → typical height px. Static table, tuned against real data. */
  kindHeights?: Readonly<Record<string, number>>;
  /** Final fallback when no prior and no kind height applies. */
  defaultSize: number;
}

/**
 * The live estimate resolver: prior snapshot value → kind height → flat
 * default. Estimates only ever predict placement for unmeasured rows —
 * they never decide what a row renders, and a measurement always
 * overrides them in the size store.
 *
 * The snapshot is positional (captured at mount), so head splices remap
 * it through `shiftBase`; `kindOf` resolves against live data and needs
 * no remap.
 */
export function createRowEstimate(options: RowEstimateOptions): RowEstimate {
  let bias = 0;
  // Rows prepended after mount are new identities: bias math alone would
  // alias them onto removed rows' priors (remove k then prepend k lands
  // bias back at 0), so the fresh head-run is excluded from the snapshot.
  let freshHeadCount = 0;
  return {
    at(index: number): number {
      const snapshot = options.snapshot;
      if (snapshot && index >= freshHeadCount) {
        const snapshotIndex = index - bias;
        if (snapshotIndex >= 0 && snapshotIndex < snapshot.length) {
          const size = snapshot[snapshotIndex];
          if (size !== UNMEASURED && size >= 0) return size;
        }
      }
      const kind = options.kindOf?.(index);
      if (kind !== undefined) {
        const kindHeight = options.kindHeights?.[kind];
        if (kindHeight !== undefined) return kindHeight;
      }
      return options.defaultSize;
    },
    shiftBase(count: number): void {
      bias += count;
      // A removal consumes fresh rows first (they sit at the head).
      freshHeadCount = Math.max(0, freshHeadCount + count);
    },
  };
}
