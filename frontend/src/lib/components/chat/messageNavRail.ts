// Pure logic behind MessageNavRail.svelte — the left-edge column of tick
// pills, one per user message, that mirrors a chat thread's shape and
// jumps on click. Everything here is plain math over the projection's
// node array and the virtualizer's memoized geometry, so the component
// stays a thin renderer and the hot-path pieces are unit-testable.
//
// Perf contract (the renderer-hang rules, frontend-scroll.md):
// - `deriveNavTicks` runs once per STRUCTURAL pass (revealedNodes
//   identity), never per streaming delta.
// - The per-scroll-frame work is `tickRangeInView` + `markerFraction`:
//   a handful of binary searches over the tick array plus two engine
//   offset lookups. No O(items) walk rides the 60Hz scroll callback.

import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { isReaderAuthoredUserText, stripAttachmentImages } from '../../utils/userMessageMeta';

/**
 * One rail tick: a reader-authored user message. The rail covers the
 * WHOLE thread, so a tick's rows may not be loaded: `nodeIndex` is its
 * row index in `revealedNodes` when they are, null when they are not
 * (engine geometry exists only for loaded ticks).
 */
export interface NavTick {
  /** Item id — the jump target. */
  id: string;
  /** Position pair, the splice key between baseline and loaded ticks. */
  turnIndex: number;
  itemIndex: number;
  /** Row index in `revealedNodes`; null for an unloaded tick. */
  nodeIndex: number | null;
}

/** One store-baseline tick, the wire shape of GetThreadUserMessageTicks. */
export interface BaselineTick {
  id: string;
  turnIndex: number;
  itemIndex: number;
}

/**
 * The rail's merged tick list plus its loaded segment's bounds
 * (inclusive global indices; loadedStart -1 when nothing is loaded).
 * The segment is contiguous by construction — see `mergeNavTicks`.
 */
export interface MergedNavTicks {
  ticks: NavTick[];
  loadedStart: number;
  loadedEnd: number;
}

/**
 * Vertical budget per tick before the rail's height cap compresses the
 * spacing. Positions are fractional, so past the cap ticks pack closer
 * together instead of overflowing — the fisheye keeps a dense pack
 * targetable, and the first/last arrows cover the ends. Sized so the
 * position dot (4px) fits the gap between two 2px lines with clearance,
 * without the lines having to move.
 */
export const NAV_TICK_SPACING_PX = 12;

/** The rail renders nothing below this many ticks (a lone message needs no map). */
export const NAV_RAIL_MIN_TICKS = 2;

/**
 * User messages are always TOP-LEVEL leaves: the projection never wraps
 * them into activity runs (a run cannot straddle a user message) and the
 * group kinds anchor on launch/wait/read rows. What counts as a real
 * user message — top-level, not wire-only — is the shared
 * `isReaderAuthoredUserText` predicate.
 */
export function deriveNavTicks(nodes: readonly TimelineNode[]): NavTick[] {
  const ticks: NavTick[] = [];
  for (let i = 0; i < nodes.length; i++) {
    const node = nodes[i];
    if (node.kind !== 'leaf') continue;
    if (!isReaderAuthoredUserText(node.item)) continue;
    ticks.push({
      id: node.item.id,
      turnIndex: node.item.turnIndex,
      itemIndex: node.item.itemIndex,
      nodeIndex: i,
    });
  }
  return ticks;
}

/** (turn, item) tuple compare: negative when a sorts before b. */
function comparePosition(
  a: { turnIndex: number; itemIndex: number },
  b: { turnIndex: number; itemIndex: number },
): number {
  return a.turnIndex !== b.turnIndex ? a.turnIndex - b.turnIndex : a.itemIndex - b.itemIndex;
}

/**
 * Splice the loaded window's live-derived ticks over the store baseline:
 * inside the window's position span the LOADED list is the truth (it
 * tracks sends, reverts, and streaming reveals with no refetch), outside
 * it the baseline stands in for history the window has not loaded.
 * `hasMore*` false drops that side's baseline outright — the window
 * reaches the thread's end there, so anything the baseline still lists
 * beyond it (a reverted tail) is stale.
 *
 * The window bound positions come from the loaded ITEM array's ends, not
 * from the loaded ticks: a window whose edge rows are all assistant
 * activity still claims that span, so a baseline tick inside it (deleted
 * or not yet revealed) cannot double-render.
 */
