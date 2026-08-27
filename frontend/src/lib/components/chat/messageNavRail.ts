// Pure logic behind MessageNavRail.svelte — the left-edge column of tick
// pills, one per user message, that mirrors a chat thread's shape and
// jumps on click. Everything here is plain math over the projection's
// node array and the virtualizer's memoized geometry, so the component
// stays a thin renderer and the hot-path pieces are unit-testable.
//
// Perf contract (the renderer-hang rules, frontend-scroll.md):
// - `deriveNavTicks` runs once per STRUCTURAL pass (revealedNodes
//   identity), never per streaming delta.
// - The per-scroll-frame work is `tickRangeInView` + `railGapLow` +
//   `railClipOffsetPx`: a handful of binary searches over the tick
//   array plus a few engine offset lookups. No O(items) walk rides the
//   60Hz scroll callback.

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
 * Vertical budget per tick — a FLOOR, never compressed. When the strip
 * of ticks outgrows the rail column, the rail clips it to a window that
 * slides with the reader's position (`railClipOffsetPx`) instead of
 * packing ticks closer; the first/last arrows appear only while their
 * end tick is clipped out. Sized so the position dot fits the gap
 * between two 2px lines with clearance, without the lines having to
 * move. 8px is the deliberate resting density (user-tuned 2026-08-19):
 * tighter than the original 12 without reaching the packed look the
 * old compression produced.
 */
export const NAV_TICK_SPACING_PX = 8;

/** The rail renders nothing below this many ticks (a lone message needs no map). */
export const NAV_RAIL_MIN_TICKS = 2;

/**
 * Horizontal interaction geometry. The pointer-only strip must end before
 * selectable transcript text begins. Keep the expanded tick inside that
 * strip, then reserve a real dead gutter before the row column.
 *
 * The row shell grows from its historical 61rem asymmetric box to the old
 * 62rem box with 40px/32px padding. That preserves its 920px wide-pane
 * content width and exact wide-pane content bounds, while narrow panes spend
 * only 8px more on each side.
 */
export const NAV_RAIL_HIT_WIDTH_PX = 32;
export const NAV_RAIL_TICK_LEFT_PX = 8;
export const NAV_RAIL_TEXT_GAP_PX = 8;
export const NAV_RAIL_ROW_LEFT_PADDING_PX = NAV_RAIL_HIT_WIDTH_PX + NAV_RAIL_TEXT_GAP_PX;
export const NAV_RAIL_ROW_RIGHT_PADDING_PX = 32;
export const NAV_RAIL_ROW_CONTENT_MAX_WIDTH_PX = 920;
export const NAV_RAIL_ROW_MAX_WIDTH_PX =
  NAV_RAIL_ROW_LEFT_PADDING_PX
  + NAV_RAIL_ROW_CONTENT_MAX_WIDTH_PX
  + NAV_RAIL_ROW_RIGHT_PADDING_PX;

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

/** The tick strip's full height for `count` ticks at natural spacing. */
export function naturalRailHeightPx(count: number): number {
  return Math.max(0, count - 1) * NAV_TICK_SPACING_PX;
}

/** Rendered rail-viewport height: the natural strip until the column caps it. */
export function railHeightPx(count: number, availablePx: number): number {
  return Math.min(naturalRailHeightPx(count), Math.max(0, availablePx));
}

/**
 * True when the tick strip outgrows the rail column. Spacing never
 * compresses: an overflowing strip is CLIPPED to a sliding window
 * instead, and the first/last jump arrows exist only in this state.
 */
export function railOverflows(count: number, availablePx: number): boolean {
  return naturalRailHeightPx(count) > Math.max(0, availablePx);
}

/**
 * How far the strip can slide: strip height minus the rail viewport.
 * 0 while the column has no measured height (pre-ResizeObserver) — a
 * window with no extent has nothing to slide within.
 */
export function railMaxClipPx(count: number, availablePx: number): number {
  if (availablePx <= 0) return 0;
  return Math.max(0, naturalRailHeightPx(count) - railHeightPx(count, availablePx));
}

/**
 * The clipped strip's slide offset for a reader at `positionFraction`
 * (0 = first message, 1 = latest) — the reader's point on the strip is
 * kept at the window's center, clamped so the window never leaves the
 * strip. 0 for a strip that fits: the whole map is always visible then.
 */
export function railClipOffsetPx(
  positionFraction: number,
  count: number,
  availablePx: number,
): number {
  const maxClip = railMaxClipPx(count, availablePx);
  if (maxClip <= 0) return 0;
  const frac = Math.min(Math.max(positionFraction, 0), 1);
  const target = frac * naturalRailHeightPx(count) - railHeightPx(count, availablePx) / 2;
  return Math.min(Math.max(target, 0), maxClip);
}

/** Tick i's position along the rail, 0..1. A single tick centers. */
export function tickFraction(index: number, count: number): number {
  if (count <= 1) return 0.5;
  const clamped = Math.min(Math.max(index, 0), count - 1);
  return clamped / (count - 1);
}

/**
 * STRIP-y → nearest tick index. `offsetY` is a strip coordinate — the
 * caller converts window y by adding the current clip offset — and
 * `heightPx` is the natural strip height, so only ticks near the
 * window are reachable when the strip is clipped.
 */
