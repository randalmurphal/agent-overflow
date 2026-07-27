// Geometry for an activity run's height-capped clip.
//
// The cap bounds how much vertical space a stretch of ACTIVITY takes. An
// expanded payload is not activity — it is content the user explicitly
// asked to read — so the cap grows by exactly what expansion added, and
// reading a diff inside a run never means scroll-within-scroll.
//
// Expanded bodies are found through the disclosure contract rather than a
// marker attribute rows have to remember: an expandable row header is a
// `TranscriptDisclosureHeader`, which emits `aria-expanded` and points at
// its body with `aria-controls`. A body that skipped the query would be an
// accessibility defect first, so the coupling pushes in the right
// direction — a new expandable body cannot silently opt out of the cap
// while staying correct for a screen reader.

import type { ScrollMetrics } from './scroll/overlayScrollbar';

/**
 * Activity rows the cap admits before a run starts scrolling in place.
 *
 * The number the cap is FOR — the cap is a height only because CSS has no
 * "eight rows" unit and activity rows are not one height. Change this to
 * change how much activity a run shows.
 */
export const ACTIVITY_RUN_CAP_ROWS = 8;

/**
 * A typical activity row, in rem.
 *
 * Not the tightest row: `ROW_KIND_ESTIMATE_PX` prices a bare one-line
 * `tool_call` near 25px because it is a placement FLOOR, and sizing the cap
 * off a floor would show a third fewer rows than it promises. Real runs mix
 * those with thinking rows, rows carrying a preview line, and the margins
 * between them, which measures closer to 36px at default settings.
 *
 * In rem so the cap tracks the font-size setting — a reader on large text
 * gets eight rows too, not eight rows' worth of last year's pixels.
 */
export const ACTIVITY_RUN_ROW_REM = 2.25;

export const ACTIVITY_RUN_CAP_REM = ACTIVITY_RUN_CAP_ROWS * ACTIVITY_RUN_ROW_REM;

/**
 * Base cap, before any expansion.
 *
 * The `50vh` half is the short-viewport guard, and only it: at the row cap
 * above it wins below a ~576px window, where eight rows would be most of the
 * conversation. Whichever half wins, the run is a window onto activity rather
 * than the whole screen.
 */
export const ACTIVITY_RUN_CAP_CSS = `min(50vh, ${ACTIVITY_RUN_CAP_REM}rem)`;

/**
 * The rem half of the cap in px, assuming a 16px root.
 *
 * For placement ESTIMATES only (`timelineSizePriors.svelte.ts`), which is why
 * the assumption is tolerable: a larger root font makes this an
 * underestimate, and an under-estimated row only grows when it measures,
 * which the engine's remeasure-above compensation absorbs invisibly. It is
 * here rather than there so the number cannot drift from the cap it describes.
 */
export const ACTIVITY_RUN_CAP_REM_PX = ACTIVITY_RUN_CAP_REM * 16;

export function activityRunClipMaxHeight(expandedPx: number): string {
  if (expandedPx <= 0) return ACTIVITY_RUN_CAP_CSS;
  return `calc(${ACTIVITY_RUN_CAP_CSS} + ${Math.round(expandedPx)}px)`;
}

/**
 * Bodies currently revealed by a disclosure inside `clip`.
 *
 * Ids are resolved WITHIN the clip, never through `document`: a body
 * outside this run must not lift this run's cap, and a stale
 * `aria-controls` pointing at a since-unmounted id resolves to nothing
 * instead of to whatever else claimed that id.
 */
export function activityRunExpandedBodies(clip: Element): HTMLElement[] {
  const found: HTMLElement[] = [];
  const seen = new Set<string>();
  for (const trigger of clip.querySelectorAll('[aria-expanded="true"][aria-controls]')) {
    const id = trigger.getAttribute('aria-controls');
    if (!id || seen.has(id)) continue;
    seen.add(id);
    const body = clip.querySelector(`[id="${CSS.escape(id)}"]`);
    if (body instanceof HTMLElement) found.push(body);
  }
  // A disclosure nested inside another expanded body (a tool row inside an
  // expanded subagent card) is already accounted for by its ancestor's
  // height. Counting both would lift the cap twice for one expansion.
  return found.filter((body) => !found.some((other) => other !== body && other.contains(body)));
}

export function activityRunExpandedHeight(bodies: readonly HTMLElement[]): number {
  let total = 0;
  for (const body of bodies) total += body.offsetHeight;
  return total;
}

/**
 * Report the total height of `clip`'s expanded bodies, and keep reporting it
 * as they open, close, and resize. Returns a teardown.
 *
 * Two observers, because the two things that change the number are unrelated:
 * a disclosure toggling (an `aria-expanded` mutation) changes WHICH bodies
 * count, and a body growing (a diff loading, output streaming into an open
 * card) changes what one of them contributes.
 */