export function mergeNavTicks(
  baseline: readonly BaselineTick[],
  loaded: readonly NavTick[],
  windowFirst: { turnIndex: number; itemIndex: number } | null,
  windowLast: { turnIndex: number; itemIndex: number } | null,
  hasMoreHistory: boolean,
  hasMoreNewer: boolean,
): MergedNavTicks {
  const ticks: NavTick[] = [];
  const asUnloaded = (t: BaselineTick): NavTick => ({ ...t, nodeIndex: null });
  if (hasMoreHistory && windowFirst) {
    for (const t of baseline) {
      if (comparePosition(t, windowFirst) >= 0) break;
      ticks.push(asUnloaded(t));
    }
  }
  const loadedStart = loaded.length > 0 ? ticks.length : -1;
  ticks.push(...loaded);
  const loadedEnd = loaded.length > 0 ? ticks.length - 1 : -1;
  if (hasMoreNewer && windowLast) {
    for (const t of baseline) {
      if (comparePosition(t, windowLast) <= 0) continue;
      ticks.push(asUnloaded(t));
    }
  }
  // An empty window (nothing loaded yet) with unloaded history on either
  // side still shows the whole baseline — the map exists before the rows.
  if (!windowFirst && loaded.length === 0 && (hasMoreHistory || hasMoreNewer)) {
    return { ticks: baseline.map(asUnloaded), loadedStart: -1, loadedEnd: -1 };
  }
  return { ticks, loadedStart, loadedEnd };
}

/**
 * The loaded window's position span, from the loaded item array's ends
 * (items stay sorted by (turn, item) — the store contract). Null when
 * nothing is loaded.
 */
export function itemWindowBounds(items: readonly Item[]): {
  first: { turnIndex: number; itemIndex: number };
  last: { turnIndex: number; itemIndex: number };
} | null {
  if (items.length === 0) return null;
  const f = items[0];
  const l = items[items.length - 1];
  return {
    first: { turnIndex: f.turnIndex, itemIndex: f.itemIndex },
    last: { turnIndex: l.turnIndex, itemIndex: l.itemIndex },
  };
}

/** Natural (uncompressed) rail height for `count` ticks. */
export function naturalRailHeightPx(count: number): number {
  return Math.max(0, count - 1) * NAV_TICK_SPACING_PX;
}

/** Rendered rail height: natural spacing until the available span caps it. */
export function railHeightPx(count: number, availablePx: number): number {
  return Math.min(naturalRailHeightPx(count), Math.max(0, availablePx));
}

/** True when the cap is compressing spacing — the overflow-arrows trigger. */
export function railCompressed(count: number, availablePx: number): boolean {
  return naturalRailHeightPx(count) > Math.max(0, availablePx);
}

/** Tick i's position along the rail, 0..1. A single tick centers. */
export function tickFraction(index: number, count: number): number {
  if (count <= 1) return 0.5;
  const clamped = Math.min(Math.max(index, 0), count - 1);
  return clamped / (count - 1);
}

/**
 * Pointer-Y → nearest tick index over the hit strip. Linear regardless
 * of compression, so a dense pack stays targetable: the strip maps its
 * whole height onto the index range and the fisheye shows what landed.
 */
export function tickIndexFromPointer(offsetY: number, heightPx: number, count: number): number {
  if (count <= 0) return -1;
  if (count === 1 || heightPx <= 0) return 0;
  const progress = Math.min(Math.max(offsetY / heightPx, 0), 1);
  return Math.round(progress * (count - 1));
}

/**
 * Fisheye horizontal scale by distance from the hovered tick. Transform
 * only — the tick's box never changes size, so hover costs no layout
 * (the perf lesson from t3-code's minimap, whose width-transition
 * version kept a style/layout/paint pipeline alive during scroll).
 * `null` distance means "no hover": every tick rests.
 */
export function tickDistanceScale(distance: number | null): number {
  switch (distance) {
    case 0:
      return 1;
    case 1:
      return 0.72;
    case 2:
      return 0.52;
    default:
      return 0.38;
  }
}

/**
 * The inclusive tick-index range whose messages intersect the visible
 * node range, or null when none do. `firstNodeInView`/`lastNodeInView`
 * come from the engine's `findItemIndex` at the viewport edges. Only
 * the loaded segment can be in view — unloaded ticks have no rows —
 * so the binary searches run over `[loadedStart, loadedEnd]`, whose
 * nodeIndexes are non-null and ascending by construction.
 */
export function tickRangeInView(
  merged: MergedNavTicks,
  firstNodeInView: number,
  lastNodeInView: number,
): [number, number] | null {
  const { ticks, loadedStart, loadedEnd } = merged;
  if (loadedStart < 0 || lastNodeInView < firstNodeInView) return null;
  const nodeAt = (i: number): number => ticks[i].nodeIndex ?? 0;
  // First loaded tick with nodeIndex >= firstNodeInView.
  let lo = loadedStart;
  let hi = loadedEnd + 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (nodeAt(mid) < firstNodeInView) lo = mid + 1;
    else hi = mid;
  }
  const first = lo;
  // Last loaded tick with nodeIndex <= lastNodeInView.
  lo = loadedStart - 1;
  hi = loadedEnd;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (nodeAt(mid) > lastNodeInView) hi = mid - 1;
    else lo = mid;
  }
  const last = lo;
  if (first > last) return null;
  return [first, last];
}