export function tickIndexFromPointer(offsetY: number, heightPx: number, count: number): number {
  if (count <= 0) return -1;
  if (count === 1 || heightPx <= 0) return 0;
  const progress = Math.min(Math.max(offsetY / heightPx, 0), 1);
  return Math.round(progress * (count - 1));
}

/**
 * Fisheye horizontal scale by distance from the hovered tick, as a
 * fraction of the full (hovered) width. Transform only — the tick's box
 * never changes size, so hover costs no layout (the perf lesson from
 * t3-code's minimap, whose width-transition version kept a
 * style/layout/paint pipeline alive during scroll).
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

/** A hovered tick's width; every other scale is a fraction of this. */
export const TICK_FULL_WIDTH_PX = 24;

/**
 * A tick's INTRINSIC width is its resting width, and the fisheye scales
 * UP from it, so a tick at rest carries no transform at all
 * (`tickTransform(null) === ''`). The inverse form — full width, rest
 * expressed as `scaleX(0.38)` — made every mounted tick a non-2D
 * transform paint-property node, and Chromium re-allocates a
 * `GeometryMapperTransformCache::PlaneRootTransform` for each such
 * node whenever any transform or scroll offset changes anywhere in the
 * page: ~540 ticks × 288B per scroll frame, 25.8MB of Oilpan garbage
 * per 14s of scrolling (measured 2026-08-23), committed heap pages that
 * the renderer never gave back. Only the ticks inside the fisheye hold
 * a transform, and only while the pointer is on the rail.
 */
export const TICK_REST_WIDTH_PX = Math.round(TICK_FULL_WIDTH_PX * tickDistanceScale(null) * 100) / 100;

const FISHEYE_TRANSFORMS: readonly string[] = [0, 1, 2].map(
  (distance) => `scaleX(${(tickDistanceScale(distance) / tickDistanceScale(null)).toFixed(4)})`,
);

/**
 * The inline transform for a tick at `distance` from the hovered tick:
 * a scale relative to the RESTING width, empty at rest. Precomputed so
 * a fisheye sweep re-renders ticks without allocating.
 */
export function tickTransform(distance: number | null): string {
  if (distance === null || distance >= FISHEYE_TRANSFORMS.length) return '';
  return FISHEYE_TRANSFORMS[distance];
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
 * The gap the viewport center sits in: the largest LOADED tick index k
 * whose message starts at or above the center (`loadedStart - 1` when
 * the center is above the first loaded tick — which is -1 only when
 * nothing precedes it). Null when fewer than two ticks exist or no
 * loaded segment does. Geometry exists only for loaded ticks, so with
 * an unloaded tail the answer tops out at `loadedEnd`, the gap toward
 * that unloaded tail.
 *
 * The one gap search behind both position consumers in the sync
 * module: the gap DOT — DISCRETE by design, centered at
 * `(k + 0.5) / (count - 1)` between the two ticks the reader is
 * between, hopping per message, hidden past either end where the
 * current-message fill takes over — and the clipped strip's slide
 * fraction. Sharing the search is what keeps the two claims from
 * disagreeing about where the reader is.
 */
export function railGapLow(
  merged: MergedNavTicks,
  viewportCenterOffset: number,
  offsetForNode: (nodeIndex: number) => number,
): number | null {
  const { ticks, loadedStart, loadedEnd } = merged;
  if (ticks.length < 2 || loadedStart < 0) return null;
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
  return lo < loadedStart ? loadedStart - 1 : lo;
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
 *
 * `items` answers STRUCTURE (which rows, in what order, of what kind);
 * the two summaries are read through `resolve` — `pane.getItemById`,
 * the row's own box — because a streaming row's summary is written in
 * place and the array signal does not fire for it. A reactive caller
 * that read `.summary` off the array would show the turn as it stood at
 * the last structural change.
 */
export function turnPreview(
  items: readonly Item[],
  userItemId: string,
  resolve: (itemId: string) => Item | undefined,
): NavTickPreview {
  const start = items.findIndex((it) => it.id === userItemId);
  if (start < 0) return { userText: '', assistantText: '' };
  const userText = compactPreview((resolve(userItemId) ?? items[start]).summary);
  const replies: Item[] = [];
  for (let i = start + 1; i < items.length; i++) {
    const it = items[i];
    if ((it.parentId ?? '') !== '') continue;
    if (it.kind === 'user_text') {
      if (isReaderAuthoredUserText(it)) break;
      continue;
    }
    if (it.kind === 'assistant_text') replies.push(it);
  }
  // The newest reply with text wins, read through the resolver: the array
  // copy a reactive scope holds can trail a streamed delta, and a reply row
  // that has landed without a word yet must not blank the one before it.
  for (let i = replies.length - 1; i >= 0; i--) {
    const assistantText = compactPreview((resolve(replies[i].id) ?? replies[i]).summary);
    if (assistantText !== '') return { userText, assistantText };
  }
  return { userText, assistantText: '' };
}

/**
 * Preview-card vertical alignment from the tick's position within the
 * rail VIEWPORT (0 = window top, 1 = window bottom): a card near the
 * top hangs down, near the bottom hangs up, everything else centers —
 * edge cards flip instead of clipping at the pane's top/bottom.
 * Viewport-relative rather than index-relative because a clipped strip
 * puts mid-list ticks at the window edges.
 */
export function previewTranslateYPercent(viewportFraction: number): number {
  if (viewportFraction <= 0.1) return 0;
  if (viewportFraction >= 0.9) return -100;
  return -50;
}