export function observeActivityRunExpansion(
  clip: HTMLElement,
  onHeight: (px: number) => void,
): () => void {
  const sizes = new ResizeObserver(() => {
    onHeight(activityRunExpandedHeight(activityRunExpandedBodies(clip)));
  });
  function retarget(): void {
    const bodies = activityRunExpandedBodies(clip);
    sizes.disconnect();
    for (const body of bodies) sizes.observe(body);
    onHeight(activityRunExpandedHeight(bodies));
  }
  const disclosures = new MutationObserver(retarget);
  disclosures.observe(clip, {
    subtree: true,
    attributes: true,
    attributeFilter: ['aria-expanded'],
  });
  retarget();
  return () => {
    disclosures.disconnect();
    sizes.disconnect();
  };
}

/**
 * Runway above the window that reads as "the reader reached the top". A few
 * rows, so the next chunk is in the DOM before they meet the boundary rather
 * than after — the same reason the conversation prefetches older history
 * instead of waiting for the wall.
 */
const MOUNT_EARLIER_PREFETCH_PX = 96;

/**
 * Should the run mount its next older chunk, because the reader scrolled to
 * the top of the window and there is more above it?
 *
 * Requires the clip to be scrollable by more than the runway, which is what
 * keeps this from overriding `activityRunWindowRows`: a window whose rows all
 * fit under the cap rests at a `scrollTop` already inside the zone, and
 * without the guard it would page in chunk after chunk at mount time until the
 * content overflowed — mounting rows nobody asked for, which is the one thing
 * the window exists to prevent. Not scrollable means there was no gesture to
 * act on, and the boundary button is still there.
 *
 * Terminates without needing a re-entrancy flag to do the real work: prepend
 * compensation puts `scrollTop` back above the rows it just added, and a chunk
 * is several clip heights, so one mount takes the reader out of the zone. When
 * a short remainder does not, `hiddenEarlier` hits 0 and the boundary is gone.
 */
export function activityRunShouldMountEarlier(
  metrics: ScrollMetrics,
  hiddenEarlier: number,
): boolean {
  if (hiddenEarlier <= 0) return false;
  if (metrics.scrollHeight - metrics.clientHeight <= MOUNT_EARLIER_PREFETCH_PX) return false;
  return metrics.scrollTop <= MOUNT_EARLIER_PREFETCH_PX;
}

/**
 * Sub-pixel slack for "at the bottom". A fractional row height leaves a
 * scroller resting at its end short by a fraction of a pixel.
 */
const AT_BOTTOM_EPSILON_PX = 1;

/** Is the clip resting on its last row? */
export function activityRunAtBottom(metrics: ScrollMetrics): boolean {
  return metrics.scrollHeight - metrics.scrollTop - metrics.clientHeight
    <= AT_BOTTOM_EPSILON_PX;
}

/**
 * The mounted wrapper for run row `index`, or null when that row is outside
 * the mount window.
 *
 * Rows are addressed by index rather than by item id because only leaf rows
 * carry `data-item-id` — a jump into a subagent card inside a run has to
 * resolve to the card, which owns no id attribute of its own.
 */
export function activityRunChildElement(clip: Element, index: number): HTMLElement | null {
  const found = clip.querySelector(`[data-run-child="${index}"]`);
  return found instanceof HTMLElement ? found : null;
}

/**
 * Where run row `index` sits in `clip`'s viewport, or null when that row is
 * not mounted.
 */
export function activityRunRowViewportTop(clip: HTMLElement, index: number): number | null {
  const el = activityRunChildElement(clip, index);
  if (!el) return null;
  return el.getBoundingClientRect().top - clip.getBoundingClientRect().top;
}

/**
 * The `scrollTop` that puts run row `index` back at `viewportTop`, or null
 * when that row is not mounted.
 *
 * Read against the live `scrollTop` rather than against a content-space offset
 * captured earlier, so the answer holds whether or not the browser has already
 * clamped the position — a window slide shrinks the clip's content, and a
 * scroller resting at its end is clamped by the DOM change before this runs.
 */
export function activityRunScrollTopHoldingRow(
  clip: HTMLElement,
  index: number,
  viewportTop: number,
): number | null {
  const now = activityRunRowViewportTop(clip, index);
  if (now === null) return null;
  return clip.scrollTop + now - viewportTop;
}

/**
 * Whether `row` is entirely inside `clip`'s viewport.
 *
 * Separate from the centering below because the two are combined by a policy
 * that depends on the jump, not on the geometry: a jump that RELOCATED the
 * mount window has to place its target (the offset it inherited pointed at
 * different rows, so wherever the target landed is an accident), while a jump
 * the window already covered must leave a visible target alone — the reader is
 * looking at those rows, and nudging them would be the jump fighting them.
 */
export function activityRunRowFullyVisible(clip: HTMLElement, row: HTMLElement): boolean {
  const top = row.getBoundingClientRect().top - clip.getBoundingClientRect().top;
  return top >= 0 && top + row.offsetHeight <= clip.clientHeight;
}

/**
 * `scrollTop` that centers `row` in `clip`. Floored at 0; the browser clamps
 * the other end, so a row near either edge lands as close to centered as the
 * run allows.
 */
export function activityRunCenteredScrollTop(clip: HTMLElement, row: HTMLElement): number {
  const top = row.getBoundingClientRect().top - clip.getBoundingClientRect().top;
  const contentTop = clip.scrollTop + top;
  return Math.max(0, contentTop - Math.max(0, (clip.clientHeight - row.offsetHeight) / 2));
}