/**
 * Of the ticks whose messages are on screen (`range`, from
 * `tickRangeInView`), the one whose message row's center is closest to
 * the visible-band center — the rail's single "current" tick. Exactly
 * one lights at a time: several messages on screen is common, but the
 * reader is only AT one of them. Linear over the range on purpose — it
 * is bounded by how many user messages fit one viewport.
 */
export function tickNearestCenter(
  merged: MergedNavTicks,
  range: [number, number],
  centerOffset: number,
  offsetForNode: (nodeIndex: number) => number,
  sizeForNode: (nodeIndex: number) => number,
): number {
  const { ticks } = merged;
  let best = range[0];
  let bestDist = Infinity;
  for (let i = range[0]; i <= range[1]; i++) {
    const node = ticks[i].nodeIndex ?? 0;
    const rowCenter = offsetForNode(node) + sizeForNode(node) / 2;
    const dist = Math.abs(rowCenter - centerOffset);
    if (dist < bestDist) {
      bestDist = dist;
      best = i;
    }
  }
  return best;
}

/**
 * The position dot's rail fraction, or null when no dot should show.
 * DISCRETE by design: the dot sits centered in the GAP between the two
 * ticks the reader is between — after message k, before message k+1 —
 * and hops to the next gap when the viewport center reaches the next
 * message. At the last message (nothing to be "between") it hides; the
 * current-message fill already marks where the reader is then. Ditto at
 * the very top, above the first message.
 *
 * Geometry exists only for loaded ticks. A center above the first
 * loaded tick sits in the gap toward unloaded history (when there is
 * one); below the last loaded tick, in the gap toward the unloaded
 * tail.
 */
export function markerGapFraction(
  merged: MergedNavTicks,
  viewportCenterOffset: number,
  offsetForNode: (nodeIndex: number) => number,
): number | null {
  const { ticks, loadedStart, loadedEnd } = merged;
  const count = ticks.length;
  if (count < 2 || loadedStart < 0) return null;
  const nodeAt = (i: number): number => ticks[i].nodeIndex ?? 0;
  // Largest loaded k with offset(ticks[k]) <= center. Binary search
  // keeps the per-frame engine lookups at O(log n).
  let lo = loadedStart - 1;
  let hi = loadedEnd;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (offsetForNode(nodeAt(mid)) <= viewportCenterOffset) lo = mid;
    else hi = mid - 1;
  }
  // Above the first loaded tick: between unloaded history and it.
  const gapLo = lo < loadedStart ? loadedStart - 1 : lo;
  if (gapLo < 0) return null; // above the thread's first message
  if (gapLo >= count - 1) return null; // at the last message
  return (gapLo + 0.5) / (count - 1);
}

/** Hover preview of one turn: the user's ask plus how it resolved. */
export interface NavTickPreview {
  userText: string;
  /** Final assistant reply of the turn; '' when none is loaded. */
  assistantText: string;
}

/** Bound on preview text so a giant message can't bloat the hover card. */
const PREVIEW_MAX_CHARS = 400;

function compactPreview(text: string): string {
  // Attachment-image scrub (shared with UserMessage's rendering), then
  // whitespace collapse — the card renders a phrase, not markdown.
  const compact = stripAttachmentImages(text).replace(/\s+/g, ' ').trim();
  return compact.length > PREVIEW_MAX_CHARS ? `${compact.slice(0, PREVIEW_MAX_CHARS)}…` : compact;
}

/**
 * Resolve the hover card's content from the loaded item window. The
 * assistant half is the LAST top-level assistant_text before the next
 * user message — how the turn ended, matching what the reader would find
 * by jumping there. Runs on hover only; the walk is bounded by the
 * window and stops at the next user message.
 */
export function turnPreview(items: readonly Item[], userItemId: string): NavTickPreview {
  const start = items.findIndex((it) => it.id === userItemId);
  if (start < 0) return { userText: '', assistantText: '' };
  const userText = compactPreview(items[start].summary);
  let assistantText = '';
  for (let i = start + 1; i < items.length; i++) {
    const it = items[i];
    if ((it.parentId ?? '') !== '') continue;
    if (it.kind === 'user_text') {
      // A wire-only injection mid-turn is context, not the next ask —
      // the walk must agree with `deriveNavTicks` about what a turn is.
      if (isReaderAuthoredUserText(it)) break;
      continue;
    }
    if (it.kind === 'assistant_text' && it.summary) assistantText = it.summary;
  }
  return { userText, assistantText: compactPreview(assistantText) };
}

/**
 * Preview-card vertical alignment: the first tick's card hangs down, the
 * last tick's hangs up, everything else centers — edge cards flip
 * instead of clipping at the pane's top/bottom.
 */
export function previewTranslateYPercent(index: number, count: number): number {
  if (count <= 1) return -50;
  if (index <= 0) return 0;
  if (index >= count - 1) return -100;
  return -50;
}
